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

// consoleCSS mirrors the dashboard tokens in web/src/theme.css; change both together.
const consoleCSS = `:root{` +
	`--bg:#fefdfb;--surface:#ffffff;--line:rgba(20,18,12,.13);` +
	`--ink:#141310;--sub:#565448;--dim:#8b887c;` +
	`--primbg:#0c8a56;--primfg:#ffffff;--primbg-h:#0a7548;` +
	`--danger:#b3261e;--dangersoft:rgba(179,38,30,.08);` +
	`--cardsh:0 1px 3px rgba(20,18,12,.04);--r:5px;` +
	`--sans:'Hanken Grotesk',system-ui,-apple-system,sans-serif;` +
	`--mono:'JetBrains Mono',ui-monospace,monospace}` +
	// Dark variant, keyed on both the root attribute this page sets and the SPA's body.dark.
	`:root[data-theme="dark"],body.dark{` +
	`--bg:#131210;--surface:#1a1815;--line:rgba(244,240,230,.12);` +
	`--ink:#f4f1e8;--sub:#a8a294;--dim:#7a7567;` +
	`--primbg:#35c98a;--primfg:#131210;--primbg-h:#2fb87e;` +
	`--danger:#f87171;--dangersoft:rgba(220,38,38,.08);` +
	`--cardsh:none}` +
	`*,*::before,*::after{box-sizing:border-box}` +
	`html,body{margin:0;height:100%}` +
	`body{font-family:var(--sans);background:var(--bg);color:var(--ink);` +
	`display:flex;min-height:100%;align-items:center;justify-content:center;padding:1.5rem}` +
	`form{background:var(--surface);border:1px solid var(--line);border-radius:var(--r);` +
	`box-shadow:var(--cardsh);padding:1.75rem;width:100%;max-width:360px}` +
	`h1{font-size:1rem;font-weight:600;letter-spacing:-.01em;margin:0 0 .35rem;color:var(--ink)}` +
	`p.sub{color:var(--sub);font-size:.82rem;line-height:1.5;margin:0 0 1.25rem}` +
	`label{display:block;font-size:.75rem;font-weight:500;color:var(--sub);margin:0 0 .35rem}` +
	`input{width:100%;padding:.55rem .65rem;font:inherit;font-size:.9rem;color:var(--ink);` +
	`background:var(--bg);border:1px solid var(--line);border-radius:var(--r);margin:0 0 .9rem;` +
	`transition:border-color .12s ease}` +
	`input:focus{outline:none;border-color:var(--primbg);box-shadow:0 0 0 3px rgba(12,138,86,.12)}` +
	`body.dark input:focus{box-shadow:0 0 0 3px rgba(53,201,138,.15)}` +
	`button{width:100%;padding:.6rem;font:inherit;font-size:.88rem;font-weight:600;cursor:pointer;` +
	`color:var(--primfg);background:var(--primbg);border:0;border-radius:var(--r);` +
	`transition:background .12s ease}` +
	`button:hover{background:var(--primbg-h)}` +
	`button:focus-visible{outline:2px solid var(--primbg);outline-offset:2px}` +
	`.err{color:var(--danger);background:var(--dangersoft);border:1px solid var(--danger);` +
	`border-radius:var(--r);padding:.5rem .65rem;font-size:.8rem;margin:0 0 .9rem;line-height:1.45}` +
	`.hint{color:var(--dim);font-size:.75rem;margin:.9rem 0 0;line-height:1.5}` +
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
		`<style>` + consoleCSS + `</style></head><body>` + body + `</body></html>`)
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
		`<h1>Create the administrator account</h1>`+
		`<p class="sub">This deployment has no administrator yet. The account you create `+
		`here is the super admin for this fleet, stored in your own database.</p>`+
		banner(errMsg)+
		`<label for="email">Email</label>`+
		`<input id="email" type="email" name="email" autofocus autocomplete="username" required>`+
		`<label for="password">Password</label>`+
		`<input id="password" type="password" name="password" autocomplete="new-password" required>`+
		`<label for="confirm">Confirm password</label>`+
		`<input id="confirm" type="password" name="confirm" autocomplete="new-password" required>`+
		`<button type="submit">Create account</button>`+
		`<p class="hint">At least `+strconv.Itoa(adminauth.MinPasswordLen)+` characters. `+
		`Nothing is sent anywhere — this is your deployment's own database.</p>`+
		`</form>`)
}

func signinHTML(errMsg string) []byte {
	return consoleShell("PillDrops — sign in", `<form method="post" action="/dashboard/login">`+
		`<h1>PillDrops control plane</h1>`+
		banner(errMsg)+
		`<label for="email">Email</label>`+
		`<input id="email" type="email" name="email" autofocus autocomplete="username" required>`+
		`<label for="password">Password</label>`+
		`<input id="password" type="password" name="password" autocomplete="current-password" required>`+
		`<button type="submit">Sign in</button>`+
		`</form>`)
}
