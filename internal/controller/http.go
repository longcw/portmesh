package controller

import (
	"embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/longcw/portmesh/internal/proto"
)

//go:embed web/index.html web/install.sh
var webFS embed.FS

var binRe = regexp.MustCompile(`^portmesh-(darwin|linux)-(amd64|arm64)$`)

type deviceView struct {
	Name     string    `json:"name"`
	Online   bool      `json:"online"`
	Remote   string    `json:"remote,omitempty"`
	Version  string    `json:"version,omitempty"`
	Since    time.Time `json:"since,omitempty"`
	LastSeen time.Time `json:"last_seen,omitempty"`
}

type forwardStatus struct {
	State       string    `json:"state"`
	Detail      string    `json:"detail,omitempty"`
	Active      int64     `json:"active"`
	Total       int64     `json:"total"`
	BytesIn     int64     `json:"bytes_in"`
	BytesOut    int64     `json:"bytes_out"`
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitempty"`
}

type forwardView struct {
	proto.Forward
	Status forwardStatus `json:"status"`
}

func (s *Server) snapshot(agentAddr string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	devices := []deviceView{}
	for _, d := range s.state.Devices {
		v := deviceView{Name: d.Name}
		if sess := s.sessions[d.Name]; sess != nil {
			sess.mu.Lock()
			v.Online, v.Remote, v.Version, v.Since, v.LastSeen = true, sess.remote, sess.version, sess.since, sess.lastSeen
			sess.mu.Unlock()
		}
		devices = append(devices, v)
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].Name < devices[j].Name })
	forwards := []forwardView{}
	for _, f := range s.state.Forwards {
		st := s.stats[f.ID]
		fs := forwardStatus{Active: st.active.Load(), Total: st.total.Load(), BytesIn: st.bytesIn.Load(), BytesOut: st.bytesOut.Load()}
		if e := st.lastError.Load(); e != nil {
			fs.LastError, fs.LastErrorAt = e.Msg, e.At
		}
		src, dst := s.sessions[f.Source], s.sessions[f.Target]
		switch {
		case !f.Enabled:
			fs.State = "disabled"
		case src == nil:
			fs.State, fs.Detail = "down", f.Source+" offline"
		default:
			src.mu.Lock()
			ls, ok := src.listeners[f.ID]
			src.mu.Unlock()
			switch {
			case !ok:
				fs.State, fs.Detail = "pending", "waiting for "+f.Source
			case !ls.Listening:
				fs.State, fs.Detail = "down", "bind: "+ls.Error
			case dst == nil:
				fs.State, fs.Detail = "degraded", "listening, "+f.Target+" offline"
			default:
				fs.State = "up"
			}
		}
		forwards = append(forwards, forwardView{f, fs})
	}
	return map[string]any{"devices": devices, "forwards": forwards, "fingerprint": s.fingerprint, "agent_addr": agentAddr}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
}

// addrs returns the configured base URL and agent address, falling back to the host the request came in on.
func (s *Server) addrs(r *http.Request) (baseURL, agentAddr string) {
	baseURL, agentAddr = s.BaseURL, s.AgentAddr
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	if baseURL == "" {
		baseURL = "http://" + r.Host
	}
	if agentAddr == "" {
		agentAddr = net.JoinHostPort(host, s.AgentPort)
	}
	return baseURL, agentAddr
}

// Handler serves the web UI, its JSON API, and the agent install script and binaries.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := webFS.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		_, agentAddr := s.addrs(r)
		writeJSON(w, http.StatusOK, s.snapshot(agentAddr))
	})
	mux.HandleFunc("POST /api/devices", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Name string }
		json.NewDecoder(r.Body).Decode(&req)
		token, err := s.AddDevice(strings.TrimSpace(req.Name))
		if err != nil {
			writeErr(w, err)
			return
		}
		baseURL, _ := s.addrs(r)
		writeJSON(w, http.StatusOK, map[string]string{"install": fmt.Sprintf("curl -fsSL %s/install.sh | sudo sh -s -- %s %s", baseURL, req.Name, token)})
	})
	mux.HandleFunc("DELETE /api/devices/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.DeleteDevice(r.PathValue("name")); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{})
	})
	saveForward := func(w http.ResponseWriter, r *http.Request) {
		var f proto.Forward
		if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
			writeErr(w, err)
			return
		}
		f.ID = r.PathValue("id")
		f, err := s.SaveForward(f)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, f)
	}
	mux.HandleFunc("POST /api/forwards", saveForward)
	mux.HandleFunc("PUT /api/forwards/{id}", saveForward)
	mux.HandleFunc("DELETE /api/forwards/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.DeleteForward(r.PathValue("id")); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{})
	})
	mux.HandleFunc("GET /install.sh", func(w http.ResponseWriter, r *http.Request) {
		b, _ := webFS.ReadFile("web/install.sh")
		baseURL, agentAddr := s.addrs(r)
		script := strings.NewReplacer("@BASE_URL@", baseURL, "@AGENT_ADDR@", agentAddr, "@FINGERPRINT@", s.fingerprint).Replace(string(b))
		w.Header().Set("Content-Type", "text/x-shellscript")
		w.Write([]byte(script))
	})
	mux.HandleFunc("GET /dl/{file}", func(w http.ResponseWriter, r *http.Request) {
		file := r.PathValue("file")
		if !binRe.MatchString(file) {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(s.BinDir, file))
	})
	return mux
}
