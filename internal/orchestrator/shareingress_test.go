package orchestrator

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const testShareDomain = "share.example.com"

// shareClient speaks the CLI's side of the protocol against a local address.
type shareClient struct {
	ctrl   *websocket.Conn
	closed chan string // the reason in a "closed" message, or "" when the socket dropped
}

func dialShareWS(t *testing.T, base, path string, h http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	if h == nil {
		h = http.Header{}
	}
	h.Set("Host", testShareDomain)
	return websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http")+path, h)
}

// startShareClient connects a tunnel for id, piping data connections to local.
func startShareClient(t *testing.T, base, id, token, local string) *shareClient {
	t.Helper()
	h := http.Header{"Authorization": {"Bearer " + token}}
	ctrl, resp, err := dialShareWS(t, base, "/tunnel?share="+id, h)
	if err != nil {
		t.Fatalf("control dial: %v (%v)", err, resp)
	}
	_, portS, _ := net.SplitHostPort(local)
	port, _ := strconv.Atoi(portS)
	if err := ctrl.WriteJSON(map[string]any{"type": "hello", "port": port}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	c := &shareClient{ctrl: ctrl, closed: make(chan string, 1)}
	t.Cleanup(func() { _ = ctrl.Close() })
	go func() {
		for {
			var msg struct{ Type, ID, Reason string }
			if err := ctrl.ReadJSON(&msg); err != nil {
				c.closed <- ""
				return
			}
			switch msg.Type {
			case "closed":
				c.closed <- msg.Reason
				return
			case "open":
				go pipeShareConn(t, base, msg.ID, local)
			}
		}
	}()
	return c
}

func pipeShareConn(t *testing.T, base, id, local string) {
	ws, _, err := dialShareWS(t, base, "/tunnel/conn/"+id, nil)
	if err != nil {
		return
	}
	remote := &wsConn{ws: ws}
	defer remote.Close()
	app, err := net.Dial("tcp", local)
	if err != nil {
		return
	}
	defer app.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(app, remote); done <- struct{}{} }()
	go func() { _, _ = io.Copy(remote, app); done <- struct{}{} }()
	<-done
}

// visit sends a request to a share's public host.
func visit(t *testing.T, base, host, method, path string, body io.Reader, h http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, base+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	for k, v := range h {
		req.Header[k] = v
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("visit: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

type liveShare struct {
	s          *Server
	base, name string
	id, token  string
	client     *shareClient
}

// startShare runs a real ingress with one connected share pointing at app.
func startShare(t *testing.T, app http.Handler) *liveShare {
	t.Helper()
	s := shareServer(t)
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	local := httptest.NewServer(app)
	t.Cleanup(local.Close)
	m := decodeJSON(t, apiReq(t, s, "POST", "u:one", "/v1/shares", `{}`))
	l := &liveShare{s: s, base: srv.URL, name: m["name"].(string), id: m["id"].(string), token: m["tunnel_token"].(string)}
	l.client = startShareClient(t, srv.URL, l.id, l.token, strings.TrimPrefix(local.URL, "http://"))
	waitFor(t, func() bool { return s.shares.get(l.name) != nil })
	return l
}

func (l *liveShare) host() string { return l.name + "." + testShareDomain }

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestControlRefusesABadToken(t *testing.T) {
	s := shareServer(t)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	id := decodeJSON(t, apiReq(t, s, "POST", "u:one", "/v1/shares", `{}`))["id"].(string)
	_, resp, err := dialShareWS(t, srv.URL, "/tunnel?share="+id, http.Header{"Authorization": {"Bearer nope"}})
	if err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("dial = %v %v", err, resp)
	}
}

func TestControlRefusesAnEndedShare(t *testing.T) {
	s := shareServer(t)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	m := decodeJSON(t, apiReq(t, s, "POST", "u:one", "/v1/shares", `{}`))
	id, tok := m["id"].(string), m["tunnel_token"].(string)
	apiReq(t, s, "DELETE", "u:one", "/v1/shares/"+id, "")
	_, resp, err := dialShareWS(t, srv.URL, "/tunnel?share="+id, http.Header{"Authorization": {"Bearer " + tok}})
	if err == nil || resp.StatusCode != http.StatusGone {
		t.Fatalf("dial = %v %v", err, resp)
	}
}

func TestDataConnRefusesAnUnknownID(t *testing.T) {
	l := startShare(t, http.NotFoundHandler())
	_, resp, err := dialShareWS(t, l.base, "/tunnel/conn/"+strings.Repeat("0", 32), nil)
	if err == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("dial = %v %v", err, resp)
	}
}

func TestOpenIDIsSingleUse(t *testing.T) {
	s := shareServer(t)
	ch := make(chan net.Conn, 1)
	id := s.shares.addPending(ch)
	if got := s.shares.takePending(id); got == nil {
		t.Fatal("first take failed")
	}
	if got := s.shares.takePending(id); got != nil {
		t.Fatal("an id was accepted twice")
	}
}

func TestDialTimesOutWhenTheClientNeverAnswers(t *testing.T) {
	old := shareDialTimeout
	shareDialTimeout = 50 * time.Millisecond
	defer func() { shareDialTimeout = old }()
	s := shareServer(t)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	m := decodeJSON(t, apiReq(t, s, "POST", "u:one", "/v1/shares", `{}`))
	name := m["name"].(string)
	// A client that says hello and then ignores every open.
	ctrl, _, err := dialShareWS(t, srv.URL, "/tunnel?share="+m["id"].(string),
		http.Header{"Authorization": {"Bearer " + m["tunnel_token"].(string)}})
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.Close()
	_ = ctrl.WriteJSON(map[string]any{"type": "hello", "port": 3000})
	go func() {
		for {
			if _, _, err := ctrl.ReadMessage(); err != nil {
				return
			}
		}
	}()
	waitFor(t, func() bool { return s.shares.get(name) != nil })
	if _, err := s.shares.get(name).dial(t.Context()); err == nil {
		t.Fatal("dial succeeded with nobody answering")
	}
}

func TestSharesStopWithinTheRefreshWhenClosedElsewhere(t *testing.T) {
	old := shareRefreshEvery
	shareRefreshEvery = 20 * time.Millisecond
	defer func() { shareRefreshEvery = old }()
	l := startShare(t, http.NotFoundHandler())
	// Written straight to the store, as an API replica would: nothing here calls end.
	if _, _, err := l.s.st.Load().BlockShare(t.Context(), l.name, l.s.now().Unix()); err != nil {
		t.Fatal(err)
	}
	select {
	case reason := <-l.client.closed:
		if reason != "blocked" {
			t.Fatalf("reason = %q", reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tunnel kept running")
	}
	waitFor(t, func() bool { return l.s.shares.get(l.name) == nil })
}

func TestBytesAreRecordedWhenTheTunnelEnds(t *testing.T) {
	l := startShare(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 1000)))
	}))
	resp := visit(t, l.base, l.host(), "GET", "/", nil, nil)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = l.client.ctrl.Close()
	waitFor(t, func() bool {
		sh, _, _ := l.s.st.Load().ShareByID(t.Context(), l.id)
		return sh.BytesOut >= 1000 && sh.BytesIn > 0
	})
}

func TestAReconnectReplacesTheOldTunnel(t *testing.T) {
	l := startShare(t, http.NotFoundHandler())
	first := l.s.shares.get(l.name)
	tok := l.s.mintShareToken(l.id)
	startShareClient(t, l.base, l.id, tok, "127.0.0.1:1")
	waitFor(t, func() bool { return l.s.shares.get(l.name) != first && l.s.shares.get(l.name) != nil })
}

func TestConcurrentDialsRespectTheConnectionLimit(t *testing.T) {
	old := shareDialTimeout
	shareDialTimeout = 500 * time.Millisecond
	defer func() { shareDialTimeout = old }()
	t.Setenv("SHARE_MAX_CONNS", "2")
	s := shareServer(t)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	m := decodeJSON(t, apiReq(t, s, "POST", "u:one", "/v1/shares", `{}`))
	name := m["name"].(string)
	ctrl, _, err := dialShareWS(t, srv.URL, "/tunnel?share="+m["id"].(string),
		http.Header{"Authorization": {"Bearer " + m["tunnel_token"].(string)}})
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.Close()
	_ = ctrl.WriteJSON(map[string]any{"type": "hello", "port": 3000})
	go func() {
		for {
			if _, _, err := ctrl.ReadMessage(); err != nil {
				return
			}
		}
	}()
	waitFor(t, func() bool { return s.shares.get(name) != nil })
	tun := s.shares.get(name)

	var busy atomic.Int32
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := tun.dial(t.Context()); errors.Is(err, errShareBusy) {
				busy.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := busy.Load(); got != 4 {
		t.Fatalf("%d dials were refused as busy, want 4 of 6 with a limit of 2", got)
	}
	if n := tun.open.Load(); n != 0 {
		t.Fatalf("open = %d after every dial failed", n)
	}
}

func TestControlEnforcesTheOrgLimitOnConnect(t *testing.T) {
	s := shareServer(t)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	t.Setenv("SHARE_MAX_PER_ORG", "2")
	a := decodeJSON(t, apiReq(t, s, "POST", "u:one", "/v1/shares", `{}`))
	b := decodeJSON(t, apiReq(t, s, "POST", "u:one", "/v1/shares", `{}`))
	startShareClient(t, srv.URL, a["id"].(string), a["tunnel_token"].(string), "127.0.0.1:1")
	waitFor(t, func() bool { return s.shares.get(a["name"].(string)) != nil })
	// As if a and b were created while unconnected shares had stopped counting.
	t.Setenv("SHARE_MAX_PER_ORG", "1")
	_, resp, err := dialShareWS(t, srv.URL, "/tunnel?share="+b["id"].(string),
		http.Header{"Authorization": {"Bearer " + b["tunnel_token"].(string)}})
	if err == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over the limit = %v %v", err, resp)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["code"] != "share_limit" {
		t.Fatalf("code = %v", body["code"])
	}
	first := s.shares.get(a["name"].(string))
	startShareClient(t, srv.URL, a["id"].(string), s.mintShareToken(a["id"].(string)), "127.0.0.1:1")
	waitFor(t, func() bool { return s.shares.get(a["name"].(string)) != first })
	t.Setenv("SHARE_MAX_PER_ORG", "0")
	startShareClient(t, srv.URL, b["id"].(string), b["tunnel_token"].(string), "127.0.0.1:1")
	waitFor(t, func() bool { return s.shares.get(b["name"].(string)) != nil })
}

func TestALiveTunnelEndsAtExpiry(t *testing.T) {
	old := shareRefreshEvery
	shareRefreshEvery = 20 * time.Millisecond
	defer func() { shareRefreshEvery = old }()
	s := shareServer(t)
	var skew atomic.Int64
	s.now = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	local := httptest.NewServer(http.NotFoundHandler())
	defer local.Close()
	m := decodeJSON(t, apiReq(t, s, "POST", "u:one", "/v1/shares", `{"ttl_seconds":60}`))
	name := m["name"].(string)
	c := startShareClient(t, srv.URL, m["id"].(string), m["tunnel_token"].(string), strings.TrimPrefix(local.URL, "http://"))
	waitFor(t, func() bool { return s.shares.get(name) != nil })
	skew.Store(int64(time.Minute))
	select {
	case reason := <-c.closed:
		if reason != "expired" {
			t.Fatalf("reason = %q", reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tunnel kept running past its expiry")
	}
	waitFor(t, func() bool { return s.shares.get(name) == nil })
	if r := visit(t, srv.URL, name+"."+testShareDomain, "GET", "/", nil, nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("link after expiry = %d", r.StatusCode)
	}
}
