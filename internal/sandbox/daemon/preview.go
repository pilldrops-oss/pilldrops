package daemon

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// previewWakeReadyTimeout bounds the post-wake poll of the app port; tests lower it.
var previewWakeReadyTimeout = 3 * time.Second

// waitDialable polls addr with TCP dials until it accepts or the deadline
// elapses. Never errors; the proxy's error page is the fallback.
func waitDialable(ctx context.Context, addr string, timeout time.Duration) {
	poll := time.NewTicker(previewProbeInterval)
	defer poll.Stop()
	deadline := time.Now().Add(timeout)
	for {
		if ctx.Err() != nil {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		remaining := time.Until(deadline)
		dialTimeout := 200 * time.Millisecond
		if remaining < dialTimeout {
			dialTimeout = remaining
		}
		c, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = c.Close()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
		}
	}
}

// StartServer launches a long-lived web server in a running sandbox. The
// command must bind $PORT. publicPort is the port the preview proxy routes; 0
// leaves the sandbox unrestricted.
func (d *Daemon) StartServer(ctx context.Context, id, cmd string, publicPort int, env map[string]string) error {
	// Runtimes default $PORT to the template app port, so set it to the
	// published port unless the caller provided one.
	if publicPort > 0 {
		if env == nil {
			env = map[string]string{}
		}
		if _, ok := env["PORT"]; !ok {
			env["PORT"] = strconv.Itoa(publicPort)
		}
	}
	d.mu.Lock()
	sb, ok := d.boxes[id]
	if !ok {
		d.mu.Unlock()
		return rt.ErrNotFound
	}
	if !canTransition(sb.State, "exec") {
		d.mu.Unlock()
		return rt.ErrPaused
	}
	sb.PublicPort = publicPort
	sb.StartCmd = cmd
	sb.Env = env
	inst := sb.inst
	rec := d.recordOf(sb)
	d.mu.Unlock()
	// Persist now so a restart can replay the published app.
	d.persistRec(ctx, rec)
	return inst.StartServer(ctx, cmd, env)
}

// SetPublicPort publishes the app port for a sandbox whose server is built into
// the template image rather than launched via StartServer.
func (d *Daemon) SetPublicPort(ctx context.Context, id string, port int) error {
	d.mu.Lock()
	sb, ok := d.boxes[id]
	if !ok {
		d.mu.Unlock()
		return rt.ErrNotFound
	}
	sb.PublicPort = port
	rec := d.recordOf(sb)
	d.mu.Unlock()
	d.persistRec(ctx, rec)
	return nil
}

// TerminalToken returns the credential envd's web terminal requires, or "" when
// the runtime has none (unauthenticated envd is valid). Does not Touch the sandbox.
func (d *Daemon) TerminalToken(_ context.Context, id string) (string, error) {
	d.mu.RLock()
	sb, ok := d.boxes[id]
	var inst rt.Instance
	if ok {
		inst = sb.inst
	}
	d.mu.RUnlock()
	if !ok {
		return "", rt.ErrNotFound
	}
	a, isAuth := inst.(rt.TerminalAuthorizer)
	if !isAuth {
		return "", nil
	}
	return a.TerminalToken()
}

// PreviewTarget returns the address the preview proxy should dial, waking a
// paused sandbox first. org is checked here, not in callers, so an unwrapped
// caller fails closed; "" is refused, never a wildcard.
func (d *Daemon) PreviewTarget(ctx context.Context, id string, port int, org string) (string, error) {
	d.mu.RLock()
	sb, ok := d.boxes[id]
	state := StateStopped
	publicPort := 0
	ownerOrg := ""
	var declared []int
	if ok {
		state = sb.State
		publicPort = sb.PublicPort
		ownerOrg = sb.OrgID
		declared = DeclaredPorts(sb.Template, sb.Ports)
	}
	d.mu.RUnlock()
	if !ok {
		return "", rt.ErrNotFound
	}
	// Check before the wake: a foreign request must not resume and bill the sandbox.
	if org == "" || org != ownerOrg {
		return "", rt.ErrForeignOrg
	}

	// Single-port runtimes publish one host port plus the declared ports, so
	// any other port would silently serve the wrong app. Platform ports
	// (terminal, agent) are exempt.
	multiPort := d.cfg.Runtime.Capabilities().MultiPort
	if !multiPort && publicPort != 0 && port != publicPort && !rt.IsPlatformPort(port) && !slices.Contains(declared, port) {
		return "", fmt.Errorf("%w: sandbox %s publishes port %d, not %d", rt.ErrNoAppOnPort, id, publicPort, port)
	}

	woke := state == StatePaused || state == StateStandby || state == StatePausing || state == StateResuming
	if woke {
		if err := d.ensureRunning(ctx, id); err != nil {
			return "", err
		}
	}
	d.Touch(id)

	d.mu.RLock()
	_, alive := d.boxes[id]
	inst := sb.inst
	d.mu.RUnlock()
	if !alive {
		return "", rt.ErrNotFound // destroyed during wake
	}
	if inst == nil {
		return "", rt.ErrPaused
	}

	// The terminal is served only on its own port, never as a fallback for an
	// app port nothing listens on.
	if port == rt.TerminalPort {
		target, err := inst.TerminalTarget()
		if err != nil {
			return "", err
		}
		if woke && target != "" {
			waitDialable(ctx, target, previewWakeReadyTimeout)
		}
		return target, nil
	}

	target, err := inst.ListenTarget(port)
	if err != nil {
		return "", err
	}

	// Give the restarted app a bounded window to bind before the first post-wake request.
	if woke && target != "" {
		waitDialable(ctx, target, previewWakeReadyTimeout)
	}
	return target, nil
}

const previewProbeInterval = 50 * time.Millisecond

// DeclaredPorts is every port a sandbox published beyond the app port: the
// request's own list plus the catalogue template's. The request list lives
// only in memory (ponytail: a restarted node keeps the catalogue's ports and
// forgets a caller-declared one; persist Ports if that ever bites).
func DeclaredPorts(template string, requested []int) []int {
	out := append([]int(nil), requested...)
	if t, ok := templates.Resolve(template); ok {
		out = append(out, t.Ports...)
	}
	return out
}
