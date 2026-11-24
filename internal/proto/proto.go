// Package proto holds the messages agents and the controller exchange, framed as length-prefixed JSON.
package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
)

const maxMsg = 1 << 20

// Forward makes BindAddr:BindPort on device Source carry traffic to TargetHost:TargetPort as dialed by device Target.
type Forward struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Source     string `json:"source"`
	BindAddr   string `json:"bind_addr"`
	BindPort   int    `json:"bind_port"`
	Target     string `json:"target"`
	TargetHost string `json:"target_host"`
	TargetPort int    `json:"target_port"`
	Enabled    bool   `json:"enabled"`
}

// Hello is the first message an agent sends on its control stream.
type Hello struct {
	Name    string `json:"name"`
	Token   string `json:"token"`
	Version string `json:"version"`
}

// Reply answers a Hello or a Dial; an empty Error means success.
type Reply struct {
	Error string `json:"error,omitempty"`
}

// Push is the full set of forwards a device takes part in, sent on every change.
type Push struct {
	Forwards []Forward `json:"forwards"`
}

// ListenerStatus is the source-side state of one forward.
type ListenerStatus struct {
	Listening bool   `json:"listening"`
	Error     string `json:"error,omitempty"`
}

// Status is what an agent reports on its control stream, keyed by forward ID.
type Status struct {
	Listeners map[string]ListenerStatus `json:"listeners"`
}

// Open starts a data stream from the source agent to the controller.
type Open struct {
	Forward string `json:"forward"`
}

// Dial asks the target agent to connect a data stream to Host:Port.
type Dial struct {
	Forward string `json:"forward"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
}

// Write sends v as one frame.
func Write(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	_, err = w.Write(buf)
	return err
}

// Read reads exactly one frame into v, never past it, so the stream can carry raw bytes afterwards.
func Read(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxMsg {
		return fmt.Errorf("frame too large: %d", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

type counter struct {
	w io.Writer
	n *atomic.Int64
}

func (c counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// Pipe copies a and b into each other until both directions end, counting a→b into ab and b→a into ba when non-nil.
func Pipe(a, b io.ReadWriteCloser, ab, ba *atomic.Int64) {
	done := make(chan struct{}, 2)
	cp := func(dst, src io.ReadWriteCloser, n *atomic.Int64) {
		var w io.Writer = dst
		if n != nil {
			w = counter{dst, n}
		}
		_, err := io.Copy(w, src)
		// a clean EOF half-closes so the other direction can drain; anything else tears both down
		if err == nil || errors.Is(err, io.EOF) {
			if hc, ok := dst.(interface{ CloseWrite() error }); ok {
				hc.CloseWrite()
			} else {
				dst.Close()
			}
		} else {
			a.Close()
			b.Close()
		}
		done <- struct{}{}
	}
	go cp(b, a, ab)
	go cp(a, b, ba)
	<-done
	<-done
	a.Close()
	b.Close()
}
