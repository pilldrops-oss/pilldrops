package docker

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// fakeSocket creates a listening unix socket so the resolver's stat check passes.
// Not under t.TempDir(): sun_path is capped at ~104 bytes and macOS temp dirs exceed it.
func fakeSocket(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sk")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, name)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("cannot create a unix socket at %s: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return path
}

// writeContext lays out a docker context the way the CLI stores it.
func writeContext(t *testing.T, home, name, host string) {
	t.Helper()
	sum := sha256.Sum256([]byte(name))
	dir := filepath.Join(home, ".docker", "contexts", "meta", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"Name":"` + name + `","Endpoints":{"docker":{"Host":"` + host + `"}}}`
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveDockerSocketFollowsTheActiveContext(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")

	sock := fakeSocket(t, "orb.sock")
	writeContext(t, home, "orbstack", "unix://"+sock)
	if err := os.WriteFile(filepath.Join(home, ".docker", "config.json"),
		[]byte(`{"currentContext":"orbstack"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	got, ok := resolveDockerSocket()
	if !ok {
		t.Fatal("resolveDockerSocket found nothing; want the active context's socket")
	}
	if got != sock {
		t.Errorf("resolved %q, want the active context's %q — hardcoding /var/run/docker.sock is exactly the bug this guards", got, sock)
	}
}

func TestDockerContextEnvOverridesConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DOCKER_HOST", "")

	chosen := fakeSocket(t, "chosen.sock")
	other := fakeSocket(t, "other.sock")
	writeContext(t, home, "chosen", "unix://"+chosen)
	writeContext(t, home, "other", "unix://"+other)
	_ = os.WriteFile(filepath.Join(home, ".docker", "config.json"),
		[]byte(`{"currentContext":"other"}`), 0o644)

	t.Setenv("DOCKER_CONTEXT", "chosen")
	got, ok := resolveDockerSocket()
	if !ok || got != chosen {
		t.Errorf("resolved %q (ok=%v), want DOCKER_CONTEXT's %q", got, ok, chosen)
	}
}

func TestDockerHostWins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sock := fakeSocket(t, "explicit.sock")
	writeContext(t, home, "ctx", "unix://"+filepath.Join(home, "ignored.sock"))
	_ = os.WriteFile(filepath.Join(home, ".docker", "config.json"),
		[]byte(`{"currentContext":"ctx"}`), 0o644)

	t.Setenv("DOCKER_HOST", "unix://"+sock)
	got, ok := resolveDockerSocket()
	if !ok || got != sock {
		t.Errorf("resolved %q (ok=%v), want DOCKER_HOST's %q", got, ok, sock)
	}
}

// Remote endpoints need TLS and identity handling the CLI already gets right.
func TestNonUnixEndpointsFallBackToTheCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DOCKER_CONTEXT", "")
	for _, host := range []string{
		"tcp://10.0.0.5:2376",
		"ssh://user@build-host",
		"npipe:////./pipe/docker_engine",
	} {
		t.Setenv("DOCKER_HOST", host)
		if got, ok := resolveDockerSocket(); ok {
			t.Errorf("DOCKER_HOST=%s resolved to %q; want a refusal so the CLI is used", host, got)
		}
	}
}

func TestMissingSocketIsRefused(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	writeContext(t, home, "dead", "unix://"+filepath.Join(home, "nope.sock"))
	_ = os.WriteFile(filepath.Join(home, ".docker", "config.json"),
		[]byte(`{"currentContext":"dead"}`), 0o644)

	if got, ok := resolveDockerSocket(); ok {
		t.Errorf("resolved %q for a context whose socket does not exist; want a refusal", got)
	}
}

func TestEngineTalksToTheSameDaemonAsTheCLI(t *testing.T) {
	e := newEngine()
	if e == nil {
		t.Skip("no usable docker socket")
	}
	var info struct {
		ID string `json:"ID"`
	}
	if err := e.get(t.Context(), "/info", &info); err != nil {
		t.Fatalf("engine /info: %v", err)
	}
	out, _, code, err := dockerOut(t.Context(), "info", "--format", "{{.ID}}")
	if err != nil || code != 0 {
		t.Skip("docker CLI unavailable")
	}
	if cli := trimSpace(out); cli != info.ID {
		t.Errorf("engine daemon %q != CLI daemon %q — the engine is pointed at the wrong Docker", info.ID, cli)
	}
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}
