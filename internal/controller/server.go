// Package controller holds the forward configuration, relays data between agents, and serves the web UI.
package controller

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/longcw/portmesh/internal/proto"
)

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

type stats struct {
	active, total     atomic.Int64
	bytesIn, bytesOut atomic.Int64
	lastError         atomic.Pointer[timedError]
}

type timedError struct {
	Msg string
	At  time.Time
}

func (s *stats) fail(format string, args ...any) {
	s.lastError.Store(&timedError{fmt.Sprintf(format, args...), time.Now()})
}

type session struct {
	name, remote, version string
	since                 time.Time
	ys                    *yamux.Session
	ctrl                  *yamux.Stream
	dirty                 chan struct{}

	mu        sync.Mutex
	listeners map[string]proto.ListenerStatus
	lastSeen  time.Time
}

func (s *session) poke() {
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

// Server is the controller: config store, agent sessions and the relay between them.
type Server struct {
	dataDir     string
	tlsConf     *tls.Config
	fingerprint string
	// AgentAddr and BaseURL are what install commands tell agents to use; empty means the host the UI was opened on.
	AgentAddr, BaseURL string
	// AgentPort is the port agents connect to when AgentAddr is derived.
	AgentPort string
	// BinDir holds the agent binaries served at /dl/.
	BinDir string

	mu       sync.Mutex
	state    State
	sessions map[string]*session
	stats    map[string]*stats
}

// New loads state and the TLS identity from dataDir, creating both on first start.
func New(dataDir string) (*Server, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	cert, err := loadOrCreateCert(dataDir)
	if err != nil {
		return nil, err
	}
	st, err := loadState(filepath.Join(dataDir, "state.json"))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(cert.Certificate[0])
	s := &Server{
		dataDir:     dataDir,
		tlsConf:     &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13},
		fingerprint: hex.EncodeToString(sum[:]),
		state:       st,
		sessions:    map[string]*session{},
		stats:       map[string]*stats{},
	}
	for _, f := range st.Forwards {
		s.stats[f.ID] = &stats{}
	}
	return s, nil
}

// Fingerprint is the SHA-256 of the controller certificate that agents pin.
func (s *Server) Fingerprint() string { return s.fingerprint }

func loadOrCreateCert(dir string) (tls.Certificate, error) {
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "portmesh"},
		DNSNames:     []string{"portmesh"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(30, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		return tls.Certificate{}, err
	}
	return tls.LoadX509KeyPair(certPath, keyPath)
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// save persists state; callers hold s.mu.
func (s *Server) save() error {
	return saveState(filepath.Join(s.dataDir, "state.json"), s.state)
}

// notifyAll asks every online session to resend its forwards; callers hold s.mu.
func (s *Server) notifyAll() {
	for _, sess := range s.sessions {
		sess.poke()
	}
}

// AddDevice enrolls a device, or re-keys an existing one and drops its session, and returns the new token.
func (s *Server) AddDevice(name string) (string, error) {
	if !nameRe.MatchString(name) {
		return "", errors.New("name must be lowercase letters, digits and dashes")
	}
	b := make([]byte, 32)
	rand.Read(b)
	token := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for i := range s.state.Devices {
		if s.state.Devices[i].Name == name {
			s.state.Devices[i].TokenHash = hashToken(token)
			found = true
		}
	}
	if !found {
		s.state.Devices = append(s.state.Devices, Device{Name: name, TokenHash: hashToken(token), Created: time.Now()})
	}
	if err := s.save(); err != nil {
		return "", err
	}
	if sess := s.sessions[name]; sess != nil {
		sess.ys.Close()
	}
	return token, nil
}

// DeleteDevice removes a device that no forward uses and drops its session.
func (s *Server) DeleteDevice(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.state.Forwards {
		if f.Source == name || f.Target == name {
			return fmt.Errorf("forward %q uses %s", f.Name, name)
		}
	}
	out := s.state.Devices[:0]
	for _, d := range s.state.Devices {
		if d.Name != name {
			out = append(out, d)
		}
	}
	s.state.Devices = out
	if sess := s.sessions[name]; sess != nil {
		sess.ys.Close()
	}
	return s.save()
}

// SaveForward validates f and creates it (empty ID) or replaces the forward with its ID.
func (s *Server) SaveForward(f proto.Forward) (proto.Forward, error) {
	if f.BindAddr == "" {
		f.BindAddr = "127.0.0.1"
	}
	if net.ParseIP(f.BindAddr) == nil {
		return f, errors.New("bind address must be an IP address")
	}
	if f.TargetHost == "" {
		f.TargetHost = "127.0.0.1"
	}
	if f.TargetPort == 0 {
		f.TargetPort = f.BindPort
	}
	if f.BindPort < 1 || f.BindPort > 65535 || f.TargetPort < 1 || f.TargetPort > 65535 {
		return f, errors.New("ports must be 1-65535")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	known := map[string]bool{}
	for _, d := range s.state.Devices {
		known[d.Name] = true
	}
	if !known[f.Source] || !known[f.Target] {
		return f, errors.New("source and target must be enrolled devices")
	}
	idx := -1
	for i, g := range s.state.Forwards {
		if g.ID == f.ID && f.ID != "" {
			idx = i
			continue
		}
		if g.Source == f.Source && g.BindPort == f.BindPort && (g.BindAddr == f.BindAddr || g.BindAddr == "0.0.0.0" || f.BindAddr == "0.0.0.0") {
			return f, fmt.Errorf("%s already binds %s:%d in forward %q", f.Source, g.BindAddr, g.BindPort, g.Name)
		}
	}
	if f.ID == "" {
		b := make([]byte, 4)
		rand.Read(b)
		f.ID = hex.EncodeToString(b)
		s.state.Forwards = append(s.state.Forwards, f)
		s.stats[f.ID] = &stats{}
	} else if idx < 0 {
		return f, errors.New("no such forward")
	} else {
		s.state.Forwards[idx] = f
	}
	if err := s.save(); err != nil {
		return f, err
	}
	s.notifyAll()
	return f, nil
}

// DeleteForward removes a forward; agents close its listener on the next push.
func (s *Server) DeleteForward(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.state.Forwards[:0]
	for _, f := range s.state.Forwards {
		if f.ID != id {
			out = append(out, f)
		}
	}
	s.state.Forwards = out
	delete(s.stats, id)
	s.notifyAll()
	return s.save()
}

// ServeAgents accepts agent connections on ln until it closes.
func (s *Server) ServeAgents(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.handleConn(c)
	}
}

func yamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.KeepAliveInterval = 10 * time.Second
	cfg.ConnectionWriteTimeout = 15 * time.Second
	cfg.LogOutput = io.Discard
	return cfg
}

func (s *Server) handleConn(raw net.Conn) {
	remote := raw.RemoteAddr().String()
	tc := tls.Server(raw, s.tlsConf)
	tc.SetDeadline(time.Now().Add(15 * time.Second))
	if err := tc.Handshake(); err != nil {
		raw.Close()
		return
	}
	tc.SetDeadline(time.Time{})
	ys, err := yamux.Server(tc, yamuxConfig())
	if err != nil {
		raw.Close()
		return
	}
	timer := time.AfterFunc(15*time.Second, func() { ys.Close() })
	ctrl, err := ys.AcceptStream()
	timer.Stop()
	if err != nil {
		ys.Close()
		return
	}
	ctrl.SetDeadline(time.Now().Add(15 * time.Second))
	var h proto.Hello
	if err := proto.Read(ctrl, &h); err != nil {
		ys.Close()
		return
	}
	if !s.authenticate(h) {
		log.Printf("agent %q from %s: rejected", h.Name, remote)
		proto.Write(ctrl, proto.Reply{Error: "unknown device or bad token"})
		time.Sleep(100 * time.Millisecond)
		ys.Close()
		return
	}
	ctrl.SetDeadline(time.Time{})
	sess := &session{
		name: h.Name, remote: remote, version: h.Version, since: time.Now(), lastSeen: time.Now(),
		ys: ys, ctrl: ctrl, dirty: make(chan struct{}, 1), listeners: map[string]proto.ListenerStatus{},
	}
	if err := proto.Write(ctrl, proto.Reply{}); err != nil {
		ys.Close()
		return
	}
	s.mu.Lock()
	if old := s.sessions[h.Name]; old != nil {
		old.ys.Close()
	}
	s.sessions[h.Name] = sess
	s.notifyAll()
	s.mu.Unlock()
	log.Printf("agent %s connected from %s (version %s)", h.Name, remote, h.Version)

	go s.pushLoop(sess)
	go func() {
		for {
			st, err := ys.AcceptStream()
			if err != nil {
				return
			}
			go s.relay(sess, st)
		}
	}()
	for {
		ctrl.SetReadDeadline(time.Now().Add(30 * time.Second))
		var st proto.Status
		if err := proto.Read(ctrl, &st); err != nil {
			log.Printf("agent %s disconnected: %v", h.Name, err)
			break
		}
		sess.mu.Lock()
		sess.listeners = st.Listeners
		sess.lastSeen = time.Now()
		sess.mu.Unlock()
	}
	ys.Close()
	s.mu.Lock()
	if s.sessions[h.Name] == sess {
		delete(s.sessions, h.Name)
	}
	s.notifyAll()
	s.mu.Unlock()
}

func (s *Server) authenticate(h proto.Hello) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.state.Devices {
		if d.Name == h.Name {
			return subtle.ConstantTimeCompare([]byte(d.TokenHash), []byte(hashToken(h.Token))) == 1
		}
	}
	return false
}

// pushLoop sends the session its current forwards each time it is poked, so pushes never arrive out of order.
func (s *Server) pushLoop(sess *session) {
	for {
		select {
		case <-sess.ys.CloseChan():
			return
		case <-sess.dirty:
		}
		s.mu.Lock()
		var p proto.Push
		for _, f := range s.state.Forwards {
			if f.Enabled && (f.Source == sess.name || f.Target == sess.name) {
				p.Forwards = append(p.Forwards, f)
			}
		}
		s.mu.Unlock()
		sess.ctrl.SetWriteDeadline(time.Now().Add(15 * time.Second))
		if err := proto.Write(sess.ctrl, p); err != nil {
			sess.ys.Close()
			return
		}
	}
}

// relay joins a data stream from a source agent to a fresh stream on the target agent.
func (s *Server) relay(src *session, in *yamux.Stream) {
	in.SetDeadline(time.Now().Add(15 * time.Second))
	var o proto.Open
	if err := proto.Read(in, &o); err != nil {
		in.Close()
		return
	}
	s.mu.Lock()
	var f proto.Forward
	for _, g := range s.state.Forwards {
		if g.ID == o.Forward {
			f = g
		}
	}
	st := s.stats[o.Forward]
	dst := s.sessions[f.Target]
	s.mu.Unlock()
	if f.ID == "" || !f.Enabled || f.Source != src.name {
		in.Close()
		return
	}
	if dst == nil {
		st.fail("target %s offline", f.Target)
		in.Close()
		return
	}
	out, err := dst.ys.OpenStream()
	if err != nil {
		st.fail("open stream to %s: %v", f.Target, err)
		in.Close()
		return
	}
	out.SetDeadline(time.Now().Add(15 * time.Second))
	var r proto.Reply
	if err := proto.Write(out, proto.Dial{Forward: f.ID, Host: f.TargetHost, Port: f.TargetPort}); err == nil {
		err = proto.Read(out, &r)
		if err != nil {
			r.Error = err.Error()
		}
	} else {
		r.Error = err.Error()
	}
	if r.Error != "" {
		st.fail("%s: %s", f.Target, r.Error)
		in.Close()
		out.Close()
		return
	}
	in.SetDeadline(time.Time{})
	out.SetDeadline(time.Time{})
	st.active.Add(1)
	st.total.Add(1)
	proto.Pipe(in, out, &st.bytesIn, &st.bytesOut)
	st.active.Add(-1)
}
