package orchestrator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// A pasted command lands in shell history.
func TestBootstrapCommandDoesNotCarryTheToken(t *testing.T) {
	t.Setenv("NODE_BOOTSTRAP_DSN", "postgres://u:p@h:5432/db")
	t.Setenv("NODE_BOOTSTRAP_SECRET", "sekret")
	s := NewServer("op-key", "nodetoken")
	req := httptest.NewRequest(http.MethodPost, "/v1/join-tokens", nil)
	req.Header.Set("Authorization", "Bearer op-key")
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", w.Code, w.Body.String())
	}
	var got struct {
		Token        string `json:"token"`
		BootstrapCmd string `json:"bootstrap_cmd"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Token == "" {
		t.Fatal("no token minted")
	}
	if strings.Contains(got.BootstrapCmd, got.Token) {
		t.Errorf("bootstrap_cmd embeds the live token:\n  %s", got.BootstrapCmd)
	}
	if strings.Contains(got.BootstrapCmd, "JOIN_TOKEN=") {
		t.Errorf("bootstrap_cmd still sets JOIN_TOKEN on a command line:\n  %s", got.BootstrapCmd)
	}
}

// A token on a command line is world-readable in /proc/<pid>/cmdline.
func TestTheServedInstallerKeepsTheTokenOffCommandLines(t *testing.T) {
	s := NewServer("op-key", "nodetoken")
	req := httptest.NewRequest(http.MethodGet, "/install.sh", nil)
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	script := w.Body.String()

	if regexp.MustCompile(`(?m)^\s*sudo env[^\n]*JOIN_TOKEN=`).MatchString(script) {
		t.Error("install.sh still passes JOIN_TOKEN through `sudo env` — visible in /proc/<pid>/cmdline")
	}
	if !strings.Contains(script, "pilldrops-join") {
		t.Fatal("install.sh no longer invokes pilldrops-join")
	}
	if !regexp.MustCompile(`printf[^\n]*JOIN_TOKEN[^\n]*\|`).MatchString(script) {
		t.Error("install.sh does not pipe the token to pilldrops-join on stdin")
	}
	if !strings.Contains(script, "JOIN_TOKEN_FILE") {
		t.Error("install.sh offers no JOIN_TOKEN_FILE path for non-interactive callers")
	}
	if !strings.Contains(script, "/dev/tty") {
		t.Error("install.sh does not prompt on /dev/tty, so an interactive operator must still type it into the command")
	}
}
