package main

// These tests drive the local-runtime exec/process/metrics/logs/lifecycle/
// destroy/info handlers through chi and check that every one enforces
// sb.UserID == ownerID(r).

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
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
