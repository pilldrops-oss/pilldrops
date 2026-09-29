package docker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// identityDir and the two files mirror cmd/envd/identity.go: envd layers
// <dir>/env into every session on both runtimes, so the guest-visible result
// is the same whether the files arrived over vsock or by docker cp.
const identityDir = "/run/pilldrops"

// identityEnvSource sources the identity env. This runtime runs commands with
// `docker exec`, not through envd's session env, so the prologue goes on every
// exec and start command; the same line is the profile drop-in login shells
// read. Written once here because the two had drifted apart before only in
// their trailing character. ponytail: sourcing after docker's -e means a
// request env cannot override these three names.
const identityEnvSource = "[ -r " + identityDir + "/env ] && { set -a; . " + identityDir + "/env; set +a; }"

// identityEnvPrefix is identityEnvSource as a command prologue.
const identityEnvPrefix = identityEnvSource + "; "

// identityEnvFile renders the same KEY=VALUE text envd writes for a SETIDENTITY push.
func identityEnvFile(id rt.Identity) string {
	return "PILLDROPS_API_URL=" + id.APIURL + "\n" +
		"PD_SANDBOX_ID=" + id.SandboxID + "\n" +
		"PD_SANDBOX_DOMAIN=" + id.Domain + "\n"
}

// deliverIdentity copies the sandbox's own credential into a just-started
// container, before Boot returns and before any tenant code has run. Docker
// has no vsock channel by design (a TCP twin would be an authenticated surface
// on the container's own loopback), so the node writes the files itself. The
// profile drop-in is delivered too: envd runs unprivileged here and cannot
// write /etc/profile.d, so login shells would otherwise miss the identity.
func deliverIdentity(ctx context.Context, cid, runUser string, id rt.Identity) error {
	tmp, err := os.MkdirTemp("", "pd-ident-")
	if err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	files := map[string]struct {
		body string
		mode os.FileMode
	}{
		"token":   {id.Token + "\n", 0o600},
		"env":     {identityEnvFile(id), 0o644},
		"profile": {identityEnvSource + "\n", 0o644},
	}
	for name, f := range files {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte(f.body), f.mode); err != nil {
			return fmt.Errorf("identity: %w", err)
		}
	}
	owner := "0"
	if runUser != "" {
		owner = userPart(runUser)
	}
	if _, se, code, err := dockerOut(ctx, "exec", "-u", "0", cid, "sh", "-c",
		"mkdir -p "+identityDir+" /etc/profile.d"); err != nil || code != 0 {
		return dockerErr("identity mkdir", err, code, se)
	}
	for src, dst := range map[string]string{
		"token": identityDir + "/token", "env": identityDir + "/env",
		"profile": "/etc/profile.d/zz-pilldrops-identity.sh",
	} {
		if _, se, code, err := dockerOut(ctx, "cp", filepath.Join(tmp, src), cid+":"+dst); err != nil || code != 0 {
			return dockerErr("identity cp "+src, err, code, se)
		}
	}
	// docker cp lands files as root; the token must be readable by exactly the user envd runs as.
	if _, se, code, err := dockerOut(ctx, "exec", "-u", "0", cid, "sh", "-c",
		"chown -R "+owner+" "+identityDir+" && chmod 0750 "+identityDir+
			" && chmod 0600 "+identityDir+"/token && chmod 0644 "+identityDir+"/env /etc/profile.d/zz-pilldrops-identity.sh"); err != nil || code != 0 {
		return dockerErr("identity perms", err, code, se)
	}
	return nil
}
