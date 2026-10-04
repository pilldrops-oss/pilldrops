// Package templatebuild turns a local Docker image into a bootable Firecracker
// template rootfs, following scripts/build-template.sh. envd, /sbin/init and
// overlay-init must be injected or the template boots and never answers.
package templatebuild

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// assets is embedded because the node is a deployed binary with no repository
// checkout. These are the ONE copy of each: scripts/build-template.sh reads
// the same two files off disk rather than keeping its own, so a template built
// through the API boots exactly like one built in the tree.
//
//go:embed assets/overlay-init assets/init
var assets embed.FS

var (
	// ErrNoDocker means this node cannot produce a rootfs at all.
	ErrNoDocker = errors.New("templatebuild: docker is not available on this node")
	// ErrNoMksquashfs means squashfs-tools is not installed.
	ErrNoMksquashfs = errors.New("templatebuild: mksquashfs is not available on this node")
	// ErrNoEnvd means the node has no envd binary to inject.
	ErrNoEnvd = errors.New("templatebuild: no envd binary to inject into the template")
	// ErrIncomplete means the read-back found the squashfs missing boot machinery.
	ErrIncomplete = errors.New("templatebuild: the built rootfs is missing its boot machinery")
)

// DefaultEnvdPath is the node's envd binary, the same one the docker runtime bind-mounts.
const DefaultEnvdPath = "/opt/pilldrops/bin/envd"

// DefaultCLIPath is the node's static linux pilldrops CLI (make cli-linux),
// baked into every template so code inside a sandbox can drive the platform
// with its own identity. The docker runtime bind-mounts the same file.
const DefaultCLIPath = "/opt/pilldrops/bin/pilldrops-linux"

// ErrNoCLI means the node has no pilldrops CLI binary to inject.
var ErrNoCLI = errors.New("templatebuild: no pilldrops CLI binary to inject into the template")

// EnvdPath resolves the envd binary to inject; PD_ENVD_PATH overrides.
func EnvdPath() string {
	if p := strings.TrimSpace(os.Getenv("PD_ENVD_PATH")); p != "" {
		return p
	}
	return DefaultEnvdPath
}

// CLIPath resolves the CLI binary to inject; PD_CLI_PATH overrides.
func CLIPath() string {
	if p := strings.TrimSpace(os.Getenv("PD_CLI_PATH")); p != "" {
		return p
	}
	return DefaultCLIPath
}

// Preflight reports which prerequisite is missing before a long docker build starts.
func Preflight() error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("%w: a Firecracker template is built by exporting a container "+
			"image's filesystem, so the building node needs a container engine even though "+
			"it boots microVMs", ErrNoDocker)
	}
	if _, err := exec.LookPath("mksquashfs"); err != nil {
		return fmt.Errorf("%w: install squashfs-tools", ErrNoMksquashfs)
	}
	p := EnvdPath()
	if st, err := os.Stat(p); err != nil || st.IsDir() {
		return fmt.Errorf("%w: expected it at %s (set PD_ENVD_PATH). envd is the in-guest "+
			"agent that implements exec and the file APIs; a template built without it boots "+
			"and is dead", ErrNoEnvd, p)
	}
	if c := CLIPath(); !isFile(c) {
		return fmt.Errorf("%w: expected it at %s (set PD_CLI_PATH, or run `make cli-linux` and "+
			"install bin/pilldrops-linux there)", ErrNoCLI, c)
	}
	return nil
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// Config is one conversion of a locally built Docker image into a squashfs at Out.
type Config struct {
	// Tag is the image to convert. It must already be built locally.
	Tag string
	// Out is the destination, conventionally <image dir>/<id>/rootfs.squashfs.
	Out string
	// EnvdPath overrides EnvdPath().
	EnvdPath string
	// CLIPath overrides CLIPath().
	CLIPath string
	// WorkDir holds a full uncompressed copy of the image; defaults to the system temp dir.
	WorkDir string
	// SkipVerify disables the read-back assertion. Tests only.
	SkipVerify bool
}

// Result describes what was produced.
type Result struct {
	Path string
	Size int64
	// EnvdSHA256 identifies the injected envd. A hash because every envd build
	// reports the same version string.
	EnvdSHA256 string
	// Duration is wall clock for the conversion alone, excluding docker build.
	Duration time.Duration
}

const (
	exportTimeout = 15 * time.Minute
	squashTimeout = 20 * time.Minute
	quickTimeout  = 2 * time.Minute
)

// Convert exports Tag's filesystem, injects the boot machinery, and builds a
// squashfs at cfg.Out.
func Convert(ctx context.Context, cfg Config) (Result, error) {
	started := time.Now()
	var res Result
	if strings.TrimSpace(cfg.Tag) == "" {
		return res, errors.New("templatebuild: no image tag to convert")
	}
	if strings.TrimSpace(cfg.Out) == "" {
		return res, errors.New("templatebuild: no output path")
	}
	envd := cfg.EnvdPath
	if envd == "" {
		envd = EnvdPath()
	}
	// Re-checked here, not only in Preflight, so no caller can skip it.
	envdBytes, err := os.ReadFile(envd)
	if err != nil {
		return res, fmt.Errorf("%w: reading %s: %w (set PD_ENVD_PATH)", ErrNoEnvd, envd, err)
	}
	sum := sha256.Sum256(envdBytes)
	res.EnvdSHA256 = hex.EncodeToString(sum[:])
	cli := cfg.CLIPath
	if cli == "" {
		cli = CLIPath()
	}
	cliBytes, err := os.ReadFile(cli)
	if err != nil {
		return res, fmt.Errorf("%w: reading %s: %w (set PD_CLI_PATH)", ErrNoCLI, cli, err)
	}

	work, err := os.MkdirTemp(cfg.WorkDir, "sf-tmplbuild-")
	if err != nil {
		return res, fmt.Errorf("templatebuild: scratch directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()
	root := filepath.Join(work, "rootfs")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return res, err
	}

	if err := exportImageFS(ctx, cfg.Tag, root); err != nil {
		return res, err
	}

	if err := writeInRoot(root, "usr/local/bin/envd", envdBytes, 0o755); err != nil {
		return res, fmt.Errorf("templatebuild: injecting envd: %w", err)
	}
	if err := writeInRoot(root, "usr/local/bin/pilldrops", cliBytes, 0o755); err != nil {
		return res, fmt.Errorf("templatebuild: injecting the pilldrops CLI: %w", err)
	}

	overlayInit, err := assets.ReadFile("assets/overlay-init")
	if err != nil {
		return res, fmt.Errorf("templatebuild: reading the embedded overlay-init: %w", err)
	}
	if err := writeInRoot(root, "sbin/overlay-init", overlayInit, 0o755); err != nil {
		return res, fmt.Errorf("templatebuild: injecting overlay-init: %w", err)
	}

	// Docker applies Config.Env at run time; a microVM boots via /sbin/init, so
	// the image's ENV has to be baked in or PATH and friends are lost.
	env, err := imageEnv(ctx, cfg.Tag)
	if err != nil {
		return res, err
	}
	if err := writeInRoot(root, "etc/sandbox-env", env, 0o644); err != nil {
		return res, err
	}
	init, err := initScript()
	if err != nil {
		return res, err
	}
	if err := writeInRoot(root, "sbin/init", init, 0o755); err != nil {
		return res, fmt.Errorf("templatebuild: writing /sbin/init: %w", err)
	}

	// overlay-init's mountpoints, pre-created because the squashfs is read-only at boot.
	for _, d := range []string{"rom", "overlay", "newroot"} {
		real, err := realPath(root, d)
		if err != nil {
			return res, err
		}
		if err := os.MkdirAll(filepath.Join(root, real), 0o755); err != nil {
			return res, err
		}
	}

	// Where the boot machinery actually landed; usrmerge images ship /sbin as a
	// symlink to usr/sbin, so the literal path is not where the file is.
	placed := make([]string, 0, len(bootMachinery))
	for _, rel := range bootMachinery {
		real, err := realPath(root, rel)
		if err != nil {
			return res, err
		}
		placed = append(placed, real)
	}

	if err := os.MkdirAll(filepath.Dir(cfg.Out), 0o755); err != nil {
		return res, err
	}
	tmp := cfg.Out + ".tmp"
	_ = os.Remove(tmp)
	sctx, cancel := context.WithTimeout(ctx, squashTimeout)
	defer cancel()
	out, err := exec.CommandContext(sctx, "mksquashfs", root, tmp,
		"-noappend", "-comp", "zstd", "-Xcompression-level", "3").CombinedOutput()
	if err != nil {
		_ = os.Remove(tmp)
		if _, lerr := exec.LookPath("mksquashfs"); lerr != nil {
			return res, fmt.Errorf("%w: install squashfs-tools", ErrNoMksquashfs)
		}
		return res, fmt.Errorf("templatebuild: mksquashfs: %w: %s", err, tail(string(out), 2000))
	}

	// mksquashfs exits 0 on a tree missing every injected file.
	if !cfg.SkipVerify {
		if err := verifyBootMachinery(ctx, tmp, placed); err != nil {
			_ = os.Remove(tmp)
			return res, err
		}
	}

	// Rename, not write in place: the jailer hardlinks rootfs.squashfs into every
	// sandbox chroot, and truncating the inode rewrites the disk under running VMs.
	if err := os.Rename(tmp, cfg.Out); err != nil {
		_ = os.Remove(tmp)
		return res, fmt.Errorf("templatebuild: publishing the rootfs at %s: %w", cfg.Out, err)
	}
	st, err := os.Stat(cfg.Out)
	if err != nil {
		return res, err
	}
	res.Path, res.Size, res.Duration = cfg.Out, st.Size(), time.Since(started)
	return res, nil
}

// bootMachinery is what a template must contain beyond the image's filesystem.
var bootMachinery = []string{
	"usr/local/bin/envd",
	"usr/local/bin/pilldrops",
	"sbin/init",
	"sbin/overlay-init",
}

// verifyBootMachinery reads the finished squashfs back and asserts the injected
// files are present at the paths they resolved to.
func verifyBootMachinery(ctx context.Context, path string, placed []string) error {
	if _, err := exec.LookPath("unsquashfs"); err != nil {
		// A missing verifier is not a reason to refuse a build that works.
		return nil
	}
	vctx, cancel := context.WithTimeout(ctx, quickTimeout)
	defer cancel()
	out, err := exec.CommandContext(vctx, "unsquashfs", "-l", path).Output()
	if err != nil {
		return fmt.Errorf("templatebuild: reading back the built rootfs: %w", err)
	}
	have := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "squashfs-root/"); ok {
			have[p] = true
		}
	}
	var missing []string
	for i, want := range bootMachinery {
		at := want
		if i < len(placed) && placed[i] != "" {
			at = placed[i]
		}
		if !have[at] {
			if at != want {
				missing = append(missing, "/"+want+" (at /"+at+")")
			} else {
				missing = append(missing, "/"+want)
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s is absent. Such an image BOOTS — Firecracker starts it "+
			"and the kernel runs — and then every create from it fails at the envd readiness "+
			"gate with \"envd not ready after 10s\", against a build already marked ready. "+
			"Refusing to publish it", ErrIncomplete, strings.Join(missing, ", "))
	}
	return nil
}

// realPath resolves rel inside root with guest semantics; a missing component
// stops the walk. An absolute symlink in an attacker-supplied rootfs is relative
// to that rootfs, and host semantics would let it redirect writes onto the node as root.
func realPath(root, rel string) (string, error) {
	const maxHops = 32
	hops := 0
	cur := ""
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == "" || part == "." {
			continue
		}
		next := path.Join(cur, part)
		for {
			fi, err := os.Lstat(filepath.Join(root, next))
			if err != nil {
				if os.IsNotExist(err) {
					break
				}
				return "", fmt.Errorf("templatebuild: resolving /%s in the rootfs: %w", rel, err)
			}
			if fi.Mode()&os.ModeSymlink == 0 {
				break
			}
			hops++
			if hops > maxHops {
				return "", fmt.Errorf("templatebuild: /%s loops through more than %d symlinks "+
					"in this image", rel, maxHops)
			}
			target, err := os.Readlink(filepath.Join(root, next))
			if err != nil {
				return "", err
			}
			t := filepath.ToSlash(target)
			if strings.HasPrefix(t, "/") {
				next = path.Clean(strings.TrimPrefix(t, "/"))
			} else {
				next = path.Join(path.Dir(next), t)
			}
			if next == ".." || strings.HasPrefix(next, "../") || next == "." {
				return "", fmt.Errorf("templatebuild: /%s resolves outside the image root "+
					"through a symlink to %q; refusing to write there", rel, target)
			}
		}
		cur = next
	}
	if cur == "" {
		return "", fmt.Errorf("templatebuild: %q is not a path inside the image", rel)
	}
	return cur, nil
}

// writeInRoot writes b at rel inside root with guest path semantics. The file is
// removed first so a shipped symlink at the final component is not written through.
func writeInRoot(root, rel string, b []byte, mode os.FileMode) error {
	real, err := realPath(root, rel)
	if err != nil {
		return err
	}
	full := filepath.Join(root, real)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	_ = os.Remove(full)
	if err := os.WriteFile(full, b, mode); err != nil {
		return err
	}
	// WriteFile's mode is masked by umask on create; chmod is not.
	return os.Chmod(full, mode)
}

// exportImageFS pipes docker export straight into tar, avoiding a second full copy on disk.
func exportImageFS(ctx context.Context, tag, dst string) error {
	cctx, cancel := context.WithTimeout(ctx, quickTimeout)
	defer cancel()
	idOut, err := exec.CommandContext(cctx, "docker", "create", tag).Output()
	if err != nil {
		if _, lerr := exec.LookPath("docker"); lerr != nil {
			return fmt.Errorf("%w: cannot export %s", ErrNoDocker, tag)
		}
		return fmt.Errorf("templatebuild: docker create %s: %w", tag, err)
	}
	cid := strings.TrimSpace(string(idOut))
	if cid == "" {
		return fmt.Errorf("templatebuild: docker create %s produced no container id", tag)
	}
	// A leaked container pins the image's layers and blocks docker rmi.
	defer func() {
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), quickTimeout)
		defer rcancel()
		_ = exec.CommandContext(rctx, "docker", "rm", "-f", cid).Run()
	}()

	ectx, ecancel := context.WithTimeout(ctx, exportTimeout)
	defer ecancel()
	export := exec.CommandContext(ectx, "docker", "export", cid)
	pipe, err := export.StdoutPipe()
	if err != nil {
		return fmt.Errorf("templatebuild: docker export %s: %w", tag, err)
	}
	var exportErr bytes.Buffer
	export.Stderr = &exportErr

	// --numeric-owner: resolving names through the host's /etc/passwd would remap every uid.
	untar := exec.CommandContext(ectx, "tar", "-x", "--numeric-owner", "-C", dst)
	untar.Stdin = pipe
	var untarErr bytes.Buffer
	untar.Stderr = &untarErr

	if err := export.Start(); err != nil {
		return fmt.Errorf("templatebuild: docker export %s: %w", tag, err)
	}
	if err := untar.Start(); err != nil {
		_ = export.Process.Kill()
		_ = export.Wait()
		return fmt.Errorf("templatebuild: extracting %s: %w", tag, err)
	}
	untarWait := untar.Wait()
	exportWait := export.Wait()
	if exportWait != nil {
		return fmt.Errorf("templatebuild: docker export %s: %w: %s",
			tag, exportWait, tail(exportErr.String(), 1000))
	}
	if untarWait != nil {
		return fmt.Errorf("templatebuild: extracting %s: %w: %s",
			tag, untarWait, tail(untarErr.String(), 1000))
	}
	return nil
}

// imageEnv renders the image's Config.Env one per line, the shape /sbin/init sources.
func imageEnv(ctx context.Context, tag string) ([]byte, error) {
	ictx, cancel := context.WithTimeout(ctx, quickTimeout)
	defer cancel()
	out, err := exec.CommandContext(ictx, "docker", "image", "inspect", tag,
		"--format", "{{range .Config.Env}}{{println .}}{{end}}").Output()
	if err != nil {
		return nil, fmt.Errorf("templatebuild: reading %s's environment: %w", tag, err)
	}
	return out, nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// initScript is the shared assets/init with the in-tree build's extra line
// dropped: an image converted through the API carries no baked
// /usr/local/bin/start.sh, so the tagged line would only be noise in the
// guest's PID 1.
func initScript() ([]byte, error) {
	b, err := assets.ReadFile("assets/init")
	if err != nil {
		return nil, fmt.Errorf("templatebuild: reading the embedded init: %w", err)
	}
	var keep []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, "__INTREE_ONLY__") {
			continue
		}
		keep = append(keep, line)
	}
	return []byte(strings.Join(keep, "\n")), nil
}
