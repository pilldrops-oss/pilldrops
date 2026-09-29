package httpapi

import (
	"errors"
	"path"
	"regexp"
	"strings"
)

// The repo URL and ref are interpolated into a shell command inside the
// sandbox, so these character classes admit no shell metacharacter at all.
// Widening them without re-reading buildCloneCmd is a command-injection bug.
var (
	// https://host/owner/repo(.git)
	repoHTTPSRe = regexp.MustCompile(`^https://[A-Za-z0-9._-]+(:[0-9]{1,5})?/[A-Za-z0-9._/-]+$`)
	// git@host:owner/repo(.git), the scp-like SSH form
	repoSCPRe = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+:[A-Za-z0-9._/-]+$`)
	// ssh://git@host/owner/repo(.git)
	repoSSHRe = regexp.MustCompile(`^ssh://[A-Za-z0-9._-]+@[A-Za-z0-9._-]+(:[0-9]{1,5})?/[A-Za-z0-9._/-]+$`)
	// A branch, tag or SHA.
	repoRefRe = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,255}$`)
)

var errBadRepo = errors.New("repo must be an https:// or ssh git URL with no shell metacharacters")

// validRepoURL reports whether repo is safe to hand to a shell. ".." is
// rejected so the clone cannot traverse out of its directory.
func validRepoURL(repo string) bool {
	if repo == "" || len(repo) > 512 || strings.Contains(repo, "..") {
		return false
	}
	return repoHTTPSRe.MatchString(repo) || repoSCPRe.MatchString(repo) || repoSSHRe.MatchString(repo)
}

// repoDirName derives the checkout directory from the repo URL, falling back
// to "repo" so the target is never empty, "." or "..".
func repoDirName(repo string) string {
	base := repo
	if i := strings.LastIndexAny(base, "/:"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(base, ".git")
	base = path.Clean(base)
	if base == "" || base == "." || base == ".." || strings.HasPrefix(base, "-") {
		return "repo"
	}
	return base
}

// buildCloneCmd returns the shell command that clones repo. GIT_TOKEN is
// referenced as a shell variable, never interpolated, so it only ever expands
// inside the sandbox. The `--` stops git reading a URL or dir as a flag.
func buildCloneCmd(repo, ref string, withToken bool) (cmd, dir string) {
	dir = repoDirName(repo)
	url := repo
	if withToken && strings.HasPrefix(repo, "https://") {
		url = "https://x-access-token:${GIT_TOKEN}@" + strings.TrimPrefix(repo, "https://")
	}
	var b strings.Builder
	b.WriteString("git clone --depth 1")
	if ref != "" {
		b.WriteString(" --branch " + ref)
	}
	b.WriteString(" -- " + url + " " + dir)
	return b.String(), dir
}

// envHasKey reports whether "NAME=VALUE" entries contain the given name.
func envHasKey(env []string, name string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, name+"=") {
			return true
		}
	}
	return false
}
