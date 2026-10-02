package docker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDockerdPIDFileHonoursTheSetting(t *testing.T) {
	t.Setenv("SANDBOXD_DOCKERD_PIDFILE", "")
	if got, set := dockerdPIDFile(); got != "/var/run/docker.pid" || set {
		t.Errorf("default pidfile = %q, configured %v", got, set)
	}
	t.Setenv("SANDBOXD_DOCKERD_PIDFILE", "/run/user/1000/docker.pid")
	if got, set := dockerdPIDFile(); got != "/run/user/1000/docker.pid" || !set {
		t.Errorf("pidfile = %q, configured %v; want the configured one", got, set)
	}
}

// The daemon is the one the CLI reaches: DOCKER_HOST, else DOCKER_CONTEXT,
// else the current context. A remote context read the local pidfile.
func TestRemoteDockerdFollowsTheCLIsEndpoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeContext(t, home, "remote", "ssh://ops@builder")
	writeContext(t, home, "rootless", "unix:///run/user/1000/docker.sock")
	setCurrent := func(name string) {
		if err := os.WriteFile(filepath.Join(home, ".docker", "config.json"),
			[]byte(`{"currentContext":"`+name+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, host, context, current string
		remote                       bool
	}{
		{"default socket", "", "", "", false},
		{"local DOCKER_HOST", "unix:///run/user/1000/docker.sock", "", "", false},
		{"tcp DOCKER_HOST", "tcp://10.0.0.5:2376", "", "", true},
		{"ssh DOCKER_HOST", "ssh://ops@builder", "", "", true},
		{"remote current context", "", "", "remote", true},
		{"local current context", "", "", "rootless", false},
		{"remote DOCKER_CONTEXT over a local current one", "", "remote", "rootless", true},
		{"DOCKER_CONTEXT that cannot be read", "", "missing", "", true},
		{"DOCKER_HOST over a remote context", "unix:///var/run/docker.sock", "", "remote", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DOCKER_HOST", tc.host)
			t.Setenv("DOCKER_CONTEXT", tc.context)
			setCurrent(tc.current)
			if got := remoteDockerd(); got != tc.remote {
				t.Errorf("remote = %v, want %v", got, tc.remote)
			}
		})
	}
}
