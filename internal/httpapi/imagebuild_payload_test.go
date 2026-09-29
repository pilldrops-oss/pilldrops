package httpapi

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/process"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
)

// An explicit artifact kind this node cannot produce is refused, not silently downgraded.

func payloadServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	d := daemon.New(daemon.Config{
		Runtime:     process.New(filepath.Join(dir, "sb")),
		SnapshotDir: filepath.Join(dir, "snap"),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	return New(d, nil)
}

func TestBuildPayloadRefusesATemplateOnANonFirecrackerNode(t *testing.T) {
	s := payloadServer(t) // process runtime, not firecracker

	got, err := s.buildPayload("")
	if err != nil || got != artifact.NameImage {
		t.Fatalf("no preference on a non-KVM node: got %q err=%v, want %q",
			got, err, artifact.NameImage)
	}

	// Every spelling of the ask must be refused, not just the canonical one.
	for _, want := range []string{artifact.NameRootfs, "template", "firecracker"} {
		if _, err := s.buildPayload(want); err == nil {
			t.Errorf("payload=%q was accepted on a %q node", want, s.d.RuntimeName())
		} else if !strings.Contains(err.Error(), "process") {
			t.Errorf("the refusal must name the runtime this node actually has; got %q", err)
		}
	}

	// An image build is available on any node.
	if got, err := s.buildPayload("image.tar"); err != nil || got != artifact.NameImage {
		t.Errorf("payload=image.tar: got %q err=%v", got, err)
	}
}

func TestBuildPayloadRejectsAnUnknownName(t *testing.T) {
	s := payloadServer(t)
	if _, err := s.buildPayload("rootfs.ext4"); err == nil {
		t.Fatal("an unknown payload name was accepted")
	} else if !strings.Contains(err.Error(), artifact.NameRootfs) ||
		!strings.Contains(err.Error(), artifact.NameImage) {
		t.Errorf("the error must list what is accepted; got %q", err)
	}
}
