// Package webui serves the embedded dashboard SPA, the public docs pages and
// the console login flow. The session key is never written into served HTML;
// login sets an HttpOnly, SameSite=Strict cookie the /v1 layers accept.
package webui

import (
	"bytes"
	"crypto/subtle"
	"embed"
	"html"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/adminauth"
)

//go:embed all:dist
var distFS embed.FS

// SessionCookie is the session cookie name; it must match what the /v1 auth layers read.
const SessionCookie = "sandboxd_session"

// PublicPages are static pages about the software, served at /<slug> from
// dist/<slug>.html in every deployment. An embed missing a page falls back to /guides.
var PublicPages = []string{
	"sdk",
	// Generated from the /guides hub by web/scripts/gen_guide_pages.py.
	"guides/code-interpreter", "guides/coding-agents", "guides/browser-automation",
	"guides/computer-use", "guides/long-running", "guides/evals",
	"guides/app-preview", "guides/functions", "guides/untrusted-code",
	"guides/ci-tests", "guides/data-pipeline", "guides/custom-images",
	"guides/mobile-testing",
	"guides/claude-subscription", "guides/chatgpt-subscription",
	// The launcher, a signed-in tool the SPA build does not own.
	"workstation",
}

// PagesDirEnv names a directory of HTML read at request time that overrides
// the embedded copy of a page, so a copy edit is a file write, not a redeploy.
// Only slugs already in the page lists are overridable.
const PagesDirEnv = "PILLDROPS_PAGES_DIR"

// Analytics is injected into the public pages at serve time. ANALYTICS_DOMAIN is
// the site as registered with the provider; ANALYTICS_SRC is the script URL,
// defaulting to Plausible. Unset injects nothing.
const (
	AnalyticsDomainEnv  = "ANALYTICS_DOMAIN"
	AnalyticsSrcEnv     = "ANALYTICS_SRC"
	defaultAnalyticsSrc = "https://plausible.io/js/script.js"
)

// analyticsTag returns the script tag to inject, or "" when unconfigured.
func analyticsTag() string {
	domain := strings.TrimSpace(os.Getenv(AnalyticsDomainEnv))
	if domain == "" {
		return ""
	}
	src := strings.TrimSpace(os.Getenv(AnalyticsSrcEnv))
	if src == "" {
		src = defaultAnalyticsSrc
	}
	return `<script defer data-domain="` + html.EscapeString(domain) +
		`" src="` + html.EscapeString(src) + `"></script>`
}

// injectAnalytics places the tag before </head>; a page with no </head> or an
// existing tag is returned untouched.
func injectAnalytics(page []byte, tag string) []byte {
	if tag == "" || len(page) == 0 {
		return page
	}
	if bytes.Contains(page, []byte("data-domain=")) {
		return page
	}
	i := bytes.Index(bytes.ToLower(page), []byte("</head>"))
	if i < 0 {
		return page
	}
	out := make([]byte, 0, len(page)+len(tag))
	out = append(out, page[:i]...)
	out = append(out, tag...)
	out = append(out, page[i:]...)
	return out
}

// Server serves the SPA shell, its static assets, the public pages and the login flow.
type Server struct {
	index   []byte            // index.html with mode and runtime injected
	testing []byte            // nil if not embedded
	docs    []byte            // nil if not embedded
	guides  []byte            // nil if not embedded
	install []byte            // install.sh, nil if not embedded
	llms    []byte            // llms.txt, nil if not embedded
	ogImg   []byte            // og.png, nil if not embedded
	extra   map[string][]byte // public pages by slug
	// admins is nil when there is no shared store; the console then uses the API-key form.
	admins AdminStore
	// pagesDir overrides embedded pages at request time. See PagesDirEnv.
	pagesDir    string
	analytics   string       // script tag injected into public pages, "" when unset
	dlDir       string       // directory of CLI binaries (PILLDROPS_DL_DIR), "" = none
	skipInstall bool         // host already serves GET /install.sh
	mode        string       // "user" (node) or "admin" (orchestrator)
	assets      http.Handler // file server rooted at dist/
	sessionKey  string
	logins      loginLimiter
}

// loginLimiter is a per-IP token bucket on login attempts: loginBurst up front,
// then one more every loginRefill.
type loginLimiter struct {
	mu      sync.Mutex
	buckets map[string]*loginBucket
}

type loginBucket struct {
	tokens float64
	last   time.Time
}

const (
	loginBurst  = 8
	loginRefill = 10 * time.Second
	// loginMaxBuckets caps the IP map; hitting it resets the map wholesale.
	loginMaxBuckets = 4096
)

func (l *loginLimiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil || len(l.buckets) >= loginMaxBuckets {
		l.buckets = make(map[string]*loginBucket)
	}
	b := l.buckets[ip]
	if b == nil {
		b = &loginBucket{tokens: loginBurst, last: now}
		l.buckets[ip] = b
	}
	b.tokens += now.Sub(b.last).Seconds() / loginRefill.Seconds()
	if b.tokens > loginBurst {
		b.tokens = loginBurst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// clientIP is the first X-Forwarded-For hop, else the RemoteAddr host.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// New builds the web UI server, injecting runtime, mode, clustered and the
// sandbox preview domain into index.html. sessionKey only gates login and is
// never written into the page.
func New(sessionKey, runtime, mode string, clustered bool, sandboxDomain string) (*Server, error) {
	idx, err := distFS.ReadFile("dist/index.html")
	if err != nil {
		return nil, err
	}
	if runtime == "" {
		runtime = "process"
	}
	if mode == "" {
		mode = "user"
	}
	clusterFlag := "false"
	if clustered {
		clusterFlag = "true"
	}
	// The key placeholder is blanked; the SPA authenticates via the session cookie.
	idx = bytes.ReplaceAll(idx, []byte("__SANDBOXD_KEY__"), []byte(""))
	idx = bytes.ReplaceAll(idx, []byte("__SANDBOXD_RUNTIME__"), []byte(runtime))
	idx = bytes.ReplaceAll(idx, []byte("__SANDBOXD_CLUSTERED__"), []byte(clusterFlag))
	idx = bytes.ReplaceAll(idx, []byte("__SANDBOXD_MODE__"), []byte(mode))
	idx = bytes.ReplaceAll(idx, []byte("__SANDBOXD_DOMAIN__"), []byte(sandboxDomain))
	// Per deployment, not baked into the bundle, or every self-hosted node would
	// ship usage data to the hosted account.
	idx = bytes.ReplaceAll(idx, []byte("__POSTHOG_KEY__"), []byte(os.Getenv("POSTHOG_PROJECT_KEY")))

	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil, err
	}
	// Each public page is optional; a missing one keeps its route's redirect.
	testing, _ := distFS.ReadFile("dist/testing.html") //nolint:errcheck // optional embed
	docs, _ := distFS.ReadFile("dist/docs.html")       //nolint:errcheck // optional embed
	guides, _ := distFS.ReadFile("dist/guides.html")   //nolint:errcheck // optional embed
	install, _ := distFS.ReadFile("dist/install.sh")   //nolint:errcheck // optional embed
	llms, _ := distFS.ReadFile("dist/llms.txt")        //nolint:errcheck // optional embed
	ogImg, _ := distFS.ReadFile("dist/og.png")         //nolint:errcheck // optional embed
	extra := make(map[string][]byte, len(PublicPages))
	for _, p := range PublicPages {
		if b, e := distFS.ReadFile("dist/" + p + ".html"); e == nil {
			extra[p] = b
		}
	}
	return &Server{index: idx, testing: testing, docs: docs, guides: guides, install: install,
		llms: llms, ogImg: ogImg, extra: extra, dlDir: os.Getenv("PILLDROPS_DL_DIR"), mode: mode,
		pagesDir: os.Getenv(PagesDirEnv), analytics: analyticsTag(),
		assets: http.FileServer(http.FS(sub)), sessionKey: sessionKey}, nil
}

// livePage reads a page from the override directory. slug comes from the
// compile-time page lists, never from the request, so no traversal is possible.
func (s *Server) livePage(slug string) ([]byte, bool) {
	if s.pagesDir == "" {
		return nil, false
	}
	b, err := os.ReadFile(filepath.Join(s.pagesDir, filepath.FromSlash(slug)+".html"))
	if err != nil || len(b) == 0 {
		return nil, false
	}
	return b, true
}

// mustWrite ignores the write error; headers are already sent and the usual
// cause is the client going away.
func mustWrite(w http.ResponseWriter, body []byte) {
	_, _ = w.Write(body) //nolint:errcheck
}

// serveLivePage writes an override page, uncacheable so an edit shows on the next reload.
func serveLivePage(w http.ResponseWriter, page []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	mustWrite(w, page)
}

// servePublic writes an embedded public page with a short cache window.
func servePublic(w http.ResponseWriter, page []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	mustWrite(w, page)
}

// Register mounts the public pages, the SPA shell and assets, and the login flow.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard", http.StatusFound)
	})
	// Docs and guides are public in every mode; old embeds without them redirect.
	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		if len(s.docs) > 0 {
			servePublic(w, s.docs)
			return
		}
		http.Redirect(w, r, "/dashboard#/docs", http.StatusFound)
	})
	mux.HandleFunc("GET /guides", func(w http.ResponseWriter, r *http.Request) {
		if len(s.guides) > 0 {
			servePublic(w, s.guides)
			return
		}
		http.Redirect(w, r, "/docs", http.StatusFound)
	})
	mux.HandleFunc("GET /testing", func(w http.ResponseWriter, r *http.Request) {
		if len(s.testing) > 0 {
			servePublic(w, s.testing)
			return
		}
		http.Redirect(w, r, "/guides", http.StatusFound)
	})
	for _, p := range PublicPages {
		slug, page := p, s.extra[p]
		mux.HandleFunc("GET /"+p, func(w http.ResponseWriter, r *http.Request) {
			if live, ok := s.livePage(slug); ok {
				serveLivePage(w, injectAnalytics(live, s.analytics))
				return
			}
			if len(page) > 0 {
				servePublic(w, injectAnalytics(page, s.analytics))
				return
			}
			http.Redirect(w, r, "/guides", http.StatusFound)
		})
	}
	// The CLI installer. The orchestrator serves its node-join script at the
	// same path, and registering both panics the mux.
	if s.mode != "admin" && !s.skipInstall {
		mux.HandleFunc("GET /install.sh", func(w http.ResponseWriter, r *http.Request) {
			if len(s.install) == 0 {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "public, max-age=300")
			mustWrite(w, s.install)
		})
	}
	mux.HandleFunc("GET /llms.txt", func(w http.ResponseWriter, r *http.Request) {
		if len(s.llms) == 0 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		mustWrite(w, s.llms)
	})
	mux.HandleFunc("GET /og.png", func(w http.ResponseWriter, r *http.Request) {
		if len(s.ogImg) == 0 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		mustWrite(w, s.ogImg)
	})
	// A prefix handler, so an image dropped in web/public/img/ needs no route.
	// Not /assets/, which the Vite bundle owns.
	mux.Handle("GET /img/", s.assets)

	if s.dlDir != "" {
		mux.Handle("GET /dl/", http.StripPrefix("/dl/", http.FileServer(http.Dir(s.dlDir))))
	}
	mux.HandleFunc("GET /dashboard/login", s.serveLogin)
	mux.HandleFunc("POST /dashboard/login", func(w http.ResponseWriter, r *http.Request) {
		if s.admins != nil {
			s.handleConsoleLogin(w, r)
			return
		}
		s.handleLogin(w, r)
	})
	mux.HandleFunc("POST /dashboard/setup", s.handleSetup)
	mux.HandleFunc("POST /dashboard/logout", s.handleLogout)
	mux.HandleFunc("GET /dashboard", s.serveIndex)
	mux.HandleFunc("GET /dashboard/{$}", s.serveIndex)
	mux.Handle("GET /dashboard/assets/", http.StripPrefix("/dashboard/", s.assets))
}

// valid reports whether the request carries a valid session cookie. An empty
// sessionKey disables auth.
func (s *Server) valid(r *http.Request) bool {
	if s.sessionKey == "" {
		return true
	}
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return false
	}
	if _, err := adminauth.VerifySession([]byte(s.sessionKey), c.Value, time.Now()); err == nil {
		return true
	}
	// The legacy cookie is the API key itself, accepted only with no account store.
	if s.admins != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.sessionKey)) == 1
}

func (s *Server) serveIndex(w http.ResponseWriter, _ *http.Request) {
	// The shell carries no secrets, so it is served publicly; /v1 enforces auth.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	mustWrite(w, s.index)
}

func (s *Server) serveLogin(w http.ResponseWriter, r *http.Request) {
	if s.valid(r) {
		http.Redirect(w, r, "/dashboard", http.StatusFound)
		return
	}
	if s.admins != nil {
		s.serveConsoleAuth(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	mustWrite(w, loginHTML(""))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.logins.allow(clientIP(r), time.Now()) {
		w.Header().Set("Retry-After", "10")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusTooManyRequests)
		mustWrite(w, loginHTML("Too many attempts. Try again shortly."))
		return
	}
	_ = r.ParseForm() //nolint:errcheck // malformed body fails the compare below
	submitted := r.PostFormValue("key")
	if s.sessionKey == "" || subtle.ConstantTimeCompare([]byte(submitted), []byte(s.sessionKey)) != 1 {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusUnauthorized)
		mustWrite(w, loginHTML("Invalid key. Try again."))
		return
	}
	secure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    s.sessionKey,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/dashboard", http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	http.Redirect(w, r, "/dashboard/login", http.StatusFound)
}

// loginHTML is the API-key form a single-box node uses; errMsg is shown when set.
func loginHTML(errMsg string) []byte {
	return consoleShell("PillDrops — sign in", `<form method="post" action="/dashboard/login">`+
		`<h1>PillDrops control plane</h1>`+
		banner(errMsg)+
		`<label for="key">API key</label>`+
		`<input id="key" type="password" name="key" autofocus autocomplete="current-password" required>`+
		`<button type="submit">Sign in</button>`+
		`<p class="hint">Enter your operator API key.</p>`+
		`</form>`)
}

// SkipInstallScript stops Register from claiming GET /install.sh when the host
// already serves that path.
func (s *Server) SkipInstallScript() { s.skipInstall = true }
