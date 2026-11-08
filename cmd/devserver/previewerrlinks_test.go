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

// withOrigin sets the package-level PORTAL_ORIGIN for one test and restores it.
func withOrigin(t *testing.T, o string) {
	t.Helper()
	prev := dashboardOrigin
	dashboardOrigin = o
	t.Cleanup(func() { dashboardOrigin = prev })
}

// With the dashboard on its own origin, the error page links there, never to
// the sandbox domain, which serves no dashboard.
func TestPreviewErrorPageLinksToThePortalOrigin(t *testing.T) {
	withDomain(t, "pilldrops.app")
	withOrigin(t, "https://pilldrops.com")

	page := previewErrorPage(502, "starting", "upstream refused")
	if !strings.Contains(page, "https://pilldrops.com/dashboard") || !strings.Contains(page, "https://pilldrops.com/docs") {
		t.Errorf("links do not use PORTAL_ORIGIN:\n%s", page)
	}
	if strings.Contains(page, "https://pilldrops.app/") {
		t.Error("the page links to the sandbox domain, which no longer serves the dashboard")
	}
}

func TestDashboardOriginError(t *testing.T) {
	for _, ok := range []string{"", "https://pilldrops.com", "http://localhost:8080"} {
		if err := dashboardOriginError(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"pilldrops.com", "ftp://pilldrops.com", "https://pilldrops.com/dashboard",
		"https://pilldrops.com?x=1", "https://u:p@pilldrops.com", "https://"} {
		if err := dashboardOriginError(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The portal refuses to start with a dashboard, or an orchestrator it calls,
// on a host its own legacy redirect would send to the preview domain.
func TestPortalLegacyDomainsRefusesHostsTheRedirectWouldTake(t *testing.T) {
	withDomain(t, "pilldrops.app")
	withOrigin(t, "https://pilldrops.com")
	if got, err := portalLegacyDomains("pilldrops.com", "https://api.pilldrops.com"); err != nil || len(got) != 1 {
		t.Fatalf("= %v %v, want pilldrops.com", got, err)
	}
	if _, err := portalLegacyDomains("pilldrops.com", "https://control.pilldrops.com"); err == nil ||
		!strings.Contains(err.Error(), "PORTAL_ORCHESTRATOR_URL host control.pilldrops.com") {
		t.Fatalf("orchestrator on a slug-shaped host: err = %v", err)
	}
	if _, err := portalLegacyDomains("", "https://control.pilldrops.com"); err != nil {
		t.Fatalf("no legacy domain: %v", err)
	}

	withOrigin(t, "https://console.pilldrops.com")
	if _, err := portalLegacyDomains("pilldrops.com", ""); err == nil ||
		!strings.Contains(err.Error(), "PORTAL_ORIGIN host console.pilldrops.com") {
		t.Fatalf("dashboard on a slug-shaped host: err = %v", err)
	}
	withOrigin(t, "pilldrops.com")
	if _, err := portalLegacyDomains("", ""); err == nil || !strings.Contains(err.Error(), "PORTAL_ORIGIN") {
		t.Fatalf("malformed PORTAL_ORIGIN: err = %v", err)
	}
}
