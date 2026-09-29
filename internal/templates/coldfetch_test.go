package templates

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestSlowFetchOutlivesTheCallerThatStartedIt is the check for the cold
// snapshot boot that 503'd for minutes. The control plane's node transport
// gives up on a create after ResponseHeaderTimeout and a 1.6 GB snapshot never
// lands inside that, so the fetch used to be cancelled and restarted by every
// retry and never finished on its own. It must now run to completion detached,
// and the impatient caller must be told it is in progress rather than that it
// failed.
//
// It fails against the old code twice over: fetchIntoDir returned ctx.Err()
// rather than ErrFetchInProgress, and the Source saw a cancelled context and
// wrote nothing.
func TestSlowFetchOutlivesTheCallerThatStartedIt(t *testing.T) {
	dir := t.TempDir()
	landed := make(chan error, 1)
	release := make(chan struct{})
	useSource(t, dir, func(ctx context.Context, id, d string) error {
		// Outlast the caller, then prove the fetch was never cancelled.
		<-release
		if err := ctx.Err(); err != nil {
			landed <- err
			return err
		}
		if err := os.MkdirAll(filepath.Join(d, id), 0o755); err != nil {
			landed <- err
			return err
		}
		err := os.WriteFile(filepath.Join(d, id, "rootfs.squashfs"), []byte("big"), 0o644)
		landed <- err
		return err
	})

	impatient, cancel := context.WithCancel(context.Background())
	cancel() // the control plane has already hung up
	if err := fetchIntoDir(impatient, "cold-snap"); !errors.Is(err, ErrFetchInProgress) {
		t.Fatalf("a caller that gave up got %v, want ErrFetchInProgress — a bare cancellation "+
			"reaches the tenant as a generic 503 that says nothing about a download", err)
	}

	close(release)
	if err := <-landed; err != nil {
		t.Fatalf("the fetch did not survive the caller that started it: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "cold-snap", "rootfs.squashfs")); err != nil {
		t.Fatalf("the detached fetch left nothing on disk: %v", err)
	}
}
