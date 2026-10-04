package main

import "testing"

// A node-API resource left off the prefix list 404s at the portal even with a
// healthy fleet behind it.
func TestFleetPathsCoverEveryProxiedResource(t *testing.T) {
	forwarded := []string{
		"/v1/sandboxes",
		"/v1/sandboxes/sb_123/exec",
		"/v1/cron",
		"/v1/cron/cron_123/run",
	}
	for _, p := range forwarded {
		if !isFleetPath(p) {
			t.Errorf("%s is not forwarded to the fleet — it would 404 at the portal", p)
		}
	}

	// Portal-owned paths must not be forwarded; a node has no idea who the
	// browser user is.
	local := []string{"/v1/limits", "/v1/keys", "/dashboard", "/pricing", "/v1/plans", "/health"}
	for _, p := range local {
		if isFleetPath(p) {
			t.Errorf("%s was sent to the fleet, but the portal owns it", p)
		}
	}
}
