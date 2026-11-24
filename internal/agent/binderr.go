package agent

import (
	"errors"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
)

var ssPidRe = regexp.MustCompile(`pid=(\d+)`)

// explainBindError turns a bind failure into a message that names what holds the port or why the address is unusable.
func explainBindError(err error, addr string) error {
	_, port, _ := net.SplitHostPort(addr)
	switch {
	case errors.Is(err, syscall.EADDRINUSE):
		return fmt.Errorf("%s is already in use by %s", addr, portHolders(port))
	case errors.Is(err, syscall.EADDRNOTAVAIL):
		return fmt.Errorf("%s: this device has no interface with that address", addr)
	case errors.Is(err, syscall.EACCES):
		return fmt.Errorf("%s: permission denied (ports below 1024 need root)", addr)
	}
	return err
}

// portHolders lists the listeners on port with their command lines, using lsof or ss, whichever exists.
func portHolders(port string) string {
	var pids, addrs []string
	seen := map[string]bool{}
	add := func(list *[]string, v string) {
		if !seen[v] {
			seen[v] = true
			*list = append(*list, v)
		}
	}
	if out, err := exec.Command("lsof", "-nP", "-iTCP:"+port, "-sTCP:LISTEN", "-Fpn").Output(); err == nil || len(out) > 0 {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "p") {
				add(&pids, line[1:])
			} else if strings.HasPrefix(line, "n") {
				add(&addrs, line[1:])
			}
		}
	} else if out, err := exec.Command("ss", "-Hltnp", "sport = :"+port).Output(); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if f := strings.Fields(line); len(f) >= 4 {
				add(&addrs, f[3])
			}
			for _, m := range ssPidRe.FindAllStringSubmatch(line, -1) {
				add(&pids, m[1])
			}
		}
	}
	var holders []string
	for _, pid := range pids {
		out, _ := exec.Command("ps", "-o", "args=", "-p", pid).Output()
		cmd := strings.TrimSpace(string(out))
		if len(cmd) > 80 {
			cmd = cmd[:77] + "..."
		}
		holders = append(holders, fmt.Sprintf("pid %s (%s)", pid, cmd))
	}
	if len(holders) == 0 {
		if len(addrs) > 0 {
			return "a process of another user on " + strings.Join(addrs, ", ")
		}
		return "a process this agent cannot see (another user or root)"
	}
	s := strings.Join(holders, ", ")
	if len(addrs) > 0 {
		s += " listening on " + strings.Join(addrs, ", ")
	}
	return s
}
