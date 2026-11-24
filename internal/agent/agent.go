// Package agent runs on each device: it keeps one outbound session to the controller, listens for forwards it is the source of, and dials for forwards it is the target of.
package agent

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/longcw/portmesh/internal/proto"
)

// Config is the agent's local file, written by install.sh.
type Config struct {
	Name        string `json:"name"`
	Token       string `json:"token"`
	Server      string `json:"server"`
	Fingerprint string `json:"fingerprint"`
}

// LoadConfig reads the agent config file.
func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if c.Name == "" || c.Token == "" || c.Server == "" || c.Fingerprint == "" {
		return c, errors.New("config needs name, token, server and fingerprint")
	}
	return c, nil
}

type listener struct {
	addr string
	stop chan struct{}

	mu     sync.Mutex
	ln     net.Listener
	status proto.ListenerStatus
}

func (l *listener) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	close(l.stop)
	if l.ln != nil {
		l.ln.Close()
	}
}

// Agent holds the forwards last pushed by the controller; listeners outlive a dropped session so binds never flap.
type Agent struct {
	cfg     Config
	version string
	sess    atomic.Pointer[yamux.Session]
	changed chan struct{}

	mu        sync.Mutex
	forwards  map[string]proto.Forward
	listeners map[string]*listener
}

// New returns an agent for cfg.
func New(cfg Config, version string) *Agent {
	return &Agent{cfg: cfg, version: version, changed: make(chan struct{}, 1), forwards: map[string]proto.Forward{}, listeners: map[string]*listener{}}
}

func (a *Agent) poke() {
	select {
	case a.changed <- struct{}{}:
	default:
	}
}

// Run connects to the controller forever, backing off between failed attempts.
func (a *Agent) Run() {
	backoff := time.Second
	for {
		start := time.Now()
		err := a.session()
		if time.Since(start) > 30*time.Second {
			backoff = time.Second
		}
		log.Printf("session ended: %v; retrying in %s", err, backoff)
		time.Sleep(backoff)
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (a *Agent) session() error {
	raw, err := (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}).Dial("tcp", a.cfg.Server)
	if err != nil {
		return err
	}
	tc := tls.Client(raw, &tls.Config{
		ServerName:         "portmesh",
		InsecureSkipVerify: true, // the pinned fingerprint below is the whole check
		MinVersion:         tls.VersionTLS13,
		VerifyPeerCertificate: func(certs [][]byte, _ [][]*x509.Certificate) error {
			if len(certs) == 0 {
				return errors.New("no certificate")
			}
			sum := sha256.Sum256(certs[0])
			if hex.EncodeToString(sum[:]) != a.cfg.Fingerprint {
				return errors.New("controller certificate does not match the pinned fingerprint")
			}
			return nil
		},
	})
	tc.SetDeadline(time.Now().Add(15 * time.Second))
	if err := tc.Handshake(); err != nil {
		raw.Close()
		return err
	}
	tc.SetDeadline(time.Time{})
	cfg := yamux.DefaultConfig()
	cfg.KeepAliveInterval = 10 * time.Second
	cfg.ConnectionWriteTimeout = 15 * time.Second
	cfg.LogOutput = io.Discard
	ys, err := yamux.Client(tc, cfg)
	if err != nil {
		raw.Close()
		return err
	}
	defer ys.Close()
	ctrl, err := ys.OpenStream()
	if err != nil {
		return err
	}
	ctrl.SetDeadline(time.Now().Add(15 * time.Second))
	if err := proto.Write(ctrl, proto.Hello{Name: a.cfg.Name, Token: a.cfg.Token, Version: a.version}); err != nil {
		return err
	}
	var r proto.Reply
	if err := proto.Read(ctrl, &r); err != nil {
		return err
	}
	if r.Error != "" {
		return fmt.Errorf("controller refused: %s", r.Error)
	}
	ctrl.SetDeadline(time.Time{})
	log.Printf("connected to %s as %s", a.cfg.Server, a.cfg.Name)
	a.sess.Store(ys)
	defer a.sess.CompareAndSwap(ys, nil)

	go func() {
		for {
			st, err := ys.AcceptStream()
			if err != nil {
				return
			}
			go a.handleDial(st)
		}
	}()
	go a.statusLoop(ys, ctrl)
	for {
		var p proto.Push
		if err := proto.Read(ctrl, &p); err != nil {
			return err
		}
		a.apply(p)
	}
}

// statusLoop reports listener state on change and every 5 s, which also keeps the controller's read deadline fresh.
func (a *Agent) statusLoop(ys *yamux.Session, ctrl *yamux.Stream) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		st := proto.Status{Listeners: map[string]proto.ListenerStatus{}}
		a.mu.Lock()
		for id, l := range a.listeners {
			l.mu.Lock()
			st.Listeners[id] = l.status
			l.mu.Unlock()
		}
		a.mu.Unlock()
		ctrl.SetWriteDeadline(time.Now().Add(15 * time.Second))
		if err := proto.Write(ctrl, st); err != nil {
			ys.Close()
			return
		}
		select {
		case <-ys.CloseChan():
			return
		case <-t.C:
		case <-a.changed:
		}
	}
}

func (a *Agent) apply(p proto.Push) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.forwards = map[string]proto.Forward{}
	for _, f := range p.Forwards {
		a.forwards[f.ID] = f
	}
	for id, l := range a.listeners {
		f, ok := a.forwards[id]
		if !ok || f.Source != a.cfg.Name || l.addr != bindAddr(f) {
			l.close()
			delete(a.listeners, id)
			log.Printf("forward %s: stopped listening on %s", id, l.addr)
		}
	}
	for id, f := range a.forwards {
		if f.Source == a.cfg.Name && a.listeners[id] == nil {
			l := &listener{addr: bindAddr(f), stop: make(chan struct{})}
			a.listeners[id] = l
			go a.listen(id, l)
		}
	}
	a.poke()
}

func bindAddr(f proto.Forward) string {
	return net.JoinHostPort(f.BindAddr, strconv.Itoa(f.BindPort))
}

// listen keeps l bound until it is closed, retrying every 5 s so an address that appears later (docker0) still gets bound.
func (a *Agent) listen(id string, l *listener) {
	for {
		ln, err := net.Listen("tcp", l.addr)
		if err != nil {
			err = explainBindError(err, l.addr)
		}
		l.mu.Lock()
		select {
		case <-l.stop:
			l.mu.Unlock()
			if ln != nil {
				ln.Close()
			}
			return
		default:
		}
		if err != nil {
			if l.status.Error != err.Error() {
				log.Printf("forward %s: %v", id, err)
			}
			l.status = proto.ListenerStatus{Error: err.Error()}
		} else {
			log.Printf("forward %s: listening on %s", id, l.addr)
			l.ln, l.status = ln, proto.ListenerStatus{Listening: true}
		}
		l.mu.Unlock()
		a.poke()
		if err == nil {
			for {
				c, err := ln.Accept()
				if err != nil {
					break
				}
				go a.handleLocal(id, c)
			}
			l.mu.Lock()
			l.ln, l.status = nil, proto.ListenerStatus{Error: "listener closed"}
			l.mu.Unlock()
		}
		select {
		case <-l.stop:
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (a *Agent) handleLocal(id string, c net.Conn) {
	ys := a.sess.Load()
	if ys == nil {
		c.Close()
		return
	}
	st, err := ys.OpenStream()
	if err != nil {
		c.Close()
		return
	}
	if err := proto.Write(st, proto.Open{Forward: id}); err != nil {
		c.Close()
		st.Close()
		return
	}
	proto.Pipe(c, st, nil, nil)
}

// handleDial serves a controller request to connect, but only to the host and port of a forward this device is the target of.
func (a *Agent) handleDial(st *yamux.Stream) {
	st.SetDeadline(time.Now().Add(15 * time.Second))
	var d proto.Dial
	if err := proto.Read(st, &d); err != nil {
		st.Close()
		return
	}
	a.mu.Lock()
	f, ok := a.forwards[d.Forward]
	a.mu.Unlock()
	if !ok || f.Target != a.cfg.Name || f.TargetHost != d.Host || f.TargetPort != d.Port {
		proto.Write(st, proto.Reply{Error: "not a forward this device targets"})
		st.Close()
		return
	}
	c, err := net.DialTimeout("tcp", net.JoinHostPort(d.Host, strconv.Itoa(d.Port)), 5*time.Second)
	if err != nil {
		proto.Write(st, proto.Reply{Error: err.Error()})
		st.Close()
		return
	}
	if err := proto.Write(st, proto.Reply{}); err != nil {
		c.Close()
		st.Close()
		return
	}
	st.SetDeadline(time.Time{})
	proto.Pipe(c, st, nil, nil)
}
