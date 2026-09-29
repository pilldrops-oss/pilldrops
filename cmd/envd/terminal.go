// Built-in web terminal: GET / serves the embedded xterm.js page, GET /ws
// bridges a WebSocket to a login-shell PTY. Failure to start is never fatal.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"

	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
)

const (
	// defaultTerminalPort must match the runtimes' TerminalTarget() default.
	defaultTerminalPort = 7681
)

// terminalPort reads PD_TERMINAL_PORT, falling back to defaultTerminalPort.
func terminalPort() int {
	if v := os.Getenv("PD_TERMINAL_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 && p < 65536 {
			return p
		}
	}
	return defaultTerminalPort
}

// startTerminalServer serves the web terminal in a goroutine; bind failures are
// logged, not fatal. Same rule as envdListenAddr: a routable bind needs a
// credential or a live channel to deliver one, otherwise loopback only.
func startTerminalServer(hashes *hashStore, credChannel bool) {
	port := terminalPort()
	addr := fmt.Sprintf("0.0.0.0:%d", port)
	if hashes.Get() == "" && !credChannel {
		addr = fmt.Sprintf("127.0.0.1:%d", port)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           terminalMux(hashes),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		fmt.Printf("envd: web terminal listening on %s\n", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "envd: web terminal server stopped: %v\n", err)
		}
	}()
}

// terminalMux builds the web-terminal routes.
func terminalMux(hashes *hashStore) *http.ServeMux {
	mux := http.NewServeMux()
	// Assets match on their last path segment so they resolve both at a
	// subdomain root and under the dashboard's path-prefixed preview.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if handleTerminalAsset(w, r) {
			return
		}
		handleTerminalPage(w, r)
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		// Read per request so a credential pushed after a snapshot restore unlocks the terminal.
		handleTerminalWS(w, r, hashes.Get())
	})
	return mux
}

// authorizedTerminal reports whether r carries a token that hashes to hash.
// The envd_token query param exists because browsers cannot set headers on a
// WebSocket handshake; it is distinct from the preview proxy's ?token=.
func authorizedTerminal(r *http.Request, hash string) bool {
	tok := r.Header.Get(sbtoken.MetadataKey)
	if tok == "" {
		tok = r.URL.Query().Get("envd_token")
	}
	return sbtoken.Verify(tok, hash)
}

// upgrader's CheckOrigin is defense-in-depth; the token check in
// authorizedTerminal runs before Upgrade and is the primary control.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		o := r.Header.Get("Origin")
		if o == "" {
			return true
		}
		u, err := url.Parse(o)
		return err == nil && u.Host == r.Host
	},
}

// resizeMsg is the JSON control frame the browser sends on terminal resize.
type resizeMsg struct {
	Type string `json:"type"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// handleTerminalWS authorizes against hash, then bridges a WebSocket to a
// login-shell PTY. The shell dies with the socket, so a reconnect starts fresh.
//
// Query parameters make it a PTY exec (envdproto.PtyCommand): cmd runs that
// command under the login shell instead of an interactive one, cwd sets its
// directory, cols and rows its first window size. The close frame's reason is
// "exit=<code>" once the process has exited, so a client can tell a command
// that finished from a connection that dropped.
func handleTerminalWS(w http.ResponseWriter, r *http.Request, hash string) {
	if !authorizedTerminal(r, hash) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already wrote the error response.
	}
	defer func() { _ = ws.Close() }()

	shell := "/bin/bash"
	if _, statErr := os.Stat(shell); statErr != nil {
		shell = "/bin/sh"
	}
	q := r.URL.Query()
	cmd := exec.Command(shell, "-l")
	if c := q.Get("cmd"); c != "" {
		cmd = exec.Command(shell, "-lc", c)
	}
	// Unset means envd's own directory, as before: on Docker envd runs as the
	// sandbox user, who cannot enter /root.
	cmd.Dir = q.Get("cwd")
	cmd.Env = append(buildSessionEnv(nil), "TERM=xterm-256color")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		_ = ws.WriteMessage(websocket.TextMessage, []byte("failed to start shell: "+err.Error()+"\r\n"))
		return
	}

	var closeOnce sync.Once
	cleanup := func() {
		closeOnce.Do(func() {
			_ = ptmx.Close()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
		})
	}
	defer cleanup()

	_ = pty.Setsize(ptmx, &pty.Winsize{Cols: querySize(q.Get("cols"), 80), Rows: querySize(q.Get("rows"), 24)})

	// PTY -> ws.
	go func() {
		// Closing the PTY is not enough: the handler below is parked in
		// ReadMessage and nothing will ever write to this socket again, so a
		// client would hang until its user typed one more key — and could not
		// tell a shell that exited from a connection that dropped. The close
		// frame says which. WriteControl is the one write safe to make while
		// another goroutine may be writing.
		defer func() {
			cleanup()
			reason := ""
			if cmd.ProcessState != nil {
				reason = "exit=" + strconv.Itoa(cmd.ProcessState.ExitCode())
			}
			_ = ws.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, reason),
				time.Now().Add(2*time.Second))
			_ = ws.Close()
		}()
		buf := make([]byte, 4096)
		for {
			n, rerr := ptmx.Read(buf)
			if n > 0 {
				if werr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	// ws -> PTY. Text frames that parse as a resize message adjust the window;
	// everything else is raw input.
	for {
		mt, data, rerr := ws.ReadMessage()
		if rerr != nil {
			return
		}
		if mt == websocket.TextMessage {
			var m resizeMsg
			if json.Unmarshal(data, &m) == nil && m.Type == "resize" && m.Cols > 0 && m.Rows > 0 {
				_ = pty.Setsize(ptmx, &pty.Winsize{Cols: m.Cols, Rows: m.Rows})
				continue
			}
		}
		if _, werr := io.Copy(ptmx, bytes.NewReader(data)); werr != nil {
			return
		}
	}
}

// querySize reads a window dimension, falling back to def for anything that is
// not a positive number a terminal could have.
func querySize(v string, def uint16) uint16 {
	n, err := strconv.ParseUint(v, 10, 16)
	if err != nil || n == 0 || n > 1000 {
		return def
	}
	return uint16(n)
}

// handleTerminalPage serves the terminal page; the CSP enforces that every
// asset comes from this binary and no other host.
func handleTerminalPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Header as well as <meta>, so a body-rewriting proxy cannot drop the policy.
	w.Header().Set("Content-Security-Policy", terminalCSP(terminalBootstrapJS))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	_, _ = io.WriteString(w, terminalHTML)
}

// terminalBootstrapJS is the page's only inline script; terminalCSP hashes
// these exact bytes into script-src. It derives asset URLs from
// location.pathname because a static relative src resolves one segment too
// high on a path-prefixed preview URL without a trailing slash.
const terminalBootstrapJS = `
(function () {
  var base = location.pathname.replace(/\/+$/, '');
  function assetURL(n) { return base + '/' + n; }

  var link = document.createElement('link');
  link.rel = 'stylesheet';
  link.href = assetURL('xterm.css');
  document.head.appendChild(link);

  // Sequential, not parallel: addon-fit and addon-attach are UMD bundles that
  // register against the Terminal global xterm.js defines.
  function load(names, done) {
    if (!names.length) { done(); return; }
    var s = document.createElement('script');
    s.src = assetURL(names[0]);
    s.onload = function () { load(names.slice(1), done); };
    s.onerror = function () {
      document.getElementById('terminal').textContent =
        'terminal assets failed to load: ' + s.src;
    };
    document.head.appendChild(s);
  }
  load(['xterm.js', 'addon-fit.js', 'addon-attach.js'], start);

  function start() {
    var term = new Terminal({
      cursorBlink: true,
      fontFamily: 'Menlo, Monaco, "Courier New", monospace',
      fontSize: 14,
      theme: { background: '#000000', foreground: '#e6e6e6' }
    });
    var fit = new FitAddon.FitAddon();
    term.loadAddon(fit);
    term.open(document.getElementById('terminal'));
    fit.fit();

    // Build the /ws URL RELATIVE to the page's current path so the terminal works
    // both at a subdomain root (8000-sb-<id>.<domain>/ → /ws) and under the
    // dashboard's path-prefixed preview (/preview/<id>/<port>/ → /preview/<id>/<port>/ws).
    // An absolute "/ws" would escape the /preview/... prefix and hit the wrong origin.
    // Carry the page's own query string onto the socket URL: previews are
    // private by default, so the ?token=… that authorised THIS page is also
    // required for /ws. Dropping it made the terminal render and then hang with a
    // silent 403 on the upgrade — the page looked broken with no error shown.
    var proto = location.protocol === 'https:' ? 'wss' : 'ws';
    var ws = new WebSocket(proto + '://' + location.host + base + '/ws' + location.search);
    ws.binaryType = 'arraybuffer';

    function sendResize() {
      fit.fit();
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }));
      }
    }

    ws.onopen = function () {
      var attach = new AttachAddon.AttachAddon(ws);
      term.loadAddon(attach);
      sendResize();
      term.focus();
    };
    // Distinguish "never connected" from "disconnected mid-session": a handshake
    // the proxy refuses (e.g. a preview token that is missing or expired) closes
    // without ever opening, and a bare "[connection closed]" gave no clue why.
    var opened = false;
    ws.addEventListener('open', function () { opened = true; });
    ws.onclose = function () {
      if (opened) {
        term.write('\r\n\x1b[31m[connection closed]\x1b[0m\r\n');
        return;
      }
      term.write('\r\n\x1b[31m[could not connect]\x1b[0m ' +
        'the terminal socket was refused.\r\n' +
        (location.search.indexOf('token=') === -1
          ? 'This preview is private — open it from the dashboard so the access token is included.\r\n'
          : 'The access token may have expired — reopen the sandbox from the dashboard.\r\n'));
    };

    window.addEventListener('resize', sendResize);
  }
})();
`

// terminalHTML embeds terminalBootstrapJS byte-for-byte; a template that
// re-indents would break the CSP hash and blank the terminal.
var terminalHTML = buildTerminalHTML()

func buildTerminalHTML() string {
	return `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<meta http-equiv="Content-Security-Policy" content="` + terminalCSP(terminalBootstrapJS) + `" />
<title>Terminal</title>
<style>
  html, body { margin: 0; padding: 0; height: 100%; background: #000; }
  #terminal { position: absolute; inset: 0; padding: 4px; box-sizing: border-box; }
</style>
</head>
<body>
<div id="terminal"></div>
<script>` + terminalBootstrapJS + `</script>
</body>
</html>
`
}
