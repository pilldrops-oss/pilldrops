package docker

import (
	"errors"
	"io/fs"
	"strings"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// FSKind ran on stderr before anything asked who failed, so the node being
// refused docker.sock reached the tenant as a 403 about their own path. Only a
// failure inside the container is the caller's.
func TestFSFailBlamesTheNodeForDockerFaults(t *testing.T) {
	sock := "permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock"
	for name, err := range map[string]error{
		"socket refused":         fsFail("mkdir x", nil, 1, sock),
		"daemon error":           fsFail("stat x", nil, 1, "Error response from daemon: container abc is not running"),
		"docker exec's own exit": fsFail("list x", nil, 126, "OCI runtime exec failed: permission denied"),
		"helper never started":   fsFail("remove x", errors.New("exec: \"docker\": executable file not found in $PATH"), 0, ""),
	} {
		for _, kind := range []error{fs.ErrPermission, fs.ErrExist, fs.ErrInvalid, rt.ErrPathNotFound} {
			if errors.Is(err, kind) {
				t.Errorf("%s: %v is tagged %v, so the caller hears their path is at fault", name, err, kind)
			}
		}
	}
	if err := fsFail("mkdir /etc/x", nil, 1, "mkdir: cannot create directory '/etc/x': Permission denied"); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("an in-container permission failure = %v, want fs.ErrPermission", err)
	}
}

// The node-fault markers matched anywhere in stderr, so a tenant's own path
// named docker.sock turned their permission error into the node's 503.
func TestFSFailReadsATenantPathNamedLikeTheSocketAsTheirs(t *testing.T) {
	for _, se := range []string{
		"mkdir: cannot create directory '/root/docker.sock/x': Permission denied",
		"rm: cannot remove '/root/Error response from daemon': Permission denied",
		"stat: cannot stat '/root/docker daemon': Permission denied",
	} {
		if err := fsFail("op", nil, 1, se); !errors.Is(err, fs.ErrPermission) {
			t.Errorf("%q = %v, want fs.ErrPermission", se, err)
		}
	}
}

// An image without the helper is not the node failing; it says what is missing.
// Still untagged, so still a 503: the caller's path is not at fault either.
func TestFSFailNamesTheToolTheImageLacks(t *testing.T) {
	for se, tool := range map[string]string{
		`OCI runtime exec failed: exec failed: unable to start container process: exec: "stat": executable file not found in $PATH: unknown`: "stat",
		"sh: 1: find: not found": "find",
		"":                       "a tool this file operation needs",
	} {
		err := fsFail("stat x", nil, 127, se)
		if !strings.Contains(err.Error(), "the sandbox image lacks "+tool) {
			t.Errorf("%q: %v, want it to say the image lacks %s", se, err, tool)
		}
		if strings.Contains(err.Error(), "node could not") {
			t.Errorf("%q: %v blames the node for the image", se, err)
		}
		for _, kind := range []error{fs.ErrPermission, fs.ErrExist, fs.ErrInvalid, rt.ErrPathNotFound} {
			if errors.Is(err, kind) {
				t.Errorf("%q: tagged %v, which would not be a 503", se, kind)
			}
		}
	}
}
