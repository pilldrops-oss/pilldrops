package docker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// engine is a minimal Docker Engine API client used on the hot paths (exec,
// top, inspect, stats) instead of forking the CLI. nil when the socket is
// unreachable, in which case every caller falls back to the CLI.
type engine struct {
	http *http.Client
	// sock is kept for execAttach, which must dial raw.
	sock string
	// base is a placeholder host; net/http needs a valid URL even though the
	// dialer ignores it.
	base string
}

// dial opens a raw connection to the daemon socket.
func (e *engine) dial(ctx context.Context) (net.Conn, error) {
	return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", e.sock)
}

// engineAPIVersion is pinned old (Docker 20.10) on purpose: every endpoint used
// here predates it, and an old version is forwards compatible.
const engineAPIVersion = "v1.41"

// newEngine connects to the daemon the CLI uses, or returns nil. It follows the
// CLI's own precedence (DOCKER_HOST, DOCKER_CONTEXT, current context, default
// socket) because on a multi-context host the default socket is the wrong daemon.
func newEngine() *engine {
	sock, ok := resolveDockerSocket()
	if !ok {
		return nil
	}
	e := &engine{
		base: "http://docker",
		sock: sock,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", sock)
				},
				MaxIdleConns:        32,
				MaxIdleConnsPerHost: 32,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !e.sameDaemon(ctx) {
		return nil
	}
	return e
}

// sameDaemon reports whether this socket and the CLI reach the same daemon, by
// comparing daemon IDs.
func (e *engine) sameDaemon(ctx context.Context) bool {
	var info struct {
		ID string `json:"ID"`
	}
	if err := e.get(ctx, "/info", &info); err != nil || info.ID == "" {
		return false
	}
	out, _, code, err := dockerOut(ctx, "info", "--format", "{{.ID}}")
	if err != nil || code != 0 {
		// No CLI to disagree with or fall back to.
		return true
	}
	cliID := strings.TrimSpace(out)
	if cliID == "" {
		return true
	}
	if cliID != info.ID {
		slog.Warn("this socket belongs to a different daemon than the CLI; using the CLI for "+
			"every operation", "component", "docker", "socket", e.sock,
			"engine_daemon", info.ID, "cli_daemon", cliID)
		return false
	}
	return true
}

// resolveDockerSocket finds the unix socket the CLI would use. tcp:// and
// ssh:// endpoints are left to the CLI.
func resolveDockerSocket() (string, bool) {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return unixPath(h)
	}
	if name := os.Getenv("DOCKER_CONTEXT"); name != "" {
		if host, ok := contextEndpoint(name); ok {
			return unixPath(host)
		}
		return "", false
	}
	if name := currentContextName(); name != "" && name != "default" {
		if host, ok := contextEndpoint(name); ok {
			return unixPath(host)
		}
	}
	return usableSocket("/var/run/docker.sock")
}

// unixPath extracts the socket path from a unix:// endpoint.
func unixPath(host string) (string, bool) {
	if !strings.HasPrefix(host, "unix://") {
		return "", false
	}
	return usableSocket(strings.TrimPrefix(host, "unix://"))
}

// usableSocket reports whether path is a socket we can try.
func usableSocket(path string) (string, bool) {
	fi, err := os.Stat(path) // follows symlinks, which /var/run/docker.sock usually is
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		return "", false
	}
	return path, true
}

// currentContextName reads the active context from the CLI's config.
func currentContextName() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(home, ".docker", "config.json"))
	if err != nil {
		return ""
	}
	var cfg struct {
		CurrentContext string `json:"currentContext"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return ""
	}
	return cfg.CurrentContext
}

// contextEndpoint reads a named context's docker endpoint. The CLI stores
// context metadata under the SHA-256 of the context name.
func contextEndpoint(name string) (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256([]byte(name))
	metaPath := filepath.Join(home, ".docker", "contexts", "meta",
		hex.EncodeToString(sum[:]), "meta.json")
	b, err := os.ReadFile(metaPath)
	if err != nil {
		return "", false
	}
	var meta struct {
		Endpoints struct {
			Docker struct {
				Host string `json:"Host"`
			} `json:"docker"`
		} `json:"Endpoints"`
	}
	if json.Unmarshal(b, &meta) != nil || meta.Endpoints.Docker.Host == "" {
		return "", false
	}
	return meta.Endpoints.Docker.Host, true
}

// url builds a request URL for an API path.
func (e *engine) url(path string, q url.Values) string {
	u := e.base + "/" + engineAPIVersion + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// do issues a request and returns the response for the caller to consume.
func (e *engine) do(ctx context.Context, method, path string, q url.Values, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, e.url(path, q), rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("docker api %s %s: %s: %s",
			method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}

// get issues a GET and decodes a JSON response into out (nil to discard).
func (e *engine) get(ctx context.Context, path string, out any) error {
	return e.getQ(ctx, path, nil, out)
}

func (e *engine) getQ(ctx context.Context, path string, q url.Values, out any) error {
	resp, err := e.do(ctx, http.MethodGet, path, q, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxEngineResponseBytes)).Decode(out)
}

// maxEngineResponseBytes caps a decoded API response.
const maxEngineResponseBytes = 8 << 20

// execConfig is the subset of Docker's exec-create body this uses.
type execConfig struct {
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	AttachStdin  bool     `json:"AttachStdin,omitempty"`
	Tty          bool     `json:"Tty"`
	Cmd          []string `json:"Cmd"`
	Env          []string `json:"Env,omitempty"`
	WorkingDir   string   `json:"WorkingDir,omitempty"`
	User         string   `json:"User,omitempty"`
	Detach       bool     `json:"-"`
}

// Exec runs argv in a container. A non-zero exit is a result, not an error,
// same as dockerOut.
func (e *engine) Exec(ctx context.Context, cid string, cfg execConfig, stdin io.Reader) (stdout, stderr string, code int, err error) {
	var so, se bytes.Buffer
	code, err = e.ExecTo(ctx, cid, cfg, stdin, &so, &se)
	return so.String(), se.String(), code, err
}

// ExecTo runs an exec, writing its output to stdout and stderr as the container
// produces it, and returns the exit code once it has finished.
func (e *engine) ExecTo(ctx context.Context, cid string, cfg execConfig, stdin io.Reader, stdout, stderr io.Writer) (code int, err error) {
	cfg.AttachStdout, cfg.AttachStderr = true, true
	cfg.AttachStdin = stdin != nil

	var created struct {
		ID string `json:"Id"`
	}
	resp, err := e.do(ctx, http.MethodPost, "/containers/"+cid+"/exec", nil, cfg)
	if err != nil {
		return 0, err
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&created); err != nil {
		resp.Body.Close()
		return 0, fmt.Errorf("exec create: %w", err)
	}
	resp.Body.Close()

	if err := e.execAttach(ctx, created.ID, stdin, stdout, stderr); err != nil {
		return 0, fmt.Errorf("exec stream: %w", err)
	}

	var insp struct {
		ExitCode int  `json:"ExitCode"`
		Running  bool `json:"Running"`
	}
	if err := e.get(ctx, "/exec/"+created.ID+"/json", &insp); err != nil {
		return 0, err
	}
	return insp.ExitCode, nil
}

// execAttach starts an exec and pumps its bidirectional stream. Hand-rolled
// HTTP because the start request hijacks the connection and net/http gives no
// access to the raw conn, so stdin would have nowhere to go.
func (e *engine) execAttach(ctx context.Context, execID string, stdin io.Reader, stdout, stderr io.Writer) error {
	conn, err := e.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }() //nolint:errcheck // best-effort cleanup

	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	// A deadline alone does not cover an early cancel.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	const body = `{"Detach":false,"Tty":false}`
	if _, err := fmt.Fprintf(conn,
		"POST /%s/exec/%s/start HTTP/1.1\r\nHost: docker\r\nContent-Type: application/json\r\n"+
			"Content-Length: %d\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n%s",
		engineAPIVersion, execID, len(body), body); err != nil {
		return fmt.Errorf("exec start request: %w", err)
	}

	br := bufio.NewReader(conn)
	// Never sent; only tells http.ReadResponse how the body is framed.
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.url("/exec/"+execID+"/start", nil), nil)
	resp, err := http.ReadResponse(br, req) //nolint:bodyclose // the body is the hijacked conn, closed by the deferred conn.Close above
	if err != nil {
		return fmt.Errorf("exec start response: %w", err)
	}
	// 101 (upgraded) and 200 (plain hijack) are both success.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusSwitchingProtocols {
		msg, _ := io.ReadAll(io.LimitReader(br, 4<<10))
		return fmt.Errorf("exec start: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}

	// Half-close after stdin so the process sees EOF.
	if stdin != nil {
		if _, err := io.Copy(conn, stdin); err != nil {
			return fmt.Errorf("exec stdin: %w", err)
		}
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}
	return demux(br, stdout, stderr)
}

// demux splits Docker's multiplexed stream: an 8-byte header (stream type in
// byte 0, big-endian length in bytes 4..8) precedes each payload.
func demux(r io.Reader, stdout, stderr io.Writer) error {
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil // clean end of stream
			}
			return err
		}
		n := binary.BigEndian.Uint32(hdr[4:8])
		if n == 0 {
			continue
		}
		w := stdout
		if hdr[0] == 2 { // 2 = stderr; 0 = stdin echo, 1 = stdout
			w = stderr
		}
		if _, err := io.CopyN(w, r, int64(n)); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// containerJSON is the subset of a container inspect this needs.
type containerJSON struct {
	ID     string `json:"Id"`
	SizeRw int64  `json:"SizeRw"`
	State  struct {
		Running   bool `json:"Running"`
		OOMKilled bool `json:"OOMKilled"`
		ExitCode  int  `json:"ExitCode"`
	} `json:"State"`
}

// State reports whether the container still runs and whether it was OOM-killed.
func (e *engine) State(ctx context.Context, cid string) (running, oomKilled bool, err error) {
	var c containerJSON
	if err := e.get(ctx, "/containers/"+cid+"/json", &c); err != nil {
		return false, false, err
	}
	return c.State.Running, c.State.OOMKilled, nil
}

// InspectSize returns the container's writable-layer size in bytes. Expensive:
// the daemon walks the layer.
func (e *engine) InspectSize(ctx context.Context, cid string) (int64, error) {
	var c containerJSON
	if err := e.getQ(ctx, "/containers/"+cid+"/json", url.Values{"size": {"true"}}, &c); err != nil {
		return 0, err
	}
	return c.SizeRw, nil
}

// statsJSON is the subset of a one-shot stats sample this needs.
type statsJSON struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage  uint64   `json:"total_usage"`
			PercpuUsage []uint64 `json:"percpu_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
		OnlineCPUs     uint32 `json:"online_cpus"`
	} `json:"cpu_stats"`
	PreCPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
	} `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
}

// Stats returns one CPU/memory sample. one-shot must stay false: without the
// precpu_stats it would omit, CPU% is always zero.
func (e *engine) Stats(ctx context.Context, cid string) (cpuPct float64, memMiB int, err error) {
	var s statsJSON
	q := url.Values{"stream": {"false"}, "one-shot": {"false"}}
	if err := e.getQ(ctx, "/containers/"+cid+"/stats", q, &s); err != nil {
		return 0, 0, err
	}
	return cpuPercent(s), memUsedMiB(s), nil
}

// cpuPercent mirrors the CLI's calculation: the container's share of the delta
// in total system CPU time, scaled by the number of CPUs.
func cpuPercent(s statsJSON) float64 {
	cpuDelta := float64(s.CPUStats.CPUUsage.TotalUsage) - float64(s.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(s.CPUStats.SystemCPUUsage) - float64(s.PreCPUStats.SystemCPUUsage)
	if cpuDelta <= 0 || sysDelta <= 0 {
		return 0
	}
	cpus := float64(s.CPUStats.OnlineCPUs)
	if cpus == 0 {
		cpus = float64(len(s.CPUStats.CPUUsage.PercpuUsage))
	}
	if cpus == 0 {
		cpus = 1
	}
	return (cpuDelta / sysDelta) * cpus * 100
}

// memUsedMiB reports resident memory minus page cache, as the CLI does. cgroup
// v1 spells the key "total_inactive_file".
func memUsedMiB(s statsJSON) int {
	usage := s.MemoryStats.Usage
	for _, k := range []string{"inactive_file", "total_inactive_file"} {
		if v, ok := s.MemoryStats.Stats[k]; ok && v < usage {
			usage -= v
			break
		}
	}
	// Rounded up: a small container under 1 MiB is using memory, not none.
	return int((usage + 1<<20 - 1) >> 20)
}

// Kill signals the container's main process; the Engine API has no per-pid
// signal.
func (e *engine) Kill(ctx context.Context, cid, signal string) error {
	q := url.Values{}
	if signal != "" {
		q.Set("signal", signal)
	}
	resp, err := e.do(ctx, http.MethodPost, "/containers/"+cid+"/kill", q, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}
