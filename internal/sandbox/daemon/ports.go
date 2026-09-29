package daemon

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// PortStatus is one port's result.
type PortStatus struct {
	Port      int  `json:"port"`
	Listening bool `json:"listening"`
	// LoopbackOnly means the guest listens on this port only on 127.0.0.1/::1,
	// so the preview cannot reach it until the server binds 0.0.0.0.
	LoopbackOnly bool `json:"loopback_only,omitempty"`
}

// DetectPorts reports which ports a running sandbox listens on. The guest's own
// socket table is the answer when it can be read: it finds a server on any port,
// and it is not fooled by docker-proxy, which accepts a connection on a published
// port whether or not anything listens behind it. Otherwise each candidate is
// dialled. ListenTarget only resolves an address; it does not confirm a listener.
func (d *Daemon) DetectPorts(ctx context.Context, id string, candidates []int) ([]PortStatus, error) {
	d.mu.RLock()
	sb, ok := d.boxes[id]
	d.mu.RUnlock()
	if !ok {
		return nil, rt.ErrNotFound
	}
	if sb.inst == nil {
		return nil, rt.ErrPaused
	}
	inst := sb.inst

	byPort := map[int]PortStatus{}
	for _, p := range candidates {
		byPort[p] = PortStatus{Port: p}
	}

	// Only an isolated guest has a socket table of its own; the process runtime's
	// /proc/net is the host's.
	var listeners map[int]bool
	read := false
	if d.cfg.Runtime.Capabilities().Isolation {
		listeners, read = guestListeners(ctx, inst)
	}
	for port, loopbackOnly := range listeners {
		if rt.IsPlatformPort(port) {
			continue
		}
		byPort[port] = PortStatus{Port: port, Listening: true, LoopbackOnly: loopbackOnly}
	}

	// Report nothing from the dial on runtimes that cannot tell ports apart.
	if !read && discriminatesByPort(func(p int) (string, bool) {
		a, err := inst.ListenTarget(p)
		return a, err == nil && a != "" && dialable(ctx, a)
	}) {
		for _, p := range candidates {
			if byPort[p].Listening {
				continue
			}
			addr, err := inst.ListenTarget(p)
			if err == nil && addr != "" && dialable(ctx, addr) {
				byPort[p] = PortStatus{Port: p, Listening: true}
			}
		}
	}

	out := make([]PortStatus, 0, len(byPort))
	for _, s := range byPort {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out, nil
}

// guestListeners reads the guest's TCP socket table through a plain exec, so it
// works with every envd and every template already built. ok is false when the
// table could not be read, and the caller falls back to dialling.
func guestListeners(ctx context.Context, inst rt.Instance) (listeners map[int]bool, ok bool) {
	res, err := inst.Exec(ctx, rt.ExecSpec{
		Cmd:     "cat /proc/net/tcp /proc/net/tcp6 2>/dev/null",
		Timeout: 5 * time.Second,
	})
	if err != nil || !strings.Contains(res.Stdout, "local_address") {
		return nil, false
	}
	return parseProcNetTCP(res.Stdout), true
}

// parseProcNetTCP maps each LISTEN port in /proc/net/tcp{,6} text to whether
// every socket on it is bound to loopback.
func parseProcNetTCP(text string) map[int]bool {
	const stateListen = "0A"
	out := map[int]bool{}
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[3] != stateListen {
			continue
		}
		ip, portHex, ok := strings.Cut(f[1], ":")
		if !ok || ip == dockerDNSAddr {
			continue
		}
		port, err := strconv.ParseUint(portHex, 16, 16)
		if err != nil || port == 0 {
			continue
		}
		loop := procIPIsLoopback(ip)
		if prev, seen := out[int(port)]; seen {
			loop = prev && loop
		}
		out[int(port)] = loop
	}
	return out
}

// dockerDNSAddr is 127.0.0.11, where Docker's embedded resolver listens inside
// every container on a user-defined network. It is not the sandbox's server.
const dockerDNSAddr = "0B00007F"

// procIPIsLoopback reads the kernel's hex address: IPv4 is one little-endian
// word (127.0.0.1 is 0100007F), IPv6 is four (::1 ends 01000000, and a
// v4-mapped ::ffff:127.x.x.x keeps the IPv4 word last).
func procIPIsLoopback(hex string) bool {
	hex = strings.ToUpper(hex)
	switch len(hex) {
	case 8:
		return strings.HasSuffix(hex, "7F")
	case 32:
		if hex == "00000000000000000000000001000000" {
			return true
		}
		return strings.HasPrefix(hex, "0000000000000000FFFF0000") && strings.HasSuffix(hex, "7F")
	}
	return false
}

// canaryPort is a port nothing should ever serve.
const canaryPort = 49871

// discriminatesByPort reports whether ListenTarget varies with the port. Docker
// and process ignore the argument, so every candidate would look reachable.
func discriminatesByPort(probe func(int) (string, bool)) bool {
	_, reachable := probe(canaryPort)
	return !reachable
}
