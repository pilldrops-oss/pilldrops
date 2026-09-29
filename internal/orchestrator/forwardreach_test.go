package orchestrator

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// reachFixture is an orchestrator running the forward ingress on a free port,
// with one sandbox (sb1, org u:one) owned by the key it returns. save() changes
// the row's visibility, post() creates a forward as its owner, and port is the
// bound public port.
func reachFixture(t *testing.T) (s *Server, st *store.Store, port int,
	save func(string), post func(string) *httptest.ResponseRecorder) {
	t.Helper()
	t.Setenv("FORWARD_INGRESS", "1")
	t.Setenv("FORWARD_PUBLIC_HOST", "fwd.example.net")

	opened := 0
	node := fakeTunnelNode(t, &opened)

	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "reach.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	s = NewServer("fleet-key", "nodetoken")
	s.SetStore(st)
	if err := s.reg.Upsert(cluster.NodeUpsert{ID: "n1", URL: node.URL, Key: "kk"}, s.now()); err != nil {
		t.Fatal(err)
	}
	s.reg.SetNodeMeta("n1", cluster.NodeMeta{Runtime: "firecracker", Isolation: true})
	// One key, held by the creator, so the control path's own ownership rule
	// is the thing being exercised rather than a stranger being refused.
	key, keyPlain, err := st.CreateKey(t.Context(), "alice", "u:one")
	if err != nil {
		t.Fatal(err)
	}
	row := store.SandboxRecord{
		ID: "sb1", OrgID: "u:one", Template: "base", State: "running",
		NodeID: "n1", CreatedBy: key.ID, CreatedAt: 1, UpdatedAt: 1,
	}
	save = func(visibility string) {
		t.Helper()
		row.Visibility = visibility
		if err := st.SaveSandbox(t.Context(), row); err != nil {
			t.Fatal(err)
		}
	}
	save(sandbox.VisibilityOrg)
	s.reg.Place("sb1", "n1")

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port = probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	t.Setenv("FORWARD_PORT_RANGE", strconv.Itoa(port)+"-"+strconv.Itoa(port))

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	s.StartForwardIngress(ctx)

	post = func(body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/sb1/forwards", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+keyPlain)
		req.RemoteAddr = "203.0.113.7:1234"
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, req)
		return w
	}
	return s, st, port, save, post
}

// The control path and the data path have to answer one question the same way:
// who may reach this sandbox. The control path asked it and the public port
// did not, so an anonymous TCP client was spliced into a private sandbox that
// the preview path refuses. This exercises both ends against one row.
func TestAPublicForwardServesOnlyWhatMayBeReachedWithoutACredential(t *testing.T) {
	s, _, port, save, post := reachFixture(t)

	// Created through the API, so the control path's own answer is what binds.
	if w := post(`{"port":5432}`); w.Code != http.StatusCreated {
		t.Fatalf("create forward on an org-visible sandbox: %d %s", w.Code, w.Body.String())
	}
	s.reconcileForwards(t.Context())

	// The positive control. Without it, "no bytes came back" below would also
	// be true of a port nothing ever served.
	if err := echoThrough(port); err != nil {
		t.Fatalf("org-visible sandbox: the forward does not carry traffic: %v", err)
	}

	// The same row, now private. Nothing about the listener changed.
	save(sandbox.VisibilityPrivate)
	if err := echoThrough(port); err == nil {
		t.Fatal("an anonymous TCP client was spliced into a PRIVATE sandbox: " +
			"the public port carries no credential, so the row has to refuse it")
	} else if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the connection was neither served nor closed: %v", err)
	}

	// And the API says so up front rather than handing out a dead port.
	w := post(`{"port":6379}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "sandbox_private") {
		t.Errorf("forward create on a private sandbox = %d %s, want 409 sandbox_private", w.Code, w.Body.String())
	}

	// Sharing it back restores the forward, so the rule is the row's and not a
	// one-way trip.
	save(sandbox.VisibilityOrg)
	if err := echoThrough(port); err != nil {
		t.Errorf("shared back to the org, the forward still refuses: %v", err)
	}
}

// echoThrough sends a byte through a bound public port and waits for it back.
func echoThrough(port int) error {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck // test
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte("ping")); err != nil {
		return err
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(conn, got); err != nil {
		return err
	}
	if string(got) != "ping" {
		return errors.New("bytes came back altered: " + string(got))
	}
	return nil
}

// The org's source-network restriction used to cover exactly one door. A
// restricted org's sandbox stayed reachable from any network on a raw
// forwarded port: peerAllowed had a single caller, the HTTP preview gate.
func TestASourceRestrictedOrgsForwardRefusesOffNetworkPeers(t *testing.T) {
	s, st, port, _, post := reachFixture(t)

	if w := post(`{"port":5432}`); w.Code != http.StatusCreated {
		t.Fatalf("create forward: %d %s", w.Code, w.Body.String())
	}
	s.reconcileForwards(t.Context())

	// Positive control: unrestricted, the port carries traffic. Without it the
	// refusal below would also be true of a port nothing ever served.
	if err := echoThrough(port); err != nil {
		t.Fatalf("with no restriction the forward does not carry traffic: %v", err)
	}

	// The loopback client is now outside the org's networks.
	if err := st.SetOrgPreviewSources(t.Context(), "u:one", []string{"100.64.0.0/10"}); err != nil {
		t.Fatalf("set sources: %v", err)
	}
	if err := echoThrough(port); err == nil {
		t.Fatal("an off-network peer was spliced into a source-restricted org's sandbox: " +
			"the restriction covers the preview path and not the port it publishes")
	} else if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the connection was neither served nor closed: %v", err)
	}

	// And it comes back when the peer's own network is allowed, so the rule is
	// the CIDR list and not a forward that broke.
	if err := st.SetOrgPreviewSources(t.Context(), "u:one", []string{"127.0.0.0/8"}); err != nil {
		t.Fatalf("set sources: %v", err)
	}
	if err := echoThrough(port); err != nil {
		t.Errorf("the peer's own network is allowed and the forward still refuses: %v", err)
	}
}
