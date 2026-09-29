package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
)

// GateSocketEnv names the environment variable the shim reads to find the socket.
const GateSocketEnv = "AGENTD_GATE_SOCK"

// GateListener serves the gate over a unix socket rather than the HTTP API, so
// the shim never needs agentd's bearer token in an environment the agent can
// read. The socket is 0600 inside a 0700 directory.
type GateListener struct {
	gate *Gate
	ln   net.Listener
	path string

	mu     sync.Mutex
	turnID string
	closed bool
}

// ListenGate creates the socket and starts accepting. A stale socket from a
// previous run is removed first, or a restart would silently disable Guided mode.
func ListenGate(g *Gate, sockPath string) (*GateListener, error) {
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		return nil, fmt.Errorf("gate socket dir: %w", err)
	}
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listen on gate socket: %w", err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("gate socket mode: %w", err)
	}
	l := &GateListener{gate: g, ln: ln, path: sockPath}
	go l.accept()
	return l, nil
}

// Path is where the socket lives, for the shim's environment.
func (l *GateListener) Path() string { return l.path }

// SetTurn sets the turn subsequent connections belong to. One value suffices
// because the supervisor runs turns one at a time.
func (l *GateListener) SetTurn(id string) {
	l.mu.Lock()
	l.turnID = id
	l.mu.Unlock()
}

func (l *GateListener) currentTurn() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.turnID
}

func (l *GateListener) accept() {
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			l.mu.Lock()
			closed := l.closed
			l.mu.Unlock()
			if !closed {
				// Not a deliberate Close: this silently ends Guided mode, so log it.
				log.Printf("[agentd] gate: accept: %v", err)
			}
			return
		}
		go func() {
			defer func() { _ = conn.Close() }()
			// ServeMCP blocks while a human decides, so each connection gets its own goroutine.
			_ = l.gate.ServeMCP(context.Background(), conn, conn, l.currentTurn())
		}()
	}
}

func (l *GateListener) Close() error {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	err := l.ln.Close()
	_ = os.Remove(l.path)
	return err
}

// RunGateShim is the whole gate subprocess: connect, copy both ways, exit. A
// missing socket is an error, since a prompt tool that cannot answer means no
// tool use rather than unreviewed use.
func RunGateShim(sockPath string, stdin io.Reader, stdout io.Writer) error {
	if sockPath == "" {
		sockPath = os.Getenv(GateSocketEnv)
	}
	if sockPath == "" {
		return fmt.Errorf("no gate socket: set %s", GateSocketEnv)
	}
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return fmt.Errorf("dial gate socket %s: %w", sockPath, err)
	}
	defer func() { _ = conn.Close() }()

	// Half-close on stdin EOF, then drain the reply side; returning when either
	// direction finishes would lose a decision still in flight.
	go func() {
		_, _ = io.Copy(conn, stdin)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
			return
		}
		// No half-close available: close outright rather than hang.
		_ = conn.Close()
	}()
	_, err = io.Copy(stdout, conn)
	return err
}

// WriteGateConfig writes the --mcp-config file that points claude at the shim.
// selfPath is the agentd binary itself, run in gate mode.
func WriteGateConfig(dir, selfPath, sockPath string) (string, error) {
	cfg := map[string]any{
		"mcpServers": map[string]any{
			GateServerName: map[string]any{
				"command": selfPath,
				"args":    []string{"-gate"},
				// The socket path goes in the environment, not the command line,
				// which is world-readable via /proc.
				"env": map[string]string{GateSocketEnv: sockPath},
			},
		},
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, ".pilldrops-gate.mcp.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return "", fmt.Errorf("write gate config: %w", err)
	}
	return p, nil
}
