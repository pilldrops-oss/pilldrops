package orchestrator

import (
	"bytes"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

var browserNav = http.Header{"Sec-Fetch-Dest": {"document"}, "Sec-Fetch-Mode": {"navigate"}}

func bodyOf(t *testing.T, r *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLinkServesRequestsWithBodies(t *testing.T) {
	var gotHost, gotFwd, gotXFF string
	l := startShare(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotFwd, gotXFF = r.Host, r.Header.Get("X-Forwarded-Host"), r.Header.Get("X-Forwarded-For")
		w.Header().Set("X-App", "yes")
		_, _ = io.Copy(w, r.Body)
	}))
	payload := bytes.Repeat([]byte("p"), 8<<20)
	resp := visit(t, l.base, l.host(), "POST", "/echo?x=1", bytes.NewReader(payload),
		http.Header{"X-Forwarded-For": {"6.6.6.6"}})
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !bytes.Equal(got, payload) || resp.Header.Get("X-App") != "yes" {
		t.Fatalf("status %d, %d bytes", resp.StatusCode, len(got))
	}
	if !strings.HasPrefix(gotHost, "localhost:") {
		t.Fatalf("Host = %q, want localhost:<port>", gotHost)
	}
	if gotFwd != l.host() {
		t.Fatalf("X-Forwarded-Host = %q", gotFwd)
	}
	if strings.Contains(gotXFF, "6.6.6.6") {
		t.Fatalf("a forged X-Forwarded-For reached the app: %q", gotXFF)
	}
	if resp.Header.Get("X-Robots-Tag") != "noindex" {
		t.Fatal("no noindex header")
	}
}

func TestStreamedBytesArriveAsWritten(t *testing.T) {
	release := make(chan struct{})
	l := startShare(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("first\n"))
		w.(http.Flusher).Flush()
		<-release
		_, _ = w.Write([]byte("second\n"))
	}))
	defer close(release)
	resp := visit(t, l.base, l.host(), "GET", "/events", nil, nil)
	buf := make([]byte, 6)
	done := make(chan error, 1)
	go func() { _, err := io.ReadFull(resp.Body, buf); done <- err }()
	select {
	case err := <-done:
		if err != nil || string(buf) != "first\n" {
			t.Fatalf("read %q %v", buf, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first chunk was held back until the response ended")
	}
}

func TestWebSocketUpgradeWorksThroughTheLink(t *testing.T) {
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	l := startShare(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		kind, msg, err := c.ReadMessage()
		if err == nil {
			_ = c.WriteMessage(kind, msg)
		}
	}))
	h := http.Header{"Host": {l.host()}}
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(l.base, "http")+"/hmr", h)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close()
	if err := ws.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, msg, err := ws.ReadMessage(); err != nil || string(msg) != "ping" {
		t.Fatalf("echo = %q %v", msg, err)
	}
}

func TestLinkSurvivesTheAppRestartingBetweenRequests(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	serve := func(l net.Listener) *httptest.Server {
		s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("ok"))
		}))
		s.Listener = l
		s.Start()
		return s
	}
	app := serve(ln)
	s := shareServer(t)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	m := decodeJSON(t, apiReq(t, s, "POST", "u:one", "/v1/shares", `{}`))
	name := m["name"].(string)
	startShareClient(t, srv.URL, m["id"].(string), m["tunnel_token"].(string), addr)
	waitFor(t, func() bool { return s.shares.get(name) != nil })
	host := name + "." + testShareDomain

	if r := visit(t, srv.URL, host, "GET", "/", nil, nil); bodyOf(t, r) != "ok" {
		t.Fatal("first request failed")
	}
	app.CloseClientConnections()
	app.Close()
	ln2, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	app2 := serve(ln2)
	defer app2.Close()
	if r := visit(t, srv.URL, host, "GET", "/", nil, nil); r.StatusCode != 200 || bodyOf(t, r) != "ok" {
		t.Fatalf("after restart = %d", r.StatusCode)
	}
}

func TestLocalAppDownIsA502(t *testing.T) {
	s := shareServer(t)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	m := decodeJSON(t, apiReq(t, s, "POST", "u:one", "/v1/shares", `{}`))
	name := m["name"].(string)
	startShareClient(t, srv.URL, m["id"].(string), m["tunnel_token"].(string), "127.0.0.1:1")
	waitFor(t, func() bool { return s.shares.get(name) != nil })
	r := visit(t, srv.URL, name+"."+testShareDomain, "GET", "/", nil, nil)
	if r.StatusCode != http.StatusBadGateway || !strings.Contains(bodyOf(t, r), "not responding") {
		t.Fatalf("status = %d", r.StatusCode)
	}
}

func TestNamesRouteWhateverTheCaseOrPort(t *testing.T) {
	l := startShare(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	r := visit(t, l.base, strings.ToUpper(l.host())+":443", "GET", "/", nil, nil)
	if r.StatusCode != 200 || bodyOf(t, r) != "ok" {
		t.Fatalf("status = %d", r.StatusCode)
	}
}

func TestWarningPage(t *testing.T) {
	var cookies []string
	l := startShare(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookies = append(cookies, r.Header.Get("Cookie"))
		_, _ = w.Write([]byte("the app"))
	}))
	// A webhook or curl sends no fetch metadata and goes straight through.
	if r := visit(t, l.base, l.host(), "POST", "/hook", strings.NewReader("{}"), nil); bodyOf(t, r) != "the app" {
		t.Fatal("a webhook saw the warning")
	}
	// A browser navigation sees the warning, and the app does not see the request.
	seen := len(cookies)
	r := visit(t, l.base, l.host(), "GET", "/a?b=1", nil, browserNav)
	page := bodyOf(t, r)
	if !strings.Contains(page, "password") || len(cookies) != seen {
		t.Fatalf("no warning: %q", page)
	}
	if r.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatal("the warning page can be framed")
	}
	// Every kind of browser navigation is warned, not only a top-level page.
	for _, dest := range []string{"iframe", "frame", "object", "embed"} {
		if r := visit(t, l.base, l.host(), "GET", "/", nil, http.Header{"Sec-Fetch-Dest": {dest}}); !strings.Contains(bodyOf(t, r), "password") {
			t.Errorf("Sec-Fetch-Dest %q was not warned", dest)
		}
	}
	if len(cookies) != seen {
		t.Fatal("a framed navigation reached the app")
	}
	// The button sets a host-only cookie and returns to the same path.
	ack := visit(t, l.base, l.host(), "POST", shareAckPath, strings.NewReader("next=%2Fa%3Fb%3D1"),
		http.Header{"Content-Type": {"application/x-www-form-urlencoded"}})
	if ack.StatusCode != http.StatusSeeOther || ack.Header.Get("Location") != "/a?b=1" {
		t.Fatalf("ack = %d %q", ack.StatusCode, ack.Header.Get("Location"))
	}
	set := ack.Header.Get("Set-Cookie")
	if !strings.HasPrefix(set, "__Host-"+shareAckCookie+"=") || strings.Contains(strings.ToLower(set), "domain=") {
		t.Fatalf("cookie = %q", set)
	}
	// With the cookie the app is served, and never sees that cookie.
	h := browserNav.Clone()
	h.Set("Cookie", shareAckName()+"=1; mine=keep")
	if r := visit(t, l.base, l.host(), "GET", "/a", nil, h); bodyOf(t, r) != "the app" {
		t.Fatal("still warned after acknowledging")
	}
	if last := cookies[len(cookies)-1]; last != "mine=keep" {
		t.Fatalf("the app saw cookies %q", last)
	}
}

func TestAckRefusesToRedirectOffSite(t *testing.T) {
	l := startShare(t, http.NotFoundHandler())
	for _, next := range []string{"https://evil.example", "//evil.example", "/\\evil.example", "",
		"/\t/evil.example", "/\n/evil.example", "/\r/evil.example", "/\t\\evil.example"} {
		r := visit(t, l.base, l.host(), "POST", shareAckPath, strings.NewReader("next="+url.QueryEscape(next)),
			http.Header{"Content-Type": {"application/x-www-form-urlencoded"}})
		if loc := r.Header.Get("Location"); loc != "/" {
			t.Errorf("next %q redirected to %q", next, loc)
		}
	}
}

func TestEndedAndOfflineLinkPages(t *testing.T) {
	s := shareServer(t)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	m := decodeJSON(t, apiReq(t, s, "POST", "u:one", "/v1/shares", `{}`))
	name, id := m["name"].(string), m["id"].(string)
	host := name + "." + testShareDomain
	if r := visit(t, srv.URL, host, "GET", "/", nil, nil); r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("offline = %d", r.StatusCode)
	}
	if r := visit(t, srv.URL, "nobody-here-aaaaaaaa."+testShareDomain, "GET", "/", nil, nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown = %d", r.StatusCode)
	}
	apiReq(t, s, "DELETE", "u:one", "/v1/shares/"+id, "")
	if r := visit(t, srv.URL, host, "GET", "/", nil, nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("closed = %d", r.StatusCode)
	}
	if _, _, err := s.st.Load().BlockShare(t.Context(), name, s.now().Unix()); err != nil {
		t.Fatal(err)
	}
	if r := visit(t, srv.URL, host, "GET", "/", nil, nil); r.StatusCode != http.StatusGone {
		t.Fatalf("blocked = %d", r.StatusCode)
	}
}

func TestBlockCutsARunningStream(t *testing.T) {
	block := make(chan struct{})
	l := startShare(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("x"))
		w.(http.Flusher).Flush()
		<-block
	}))
	defer close(block)
	resp := visit(t, l.base, l.host(), "GET", "/", nil, nil)
	one := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, one); err != nil {
		t.Fatal(err)
	}
	l.s.shares.end(l.name, "blocked")
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, resp.Body); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream kept running after the block")
	}
}

func TestRequestAndConnectionLimits(t *testing.T) {
	t.Setenv("SHARE_RATE_PER_SEC", "1")
	l := startShare(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	limited := false
	for range 10 {
		r := visit(t, l.base, l.host(), "GET", "/", nil, nil)
		if r.StatusCode == http.StatusTooManyRequests {
			limited = r.Header.Get("Retry-After") != ""
			break
		}
	}
	if !limited {
		t.Fatal("no 429 with Retry-After")
	}
}

func TestConnectionLimitAnswers429(t *testing.T) {
	t.Setenv("SHARE_MAX_CONNS", "1")
	release := make(chan struct{})
	l := startShare(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("x"))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer close(release)
	first := visit(t, l.base, l.host(), "GET", "/", nil, nil)
	one := make([]byte, 1)
	if _, err := io.ReadFull(first.Body, one); err != nil {
		t.Fatal(err)
	}
	if r := visit(t, l.base, l.host(), "GET", "/", nil, nil); r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second connection = %d", r.StatusCode)
	}
}

func TestShareHostNeverReachesTheDashboard(t *testing.T) {
	s := shareServer(t)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	// The bare domain answers only the tunnel and health.
	if r := visit(t, srv.URL, testShareDomain, "GET", "/v1/capabilities", nil, nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("apex API = %d", r.StatusCode)
	}
	if r := visit(t, srv.URL, "a.b."+testShareDomain, "GET", "/", nil, nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("two labels = %d", r.StatusCode)
	}
	// A trailing dot is the same host, and must not fall through to the dashboard.
	if r := visit(t, srv.URL, testShareDomain+".", "GET", "/v1/capabilities", nil, nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("dotted apex API = %d", r.StatusCode)
	}
	if r := visit(t, srv.URL, "nobody-here-aaaaaaaa."+testShareDomain+".", "GET", "/", nil, nil); r.StatusCode != http.StatusNotFound || r.Header.Get("X-Robots-Tag") != "noindex" {
		t.Fatalf("dotted share host = %d", r.StatusCode)
	}
	// A replica that is not the ingress says so.
	t.Setenv("SHARE_INGRESS", "remote")
	srv2 := httptest.NewServer(s.Routes())
	defer srv2.Close()
	if r := visit(t, srv2.URL, "any-name-aaaaaaaa."+testShareDomain, "GET", "/", nil, nil); r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("not the ingress = %d", r.StatusCode)
	}
	// An ordinary host is untouched.
	if r := visit(t, srv.URL, "dashboard.other.example", "GET", "/health", nil, nil); r.StatusCode != 200 {
		t.Fatalf("ordinary host = %d", r.StatusCode)
	}
}

func TestEdgeRoutesServeShares(t *testing.T) {
	s := shareServer(t)
	srv := httptest.NewServer(s.EdgeRoutes())
	defer srv.Close()
	if r := visit(t, srv.URL, testShareDomain, "GET", "/health", nil, nil); r.StatusCode != 200 {
		t.Fatalf("edge apex = %d", r.StatusCode)
	}
}

func TestAckFromAnotherSiteIsRefused(t *testing.T) {
	l := startShare(t, http.NotFoundHandler())
	post := func(site string) *http.Response {
		h := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}
		if site != "" {
			h.Set("Sec-Fetch-Site", site)
		}
		return visit(t, l.base, l.host(), "POST", shareAckPath, strings.NewReader("next=%2Fa"), h)
	}
	for _, site := range []string{"cross-site", "same-site", "none"} {
		r := post(site)
		if r.Header.Get("Set-Cookie") != "" || !strings.Contains(bodyOf(t, r), "password") {
			t.Errorf("Sec-Fetch-Site %q acknowledged the warning", site)
		}
	}
	for _, site := range []string{"same-origin", ""} {
		if r := post(site); r.StatusCode != http.StatusSeeOther || r.Header.Get("Set-Cookie") == "" {
			t.Errorf("Sec-Fetch-Site %q = %d, want the ack", site, r.StatusCode)
		}
	}
}

func TestAckIgnoresMultipartAndOversizedBodies(t *testing.T) {
	l := startShare(t, http.NotFoundHandler())
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("next", "/a")
	_ = mw.Close()
	r := visit(t, l.base, l.host(), "POST", shareAckPath, &body, http.Header{"Content-Type": {mw.FormDataContentType()}})
	if loc := r.Header.Get("Location"); loc != "/" {
		t.Errorf("multipart next was honoured: %q", loc)
	}
	big := strings.NewReader("next=%2Fa&pad=" + strings.Repeat("x", 64<<10))
	r = visit(t, l.base, l.host(), "POST", shareAckPath, big, http.Header{"Content-Type": {"application/x-www-form-urlencoded"}})
	if loc := r.Header.Get("Location"); loc != "/" {
		t.Errorf("an over-limit body was honoured: %q", loc)
	}
}

func TestAppCannotSetTheAckCookie(t *testing.T) {
	l := startShare(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for _, c := range []string{
			"pd_share_ack=1; SameSite=None; Secure",
			"__Host-pd_share_ack=1; Path=/; Secure",
			"__host-PD_SHARE_ACK=1; Path=/; Secure",
			" pd_share_ack =1; Domain=" + testShareDomain,
			"pd_share_ack",
			"mine=keep",
		} {
			w.Header().Add("Set-Cookie", c)
		}
		_, _ = w.Write([]byte("the app"))
	}))
	// An <img> is not a navigation, so it reaches the app without the warning.
	r := visit(t, l.base, l.host(), "GET", "/x", nil, http.Header{"Sec-Fetch-Dest": {"image"}})
	if got := r.Header.Values("Set-Cookie"); len(got) != 1 || got[0] != "mine=keep" {
		t.Fatalf("Set-Cookie reaching the visitor = %q", got)
	}
	h := browserNav.Clone()
	for _, c := range r.Cookies() {
		h.Add("Cookie", c.Name+"="+c.Value)
	}
	if page := bodyOf(t, visit(t, l.base, l.host(), "GET", "/", nil, h)); !strings.Contains(page, "password") {
		t.Fatal("the warning was skipped after the app tried to set the cookie")
	}
}

func TestSharesOnTheSandboxDomainTakeOnlyTheShareHosts(t *testing.T) {
	const domain = "sb.example.com"
	t.Setenv("PORTAL_SANDBOX_DOMAIN", domain)
	s := shareServer(t)
	t.Setenv("SHARE_DOMAIN", domain)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	m := decodeJSON(t, apiReq(t, s, "POST", "u:one", "/v1/shares", `{}`))
	name := m["name"].(string)
	if m["url"] != "https://share-"+name+"."+domain || m["tunnel_url"] != "wss://share."+domain+"/tunnel" {
		t.Fatalf("url = %v, tunnel_url = %v", m["url"], m["tunnel_url"])
	}
	if r := visit(t, srv.URL, "share."+domain, "GET", "/health", nil, nil); r.StatusCode != 200 {
		t.Fatalf("tunnel host health = %d", r.StatusCode)
	}
	if r := visit(t, srv.URL, "share."+domain, "GET", "/v1/capabilities", nil, nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("tunnel host API = %d", r.StatusCode)
	}
	if r := visit(t, srv.URL, "share-"+name+"."+domain, "GET", "/", nil, nil); r.Header.Get("X-Robots-Tag") != "noindex" {
		t.Fatalf("link host = %d, not answered as a share", r.StatusCode)
	}

	// Previews and slugs on the same domain are not the share front's.
	t.Setenv("SHARE_INGRESS", "remote")
	srv2 := httptest.NewServer(s.Routes())
	defer srv2.Close()
	if r := visit(t, srv2.URL, "share-"+name+"."+domain, "GET", "/", nil, nil); r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("link host off the ingress = %d", r.StatusCode)
	}
	for _, host := range []string{"8000-sb1." + domain, "myapp." + domain, "agent-sb1." + domain, domain} {
		if r := visit(t, srv2.URL, host, "GET", "/", nil, nil); r.StatusCode == http.StatusServiceUnavailable {
			t.Fatalf("%s was taken by the share front", host)
		}
	}
}
