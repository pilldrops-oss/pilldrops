//go:build linux

package vm

// Live test that EnvdAuth and EnableSnapshots together, through Manager.Create,
// actually reach the restore path. The golden is built by the production
// createOneSnapshot via NewManager, so it also proves a startup-built snapshot restores.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// s5Cfg is liveCfg with auth and snapshots on. VM_LIVE_TEMPLATE_HAS_S1 is
// required because createOneSnapshot boots the template's baked envd; an old one
// makes every restore fail for the wrong reason, so such a run skips instead.
func s5Cfg(t *testing.T) (Config, string) {
	t.Helper()
	cfg, tmpl := liveCfg(t)
	if os.Getenv("VM_LIVE_TEMPLATE_HAS_S1") != "1" {
		t.Skip("SKIP (not a pass): set VM_LIVE_TEMPLATE_HAS_S1=1 only when SNAPSHOT_DIR's " +
			"template has a §1/§3 envd BAKED IN (strings rootfs | grep SETNET). " +
			"createOneSnapshot boots the baked binary, not VM_LIVE_ENVD, so an old one " +
			"makes every restore fail for the wrong reason")
	}
	cfg.EnvdAuth = true
	cfg.EnableSnapshots = true
	cfg.Templates = []string{tmpl}
	return cfg, tmpl
}

func TestLiveSnapshotBackedCreateWithEnvdAuth(t *testing.T) {
	cfg, tmpl := s5Cfg(t)
	ctx := context.Background()

	// NewManager builds the golden snapshot into the side-car tree.
	base, _ := liveCfg(t)
	cfg.SnapshotDir = sidecarTemplates(t, base, tmpl)

	m := liveManager(t, cfg)

	snapDir := snapshotDir(tmpl, "v1", cfg)
	if !snapshotValid(snapDir) {
		t.Fatalf("NewManager did not leave a valid golden snapshot at %s; every create "+
			"below would cold boot and this test would prove nothing about restore", snapDir)
	}
	golden, err := readGoldenIdentity(snapDir)
	if err != nil {
		t.Fatalf("golden identity: %v", err)
	}
	t.Logf("createOneSnapshot built %s pinning %+v", snapDir, golden)

	sb, err := m.Create(ctx, CreateRequest{Template: tmpl, UserID: "s5-1"})
	if err != nil {
		t.Fatalf("create with ENVD_AUTH and snapshots both on: %v", err)
	}
	defer func() { _ = m.Destroy(ctx, sb.ID) }()

	if sb.Token == "" {
		t.Fatal("EnvdAuth is on but Create minted no token — the sandbox has no credential " +
			"and everything below would be measuring an unauthenticated node")
	}
	if sb.BootType != "snapshot" {
		t.Fatalf("BootType = %q, want \"snapshot\". ENVD_AUTH and FC_SNAPSHOTS are both on, "+
			"so a cold boot here means the create never reached the restore path — which is "+
			"the state acquireVM's tokenHash gate used to enforce", sb.BootType)
	}
	t.Logf("5.1: create %s -> BootType=%s in %d ms, on a node with ENVD_AUTH=1 and "+
		"FC_SNAPSHOTS=1", sb.ID, sb.BootType, sb.BootMs)

	// m.Exec uses the client Create dialed with the token, so an unauthenticated envd fails here.
	res, err := m.Exec(ctx, sb.ID, "echo s5-ok; echo HASHCOUNT=$(grep -c envd.token_hash= /proc/cmdline); echo ROOT=$(grep -c root= /proc/cmdline)", "/root", 20*time.Second)
	if err != nil {
		t.Fatalf("authenticated exec on the snapshot-backed sandbox: %v", err)
	}
	out := strings.TrimSpace(string(res.Stdout))
	g := kv(t, out, "HASHCOUNT", "ROOT")
	if !strings.Contains(out, "s5-ok") {
		t.Fatalf("exec output %q", out)
	}
	// ROOT is the control that the cmdline was read at all.
	if g["ROOT"] != "1" {
		t.Fatalf("/proc/cmdline does not look like a kernel cmdline (%q); HASHCOUNT=%s "+
			"would prove nothing", out, g["HASHCOUNT"])
	}
	if g["HASHCOUNT"] != "0" {
		t.Fatalf("the restored guest's cmdline carries envd.token_hash= (%s); the "+
			"credential may have come from the boot arguments", g["HASHCOUNT"])
	}
	t.Logf("5.1: authenticated exec ok on %s; /proc/cmdline carries no token hash "+
		"(HASHCOUNT=0 with ROOT=1 as the control)", sb.ID)

	// RestoreFromSnapshot deletes snapDir/state on any failure.
	if !snapshotValid(snapDir) {
		t.Fatalf("%s is no longer a valid snapshot after one restore", snapDir)
	}
}

// Two simultaneous creates must both be restores and share nothing; a reintroduced
// singleflight or an over-held claimGoldenTap shows up here.
func TestLiveConcurrentSnapshotBackedCreates(t *testing.T) {
	cfg, tmpl := s5Cfg(t)
	ctx := context.Background()

	base, _ := liveCfg(t)
	cfg.SnapshotDir = sidecarTemplates(t, base, tmpl)
	m := liveManager(t, cfg)

	if !snapshotValid(snapshotDir(tmpl, "v1", cfg)) {
		t.Fatal("no valid golden snapshot; both creates would cold boot")
	}

	type made struct {
		sb  *Sandbox
		err error
		ms  int64
	}
	out := make([]made, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range out {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			t0 := time.Now()
			out[i].sb, out[i].err = m.Create(ctx, CreateRequest{Template: tmpl, UserID: "s5-4"})
			out[i].ms = time.Since(t0).Milliseconds()
		}()
	}
	close(start)
	wg.Wait()

	for i := range out {
		if out[i].sb != nil {
			id := out[i].sb.ID
			defer func() { _ = m.Destroy(ctx, id) }()
		}
	}
	for i := range out {
		if out[i].err != nil {
			t.Fatalf("concurrent create %d: %v", i, out[i].err)
		}
		if out[i].sb.BootType != "snapshot" {
			t.Fatalf("concurrent create %d has BootType %q, want \"snapshot\" — one of two "+
				"simultaneous creates fell back to a cold boot, which is the golden-name "+
				"collision this phase exists to remove", i, out[i].sb.BootType)
		}
		t.Logf("5.4: %s BootType=%s ip=%s tap=%s overlay=%s in %d ms",
			out[i].sb.ID, out[i].sb.BootType, out[i].sb.VMIP, out[i].sb.TapName,
			out[i].sb.OverlayPath, out[i].ms)
	}
	a, b := out[0].sb, out[1].sb
	for _, p := range []struct{ what, x, y string }{
		{"address", a.VMIP, b.VMIP},
		{"tap", a.TapName, b.TapName},
		{"overlay", a.OverlayPath, b.OverlayPath},
		{"token", a.Token, b.Token},
	} {
		if p.x == p.y {
			t.Fatalf("both concurrently-created sandboxes share a %s (%s)", p.what, p.x)
		}
	}
	if filepath.Dir(a.OverlayPath) == filepath.Dir(b.OverlayPath) {
		t.Fatalf("both overlays are in %s", filepath.Dir(a.OverlayPath))
	}

	for _, sb := range []*Sandbox{a, b} {
		res, err := m.Exec(ctx, sb.ID, "echo alive-"+sb.ID, "/root", 20*time.Second)
		if err != nil {
			t.Fatalf("exec on %s: %v", sb.ID, err)
		}
		if got := strings.TrimSpace(string(res.Stdout)); got != "alive-"+sb.ID {
			t.Fatalf("exec on %s returned %q", sb.ID, got)
		}
	}
	t.Logf("5.4: both snapshot-backed sandboxes driven concurrently, on distinct "+
		"addresses (%s, %s), taps (%s, %s), overlays and credentials",
		a.VMIP, b.VMIP, a.TapName, b.TapName)
}
