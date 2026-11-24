package agent_test

import (
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/longcw/portmesh/internal/agent"
	"github.com/longcw/portmesh/internal/controller"
	"github.com/longcw/portmesh/internal/proto"
)

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestForwardCarriesTraffic runs a controller and two agents and echoes bytes through a forward between them.
func TestForwardCarriesTraffic(t *testing.T) {
	srv, err := controller.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.ServeAgents(ln)

	echo, _ := net.Listen("tcp", "127.0.0.1:0")
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()

	for _, name := range []string{"src", "dst"} {
		token, err := srv.AddDevice(name)
		if err != nil {
			t.Fatal(err)
		}
		go agent.New(agent.Config{Name: name, Token: token, Server: ln.Addr().String(), Fingerprint: srv.Fingerprint()}, "test").Run()
	}
	bind := freePort(t)
	if _, err := srv.SaveForward(proto.Forward{Source: "src", BindPort: bind, Target: "dst", TargetPort: echo.Addr().(*net.TCPAddr).Port, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(bind)))
		if err == nil {
			c.SetDeadline(time.Now().Add(2 * time.Second))
			c.Write([]byte("ping"))
			buf := make([]byte, 4)
			_, err = io.ReadFull(c, buf)
			c.Close()
			if err == nil && string(buf) == "ping" {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("no echo through the forward")
}
