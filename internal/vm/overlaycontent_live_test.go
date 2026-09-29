//go:build linux

package vm

// Live proof that a restored guest's disk carries the golden image's content.
// A snapshot restores the page cache too, so every read follows a drop_caches in
// the guest; the payload is written from the host since a golden VM holds no credential.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
	"github.com/pilldrops-oss/pilldrops/internal/envd"
)

// payloadBytes is 64 MiB because a small file can survive drop_caches in a readahead window.
const payloadBytes = 64 << 20

// goldenPayload is written into the golden image before the snapshot; sha is
// computed on the host so the expected value never passes through the guest.
type goldenPayload struct {
	dir    string // /root/s4-<hex> inside the guest
	marker string
	sha    string
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// execLong has a timeout that survives checksumming 64 MiB in a 1-vCPU guest with a cold cache.
func execLong(t *testing.T, c *envd.Client, cmd string) string {
	t.Helper()
	res, err := c.Execute(context.Background(), cmd, "/root", 120*time.Second)
	if err != nil {
		t.Fatalf("exec %q: %v", cmd, err)
	}
	return strings.TrimSpace(string(res.Stdout))
}

// execTry runs a command that is allowed to fail.
func execTry(t *testing.T, c *envd.Client, cmd string) (string, string, bool) {
	t.Helper()
	res, err := c.Execute(context.Background(), cmd, "/root", 120*time.Second)
	if err != nil {
		return "", err.Error(), false
	}
	return strings.TrimSpace(string(res.Stdout)), strings.TrimSpace(string(res.Stderr)), true
}

// kv parses `KEY=value` lines and fails on any missing key.
func kv(t *testing.T, out string, want ...string) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			m[k] = v
		}
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Fatalf("the guest did not report %s; its whole output was:\n%s", k, out)
		}
	}
	return m
}

// dropGuestCaches evicts the guest's caches and proves it via Cached: in
// /proc/meminfo, since the drop_caches write itself is silent.
func dropGuestCaches(t *testing.T, c *envd.Client, who string) {
	t.Helper()
	out := execLong(t, c, strings.Join([]string{
		"echo BEFORE=$(awk '/^Cached:/{print $2}' /proc/meminfo)",
		"sync",
		"echo 3 > /proc/sys/vm/drop_caches",
		"echo RC=$?",
		"echo AFTER=$(awk '/^Cached:/{print $2}' /proc/meminfo)",
	}, "\n"))
	m := kv(t, out, "BEFORE", "RC", "AFTER")
	if m["RC"] != "0" {
		t.Fatalf("%s: the guest could not write /proc/sys/vm/drop_caches (rc=%s). Every "+
			"'read from disk' assertion here would then be a read from the page cache the "+
			"snapshot restored", who, m["RC"])
	}
	before := atoiOr(t, m["BEFORE"], "Cached before")
	after := atoiOr(t, m["AFTER"], "Cached after")
	if after >= before {
		t.Fatalf("%s: dropping the caches did not shrink Cached: (%d kB -> %d kB). The "+
			"eviction did not happen, so nothing below distinguishes the disk from RAM",
			who, before, after)
	}
	t.Logf("%s: caches dropped, Cached: %d kB -> %d kB", who, before, after)
}

// writeGoldenPayload writes the payload into the golden image on the host before
// the VM boots. beforeImage, when set, receives a copy taken just before the
// payload is added, for the mutation test.
func writeGoldenPayload(p *goldenPayload, beforeImage string) goldenPrep {
	return func(t *testing.T, overlayPath string) {
		t.Helper()

		if beforeImage != "" {
			if err := copySparse(overlayPath, beforeImage); err != nil {
				t.Fatalf("copy the pre-payload image: %v", err)
			}
		}

		sum := sha256.New()
		withOverlayMounted(t, overlayPath, func(mnt string) {
			dir := filepath.Join(mnt, "upper", strings.TrimPrefix(p.dir, "/"))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("mkdir %s in the overlay: %v", dir, err)
			}
			if err := os.WriteFile(filepath.Join(dir, "marker"), []byte(p.marker+"\n"), 0o644); err != nil {
				t.Fatalf("write marker: %v", err)
			}
			f, err := os.Create(filepath.Join(dir, "blob"))
			if err != nil {
				t.Fatalf("create blob: %v", err)
			}
			if _, err := io.CopyN(io.MultiWriter(f, sum), rand.Reader, payloadBytes); err != nil {
				f.Close()
				t.Fatalf("write %d random bytes: %v", payloadBytes, err)
			}
			if err := f.Close(); err != nil {
				t.Fatalf("close blob: %v", err)
			}
		})
		p.sha = hex.EncodeToString(sum.Sum(nil))

		if beforeImage != "" {
			bs, err := os.Stat(beforeImage)
			if err != nil {
				t.Fatalf("stat the pre-payload image: %v", err)
			}
			as, err := os.Stat(overlayPath)
			if err != nil {
				t.Fatalf("stat the payload image: %v", err)
			}
			if bs.Size() != as.Size() {
				t.Fatalf("the two images differ in SIZE (%d vs %d); a restore could then "+
					"fail for a reason that is not about content", bs.Size(), as.Size())
			}
			t.Logf("pre-payload image kept at %s (%d bytes apparent, same as the payload image)",
				filepath.Base(beforeImage), bs.Size())
		}
		t.Logf("golden payload written on the HOST into %s: marker=%s, blob=%d bytes, "+
			"sha256=%s", p.dir, p.marker, payloadBytes, p.sha)
	}
}

// readPayload drops the guest's caches, then reads the golden payload back from its disk.
func readPayload(t *testing.T, c *envd.Client, p goldenPayload, who string) map[string]string {
	t.Helper()
	dropGuestCaches(t, c, who)
	out := execLong(t, c, strings.Join([]string{
		// LS separates "file missing" from "nothing mounted".
		"echo LS=$(ls " + p.dir + " 2>/dev/null | tr '\\n' ',')",
		"echo MARKER=$(cat " + p.dir + "/marker 2>/dev/null)",
		"echo SIZE=$(stat -c %s " + p.dir + "/blob 2>/dev/null)",
		"echo SHA=$(sha256sum " + p.dir + "/blob 2>/dev/null | cut -d' ' -f1)",
	}, "\n"))
	t.Logf("%s read back:\n%s", who, out)
	return kv(t, out, "LS", "MARKER", "SIZE", "SHA")
}

func TestLiveRestoredOverlayCarriesTheGoldenImageContent(t *testing.T) {
	cfg, tmpl := liveCfg(t)
	log := zap.NewNop()
	m := liveManager(t, cfg)

	cfg.SnapshotDir = sidecarTemplates(t, cfg, tmpl)
	p := &goldenPayload{dir: "/root/s4-" + randHex(4), marker: randHex(16)}
	snapDir, golden := buildGoldenSnapshot(t, m, cfg, tmpl, writeGoldenPayload(p, ""))
	t.Logf("golden snapshot at %s pins %+v", snapDir, golden)

	goldenImg := filepath.Join(snapDir, goldenOverlayFile)
	gi, err := os.Stat(goldenImg)
	if err != nil {
		t.Fatalf("the golden snapshot has no %s beside state/mem: %v", goldenOverlayFile, err)
	}
	t.Logf("golden image kept beside state/mem: %s (%d bytes apparent)", goldenImg, gi.Size())

	id := "s4content-" + randHex(4)
	token, hash := sbtoken.Mint()
	t0 := time.Now()
	vm, err := RestoreFromSnapshot(tmpl, id, cfg, log, hash, nil)
	if err != nil {
		t.Fatalf("restore %s: %v", id, err)
	}
	defer vm.Destroy()
	t.Logf("restored %s in %d ms: overlay=%s", id, time.Since(t0).Milliseconds(), vm.OverlayPath)

	// The sandbox must write to its own file, or it corrupts the template for every later restore.
	if vm.OverlayPath == goldenImg {
		t.Fatalf("the restored sandbox was handed the SNAPSHOT's own image %s", goldenImg)
	}
	if filepath.Base(vm.OverlayPath) != golden.OverlayBase {
		t.Fatalf("overlay basename is %q, want the golden basename %q (Firecracker "+
			"restores the block device by name)", filepath.Base(vm.OverlayPath), golden.OverlayBase)
	}

	c, err := envd.ConnectWithToken(vm.VsockPath, token, 60*time.Second)
	if err != nil {
		t.Fatalf("%s: restored guest unreachable at %s: %v", id, vm.VsockPath, err)
	}
	defer c.Close()

	got := readPayload(t, c, *p, id)
	if got["LS"] == "" {
		t.Fatalf("%s: %s is empty or absent in the restored guest — the overlay did not "+
			"attach, so a missing marker would not be a CONTENT failure", id, p.dir)
	}
	if got["MARKER"] != p.marker {
		t.Fatalf("%s: the marker written into the golden image before the snapshot reads "+
			"back as %q, want %q — the restored guest is not on the image its snapshot was "+
			"taken with", id, got["MARKER"], p.marker)
	}
	if atoiOr(t, got["SIZE"], "restored blob size") != payloadBytes {
		t.Fatalf("%s: the %d MiB payload is %s bytes after restore", id, payloadBytes>>20, got["SIZE"])
	}
	if got["SHA"] != p.sha {
		t.Fatalf("%s: the payload's sha256 is %s after restore, want %s (computed on the "+
			"host from the bytes it generated) — the image is present but its CONTENT "+
			"differs", id, got["SHA"], p.sha)
	}
	t.Logf("4.1 PROVED: %d MiB written into the golden image before the snapshot came back "+
		"byte-identical from a restored guest with a demonstrably cold cache "+
		"(sha256=%s, computed host-side)", payloadBytes>>20, got["SHA"])

	// A read-only attach would pass everything above.
	fresh := randHex(16)
	out := execLong(t, c, "echo "+fresh+" > "+p.dir+"/postrestore\nsync\necho POST=$(cat "+p.dir+"/postrestore)")
	if kv(t, out, "POST")["POST"] != fresh {
		t.Fatalf("%s: the restored guest cannot write to its own overlay: %s", id, out)
	}
	t.Logf("%s: the restored overlay is writable (wrote and read back %s)", id, fresh)
}

// Mutation control for the test above: restore the same state/mem against a copy
// of the image taken before the payload was written. If the payload still reads
// back, the reads were served from the restored page cache, not the disk.
func TestLiveRestoredOverlayMustBeTheSnapshotsOwnImage(t *testing.T) {
	cfg, tmpl := liveCfg(t)
	log := zap.NewNop()
	m := liveManager(t, cfg)

	cfg.SnapshotDir = sidecarTemplates(t, cfg, tmpl)
	before := filepath.Join(cfg.OverlayDir, "s4-before-"+randHex(4)+".ext4")
	t.Cleanup(func() { os.Remove(before) })

	p := &goldenPayload{dir: "/root/s4-" + randHex(4), marker: randHex(16)}
	snapDir, golden := buildGoldenSnapshot(t, m, cfg, tmpl, writeGoldenPayload(p, before))

	real := filepath.Join(snapDir, goldenOverlayFile)
	if err := os.Rename(real, real+".keep"); err != nil {
		t.Fatalf("set aside the real golden image: %v", err)
	}
	if err := copySparse(before, real); err != nil {
		t.Fatalf("install the pre-payload image: %v", err)
	}
	rs, err := os.Stat(real)
	if err != nil {
		t.Fatalf("stat the mutated image: %v", err)
	}
	ks, err := os.Stat(real + ".keep")
	if err != nil {
		t.Fatalf("stat the real image: %v", err)
	}
	if rs.Size() != ks.Size() {
		t.Fatalf("the mutation changed the image SIZE (%d vs %d); the restore could then "+
			"fail for a reason that is not about content", rs.Size(), ks.Size())
	}
	if !snapshotValid(snapDir) {
		t.Fatal("the mutated snapshot no longer looks valid, so the restore below would " +
			"be refused before it read anything")
	}
	t.Logf("MUTATION APPLIED: %s replaced by the pre-payload copy (%d bytes apparent, "+
		"unchanged), snapshot still valid, golden %+v", goldenOverlayFile, rs.Size(), golden)

	id := "s4mut-" + randHex(4)
	token, hash := sbtoken.Mint()
	vm, err := restoreVersion(tmpl, id, snapDir, cfg, log, hash, nil)
	if err != nil {
		t.Logf("MUTATION FIRED: with the pre-payload image the restore was refused "+
			"outright: %v", err)
		return
	}
	defer vm.Destroy()

	c, err := envd.ConnectWithToken(vm.VsockPath, token, 45*time.Second)
	if err != nil {
		t.Logf("MUTATION FIRED: the guest restored onto the pre-payload image never "+
			"became usable: %v", err)
		return
	}
	defer c.Close()

	out, stderr, ok := execTry(t, c, strings.Join([]string{
		"sync",
		"echo 3 > /proc/sys/vm/drop_caches",
		"echo MARKER=$(cat " + p.dir + "/marker 2>/dev/null)",
		"echo SHA=$(sha256sum " + p.dir + "/blob 2>/dev/null | cut -d' ' -f1)",
	}, "\n"))
	if !ok {
		t.Logf("MUTATION FIRED: the guest could not be driven after restoring onto the "+
			"pre-payload image: %s", stderr)
		return
	}
	got := kv(t, out, "MARKER", "SHA")
	if got["MARKER"] == p.marker || got["SHA"] == p.sha {
		t.Fatalf("MUTATION DID NOT FIRE: a guest restored onto an image that NEVER "+
			"CONTAINED the payload read it back anyway (marker=%q sha=%q). The reads in "+
			"TestLiveRestoredOverlayCarriesTheGoldenImageContent are being served from the "+
			"restored page cache or from the read-only squashfs, not from the snapshot's "+
			"own disk, and that test cannot fail", got["MARKER"], got["SHA"])
	}
	t.Logf("MUTATION FIRED: restored onto the pre-payload image, the payload is absent "+
		"(marker=%q sha=%q). The content test above is reading the DISK", got["MARKER"], got["SHA"])
}

// Two concurrent restores must hold distinct overlays and distinct credentials.
func TestLiveRestoresAreDisjoint(t *testing.T) {
	cfg, tmpl := liveCfg(t)
	log := zap.NewNop()
	m := liveManager(t, cfg)

	cfg.SnapshotDir = sidecarTemplates(t, cfg, tmpl)
	// Positive control: both guests must read this, or "cannot see" only means "cannot read".
	shared := &goldenPayload{dir: "/root/s4-" + randHex(4), marker: randHex(16)}
	_, golden := buildGoldenSnapshot(t, m, cfg, tmpl, writeGoldenPayload(shared, ""))

	type restored struct {
		id     string
		token  string
		hash   string
		secret string
		vm     *RestoredVM
		err    error
	}
	pair := []*restored{{id: "s4a-" + randHex(4)}, {id: "s4b-" + randHex(4)}}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, r := range pair {
		r := r
		r.token, r.hash = sbtoken.Mint()
		r.secret = randHex(16)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r.vm, r.err = RestoreFromSnapshot(tmpl, r.id, cfg, log, r.hash, nil)
		}()
	}
	close(start)
	wg.Wait()

	for _, r := range pair {
		if r.vm != nil {
			defer r.vm.Destroy()
		}
	}
	for _, r := range pair {
		if r.err != nil {
			t.Fatalf("restore %s failed: %v", r.id, r.err)
		}
	}
	a, b := pair[0], pair[1]

	if a.vm.OverlayPath == b.vm.OverlayPath {
		t.Fatalf("both restored sandboxes are on the overlay %s", a.vm.OverlayPath)
	}
	if filepath.Dir(a.vm.OverlayPath) == filepath.Dir(b.vm.OverlayPath) {
		t.Fatalf("both overlays are in %s; the snapshot pins the BASENAME, so one "+
			"directory means one file", filepath.Dir(a.vm.OverlayPath))
	}
	ai, err := os.Stat(a.vm.OverlayPath)
	if err != nil {
		t.Fatalf("stat %s: %v", a.vm.OverlayPath, err)
	}
	bi, err := os.Stat(b.vm.OverlayPath)
	if err != nil {
		t.Fatalf("stat %s: %v", b.vm.OverlayPath, err)
	}
	if os.SameFile(ai, bi) {
		t.Fatalf("the two overlay paths are the same file (hard link or bind): %s and %s",
			a.vm.OverlayPath, b.vm.OverlayPath)
	}
	t.Logf("structural: %s -> %s, %s -> %s (distinct inodes)",
		a.id, a.vm.OverlayPath, b.id, b.vm.OverlayPath)

	if a.hash == b.hash {
		t.Fatal("the two sandboxes were minted the same credential")
	}
	clients := map[string]*envd.Client{}
	for _, r := range pair {
		c, err := envd.ConnectWithToken(r.vm.VsockPath, r.token, 60*time.Second)
		if err != nil {
			t.Fatalf("%s: unreachable at %s: %v", r.id, r.vm.VsockPath, err)
		}
		defer c.Close()
		clients[r.id] = c
	}
	for i, r := range pair {
		other := pair[1-i]
		if c, err := envd.ConnectWithToken(other.vm.VsockPath, r.token, 8*time.Second); err == nil {
			c.Close()
			t.Fatalf("%s's token was accepted at %s's address — the two restores share a "+
				"credential", r.id, other.id)
		}
		t.Logf("CONTROL: %s's valid token is refused at %s (%s)", r.id, other.id, other.vm.VsockPath)
	}

	for _, r := range pair {
		got := readPayload(t, clients[r.id], *shared, r.id)
		if got["MARKER"] != shared.marker || got["SHA"] != shared.sha {
			t.Fatalf("%s cannot read the golden image's own content (marker=%q sha=%q); "+
				"every 'cannot see the neighbour's file' result below would then just mean "+
				"'cannot read'", r.id, got["MARKER"], got["SHA"])
		}
		t.Logf("POSITIVE CONTROL %s: reads the shared golden payload from its own disk "+
			"(sha256=%s)", r.id, got["SHA"])
	}

	both := shared.dir + "/tenant"
	for _, r := range pair {
		execLong(t, clients[r.id], "echo "+r.secret+" > "+both+"\n"+
			"echo "+r.secret+" > "+shared.dir+"/only-"+r.id+"\nsync")
	}
	for _, r := range pair {
		dropGuestCaches(t, clients[r.id], r.id+" (after both wrote)")
	}
	for i, r := range pair {
		other := pair[1-i]
		out := execLong(t, clients[r.id], strings.Join([]string{
			"echo MINE=$(cat " + both + " 2>/dev/null)",
			"echo OWNFILE=$(cat " + shared.dir + "/only-" + r.id + " 2>/dev/null)",
			"echo PEERFILE=$(cat " + shared.dir + "/only-" + other.id + " 2>/dev/null)",
			"echo SHARED=$(cat " + shared.dir + "/marker 2>/dev/null)",
		}, "\n"))
		g := kv(t, out, "MINE", "OWNFILE", "PEERFILE", "SHARED")

		if g["SHARED"] != shared.marker {
			t.Fatalf("%s cannot read the shared marker (%q); PEERFILE=%q proves nothing",
				r.id, g["SHARED"], g["PEERFILE"])
		}
		if g["OWNFILE"] != r.secret {
			t.Fatalf("%s cannot read back a file it just wrote (%q want %q); PEERFILE=%q "+
				"proves nothing", r.id, g["OWNFILE"], r.secret, g["PEERFILE"])
		}
		if g["MINE"] != r.secret {
			t.Fatalf("%s wrote %s to %s and reads back %q — the two sandboxes are writing "+
				"to one disk", r.id, r.secret, both, g["MINE"])
		}
		if g["PEERFILE"] != "" {
			t.Fatalf("%s can see %s's private file (%q). The two restored sandboxes share "+
				"an overlay", r.id, other.id, g["PEERFILE"])
		}
		t.Logf("%s: MINE=%s (its own), OWNFILE ok, SHARED ok, %s's private file invisible",
			r.id, g["MINE"], other.id)
	}
	t.Logf("4.2 / 5.2 PROVED: two sandboxes restored concurrently from the golden snapshot "+
		"pinning %+v hold distinct overlays (distinct inodes), distinct credentials (each "+
		"refused at the other's address) and disjoint filesystem contents, while both "+
		"still read the shared golden payload from their own disks", golden)
}
