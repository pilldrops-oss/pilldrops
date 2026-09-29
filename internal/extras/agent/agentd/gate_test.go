package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestGate(t *testing.T) (*Gate, *Journal) {
	t.Helper()
	j, err := OpenJournal(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	g := NewGate(j, DefaultPolicy)
	g.Timeout = 150 * time.Millisecond // real waits, small ones
	return g, j
}

func TestPolicyAllowedCallsDoNotPark(t *testing.T) {
	g, _ := newTestGate(t)
	d, msg := g.Request(context.Background(), "turn_1",
		ToolCall{Tool: "Read", Input: map[string]any{"file_path": "/work/x"}, ID: "tu_1"})
	if d != DecisionAllow {
		t.Fatalf("Read → %s (%s), want allow without asking", d, msg)
	}
	if len(g.Pending()) != 0 {
		t.Fatal("an allowed call was parked")
	}
}

func TestAPolicyAllowIsStillJournalled(t *testing.T) {
	g, j := newTestGate(t)
	g.Request(context.Background(), "turn_1",
		ToolCall{Tool: "Bash", Input: map[string]any{"command": "git status"}, ID: "tu_1"})

	for _, m := range journalLines(t, j) {
		if m["type"] != EventApprovalGranted {
			continue
		}
		payload, _ := m["payload"].(map[string]any)
		reason, _ := payload["reason"].(string)
		if !strings.Contains(reason, "allowed by policy") || len(reason) < 25 {
			t.Fatalf("automatic allow carries no usable reason: %q", reason)
		}
		return
	}
	t.Fatal("an automatic allow was not journalled at all")
}

func TestTimeoutDenies(t *testing.T) {
	g, j := newTestGate(t)

	start := time.Now()
	d, msg := g.Request(context.Background(), "turn_1",
		ToolCall{Tool: "Write", Input: map[string]any{"file_path": "/work/x", "content": "y"}, ID: "tu_1"})
	if d == DecisionAllow {
		t.Fatal("a call nobody answered was ALLOWED — this is a time-delayed autonomous agent")
	}
	if time.Since(start) < g.Timeout {
		t.Fatalf("returned after %v, before the %v timeout", time.Since(start), g.Timeout)
	}
	if !strings.Contains(msg, "denied") {
		t.Errorf("timeout message does not say it was denied: %q", msg)
	}
	if len(g.Pending()) != 0 {
		t.Fatal("a timed-out approval is still pending")
	}
	if !hasEvent(t, j, EventApprovalTimeout) {
		t.Error("no approval.timeout event: an auditor cannot tell a timeout from a refusal")
	}
}

func TestContextCancelDenies(t *testing.T) {
	g, _ := newTestGate(t)
	g.Timeout = 10 * time.Second // long, so the cancel is what ends the wait

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()

	d, msg := g.Request(ctx, "turn_1",
		ToolCall{Tool: "Write", Input: map[string]any{"file_path": "/work/x"}, ID: "tu_1"})
	if d == DecisionAllow {
		t.Fatal("a cancelled request was allowed")
	}
	if msg == "" {
		t.Error("no explanation for the refusal")
	}
}

func TestHumanAllowUnblocksTheCall(t *testing.T) {
	g, _ := newTestGate(t)
	g.Timeout = 5 * time.Second

	var (
		wg  sync.WaitGroup
		got Decision
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		got, _ = g.Request(context.Background(), "turn_1",
			ToolCall{Tool: "Write", Input: map[string]any{"file_path": "/work/x"}, ID: "tu_1"})
	}()

	id := waitForPending(t, g)
	if err := g.Decide(id, DecisionAllow, RememberOnce, "key:alice"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	wg.Wait()
	if got != DecisionAllow {
		t.Fatalf("decision = %s, want allow", got)
	}
}

func TestHumanDenyCarriesTheRefusalToTheModel(t *testing.T) {
	g, j := newTestGate(t)
	g.Timeout = 5 * time.Second

	var (
		wg  sync.WaitGroup
		msg string
		d   Decision
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		d, msg = g.Request(context.Background(), "turn_1",
			ToolCall{Tool: "Bash", Input: map[string]any{"command": "rm -rf /work"}, ID: "tu_1"})
	}()

	id := waitForPending(t, g)
	if err := g.Decide(id, DecisionAsk, RememberOnce, "key:alice"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	wg.Wait()

	if d == DecisionAllow {
		t.Fatal("a denied call was allowed")
	}
	if msg == "" {
		t.Error("a denial with no message leaves the model to invent a reason")
	}
	if !hasEvent(t, j, EventApprovalDenied) {
		t.Error("no approval.denied event")
	}
}

func TestDecideRejectsUnknownAndDoubleAnswers(t *testing.T) {
	g, _ := newTestGate(t)
	g.Timeout = 5 * time.Second

	if err := g.Decide("apr_nope", DecisionAllow, RememberOnce, "key:alice"); err == nil {
		t.Error("deciding an approval that does not exist succeeded")
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		g.Request(context.Background(), "turn_1",
			ToolCall{Tool: "Write", Input: map[string]any{"file_path": "/work/x"}, ID: "tu_1"})
	}()
	id := waitForPending(t, g)
	if err := g.Decide(id, DecisionAllow, RememberOnce, "key:alice"); err != nil {
		t.Fatalf("first decide: %v", err)
	}
	wg.Wait()
	if err := g.Decide(id, DecisionAsk, RememberOnce, "key:bob"); err == nil {
		t.Error("an already-decided approval accepted a second answer")
	}
}

func TestRememberSessionAppliesToTheCategoryItNames(t *testing.T) {
	g, _ := newTestGate(t)
	g.Timeout = 5 * time.Second

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		g.Request(context.Background(), "turn_1", bash("npm install left-pad"))
	}()
	id := waitForPending(t, g)
	sig := g.Pending()
	_ = sig
	if err := g.Decide(id, DecisionAllow, RememberSession, "key:alice"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	wg.Wait()

	if d, _ := g.Request(context.Background(), "turn_2", bash("npm install react")); d != DecisionAllow {
		t.Fatalf("a second npm install was not covered by the remembered grant: %s", d)
	}
	// A different subcommand is not covered.
	go func() {
		time.Sleep(20 * time.Millisecond)
		for _, a := range g.Pending() {
			_ = g.Decide(a.ID, DecisionAsk, RememberOnce, "key:alice")
		}
	}()
	if d, _ := g.Request(context.Background(), "turn_3", bash("npm publish")); d == DecisionAllow {
		t.Fatal("allowing `npm install` also allowed `npm publish`")
	}
}

func TestRememberedDenialIsNotAThing(t *testing.T) {
	g, _ := newTestGate(t)
	g.Timeout = 5 * time.Second

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		g.Request(context.Background(), "turn_1", bash("curl evil.example.com"))
	}()
	id := waitForPending(t, g)
	if err := g.Decide(id, DecisionAsk, RememberSandbox, "key:alice"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	wg.Wait()

	// The next identical call must still ask.
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.Request(context.Background(), "turn_2", bash("curl evil.example.com"))
	}()
	if id2 := waitForPending(t, g); id2 == "" {
		t.Fatal("a remembered denial silently refused a later call")
	}
	for _, a := range g.Pending() {
		_ = g.Decide(a.ID, DecisionAsk, RememberOnce, "key:alice")
	}
	<-done
}

func TestRememberScopeValidation(t *testing.T) {
	for _, s := range []RememberScope{RememberOnce, RememberSession, RememberSandbox} {
		if !ValidRememberScope(s) {
			t.Errorf("%s should be valid", s)
		}
	}
	for _, s := range []RememberScope{"org", "organization", "forever", "global", ""} {
		if ValidRememberScope(s) {
			t.Errorf("%q must not be an offered scope", s)
		}
	}

	g, _ := newTestGate(t)
	g.Timeout = 5 * time.Second
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); g.Request(context.Background(), "t", bash("curl x")) }()
	id := waitForPending(t, g)
	if err := g.Decide(id, DecisionAllow, "org", "key:alice"); err == nil {
		t.Fatal("Decide accepted an org-wide remember scope")
	}
	_ = g.Decide(id, DecisionAllow, RememberOnce, "key:alice")
	wg.Wait()
}

func TestRememberSandboxSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "remembered.json")

	g1, _ := newTestGate(t)
	g1.Remembered = OpenRememberStore(path)
	g1.Timeout = 5 * time.Second

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); g1.Request(context.Background(), "t", bash("npm install x")) }()
	id := waitForPending(t, g1)
	if err := g1.Decide(id, DecisionAllow, RememberSandbox, "key:alice"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	wg.Wait()

	// A fresh gate is a restarted agentd.
	g2, _ := newTestGate(t)
	g2.Remembered = OpenRememberStore(path)
	if d, _ := g2.Request(context.Background(), "t2", bash("npm install y")); d != DecisionAllow {
		t.Fatal("a sandbox-scoped grant did not survive a restart, so the scope's name is a lie")
	}

	sigs := g2.Remembered.List()
	if len(sigs) != 1 {
		t.Fatalf("remembered list = %v, want exactly the granted signature", sigs)
	}
	if err := g2.Remembered.Forget(sigs[0]); err != nil {
		t.Fatalf("forget: %v", err)
	}
	g3, _ := newTestGate(t)
	g3.Remembered = OpenRememberStore(path)
	g3.Timeout = 60 * time.Millisecond
	if d, _ := g3.Request(context.Background(), "t3", bash("npm install z")); d == DecisionAllow {
		t.Fatal("a forgotten grant still allowed")
	}
}

func TestCorruptRememberStoreStartsEmpty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "remembered.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := OpenRememberStore(p)
	if len(s.List()) != 0 {
		t.Fatal("a corrupt store produced entries")
	}
	if err := s.Add("Bash: ls *"); err != nil {
		t.Fatalf("a corrupt store could not be written over: %v", err)
	}
}

func TestRawArgumentsAreNeverStored(t *testing.T) {
	g, j := newTestGate(t)
	red := NewRedactor()
	red.AddSecret("ANTHROPIC_API_KEY", "sk-ant-supersecret-value-1234567890")
	g.Redactor = red

	secretBody := "config:\n  api_key: sk-ant-supersecret-value-1234567890\n"
	go func() {
		g.Request(context.Background(), "turn_1", ToolCall{
			Tool:  "Write",
			Input: map[string]any{"file_path": "/work/cfg.yaml", "content": secretBody},
			ID:    "tu_1",
		})
	}()
	id := waitForPending(t, g)

	var a Approval
	for _, p := range g.Pending() {
		if p.ID == id {
			a = p
		}
	}
	if strings.Contains(a.ArgsPreview, "sk-ant-supersecret-value-1234567890") {
		t.Fatalf("the preview leaked a known secret: %s", a.ArgsPreview)
	}
	if a.ArgsDigest == "" || len(a.ArgsDigest) != 64 {
		t.Fatalf("no usable args digest: %q", a.ArgsDigest)
	}
	// The preview is written to the journal too.
	raw := journalBytes(t, j)
	if bytes.Contains(raw, []byte("sk-ant-supersecret-value-1234567890")) {
		t.Fatal("the journal recorded a raw secret from tool arguments")
	}
	if !strings.Contains(a.ArgsPreview, "/work/cfg.yaml") {
		t.Errorf("the preview does not name what is being written: %s", a.ArgsPreview)
	}
}

func TestPreviewTruncationSaysSo(t *testing.T) {
	g, _ := newTestGate(t)
	huge := strings.Repeat("x", maxPreviewBytes*3)
	p := g.preview(map[string]any{"content": huge})
	if len(p) > maxPreviewBytes+80 {
		t.Fatalf("preview not truncated: %d bytes", len(p))
	}
	if !strings.Contains(p, "truncated") {
		t.Errorf("truncation is not disclosed: %q", p[len(p)-60:])
	}
}

func TestArgsDigestIsStableAcrossKeyOrder(t *testing.T) {
	a := map[string]any{"file_path": "/work/x", "content": "y", "zzz": 1}
	b := map[string]any{"zzz": 1, "content": "y", "file_path": "/work/x"}
	for range 20 {
		if ArgsDigest(a) != ArgsDigest(b) {
			t.Fatal("digest depends on key order")
		}
	}
	if ArgsDigest(a) == ArgsDigest(map[string]any{"file_path": "/work/OTHER"}) {
		t.Fatal("different arguments produced the same digest")
	}
}

func TestSignatureWidth(t *testing.T) {
	for _, c := range []struct {
		call ToolCall
		want string
	}{
		{bash("npm install left-pad"), "Bash: npm install *"},
		{bash("npm install react"), "Bash: npm install *"},
		{bash("npm publish"), "Bash: npm publish *"},
		{bash("curl https://x.example.com"), "Bash: curl *"},
		// A path or flag as the second word is not a subcommand.
		{bash("cat /etc/passwd"), "Bash: cat *"},
		{bash("ls -la"), "Bash: ls *"},
		{bash("/usr/bin/git status"), "Bash: git status *"},
		{ToolCall{Tool: "Write", Input: map[string]any{"file_path": "/work/src/a.go"}},
			"Write: /work/src/*"},
		{ToolCall{Tool: "Write", Input: map[string]any{"file_path": "/etc/passwd"}},
			"Write: /etc/*"},
		{ToolCall{Tool: "WebFetch", Input: map[string]any{"url": "https://x"}}, "WebFetch"},
	} {
		if got := Signature(c.call); got != c.want {
			t.Errorf("Signature(%s) = %q, want %q", describe(c.call), got, c.want)
		}
	}
}

func TestUnparseableCommandsGetAUniqueSignature(t *testing.T) {
	a := Signature(bash(`cat "unterminated`))
	b := Signature(bash(`rm "also unterminated`))
	if a == b {
		t.Fatalf("two different unparseable commands share a signature: %q", a)
	}
}

// Replays the exact frames captured from a real claude-code client.
func TestMCPHandshakeMatchesWhatTheClientSent(t *testing.T) {
	g, _ := newTestGate(t)

	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"roots":{"listChanged":true},"elicitation":{}},"clientInfo":{"name":"claude-code","version":"2.1.226"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := g.ServeMCP(context.Background(), strings.NewReader(in), &out, "turn_1"); err != nil {
		t.Fatalf("ServeMCP: %v", err)
	}

	msgs := decodeFrames(t, out.String())
	// Two replies for three frames: a notification takes no reply.
	if len(msgs) != 2 {
		t.Fatalf("got %d replies, want 2 (the notification must not be answered): %s", len(msgs), out.String())
	}

	init := msgs[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-11-25" {
		t.Errorf("protocolVersion = %v, want the client's own", init["protocolVersion"])
	}
	if _, ok := init["capabilities"].(map[string]any)["tools"]; !ok {
		t.Error("no tools capability announced, so the client will not call tools/list")
	}

	tools := msgs[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("advertised %d tools, want exactly the gate", len(tools))
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != GateToolName {
		t.Errorf("tool name = %v, want %s", tool["name"], GateToolName)
	}
	if GatePermissionTool != "mcp__"+GateServerName+"__"+GateToolName {
		t.Errorf("permission tool value %q does not match the advertised server/tool",
			GatePermissionTool)
	}
}

func TestMCPDecisionIsJSONInsideATextBlock(t *testing.T) {
	g, _ := newTestGate(t)

	// A policy-allowed call, so no human is needed.
	in := `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"Read","input":{"file_path":"/work/x"},"tool_use_id":"toolu_1"}}}` + "\n"
	var out bytes.Buffer
	if err := g.ServeMCP(context.Background(), strings.NewReader(in), &out, "turn_1"); err != nil {
		t.Fatalf("ServeMCP: %v", err)
	}

	msg := decodeFrames(t, out.String())[0]
	content := msg["result"].(map[string]any)["content"].([]any)
	block := content[0].(map[string]any)
	if block["type"] != "text" {
		t.Fatalf("content block type = %v, want text", block["type"])
	}
	var dec map[string]any
	if err := json.Unmarshal([]byte(block["text"].(string)), &dec); err != nil {
		t.Fatalf("the decision is not JSON inside the text block: %v", err)
	}
	if dec["behavior"] != "allow" {
		t.Fatalf("behavior = %v, want allow", dec["behavior"])
	}
	// updatedInput is required on allow; the client applies what the gate returns.
	ui, ok := dec["updatedInput"].(map[string]any)
	if !ok || ui["file_path"] != "/work/x" {
		t.Fatalf("updatedInput missing or altered: %v", dec["updatedInput"])
	}
}

func TestMCPDenyCarriesAMessage(t *testing.T) {
	g, _ := newTestGate(t)
	g.Timeout = 60 * time.Millisecond // nobody answers, so it times out and denies

	in := `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"Bash","input":{"command":"rm -rf /"},"tool_use_id":"toolu_2"}}}` + "\n"
	var out bytes.Buffer
	if err := g.ServeMCP(context.Background(), strings.NewReader(in), &out, "turn_1"); err != nil {
		t.Fatalf("ServeMCP: %v", err)
	}

	msg := decodeFrames(t, out.String())[0]
	block := msg["result"].(map[string]any)["content"].([]any)[0].(map[string]any)
	var dec map[string]any
	_ = json.Unmarshal([]byte(block["text"].(string)), &dec)
	if dec["behavior"] != "deny" {
		t.Fatalf("behavior = %v, want deny", dec["behavior"])
	}
	if s, _ := dec["message"].(string); s == "" {
		t.Error("a denial with no message leaves the model to invent a reason for the user")
	}
}

func TestAnUnreadableCallIsDenied(t *testing.T) {
	g, _ := newTestGate(t)
	in := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"approve","arguments":{}}}` + "\n"
	var out bytes.Buffer
	if err := g.ServeMCP(context.Background(), strings.NewReader(in), &out, "turn_1"); err != nil {
		t.Fatalf("ServeMCP: %v", err)
	}
	msg := decodeFrames(t, out.String())[0]
	block := msg["result"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if !strings.Contains(block["text"].(string), `"deny"`) {
		t.Fatalf("an unreadable request was not denied: %s", block["text"])
	}
	// Malformed traffic must not satisfy the consultation audit.
	if g.Consulted() != 0 {
		t.Errorf("an unparseable request counted as a gate consultation")
	}
}

func TestUnknownMethodGetsAnErrorNotSilence(t *testing.T) {
	g, _ := newTestGate(t)
	in := `{"jsonrpc":"2.0","id":9,"method":"resources/list"}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/somethingNew"}` + "\n"
	var out bytes.Buffer
	if err := g.ServeMCP(context.Background(), strings.NewReader(in), &out, "t"); err != nil {
		t.Fatalf("ServeMCP: %v", err)
	}
	msgs := decodeFrames(t, out.String())
	// The unknown notification gets nothing; replying to a notification is a protocol error.
	if len(msgs) != 1 {
		t.Fatalf("got %d replies, want 1: %s", len(msgs), out.String())
	}
	if _, ok := msgs[0]["error"]; !ok {
		t.Errorf("unknown method did not get a JSON-RPC error: %v", msgs[0])
	}
}

func TestGateCountsConsultations(t *testing.T) {
	g, _ := newTestGate(t)
	if g.Consulted() != 0 {
		t.Fatal("a fresh gate reports consultations")
	}
	g.Request(context.Background(), "t", ToolCall{Tool: "Read", Input: map[string]any{"file_path": "/work/x"}})
	g.Request(context.Background(), "t", ToolCall{Tool: "Grep", Input: map[string]any{"pattern": "x"}})
	if g.Consulted() != 2 {
		t.Fatalf("Consulted = %d, want 2", g.Consulted())
	}
}

func TestShimAndListenerSpeakEndToEnd(t *testing.T) {
	g, _ := newTestGate(t)
	sock := filepath.Join(shortTempDir(t), "gate.sock")
	l, err := ListenGate(g, sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	l.SetTurn("turn_42")

	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"Read","input":{"file_path":"/work/x"},"tool_use_id":"tu"}}}` + "\n"
	var out bytes.Buffer
	if err := RunGateShim(sock, strings.NewReader(in), &out); err != nil && err != io.EOF {
		t.Fatalf("shim: %v", err)
	}
	// Decoded rather than substring-matched: the decision arrives JSON-escaped inside a text block.
	msg := decodeFrames(t, out.String())[0]
	block := msg["result"].(map[string]any)["content"].([]any)[0].(map[string]any)
	var dec map[string]any
	if err := json.Unmarshal([]byte(block["text"].(string)), &dec); err != nil {
		t.Fatalf("decision that came over the socket is not readable: %v", err)
	}
	if dec["behavior"] != "allow" {
		t.Fatalf("behavior = %v, want allow", dec["behavior"])
	}
}

func TestSocketIsNotWorldAccessible(t *testing.T) {
	g, _ := newTestGate(t)
	sock := filepath.Join(shortTempDir(t), "sub", "gate.sock")
	l, err := ListenGate(g, sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("socket mode %o is group/world accessible", perm)
	}
	di, err := os.Stat(filepath.Dir(sock))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	// A parent the agent's user cannot traverse cannot be reasoned around.
	if perm := di.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("socket directory mode %o is group/world accessible", perm)
	}
}

func TestListenGateReplacesAStaleSocket(t *testing.T) {
	g, _ := newTestGate(t)
	sock := filepath.Join(shortTempDir(t), "gate.sock")
	if err := os.WriteFile(sock, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := ListenGate(g, sock)
	if err != nil {
		t.Fatalf("a stale socket file blocked startup: %v", err)
	}
	l.Close()
}

func TestShimWithoutASocketFails(t *testing.T) {
	t.Setenv(GateSocketEnv, "")
	if err := RunGateShim("", strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("the shim started with no socket configured")
	}
	if err := RunGateShim(filepath.Join(shortTempDir(t), "absent.sock"), strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("the shim started with a socket that does not exist")
	}
}

func TestGuidedModeNeverCombinesWithSkipPermissions(t *testing.T) {
	tp := &Templates{
		RunCmd: `claude -p "{task}"`, CLIBin: "claude",
		RunFlags: "--dangerously-skip-permissions", RunFlagsEnv: "IS_SANDBOX=1",
		PermissionTool: "--permission-prompt-tool {tool}",
		GateConfig:     "/var/lib/agentd/.pilldrops-gate.mcp.json",
	}
	cmd, _, err := tp.Build(&Turn{Request: "do it", Mode: ModeGuided})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if strings.Contains(cmd, "dangerously-skip-permissions") {
		t.Fatalf("Guided mode built a command that bypasses its own gate: %s", cmd)
	}
	if !strings.Contains(cmd, "--permission-prompt-tool "+GatePermissionTool) {
		t.Errorf("no permission-prompt-tool flag: %s", cmd)
	}
	// --mcp-config is greedy: the space form swallows the prompt as a second config path.
	if !strings.Contains(cmd, "--mcp-config=") {
		t.Errorf("--mcp-config must use the = form or it eats the prompt: %s", cmd)
	}
	if strings.Contains(cmd, "--mcp-config /") {
		t.Errorf("space-separated --mcp-config will swallow the task: %s", cmd)
	}
	if !strings.Contains(cmd, "--strict-mcp-config") {
		t.Errorf("without --strict-mcp-config a user's own config can add servers: %s", cmd)
	}
	if !strings.Contains(cmd, "do it") {
		t.Errorf("the task was lost: %s", cmd)
	}
}

func TestAutonomousModeIsUnchanged(t *testing.T) {
	tp := &Templates{
		RunCmd: `claude -p "{task}"`, CLIBin: "claude",
		RunFlags: "--dangerously-skip-permissions", RunFlagsEnv: "IS_SANDBOX=1",
		PermissionTool: "--permission-prompt-tool {tool}", GateConfig: "/x/cfg.json",
	}
	cmd, _, _ := tp.Build(&Turn{Request: "do it", Mode: ModeAutonomous})
	if !strings.Contains(cmd, "--dangerously-skip-permissions") {
		t.Errorf("autonomous mode lost its autonomy flag: %s", cmd)
	}
	if strings.Contains(cmd, "permission-prompt-tool") {
		t.Errorf("autonomous mode gained a gate it does not use: %s", cmd)
	}
}

func TestGuidedIsNotOfferedWithoutAVerifiedGate(t *testing.T) {
	for name, tp := range map[string]*Templates{
		"no permission tool": {RunCmd: `x "{task}"`, CLIBin: "x", GateConfig: "/x/cfg.json"},
		"no gate config":     {RunCmd: `x "{task}"`, CLIBin: "x", PermissionTool: "--ppt {tool}"},
		"neither":            {RunCmd: `x "{task}"`, CLIBin: "x"},
	} {
		if tp.GuidedAvailable() {
			t.Errorf("%s: Guided reported available", name)
		}
		// A Guided turn built anyway must be a plain command, not an ungated autonomous one.
		cmd, _, err := tp.Build(&Turn{Request: "t", Mode: ModeGuided})
		if err != nil {
			t.Fatalf("%s: build: %v", name, err)
		}
		if strings.Contains(cmd, "dangerously") {
			t.Errorf("%s: a Guided turn fell back to autonomy: %s", name, cmd)
		}
	}
}

func TestGateConfigPathIsQuoted(t *testing.T) {
	tp := &Templates{
		RunCmd: `claude -p "{task}"`, CLIBin: "claude",
		PermissionTool: "--permission-prompt-tool {tool}",
		GateConfig:     "/var/lib/my agent/cfg.json",
	}
	cmd, _, _ := tp.Build(&Turn{Request: "t", Mode: ModeGuided})
	// --mcp-config is greedy, so an unquoted path with a space becomes two config paths.
	if !strings.Contains(cmd, `--mcp-config='/var/lib/my agent/cfg.json'`) {
		t.Fatalf("gate config path is not quoted: %s", cmd)
	}
}

func TestWriteGateConfigProducesAConfigTheCLICanRead(t *testing.T) {
	dir := t.TempDir()
	p, err := WriteGateConfig(dir, "/usr/local/bin/agentd", "/run/gate.sock")
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("config is not valid json: %v", err)
	}
	srv, ok := cfg.MCPServers[GateServerName]
	if !ok {
		t.Fatalf("config does not register %q, so the flag names a tool that does not exist: %s",
			GateServerName, b)
	}
	if srv.Command != "/usr/local/bin/agentd" {
		t.Errorf("command = %q, want the agentd binary itself", srv.Command)
	}
	if len(srv.Args) != 1 || srv.Args[0] != "-gate" {
		t.Errorf("args = %v, want [-gate]", srv.Args)
	}
	// The socket is passed in the environment, not the world-readable command line.
	if srv.Env[GateSocketEnv] != "/run/gate.sock" {
		t.Errorf("socket not passed in env: %v", srv.Env)
	}
	if fi, err := os.Stat(p); err == nil && fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("gate config mode %o is group/world readable", fi.Mode().Perm())
	}
}

// shortTempDir returns a path short enough for a unix socket: macOS caps
// sun_path at 104 bytes and t.TempDir is routinely longer.
func shortTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "g")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func waitForPending(t *testing.T, g *Gate) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p := g.Pending(); len(p) > 0 {
			return p[0].ID
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("nothing parked within 2s")
	return ""
}

// journalLines decodes every journal line as a generic map, so a leak in any field is visible.
func journalLines(t *testing.T, j *Journal) []map[string]any {
	t.Helper()
	if err := j.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	var out []map[string]any
	err := j.Replay(0, func(_ uint64, line []byte) error {
		var m map[string]any
		if json.Unmarshal(line, &m) == nil {
			out = append(out, m)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	return out
}

func hasEvent(t *testing.T, j *Journal, name string) bool {
	t.Helper()
	for _, m := range journalLines(t, j) {
		if m["type"] == name {
			return true
		}
	}
	return false
}

func journalBytes(t *testing.T, j *Journal) []byte {
	t.Helper()
	b, _ := json.Marshal(journalLines(t, j))
	return b
}

func decodeFrames(t *testing.T, s string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("reply is not JSON: %q (%v)", line, err)
		}
		out = append(out, m)
	}
	return out
}

var _ = net.Dial // keep net imported

func TestApprovalIDsAreUniqueAcrossRestarts(t *testing.T) {
	seen := map[string]bool{}
	for range 3 {
		g, _ := newTestGate(t) // a fresh gate is a restarted agentd
		g.Timeout = 40 * time.Millisecond
		g.Request(context.Background(), "t", bash("curl evil.example.com"))
		for _, a := range g.Resolved() {
			if seen[a.ID] {
				t.Fatalf("approval id %q reused after a restart — a stale click would "+
					"decide a different action", a.ID)
			}
			seen[a.ID] = true
		}
	}
	if len(seen) != 3 {
		t.Fatalf("recorded %d resolved approvals, want 3", len(seen))
	}
}

func TestAutomaticAllowsReachTheAuditTrail(t *testing.T) {
	g, _ := newTestGate(t)
	g.Request(context.Background(), "turn_1",
		ToolCall{Tool: "Read", Input: map[string]any{"file_path": "/work/x"}, ID: "tu_1"})

	res := g.Resolved()
	if len(res) != 1 {
		t.Fatalf("resolved = %d, want the policy allow recorded", len(res))
	}
	if res[0].Decision != DecisionAllow {
		t.Errorf("decision = %s", res[0].Decision)
	}
	if res[0].DecidedBy != "policy" {
		t.Errorf("decided_by = %q, want policy — an auditor must be able to tell this "+
			"from a person's approval", res[0].DecidedBy)
	}
	if res[0].Reason == "" {
		t.Error("no reason recorded, so the trail cannot say WHICH rule allowed it")
	}
}

func TestTimeoutsReachTheAuditTrail(t *testing.T) {
	g, _ := newTestGate(t)
	g.Request(context.Background(), "turn_1", bash("rm -rf /work"))

	res := g.Resolved()
	if len(res) != 1 {
		t.Fatalf("resolved = %d, want the timeout recorded", len(res))
	}
	if res[0].Decision == DecisionAllow {
		t.Fatal("a timeout was recorded as an allow")
	}
	// A timeout must be distinguishable from a human refusal.
	if !strings.Contains(res[0].Reason, "no answer within") {
		t.Errorf("a timeout is indistinguishable from a refusal in the trail: %q", res[0].Reason)
	}
}

func TestResolvedRingIsBounded(t *testing.T) {
	g, _ := newTestGate(t)
	for i := range maxResolvedKept + 50 {
		g.Request(context.Background(), "t",
			ToolCall{Tool: "Read", Input: map[string]any{"file_path": "/work/x"}, ID: string(rune('a' + i%26))})
	}
	if n := len(g.Resolved()); n > maxResolvedKept {
		t.Fatalf("resolved ring holds %d, above the %d bound", n, maxResolvedKept)
	}
}

func ladderFor(t *testing.T, guided func() bool) AutonomyLadder {
	t.Helper()
	j := openTestJournal(t, t.TempDir())
	sup := &Supervisor{
		Queue: mustQueue(t), Journal: j, GuidedAvailable: guided,
	}
	return sup.Status().Autonomy
}

func mustQueue(t *testing.T) *Queue {
	t.Helper()
	q, err := OpenQueue(t.TempDir())
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	return q
}

func TestGuidedIsOnlyOfferedWithAWorkingGate(t *testing.T) {
	yes := ladderFor(t, func() bool { return true })
	if !slicesContain(yes.Available, ModeGuided) {
		t.Errorf("guided not offered with a working gate: %v", yes.Available)
	}
	if yes.GuidedUnavailableReason != "" {
		t.Errorf("a reason was given while guided IS available: %q", yes.GuidedUnavailableReason)
	}

	for name, l := range map[string]AutonomyLadder{
		"gate failed to start": ladderFor(t, func() bool { return false }),
		"agent has no gate":    ladderFor(t, nil),
	} {
		if slicesContain(l.Available, ModeGuided) {
			t.Errorf("%s: guided was offered anyway", name)
		}
		if l.GuidedUnavailableReason == "" {
			t.Errorf("%s: no reason given for guided being unavailable", name)
		}
	}
}

func TestLadderIsOrderedNarrowestFirst(t *testing.T) {
	l := ladderFor(t, func() bool { return true })
	want := []string{ModeSupervised, ModeGuided, ModeAutonomous}
	if len(l.Available) != len(want) {
		t.Fatalf("available = %v, want %v", l.Available, want)
	}
	for i := range want {
		if l.Available[i] != want[i] {
			t.Fatalf("available = %v, want %v", l.Available, want)
		}
	}
	// The default stays autonomous; see Queue.Enqueue.
	if l.Default != ModeAutonomous {
		t.Errorf("default = %q, want autonomous", l.Default)
	}
}

// Without a gate a guided turn would build as a plain command, which for claude
// does nothing while reporting success.
func TestGuidedIsRefusedNotDowngraded(t *testing.T) {
	j := openTestJournal(t, t.TempDir())
	sup := &Supervisor{Queue: mustQueue(t), Journal: j} // no gate

	_, err := sup.Submit("do it", ModeGuided, "latest", "api", false)
	if err == nil {
		t.Fatal("a guided turn was accepted on an agent with no gate; it would silently do nothing")
	}
	if !errors.Is(err, ErrModeUnavailable) {
		t.Fatalf("error = %v, want ErrModeUnavailable", err)
	}
	if !strings.Contains(err.Error(), "approval gate") {
		t.Errorf("the refusal does not explain itself: %v", err)
	}
	if d := sup.Queue.Depth(); d != 0 {
		t.Errorf("queue depth = %d after a refused submit", d)
	}
}

func TestUnknownModeIsRefusedRatherThanDefaulted(t *testing.T) {
	j := openTestJournal(t, t.TempDir())
	sup := &Supervisor{Queue: mustQueue(t), Journal: j, GuidedAvailable: func() bool { return true }}

	if _, err := sup.Submit("do it", "guarded", "latest", "api", false); !errors.Is(err, ErrModeUnavailable) {
		t.Fatalf("an unknown mode was accepted: %v", err)
	}
	// An empty mode is fine; Queue.Enqueue defaults it.
	if _, err := sup.Submit("do it", "", "latest", "api", false); err != nil {
		t.Fatalf("an unset mode was refused: %v", err)
	}
}

func slicesContain(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// Only "plain works, flag fails" counts as rejection; grepping stderr would
// break in the dangerous direction when the CLI rewords its errors.
func TestProbeIsDifferentialNotErrorMatching(t *testing.T) {
	dir := shortTempDir(t)

	// A CLI that accepts anything.
	accepts := filepath.Join(dir, "accepts")
	if err := os.WriteFile(accepts, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if r := ProbePermissionTool(context.Background(), "/bin/sh", accepts, "--ppt {tool}"); !r.Accepted {
		t.Fatalf("a permissive CLI was reported as rejecting the flag: %+v", r)
	}

	// Rejects the flag with a message the probe has never seen.
	rejects := filepath.Join(dir, "rejects")
	script := "#!/bin/sh\nfor a in \"$@\"; do\n case \"$a\" in --ppt) echo 'whoops, no such switch' >&2; exit 64;; esac\ndone\nexit 0\n"
	if err := os.WriteFile(rejects, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := ProbePermissionTool(context.Background(), "/bin/sh", rejects, "--ppt {tool}")
	if r.Accepted || r.Inconclusive {
		t.Fatalf("a rejecting CLI was not detected: %+v", r)
	}
	if r.Reason == "" {
		t.Error("no reason given, so the UI cannot explain why Guided is missing")
	}
}

// A broken install must not be reported as a capability change.
func TestProbeIsInconclusiveRatherThanWrong(t *testing.T) {
	for name, bin := range map[string]string{
		"missing binary": filepath.Join(shortTempDir(t), "not-there"),
		"empty binary":   "",
	} {
		r := ProbePermissionTool(context.Background(), "/bin/sh", bin, "--ppt {tool}")
		if !r.Inconclusive {
			t.Errorf("%s: expected inconclusive, got %+v", name, r)
		}
		if r.Reason == "" {
			t.Errorf("%s: inconclusive with no reason", name)
		}
	}
	// A CLI that fails everything is inconclusive too.
	broken := filepath.Join(shortTempDir(t), "broken")
	if err := os.WriteFile(broken, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if r := ProbePermissionTool(context.Background(), "/bin/sh", broken, "--ppt {tool}"); !r.Inconclusive {
		t.Errorf("a wholly broken CLI was not inconclusive: %+v", r)
	}
}

func TestTierTwoCatchesAnAcceptedButIgnoredFlag(t *testing.T) {
	j := openTestJournal(t, t.TempDir())
	sup := &Supervisor{
		Queue: mustQueue(t), Journal: j,
		GuidedAvailable: func() bool { return true },
		// A gate that is never consulted, which is exactly the symptom.
		GateConsulted: func() int { return 0 },
	}
	if !slicesContain(sup.Status().Autonomy.Available, ModeGuided) {
		t.Fatal("guided was not offered before the audit, so this proves nothing")
	}

	turn := &Turn{ID: "turn_1", Mode: ModeGuided}
	sup.auditGateConsultation(turn, Result{ToolCalls: 3}, 0)

	l := sup.Status().Autonomy
	if slicesContain(l.Available, ModeGuided) {
		t.Fatal("guided is still offered after the gate was proven to be bypassed")
	}
	if l.GuidedUnavailableReason == "" {
		t.Error("no reason given for the withdrawal")
	}
	if !strings.Contains(l.GuidedUnavailableReason, "ignored") {
		t.Errorf("the reason does not say the flag is being ignored: %q", l.GuidedUnavailableReason)
	}
	if sup.Status().State != StateDegraded {
		t.Errorf("session state = %s, want degraded", sup.Status().State)
	}
	if !hasEvent(t, j, EventSessionDegraded) {
		t.Error("the withdrawal was not journalled")
	}
	if _, err := sup.Submit("x", ModeGuided, "latest", "api", false); !errors.Is(err, ErrModeUnavailable) {
		t.Errorf("guided was still accepted after withdrawal: %v", err)
	}
}

func TestTierTwoDoesNotFireWithoutEvidence(t *testing.T) {
	for name, c := range map[string]struct {
		mode string
		res  Result
	}{
		"no tool calls at all":    {ModeGuided, Result{ToolCalls: 0}},
		"an autonomous turn":      {ModeAutonomous, Result{ToolCalls: 5}},
		"a turn with no mode set": {"", Result{ToolCalls: 5}},
	} {
		j := openTestJournal(t, t.TempDir())
		sup := &Supervisor{
			Queue: mustQueue(t), Journal: j,
			GuidedAvailable: func() bool { return true },
			GateConsulted:   func() int { return 0 },
		}
		sup.auditGateConsultation(&Turn{ID: "t", Mode: c.mode}, c.res, 0)
		if !slicesContain(sup.Status().Autonomy.Available, ModeGuided) {
			t.Errorf("%s: guided was withdrawn without evidence", name)
		}
	}

	// The healthy case: tools used and the gate consulted.
	j := openTestJournal(t, t.TempDir())
	consulted := 0
	sup := &Supervisor{
		Queue: mustQueue(t), Journal: j,
		GuidedAvailable: func() bool { return true },
		GateConsulted:   func() int { return consulted },
	}
	before := consulted
	consulted = 3 // the gate was asked three times during the turn
	sup.auditGateConsultation(&Turn{ID: "t", Mode: ModeGuided}, Result{ToolCalls: 3}, before)
	if !slicesContain(sup.Status().Autonomy.Available, ModeGuided) {
		t.Error("guided was withdrawn on a turn where the gate WAS consulted")
	}
}

func TestTierTwoWithdrawalIsSticky(t *testing.T) {
	j := openTestJournal(t, t.TempDir())
	sup := &Supervisor{
		Queue: mustQueue(t), Journal: j,
		GuidedAvailable: func() bool { return true },
		GateConsulted:   func() int { return 0 },
	}
	sup.auditGateConsultation(&Turn{ID: "t1", Mode: ModeGuided}, Result{ToolCalls: 1}, 0)
	// A later turn that consults the gate must not bring Guided back.
	sup.GateConsulted = func() int { return 99 }
	sup.auditGateConsultation(&Turn{ID: "t2", Mode: ModeGuided}, Result{ToolCalls: 1}, 0)
	if slicesContain(sup.Status().Autonomy.Available, ModeGuided) {
		t.Fatal("a withdrawn Guided mode came back")
	}
}

func TestToolCallsAreCountedForTheAudit(t *testing.T) {
	a := newTurnAccum()
	if a.toolCallCount() != 0 {
		t.Fatal("a fresh accumulator reports tool calls")
	}
	a.noteToolCall()
	a.noteToolCall()
	if a.toolCallCount() != 2 {
		t.Fatalf("tool calls = %d, want 2", a.toolCallCount())
	}
}

// The agent runs as the same user as the socket, so it can flood the gate with asks of its own.
func TestParkedApprovalsAreCapped(t *testing.T) {
	g, j := newTestGate(t)
	g.Timeout = 20 * time.Second // long, so nothing drains on its own

	var wg sync.WaitGroup
	for i := range maxPendingApprovals {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			g.Request(context.Background(), "turn_1",
				ToolCall{Tool: "Write", Input: map[string]any{"file_path": "/work/f"},
					ID: "tu_" + strconv.Itoa(i)})
		}(i)
	}
	waitFor := time.Now().Add(5 * time.Second)
	for len(g.Pending()) < maxPendingApprovals && time.Now().Before(waitFor) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := len(g.Pending()); n != maxPendingApprovals {
		t.Fatalf("parked %d, want the cap of %d", n, maxPendingApprovals)
	}

	// One more must be refused immediately rather than parked.
	start := time.Now()
	d, msg := g.Request(context.Background(), "turn_1",
		ToolCall{Tool: "Write", Input: map[string]any{"file_path": "/work/overflow"}, ID: "tu_x"})
	if d == DecisionAllow {
		t.Fatal("a call over the cap was ALLOWED — a flood would grant itself access")
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("the over-cap call blocked for %v; it must refuse immediately", time.Since(start))
	}
	if !strings.Contains(msg, "already waiting") {
		t.Errorf("the refusal does not explain itself: %q", msg)
	}
	if n := len(g.Pending()); n != maxPendingApprovals {
		t.Errorf("the over-cap call was parked anyway: %d pending", n)
	}
	if !hasEvent(t, j, EventApprovalDenied) {
		t.Error("the refusal was not journalled")
	}

	// Drain, so the goroutines do not outlive the test.
	for _, a := range g.Pending() {
		_ = g.Decide(a.ID, DecisionAsk, RememberOnce, "key:test")
	}
	wg.Wait()
}

// chooseAddrForTest mirrors cmd/agentd's chooseAddr, which lives in package main.
func chooseAddrForTest(explicit, tokenHash string, allowExposed bool) string {
	if explicit != "" {
		return explicit
	}
	if tokenHash != "" || allowExposed {
		return "0.0.0.0:6002"
	}
	return LoopbackAddr
}

// With a token hash agentd binds 0.0.0.0 so the node can reach it over TCP; the
// token is what defends it, so that is what this asserts.
func TestBindAddressChoiceIsTokenGated(t *testing.T) {
	// No token: loopback.
	if addr := chooseAddrForTest("", "", false); !isLoopbackAddr(addr) {
		t.Errorf("with no token hash the bind address is %q, which is routable and unauthenticated", addr)
	}
	// With a token: routable, so the node can reach it.
	if addr := chooseAddrForTest("", "deadbeef", false); isLoopbackAddr(addr) {
		t.Error("with a token hash the address is loopback, so the node cannot reach the agent API")
	}
	// The server itself refuses a routable bind with no token.
	srv := &Server{Addr: "0.0.0.0:0", TokenHash: ""}
	if _, _, err := srv.Listen(); !errors.Is(err, ErrExposedWithoutAuth) {
		t.Fatalf("Listen on a routable address with no token = %v, want a refusal", err)
	}
}

func TestParkedTurnIsReportedAsAwaiting(t *testing.T) {
	g := NewGate(nil, Policy{Rules: nil})
	g.Timeout = 300 * time.Millisecond

	var mu sync.Mutex
	var states []bool
	g.OnPark = func(turnID string, waiting bool) {
		mu.Lock()
		defer mu.Unlock()
		if turnID != "turn_1" {
			t.Errorf("OnPark reported turn %q", turnID)
		}
		states = append(states, waiting)
	}

	// A call the default-empty policy will not allow, so it parks.
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.Request(context.Background(), "turn_1", ToolCall{
			Tool: "Bash", ID: "toolu_1", Input: map[string]any{"command": "rm -rf /"},
		})
	}()

	// Marked waiting before a decision arrives.
	waitFor(t, "the turn to be reported as awaiting approval", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(states) == 1 && states[0]
	})

	<-done // let it time out, which denies

	// And cleared afterwards.
	mu.Lock()
	defer mu.Unlock()
	if len(states) != 2 {
		t.Fatalf("OnPark fired %d times, want 2 (park then clear): %v", len(states), states)
	}
	if states[1] {
		t.Error("the turn was left marked as awaiting approval after the gate released it")
	}
}

func TestApprovalClearsAwaitingOnEveryExit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		decide func(g *Gate, id string)
	}{
		{"allowed", func(g *Gate, id string) { _ = g.Decide(id, DecisionAllow, RememberOnce, "someone") }},
		{"denied", func(g *Gate, id string) { _ = g.Decide(id, DecisionAsk, RememberOnce, "someone") }},
		{"timed out", func(g *Gate, id string) { /* let the timer fire */ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGate(nil, Policy{Rules: nil})
			g.Timeout = 300 * time.Millisecond

			var mu sync.Mutex
			var last bool
			var calls int
			g.OnPark = func(_ string, waiting bool) {
				mu.Lock()
				defer mu.Unlock()
				last, calls = waiting, calls+1
			}

			done := make(chan struct{})
			go func() {
				defer close(done)
				g.Request(context.Background(), "turn_1", ToolCall{
					Tool: "Bash", ID: "toolu_1", Input: map[string]any{"command": "rm -rf /"},
				})
			}()

			waitFor(t, "the call to park", func() bool { return len(g.Pending()) == 1 })
			tc.decide(g, g.Pending()[0].ID)
			<-done

			mu.Lock()
			defer mu.Unlock()
			if calls < 2 || last {
				t.Errorf("after %s the turn is still marked awaiting (calls=%d last=%v)",
					tc.name, calls, last)
			}
		})
	}
}
