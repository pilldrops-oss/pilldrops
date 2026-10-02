package docker

import "testing"

func TestDockerdPIDFileHonoursTheSetting(t *testing.T) {
	t.Setenv("SANDBOXD_DOCKERD_PIDFILE", "")
	if got := dockerdPIDFile(); got != "/var/run/docker.pid" {
		t.Errorf("default pidfile = %q", got)
	}
	t.Setenv("SANDBOXD_DOCKERD_PIDFILE", "/run/user/1000/docker.pid")
	if got := dockerdPIDFile(); got != "/run/user/1000/docker.pid" {
		t.Errorf("pidfile = %q, want the configured one", got)
	}
}

func TestRemoteDockerHost(t *testing.T) {
	for host, remote := range map[string]bool{
		"":                                  false,
		"unix:///run/user/1000/docker.sock": false,
		"tcp://10.0.0.5:2376":               true,
		"ssh://ops@builder":                 true,
	} {
		t.Setenv("DOCKER_HOST", host)
		if got := remoteDockerHost(); got != remote {
			t.Errorf("DOCKER_HOST=%q: remote = %v, want %v", host, got, remote)
		}
	}
}
