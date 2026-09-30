package docker

import (
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// sandboxRunUser is the unprivileged identity plain-template tenant code runs
// as. Numeric so no /etc/passwd entry is needed on any base image; 1001 rather
// than 1000 because 1000 is `node` in the official node images.
const (
	sandboxRunUID  = "1001"
	sandboxRunUser = sandboxRunUID + ":" + sandboxRunUID
	sandboxRunHome = containerWorkdir
)

// rootReason names why a sandbox's tenant surface still runs as uid 0, or
// returns "" when it drops. Only image-backed services stay root: their
// entrypoints (postgres initdb, nginx binding :80) must start as root and
// drop privilege themselves. A tenant reaches that class only via image+ports.
func rootReason(service bool) string {
	if service {
		return "image-backed service: its own entrypoint must start as root to initialise and then drop privilege itself"
	}
	return ""
}

// dropUser is the identity the tenant surface runs as; "" means uid 0.
func dropUser(service bool) string {
	if rootReason(service) != "" {
		return ""
	}
	return sandboxRunUser
}

// runUserEnv points each toolchain at a directory under $HOME so pip, npm,
// cargo and go work non-root. Chowning /usr/local instead would let the tenant
// edit start.sh, which root PID 1 runs again after a resume. PATH is not set
// here because a fixed value would drop the image's own; see execPathPrefix.
func runUserEnv(user string) []string {
	if user == "" {
		return nil
	}
	return []string{
		"HOME=" + sandboxRunHome,
		"NPM_CONFIG_PREFIX=" + sandboxRunHome + "/.npm-global",
		"CARGO_HOME=" + sandboxRunHome + "/.cargo",
		"GOPATH=" + sandboxRunHome + "/go",
		"GOCACHE=" + sandboxRunHome + "/.cache/go-build",
		"PIP_DISABLE_PIP_VERSION_CHECK=1",
	}
}

// execPathPrefix puts the $HOME pip and npm bin dirs on the image's own PATH.
func execPathPrefix(user string) string {
	if user == "" {
		return ""
	}
	return `export PATH="` + sandboxRunHome + `/.local/bin:` + sandboxRunHome + `/.npm-global/bin:$PATH"; `
}

// privilegeHandover is the shell prologue root PID 1 runs before dropping: it
// chowns the workspace and /root/app (the nextjs starter) to user and nothing
// else. Also fixes up root-owned workspaces in snapshots taken before the drop.
func privilegeHandover(user string) string {
	if user == "" {
		return ""
	}
	return "chown -R " + user + " " + containerWorkdir + " 2>/dev/null; " +
		"if [ -d /root/app ]; then chmod o+rx /root 2>/dev/null; chown -R " + user + " /root/app 2>/dev/null; fi; "
}

// dropPrefix wraps a command so it runs as user with no inheritable caps.
// setpriv, not su/sudo: those are setuid and no-new-privileges blocks them.
// --clear-groups because a numeric uid has no /etc/passwd entry to init groups from.
func dropPrefix(user string) string {
	if user == "" {
		return ""
	}
	return "setpriv --reuid=" + userPart(user) + " --regid=" + groupPart(user) +
		" --clear-groups --inh-caps=-all "
}

// userPart/groupPart split "<user>:<group>", tolerating the bare-name form.
func userPart(user string) string {
	for i := 0; i < len(user); i++ {
		if user[i] == ':' {
			return user[:i]
		}
	}
	return user
}

func groupPart(user string) string {
	for i := 0; i < len(user); i++ {
		if user[i] == ':' {
			return user[i+1:]
		}
	}
	return user
}

// serviceTemplate reports whether spec runs its own image entrypoint.
func serviceTemplate(spec rt.BootSpec) bool {
	return spec.Template.Image != "" && len(spec.Template.Ports) > 0
}
