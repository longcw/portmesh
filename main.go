// Command portmesh runs either the controller (web UI + relay) or a device agent.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"

	"github.com/longcw/portmesh/internal/agent"
	"github.com/longcw/portmesh/internal/controller"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: portmesh controller|agent|version [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "controller":
		fs := flag.NewFlagSet("controller", flag.ExitOnError)
		data := fs.String("data", "/data", "state and TLS key directory")
		agentListen := fs.String("agent-listen", ":7900", "address agents connect to")
		httpListen := fs.String("http-listen", ":7901", "web UI address")
		agentAddr := fs.String("agent-addr", os.Getenv("PORTMESH_AGENT_ADDR"), "agent address written into install commands (default: the host the UI is opened on)")
		baseURL := fs.String("base-url", os.Getenv("PORTMESH_BASE_URL"), "URL install commands download from (default: the URL the UI is opened on)")
		binDir := fs.String("bin-dir", "/app/bin", "directory of agent binaries")
		fs.Parse(os.Args[2:])
		s, err := controller.New(*data)
		if err != nil {
			log.Fatal(err)
		}
		_, agentPort, err := net.SplitHostPort(*agentListen)
		if err != nil {
			log.Fatal(err)
		}
		s.AgentAddr, s.BaseURL, s.BinDir, s.AgentPort = *agentAddr, *baseURL, *binDir, agentPort
		ln, err := net.Listen("tcp", *agentListen)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("portmesh %s controller: agents on %s, UI on %s, fingerprint %s", version, *agentListen, *httpListen, s.Fingerprint())
		go func() { log.Fatal(s.ServeAgents(ln)) }()
		log.Fatal(http.ListenAndServe(*httpListen, s.Handler()))
	case "agent":
		fs := flag.NewFlagSet("agent", flag.ExitOnError)
		cfgPath := fs.String("config", "/etc/portmesh/agent.json", "agent config file")
		fs.Parse(os.Args[2:])
		cfg, err := agent.LoadConfig(*cfgPath)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("portmesh %s agent %s", version, cfg.Name)
		agent.New(cfg, version).Run()
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}
