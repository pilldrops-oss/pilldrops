package agentd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func newTestServer(t *testing.T, script func(*Turn) string) (*httptest.Server, *Supervisor) {
	t.Helper()
	sup := newTestSupervisor(t, script)
	srv := &Server{
		Sup: sup, Journal: sup.Journal, Queue: sup.Queue,
		TokenHash: sbtoken.HashHex(testToken),
		KeepAlive: 50 * time.Millisecond,
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, sup
}

// newServerFor wraps a Server the test configured itself.
func newServerFor(t *testing.T, srv *Server) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func req(t *testing.T, ts *httptest.Server, method, path, body, token string) (*http.Response, map[string]any) {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	r, err := http.NewRequest(method, ts.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		r.Header.Set(sbtoken.MetadataKey, token)
	}
	resp, err := ts.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

// The agent itself can reach loopback, so the token is the boundary.
func TestEveryStatefulRouteRequiresTheAccessToken(t *testing.T) {
	ts, _ := newTestServer(t, func(*Turn) string { return `true` })

	for _, c := range []struct{ method, path string }{
		{"GET", "/status"},
		{"GET", "/snapshot-safe"},
		{"POST", "/turns"},
		{"GET", "/turns"},
		{"GET", "/turns/turn_x"},
		{"POST", "/turns/turn_x/interrupt"},
		{"DELETE", "/turns/turn_x"},
		{"POST", "/queue/reorder"},
		{"GET", "/events"},
	} {
		resp, _ := req(t, ts, c.method, c.path, `{}`, "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without a token = %d, want 401", c.method, c.path, resp.StatusCode)
		}
		resp, _ = req(t, ts, c.method, c.path, `{}`, "wrong-token")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s with a wrong token = %d, want 401", c.method, c.path, resp.StatusCode)
		}
	}
}

func TestHealthIsReachableWithoutAToken(t *testing.T) {
	ts, _ := newTestServer(t, func(*Turn) string { return `true` })
	resp, body := req(t, ts, "GET", "/health", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health = %d, want 200", resp.StatusCode)
	}
	if body["ok"] != true {
		t.Errorf("health body = %v", body)
	}
}

func TestSubmitReturns202WithACursor(t *testing.T) {
	ts, sup := newTestServer(t, func(*Turn) string { return `printf 'ok'` })
	stop := runLoop(t, sup)
	defer stop()

	resp, body := req(t, ts, "POST", "/turns", `{"message":"do it","mode":"autonomous","source":"api"}`, testToken)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("submit = %d, want 202", resp.StatusCode)
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("no turn id in %v", body)
	}
	if body["seq_start"] == nil {
		t.Error("no seq_start; a client has no cursor to stream the turn from")
	}
}

func TestSubmitRejectsAnEmptyMessage(t *testing.T) {
	ts, _ := newTestServer(t, func(*Turn) string { return `true` })
	for _, body := range []string{`{}`, `{"message":""}`, `not json`} {
		resp, _ := req(t, ts, "POST", "/turns", body, testToken)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("submit %s = %d, want 400", body, resp.StatusCode)
		}
	}
}

func TestFullQueueReturns429WithRetryAfter(t *testing.T) {
	ts, sup := newTestServer(t, func(*Turn) string { return `sleep 5` })
	sup.Queue.maxPending = 1

	if resp, _ := req(t, ts, "POST", "/turns", `{"message":"one"}`, testToken); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first submit = %d", resp.StatusCode)
	}
	resp, body := req(t, ts, "POST", "/turns", `{"message":"two"}`, testToken)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("submit past the cap = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After on a 429")
	}
	if msg, _ := body["error"].(string); msg == "" {
		t.Error("429 carries no explanation")
	}
}

func TestEventStreamResumesFromTheCursor(t *testing.T) {
	ts, sup := newTestServer(t, func(*Turn) string { return `true` })

	for i := 0; i < 3; i++ {
		if _, err := sup.Journal.Append(EventTurnOutput, "turn_1", map[string]any{"i": i}); err != nil {
			t.Fatal(err)
		}
	}

	seqs := readSSE(t, ts, "/events?since_seq=2", 1)
	if len(seqs) != 1 || seqs[0] != 3 {
		t.Errorf("since_seq=2 delivered %v, want just [3]", seqs)
	}
}

func TestLastEventIDOverridesTheQueryCursor(t *testing.T) {
	ts, sup := newTestServer(t, func(*Turn) string { return `true` })
	for i := 0; i < 4; i++ {
		if _, err := sup.Journal.Append(EventTurnOutput, "turn_1", map[string]any{"i": i}); err != nil {
			t.Fatal(err)
		}
	}

	r, err := http.NewRequest("GET", ts.URL+"/events?since_seq=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set(sbtoken.MetadataKey, testToken)
	r.Header.Set("Last-Event-ID", "3")
	resp, err := ts.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	seqs := scanSSESeqs(t, resp, 1)
	if len(seqs) != 1 || seqs[0] != 4 {
		t.Errorf("Last-Event-ID=3 delivered %v, want just [4]", seqs)
	}
}

func TestEventStreamDeliversLiveEvents(t *testing.T) {
	ts, sup := newTestServer(t, func(*Turn) string { return `true` })
	head := sup.Journal.LastSeq()

	r, err := http.NewRequest("GET", ts.URL+"/events?since_seq="+strconv.FormatUint(head, 10), nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set(sbtoken.MetadataKey, testToken)
	resp, err := ts.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = sup.Journal.Append(EventTurnFinished, "turn_live", map[string]any{"exit_code": 0})
	}()

	seqs := scanSSESeqs(t, resp, 1)
	if len(seqs) != 1 || seqs[0] != head+1 {
		t.Errorf("live stream delivered %v, want [%d]", seqs, head+1)
	}
}

func TestIdleEventStreamSendsKeepAlives(t *testing.T) {
	ts, sup := newTestServer(t, func(*Turn) string { return `true` })
	head := sup.Journal.LastSeq()

	r, err := http.NewRequest("GET", ts.URL+"/events?since_seq="+strconv.FormatUint(head, 10), nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set(sbtoken.MetadataKey, testToken)
	resp, err := ts.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	deadline := time.Now().Add(5 * time.Second)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), ":") {
			return
		}
		if time.Now().After(deadline) {
			break
		}
	}
	t.Error("no keep-alive comment on an idle stream")
}

func TestInterruptWithNothingRunningIs409(t *testing.T) {
	ts, _ := newTestServer(t, func(*Turn) string { return `true` })
	resp, body := req(t, ts, "POST", "/turns/turn_missing/interrupt", `{}`, testToken)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("interrupt with nothing running = %d, want 409", resp.StatusCode)
	}
	if msg, _ := body["error"].(string); msg == "" {
		t.Error("409 carries no explanation")
	}
}

func TestDeletingARunningTurnIs409AndNamesTheRightVerb(t *testing.T) {
	ts, sup := newTestServer(t, func(*Turn) string { return `sleep 5` })
	stop := runLoop(t, sup)
	defer stop()

	_, body := req(t, ts, "POST", "/turns", `{"message":"long"}`, testToken)
	id, _ := body["id"].(string)
	waitFor(t, "the turn to start", func() bool { return sup.Runner.RunningTurn() == id })

	resp, errBody := req(t, ts, "DELETE", "/turns/"+id, "", testToken)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("deleting a running turn = %d, want 409", resp.StatusCode)
	}
	if msg, _ := errBody["error"].(string); !strings.Contains(msg, "interrupt") {
		t.Errorf("refusal %q does not say to interrupt instead", msg)
	}
	_ = sup.Interrupt(id, 100*time.Millisecond)
}

func TestSnapshotSafeReflectsWhetherATurnIsRunning(t *testing.T) {
	ts, sup := newTestServer(t, func(*Turn) string { return `sleep 5` })
	stop := runLoop(t, sup)
	defer stop()

	_, body := req(t, ts, "GET", "/snapshot-safe", "", testToken)
	if body["safe"] != true {
		t.Errorf("idle workstation reported unsafe: %v", body)
	}

	_, sub := req(t, ts, "POST", "/turns", `{"message":"long"}`, testToken)
	id, _ := sub["id"].(string)
	waitFor(t, "the turn to start", func() bool { return sup.Runner.Busy() })

	_, body = req(t, ts, "GET", "/snapshot-safe", "", testToken)
	if body["safe"] != false {
		t.Errorf("running workstation reported safe to snapshot: %v", body)
	}
	if reason, _ := body["reason"].(string); reason == "" {
		t.Error("refusal carries no reason")
	}
	_ = sup.Interrupt(id, 100*time.Millisecond)
}

func TestStatusAndTurnsAreServed(t *testing.T) {
	ts, sup := newTestServer(t, func(*Turn) string { return `printf 'ok'` })
	stop := runLoop(t, sup)
	defer stop()

	_, sub := req(t, ts, "POST", "/turns", `{"message":"hello","source":"api"}`, testToken)
	id, _ := sub["id"].(string)
	waitFor(t, "the turn to finish", func() bool {
		got, err := sup.Queue.Get(id)
		return err == nil && got.State == TurnDone
	})

	_, st := req(t, ts, "GET", "/status", "", testToken)
	if st["mode"] != string(ModeRich) {
		t.Errorf("status mode = %v, want %q", st["mode"], ModeRich)
	}
	if st["last_seq"] == nil {
		t.Error("status has no last_seq cursor")
	}

	resp, list := req(t, ts, "GET", "/turns", "", testToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list turns = %d", resp.StatusCode)
	}
	turns, _ := list["turns"].([]any)
	if len(turns) != 1 {
		t.Errorf("listed %d turns, want 1", len(turns))
	}

	resp, one := req(t, ts, "GET", "/turns/"+id, "", testToken)
	if resp.StatusCode != http.StatusOK || one["id"] != id {
		t.Errorf("get turn = %d %v", resp.StatusCode, one)
	}
	if resp, _ := req(t, ts, "GET", "/turns/turn_nope", "", testToken); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown turn = %d, want 404", resp.StatusCode)
	}
}

// readSSE opens a stream and returns the first want event sequence numbers.
func readSSE(t *testing.T, ts *httptest.Server, path string, want int) []uint64 {
	t.Helper()
	r, err := http.NewRequest("GET", ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set(sbtoken.MetadataKey, testToken)
	resp, err := ts.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return scanSSESeqs(t, resp, want)
}

// scanSSESeqs reads id lines until it has want of them or the read stalls.
func scanSSESeqs(t *testing.T, resp *http.Response, want int) []uint64 {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type = %q, want text/event-stream", ct)
	}
	type result struct{ seqs []uint64 }
	done := make(chan result, 1)
	go func() {
		var seqs []uint64
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "id: ") {
				continue
			}
			var n uint64
			for _, c := range line[4:] {
				if c < '0' || c > '9' {
					break
				}
				n = n*10 + uint64(c-'0')
			}
			seqs = append(seqs, n)
			if len(seqs) >= want {
				break
			}
		}
		done <- result{seqs}
	}()

	select {
	case r := <-done:
		return r.seqs
	case <-time.After(10 * time.Second):
		t.Fatal("timed out reading the event stream")
		return nil
	}
}

func TestEventStreamStopsWhenTheClientDisconnects(t *testing.T) {
	sup := newTestSupervisor(t, func(*Turn) string { return `true` })
	srv := &Server{Sup: sup, Journal: sup.Journal, Queue: sup.Queue, KeepAlive: 20 * time.Millisecond}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	r, err := http.NewRequestWithContext(ctx, "GET", ts.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ts.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	resp.Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	for {
		sup.Journal.mu.Lock()
		n := len(sup.Journal.subs)
		sup.Journal.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d journal subscriptions leaked after the client disconnected", n)
		}
		// Appends keep the handler's loop cycling so it notices the closed context.
		_, _ = sup.Journal.Append(EventTurnOutput, "turn_1", map[string]any{"x": 1})
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRefusesToBindARoutableAddressWithoutAToken(t *testing.T) {
	sup := newTestSupervisor(t, func(*Turn) string { return `true` })

	for _, addr := range []string{
		"0.0.0.0:0",
		":0",
		"[::]:0",
		"192.0.2.7:0",
	} {
		srv := &Server{Sup: sup, Journal: sup.Journal, Queue: sup.Queue, Addr: addr}
		if _, _, err := srv.Listen(); !errors.Is(err, ErrExposedWithoutAuth) {
			t.Errorf("Listen(%q) with no token = %v, want ErrExposedWithoutAuth", addr, err)
		}
	}
}

func TestLoopbackMayBindWithoutATokenAndRoutableMayBindWithOne(t *testing.T) {
	sup := newTestSupervisor(t, func(*Turn) string { return `true` })

	for _, addr := range []string{"127.0.0.1:0", "localhost:0", "[::1]:0"} {
		srv := &Server{Sup: sup, Journal: sup.Journal, Queue: sup.Queue, Addr: addr}
		ln, _, err := srv.Listen()
		if err != nil {
			t.Errorf("Listen(%q) on loopback without a token: %v", addr, err)
			continue
		}
		ln.Close()
	}

	srv := &Server{
		Sup: sup, Journal: sup.Journal, Queue: sup.Queue,
		Addr: "127.0.0.1:0", TokenHash: sbtoken.HashHex(testToken),
	}
	ln, _, err := srv.Listen()
	if err != nil {
		t.Fatalf("Listen with a token: %v", err)
	}
	ln.Close()
}

func TestSubmitRejectsWhitespaceOnlyMessage(t *testing.T) {
	ts, _ := newTestServer(t, func(*Turn) string { return `true` })
	for _, body := range []string{`{"message":"   "}`, `{"message":"\t\n"}`} {
		resp, _ := req(t, ts, "POST", "/turns", body, testToken)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("submit %s = %d, want 400", body, resp.StatusCode)
		}
	}
}
