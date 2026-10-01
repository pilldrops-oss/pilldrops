package main

// These tests drive the local-runtime exec/process/metrics/logs/lifecycle/
// destroy/info handlers through chi and check that every one enforces
// sb.UserID == ownerID(r).

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	pb "github.com/pilldrops-oss/pilldrops/proto/envdpb"
)

// fakeExecStream stands in for the envd exec stream. DevManager.Exec only calls
// Recv, so the nil embedded ClientStream is never dereferenced.
type fakeExecStream struct {
	grpc.ClientStream
	msgs []*pb.ExecuteResponse
	i    int
}

func (s *fakeExecStream) Recv() (*pb.ExecuteResponse, error) {
	if s.i >= len(s.msgs) {
		return nil, io.EOF
	}
	m := s.msgs[s.i]
	s.i++
	return m, nil
}

// fakeEnvdClient satisfies pb.EnvdServiceClient with no network so
// DevManager.Exec can run in a unit test. Only Execute is exercised.
type fakeEnvdClient struct {
	// Embedded so the fake keeps compiling as the service grows; only the
	// methods DevManager.Exec touches are defined.
	pb.EnvdServiceClient
	stdout   string
	exitCode int32
	calls    int
	last     *pb.ExecuteRequest
}

func (f *fakeEnvdClient) Health(ctx context.Context, in *pb.HealthRequest, opts ...grpc.CallOption) (*pb.HealthResponse, error) {
	return &pb.HealthResponse{}, nil
}

func (f *fakeEnvdClient) Execute(ctx context.Context, in *pb.ExecuteRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[pb.ExecuteResponse], error) {
	f.calls++
	f.last = in
	return &fakeExecStream{msgs: []*pb.ExecuteResponse{
		{Type: pb.OutputType_STDOUT, Data: []byte(f.stdout)},
		{Type: pb.OutputType_EXIT, ExitCode: f.exitCode},
	}}, nil
}

// sandboxActionRouter pins ownerID to a literal owner; see localFSRouter for
// why two distinct owners are needed.
func sandboxActionRouter(mgr *DevManager, execHistory *ExecHistory, usage *UsageTracker, owner string) chi.Router {
	r := chi.NewRouter()
	ownerID := func(*http.Request) string { return owner }
	registerSandboxActionRoutes(r, mgr, ownerID, execHistory, usage)
	return r
}

func TestSandboxActions_CrossOwner404(t *testing.T) {
	workDir := t.TempDir()
	mgr := &DevManager{
		sandboxes: map[string]*DevSandbox{
			"sb-1": {
				ID:      "sb-1",
				UserID:  "u:alice",
				WorkDir: workDir,
				client:  &fakeEnvdClient{stdout: "hello\n", exitCode: 0},
			},
		},
	}
	execHistory := NewExecHistory()
	usage := NewUsageTracker()

	rBob := sandboxActionRouter(mgr, execHistory, usage, "u:bob")

	bobCases := []*http.Request{
		httptest.NewRequest(http.MethodPost, "/v1/sandboxes/sb-1/exec", strings.NewReader(`{"cmd":"echo hi"}`)),
		httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb-1", nil),
		httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb-1/logs", nil),
		httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb-1/metrics", nil),
		httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb-1/process/list", nil),
		httptest.NewRequest(http.MethodPost, "/v1/sandboxes/sb-1/process/signal", strings.NewReader(`{"pid":1,"signal":"SIGTERM"}`)),
		httptest.NewRequest(http.MethodPost, "/v1/sandboxes/sb-1/pause", nil),
		httptest.NewRequest(http.MethodPost, "/v1/sandboxes/sb-1/resume", nil),
		httptest.NewRequest(http.MethodPost, "/v1/sandboxes/sb-1/timeout", strings.NewReader(`{"timeout":60}`)),
		httptest.NewRequest(http.MethodPost, "/v1/sandboxes/sb-1/refresh", nil),
		httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb-1/history", nil),
		httptest.NewRequest(http.MethodDelete, "/v1/sandboxes/sb-1", nil),
	}
	for _, req := range bobCases {
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		rBob.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("bob %s %s: got %d, want 404: %s", req.Method, req.URL.String(), w.Code, w.Body.String())
		}
	}

	// Proves the ownership check runs before the action: nothing recorded,
	// nothing destroyed.
	if _, ok := mgr.GetSandbox("sb-1"); !ok {
		t.Fatalf("bob's cross-owner DELETE destroyed alice's sandbox despite the 404")
	}
	if len(execHistory.BySandbox("sb-1")) != 0 {
		t.Fatalf("bob's cross-owner exec was recorded in alice's history despite the 404")
	}

	// Regression guard: alice's own requests still work, including a real exec
	// round-trip through the fake envd.
	rAlice := sandboxActionRouter(mgr, execHistory, usage, "u:alice")

	execReq := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/sb-1/exec", strings.NewReader(`{"cmd":"echo hello"}`))
	execReq.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rAlice.ServeHTTP(w, execReq)
	if w.Code != http.StatusOK {
		t.Fatalf("alice exec: got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "hello") {
		t.Fatalf("alice exec: unexpected body (fake envd output missing): %s", w.Body.String())
	}
	if len(execHistory.BySandbox("sb-1")) != 1 {
		t.Fatalf("alice's exec was not recorded in her own history: n=%d", len(execHistory.BySandbox("sb-1")))
	}

	if w := do(t, rAlice, "GET", "/v1/sandboxes/sb-1/metrics", ""); w.Code != http.StatusOK {
		t.Fatalf("alice metrics: got %d: %s", w.Code, w.Body.String())
	}
	if w := do(t, rAlice, "GET", "/v1/sandboxes/sb-1/logs", ""); w.Code != http.StatusOK {
		t.Fatalf("alice logs: got %d: %s", w.Code, w.Body.String())
	}
	if w := do(t, rAlice, "GET", "/v1/sandboxes/sb-1", ""); w.Code != http.StatusOK {
		t.Fatalf("alice info: got %d: %s", w.Code, w.Body.String())
	}
}

func postAs(t *testing.T, r http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// A relative cwd is inside the workspace, not relative to wherever envd runs.
func TestExecRelativeCwdResolvesInWorkspace(t *testing.T) {
	envd := &fakeEnvdClient{}
	mgr := &DevManager{sandboxes: map[string]*DevSandbox{"sb-1": {ID: "sb-1", UserID: "u:a", WorkDir: "/ws/sb-1", client: envd}}}
	r := sandboxActionRouter(mgr, NewExecHistory(), NewUsageTracker(), "u:a")
	for cwd, want := range map[string]string{"": "/ws/sb-1", "sub": "/ws/sb-1/sub", "/tmp": "/tmp"} {
		if w := postAs(t, r, "/v1/sandboxes/sb-1/exec", `{"cmd":"pwd","cwd":"`+cwd+`"}`); w.Code != http.StatusOK {
			t.Fatalf("cwd %q: %d %s", cwd, w.Code, w.Body.String())
		}
		if envd.last.Cwd != want {
			t.Errorf("cwd %q reached envd as %q, want %q", cwd, envd.last.Cwd, want)
		}
	}
}

// Refresh extends a short lease and never cuts a long one.
func TestRefreshNeverShortensTheLease(t *testing.T) {
	long := time.Now().Add(time.Hour)
	mgr := &DevManager{sandboxes: map[string]*DevSandbox{"sb-1": {ID: "sb-1", UserID: "u:a", ExpiresAt: long}}}
	r := sandboxActionRouter(mgr, NewExecHistory(), NewUsageTracker(), "u:a")
	postAs(t, r, "/v1/sandboxes/sb-1/refresh", "")
	if got := mgr.sandboxes["sb-1"].ExpiresAt; !got.Equal(long) {
		t.Fatalf("refresh moved an hour-long lease to %v", got)
	}
	mgr.sandboxes["sb-1"].ExpiresAt = time.Now().Add(time.Minute)
	postAs(t, r, "/v1/sandboxes/sb-1/refresh", "")
	if got := time.Until(mgr.sandboxes["sb-1"].ExpiresAt); got < 4*time.Minute {
		t.Fatalf("refresh left %v on a one-minute lease, want about 5m", got)
	}
}

// The local runtime cannot pause, so it must not claim it did.
func TestPauseAndResumeSayTheyNeedTheFleet(t *testing.T) {
	mgr := &DevManager{sandboxes: map[string]*DevSandbox{"sb-1": {ID: "sb-1", UserID: "u:a"}}}
	r := sandboxActionRouter(mgr, NewExecHistory(), NewUsageTracker(), "u:a")
	for _, a := range []string{"pause", "resume"} {
		if w := postAs(t, r, "/v1/sandboxes/sb-1/"+a, ""); w.Code != http.StatusNotImplemented {
			t.Errorf("%s = %d, want 501: %s", a, w.Code, w.Body.String())
		}
	}
}

// The signal is interpolated into a shell command, so only a name or number reaches it.
func TestSignalIsValidatedBeforeTheShell(t *testing.T) {
	envd := &fakeEnvdClient{}
	mgr := &DevManager{sandboxes: map[string]*DevSandbox{"sb-1": {ID: "sb-1", UserID: "u:a", WorkDir: t.TempDir(), client: envd}}}
	r := sandboxActionRouter(mgr, NewExecHistory(), NewUsageTracker(), "u:a")
	if w := postAs(t, r, "/v1/sandboxes/sb-1/process/signal", `{"pid":1,"signal":"0 1; touch /tmp/x #"}`); w.Code != http.StatusBadRequest || envd.calls != 0 {
		t.Fatalf("injected signal: %d, envd calls %d; want 400 and none", w.Code, envd.calls)
	}
	for _, sig := range []string{"KILL", "SIGTERM", "9", ""} {
		if w := postAs(t, r, "/v1/sandboxes/sb-1/process/signal", `{"pid":1,"signal":"`+sig+`"}`); w.Code != http.StatusOK {
			t.Errorf("signal %q = %d, want 200", sig, w.Code)
		}
	}
}

// Listing is per owner: another tenant's sandboxes are not even visible.
func TestListIsScopedToTheOwner(t *testing.T) {
	mgr := &DevManager{sandboxes: map[string]*DevSandbox{
		"sb-a": {ID: "sb-a", UserID: "u:a"}, "sb-b": {ID: "sb-b", UserID: "u:b"},
	}}
	rows := mgr.List("u:a")
	if len(rows) != 1 || rows[0]["id"] != "sb-a" {
		t.Fatalf("List(u:a) = %v, want only sb-a", rows)
	}
}

// Destroying a sandbox stops its app server and whatever that spawned; they
// used to outlive it, holding the port.
func TestDestroyStopsTheAppServer(t *testing.T) {
	mgr := &DevManager{log: zap.NewNop(), sandboxes: map[string]*DevSandbox{
		"sb-1": {ID: "sb-1", UserID: "u:a", WorkDir: t.TempDir(), Port: 1, cancel: func() {}},
	}}
	pid, _, err := mgr.StartProcess("sb-1", "sleep 30 & sleep 30", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(-pid, 0); err != nil {
		t.Fatalf("the app is not running as its own process group: %v", err)
	}
	mgr.Destroy(t.Context(), "sb-1")
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(-pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatal("the app's process group is still alive after Destroy")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A port something already listens on is never handed to a new sandbox.
func TestFreePortSkipsABusyPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	busy := ln.Addr().(*net.TCPAddr).Port
	m := &DevManager{}
	m.nextPort.Store(int32(busy - 1))
	if got := m.freePort(); got == busy {
		t.Fatalf("freePort returned %d, which is in use", got)
	}
}

// The SDKs send {"seconds":N}; reading only "timeout" left the TTL untouched.
func TestSandboxTimeout_ReadsSecondsAndRefusesZero(t *testing.T) {
	start := time.Now().Add(time.Minute)
	sb := &DevSandbox{ID: "sb-1", UserID: "u:alice", ExpiresAt: start}
	mgr := &DevManager{sandboxes: map[string]*DevSandbox{"sb-1": sb}}
	r := sandboxActionRouter(mgr, NewExecHistory(), NewUsageTracker(), "u:alice")

	post := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/sb-1/timeout", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code == http.StatusBadRequest {
			var e map[string]string
			if ct := w.Header().Get("Content-Type"); ct != "application/json" || json.Unmarshal(w.Body.Bytes(), &e) != nil || e["error"] == "" {
				t.Errorf("%s: 400 came back as %q %q, want a JSON error", body, ct, w.Body.String())
			}
		}
		return w.Code
	}

	for _, body := range []string{`{}`, `{"seconds":0}`} {
		if code := post(body); code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", body, code)
		}
	}
	if !sb.ExpiresAt.Equal(start) {
		t.Fatalf("a refused /timeout moved the expiry to %v", sb.ExpiresAt)
	}

	for body, want := range map[string]time.Duration{`{"seconds":3600}`: time.Hour, `{"timeout":7200}`: 2 * time.Hour} {
		if code := post(body); code != http.StatusOK {
			t.Fatalf("%s = %d, want 200", body, code)
		}
		if d := time.Until(sb.ExpiresAt); d < want-time.Minute || d > want {
			t.Errorf("%s: expiry is %v out, want ~%v", body, d, want)
		}
	}
}
