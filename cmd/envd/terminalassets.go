// xterm.js and its two addons, vendored from the npm tarballs and served by
// envd. Scripts in this page can read and write the tenant's shell, so nothing
// loads from a CDN; the SRI digests in the manifest pin the embedded bytes.
package main

import (
	"crypto/sha256"
	"crypto/sha512"
	"embed"
	"encoding/base64"
	"net/http"
	"strings"
)

//go:embed assets
var terminalAssetFS embed.FS

// terminalAsset is one vendored file.
type terminalAsset struct {
	Name   string // basename under assets/ and the last URL path segment
	MIME   string
	SRI    string // "sha384-<base64>" of the embedded bytes; enforced by a test, not sent to the browser
	Origin string // npm path the file came from, for re-vendoring
}

// terminalAssets is the manifest; xterm.js must precede the addons that attach to it.
var terminalAssets = []terminalAsset{
	{
		Name:   "xterm.css",
		MIME:   "text/css; charset=utf-8",
		SRI:    "sha384-LJcOxlx9IMbNXDqJ2axpfEQKkAYbFjJfhXexLfiRJhjDU81mzgkiQq8rkV0j6dVh",
		Origin: "npm:xterm@5.3.0/css/xterm.css",
	},
	{
		Name:   "xterm.js",
		MIME:   "text/javascript; charset=utf-8",
		SRI:    "sha384-/nfmYPUzWMS6v2atn8hbljz7NE0EI1iGx34lJaNzyVjWGDzMv+ciUZUeJpKA3Glc",
		Origin: "npm:xterm@5.3.0/lib/xterm.js",
	},
	{
		Name:   "addon-fit.js",
		MIME:   "text/javascript; charset=utf-8",
		SRI:    "sha384-iF+jqbuti4XlB64clWgFWYEscb+UnSRv3VgVikGYZu+otNFnSHr7y7NcKfBnGizn",
		Origin: "npm:@xterm/addon-fit@0.10.0/lib/addon-fit.js",
	},
	{
		Name:   "addon-attach.js",
		MIME:   "text/javascript; charset=utf-8",
		SRI:    "sha384-CiZ7I6CW+T3IMWRww9jbMuc8Sq4D/iKqYFjYtbZjdK6PhD9p+mDpo4phnhhk5w0Z",
		Origin: "npm:@xterm/addon-attach@0.11.0/lib/addon-attach.js",
	},
}

// terminalAssetBytes returns the embedded bytes for a manifest name, or ok=false.
// Only manifest names reach the embedded FS, so no traversal or listing is possible.
func terminalAssetBytes(name string) (terminalAsset, []byte, bool) {
	for _, a := range terminalAssets {
		if a.Name == name {
			b, err := terminalAssetFS.ReadFile("assets/" + a.Name)
			if err != nil {
				return a, nil, false
			}
			return a, b, true
		}
	}
	return terminalAsset{}, nil, false
}

// sriOf computes the "sha384-<base64>" subresource-integrity digest of b.
func sriOf(b []byte) string {
	sum := sha512.Sum384(b)
	return "sha384-" + base64.StdEncoding.EncodeToString(sum[:])
}

// handleTerminalAsset serves a vendored asset, matching on the last path
// segment because a relative src under a path-prefixed preview URL with no
// trailing slash resolves one segment too high. The manifest is the allowlist.
func handleTerminalAsset(w http.ResponseWriter, r *http.Request) bool {
	name := r.URL.Path
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	asset, b, ok := terminalAssetBytes(name)
	if !ok {
		return false
	}
	w.Header().Set("Content-Type", asset.MIME)
	// The bytes cannot change without a new envd binary.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(b)
	return true
}

// terminalCSP is the Content-Security-Policy for the terminal page. default-src
// 'none' makes a reintroduced CDN tag fail visibly. style-src keeps
// 'unsafe-inline' because xterm.js injects a <style> element at runtime.
func terminalCSP(inlineScript string) string {
	sum := sha256.Sum256([]byte(inlineScript))
	hash := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	return strings.Join([]string{
		"default-src 'none'",
		"script-src 'self' " + hash,
		"style-src 'self' 'unsafe-inline'",
		"connect-src 'self'",
		"img-src 'self' data:",
		"font-src 'self'",
		"base-uri 'none'",
		"form-action 'none'",
		// No frame-ancestors: the dashboard iframes the preview from another
		// origin, and the portal domain differs per self-hosted deployment.
	}, "; ")
}
