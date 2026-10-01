package docker

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// bootImage runs a container from img (a template base image on Boot, a
// committed snapshot on Restore).
func (r *Runtime) bootImage(ctx context.Context, spec rt.BootSpec, img string) (rt.Instance, error) {
	if err := r.ensureSandboxNet(ctx); err != nil {
		return nil, err
	}
	if err := r.ensureImage(ctx, img); err != nil {
		return nil, err
	}
	// A service image runs its own entrypoint; a toolbox image is overridden
	// with `sleep infinity` and exec'd into.
	service := serviceTemplate(spec)
	base := []string{
		"run", "-d",
		"--label", "sandboxd=1",
		"--label", "sandboxd.id=" + spec.SandboxID,
		"--network", "sandboxd-net", // inter-container isolation via icc=false
		// Makes the run-user drop irreversible: no setuid binary can regain
		// root. Service entrypoints only drop privilege, so they are unaffected.
		"--security-opt", "no-new-privileges",
	}
	// Toolbox sandboxes run under the configured OCI runtime (gVisor) when set.
	// Service containers are platform-trusted and may not work under it.
	if r.ociRuntime != "" && !service {
		base = append(base, "--runtime", r.ociRuntime)
	}
	if !service {
		base = append(base, "-w", containerWorkdir)
		// Without PORT a template's start.sh binds its own default and the preview 503s.
		base = append(base, "-e", "PORT="+strconv.Itoa(appPort))
	}
	// De-duped: publishing the same container port twice races on the host
	// port allocation.
	pubPorts := append([]int{terminalPort}, spec.Template.Ports...)
	if wantsDesktop(spec.Template) {
		pubPorts = append(pubPorts, desktopPort)
	}
	// This runtime execs with `docker exec`, not through envd, so envd's own
	// session env never reaches an exec here and DISPLAY has to come from the
	// container. Measured: without it `exec 'xdotool ...'` on a sandbox with a
	// running desktop fails with "cannot open display".
	if wantsDesktop(spec.Template) {
		base = append(base, "-e", "DISPLAY=:0")
	}
	for _, p := range dedupePublishPorts(appPort, pubPorts) {
		base = append(base, "-p", "127.0.0.1:0:"+strconv.Itoa(p))
	}
	// The run user is decided once here and used by the launch command and the
	// instance's exec/file surface.
	runUser := dropUser(service)
	// Labelled on the container so Adopt can recover it without a BootSpec;
	// otherwise a restart silently returns the adopted sandbox to uid 0.
	base = append(base, "--label", "sandboxd.runuser="+runUser)
	// Before the template's own env so an explicit value still wins.
	for _, e := range runUserEnv(runUser) {
		base = append(base, "-e", e)
	}
	for _, e := range spec.Template.Env {
		base = append(base, "-e", e)
	}
	// Unconditional: a carve-out keyed on anything a request can influence
	// makes the hardening opt-out.
	base = append(base, "--cap-drop", "ALL")
	for _, c := range sandboxCaps {
		base = append(base, "--cap-add", c)
	}
	// Services run the image's own entrypoint, so the drop must apply to the
	// container. Plain templates keep PID 1 root to hand over the workspace first.
	if service && runUser != "" {
		base = append(base, "--user", runUser)
	}
	if spec.VCPU > 0 {
		base = append(base, "--cpus", strconv.Itoa(spec.VCPU))
	}
	if spec.MemMiB > 0 {
		base = append(base, "--memory", strconv.Itoa(spec.MemMiB)+"m")
	}
	// Always capped, even when the spec leaves them at 0.
	pidsLimit := spec.PidsLimit
	if pidsLimit <= 0 {
		pidsLimit = defaultPidsLimit
	}
	nofileLimit := spec.NofileLimit
	if nofileLimit <= 0 {
		nofileLimit = defaultNofileLimit
	}
	base = append(base, "--pids-limit", strconv.Itoa(pidsLimit))
	base = append(base, "--ulimit", fmt.Sprintf("nofile=%d:%d", nofileLimit, nofileLimit))
	// The egress rules are IPv4 only, so IPv6 is disabled in the container
	// rather than filtered. Gated because the sysctl does not exist on a
	// kernel without IPv6 and docker run would fail.
	if hostHasIPv6() {
		base = append(base, "--sysctl", "net.ipv6.conf.all.disable_ipv6=1")
	}
	// The token stays on the node; only its hash enters the container.
	var envdToken string
	var cmd []string
	if !service {
		// PID 1 is otherwise `sleep infinity`, which never reaps: a killed
		// background process stayed a zombie and still listed as running.
		base = append(base, "--init")
		// Under Firecracker the guest init runs start.sh; here PID 1 must.
		const startIfPresent = "if [ -x /usr/local/bin/start.sh ]; then /usr/local/bin/start.sh >/tmp/start.log 2>&1 & fi; "

		// start.sh is deliberately not dropped: it is the template's own baked
		// boot script (desktop binds an X server), not tenant input.
		handover := privilegeHandover(runUser)

		if cp := cliHostPath(); fileExists(cp) {
			base = append(base, "-v", cp+":/usr/local/bin/pilldrops:ro")
		}
		if ep := envdHostPath(); fileExists(ep) {
			base = append(base, "-v", ep+":/usr/local/bin/envd:ro")
			// envd refuses its TCP bind without a hash, and TCP is its only
			// bind under Docker.
			var envdHash string
			envdToken, envdHash = sbtoken.Mint()
			base = append(base, "-e", "ENVD_TOKEN_HASH="+envdHash)
			// envd drops too: its terminal is a tenant surface. The log fd is
			// opened by root before the drop.
			cmd = []string{"sh", "-c", handover + startIfPresent +
				dropPrefix(runUser) + "/usr/local/bin/envd >/tmp/envd.log 2>&1 & exec sleep infinity"}
		} else {
			cmd = []string{"sh", "-c", handover + startIfPresent + "exec sleep infinity"}
		}
	}
	// Some storage drivers reject --storage-opt size; retried without it below.
	runArgs := append([]string{}, base...)
	if spec.StorageMB > 0 {
		runArgs = append(runArgs, "--storage-opt", "size="+strconv.Itoa(spec.StorageMB)+"m")
	}
	runArgs = append(append(runArgs, img), cmd...)

	out, se, code, err := dockerOut(ctx, runArgs...)
	if (err != nil || code != 0) && spec.StorageMB > 0 && strings.Contains(strings.ToLower(se), "storage-opt") {
		if productionNode() {
			return nil, fmt.Errorf("docker run %s: storage driver rejects --storage-opt "+
				"(disk cap %dMB unenforceable) — refusing to boot uncapped on a "+
				"production node (SANDBOXD_PRODUCTION=1): %s", img, spec.StorageMB, strings.TrimSpace(se))
		}
		fb := append(append([]string{}, base...), img)
		fb = append(fb, cmd...)
		out, se, code, err = dockerOut(ctx, fb...)
		if err == nil && code == 0 {
			noteStorageCapUnenforced(spec.StorageMB)
		}
	}
	// Docker can lose the race for an ephemeral host port under concurrent
	// boots; each retry picks fresh ports.
	for attempt := 0; (err != nil || code != 0) && isPortBindConflict(se) && attempt < portBindRetries; attempt++ {
		out, se, code, err = dockerOut(ctx, runArgs...)
	}
	if err != nil || code != 0 {
		return nil, dockerErr("docker run "+img, err, code, se)
	}
	cid := strings.TrimSpace(out)

	primary := appPort
	if service && len(spec.Template.Ports) > 0 {
		primary = spec.Template.Ports[0]
	}
	// Before the container is handed out: a sandbox is never returned without
	// the identity its row promises. Zero on a resume, which delivers nothing.
	if spec.Identity.Token != "" {
		if err := deliverIdentity(ctx, cid, runUser, spec.Identity); err != nil {
			_, _, _, _ = dockerOut(ctx, "rm", "-f", cid)
			return nil, fmt.Errorf("sandbox %s: could not deliver the sandbox identity: %w", spec.SandboxID, err)
		}
	}
	host, err := mappedPort(ctx, cid, primary)
	if err != nil {
		_, _, _, _ = dockerOut(ctx, "rm", "-f", cid)
		return nil, err
	}
	// The terminal port is best-effort; the sandbox boots without it.
	termHost, _ := mappedPort(ctx, cid, terminalPort)
	return &dockerInstance{
		id: spec.SandboxID, cid: cid, host: host, termHost: termHost,
		envdToken: envdToken,
		vcpu:      spec.VCPU, memMiB: spec.MemMiB, service: service, runUser: runUser,
		start: time.Now(),
		eng:   r.eng,
		ports: map[int]string{primary: host},
	}, nil
}

// hostHasIPv6 reports whether this kernel has an IPv6 stack. Not cached: the
// answer can change under the process.
func hostHasIPv6() bool {
	_, err := os.Stat("/proc/net/if_inet6")
	return err == nil
}
