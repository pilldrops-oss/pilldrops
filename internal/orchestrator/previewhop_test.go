package orchestrator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// The hop to the node is in the PATH form. If it carries the caller's
// <port>-<id>.<domain> Host as well, a node with SANDBOXD_SANDBOX_DOMAIN set
// routes by Host instead and hands the guest app the whole internal path —
// measured on a two-node fleet, where the app was asked for
// GET /preview/<id>/8000/mark.txt and answered 404.
func TestPreviewHopDoesNotCarryTheCallersHostToTheNode(t *testing.T) {
	var gotHost, gotPath string
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotPath = r.Host, r.URL.Path
		w.WriteHeader(200)
	}))
	t.Cleanup(node.Close)
	nodeHost := must(t, node.URL).Host

	s := NewServer("k", "t")
	s.sandboxDomain = "fleet.test"
	upsert(s.reg, "node-a", node.URL, "kk", "", nil, "", cluster.Capacity{}, time.Now())
	s.reg.Place("sb1", "node-a")

	// Exactly what a browser sends at a host-style preview, through the
	// ingress rewrite the control plane applies before handlePreview.
	req := httptest.NewRequest("GET", "http://x/mark.txt?token="+s.previewToken(context.Background(), "sb1", 8000), nil)
	req.Host = "8000-sb1.fleet.test"
	req = previewRewrite(req.Context(), req, "sb1", 8000)
	req.SetPathValue("id", "sb1")
	req.SetPathValue("rest", "8000/mark.txt")
	s.handlePreview(httptest.NewRecorder(), req)

	if gotHost != nodeHost {
		t.Fatalf("node was handed Host %q, want the node's own %q — a node with a sandbox domain "+
			"set will route that by Host and forward the internal path to the app", gotHost, nodeHost)
	}
	if !strings.HasPrefix(gotPath, "/preview/sb1/8000/") {
		t.Fatalf("node was handed path %q, want the /preview/<id>/<port>/ form", gotPath)
	}
}

func must(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}
