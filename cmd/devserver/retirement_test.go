package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every /v1 route here must also exist on the control plane or a node, or it
// works locally and is missing on a fleet. On failure, add the handler to
// internal/orchestrator or internal/httpapi, not here.

var (
	chiRouteRe  = regexp.MustCompile(`\br\.(?:Handle|HandleFunc|Get|Post|Put|Delete|Patch)\("([^"]+)"`)
	muxRouteRe  = regexp.MustCompile(`\bmux\.(?:Handle|HandleFunc)\("([^"]+)"`)
	nodeRouteRe = regexp.MustCompile(`\broute\("([^"]+)"`)
	pathParamRe = regexp.MustCompile(`\{[^}]*\}`)
	methodRe    = regexp.MustCompile(`^(?:GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS)\s+`)
)

// normRoute strips the method and collapses path params, so "GET /v1/x/{id}"
// and `r.Delete("/v1/x/{sandboxID}")` compare equal.
func normRoute(p string) string {
	p = methodRe.ReplaceAllString(strings.TrimSpace(p), "")
	p = pathParamRe.ReplaceAllString(p, "{}")
	return strings.TrimSuffix(p, "/")
}

func routesIn(t *testing.T, dir string, re *regexp.Regexp) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, m := range re.FindAllStringSubmatch(string(body), -1) {
			out[normRoute(m[1])] = true
		}
	}
	return out
}

func TestDevserverOwnsNoAPIRouteAlone(t *testing.T) {
	dev := routesIn(t, ".", chiRouteRe)
	orch := routesIn(t, filepath.Join("..", "..", "internal", "orchestrator"), muxRouteRe)
	node := routesIn(t, filepath.Join("..", "..", "internal", "httpapi"), nodeRouteRe)

	if len(dev) == 0 || len(orch) == 0 || len(node) == 0 {
		t.Fatalf("route extraction found nothing (dev=%d orch=%d node=%d) — the regexes have "+
			"drifted from how routes are registered, and this guard is silently passing",
			len(dev), len(orch), len(node))
	}

	var orphans []string
	for p := range dev {
		if !strings.HasPrefix(p, "/v1") {
			continue // the marketing site and docs are legitimately its own
		}
		if !orch[p] && !node[p] {
			orphans = append(orphans, p)
		}
	}
	sort.Strings(orphans)

	if len(orphans) > 0 {
		t.Errorf("cmd/devserver serves %d /v1 route(s) that neither the control plane nor a node "+
			"serves:\n  %s\n\nThat route works when you run the portal locally and is MISSING on a "+
			"fleet — which is how a route has 404'd for every fleet user while its tests passed. "+
			"Put the handler in internal/orchestrator (shared store) or internal/httpapi (touches a "+
			"sandbox), not here.", len(orphans), strings.Join(orphans, "\n  "))
	}
}
