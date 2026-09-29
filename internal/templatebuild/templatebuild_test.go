package templatebuild

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitScriptStartsEnvd(t *testing.T) {
	b, err := initScript()
	if err != nil {
		t.Fatalf("initScript: %v", err)
	}
	init := string(b)
	for _, want := range []string{
		"/usr/local/bin/envd &",
		"/etc/sandbox-env",
		"/etc/resolv.conf",
		"/etc/hosts",
		"exec sleep infinity",
	} {
		if !strings.Contains(init, want) {
			t.Errorf("/sbin/init does not contain %q", want)
		}
	}
	// A leftover placeholder would be written into the guest's PID 1 verbatim,
	// and the in-tree-only start.sh line assumes a baked script this build has not.
	if strings.Contains(init, "agentd") || strings.Contains(init, "__") ||
		strings.Contains(init, "/usr/local/bin/start.sh") {
		t.Errorf("/sbin/init kept a line only the in-tree build wants:\n%s", init)
	}
}

func TestConvertRefusesWithoutEnvd(t *testing.T) {
	_, err := Convert(context.Background(), Config{
		Tag:      "irrelevant:latest",
		Out:      filepath.Join(t.TempDir(), "rootfs.squashfs"),
		EnvdPath: filepath.Join(t.TempDir(), "no-such-envd"),
	})
	if !errors.Is(err, ErrNoEnvd) {
		t.Fatalf("want ErrNoEnvd, got %v", err)
	}
	if !strings.Contains(err.Error(), "PD_ENVD_PATH") {
		t.Errorf("the refusal must name the setting an operator would change; got %q", err)
	}
	// Refused before docker create, so no export is spent on a doomed build.
	if strings.Contains(err.Error(), "docker") {
		t.Errorf("the envd check must run before anything touches docker; got %q", err)
	}
}

// The CLI is boot machinery too: a template without it boots, but code inside
// cannot call the platform. Refused before docker, naming the setting.
func TestConvertRefusesWithoutCLI(t *testing.T) {
	envd := filepath.Join(t.TempDir(), "envd")
	if err := os.WriteFile(envd, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Convert(context.Background(), Config{
		Tag:      "irrelevant:latest",
		Out:      filepath.Join(t.TempDir(), "rootfs.squashfs"),
		EnvdPath: envd,
		CLIPath:  filepath.Join(t.TempDir(), "no-such-cli"),
	})
	if !errors.Is(err, ErrNoCLI) {
		t.Fatalf("want ErrNoCLI, got %v", err)
	}
	if !strings.Contains(err.Error(), "PD_CLI_PATH") || !strings.Contains(err.Error(), "no-such-cli") {
		t.Errorf("the refusal must name the setting and the path; got %q", err)
	}
	if strings.Contains(err.Error(), "docker") {
		t.Errorf("the CLI check must run before anything touches docker; got %q", err)
	}
}

func TestEnvdPathHonoursOverride(t *testing.T) {
	t.Setenv("PD_ENVD_PATH", "/custom/envd")
	if got := EnvdPath(); got != "/custom/envd" {
		t.Fatalf("PD_ENVD_PATH ignored: got %q", got)
	}
	t.Setenv("PD_ENVD_PATH", "")
	if got := EnvdPath(); got != DefaultEnvdPath {
		t.Fatalf("default: got %q want %q", got, DefaultEnvdPath)
	}
}

func TestPreflightNamesTheMissingPrerequisite(t *testing.T) {
	t.Setenv("PD_ENVD_PATH", filepath.Join(t.TempDir(), "absent"))
	err := Preflight()
	if err == nil {
		t.Skip("this machine has docker, mksquashfs and an envd at the default path")
	}
	switch {
	case errors.Is(err, ErrNoDocker):
		if !strings.Contains(err.Error(), "container engine") {
			t.Errorf("the docker refusal must say a KVM node still needs a container "+
				"engine; got %q", err)
		}
	case errors.Is(err, ErrNoMksquashfs):
		if !strings.Contains(err.Error(), "squashfs-tools") {
			t.Errorf("the mksquashfs refusal must name the package; got %q", err)
		}
	case errors.Is(err, ErrNoEnvd):
		if !strings.Contains(err.Error(), "boots and is dead") {
			t.Errorf("the envd refusal must say what a template without it does; got %q", err)
		}
	default:
		t.Fatalf("Preflight failed with an unclassified error: %v", err)
	}
}

// Builds one complete and one envd-less squashfs and asserts the read-back tells them apart.
func TestVerifyBootMachineryRejectsATemplateWithoutEnvd(t *testing.T) {
	if _, err := exec.LookPath("mksquashfs"); err != nil {
		t.Skip("mksquashfs not installed")
	}
	if _, err := exec.LookPath("unsquashfs"); err != nil {
		t.Skip("unsquashfs not installed")
	}
	build := func(t *testing.T, with []string) string {
		t.Helper()
		root := t.TempDir()
		for _, p := range with {
			full := filepath.Join(root, p)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte("x"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		out := filepath.Join(t.TempDir(), "rootfs.squashfs")
		if b, err := exec.Command("mksquashfs", root, out, "-noappend").CombinedOutput(); err != nil {
			t.Fatalf("mksquashfs: %v: %s", err, b)
		}
		return out
	}

	complete := build(t, bootMachinery)
	if err := verifyBootMachinery(context.Background(), complete, bootMachinery); err != nil {
		t.Fatalf("a complete rootfs was rejected: %v", err)
	}

	withoutEnvd := build(t, bootMachinery[1:])
	err := verifyBootMachinery(context.Background(), withoutEnvd, bootMachinery)
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("a rootfs with no envd was accepted (err=%v). mksquashfs exits 0 on it and "+
			"the resulting VM boots, so nothing else would have caught this", err)
	}
	if !strings.Contains(err.Error(), "/usr/local/bin/envd") {
		t.Errorf("the error must name what is missing; got %q", err)
	}
	// The message must say what the image would do, not only what is missing.
	for _, want := range []string{"BOOTS", "envd not ready"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must contain %q so the operator is not sent looking for "+
				"a boot failure; got %q", want, err)
		}
	}
}

func TestRealPathFollowsAUsrMergeSymlink(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, root, "usr/sbin")
	mustSymlink(t, "usr/sbin", filepath.Join(root, "sbin"))

	got, err := realPath(root, "sbin/init")
	if err != nil {
		t.Fatal(err)
	}
	if got != "usr/sbin/init" {
		t.Fatalf("realPath(sbin/init) = %q, want usr/sbin/init", got)
	}
}

// Host semantics here would let a build context redirect writes onto the node as root.
func TestRealPathTreatsAnAbsoluteSymlinkAsRootRelative(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, root, "usr/sbin")
	mustSymlink(t, "/usr/sbin", filepath.Join(root, "sbin"))

	got, err := realPath(root, "sbin/init")
	if err != nil {
		t.Fatal(err)
	}
	if got != "usr/sbin/init" {
		t.Fatalf("an absolute in-image symlink resolved to %q; it must stay inside the image "+
			"(host semantics would have written to the NODE's /usr/sbin as root)", got)
	}
	if err := writeInRoot(root, "sbin/init", []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "usr/sbin/init")); err != nil {
		t.Fatalf("the injected init is not inside the image: %v", err)
	}
}

func TestRealPathRefusesAnEscape(t *testing.T) {
	root := t.TempDir()
	mustSymlink(t, "../../../../etc", filepath.Join(root, "sbin"))

	if _, err := realPath(root, "sbin/init"); err == nil {
		t.Fatal("a symlink escaping the image root was followed")
	} else if !strings.Contains(err.Error(), "outside the image root") {
		t.Errorf("the refusal must say what happened; got %q", err)
	}
}

func TestRealPathRefusesASymlinkLoop(t *testing.T) {
	root := t.TempDir()
	mustSymlink(t, "sbin2", filepath.Join(root, "sbin"))
	mustSymlink(t, "sbin", filepath.Join(root, "sbin2"))

	if _, err := realPath(root, "sbin/init"); err == nil {
		t.Fatal("a symlink loop did not terminate with an error")
	}
}

func mustMkdir(t *testing.T, root, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, at string) {
	t.Helper()
	if err := os.Symlink(target, at); err != nil {
		t.Fatal(err)
	}
}
