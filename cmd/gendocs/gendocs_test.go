package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/httpapi"
)

const outDir = "../../docs/agent"

func TestGeneratedDocsAreCurrent(t *testing.T) {
	pages, err := render("../../internal/httpapi/openapi.json")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for name, want := range pages {
		got, err := os.ReadFile(filepath.Join(outDir, name))
		if err != nil {
			t.Errorf("%s is missing — run `make docs`: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s is out of date. Run `make docs` and commit the result.", name)
		}
	}
}

// The generator filters the spec, so a filter bug would silently drop a whole group.
func TestReferenceCoversEveryAgentEndpoint(t *testing.T) {
	pages, err := render("../../internal/httpapi/openapi.json")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	ref := pages["api-reference.md"]

	for _, path := range []string{
		"/v1/sandboxes/{id}/agent/status",
		"/v1/sandboxes/{id}/agent/turns",
		"/v1/sandboxes/{id}/agent/events",
		"/v1/sandboxes/{id}/agent/approvals",
		"/v1/sandboxes/{id}/agent/policy",
		"/v1/sandboxes/{id}/agent/budget",
		"/v1/sandboxes/{id}/agent/tokens",
		"/v1/sandboxes/{id}/agent/webhooks",
		"/v1/sandboxes/{id}/agent/isolation",
		"/v1/sandboxes/{id}/agent/connect",
		"/v1/sandboxes/{id}/agent/responses",
	} {
		if !strings.Contains(ref, path) {
			t.Errorf("the reference does not mention %s", path)
		}
	}
	if !strings.Contains(ref, "no scope implies another") {
		t.Error("the reference does not state that scopes do not imply one another")
	}
}

func TestDocumentedRetentionMatchesTheServer(t *testing.T) {
	if got := httpapi.DefaultAgentEventRetentionDays; got != defaultRetentionDays {
		t.Errorf("the docs publish %d days of event retention but the server defaults to %d — "+
			"one of them is lying to a customer about how long their history survives",
			defaultRetentionDays, got)
	}
}

func TestLimitsPagePublishesTheNumbersThatBite(t *testing.T) {
	pages, err := render("../../internal/httpapi/openapi.json")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	limits := strings.ToLower(pages["limits.md"])
	for _, must := range []string{
		"turn.output_truncated",
		"consecutive",
		"turn.output",
		"timeout is a denial",
	} {
		must = strings.ToLower(must)
		if !strings.Contains(limits, must) {
			t.Errorf("the limits page does not mention %q", must)
		}
	}
	if !strings.Contains(limits, (5 * time.Minute).String()) {
		t.Error("the limits page does not publish the signature skew window")
	}
}

// An all-yes matrix means the generator stopped reading the manifest.
func TestCapabilityMatrixIsHonestAboutGaps(t *testing.T) {
	pages, err := render("../../internal/httpapi/openapi.json")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	m := pages["capability-matrix.md"]
	if !strings.Contains(m, "| —") && !strings.Contains(m, " — |") {
		t.Error("no gaps in the matrix at all, which means it is not reading the manifest")
	}
	for _, must := range []string{"Guided mode\nis not offered", "remembers: false", "raw output"} {
		if !strings.Contains(m, must) && !strings.Contains(m, strings.ReplaceAll(must, "\n", " ")) {
			t.Errorf("the matrix does not explain the consequence: %q", must)
		}
	}
}
