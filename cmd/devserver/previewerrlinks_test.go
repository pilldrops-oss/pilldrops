package main

import (
	"strings"
	"testing"
)

// The preview error page is seen by a self-hoster's users, so its links must
// point at the deployment's own domain, never a hardcoded one.

// withDomain sets the package-level domain for one test and restores it.
func withDomain(t *testing.T, d string) {
	t.Helper()
	prev := portalSandboxDomain
	portalSandboxDomain = d
	t.Cleanup(func() { portalSandboxDomain = prev })
}

func TestPreviewErrorPageLinksNowhereWhenNoDomainIsConfigured(t *testing.T) {
	withDomain(t, "")

	page := previewErrorPage(502, "starting", "upstream refused")
	if strings.Contains(page, "pilldrops.com") {
		t.Error("a self-hosted install's error page still points at pilldrops.com")
	}
	if strings.Contains(page, "https://") {
		t.Errorf("the page links off-origin with no domain configured; a wrong "+
			"destination is worse than no button:\n%s", page)
	}
	// The page must still do its actual job without them.
	if !strings.Contains(page, "502") || !strings.Contains(page, "retrying") {
		t.Error("dropping the links took the status or the auto-retry with them")
	}
}

func TestPreviewErrorPageLinksToTheDeploymentsOwnDomain(t *testing.T) {
	withDomain(t, "sandboxes.example.org")

	page := previewErrorPage(502, "starting", "upstream refused")
	if !strings.Contains(page, "https://sandboxes.example.org/dashboard") {
		t.Errorf("links do not use the configured domain:\n%s", page)
	}
	if strings.Contains(page, "pilldrops.com") {
		t.Error("the hardcoded domain outlived the configured one")
	}
}

// "gone" carries different copy from "starting", so it needs its own check.
func TestPreviewGonePageMakesNoPromiseAboutCost(t *testing.T) {
	withDomain(t, "")

	page := previewErrorPage(404, "gone", "no such sandbox")
	for _, claim := range []string{"cost", "$", "billed", "credits"} {
		if strings.Contains(strings.ToLower(page), claim) {
			t.Errorf("the gone page still talks about %q, which this core no longer records", claim)
		}
	}
}
