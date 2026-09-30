package webui

import (
	"context"
	"html"
	"net/http"
	"strconv"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/adminauth"
)

// AdminStore is the slice of the store the console's account sign-in needs.
type AdminStore interface {
	AdminCount(ctx context.Context) (int, error)
	CreateFirstAdmin(ctx context.Context, email, passHash string) error
	AdminByEmail(ctx context.Context, email string) (email2, passHash, role string, found bool, err error)
	TouchAdminLogin(ctx context.Context, email string)
	UpdateAdminPassword(ctx context.Context, email, passHash string) error
}

// SetAdminStore switches the console from the API-key form to account sign-in.
func (s *Server) SetAdminStore(a AdminStore) { s.admins = a }

// firstRun reports whether the fleet has never been claimed. A store error
// counts as claimed, so a database blip never presents an open setup form.
func (s *Server) firstRun(ctx context.Context) bool {
	if s.admins == nil {
		return false
	}
	n, err := s.admins.AdminCount(ctx)
	return err == nil && n == 0
}

// serveConsoleAuth renders setup on an unclaimed fleet, sign-in otherwise.
func (s *Server) serveConsoleAuth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if s.firstRun(r.Context()) {
		mustWrite(w, setupHTML(""))
		return
	}
	mustWrite(w, signinHTML(""))
}

// handleSetup creates the first administrator. It is a no-op once any account
// exists, and the store's insert is atomic against a simultaneous second claim.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if s.admins == nil {
		http.NotFound(w, r)
		return
	}
	if !s.logins.allow(clientIP(r), time.Now()) {
		s.consoleError(w, http.StatusTooManyRequests, setupHTML, "Too many attempts. Try again shortly.")
		return
	}
	if !s.firstRun(r.Context()) {
		s.consoleError(w, http.StatusConflict, signinHTML, "This deployment already has an administrator. Sign in instead.")
		return
	}
	_ = r.ParseForm() //nolint:errcheck // malformed body fails validation below

	email := adminauth.NormalizeEmail(r.PostFormValue("email"))
	pw, confirm := r.PostFormValue("password"), r.PostFormValue("confirm")

	switch {
	case !adminauth.ValidEmail(email):
		s.consoleError(w, http.StatusBadRequest, setupHTML, "Enter a valid email address.")
		return
	case pw != confirm:
		s.consoleError(w, http.StatusBadRequest, setupHTML, "The two passwords do not match.")
		return
	case len(pw) < adminauth.MinPasswordLen:
		s.consoleError(w, http.StatusBadRequest, setupHTML, adminauth.ErrWeakPassword.Error())
		return
	}

	hash, err := adminauth.Hash(pw)
	if err != nil {
		s.consoleError(w, http.StatusBadRequest, setupHTML, err.Error())
		return
	}
	if err := s.admins.CreateFirstAdmin(r.Context(), email, hash); err != nil {
		// Includes losing the race to another claim.
		s.consoleError(w, http.StatusConflict, signinHTML, "This deployment already has an administrator. Sign in instead.")
		return
	}
	s.startSession(w, r, email)
}

// handleConsoleLogin verifies email + password.
func (s *Server) handleConsoleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.logins.allow(clientIP(r), time.Now()) {
		s.consoleError(w, http.StatusTooManyRequests, signinHTML, "Too many attempts. Try again shortly.")
		return
	}
	_ = r.ParseForm() //nolint:errcheck // empty values fail verification below

	email := adminauth.NormalizeEmail(r.PostFormValue("email"))
	pw := r.PostFormValue("password")

	_, hash, _, found, err := s.admins.AdminByEmail(r.Context(), email)
	if err != nil {
		s.consoleError(w, http.StatusServiceUnavailable, signinHTML, "Cannot verify credentials right now. Try again shortly.")
		return
	}
	// Same message, code and hash cost for an unknown account and a wrong
	// password, or the form enumerates accounts.
	if !found {
		adminauth.Verify(dummyHash, pw)
		s.consoleError(w, http.StatusUnauthorized, signinHTML, "Incorrect email or password.")
		return
	}
	if !adminauth.Verify(hash, pw) {
		s.consoleError(w, http.StatusUnauthorized, signinHTML, "Incorrect email or password.")
		return
	}
	// Login is the only moment the plaintext exists to rehash with a newer work factor.
	if adminauth.NeedsUpgrade(hash) {
		if fresh, err := adminauth.Hash(pw); err == nil {
			_ = s.admins.UpdateAdminPassword(r.Context(), email, fresh) //nolint:errcheck // best-effort upgrade
		}
	}
	s.admins.TouchAdminLogin(r.Context(), email)
	s.startSession(w, r, email)
}

// startSession issues the signed session cookie and lands on the dashboard.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, email string) {
	tok := adminauth.MintSession([]byte(s.sessionKey), email, time.Now())
	if tok == "" {
		// An unverifiable cookie would present as a login loop.
		s.consoleError(w, http.StatusServiceUnavailable, signinHTML,
			"This deployment has no session key configured; set ORCHESTRATOR_API_KEY.")
		return
	}
	secure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(adminauth.SessionTTL.Seconds()),
	})
	http.Redirect(w, r, "/dashboard", http.StatusFound)
}

// consoleError renders a form with a message.
func (s *Server) consoleError(w http.ResponseWriter, code int, form func(string) []byte, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	mustWrite(w, form(msg))
}

// dummyHash is verified against when no account exists, so an unknown address
// costs the same time as a wrong password.
var dummyHash = func() string {
	h, err := adminauth.Hash("not-a-real-password-placeholder")
	if err != nil {
		return ""
	}
	return h
}()

// consoleCSS mirrors the dashboard's tokens and components in web/src/theme.css
// (.ws brand block, .mark, .btn.primary, the dotted body); change both together.
const consoleCSS = `:root{` +
	`--bg:#fefdfb;--surface:#ffffff;--line:rgba(20,18,12,.13);--line2:rgba(20,18,12,.26);` +
	`--ink:#141310;--sub:#565448;--dim:#8b887c;--dots:rgba(20,18,12,.055);` +
	`--primbg:#0c8a56;--primfg:#ffffff;--primbg-h:#0a7548;--ring:rgba(12,138,86,.14);` +
	`--danger:#b3261e;--dangersoft:rgba(179,38,30,.08);` +
	`--cardsh:0 1px 3px rgba(20,18,12,.04);--r:5px;` +
	`--sans:'Hanken Grotesk',system-ui,-apple-system,sans-serif;` +
	`--mono:'JetBrains Mono',ui-monospace,monospace}` +
	// Dark variant, keyed on both the root attribute this page sets and the SPA's body.dark.
	`:root[data-theme="dark"],body.dark{` +
	`--bg:#131210;--surface:#1a1815;--line:rgba(244,240,230,.12);--line2:rgba(244,240,230,.26);` +
	`--ink:#f4f1e8;--sub:#a8a294;--dim:#7a7567;--dots:rgba(244,240,230,.06);` +
	`--primbg:#35c98a;--primfg:#131210;--primbg-h:#2fb87e;--ring:rgba(53,201,138,.16);` +
	`--danger:#f87171;--dangersoft:rgba(220,38,38,.08);` +
	`--cardsh:none}` +
	`*,*::before,*::after{box-sizing:border-box}` +
	`html,body{margin:0;height:100%}` +
	`body{font-family:var(--sans);font-size:14px;background:var(--bg);color:var(--ink);` +
	`-webkit-font-smoothing:antialiased;` +
	`background-image:radial-gradient(var(--dots) 1px,transparent 1px);background-size:22px 22px;` +
	`display:flex;min-height:100%;align-items:center;justify-content:center;padding:1.5rem}` +
	`main{width:100%;max-width:380px}` +
	`.ws{display:flex;align-items:center;gap:11px;margin:0 0 18px 2px}` +
	`.mark{width:28px;height:28px;border-radius:var(--r);flex:none;position:relative;` +
	`background:conic-gradient(from 210deg,#e05d2a,#ffb03a 30%,#35c98a 66%,#0f3d28 100%)}` +
	`.mark::after{content:'';position:absolute;inset:6px;background:var(--bg);border-radius:2px}` +
	`.nm{font-weight:700;font-size:14px;line-height:1.1;letter-spacing:-.2px}` +
	`.nm small{display:block;color:var(--dim);font-weight:500;font-size:11px}` +
	`form{background:var(--surface);border:1px solid var(--line);border-radius:14px;` +
	`box-shadow:var(--cardsh);padding:28px}` +
	`h1{font-family:var(--mono);font-size:18px;font-weight:700;letter-spacing:-.02em;margin:0 0 16px;color:var(--ink)}` +
	`p.sub{color:var(--sub);font-size:13px;line-height:1.55;margin:-10px 0 20px}` +
	`label{display:block;font-size:12.5px;font-weight:600;color:var(--ink);margin:0 0 5px}` +
	`input{width:100%;padding:8px 11px;font:inherit;font-size:13.5px;color:var(--ink);` +
	`background:var(--bg);border:1px solid var(--line2);border-radius:var(--r);margin:0 0 14px;` +
	`transition:border-color .12s ease}` +
	`input:focus{outline:none;border-color:var(--primbg);box-shadow:0 0 0 3px var(--ring)}` +
	`button{width:100%;margin-top:4px;padding:9px 14px;font-family:var(--mono);font-size:12.5px;` +
	`font-weight:600;letter-spacing:.01em;cursor:pointer;color:var(--primfg);background:var(--primbg);` +
	`border:1px solid var(--primbg);border-radius:var(--r);transition:background .12s ease}` +
	`button:hover{background:var(--primbg-h);border-color:var(--primbg-h)}` +
	`button:focus-visible{outline:2px solid var(--primbg);outline-offset:2px}` +
	`.err{color:var(--danger);background:var(--dangersoft);border:1px solid var(--danger);` +
	`border-radius:6px;padding:9px 12px;font-size:13px;margin:0 0 14px;line-height:1.45}` +
	`.hint{color:var(--dim);font-size:12px;margin:14px 0 0;line-height:1.5}` +
	`@media (prefers-reduced-motion:reduce){*{transition:none!important}}`

// themeScript reads the SPA's stored theme choice. It runs synchronously in
// <head> and sets an attribute on <html>, because <body> does not exist yet and
// waiting for it would flash the light theme first.
const themeScript = `<script>try{if(localStorage.getItem('sandboxd-theme')==='dark')` +
	`document.documentElement.dataset.theme='dark'}catch(e){}</script>`

func consoleShell(title, body string) []byte {
	return []byte(`<!doctype html><html lang="en"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<meta name="robots" content="noindex">` +
		`<title>` + title + `</title>` + themeScript +
		// The dashboard's own faces, from the same source web/index.html loads them.
		`<link rel="preconnect" href="https://fonts.googleapis.com">` +
		`<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>` +
		`<link href="https://fonts.googleapis.com/css2?family=Hanken+Grotesk:wght@400;500;600;700` +
		`&family=JetBrains+Mono:wght@400;500;600;700&display=swap" rel="stylesheet">` +
		`<style>` + consoleCSS + `</style></head><body><main>` +
		`<div class="ws"><div class="mark"></div><div class="nm">pilldrops<small>self-hosted sandboxes</small></div></div>` +
		body + `</main></body></html>`)
}

func banner(errMsg string) string {
	if errMsg == "" {
		return ""
	}
	return `<p class="err">` + html.EscapeString(errMsg) + `</p>`
}

// setupHTML is the first-run form.
func setupHTML(errMsg string) []byte {
	return consoleShell("PillDrops — set up", `<form method="post" action="/dashboard/setup">`+
		`<h1>Create the admin account</h1>`+
		`<p class="sub">Nobody has set up this server yet. The account you create now is `+
		`its administrator, stored in the server's own database.</p>`+
		banner(errMsg)+
		`<label for="email">Email</label>`+
		`<input id="email" type="email" name="email" autofocus autocomplete="username" required>`+
		`<label for="password">Password</label>`+
		`<input id="password" type="password" name="password" autocomplete="new-password" required>`+
		`<label for="confirm">Confirm password</label>`+
		`<input id="confirm" type="password" name="confirm" autocomplete="new-password" required>`+
		`<button type="submit">Create account</button>`+
		`<p class="hint">At least `+strconv.Itoa(adminauth.MinPasswordLen)+` characters. `+
		`Nothing leaves this server.</p>`+
		`</form>`)
}

func signinHTML(errMsg string) []byte {
	return consoleShell("PillDrops — sign in", `<form method="post" action="/dashboard/login">`+
		`<h1>Sign in</h1>`+
		banner(errMsg)+
		`<label for="email">Email</label>`+
		`<input id="email" type="email" name="email" autofocus autocomplete="username" required>`+
		`<label for="password">Password</label>`+
		`<input id="password" type="password" name="password" autocomplete="current-password" required>`+
		`<button type="submit">Sign in</button>`+
		`</form>`)
}
