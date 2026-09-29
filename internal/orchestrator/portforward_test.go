package orchestrator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// forwardServer is an orchestrator with a real store, two sandboxes on one
// node, and a plan assigned to org u:one so the limit is live.
func forwardServer(t *testing.T, runtime, template string) *Server {
	t.Helper()
	// This deployment has an ingress; another replica holds the sockets. Without
	// the declaration the API refuses every create, which is TestForwardsAreRefused-
	// WhereNothingBindsThem below.
	t.Setenv("FORWARD_INGRESS", "remote")
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"` + strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/") + `","state":"running"}`))
	}))
	t.Cleanup(node.Close)

	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "fwd.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	s := NewServer("fleet-key", "nodetoken")
	s.sandboxDomain = "sb.example.com"
	s.SetStore(st)
	if err := s.reg.Upsert(cluster.NodeUpsert{ID: "n1", URL: node.URL, Key: "kk"}, s.now()); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// The runtime is SetMeta's, not Upsert's; setting it on the upsert is silently dropped.
	s.reg.SetNodeMeta("n1", cluster.NodeMeta{Runtime: runtime, Isolation: true})
	for _, sb := range []struct{ id, org string }{{"sb1", "u:one"}, {"sb2", "u:two"}} {
		if err := st.SaveSandbox(t.Context(), store.SandboxRecord{
			ID: sb.id, OrgID: sb.org, Template: template, State: "running", PublicPort: 8000,
			NodeID: "n1", CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			t.Fatalf("save %s: %v", sb.id, err)
		}
		s.reg.Place(sb.id, "n1")
	}
	// "free" carries no MaxForwards, so the default of 3 applies.
	if err := st.SetPlan(t.Context(), "u:one", "free"); err != nil {
		t.Fatalf("plan: %v", err)
	}
	return s
}

func decodeJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return m
}

func TestForwardLimitAndReExposeAndTenancy(t *testing.T) {
	s := forwardServer(t, "firecracker", "base")

	// Three distinct guest ports are allowed on the default plan.
	seen := map[float64]int{}
	for i, port := range []string{"5432", "6379", "9000"} {
		w := apiReq(t, s, http.MethodPost, "u:one", "/v1/sandboxes/sb1/forwards", `{"port":`+port+`}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("post %s: %d %s", port, w.Code, w.Body.String())
		}
		body := decodeJSON(t, w)
		pub, _ := body["public_port"].(float64)
		if pub < 40000 || pub > 60000 {
			t.Fatalf("public_port %v outside the range", body["public_port"])
		}
		if prev, dup := seen[pub]; dup {
			t.Fatalf("public port %v handed out twice (calls %d and %d)", pub, prev, i)
		}
		seen[pub] = i
		// The endpoint host comes from the forwarding host, not the sandbox path.
		if ep, _ := body["endpoint"].(string); !strings.HasPrefix(ep, "sb.example.com:") {
			t.Fatalf("endpoint = %q", ep)
		}
	}

	// The fourth distinct port is refused, naming the plan and the count.
	w := apiReq(t, s, http.MethodPost, "u:one", "/v1/sandboxes/sb1/forwards", `{"port":1234}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("fourth forward: %d %s", w.Code, w.Body.String())
	}
	if b := w.Body.String(); !strings.Contains(b, "'free'") || !strings.Contains(b, "3") {
		t.Fatalf("refusal does not name the plan and the count: %s", b)
	}

	// Re-exposing an existing port is not a fourth forward, keeps the public
	// port, and updates the protocol set.
	first := apiReq(t, s, http.MethodGet, "u:one", "/v1/sandboxes/sb1/forwards", "")
	var listed struct {
		Forwards []struct {
			GuestPort  int  `json:"guest_port"`
			PublicPort int  `json:"public_port"`
			UDP        bool `json:"udp"`
		} `json:"forwards"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Forwards) != 3 {
		t.Fatalf("listed %d forwards, want 3: %s", len(listed.Forwards), first.Body.String())
	}
	was := listed.Forwards[0]
	again := apiReq(t, s, http.MethodPost, "u:one", "/v1/sandboxes/sb1/forwards",
		`{"port":`+strconv.Itoa(was.GuestPort)+`,"tcp":true,"udp":true}`)
	if again.Code != http.StatusOK {
		t.Fatalf("re-expose: %d %s", again.Code, again.Body.String())
	}
	body := decodeJSON(t, again)
	if int(body["public_port"].(float64)) != was.PublicPort {
		t.Fatalf("re-expose moved the public port %d -> %v", was.PublicPort, body["public_port"])
	}
	if body["udp"] != true {
		t.Fatalf("re-expose did not update the protocol set: %v", body)
	}

	// Another org sees a 404, on every verb, and cannot read the endpoints.
	for _, c := range []struct{ method, target, body string }{
		{http.MethodGet, "/v1/sandboxes/sb1/forwards", ""},
		{http.MethodPost, "/v1/sandboxes/sb1/forwards", `{"port":22}`},
		{http.MethodDelete, "/v1/sandboxes/sb1/forwards/" + strconv.Itoa(was.GuestPort), ""},
	} {
		w := apiReq(t, s, c.method, "u:two", c.target, c.body)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s as another org: %d %s", c.method, w.Code, w.Body.String())
		}
	}
	// Positive control: the owner's delete works and frees the row.
	if w := apiReq(t, s, http.MethodDelete, "u:one", "/v1/sandboxes/sb1/forwards/"+strconv.Itoa(was.GuestPort), ""); w.Code != http.StatusNoContent {
		t.Fatalf("owner delete: %d %s", w.Code, w.Body.String())
	}
	if n, _ := s.st.Load().CountPortForwards(t.Context(), "sb1"); n != 2 {
		t.Fatalf("after delete %d rows remain, want 2", n)
	}
}

// Docker publishes only the ports declared at create, so a forward to anything
// else is refused rather than accepted as a listener that can never connect.
func TestDockerForwardRefusesAnUndeclaredPort(t *testing.T) {
	s := forwardServer(t, "docker", "postgres")

	if w := apiReq(t, s, http.MethodPost, "u:one", "/v1/sandboxes/sb1/forwards", `{"port":5432}`); w.Code != http.StatusCreated {
		t.Fatalf("declared port: %d %s", w.Code, w.Body.String())
	}
	w := apiReq(t, s, http.MethodPost, "u:one", "/v1/sandboxes/sb1/forwards", `{"port":5433}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("undeclared port: %d %s", w.Code, w.Body.String())
	}
	b := w.Body.String()
	if !strings.Contains(b, "port_not_declared") || !strings.Contains(b, "docker") {
		t.Fatalf("refusal names neither the code nor the runtime: %s", b)
	}
	// Docker publishes container ports as TCP only, so a UDP forward is refused
	// rather than bound to a socket nothing can answer.
	u := apiReq(t, s, http.MethodPost, "u:one", "/v1/sandboxes/sb1/forwards", `{"port":5432,"udp":true}`)
	if u.Code != http.StatusConflict || !strings.Contains(u.Body.String(), "udp_not_supported") {
		t.Fatalf("docker udp forward: %d %s", u.Code, u.Body.String())
	}

	// Positive controls on a firecracker node: the same undeclared port and the
	// same UDP request both succeed there.
	fc := forwardServer(t, "firecracker", "postgres")
	if w := apiReq(t, fc, http.MethodPost, "u:one", "/v1/sandboxes/sb1/forwards", `{"port":5433}`); w.Code != http.StatusCreated {
		t.Fatalf("firecracker undeclared port: %d %s", w.Code, w.Body.String())
	}
	if w := apiReq(t, fc, http.MethodPost, "u:one", "/v1/sandboxes/sb1/forwards", `{"port":5432,"udp":true}`); w.Code != http.StatusCreated {
		t.Fatalf("firecracker udp forward: %d %s", w.Code, w.Body.String())
	}
}

// The endpoint host is FORWARD_PUBLIC_HOST when both it and the sandbox domain
// are set, or a fleet with a separate ingress host publishes the wrong address.
func TestForwardEndpointHostComesFromItsOwnEnv(t *testing.T) {
	s := forwardServer(t, "firecracker", "base")
	t.Setenv("FORWARD_PUBLIC_HOST", "fwd.example.net")

	w := apiReq(t, s, http.MethodPost, "u:one", "/v1/sandboxes/sb1/forwards", `{"port":5432}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("post: %d %s", w.Code, w.Body.String())
	}
	if ep, _ := decodeJSON(t, w)["endpoint"].(string); !strings.HasPrefix(ep, "fwd.example.net:") {
		t.Fatalf("endpoint = %q, want the FORWARD_PUBLIC_HOST host, not the sandbox domain %q", ep, s.sandboxDomain)
	}

	// And the sandbox detail carries it, so a client needs one call, not two.
	got := apiReq(t, s, http.MethodGet, "u:one", "/v1/sandboxes/sb1", "")
	if got.Code != http.StatusOK {
		t.Fatalf("get: %d %s", got.Code, got.Body.String())
	}
	if !strings.Contains(got.Body.String(), "fwd.example.net:") {
		t.Fatalf("sandbox detail has no forwards: %s", got.Body.String())
	}
}

// The default deployment binds nothing: FORWARD_INGRESS is unset, so no
// replica runs the ingress and the endpoint a create hands back refuses every
// connection. It used to answer 201 anyway.
func TestForwardsAreRefusedWhereNothingBindsThem(t *testing.T) {
	s := forwardServer(t, "firecracker", "base")
	t.Setenv("FORWARD_INGRESS", "") // after the fixture: the default, nothing declared

	w := apiReq(t, s, http.MethodPost, "u:one", "/v1/sandboxes/sb1/forwards", `{"port":5432}`)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("create with no ingress anywhere = %d, want 501: %s", w.Code, w.Body.String())
	}
	if code, _ := decodeJSON(t, w)["code"].(string); code != "no_forward_ingress" {
		t.Errorf("refusal code = %q, want no_forward_ingress: %s", code, w.Body.String())
	}
	// And no row was written, so a replica that later does run the ingress does
	// not resurrect it on the next reconcile.
	if list := apiReq(t, s, http.MethodGet, "u:one", "/v1/sandboxes/sb1/forwards", ""); strings.Contains(list.Body.String(), "5432") {
		t.Errorf("a refused create still left a row: %s", list.Body.String())
	}
	// The ingress replica itself is allowed, with nothing else set.
	t.Setenv("FORWARD_INGRESS", "1")
	if w := apiReq(t, s, http.MethodPost, "u:one", "/v1/sandboxes/sb1/forwards", `{"port":5432}`); w.Code != http.StatusCreated {
		t.Fatalf("create on the ingress replica = %d, want 201: %s", w.Code, w.Body.String())
	}
}
