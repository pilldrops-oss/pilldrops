package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
)

func TestTerminalWS_RejectsWithoutToken(t *testing.T) {
	tok, hash := sbtoken.Mint()
	srv := httptest.NewServer(terminalMux(newHashStore(hash)))
	defer srv.Close()
	wsURL := "ws" + srv.URL[len("http"):] + "/ws"

	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Fatal("expected dial to fail without a token")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %v", resp)
	}

	c, _, err := websocket.DefaultDialer.Dial(wsURL+"?envd_token="+tok, nil)
	if err != nil {
		t.Fatalf("expected upgrade with a valid token: %v", err)
	}
	c.Close()
}

// When the shell exits, the client must be told. The PTY reader used to just
// return, leaving the handler blocked in ReadMessage on a socket nothing would
// ever write to again: a CLI client sat there until the user typed one more
// key, and could not tell "the shell exited" from "the connection dropped".
func TestTerminalWS_ClosesWhenTheShellExits(t *testing.T) {
	tok, hash := sbtoken.Mint()
	srv := httptest.NewServer(terminalMux(newHashStore(hash)))
	defer srv.Close()

	c, _, err := websocket.DefaultDialer.Dial("ws"+srv.URL[len("http"):]+"/ws?envd_token="+tok, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if err := c.WriteMessage(websocket.BinaryMessage, []byte("exit\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Read until the close frame. A shell that exits must produce one; the
	// deadline is what fails if it does not.
	_ = c.SetReadDeadline(time.Now().Add(15 * time.Second))
	for {
		_, _, err := c.ReadMessage()
		if err == nil {
			continue
		}
		if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
			return
		}
		t.Fatalf("want a normal close frame once the shell exits, got %v", err)
	}
}

// With cmd, /ws is a PTY exec: the command sees a terminal of the asked size in
// the asked directory, and the close frame carries its exit code.
func TestTerminalWS_RunsACommandAndReportsItsExitCode(t *testing.T) {
	tok, hash := sbtoken.Mint()
	srv := httptest.NewServer(terminalMux(newHashStore(hash)))
	defer srv.Close()
	dir := t.TempDir()

	q := url.Values{
		"envd_token": {tok},
		"cmd":        {"test -t 1 && echo tty-yes; stty size; pwd; exit 3"},
		"cwd":        {dir},
		"cols":       {"101"},
		"rows":       {"33"},
	}
	c, _, err := websocket.DefaultDialer.Dial("ws"+srv.URL[len("http"):]+"/ws?"+q.Encode(), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	_ = c.SetReadDeadline(time.Now().Add(15 * time.Second))
	var out strings.Builder
	for {
		_, data, err := c.ReadMessage()
		if err == nil {
			out.Write(data)
			continue
		}
		ce, ok := err.(*websocket.CloseError)
		if !ok {
			t.Fatalf("want a close frame, got %v (output so far %q)", err, out.String())
		}
		if ce.Text != "exit=3" {
			t.Errorf("close reason %q, want exit=3", ce.Text)
		}
		break
	}
	got := out.String()
	for _, want := range []string{"tty-yes", "33 101"} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q lacks %q", got, want)
		}
	}
	// macOS temp dirs sit behind a /private symlink, so compare the tail.
	if !strings.Contains(got, strings.TrimPrefix(dir, "/private")) {
		t.Errorf("output %q does not show the cwd %s", got, dir)
	}
}
