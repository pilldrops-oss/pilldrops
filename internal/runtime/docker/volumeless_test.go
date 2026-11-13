package docker

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

func TestConfigChangesKeepEverythingButVolumes(t *testing.T) {
	got, err := configChanges(imageConfig{
		Env:          []string{"PATH=/usr/bin:/bin", `TRICKY=a "b" $c \d`, "EMPTY="},
		Entrypoint:   []string{"docker-entrypoint.sh"},
		Cmd:          []string{"redis-server", "--save 60 1"},
		WorkingDir:   "/data",
		User:         "999",
		ExposedPorts: map[string]struct{}{"6379/tcp": {}, "16379/tcp": {}},
		Volumes:      map[string]struct{}{"/data": {}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`ENV PATH="/usr/bin:/bin"`,
		`ENV TRICKY="a \"b\" \$c \\d"`,
		`ENV EMPTY=""`,
		`ENTRYPOINT ["docker-entrypoint.sh"]`,
		`CMD ["redis-server","--save 60 1"]`,
		`WORKDIR /data`,
		`USER 999`,
		`EXPOSE 16379/tcp`,
		`EXPOSE 6379/tcp`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("changes:\n got %q\nwant %q", got, want)
	}
	if _, err := configChanges(imageConfig{Env: []string{"A=one\ntwo"}}); err == nil {
		t.Error("an env value with a line break was accepted; it cannot be written as one instruction")
	}
}

// A database image keeps its data in a declared volume, which `docker commit`
// leaves out. Real docker, because that omission is the thing under test.
func TestLiveServiceDataSurvivesCheckpointAndPause(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Skipf("docker runtime unavailable: %v", err)
	}
	ctx := t.Context()
	spec := rt.BootSpec{
		SandboxID: liveSandboxID(t, "sb-live-vol"),
		Template:  rt.Template{Name: "redis", Image: "redis:7", Ports: []int{6379}},
	}
	inst, err := r.Boot(ctx, spec)
	if err != nil {
		t.Skipf("boot failed (image or daemon unavailable): %v", err)
	}
	// The stored value proves the service itself reads the directory again,
	// not only that a file is on disk.
	stored := func(i rt.Instance) string {
		t.Helper()
		var out string
		for range 40 {
			res, _ := i.Exec(ctx, rt.ExecSpec{Cmd: "redis-cli get k"})
			if out = strings.TrimSpace(res.Stdout); res.ExitCode == 0 && out != "" {
				return out
			}
			time.Sleep(250 * time.Millisecond)
		}
		return out
	}
	set := func(i rt.Instance, v string) {
		t.Helper()
		for range 40 {
			if res, _ := i.Exec(ctx, rt.ExecSpec{Cmd: "redis-cli set k " + v + " && redis-cli save"}); res.ExitCode == 0 {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatalf("could not store %q", v)
	}

	set(inst, "checkpointed")
	dir := t.TempDir()
	ckpt := rt.SnapshotRef{Path: dir, Name: "c1"}
	if _, err := r.Checkpoint(ctx, inst, ckpt); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	t.Cleanup(func() { _, _, _, _ = dockerOut(t.Context(), "rmi", "-f", ckptImageTag(spec.SandboxID, "c1")) })
	set(inst, "later")
	inst, err = r.RestoreCheckpoint(ctx, inst, ckpt, spec)
	if err != nil {
		t.Fatalf("restore checkpoint: %v", err)
	}
	if got := stored(inst); got != "checkpointed" {
		t.Errorf("after a checkpoint restore the service holds %q, want %q", got, "checkpointed")
	}

	set(inst, "paused")
	snap := rt.SnapshotRef{Path: filepath.Join(dir, "pause")}
	if err := inst.Snapshot(ctx, snap); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	t.Cleanup(func() { _ = r.DeleteSnapshot(t.Context(), spec.SandboxID, snap) })
	if err := inst.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	inst, err = r.Restore(ctx, snap, spec)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := stored(inst); got != "paused" {
		t.Errorf("after a pause and resume the service holds %q, want %q", got, "paused")
	}
}
