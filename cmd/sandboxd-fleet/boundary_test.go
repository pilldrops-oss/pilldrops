package main

import (
	"os/exec"
	"strings"
	"testing"
)

// TestFleetDependencyBoundary asserts the fleet binary never links the web UI or dashboard server.
func TestFleetDependencyBoundary(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		for _, forbidden := range []string{
			"github.com/pilldrops-oss/pilldrops/internal/webui",
			"github.com/pilldrops-oss/pilldrops/internal/dashboard",
		} {
			if dep == forbidden || strings.HasPrefix(dep, forbidden+"/") {
				t.Errorf("forbidden dependency in fleet closure: %s", dep)
			}
		}
	}
}
