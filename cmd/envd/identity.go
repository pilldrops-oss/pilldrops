package main

// This sandbox's own platform credential. The node pushes it over the
// credential channel after boot (`SETIDENTITY`); on Docker the runtime copies
// the same two files in before the tenant runs. Either way the guest sees
// /run/pilldrops/token and /run/pilldrops/env, and env.go layers the latter
// into every exec, terminal and login shell.

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
)

const (
	// credSetIdentity is `SETIDENTITY <token> <api_url> <sandbox_id> <domain>`;
	// domain is "-" when the node has none. Rendered by internal/vm/identitypush.go.
	credSetIdentity = "SETIDENTITY"
)

// identityDir is where the credential lands. A var so tests redirect it.
var identityDir = "/run/pilldrops"

type identity struct{ token, apiURL, sandboxID, domain string }

// parseIdentity validates the four fields. Everything is attacker-adjacent
// once written (the env file is sourced by login shells), so each field is
// whitelisted rather than passed through.
func parseIdentity(fields []string) (identity, error) {
	if len(fields) != 4 {
		return identity{}, fmt.Errorf("%s takes exactly 4 arguments, got %d", credSetIdentity, len(fields))
	}
	id := identity{token: fields[0], apiURL: fields[1], sandboxID: fields[2], domain: fields[3]}
	rest, ok := strings.CutPrefix(id.token, sbtoken.IdentityPrefix)
	if !ok || !validTokenHash(rest) {
		// validTokenHash: 64 lower-case hex, which is exactly what sbtoken.Mint produces.
		return identity{}, errors.New("token is not " + sbtoken.IdentityPrefix + " followed by 64 hex characters")
	}
	u, err := url.Parse(id.apiURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return identity{}, errors.New("api_url is not an http(s) URL with a host")
	}
	if !dnsLabel(id.sandboxID) {
		return identity{}, errors.New("sandbox_id is not a DNS label")
	}
	if id.domain == "-" {
		id.domain = ""
	} else if !hostname(id.domain) {
		return identity{}, errors.New("domain is not a hostname")
	}
	return id, nil
}

// dnsLabelPat is [a-z0-9-]{1,63} with no leading or trailing hyphen. Spelled
// as a pattern because a domain is labels joined by dots, and the two checks
// must not be able to disagree about what a label is.
const dnsLabelPat = `[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?`

var (
	dnsLabelRe = regexp.MustCompile(`^` + dnsLabelPat + `$`)
	domainRe   = regexp.MustCompile(`^` + dnsLabelPat + `(\.` + dnsLabelPat + `)*$`)
)

func dnsLabel(s string) bool { return dnsLabelRe.MatchString(s) }

// hostname is the sandbox's own domain, not the kernel hostname that
// applyHostname sets: that one has its own rule (cmd/envd/hostname.go) because
// HOST_NAME_MAX bounds it at 64 and it may carry uppercase.
func hostname(s string) bool { return len(s) <= 253 && domainRe.MatchString(s) }

// envFile is the KEY=VALUE text readEnvFile parses. Same shape the docker
// runtime writes (internal/runtime/docker/identity.go); keep them identical.
func (id identity) envFile() string {
	return "PILLDROPS_API_URL=" + id.apiURL + "\n" +
		"PD_SANDBOX_ID=" + id.sandboxID + "\n" +
		"PD_SANDBOX_DOMAIN=" + id.domain + "\n"
}

// applyIdentity writes both files; the token is 0600 to the user envd runs as
// (root on Firecracker), the env 0644. Written whole-then-renamed so a reader
// never sees a partial token.
func applyIdentity(dir string, id identity) error {
	// 0750, not 0755: only the user envd runs as (and root) needs to traverse
	// to the token. Sessions get the env layered in by envd, so nothing else
	// has to read through this directory.
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	for _, f := range []struct {
		name, body string
		mode       os.FileMode
	}{
		{"token", id.token + "\n", 0o600},
		{"env", id.envFile(), 0o644},
	} {
		p := filepath.Join(dir, f.name)
		if err := os.WriteFile(p+".tmp", []byte(f.body), f.mode); err != nil {
			return err
		}
		if err := os.Chmod(p+".tmp", f.mode); err != nil { // WriteFile honours umask; the token must be exactly 0600
			return err
		}
		if err := os.Rename(p+".tmp", p); err != nil {
			return err
		}
	}
	return nil
}
