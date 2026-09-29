//go:build linux

package vm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// PATH is emptied so LookPath fails for both nft and nsenter; the error must
// name the first missing binary.
func TestCheckHostDepsRefusesNetNSWithoutNft(t *testing.T) {
	jailer := writeFakeJailer(t)
	t.Setenv("PATH", "")
	err := checkHostDeps(Config{NetNS: true, JailerBin: jailer})
	if err == nil {
		t.Fatal("checkHostDeps accepted a netns config with no nft binary on PATH.\n\n" +
			"With this nil the node inits Firecracker far enough for SelectRuntime to " +
			"probe past it — or fails later inside EnsureNetNSHostRules with an exec " +
			"error nothing maps to a package name — and the pre-fix behaviour (serve " +
			"tenants on a shared kernel because a firewall tool was missing) comes back.")
	}
	if !strings.Contains(err.Error(), "nft") {
		t.Fatalf("the refusal must name the missing binary; got: %v", err)
	}
}

func TestCheckHostDepsRefusesNetNSWithoutNsenter(t *testing.T) {
	jailer := writeFakeJailer(t)
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "nft"))
	t.Setenv("PATH", dir)
	err := checkHostDeps(Config{NetNS: true, JailerBin: jailer})
	if err == nil {
		t.Fatal("checkHostDeps accepted a netns config with nft present but nsenter missing — " +
			"the per-slot ruleset would be built and then silently never applied inside the namespace")
	}
	if !strings.Contains(err.Error(), "nsenter") {
		t.Fatalf("the refusal must name nsenter; got: %v", err)
	}
}

// The jailer is required in every firecracker mode, not only netns.
func TestCheckHostDepsRefusesMissingJailer(t *testing.T) {
	err := checkHostDeps(Config{JailerBin: filepath.Join(t.TempDir(), "no-such-jailer")})
	if err == nil {
		t.Fatal("checkHostDeps accepted a config whose jailer binary does not exist")
	}
	if !strings.Contains(err.Error(), "jailer") {
		t.Fatalf("the refusal must name the jailer; got: %v", err)
	}
}

func TestCheckHostDepsPassesWhenProvisioned(t *testing.T) {
	jailer := writeFakeJailer(t)
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "nft"))
	writeExecutable(t, filepath.Join(dir, "nsenter"))
	t.Setenv("PATH", dir)
	for _, netns := range []bool{false, true} {
		if err := checkHostDeps(Config{NetNS: netns, JailerBin: jailer}); err != nil {
			t.Errorf("NetNS=%v: checkHostDeps refused a fully provisioned host: %v", netns, err)
		}
	}
}

func writeFakeJailer(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "jailer")
	writeExecutable(t, p)
	return p
}

func writeExecutable(t *testing.T, p string) {
	t.Helper()
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}
