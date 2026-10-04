package docker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// fakeDockerCLI puts a docker on PATH that answers from files in the returned
// dir and appends each call to dir/calls:
//
//	ps-<sandbox id>  `ps -a` lines for that sandbox ("<cid> <state>"); a
//	                 status=running filter keeps the running ones
//	all              every sandboxd container id, for the sweep
//	psfail           present = every ps fails, as before dockerd is up
//	label-<cid>      the container's sandboxd.id; absent = inspect fails
//	port-<cid>       the container's sandboxd.port label
//	pub-<cid>-<port> the host address docker published that port on
const fakeDocker = `#!/bin/sh
d=$FAKE_DOCKER_DIR
all="$*"
echo "$all" >> "$d/calls"
eval last=\${$#}
case "$1" in
ps)
	[ -f "$d/psfail" ] && { echo "Cannot connect to the Docker daemon" >&2; exit 1; }
	case "$*" in
	*label=sandboxd=1*) cat "$d/all" ;;
	*status=running*) id=${all##*label=sandboxd.id=}; grep ' running$' "$d/ps-${id%% *}" 2>/dev/null | cut -d' ' -f1 ;;
	*label=sandboxd.id=*) id=${all##*label=sandboxd.id=}; cat "$d/ps-${id%% *}" 2>/dev/null ;;
	esac ;;
inspect)
	case "$*" in
	*sandboxd.id*) [ -f "$d/label-$last" ] || { echo "Error: No such object: $last" >&2; exit 1; }; cat "$d/label-$last" ;;
	*sandboxd.runuser*) echo 1000 ;;
	*sandboxd.port*) cat "$d/port-$last" 2>/dev/null || echo "<no value>" ;;
	*State.Running*) echo "true false 0001-01-01T00:00:00Z" ;;
	*) echo "sh -c sleep infinity" ;;
	esac ;;
port) cat "$d/pub-$2-${3%/tcp}" 2>/dev/null || exit 1 ;;
esac
exit 0
`

func fakeDockerCLI(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(fakeDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DOCKER_DIR", dir)
	return dir
}

func dockerCalls(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// A host reboot stops every container. Adopt found only running ones, so the
// daemon dropped the record and the sweep removed the container with its data.
func TestAdoptFindsAStoppedContainer(t *testing.T) {
	fakeDockerCLI(t, map[string]string{
		"ps-sb-stopped": "c1 exited\n",
		"ps-sb-two":     "c2 exited\nc3 running\n",
		"ps-sb-standby": "c4 paused\n",
	})
	r := &Runtime{}
	for _, tc := range []struct {
		id, cid  string
		casualty bool
	}{
		{"sb-stopped", "c1", true},
		{"sb-two", "c3", false},
		{"sb-standby", "c4", false},
	} {
		inst, err := r.Adopt(t.Context(), tc.id)
		if err != nil {
			t.Errorf("%s: not adopted (%v); its container is there", tc.id, err)
			continue
		}
		di := inst.(*dockerInstance)
		if di.cid != tc.cid || di.Casualty() != tc.casualty {
			t.Errorf("%s: adopted %s casualty=%v, want %s casualty=%v", tc.id, di.cid, di.Casualty(), tc.cid, tc.casualty)
		}
	}
	if _, err := r.Adopt(t.Context(), "sb-none"); !errors.Is(err, rt.ErrNotFound) {
		t.Errorf("a sandbox with no container: err = %v, want rt.ErrNotFound", err)
	}
}

func TestSweepUnknownRemovesOnlyContainersWithNoRecord(t *testing.T) {
	dir := fakeDockerCLI(t, map[string]string{
		"all":            "c-known\nc-orphan\nc-unreadable\n",
		"label-c-known":  "sb-known\n",
		"label-c-orphan": "sb-gone\n",
	})
	(&Runtime{}).SweepUnknown([]string{"sb-known"})

	var removed []string
	for _, c := range dockerCalls(t, dir) {
		if strings.HasPrefix(c, "rm ") {
			removed = append(removed, c)
		}
	}
	if len(removed) != 1 || removed[0] != "rm -f c-orphan" {
		t.Errorf("removed %v, want only [rm -f c-orphan]", removed)
	}
}

// TestLiveAdoptRestartsAStoppedContainerWithItsData is the reboot on a real
// docker: the container is stopped under the sandbox, then adopted and started.
func TestLiveAdoptRestartsAStoppedContainerWithItsData(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Skipf("docker runtime unavailable: %v", err)
	}
	ctx := t.Context()
	id := liveSandboxID(t, "sb-reboot")
	inst, err := r.Boot(ctx, rt.BootSpec{SandboxID: id, Template: rt.Template{Name: "base", Image: "alpine:3"}, VCPU: 1, MemMiB: 256})
	if err != nil {
		t.Skipf("boot: %v", err)
	}
	if err := inst.FS().Write(ctx, "kept.txt", []byte("survives")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, code, err := dockerOut(ctx, "stop", "-t", "1", inst.(*dockerInstance).cid); err != nil || code != 0 {
		t.Fatalf("docker stop: %v %d", err, code)
	}

	adopted, err := r.Adopt(ctx, id)
	if err != nil {
		t.Fatalf("the stopped container was not adopted: %v", err)
	}
	rv := adopted.(rt.Reviver)
	if !rv.Casualty() {
		// Show what docker held for this sandbox, so a failure says which container was live.
		ps, _, _, _ := dockerOut(ctx, "ps", "-a", "--no-trunc", "--filter", "label=sandboxd.id="+id,
			"--format", "{{.ID}} {{.State}} {{.Status}} {{.CreatedAt}}")
		t.Fatalf("a stopped container adopted as live: stopped %s, adopted %s, docker ps:\n%s",
			inst.(*dockerInstance).cid, adopted.(*dockerInstance).cid, ps)
	}
	revived, err := rv.Revive(ctx)
	if err != nil {
		t.Fatalf("revive: %v", err)
	}
	got, err := revived.FS().Read(ctx, "kept.txt")
	if err != nil || string(got) != "survives" {
		t.Fatalf("after restart: %q, %v; want the file written before the stop", got, err)
	}
}

// A service template's preview is its first declared port. Adopt and Revive
// re-read appPort only, which a service does not publish: no preview.
func TestAdoptAndReviveKeepAServicesPrimaryPort(t *testing.T) {
	defer func(b time.Duration) { reviveBackoff = b }(reviveBackoff)
	reviveBackoff = time.Millisecond
	fakeDockerCLI(t, map[string]string{
		"ps-sb-svc":   "c1 exited\n",
		"port-c1":     "3000\n",
		"pub-c1-3000": "127.0.0.1:41000\n",
	})
	inst, err := (&Runtime{}).Adopt(t.Context(), "sb-svc")
	if err != nil {
		t.Fatalf("not adopted: %v", err)
	}
	revived, err := inst.(rt.Reviver).Revive(t.Context())
	if err != nil {
		t.Fatalf("revive: %v", err)
	}
	for name, i := range map[string]rt.Instance{"adopted": inst, "revived": revived} {
		got, err := i.(*dockerInstance).ListenTarget(3000)
		if err != nil || got != "127.0.0.1:41000" {
			t.Errorf("%s: port 3000 -> %q, %v; want 127.0.0.1:41000", name, got, err)
		}
	}
}

// The other half: bootImage writes the label Adopt reads.
func TestBootLabelsThePrimaryPort(t *testing.T) {
	if !strings.Contains(sourceContaining(t, "func (r *Runtime) bootImage("), `"--label", primaryPortLabel+"="+strconv.Itoa(primary)`) {
		t.Error("bootImage does not label the container with its primary port; Adopt falls back to appPort")
	}
}

// A ps that fails is not "no container": restore dropped the record on it and
// the next sweep removed the container.
func TestAdoptReportsAFailedListingAsAnError(t *testing.T) {
	fakeDockerCLI(t, map[string]string{"ps-sb-1": "c1 running\n", "psfail": ""})
	_, err := (&Runtime{}).Adopt(t.Context(), "sb-1")
	if err == nil || errors.Is(err, rt.ErrNotFound) {
		t.Fatalf("err = %v, want a listing error that is not rt.ErrNotFound", err)
	}
}
