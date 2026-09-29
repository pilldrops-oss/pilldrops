package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// fakeTelegram records what the bridge sent and answers the Bot API shape.
type fakeTelegram struct {
	mu   sync.Mutex
	sent []string
	srv  *httptest.Server
}

func newFakeTelegram(t *testing.T) (*fakeTelegram, *Client) {
	t.Helper()
	f := &fakeTelegram{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		if txt, ok := body["text"].(string); ok {
			f.sent = append(f.sent, txt)
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
	}))
	t.Cleanup(f.srv.Close)
	c := NewClient("test-token")
	c.BaseURL = f.srv.URL
	c.HTTP = f.srv.Client()
	return f, c
}

func (f *fakeTelegram) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

func (f *fakeTelegram) last() string {
	m := f.messages()
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1]
}

type fakeLinks struct {
	link     store.ChatLink
	linked   bool
	redeem   error
	deleted  bool
	redeemed int
}

func (f *fakeLinks) RedeemLinkCode(ctx context.Context, code, channel, chatID string) (store.ChatLink, error) {
	f.redeemed++
	if f.redeem != nil {
		return store.ChatLink{}, f.redeem
	}
	f.link = store.ChatLink{Channel: channel, ChatID: chatID, SandboxID: "sb_ws", OrgID: "org"}
	f.linked = true
	return f.link, nil
}
func (f *fakeLinks) ChatLinkFor(ctx context.Context, channel, chatID string) (store.ChatLink, bool, error) {
	return f.link, f.linked, nil
}
func (f *fakeLinks) DeleteChatLink(ctx context.Context, channel, chatID string) error {
	f.deleted = true
	f.linked = false
	return nil
}

type fakeRunner struct {
	mu        sync.Mutex
	out       string
	err       error
	followUps []bool
	autonomy  []bool
	tasks     []string
}

func (r *fakeRunner) Run(_ context.Context, _, task string, opts RunOpts) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tasks = append(r.tasks, task)
	r.followUps = append(r.followUps, opts.FollowUp)
	r.autonomy = append(r.autonomy, opts.Autonomous)
	return r.out, r.err
}

func callback(chatID int64, data string) Update {
	u := Update{UpdateID: 2}
	u.CallbackQuery = &struct {
		ID   string `json:"id"`
		Data string `json:"data"`
		From struct {
			ID int64 `json:"id"`
		} `json:"from"`
		Message *struct {
			MessageID int64 `json:"message_id"`
			Chat      struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
	}{ID: "cb1", Data: data}
	u.CallbackQuery.Message = &struct {
		MessageID int64 `json:"message_id"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	}{MessageID: 5}
	u.CallbackQuery.Message.Chat.ID = chatID
	return u
}

func msg(chatID int64, text string) Update {
	u := Update{UpdateID: 1}
	u.Message = &struct {
		MessageID int64 `json:"message_id"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		From *struct {
			Username string `json:"username"`
		} `json:"from"`
		Text  string `json:"text"`
		Voice *struct {
			FileID   string `json:"file_id"`
			Duration int    `json:"duration"`
		} `json:"voice"`
	}{Text: text}
	u.Message.Chat.ID = chatID
	return u
}

// An unlinked chat is told how to link rather than ignored.
func TestUnlinkedChatCannotRunTasks(t *testing.T) {
	f, c := newFakeTelegram(t)
	runner := &fakeRunner{out: "done"}
	b := New(c, &fakeLinks{}, runner)

	b.handle(context.Background(), msg(42, "fix the failing test"))

	if len(runner.tasks) != 0 {
		t.Fatal("an unlinked chat ran a task on a workstation")
	}
	if !strings.Contains(f.last(), "/link") {
		t.Errorf("unlinked reply doesn't say how to link: %q", f.last())
	}
}

func TestReadOnlyLinkRefusesTasks(t *testing.T) {
	f, c := newFakeTelegram(t)
	links := &fakeLinks{linked: true, link: store.ChatLink{SandboxID: "sb_ws", ReadOnly: true}}
	runner := &fakeRunner{out: "done"}
	b := New(c, links, runner)

	b.handle(context.Background(), msg(42, "delete everything"))

	if len(runner.tasks) != 0 {
		t.Fatal("a read-only link executed a task")
	}
	if !strings.Contains(strings.ToLower(f.last()), "read-only") {
		t.Errorf("reply doesn't explain the refusal: %q", f.last())
	}
}

func TestFirstMessageStartsFreshThenContinues(t *testing.T) {
	_, c := newFakeTelegram(t)
	links := &fakeLinks{linked: true, link: store.ChatLink{SandboxID: "sb_ws"}}
	runner := &fakeRunner{out: "ok"}
	b := New(c, links, runner)
	b.TaskTimeout = time.Second

	b.handle(context.Background(), msg(42, "first task"))
	b.lastAt = map[string]time.Time{} // bypass the rate limit for the test
	b.handle(context.Background(), msg(42, "second task"))

	if len(runner.followUps) != 2 {
		t.Fatalf("ran %d tasks, want 2", len(runner.followUps))
	}
	if runner.followUps[0] {
		t.Error("first message continued a conversation that does not exist yet")
	}
	if !runner.followUps[1] {
		t.Error("second message started a new conversation — the agent would forget the thread")
	}
}

// The new workstation has no history, so continuing into it would leak the previous machine's context.
func TestRelinkResetsConversation(t *testing.T) {
	_, c := newFakeTelegram(t)
	links := &fakeLinks{linked: true, link: store.ChatLink{SandboxID: "sb_ws"}}
	runner := &fakeRunner{out: "ok"}
	b := New(c, links, runner)

	b.handle(context.Background(), msg(42, "first task"))
	b.handle(context.Background(), msg(42, "/link ABCDEF"))
	b.lastAt = map[string]time.Time{}
	b.handle(context.Background(), msg(42, "after relink"))

	if len(runner.followUps) != 2 {
		t.Fatalf("ran %d tasks, want 2", len(runner.followUps))
	}
	if runner.followUps[1] {
		t.Error("continued the old conversation on a freshly linked workstation")
	}
}

func TestRunFailureIsReported(t *testing.T) {
	f, c := newFakeTelegram(t)
	links := &fakeLinks{linked: true, link: store.ChatLink{SandboxID: "sb_ws"}}
	b := New(c, links, &fakeRunner{err: errors.New("workstation is out of hours")})

	b.handle(context.Background(), msg(42, "do the thing"))

	if !strings.Contains(f.last(), "out of hours") {
		t.Errorf("failure not reported to the chat: %q", f.last())
	}
}

func TestEmptyOutputStillReplies(t *testing.T) {
	f, c := newFakeTelegram(t)
	links := &fakeLinks{linked: true, link: store.ChatLink{SandboxID: "sb_ws"}}
	b := New(c, links, &fakeRunner{out: "   "})

	b.handle(context.Background(), msg(42, "do the thing"))

	if !strings.Contains(strings.ToLower(f.last()), "no output") {
		t.Errorf("empty output left the user with no answer: %q", f.last())
	}
}

func TestRateLimitPerChat(t *testing.T) {
	_, c := newFakeTelegram(t)
	links := &fakeLinks{linked: true, link: store.ChatLink{SandboxID: "sb_ws"}}
	runner := &fakeRunner{out: "ok"}
	b := New(c, links, runner)

	b.handle(context.Background(), msg(42, "one"))
	b.handle(context.Background(), msg(42, "two"))

	if len(runner.tasks) != 1 {
		t.Errorf("ran %d tasks back to back, want 1 (the second is rate-limited)", len(runner.tasks))
	}
}

// With no transcriber configured, say so rather than dropping the message.
func TestVoiceNoteSaysNotReady(t *testing.T) {
	f, c := newFakeTelegram(t)
	links := &fakeLinks{linked: true, link: store.ChatLink{SandboxID: "sb_ws"}}
	b := New(c, links, &fakeRunner{})

	u := msg(42, "")
	u.Message.Voice = &struct {
		FileID   string `json:"file_id"`
		Duration int    `json:"duration"`
	}{FileID: "f1", Duration: 3}
	b.handle(context.Background(), u)

	if !strings.Contains(strings.ToLower(f.last()), "voice") {
		t.Errorf("voice note dropped silently: %q", f.last())
	}
}

func TestVoiceNoteBecomesTaskAndIsEchoed(t *testing.T) {
	f, c := newFakeTelegram(t)
	links := &fakeLinks{linked: true, link: store.ChatLink{SandboxID: "sb_ws", OrgID: "org"}}
	runner := &fakeRunner{out: "done"}
	b := New(c, links, runner)

	audio := newAudioServer(t, []byte("ogg"), 200)
	boxes := &fakeBoxes{exists: true, state: SandboxState{Template: "claude"},
		res: rt.ExecResult{Stdout: "open a PR for the changelog"}}
	b.Voice = &Transcriber{Files: &fakeFiles{url: audio}, Writer: &fakeWriter{}, Boxes: boxes}

	u := msg(42, "")
	u.Message.Voice = &struct {
		FileID   string `json:"file_id"`
		Duration int    `json:"duration"`
	}{FileID: "f1", Duration: 4}
	b.handle(context.Background(), u)

	if len(runner.tasks) != 1 {
		t.Fatalf("ran %d tasks from a voice note, want 1", len(runner.tasks))
	}
	if runner.tasks[0] != "open a PR for the changelog" {
		t.Errorf("task = %q, want the transcript", runner.tasks[0])
	}
	var echoed bool
	for _, m := range f.messages() {
		if strings.HasPrefix(m, "Heard: ") {
			echoed = true
		}
	}
	if !echoed {
		t.Error("transcript not echoed — a misheard instruction would be invisible")
	}
}

func TestVoiceFailureRunsNothing(t *testing.T) {
	f, c := newFakeTelegram(t)
	links := &fakeLinks{linked: true, link: store.ChatLink{SandboxID: "sb_ws"}}
	runner := &fakeRunner{out: "done"}
	b := New(c, links, runner)

	audio := newAudioServer(t, []byte("ogg"), 200)
	b.Voice = &Transcriber{
		Files: &fakeFiles{url: audio}, Writer: &fakeWriter{},
		Boxes: &fakeBoxes{exists: true, res: rt.ExecResult{ExitCode: 2}},
	}

	u := msg(42, "")
	u.Message.Voice = &struct {
		FileID   string `json:"file_id"`
		Duration int    `json:"duration"`
	}{FileID: "f1", Duration: 4}
	b.handle(context.Background(), u)

	if len(runner.tasks) != 0 {
		t.Fatal("ran a task despite a failed transcription")
	}
	if !strings.Contains(f.last(), "OPENAI_API_KEY") {
		t.Errorf("failure does not name the fix: %q", f.last())
	}
}

func TestUnlinkAndStatus(t *testing.T) {
	f, c := newFakeTelegram(t)
	links := &fakeLinks{linked: true, link: store.ChatLink{SandboxID: "sb_ws"}}
	b := New(c, links, &fakeRunner{})

	b.handle(context.Background(), msg(42, "/status"))
	if !strings.Contains(f.last(), "sb_ws") {
		t.Errorf("/status doesn't name the workstation: %q", f.last())
	}
	b.handle(context.Background(), msg(42, "/unlink"))
	if !links.deleted {
		t.Error("/unlink did not revoke the link")
	}
	b.handle(context.Background(), msg(42, "/status"))
	if strings.Contains(f.last(), "sb_ws") {
		t.Errorf("/status still reports a workstation after unlink: %q", f.last())
	}
}

// Telegram appends @botname to commands in groups.
func TestCommandWithBotSuffix(t *testing.T) {
	f, c := newFakeTelegram(t)
	b := New(c, &fakeLinks{}, &fakeRunner{})
	b.handle(context.Background(), msg(42, "/help@example_bot"))
	if !strings.Contains(f.last(), "/link") {
		t.Errorf("/help@bot not routed: %q", f.last())
	}
}

// Agent output ends with the summary and errors, so the tail is the part worth keeping.
func TestTruncateKeepsTail(t *testing.T) {
	long := strings.Repeat("noise\n", 2000) + "TESTS PASSED"
	got := Truncate(long, 200)
	if len([]rune(got)) > 200 {
		t.Fatalf("truncate returned %d runes, want <=200", len([]rune(got)))
	}
	if !strings.Contains(got, "TESTS PASSED") {
		t.Error("truncation dropped the tail — the useful half of agent output")
	}
	if !strings.Contains(got, "truncated") {
		t.Error("truncation is silent; the user cannot tell output was cut")
	}
	if short := Truncate("hello", 200); short != "hello" {
		t.Errorf("short text was altered: %q", short)
	}
}

func TestAutonomyFlowsFromLink(t *testing.T) {
	for _, autonomous := range []bool{false, true} {
		_, c := newFakeTelegram(t)
		links := &fakeLinks{linked: true, link: store.ChatLink{SandboxID: "sb_ws", Autonomous: autonomous}}
		runner := &fakeRunner{out: "ok"}
		b := New(c, links, runner)

		b.handle(context.Background(), msg(42, "tidy up the imports"))

		if len(runner.autonomy) != 1 {
			t.Fatalf("autonomous=%v: ran %d tasks, want 1", autonomous, len(runner.autonomy))
		}
		if runner.autonomy[0] != autonomous {
			t.Errorf("autonomous=%v: runner got %v", autonomous, runner.autonomy[0])
		}
	}
}

func TestRiskyTaskWaitsForConfirmation(t *testing.T) {
	f, c := newFakeTelegram(t)
	links := &fakeLinks{linked: true, link: store.ChatLink{SandboxID: "sb_ws", Autonomous: true}}
	runner := &fakeRunner{out: "ok"}
	b := New(c, links, runner)

	b.handle(context.Background(), msg(42, "force push the release branch"))

	if len(runner.tasks) != 0 {
		t.Fatal("an irreversible task ran without confirmation")
	}
	if !strings.Contains(strings.ToLower(f.last()), "irreversible") {
		t.Errorf("no confirmation prompt: %q", f.last())
	}
}

func TestConfirmationTapRunsTask(t *testing.T) {
	_, c := newFakeTelegram(t)
	links := &fakeLinks{linked: true, link: store.ChatLink{SandboxID: "sb_ws", Autonomous: true}}
	runner := &fakeRunner{out: "pushed"}
	b := New(c, links, runner)

	b.handle(context.Background(), msg(42, "force push the release branch"))
	b.handle(context.Background(), callback(42, "go:1"))

	if len(runner.tasks) != 1 {
		t.Fatalf("ran %d tasks after confirming, want 1", len(runner.tasks))
	}
	if !strings.Contains(runner.tasks[0], "force push") {
		t.Errorf("ran the wrong task: %q", runner.tasks[0])
	}
}

func TestConfirmationCancel(t *testing.T) {
	f, c := newFakeTelegram(t)
	links := &fakeLinks{linked: true, link: store.ChatLink{SandboxID: "sb_ws", Autonomous: true}}
	runner := &fakeRunner{out: "ok"}
	b := New(c, links, runner)

	b.handle(context.Background(), msg(42, "rm -rf build"))
	b.handle(context.Background(), callback(42, "no:1"))

	if len(runner.tasks) != 0 {
		t.Fatal("a cancelled task ran anyway")
	}
	if !strings.Contains(strings.ToLower(f.last()), "cancelled") {
		t.Errorf("cancel not acknowledged: %q", f.last())
	}
}

func TestConfirmationIsChatScoped(t *testing.T) {
	_, c := newFakeTelegram(t)
	links := &fakeLinks{linked: true, link: store.ChatLink{SandboxID: "sb_ws", Autonomous: true}}
	runner := &fakeRunner{out: "ok"}
	b := New(c, links, runner)

	b.handle(context.Background(), msg(42, "drop table users"))
	b.handle(context.Background(), callback(999, "go:1")) // another chat taps

	if len(runner.tasks) != 0 {
		t.Fatal("a foreign chat confirmed someone else's irreversible task")
	}
}

func TestConfirmationRechecksLink(t *testing.T) {
	f, c := newFakeTelegram(t)
	links := &fakeLinks{linked: true, link: store.ChatLink{SandboxID: "sb_ws", Autonomous: true}}
	runner := &fakeRunner{out: "ok"}
	b := New(c, links, runner)

	b.handle(context.Background(), msg(42, "delete the repository"))
	links.linked = false // revoked while the confirmation sat there
	b.handle(context.Background(), callback(42, "go:1"))

	if len(runner.tasks) != 0 {
		t.Fatal("a revoked link still executed a confirmed task")
	}
	if !strings.Contains(strings.ToLower(f.last()), "no longer connected") {
		t.Errorf("revocation not explained: %q", f.last())
	}
}

// A non-autonomous link keeps the agent's own approval prompts.
func TestNoConfirmationWhenNotAutonomous(t *testing.T) {
	_, c := newFakeTelegram(t)
	links := &fakeLinks{linked: true, link: store.ChatLink{SandboxID: "sb_ws"}}
	runner := &fakeRunner{out: "ok"}
	b := New(c, links, runner)

	b.handle(context.Background(), msg(42, "rm -rf everything"))

	if len(runner.tasks) != 1 {
		t.Errorf("ran %d tasks, want 1 (no confirmation without autonomy)", len(runner.tasks))
	}
}

func TestIsRisky(t *testing.T) {
	risky := []string{
		"force push to main", "git push --force", "rm -rf /tmp/x",
		"drop table users", "deploy to production", "run the migration on prod",
		"git reset --hard origin/main", "delete the staging branch",
		"rotate the API keys", "publish the npm package",
	}
	for _, s := range risky {
		if !IsRisky(s) {
			t.Errorf("not flagged as risky: %q", s)
		}
	}
	safe := []string{
		"fix the failing test", "add a README", "what changed yesterday?",
		"run the tests", "summarise this repo", "format the code",
	}
	for _, s := range safe {
		if IsRisky(s) {
			t.Errorf("ordinary task flagged as risky: %q", s)
		}
	}
}

// A 409 Conflict means another consumer holds the bot token; it must not look like a network blip.
func TestConflictIsRecognised(t *testing.T) {
	if !isConflict(errors.New("getUpdates: telegram: Conflict: terminated by other getUpdates request")) {
		t.Error("a token conflict was not recognised")
	}
	if isConflict(errors.New("dial tcp: i/o timeout")) {
		t.Error("a network timeout was reported as a token conflict")
	}
	if isConflict(nil) {
		t.Error("nil reported as a conflict")
	}
}
