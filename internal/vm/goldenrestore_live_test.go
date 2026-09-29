//go:build linux

package vm

// Live verification: two sandboxes restored from one golden snapshot at the
// same time, both driven, neither able to reach the other. Sequential restores
// never collide, so concurrency is the point. See credpush_live_test.go to run it.

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
	"github.com/pilldrops-oss/pilldrops/internal/envd"
)

// terminalPort is envd's plain-HTTP web terminal; curl gets a real status from it.
const terminalPort = 7681

// hostCurl runs curl on the node, the positive control that the target is up.
func hostCurl(t *testing.T, ip string) string {
	t.Helper()
	out, _ := exec.Command("curl", "-s", "-m", "5", "-o", "/dev/null",
		"-w", "%{http_code}", fmt.Sprintf("http://%s:%d/", ip, terminalPort)).CombinedOutput()
	return strings.TrimSpace(string(out))
}

// guestProbes is one guest command reporting identity and reachability
// together, so a blocked peer always sits next to proof the probe can reach
// something. curl, not /dev/tcp: the guest's /bin/sh is dash, which has none.
func guestProbes(own, golden, peer string) string {
	get := func(ip string, port int, timeout int) string {
		return fmt.Sprintf("curl -s -m %d -o /dev/null -w '%%{http_code}' http://%s:%d/; echo \":$?\"",
			timeout, ip, port)
	}
	return strings.Join([]string{
		// Identity, as the guest sees it.
		"echo MAC=$(cat /sys/class/net/eth0/address)",
		"echo TRIE=$(grep -c . /proc/net/fib_trie)",
		"echo OWN=$(grep -c '|-- " + own + "' /proc/net/fib_trie)",
		"echo GOLDEN=$(grep -c '|-- " + golden + "' /proc/net/fib_trie)",
		// Reachability.
		"echo SELF=$(" + get(own, terminalPort, 5) + ")",
		"echo PEER=$(" + get(peer, terminalPort, 5) + ")",
		"echo NET=$(" + get("1.1.1.1", 80, 8) + ")",
	}, "\n")
}

// probeResult is guestProbes' output, parsed.
type probeResult map[string]string

func parseProbes(t *testing.T, out string) probeResult {
	t.Helper()
	r := probeResult{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			r[k] = v
		}
	}
	for _, k := range []string{"MAC", "TRIE", "OWN", "GOLDEN", "SELF", "PEER", "NET"} {
		if _, ok := r[k]; !ok {
			t.Fatalf("the guest probe did not report %s; its whole output was:\n%s", k, out)
		}
	}
	return r
}

// reached parses "<http_code>:<curl exit>", e.g. "200:0" or "000:28" for a timeout.
func reached(v string) bool {
	code, rc, ok := strings.Cut(v, ":")
	return ok && rc == "0" && code != "000" && code != ""
}

func atoiOr(t *testing.T, s string, field string) int {
	t.Helper()
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		t.Fatalf("%s = %q, which is not a number", field, s)
	}
	return n
}

// sidecarTemplates builds a private SNAPSHOT_DIR so the node's template tree
// is never written. It must share a filesystem with /srv/jailer because the
// SDK hard-links the rootfs into the chroot.
func sidecarTemplates(t *testing.T, cfg Config, tmpl string) string {
	t.Helper()
	dir, err := os.MkdirTemp(cfg.OverlayDir, "s3tmpl-")
	if err != nil {
		t.Fatalf("side-car template dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.MkdirAll(filepath.Join(dir, tmpl), 0o755); err != nil {
		t.Fatalf("side-car template %s: %v", tmpl, err)
	}
	src := filepath.Join(cfg.SnapshotDir, tmpl, "rootfs.squashfs")
	if err := os.Link(src, filepath.Join(dir, tmpl, "rootfs.squashfs")); err != nil {
		t.Fatalf("hard-link %s into the side-car: %v", src, err)
	}
	return dir
}

// goldenPrep writes into the golden VM's image before it boots. Not through
// the guest: a golden VM holds no credential, and pushing one would bake a
// hash into the snapshot every restore then inherits.
type goldenPrep func(t *testing.T, overlayPath string)

// buildGoldenSnapshot cold-boots a golden VM with the injected envd, snapshots
// it into the side-car's v1 directory, and records the pinned identity. Taken
// by hand rather than via createOneSnapshot, which boots the template's baked envd.
func buildGoldenSnapshot(t *testing.T, m *Manager, cfg Config, tmpl string, prep ...goldenPrep) (string, goldenIdentity) {
	t.Helper()
	ctx := context.Background()

	goldenID := "s3gold-" + randomHex(4)
	machine, netw, overlay := bootGoldenVM(t, cfg, tmpl, goldenID, prep...)
	addr := netw.VMIP.String() + ":6001"
	t.Logf("golden VM %s: tap=%s ip=%s overlay=%s", goldenID, netw.TapName, netw.VMIP, filepath.Base(overlay))

	// Must reach ready with no credential.
	c, err := envd.Connect(addr, 90*time.Second)
	if err != nil {
		_ = machine.Stop(ctx)
		deleteOverlay(overlay)
		TeardownVMNetwork(netw.TapName)
		t.Fatalf("golden VM never became ready: %v", err)
	}

	m.mu.Lock()
	m.active[goldenID] = &Sandbox{
		ID: goldenID, Template: tmpl, Machine: machine, Envd: c,
		OverlayPath: overlay, TapName: netw.TapName, VMIP: netw.VMIP.String(),
		Token: "", stopTimer: func() {},
	}
	m.mu.Unlock()

	snapDir := snapshotDir(tmpl, "v1", cfg)
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", snapDir, err)
	}
	if err := m.SnapshotSandbox(ctx, goldenID, snapDir); err != nil {
		t.Fatalf("SnapshotSandbox: %v", err)
	}
	// Keep the image beside the snapshot as createOneSnapshot does: the
	// restored guest's memory holds this filesystem's mounted state.
	if err := copySparse(overlay, filepath.Join(snapDir, goldenOverlayFile)); err != nil {
		t.Fatalf("preserve the golden overlay: %v", err)
	}
	deleteOverlay(overlay)
	// The tap must go: every restore below claims its name.
	TeardownVMNetwork(netw.TapName)

	id := goldenIdentity{
		TapName:     netw.TapName,
		VMIP:        netw.VMIP.String(),
		OverlayBase: filepath.Base(overlay),
	}
	if err := writeGoldenIdentity(snapDir, id); err != nil {
		t.Fatalf("writeGoldenIdentity: %v", err)
	}
	ReserveGoldenTap(id.TapName)
	if !snapshotValid(snapDir) {
		t.Fatalf("the snapshot this test just built is not valid: %s", snapDir)
	}
	return snapDir, id
}

// "A cannot reach B" only means something while B exists, so both halves run together.
func TestLiveConcurrentGoldenRestore(t *testing.T) {
	cfg, tmpl := liveCfg(t)
	log := zap.NewNop()

	m := liveManager(t, cfg) // before any VM exists

	cfg.SnapshotDir = sidecarTemplates(t, cfg, tmpl)
	snapDir, golden := buildGoldenSnapshot(t, m, cfg, tmpl)
	t.Logf("golden snapshot at %s pins %+v", snapDir, golden)

	type restored struct {
		id    string
		token string
		vm    *RestoredVM
		err   error
		ms    int64
	}
	pair := []*restored{{id: "s3a-" + randomHex(4)}, {id: "s3b-" + randomHex(4)}}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, r := range pair {
		r := r
		token, hash := sbtoken.Mint()
		r.token = token
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // released together, so the two restores genuinely overlap
			t0 := time.Now()
			r.vm, r.err = RestoreFromSnapshot(tmpl, r.id, cfg, log, hash, nil)
			r.ms = time.Since(t0).Milliseconds()
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
		t.Logf("restored %s in %d ms: ip=%s tap=%s overlay=%s",
			r.id, r.ms, r.vm.VMIP, r.vm.TapName, r.vm.OverlayPath)
	}
	a, b := pair[0], pair[1]

	if a.vm.VMIP == b.vm.VMIP {
		t.Fatalf("both restored sandboxes are at %s — the collision is still there", a.vm.VMIP)
	}
	if a.vm.TapName == b.vm.TapName {
		t.Fatalf("both restored sandboxes are on tap %s", a.vm.TapName)
	}
	if a.vm.OverlayPath == b.vm.OverlayPath {
		t.Fatalf("both restored sandboxes share the overlay %s", a.vm.OverlayPath)
	}
	for _, r := range pair {
		if r.vm.VMIP == golden.VMIP {
			t.Fatalf("%s came up on the GOLDEN address %s", r.id, golden.VMIP)
		}
		if r.vm.TapName == golden.TapName {
			t.Fatalf("%s is still on the golden device name %s — the rename did not happen, "+
				"and a second restore could not have been given a tap at all", r.id, golden.TapName)
		}
	}

	clients := map[string]*envd.Client{}
	for _, r := range pair {
		c, err := envd.ConnectWithToken(r.vm.VsockPath, r.token, 60*time.Second)
		if err != nil {
			t.Fatalf("%s: the node dialled %s and got nothing — the address it reports is not "+
				"one the guest holds: %v", r.id, r.vm.VsockPath, err)
		}
		defer c.Close()
		clients[r.id] = c
		if got := execIn(t, c, "echo driven-"+r.id); got != "driven-"+r.id {
			t.Fatalf("%s: exec returned %q", r.id, got)
		}
		t.Logf("%s driven: authenticated exec on %s ok", r.id, r.vm.VsockPath)
	}

	// Per-sandbox credentials prove the two addresses are two VMs, not one answering twice.
	if c, err := envd.ConnectWithToken(b.vm.VsockPath, a.token, 8*time.Second); err == nil {
		c.Close()
		t.Fatal("A's token was accepted at B's address — the two restores are not two sandboxes")
	} else {
		t.Logf("CONTROL: A's token is refused at B's address (%v)", err)
	}

	results := map[string]probeResult{}
	for i, r := range pair {
		peer := pair[1-i].vm.VMIP
		out := execIn(t, clients[r.id], guestProbes(r.vm.VMIP, golden.VMIP, peer))
		t.Logf("%s probes:\n%s", r.id, out)
		results[r.id] = parseProbes(t, out)
	}

	for i, r := range pair {
		p := results[r.id]

		// Positive control: an unreadable fib_trie would make every grep -c 0.
		if atoiOr(t, p["TRIE"], "TRIE") == 0 {
			t.Fatalf("%s: /proc/net/fib_trie is empty or unreadable, so OWN=%s and GOLDEN=%s "+
				"prove nothing", r.id, p["OWN"], p["GOLDEN"])
		}
		if atoiOr(t, p["OWN"], "OWN") == 0 {
			t.Fatalf("%s: the guest does not hold %s, the address the node reports for it",
				r.id, r.vm.VMIP)
		}
		if atoiOr(t, p["GOLDEN"], "GOLDEN") != 0 {
			t.Fatalf("%s: the guest still holds the GOLDEN address %s alongside its own — "+
				"the rewrite added an address instead of replacing one, and two sandboxes "+
				"are answering on one address", r.id, golden.VMIP)
		}

		// Two guests on one MAC get each other's replies via the bridge FDB.
		want := guestNetworkFor(net.ParseIP(r.vm.VMIP)).MAC
		if p["MAC"] != want {
			t.Fatalf("%s: eth0 MAC is %s, want %s (the MAC the node derives from this "+
				"sandbox's address)", r.id, p["MAC"], want)
		}
		_ = i
	}
	if results[a.id]["MAC"] == results[b.id]["MAC"] {
		t.Fatalf("both restored guests are on MAC %s", results[a.id]["MAC"])
	}
	t.Logf("MACs are distinct: %s=%s  %s=%s", a.id, results[a.id]["MAC"], b.id, results[b.id]["MAC"])

	// The restore path creates, renames and bridges its own tap, so isolation
	// is re-proven here rather than inherited from the cold path.
	for _, r := range pair {
		if got := hostCurl(t, r.vm.VMIP); got != "200" {
			t.Fatalf("the NODE could not reach %s's terminal at %s:%d (got %q). Every "+
				"'blocked' result below would then be a sandbox that was never reachable "+
				"by anyone", r.id, r.vm.VMIP, terminalPort, got)
		}
		t.Logf("POSITIVE CONTROL: the node reaches %s at %s:%d -> 200", r.id, r.vm.VMIP, terminalPort)
	}

	for i, r := range pair {
		p := results[r.id]
		peer := pair[1-i]

		if !reached(p["SELF"]) {
			t.Fatalf("%s: curl inside the guest could not reach the guest's OWN terminal "+
				"(%s). The probe cannot open sockets, so PEER=%s proves nothing — this is "+
				"the dash /dev/tcp failure in a different costume", r.id, p["SELF"], p["PEER"])
		}
		if !reached(p["NET"]) {
			t.Fatalf("%s: the guest cannot reach the internet (NET=%s). C1 is not just "+
				"'sandboxes are cut off'; egress has to keep working", r.id, p["NET"])
		}
		if reached(p["PEER"]) {
			t.Fatalf("C1 REGRESSION: %s reached %s's terminal at %s:%d (PEER=%s). Two "+
				"sandboxes on this node can talk to each other",
				r.id, peer.id, peer.vm.VMIP, terminalPort, p["PEER"])
		}
		t.Logf("C1 %s: SELF=%s NET=%s PEER(%s)=%s — reaches itself and the internet, "+
			"not its neighbour", r.id, p["SELF"], p["NET"], peer.vm.VMIP, p["PEER"])
	}

	// Control: both taps are isolated ports of fcbr0.
	for _, r := range pair {
		out, err := exec.Command("bridge", "-d", "link", "show", "dev", r.vm.TapName).CombinedOutput()
		if err != nil {
			t.Fatalf("bridge link show %s: %v: %s", r.vm.TapName, err, out)
		}
		s := string(out)
		if !strings.Contains(s, "master fcbr0") {
			t.Fatalf("%s's tap %s is not a port of fcbr0:\n%s", r.id, r.vm.TapName, s)
		}
		if !strings.Contains(s, "isolated on") {
			t.Fatalf("%s's tap %s is on fcbr0 WITHOUT isolation:\n%s", r.id, r.vm.TapName, s)
		}
		t.Logf("C1 %s: tap %s is an isolated port of fcbr0", r.id, r.vm.TapName)
	}
}

// Mutation: with the SETNET push suppressed the guest keeps the golden address
// and the restore must fail. Without this the test above passes against an
// implementation that allocates addresses and never tells the guest.
func TestLiveGoldenRestoreDegradesWhenTheRewriteIsSkipped(t *testing.T) {
	cfg, tmpl := liveCfg(t)
	log := zap.NewNop()
	m := liveManager(t, cfg)

	cfg.SnapshotDir = sidecarTemplates(t, cfg, tmpl)
	_, golden := buildGoldenSnapshot(t, m, cfg, tmpl)

	orig := deliverGuestNetworkFn
	deliverGuestNetworkFn = func(chrootRoot, sandboxID string, g guestNetwork, d time.Duration) error {
		return nil // accepted, applied nowhere
	}
	t.Cleanup(func() { deliverGuestNetworkFn = orig })

	id := "s3mut-" + randomHex(4)
	token, hash := sbtoken.Mint()
	vm, err := RestoreFromSnapshot(tmpl, id, cfg, log, hash, nil)
	if err != nil {
		t.Logf("MUTATION: with the rewrite suppressed the restore failed outright: %v", err)
		return
	}
	defer vm.Destroy()

	if vm.VMIP == golden.VMIP {
		t.Fatalf("the mutation did not take effect: the node reported the golden address %s", vm.VMIP)
	}
	c, err := envd.ConnectWithToken(vm.VsockPath, token, 20*time.Second)
	if err == nil {
		c.Close()
		t.Fatalf("MUTATION DID NOT FIRE: with SETNET suppressed the guest still answered at %s. "+
			"Either the guest is getting its address some other way, or the concurrent test "+
			"above cannot fail", vm.VsockPath)
	}
	t.Logf("MUTATION fired: with SETNET suppressed, %s is unreachable at the address the node "+
		"allocated for it (%v)", id, err)
}
