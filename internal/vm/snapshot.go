//go:build linux

package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"go.uber.org/zap"

	"github.com/pilldrops-oss/pilldrops/internal/egress"
	"github.com/pilldrops-oss/pilldrops/internal/envd"
	"github.com/pilldrops-oss/pilldrops/internal/envdproto"
)

// errGoldenSkipped means a build was not started: one is already running, or the
// last one for this exact rootfs failed recently or was refused outright.
var errGoldenSkipped = errors.New("golden snapshot build skipped")

// goldens is the node's one record of golden builds (see goldenBuilds).
var goldens = &goldenBuilds{state: map[string]goldenBuildState{}}

// snapshotVersions is the rotation order; if every version fails, rebuild.
var snapshotVersions = []string{"v1", "v2"}

// goldenIdentity records what Firecracker pins inside the microVM state: the
// tap name it will open, the guest address, and the overlay basename it will
// look for. A restore that guesses any of them fails or collides.
type goldenIdentity struct {
	TapName     string `json:"tap_name"`
	VMIP        string `json:"vmip"`
	OverlayBase string `json:"overlay_base"`
	// StorageMB is the disk the golden was taken with. A golden whose size is
	// not the template's current default serves no default create, so it is
	// rebuilt; one from before this was recorded (0) is too.
	StorageMB int `json:"storage_mb,omitempty"`
}

const goldenIdentityFile = "golden.json"

// goldenOverlayFile is the writable image the snapshot was taken with.
const goldenOverlayFile = "overlay.ext4"

func writeGoldenIdentity(dir string, g goldenIdentity) error {
	b, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	// Only the node reads this, unlike state/mem which every VMM uid opens.
	return os.WriteFile(filepath.Join(dir, goldenIdentityFile), b, 0o600)
}

func readGoldenIdentity(dir string) (goldenIdentity, error) {
	var g goldenIdentity
	b, err := os.ReadFile(filepath.Join(dir, goldenIdentityFile))
	if err != nil {
		return g, fmt.Errorf("read %s: %w", goldenIdentityFile, err)
	}
	if err := json.Unmarshal(b, &g); err != nil {
		return g, fmt.Errorf("parse %s: %w", goldenIdentityFile, err)
	}
	if g.TapName == "" || g.VMIP == "" || g.OverlayBase == "" {
		return g, fmt.Errorf("%s is incomplete: %+v", goldenIdentityFile, g)
	}
	if net.ParseIP(g.VMIP) == nil {
		return g, fmt.Errorf("%s has an invalid vmip %q", goldenIdentityFile, g.VMIP)
	}
	return g, nil
}

// ValidateOrCreateSnapshot seals and registers existing snapshots, rebuilding when none is valid.
func ValidateOrCreateSnapshot(template string, cfg Config, log *zap.Logger) error {
	ok, err := validateSnapshots(template, cfg, log)
	if err != nil || ok {
		return err
	}
	log.Info("no_valid_snapshots_rebuilding", zap.String("template", template))
	return RebuildSnapshots(template, cfg, log)
}

// validateSnapshots seals and registers the snapshots already on disk and reports
// whether any is restorable. It boots nothing, so it is safe on the startup path.
func validateSnapshots(template string, cfg Config, log *zap.Logger) (bool, error) {
	valid := 0
	for _, ver := range snapshotVersions {
		dir := snapshotDir(template, ver, cfg)
		if snapshotValid(dir) {
			// Older snapshots are owned by an allocatable sandbox uid. Failing to
			// seal fails startup: skipping would leave RestoreFromSnapshot, which
			// never comes back through here, serving from that image.
			if err := sealGoldenDir(dir); err != nil {
				return false, fmt.Errorf("refusing to serve golden snapshots for %s/%s: %w",
					template, ver, err)
			}
			// Sealed first, so an unsealable golden is refused whatever its size.
			if !goldenSized(dir, cfg, template) {
				log.Info("golden_wrong_size_rebuilding", zap.String("template", template),
					zap.String("version", ver), zap.Int("want_storage_mb", goldenStorageMB(cfg, template)))
				continue
			}
			valid++
			if g, err := readGoldenIdentity(dir); err == nil {
				ReserveGoldenTap(g.TapName)
			}
			log.Info("snapshot_ok",
				zap.String("template", template),
				zap.String("version", ver),
			)
		}
	}

	return valid > 0, nil
}

// RestoreFromSnapshot tries each version in turn, then triggers a rebuild.
// A template with no golden yet returns ErrNoGolden at once and starts at most
// one build in the background.
// tokenHash is pushed to the guest over vsock after resume; empty means auth is off.
func RestoreFromSnapshot(template, sandboxID string, cfg Config, log *zap.Logger, tokenHash string, rules *egress.Rules) (*RestoredVM, error) {
	tried := false
	for _, ver := range snapshotVersions {
		dir := snapshotDir(template, ver, cfg)
		// A golden of another size would hand this sandbox that size's disk,
		// past its cap; it is not a restore candidate until it is rebuilt.
		if !snapshotValid(dir) || !goldenSized(dir, cfg, template) {
			continue
		}
		tried = true

		vm, err := restoreVersion(template, sandboxID, dir, cfg, log, tokenHash, rules)
		if err != nil {
			log.Warn("snapshot_restore_failed",
				zap.String("template", template),
				zap.String("version", ver),
				zap.Error(err),
			)
			// Mark this version corrupt so the next restore skips it.
			_ = os.Remove(filepath.Join(dir, "state"))
			continue
		}

		// The golden image was written whenever this template was baked, which on
		// a node that fetched a prebuilt one is weeks ago, and nothing in the
		// guest keeps time across a restore. Same push the sandbox restore path
		// makes, for the same reason; best-effort, like the hostname.
		if err := deliverGuestClock(vm.Machine.ChrootRoot(), sandboxID); err != nil {
			log.Warn("guest_clock_not_set", zap.String("sandbox_id", sandboxID), zap.Error(err))
		}

		log.Info("snapshot_restored",
			zap.String("template", template),
			zap.String("version", ver),
			zap.String("sandbox_id", sandboxID),
		)
		return vm, nil
	}

	if tried {
		log.Error("all_snapshots_failed_rebuilding", zap.String("template", template))
	}
	go func() {
		if err := RebuildSnapshots(template, cfg, log); err != nil && !errors.Is(err, errGoldenSkipped) {
			log.Error("snapshot_rebuild_failed", zap.String("template", template), zap.Error(err))
		}
	}()
	if !tried {
		return nil, ErrNoGolden
	}
	return nil, fmt.Errorf("all snapshot versions failed for %s", template)
}

// RebuildSnapshots creates a fresh snapshot for every version of a template. At
// most one build per template runs at a time, and a failed or refused one is not
// retried on every create (see goldenBuilds).
func RebuildSnapshots(template string, cfg Config, log *zap.Logger) (err error) {
	rootfs := filepath.Join(cfg.SnapshotDir, template, "rootfs.squashfs")
	var rootfsMod time.Time
	if fi, statErr := os.Stat(rootfs); statErr == nil {
		rootfsMod = fi.ModTime()
	}
	if !goldens.start(template, rootfsMod, time.Now()) {
		return errGoldenSkipped
	}
	refused := false
	defer func() { goldens.finish(template, err, refused, time.Now()) }()

	// Read the baked envd's level before booting anything: an envd that cannot
	// bind in a credential-less VM would cost a 60 s stall to learn the same.
	if level, lerr := bakedEnvdProtocol(rootfs); lerr != nil {
		log.Warn("golden_envd_level_unknown_booting_anyway",
			zap.String("template", template), zap.Error(lerr))
	} else if gerr := goldenEnvdCheck(level); gerr != nil {
		refused = true
		log.Warn("golden_snapshot_refused", zap.String("template", template), zap.Error(gerr))
		return gerr
	}

	log.Info("snapshot_rebuild_start", zap.String("template", template))

	for _, ver := range snapshotVersions {
		dir := snapshotDir(template, ver, cfg)
		_ = os.MkdirAll(dir, 0755)

		if err := createOneSnapshot(template, dir, cfg, log); err != nil {
			return fmt.Errorf("create snapshot %s/%s: %w", template, ver, err)
		}
		log.Info("snapshot_version_created",
			zap.String("template", template),
			zap.String("version", ver),
		)
	}
	return nil
}

func createOneSnapshot(template, snapDir string, cfg Config, log *zap.Logger) error {
	goldenID := "golden-" + template + "-" + randomHex(4)
	gvcpu, gmem := defaultResources(cfg, template)
	gstorage := goldenStorageMB(cfg, template)
	vm, err := ColdBoot(template, goldenID, cfg, log, gvcpu, gmem, gstorage, "", nil) // the golden VM holds no token and no tenant policy
	if err != nil {
		return fmt.Errorf("cold boot: %w", err)
	}
	defer func() {
		_ = vm.Machine.Stop(context.Background())
		_ = os.Remove(vm.OverlayPath)
		if vm.NetNS != "" {
			teardownNetNS(vm.NetNS)
		} else {
			TeardownVMNetwork(vm.TapName)
		}
	}()

	// Connect, not ConnectWithToken: the golden VM holds no credential; every
	// restore gets its own pushed after resume. A timeout here almost always means
	// the template's baked envd lacks the vsock credential channel.
	client, err := envd.Connect(vm.VsockPath, 60*time.Second)
	if err != nil {
		return fmt.Errorf("envd not ready in the golden VM for %s: %w — if this timed out, "+
			"check that this template's BAKED envd has the vsock credential channel "+
			"(`strings usr/local/bin/envd | grep SETHASH` inside the rootfs). An envd "+
			"predating Phase 4b §1 refuses to bind without a boot credential, which a "+
			"golden VM deliberately does not have", template, err)
	}
	client.Close()

	if err := vm.Machine.PauseVM(context.Background()); err != nil {
		return fmt.Errorf("pause VM: %w", err)
	}

	// The jailed VMM can only write inside its chroot, so snapshot to /state and
	// /mem there and move the files out.
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		return fmt.Errorf("mkdir snapshot dir: %w", err)
	}
	chrootRoot := filepath.Join(jailerChrootDir(), goldenID, "root")

	if err := vm.Machine.CreateSnapshot(context.Background(), "/state", "/mem"); err != nil {
		return fmt.Errorf("create snapshot: %w", err)
	}

	// Written by the golden VM's uid, restored by every other sandbox uid, so the
	// pair must be readable by all of them and owned by none.
	for _, name := range []string{"state", "mem"} {
		dst := filepath.Join(snapDir, name)
		if err := moveFile(filepath.Join(chrootRoot, name), dst); err != nil {
			return fmt.Errorf("move snapshot %s out of chroot: %w", name, err)
		}
		if err := sealGolden(dst); err != nil {
			return err
		}
	}

	// Keep the image the snapshot was taken with. The restored guest's memory
	// holds this filesystem's mounted state, so a fresh mkfs at the same block
	// addresses crashes the guest at an arbitrary later fault.
	if err := copySparse(vm.OverlayPath, filepath.Join(snapDir, goldenOverlayFile)); err != nil {
		return fmt.Errorf("preserve the golden overlay: %w", err)
	}

	// Written last so a half-written snapshot directory never looks valid.
	id := goldenIdentity{
		TapName:     vm.TapName,
		VMIP:        vm.VMIP,
		OverlayBase: filepath.Base(vm.OverlayPath),
		StorageMB:   gstorage,
	}
	if err := writeGoldenIdentity(snapDir, id); err != nil {
		return fmt.Errorf("record golden identity: %w", err)
	}
	ReserveGoldenTap(id.TapName)

	return nil
}

// goldenSealedMode is readable by every restoring VMM uid, writable by none.
const goldenSealedMode os.FileMode = 0o444

// sealGolden makes a golden file root-owned and read-only. Firecracker writes it
// as the golden VM's sandbox uid, which is allocatable: a later sandbox hashing
// to that uid would own the memory image every restore on the node resumes into.
func sealGolden(path string) error {
	if err := os.Chown(path, 0, 0); err != nil {
		return fmt.Errorf("seal golden %s: chown to root: %w", path, err)
	}
	if err := os.Chmod(path, goldenSealedMode); err != nil {
		return fmt.Errorf("seal golden %s: chmod %04o: %w", path, goldenSealedMode, err)
	}
	return nil
}

// sealGoldenDir seals state and mem; the overlay and golden.json are already root-only.
func sealGoldenDir(dir string) error {
	for _, name := range []string{"state", "mem"} {
		if err := sealGolden(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

// moveFile renames src to dst, falling back to copy+remove across filesystems.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyFile(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

// restoreVersion boots one sandbox from a golden snapshot. The guest comes back
// holding the golden tap name, address and MAC, so the tap is claimed under the
// golden name, renamed, and the guest rewritten over vsock before it is bridged.
func restoreVersion(template, sandboxID, snapDir string, cfg Config, log *zap.Logger, tokenHash string, rules *egress.Rules) (*RestoredVM, error) {
	golden, err := readGoldenIdentity(snapDir)
	if err != nil {
		return nil, fmt.Errorf("golden snapshot %s: %w", snapDir, err)
	}

	// In a private namespace the baked identity collides with nothing.
	if cfg.NetNS {
		return restoreVersionNetNS(template, sandboxID, snapDir, golden, cfg, log, tokenHash, rules)
	}

	// Same as the cold path: the shared bridge cannot hold a per-sandbox rule,
	// and a sandbox that reads as confined and is not is worse than a failed restore.
	if rules != nil {
		return nil, fmt.Errorf("this node runs sandboxes on the shared bridge, which cannot "+
			"enforce a per-sandbox egress policy; run with FC_NETNS=1 or create %s without one", sandboxID)
	}

	goldenIP := net.ParseIP(golden.VMIP)

	// Must match the uid restoreFirecrackerSnapshot derives, or the overlay chown
	// and the jailer uid disagree.
	uid := vmUID(cfg.NonRootVMM, sandboxID)

	// Allocated up front so the name is reserved before the golden name is claimed.
	vmIP, tapName := allocateVM()

	overlayPath, err := createRestoreOverlay(sandboxID, uid, snapDir, golden.OverlayBase, cfg)
	if err != nil {
		return nil, err
	}

	release := claimGoldenTap(golden.TapName)
	claimed := true
	releaseClaim := func() {
		if claimed {
			claimed = false
			release()
		}
	}
	defer releaseClaim()

	// Unbridged: the guest holds the golden identity until the rewrite below.
	if _, err := createTap(golden.TapName, uid); err != nil {
		deleteOverlay(overlayPath)
		return nil, fmt.Errorf("claim golden tap %s for %s: %w", golden.TapName, sandboxID, err)
	}

	machine, err := restoreFirecrackerSnapshot(FirecrackerConfig{
		SandboxID: sandboxID,
		Template:  template,
		// The golden values: Firecracker restores the net device from the snapshot.
		TapName:     golden.TapName,
		VMIP:        goldenIP,
		OverlayPath: overlayPath,
		StatePath:   filepath.Join(snapDir, "state"),
		MemPath:     filepath.Join(snapDir, "mem"),
		Config:      cfg,
	})
	if err != nil {
		TeardownVMNetwork(golden.TapName)
		deleteOverlay(overlayPath)
		return nil, err
	}

	failed := func(format string, a ...any) (*RestoredVM, error) {
		_ = machine.Stop(context.Background())
		TeardownVMNetwork(tapName)
		TeardownVMNetwork(golden.TapName)
		deleteOverlay(overlayPath)
		return nil, fmt.Errorf(format, a...)
	}
	tap, err := renameTap(golden.TapName, tapName)
	if err != nil {
		return failed("restore %s: %w", sandboxID, err)
	}
	releaseClaim()

	if err := deliverGuestNetworkFn(machine.ChrootRoot(), sandboxID, guestNetworkFor(vmIP), credPushDeadline); err != nil {
		return failed("restore %s: %w", sandboxID, err)
	}
	// Its own name, for the same reason it just got its own address. Every
	// sandbox warm-restored from a template's golden snapshot comes up calling
	// itself by the golden's hostname — measured on the reference node, an
	// ordinary create reported `hostname` as 192.168.30.2, the golden's address
	// — so this is not specific to snapshots, it is just first visible there.
	// Logged rather than fatal: a wrong hostname is visible and workable.
	if err := deliverGuestHostname(machine.ChrootRoot(), sandboxID); err != nil {
		log.Warn("guest_hostname_not_set", zap.String("sandbox_id", sandboxID), zap.Error(err))
	}
	// An empty hash is never pushed: envd would accept it and nothing could satisfy it.
	if tokenHash != "" {
		if err := deliverCredential(machine.ChrootRoot(), sandboxID, tokenHash, credPushDeadline); err != nil {
			return failed("restore %s: %w", sandboxID, err)
		}
	}
	guestMAC, err := net.ParseMAC(generateMAC(vmIP))
	if err != nil {
		return failed("restore %s: guest MAC for %s: %w", sandboxID, vmIP, err)
	}
	if err := attachTapToBridge(tap, guestMAC); err != nil {
		return failed("restore %s: %w", sandboxID, err)
	}

	log.Info("snapshot_restore_networked",
		zap.String("sandbox_id", sandboxID),
		zap.String("golden_tap", golden.TapName),
		zap.String("golden_ip", golden.VMIP),
		zap.String("tap", tapName),
		zap.String("vmip", vmIP.String()),
	)

	return &RestoredVM{
		Machine: machine,
		// The host reaches envd over TCP on the guest network.
		VsockPath:   vmIP.String() + ":6001",
		OverlayPath: overlayPath,
		TapName:     tapName,
		VMIP:        vmIP.String(),
		MemBase:     filepath.Join(snapDir, "mem"),
	}, nil
}

// restoreVersionNetNS restores inside a private namespace, where the golden's
// baked tap name, address and MAC are free, so no claim, rename or SETNET is needed.
func restoreVersionNetNS(template, sandboxID, snapDir string, golden goldenIdentity, cfg Config, log *zap.Logger, tokenHash string, rules *egress.Rules) (*RestoredVM, error) {
	uid := vmUID(cfg.NonRootVMM, sandboxID)

	overlayPath, err := createRestoreOverlay(sandboxID, uid, snapDir, golden.OverlayBase, cfg)
	if err != nil {
		return nil, err
	}

	ns, err := setupNetNS(sandboxID, uid, golden.TapName, net.ParseIP(golden.VMIP), rules, log)
	if err != nil {
		deleteOverlay(overlayPath)
		return nil, fmt.Errorf("netns: %w", err)
	}
	machine, err := restoreFirecrackerSnapshot(FirecrackerConfig{
		SandboxID:   sandboxID,
		Template:    template,
		TapName:     golden.TapName,
		VMIP:        net.ParseIP(golden.VMIP),
		OverlayPath: overlayPath,
		StatePath:   filepath.Join(snapDir, "state"),
		MemPath:     filepath.Join(snapDir, "mem"),
		NetNS:       ns.NetNSPath,
		Config:      cfg,
	})
	if err != nil {
		teardownNetNS(ns.Name)
		deleteOverlay(overlayPath)
		return nil, err
	}

	if tokenHash != "" {
		if err := deliverCredential(machine.ChrootRoot(), sandboxID, tokenHash, credPushDeadline); err != nil {
			_ = machine.Stop(context.Background())
			teardownNetNS(ns.Name)
			deleteOverlay(overlayPath)
			return nil, fmt.Errorf("restore %s: %w", sandboxID, err)
		}
	}

	// Its own name, for the same reason the bridge path pushes one: the golden's
	// hostname is baked into the restored memory, so without this every sandbox
	// warm-restored on a namespaced node calls itself by the golden's name.
	// After the credential, by which point envd has answered; logged rather
	// than fatal, like the bridge path's.
	if err := deliverGuestHostname(machine.ChrootRoot(), sandboxID); err != nil {
		log.Warn("guest_hostname_not_set", zap.String("sandbox_id", sandboxID), zap.Error(err))
	}

	log.Info("snapshot_restore_networked_netns",
		zap.String("sandbox_id", sandboxID),
		zap.String("tap", golden.TapName),
		zap.String("vmip", golden.VMIP),
		zap.String("netns", ns.Name),
		zap.String("node_addr", ns.NodeAddr),
	)

	return &RestoredVM{
		Machine:     machine,
		VsockPath:   ns.NodeAddr,
		OverlayPath: overlayPath,
		TapName:     ns.TapName,
		VMIP:        golden.VMIP,
		NetNS:       ns.Name,
		MemBase:     filepath.Join(snapDir, "mem"),
	}, nil
}

// restoreOverlaySubdir holds one directory per restored sandbox: the snapshot
// pins the overlay basename, so same-named files need separate directories.
const restoreOverlaySubdir = "restore"

// createRestoreOverlay gives the sandbox a private copy of the golden image under
// the basename the snapshot pins. A copy because the guest's restored memory
// holds this filesystem's state; private because the guest writes to it.
func createRestoreOverlay(sandboxID string, uid int, snapDir, goldenBase string, cfg Config) (string, error) {
	src := filepath.Join(snapDir, goldenOverlayFile)
	dir := filepath.Join(cfg.OverlayDir, restoreOverlaySubdir, sandboxID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create overlay dir for %s: %w", sandboxID, err)
	}
	dst := filepath.Join(dir, goldenBase)
	if err := copySparse(src, dst); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("copy the golden image for %s: %w", sandboxID, err)
	}
	if err := os.Chown(dst, uid, uid); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("chown overlay to %d: %w", uid, err)
	}
	if err := os.Chmod(dst, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("chmod overlay: %w", err)
	}
	return dst, nil
}

func snapshotDir(template, version string, cfg Config) string {
	return filepath.Join(cfg.SnapshotDir, template, "snapshots", version)
}

// snapshotValid reports whether dir holds a restorable snapshot. golden.json and
// the golden image are required: a restore that guesses the baked identity or
// boots on a fresh filesystem is quietly wrong rather than failing.
func snapshotValid(dir string) bool {
	for _, name := range []string{"state", "mem"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Size() == 0 {
			return false
		}
	}
	if info, err := os.Stat(filepath.Join(dir, goldenOverlayFile)); err != nil || info.Size() == 0 {
		return false
	}
	_, err := readGoldenIdentity(dir)
	return err == nil
}

// bakedEnvdProtocol reads the envd level out of a template's rootfs without
// booting it. unsquashfs ships in squashfs-tools, which provision-node.sh installs.
func bakedEnvdProtocol(rootfs string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "unsquashfs", "-cat", rootfs, "usr/local/bin/envd").Output()
	if err != nil {
		return 0, fmt.Errorf("read usr/local/bin/envd from %s: %w", rootfs, err)
	}
	return envdproto.Scan(out), nil
}

// goldenSized reports whether the golden in dir was built at the size a default
// create of template now asks for (see goldenStorageMB).
func goldenSized(dir string, cfg Config, template string) bool {
	g, err := readGoldenIdentity(dir)
	return err == nil && g.StorageMB == goldenStorageMB(cfg, template)
}
