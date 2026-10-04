package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fakeAdmins is an in-memory AdminStore whose CreateFirstAdmin is atomic.
type fakeAdmins struct {
	mu   sync.Mutex
	rows map[string]string // email -> hash
	err  error
}

func newFakeAdmins() *fakeAdmins { return &fakeAdmins{rows: map[string]string{}} }

func (f *fakeAdmins) AdminCount(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows), f.err
}
func (f *fakeAdmins) CreateFirstAdmin(_ context.Context, email, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.rows) > 0 {
		return context.Canceled // any error will do
	}
	f.rows[email] = hash
	return nil
}
func (f *fakeAdmins) AdminByEmail(_ context.Context, email string) (string, string, string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", "", "", false, f.err
	}
	h, ok := f.rows[email]
	if !ok {
		return "", "", "", false, nil
	}
	return email, h, "superadmin", true, nil
}
func (f *fakeAdmins) TouchAdminLogin(context.Context, string) {}
func (f *fakeAdmins) UpdateAdminPassword(_ context.Context, e, h string) error {
	f.mu.Lock()
	f.rows[e] = h
	f.mu.Unlock()
	return nil
}

func consoleServer(t *testing.T, a AdminStore) (*Server, *http.ServeMux) {
	t.Helper()
	s, err := New("session-signing-key", "process", "user", false, "")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	s.SetAdminStore(a)
	mux := http.NewServeMux()
	s.Register(mux)
	return s, mux
}

func post(mux *http.ServeMux, path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

func TestFirstRunOffersSetup(t *testing.T) {
	_, mux := consoleServer(t, newFakeAdmins())
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/dashboard/login", nil))

	body := rr.Body.String()
	if !strings.Contains(body, "/dashboard/setup") {
		t.Error("first run did not offer the setup form")
	}
	if !strings.Contains(body, "Create the admin account") {
		t.Error("setup form missing its heading")
	}
	if strings.Contains(body, "API key") {
		t.Error("still prompting for an API key")
	}
}

func TestSetupCreatesTheSuperAdminAndSignsIn(t *testing.T) {
	f := newFakeAdmins()
	_, mux := consoleServer(t, f)

	rr := post(mux, "/dashboard/setup", url.Values{
		"email": {"Admin@Example.COM"}, "password": {"a-long-enough-password"}, "confirm": {"a-long-enough-password"},
	})
	if rr.Code != http.StatusFound {
		t.Fatalf("setup = %d, want 302 (body %q)", rr.Code, rr.Body.String())
	}
	if _, ok := f.rows["admin@example.com"]; !ok {
		t.Fatalf("account not stored under a normalised email: %v", f.rows)
	}
	if h := f.rows["admin@example.com"]; strings.Contains(h, "a-long-enough-password") {
		t.Fatal("the password was stored in recoverable form")
	}
	var got *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == SessionCookie {
			got = c
		}
	}
	if got == nil {
		t.Fatal("no session cookie issued")
	}
	if !got.HttpOnly || got.SameSite != http.SameSiteStrictMode {
		t.Errorf("session cookie is not HttpOnly/SameSite=Strict: %+v", got)
	}
	if got.Value == "session-signing-key" {
		t.Fatal("the cookie is the API key — that is the arrangement this replaces")
	}
}

func TestSetupIsClosedOnceClaimed(t *testing.T) {
	f := newFakeAdmins()
	_, mux := consoleServer(t, f)

	if rr := post(mux, "/dashboard/setup", url.Values{
		"email": {"first@example.com"}, "password": {"a-long-enough-password"}, "confirm": {"a-long-enough-password"},
	}); rr.Code != http.StatusFound {
		t.Fatalf("first setup = %d", rr.Code)
	}
	rr := post(mux, "/dashboard/setup", url.Values{
		"email": {"second@example.com"}, "password": {"another-long-password"}, "confirm": {"another-long-password"},
	})
	if rr.Code == http.StatusFound {
		t.Fatal("a second administrator was created through setup")
	}
	if _, ok := f.rows["second@example.com"]; ok {
		t.Fatal("the second account was stored")
	}
	if !strings.Contains(rr.Body.String(), "/dashboard/login") {
		t.Error("did not redirect the user to sign in")
	}
}

func TestConcurrentSetupYieldsOneAdmin(t *testing.T) {
	f := newFakeAdmins()
	_, mux := consoleServer(t, f)

	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = post(mux, "/dashboard/setup", url.Values{
				"email":    {"claimer" + string(rune('a'+i)) + "@example.com"},
				"password": {"a-long-enough-password"}, "confirm": {"a-long-enough-password"},
			}).Code
		}(i)
	}
	wg.Wait()

	if n := len(f.rows); n != 1 {
		t.Fatalf("%d administrators created, want exactly 1", n)
	}
	won := 0
	for _, c := range codes {
		if c == http.StatusFound {
			won++
		}
	}
	if won != 1 {
		t.Errorf("%d requests reported success, want 1", won)
	}
}

func TestLoginVerifiesThePassword(t *testing.T) {
	f := newFakeAdmins()
	_, mux := consoleServer(t, f)
	post(mux, "/dashboard/setup", url.Values{
		"email": {"admin@example.com"}, "password": {"a-long-enough-password"}, "confirm": {"a-long-enough-password"},
	})

	if rr := post(mux, "/dashboard/login", url.Values{
		"email": {"admin@example.com"}, "password": {"a-long-enough-password"},
	}); rr.Code != http.StatusFound {
		t.Errorf("correct password = %d, want 302", rr.Code)
	}
	if rr := post(mux, "/dashboard/login", url.Values{
		"email": {"admin@example.com"}, "password": {"wrong-but-long-enough"},
	}); rr.Code != http.StatusUnauthorized {
		t.Errorf("wrong password = %d, want 401", rr.Code)
	}
	if rr := post(mux, "/dashboard/login", url.Values{
		"key": {"session-signing-key"},
	}); rr.Code == http.StatusFound {
		t.Error("the API key still signs in — the console is still the API's threat model")
	}
}

func TestLoginDoesNotRevealWhichAccountsExist(t *testing.T) {
	f := newFakeAdmins()
	_, mux := consoleServer(t, f)
	post(mux, "/dashboard/setup", url.Values{
		"email": {"real@example.com"}, "password": {"a-long-enough-password"}, "confirm": {"a-long-enough-password"},
	})

	wrongPw := post(mux, "/dashboard/login", url.Values{
		"email": {"real@example.com"}, "password": {"definitely-not-it-x"},
	})
	noSuch := post(mux, "/dashboard/login", url.Values{
		"email": {"ghost@example.com"}, "password": {"definitely-not-it-x"},
	})
	if wrongPw.Code != noSuch.Code {
		t.Errorf("status differs: wrong password %d vs unknown account %d", wrongPw.Code, noSuch.Code)
	}
	if wrongPw.Body.String() != noSuch.Body.String() {
		t.Error("the response body differs between a wrong password and an unknown account")
	}
}

func TestSetupValidatesItsInput(t *testing.T) {
	for _, tc := range []struct{ name, email, pw, confirm string }{
		{"bad email", "not-an-email", "a-long-enough-password", "a-long-enough-password"},
		{"mismatch", "a@b.co", "a-long-enough-password", "a-different-password"},
		{"too short", "a@b.co", "short", "short"},
		{"empty", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAdmins()
			_, mux := consoleServer(t, f)
			rr := post(mux, "/dashboard/setup", url.Values{
				"email": {tc.email}, "password": {tc.pw}, "confirm": {tc.confirm},
			})
			if rr.Code == http.StatusFound {
				t.Errorf("%s was accepted", tc.name)
			}
			if len(f.rows) != 0 {
				t.Errorf("%s created an account", tc.name)
			}
		})
	}
}

// Asserts the token values so a drift from web/src/theme.css is caught.
func TestAuthPagesUseTheDashboardTheme(t *testing.T) {
	f := newFakeAdmins()
	_, mux := consoleServer(t, f)

	page := func(path string) string {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		return rr.Body.String()
	}
	setup := page("/dashboard/login") // first run serves the setup form

	for _, want := range []struct{ token, why string }{
		{"#fefdfb", "the dashboard's warm page background"},
		{"#0c8a56", "the dashboard's green primary"},
		{"#141310", "the dashboard's ink"},
		{"Hanken Grotesk", "the dashboard's sans"},
		{"#131210", "the coal dark background"},
		{"#35c98a", "the dark-mode green"},
	} {
		if !strings.Contains(setup, want.token) {
			t.Errorf("missing %s (%s)", want.token, want.why)
		}
	}
	for _, gone := range []string{"#0b0d10", "#4f8cff", "#15181d"} {
		if strings.Contains(setup, gone) {
			t.Errorf("the old dark/blue palette survives: %s", gone)
		}
	}
	if !strings.Contains(setup, "sandboxd-theme") {
		t.Error("does not read the dashboard's stored theme preference")
	}
	if !strings.Contains(setup, "documentElement") {
		t.Error("theme is not applied to the root element, so it will flash")
	}
}

func TestEveryAuthFormSharesTheShell(t *testing.T) {
	f := newFakeAdmins()
	_, mux := consoleServer(t, f)
	setup := httptest.NewRecorder()
	mux.ServeHTTP(setup, httptest.NewRequest(http.MethodGet, "/dashboard/login", nil))

	post(mux, "/dashboard/setup", url.Values{
		"email": {"a@b.co"}, "password": {"a-long-enough-password"}, "confirm": {"a-long-enough-password"},
	})
	signin := httptest.NewRecorder()
	mux.ServeHTTP(signin, httptest.NewRequest(http.MethodGet, "/dashboard/login", nil))

	keyForm := string(loginHTML(""))

	for name, body := range map[string]string{
		"setup": setup.Body.String(), "sign-in": signin.Body.String(), "api-key": keyForm,
	} {
		if !strings.Contains(body, "#0c8a56") {
			t.Errorf("%s form does not use the dashboard primary", name)
		}
		if !strings.Contains(body, "--r:5px") {
			t.Errorf("%s form does not use the dashboard corner radius", name)
		}
	}
}

func TestConsoleSignsInWithACustomerKey(t *testing.T) {
	s, mux := consoleServer(t, newFakeAdmins())
	s.SetKeyCheck(func(_ context.Context, key string) (string, error) {
		switch key {
		case "outage":
			return "", context.DeadlineExceeded
		case "pd_customer":
			return "user", nil
		case "pd_node":
			return "operator", nil
		}
		return "", nil
	})

	rr := post(mux, "/dashboard/login", url.Values{"key": {"pd_customer"}})
	if rr.Code != http.StatusFound {
		t.Fatalf("valid key = %d, want 302 (body %q)", rr.Code, rr.Body.String())
	}
	var got *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == SessionCookie {
			got = c
		}
	}
	// The /v1 layer reads a raw key in this cookie as that key.
	if got == nil || got.Value != "pd_customer" || !got.HttpOnly || got.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie = %+v, want the key, HttpOnly, SameSite=Strict", got)
	}

	if rr := post(mux, "/dashboard/login", url.Values{"key": {"pd_revoked"}}); rr.Code != http.StatusUnauthorized {
		t.Errorf("unknown key = %d, want 401", rr.Code)
	}
	if rr := post(mux, "/dashboard/login", url.Values{"key": {"session-signing-key"}}); rr.Code != http.StatusUnauthorized {
		t.Errorf("the master key = %d, want 401: it is not a customer key", rr.Code)
	}
	if rr := post(mux, "/dashboard/login", url.Values{"key": {"outage"}}); rr.Code != http.StatusServiceUnavailable {
		t.Errorf("store error = %d, want 503", rr.Code)
	}

	// An operator-role key (node or bootstrap) must not open a console
	// session; operators sign in with their admin account instead.
	unknown := post(mux, "/dashboard/login", url.Values{"key": {"pd_revoked"}})
	operator := post(mux, "/dashboard/login", url.Values{"key": {"pd_node"}})
	if operator.Code != http.StatusUnauthorized {
		t.Errorf("operator-role key = %d, want 401", operator.Code)
	}
	if operator.Body.String() != unknown.Body.String() {
		t.Error("an operator-role key gets a different error than an unknown key")
	}
}

func TestSignInOffersBothEmailAndKey(t *testing.T) {
	body := string(signinHTML(""))
	for _, want := range []string{`name="email"`, `name="password"`, `name="key"`} {
		if !strings.Contains(body, want) {
			t.Errorf("sign-in page lacks %s", want)
		}
	}
}
