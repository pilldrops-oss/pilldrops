package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
)

// SlugResolver is an optional Resolver capability: the sandbox holding a slug
// and its public port. err means no such slug.
type SlugResolver interface {
	BySlug(ctx context.Context, slug string) (id string, port int, err error)
}

// NormalizeHost is a Host header reduced to what the parsers below expect:
// any :port stripped and lower-cased.
func NormalizeHost(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(host)
}

// hostLabel returns the single label host carries below domain. ok=false for
// no domain, a host outside it, or anything but one label.
func hostLabel(host, domain string) (string, bool) {
	if domain == "" {
		return "", false
	}
	suffix := "." + domain
	if !strings.HasSuffix(host, suffix) {
		return "", false
	}
	label := strings.TrimSuffix(host, suffix)
	if label == "" || strings.Contains(label, ".") {
		return "", false
	}
	return label, true
}

// ParseSlugHost extracts the slug from <slug>.<domain>. A slug starts with a
// letter, so it never overlaps <port>-<id>; agent-* and the reserved labels
// are refused (sandbox.ValidSlug). host must be NormalizeHost'd.
func ParseSlugHost(host, domain string) (slug string, ok bool) {
	label, ok := hostLabel(host, domain)
	if !ok || sandbox.ValidSlug(label) != nil {
		return "", false
	}
	return label, true
}

// AgentLabel is the reserved label for a sandbox's agent surface, agent-<id>.<domain>.
// A separate origin because share-link credentials live in a cookie and agent
// output is untrusted content; the browser's security boundary is the origin.
const AgentLabel = "agent"

// AgentAPIPrefix is the path prefix on an agent origin, rewritten to the node's
// /v1/sandboxes/<id>/agent/... route.
const AgentAPIPrefix = "/v1/agent/"

// ParseAgentHost extracts the sandbox id from agent-<id>.<domain>. host must
// be NormalizeHost'd. Only the first hyphen splits the label from the id.
func ParseAgentHost(host, domain string) (id string, ok bool) {
	label, ok := hostLabel(host, domain)
	if !ok {
		return "", false
	}
	rest, found := strings.CutPrefix(label, AgentLabel+"-")
	if !found || rest == "" {
		return "", false
	}
	return rest, true
}

// AgentRequestPath rewrites /v1/agent/... to /v1/sandboxes/<id>/agent/... and
// passes /agent/claim through; anything else is refused so a path can never
// name a different sandbox than the hostname does.
func AgentRequestPath(id, p string) (string, bool) {
	if rest, found := strings.CutPrefix(p, AgentAPIPrefix); found {
		return "/v1/sandboxes/" + id + "/agent/" + rest, true
	}
	if p == "/agent/claim" {
		return p, true
	}
	return "", false
}

// ParseSandboxHost extracts the sandbox id and app port from <port>-<id>.<domain>.
// host must have any :port stripped. Only the first hyphen splits port from id.
func ParseSandboxHost(host, domain string) (id string, port int, ok bool) {
	label, ok := hostLabel(host, domain)
	if !ok || !isDNSLabel(label) {
		return "", 0, false
	}
	parts := strings.SplitN(label, "-", 2)
	if len(parts) != 2 || parts[1] == "" {
		return "", 0, false
	}
	p, err := strconv.Atoi(parts[0])
	if err != nil || p < 1 || p > 65535 {
		return "", 0, false
	}
	return parts[1], p, true
}

// isDNSLabel reports whether s is ASCII letters, digits and hyphens only.
// The Host header is unvalidated client bytes and the id parsed from it lands
// in a URL path downstream, so a "/" would change which sandbox is named.
func isDNSLabel(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

// HostRouter serves sandbox apps on <port>-<id>.<domain> (and, when the
// resolver is a SlugResolver, on <slug>.<domain> at the sandbox's public
// port) and agent origins on agent-<id>.<domain>, delegating everything else
// to next. A slug no sandbox holds falls through to next. verifyPreview must
// authorize ?ptok= and return the sandbox's owner org; nil fails closed.
func HostRouter(domain string, r Resolver, next http.Handler, verifyPreview func(id string, port int, token string) (org string, ok bool)) http.Handler {
	if domain == "" {
		return next
	}
	suffix := "." + domain
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		host := NormalizeHost(req.Host)
		if !strings.HasSuffix(host, suffix) {
			next.ServeHTTP(w, req)
			return
		}
		// Agent hosts must be checked before the port parse; "agent-sb1" would
		// otherwise fail Atoi and be rejected.
		if id, ok := ParseAgentHost(host, domain); ok {
			target, ok := AgentRequestPath(id, req.URL.Path)
			if !ok {
				// 404 rather than fall through, so the agent origin cannot reach
				// the rest of the node API with a share-link credential.
				http.Error(w, "not an agent endpoint", http.StatusNotFound)
				return
			}
			r2 := req.Clone(req.Context())
			r2.URL.Path = target
			r2.URL.RawPath = ""
			next.ServeHTTP(w, r2)
			return
		}
		id, port, ok := ParseSandboxHost(host, domain)
		if !ok {
			slug, isSlug := ParseSlugHost(host, domain)
			sr, canSlug := r.(SlugResolver)
			if !isSlug || !canSlug {
				http.Error(w, "not a valid sandbox host", http.StatusNotFound)
				return
			}
			var err error
			if id, port, err = sr.BySlug(req.Context(), slug); err != nil {
				next.ServeHTTP(w, req)
				return
			}
		}
		// The slug host is only another spelling of id+port, so the token
		// still binds to those. Verify before asking the daemon to resolve,
		// and possibly wake, the sandbox.
		token := req.URL.Query().Get("ptok")
		var org string
		if verifyPreview != nil {
			var ok bool
			if org, ok = verifyPreview(id, port, token); !ok {
				org = ""
			}
		}
		// Also catches a verifier that authorized but resolved no org; letting
		// that through would make "" a wildcard.
		if org == "" {
			http.Error(w, "invalid or missing preview token", http.StatusForbidden)
			return
		}
		req = requestWithPreviewOrg(req, org)
		target, err := r.PreviewTarget(req.Context(), id, port, org)
		if err != nil {
			switch {
			case errors.Is(err, rt.ErrNotFound), errors.Is(err, rt.ErrForeignOrg):
				http.Error(w, "sandbox not found", http.StatusNotFound)
			case errors.Is(err, rt.ErrNoAppOnPort):
				http.Error(w, err.Error(), http.StatusNotFound)
			default:
				http.Error(w, "sandbox unavailable: "+err.Error(), http.StatusServiceUnavailable)
			}
			return
		}
		errBody := fmt.Sprintf("App is not responding on port %d.\n", port)
		// The subdomain carries id and port, so the whole request path is the app path.
		Forward(keepAwake(w, req, r, id), req, target, req.URL.Path, errBody)
	})
}
