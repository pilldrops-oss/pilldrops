//go:build linux

package vm

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The tap counter resets on restart and a golden VM's tap is deleted after the
// snapshot, so without the reservation the first sandbox after a restart takes
// the golden name.
func TestAllocateVMSkipsReservedTaps(t *testing.T) {
	// Reserve the next three names the counter is about to produce.
	start := ipCounter.Load()
	var reserved []string
	for i := uint32(1); i <= 3; i++ {
		name := tapNameFor(start + i)
		reserved = append(reserved, name)
		ReserveGoldenTap(name)
	}
	t.Cleanup(func() { unreserveTaps(reserved) })

	_, got := allocateVM()
	for _, r := range reserved {
		if got == r {
			t.Fatalf("allocateVM handed out %s, which a golden snapshot has reserved", got)
		}
	}
}

// unreserveTaps undoes ReserveGoldenTap; production never un-reserves.
func unreserveTaps(names []string) {
	reservedTapsMu.Lock()
	defer reservedTapsMu.Unlock()
	for _, n := range names {
		delete(reservedTaps, n)
	}
}

func TestReserveGoldenTapIgnoresEmpty(t *testing.T) {
	ReserveGoldenTap("")
	if tapReserved("") {
		t.Fatal(`"" was reserved; every allocation would then loop forever`)
	}
}

// Two restores of one snapshot both need the same tap name, and only one such device can exist.
func TestClaimGoldenTapSerialises(t *testing.T) {
	const name = "fctap-claim-test"
	var mu sync.Mutex
	inside, maxInside := 0, 0

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release := claimGoldenTap(name)
			mu.Lock()
			inside++
			if inside > maxInside {
				maxInside = inside
			}
			mu.Unlock()
			time.Sleep(2 * time.Millisecond)
			mu.Lock()
			inside--
			mu.Unlock()
			release()
		}()
	}
	wg.Wait()
	if maxInside != 1 {
		t.Fatalf("%d goroutines held the claim on %s at once", maxInside, name)
	}
}

func TestClaimGoldenTapIsPerName(t *testing.T) {
	releaseA := claimGoldenTap("fctap-name-a")
	defer releaseA()

	done := make(chan struct{})
	go func() {
		claimGoldenTap("fctap-name-b")()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("claiming one snapshot's tap name blocked a different one")
	}
}

func TestGoldenIdentityRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := goldenIdentity{TapName: "fctap4", VMIP: "192.168.0.4", OverlayBase: "golden-base-ab12cd34.ext4"}
	if err := writeGoldenIdentity(dir, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readGoldenIdentity(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != want {
		t.Fatalf("round trip: got %+v want %+v", got, want)
	}
}

func TestGoldenIdentityRejections(t *testing.T) {
	cases := map[string]string{
		"missing file": "",
		"not json":     "{",
		"no tap":       `{"vmip":"192.168.0.4","overlay_base":"g.ext4"}`,
		"no vmip":      `{"tap_name":"fctap4","overlay_base":"g.ext4"}`,
		"no overlay":   `{"tap_name":"fctap4","vmip":"192.168.0.4"}`,
		"bad vmip":     `{"tap_name":"fctap4","vmip":"not-an-ip","overlay_base":"g.ext4"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if body != "" {
				if err := os.WriteFile(filepath.Join(dir, goldenIdentityFile), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := readGoldenIdentity(dir); err == nil {
				t.Fatalf("accepted %q", body)
			}
		})
	}
}

// Snapshots written before golden.json existed must be rebuilt, not restored blind.
func TestSnapshotValidRequiresGoldenIdentity(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"state", "mem"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, goldenOverlayFile), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if snapshotValid(dir) {
		t.Fatal("a snapshot with state+mem and no golden.json was accepted as valid")
	}
	if err := writeGoldenIdentity(dir, goldenIdentity{
		TapName: "fctap4", VMIP: "192.168.0.4", OverlayBase: "g.ext4",
	}); err != nil {
		t.Fatal(err)
	}
	if !snapshotValid(dir) {
		t.Fatal("a complete snapshot was rejected")
	}
}

// The snapshot pins the overlay basename, so two restores need the same file
// name in different directories; the content must be a private copy of the
// golden image, since the restored memory holds its mounted state.
func TestRestoreOverlayIsAPrivateCopyUnderTheGoldenName(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("SKIP (not a pass): the overlay copy chowns, which needs root")
	}
	cfg := Config{OverlayDir: t.TempDir()}
	const goldenBase = "golden-base-ab12cd34.ext4"

	// The marker proves the restored image is the golden one, not a fresh file.
	snapDir := t.TempDir()
	const marker = "golden-image-marker"
	if err := os.WriteFile(filepath.Join(snapDir, goldenOverlayFile), []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}

	a, err := createRestoreOverlay("sb-aaa", 0, snapDir, goldenBase, cfg)
	if err != nil {
		t.Fatalf("A: %v", err)
	}
	b, err := createRestoreOverlay("sb-bbb", 0, snapDir, goldenBase, cfg)
	if err != nil {
		t.Fatalf("B: %v", err)
	}
	if a == b {
		t.Fatalf("two sandboxes were given ONE overlay path: %s", a)
	}
	if filepath.Base(a) != goldenBase || filepath.Base(b) != goldenBase {
		t.Fatalf("overlays are not named for the golden image: %s, %s", a, b)
	}
	for _, p := range []string{a, b} {
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if string(got) != marker {
			t.Fatalf("%s is not a copy of the golden image (%q)", p, got)
		}
	}

	// Private: writing through A must not be visible through B.
	if err := os.WriteFile(a, []byte("A-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != marker {
		t.Fatalf("a write to A's overlay changed B's: %q — they are one file", got)
	}

	// Nor the snapshot's own image: a write-through would corrupt the template
	// for every later create.
	if src, err := os.ReadFile(filepath.Join(snapDir, goldenOverlayFile)); err != nil {
		t.Fatalf("read the golden image: %v", err)
	} else if string(src) != marker {
		t.Fatalf("a write to A's overlay changed the SNAPSHOT's own image (%q) — the "+
			"first restore would corrupt the template for every create after it", src)
	}

	// The per-sandbox directory goes away with the file.
	deleteOverlay(a)
	if _, err := os.Stat(filepath.Dir(a)); !os.IsNotExist(err) {
		t.Errorf("deleteOverlay left %s behind", filepath.Dir(a))
	}
	if _, err := os.Stat(b); err != nil {
		t.Errorf("deleting A's overlay disturbed B's: %v", err)
	}
	deleteOverlay(b)
}

// Without the image the restored guest dies at an arbitrary later moment rather than at start.
func TestSnapshotValidRequiresTheGoldenImage(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"state", "mem"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeGoldenIdentity(dir, goldenIdentity{
		TapName: "fctap4", VMIP: "192.168.0.4", OverlayBase: "g.ext4",
	}); err != nil {
		t.Fatal(err)
	}
	if snapshotValid(dir) {
		t.Fatal("a snapshot with no golden image was accepted as valid")
	}
	if err := os.WriteFile(filepath.Join(dir, goldenOverlayFile), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !snapshotValid(dir) {
		t.Fatal("a complete snapshot was rejected")
	}
}

func TestDeleteOverlayLeavesTheOverlayDirAlone(t *testing.T) {
	dir := t.TempDir()
	flat := filepath.Join(dir, "sb-flat.ext4")
	if err := os.WriteFile(flat, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	deleteOverlay(flat)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("deleteOverlay removed the overlay directory itself: %v", err)
	}
}
