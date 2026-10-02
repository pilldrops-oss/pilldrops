package docker

import (
	"cmp"
	"os"
	"strings"
)

// dockerdPIDFile is the dockerd pidfile its start time is read from.
func dockerdPIDFile() string {
	return cmp.Or(strings.TrimSpace(os.Getenv("SANDBOXD_DOCKERD_PIDFILE")), "/var/run/docker.pid")
}

// remoteDockerHost reports a DOCKER_HOST that is not a local socket: that
// daemon's pidfile, if one exists here, belongs to some other dockerd.
func remoteDockerHost() bool {
	h := os.Getenv("DOCKER_HOST")
	return h != "" && !strings.HasPrefix(h, "unix://")
}
