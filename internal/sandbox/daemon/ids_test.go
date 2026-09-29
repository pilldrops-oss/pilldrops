package daemon

import (
	"regexp"
	"testing"
)

// dnsLabel matches one valid DNS label; ids must satisfy it to serve as preview subdomains.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

var idShape = regexp.MustCompile(`^sb-[0-9]+-[0-9a-f]+$`)

func TestNewSandboxID_IsDNSLabelSafe(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id := newSandboxID()
		if !dnsLabel.MatchString(id) {
			t.Fatalf("id %q is not a valid DNS label", id)
		}
		if len(id) > 63 {
			t.Fatalf("id %q exceeds 63 chars (%d)", id, len(id))
		}
		if seen[id] {
			t.Fatalf("duplicate id %q at iteration %d", id, i)
		}
		seen[id] = true
	}
}

func TestNewSandboxID_HasExpectedShape(t *testing.T) {
	id := newSandboxID()
	if !idShape.MatchString(id) {
		t.Fatalf("id %q does not match sb-<ms>-<hex>", id)
	}
}

func TestSandboxSubdomainLabelFitsWithPort(t *testing.T) {
	// Preview hostnames use <id>-<port> as a single label.
	id := newSandboxID()
	label := id + "-65535"
	if len(label) > 63 {
		t.Fatalf("<id>-<port> label %q exceeds 63 chars (%d)", label, len(label))
	}
	if !dnsLabel.MatchString(label) {
		t.Fatalf("<id>-<port> label %q is not a valid DNS label", label)
	}
}
