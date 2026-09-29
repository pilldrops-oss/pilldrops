package docker

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// bootImage runs a container from img (a template base image on Boot, a
// committed snapshot on Restore). devices are host device paths to pass through.
func (r *Runtime) bootImage(ctx context.Context, spec rt.BootSpec, img string, devices []string) (rt.Instance, error) {
	if err := r.ensureSandboxNet(ctx); err != nil {
		return nil, err
	}
	for _, d := range devices {
		if _, err := os.Stat(d); err != nil {
			return nil, fmt.Errorf("template %q requires host device %s, which this node does not have (KVM-capable node needed)", spec.Template.Name, d)
		}
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
	// Service and device-bearing containers are platform-trusted and may not
	// work under it.
	if r.ociRuntime != "" && !service && len(devices) == 0 {
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
	if spec.Template.AgentID != "" {
		pubPorts = append(pubPorts, agentdPort)
	}
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
	agentDropped := spec.Template.AgentID != "" && fileExists(agentdHostPath())
	runUser := dropUser(service, devices, agentDropped, builtinRunUser(spec.Template.Name))
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
	for _, d := range devices {
		base = append(base, "--device", d)
		// /dev/kvm is 0660 root:kvm; without the group a non-root container
		// silently falls back to software emulation.
		if gid := deviceGroup(d); gid != "" {
			base = append(base, "--group-add", gid)
		}
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
	// Tokens stay on the node; only their hashes enter the container.
	var agentToken, agentHash string
	var envdToken string
	var cmd []string
	if !service {
		// Under Firecracker the guest init runs start.sh; here PID 1 must.
		const startIfPresent = "if [ -x /usr/local/bin/start.sh ]; then /usr/local/bin/start.sh >/tmp/start.log 2>&1 & fi; "

		// start.sh is deliberately not dropped: it is the template's own baked
		// boot script (desktop binds an X server), not tenant input.
		handover := privilegeHandover(runUser)

		agentCmd := ""
		if agentDropped {
			if ap := agentdHostPath(); fileExists(ap) {
				agentToken, agentHash = sbtoken.Mint()
				base = append(base, "-v", ap+":/usr/local/bin/agentd:ro")
				base = append(base, "-e", "ENVD_TOKEN_HASH="+agentHash)
				// PID 1 stays root to chown the workspace before dropping; setpriv
				// rather than su so no PAM or extra process sits between agentd
				// and its parent.
				agentCmd = "chown -R " + agentRunUser + ":" + agentRunUser + " " +
					containerWorkdir + " " + agentStateDir + " 2>/dev/null; " +
					"setpriv --reuid=" + agentRunUser + " --regid=" + agentRunUser +
					" --init-groups --inh-caps=-all " +
					"env HOME=" + agentRunHome + " " +
					"/usr/local/bin/agentd -template " + spec.Template.AgentID +
					" -work-dir " + containerWorkdir +
					" -state-dir " + agentStateDir +
					" >/tmp/agentd.log 2>&1 & "
			}
		}
		if cp := cliHostPath(); fileExists(cp) {
			base = append(base, "-v", cp+":/usr/local/bin/pilldrops:ro")
		}
		if ep := envdHostPath(); fileExists(ep) {
			base = append(base, "-v", ep+":/usr/local/bin/envd:ro")
			if agentHash != "" {
				// One hash covers agentd and envd; a second -e ENVD_TOKEN_HASH
				// would win and silently discard the first.
				envdToken = agentToken
			} else {
				// envd refuses its TCP bind without a hash, and TCP is its only
				// bind under Docker.
				var envdHash string
				envdToken, envdHash = sbtoken.Mint()
				base = append(base, "-e", "ENVD_TOKEN_HASH="+envdHash)
			}
			// envd drops too: its terminal is a tenant surface. The log fd is
			// opened by root before the drop.
			cmd = []string{"sh", "-c", handover + startIfPresent + agentCmd +
				dropPrefix(runUser) + "/usr/local/bin/envd >/tmp/envd.log 2>&1 & exec sleep infinity"}
		} else {
			cmd = []string{"sh", "-c", handover + startIfPresent + agentCmd + "exec sleep infinity"}
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
	if err := r.injectHarness(ctx, cid, spec.Template.Name); err != nil {
		_, _, _, _ = dockerOut(ctx, "rm", "-f", cid)
		return nil, err
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
	// Terminal and agent ports are best-effort; the sandbox boots without them.
	termHost, _ := mappedPort(ctx, cid, terminalPort)
	var agentHost string
	if agentToken != "" {
		agentHost, _ = mappedPort(ctx, cid, agentdPort)
	}
	return &dockerInstance{
		id: spec.SandboxID, cid: cid, host: host, termHost: termHost,
		agentHost: agentHost, agentToken: agentToken, envdToken: envdToken,
		vcpu: spec.VCPU, memMiB: spec.MemMiB, service: service, runUser: runUser,
		start: time.Now(),
		eng:   r.eng,
		ports: map[int]string{primary: host},
	}, nil
}

// deviceGroup is the numeric gid owning a host device, or "" for root-owned or
// unreadable. Numeric because --group-add resolves names against the
// container's /etc/group.
func deviceGroup(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Gid == 0 {
		return ""
	}
	return strconv.FormatUint(uint64(st.Gid), 10)
}

// injectHarness copies the embedded test-session harness into a harness-template
// container and starts it; a no-op for every other template.
func (r *Runtime) injectHarness(ctx context.Context, cid, templateName string) error {
	bs, ok := builtinServices[canonicalTemplate(templateName)]
	if !ok || !bs.harness {
		return nil
	}
	tmp, err := os.MkdirTemp("", "pd-harness-")
	if err != nil {
		return fmt.Errorf("harness: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	sub, err := fs.Sub(sessionHarness, "session_harness")
	if err != nil {
		return fmt.Errorf("harness: %w", err)
	}
	err = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return werr
		}
		data, rerr := fs.ReadFile(sub, p)
		if rerr != nil {
			return rerr
		}
		dst := filepath.Join(tmp, p)
		if merr := os.MkdirAll(filepath.Dir(dst), 0o755); merr != nil {
			return merr
		}
		return os.WriteFile(dst, data, 0o755)
	})
	if err != nil {
		return fmt.Errorf("harness: %w", err)
	}
	// Numeric uid 0: the emulator image strips root from /etc/passwd.
	if _, se, code, err := dockerOut(ctx, "exec", "-u", "0", cid, "sh", "-c",
		"mkdir -p /opt/pd /test && chmod 0777 /test"); err != nil || code != 0 {
		return dockerErr("harness setup", err, code, se)
	}
	if _, se, code, err := dockerOut(ctx, "cp", tmp+"/.", cid+":/opt/pd"); err != nil || code != 0 {
		return dockerErr("harness cp", err, code, se)
	}
	if _, se, code, err := dockerOut(ctx, "exec", "-u", "0", cid, "sh", "-c",
		"chmod -R a+rX /opt/pd"); err != nil || code != 0 {
		return dockerErr("harness perms", err, code, se)
	}
	if _, se, code, err := dockerOut(ctx, "exec", "-d", cid, "bash", "/opt/pd/harness.sh"); err != nil || code != 0 {
		return dockerErr("harness start", err, code, se)
	}
	return nil
}

// hostHasIPv6 reports whether this kernel has an IPv6 stack. Not cached: the
// answer can change under the process.
func hostHasIPv6() bool {
	_, err := os.Stat("/proc/net/if_inet6")
	return err == nil
}
