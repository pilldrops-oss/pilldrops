package docker

import (
	"reflect"
	"strings"
	"testing"
)

func TestCanonicalTemplateAndEnrichment(t *testing.T) {
	for _, alias := range []string{"code-interpreter", "python-3.11", "python-data-science", "code-interpreter-v1", "CODE-INTERPRETER"} {
		if got := canonicalTemplate(alias); got != "code-interpreter" {
			t.Errorf("canonicalTemplate(%q) = %q, want code-interpreter", alias, got)
		}
	}
	df, ok := enrichedDockerfiles["code-interpreter"]
	if !ok {
		t.Fatal("code-interpreter must have an enriched Dockerfile (the data stack)")
	}
	for _, pkg := range []string{"numpy", "pandas", "matplotlib"} {
		if !strings.Contains(df, pkg) {
			t.Errorf("enriched code-interpreter Dockerfile missing %q", pkg)
		}
	}
	if got := canonicalTemplate("go"); got != "go" {
		t.Errorf("canonicalTemplate(go) = %q, want go (unchanged)", got)
	}

	for _, alias := range []string{"playwright", "playwright-test", "e2e"} {
		if got := canonicalTemplate(alias); got != "playwright" {
			t.Errorf("canonicalTemplate(%q) = %q, want playwright", alias, got)
		}
		if img := imageFor(alias); img != "mcr.microsoft.com/playwright:v1.49.0-jammy" {
			t.Errorf("imageFor(%q) = %q, want the official playwright image", alias, img)
		}
	}
	if pw, ok := enrichedDockerfiles["playwright"]; !ok || !strings.Contains(pw, "@playwright/test") {
		t.Error("playwright enriched Dockerfile must install the @playwright/test runner")
	}
}

func TestDedupePublishPorts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra []int
		want  []int
	}{
		{"no extra ports", nil, []int{appPort}},
		{"distinct extra", []int{5432, 6379}, []int{appPort, 5432, 6379}},
		// Re-publishing appPort made docker bind it twice.
		{"extra equals appPort", []int{appPort}, []int{appPort}},
		{"extra with appPort mixed in", []int{5432, appPort, 6379}, []int{appPort, 5432, 6379}},
		{"duplicate extras", []int{5432, 5432}, []int{appPort, 5432}},
		{"ignores non-positive", []int{0, -1, 5432}, []int{appPort, 5432}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dedupePublishPorts(appPort, tc.extra); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("dedupePublishPorts(%d, %v) = %v, want %v", appPort, tc.extra, got, tc.want)
			}
		})
	}
}

func TestIsPortBindConflict(t *testing.T) {
	conflict := []string{
		"docker: Error response from daemon: failed to set up container networking: driver failed programming external connectivity on endpoint x: failed to bind host port 127.0.0.1:33442/tcp: address already in use",
		"Bind for 0.0.0.0:8000 failed: port is already allocated",
	}
	for _, s := range conflict {
		if !isPortBindConflict(s) {
			t.Fatalf("expected port-bind conflict for: %q", s)
		}
	}
	notConflict := []string{
		"",
		"docker: Error response from daemon: pull access denied for foo, repository does not exist",
		"no space left on device",
	}
	for _, s := range notConflict {
		if isPortBindConflict(s) {
			t.Fatalf("did not expect port-bind conflict for: %q", s)
		}
	}
}
