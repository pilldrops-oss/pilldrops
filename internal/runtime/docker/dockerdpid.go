package docker

import (
	"os"
	"strings"
)

// dockerdPIDFile is the dockerd pidfile its start time is read from, and
// whether it was configured.
func dockerdPIDFile() (string, bool) {
	if p := strings.TrimSpace(os.Getenv("SANDBOXD_DOCKERD_PIDFILE")); p != "" {
		return p, true
	}
	return "/var/run/docker.pid", false
}

// remoteDockerd reports a daemon the CLI reaches other than by a local socket,
// through DOCKER_HOST or a docker context: a pidfile here, if any, belongs to
// some other dockerd.
func remoteDockerd() bool {
	host, ok := dockerEndpoint()
	return !ok || (host != "" && !strings.HasPrefix(host, "unix://"))
}
