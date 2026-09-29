package discord

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/extras/bridge/telegram"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

type fakeSender struct {
	mu  sync.Mutex
	log []string // chronological: "send:chat|text" and "edit:chat|msg|text"
}

func (f *fakeSender) SendMessage(_ context.Context, chat, text string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, "send:"+chat+"|"+text)
	return "m1", nil
}
func (f *fakeSender) EditMessage(_ context.Context, chat, id, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, "edit:"+chat+"|"+id+"|"+text)
	return nil
}
func (f *fakeSender) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.log) == 0 {
		return ""
	}
	return f.log[len(f.log)-1]
}
func (f *fakeSender) edits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.log {
		if strings.HasPrefix(e, "edit:") {
			n++
		}
	}
	return n
}

type fakeLinks struct {
	links map[string]store.ChatLink // chatID → link
	codes map[string]store.ChatLink // code → link template
}

func (f *fakeLinks) RedeemLinkCode(ctx context.Context, code, channel, chatID string) (store.ChatLink, error) {
	l, ok := f.codes[code]
	if !ok {
		return store.ChatLink{}, errors.New("bad code")
	}
	delete(f.codes, code)
	l.Channel, l.ChatID = channel, chatID
	f.links[chatID] = l
	return l, nil
}
func (f *fakeLinks) ChatLinkFor(ctx context.Context, _, chatID string) (store.ChatLink, bool, error) {
	l, ok := f.links[chatID]
	return l, ok, nil
}
func (f *fakeLinks) DeleteChatLink(ctx context.Context, _, chatID string) error {
	delete(f.links, chatID)
	return nil
}

type fakeRunner struct {
	mu    sync.Mutex
	tasks []string
	out   string
	err   error
}

func (f *fakeRunner) Run(_ context.Context, sandboxID, task string, _ telegram.RunOpts) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tasks = append(f.tasks, sandboxID+"|"+task)
	return f.out, f.err
}

func newTestBridge(links *fakeLinks, runner *fakeRunner) (*Bridge, *fakeSender) {
	s := &fakeSender{}
	b := New(s, links, runner)
	return b, s
}

func msg(chat, text string) Message {
	m := Message{ChannelID: chat, Content: text}
	m.Author.ID = "user1"
	return m
}

func TestLinkRedeemsDashboardCode(t *testing.T) {
	links := &fakeLinks{
		links: map[string]store.ChatLink{},
		codes: map[string]store.ChatLink{"ABC123": {SandboxID: "sb1", OrgID: "org1"}},
	}
	b, s := newTestBridge(links, &fakeRunner{})

	b.Handle(context.Background(), msg("chan9", "!link abc123")) // case-insensitive
	if got := s.last(); !strings.Contains(got, "Connected to sb1") {
		t.Fatalf("link reply = %q", got)
	}
	if _, ok := links.links["chan9"]; !ok {
		t.Fatal("link not stored for the Discord channel")
	}
	// Single use: the same code again must fail.
	b.Handle(context.Background(), msg("chan10", "/link ABC123"))
	if got := s.last(); !strings.Contains(got, "didn't work") {
		t.Fatalf("reused code reply = %q", got)
	}
}

func TestUnlinkedChatIsToldToLink(t *testing.T) {
	b, s := newTestBridge(&fakeLinks{links: map[string]store.ChatLink{}}, &fakeRunner{})
	b.Handle(context.Background(), msg("c1", "fix the tests"))
	if got := s.last(); !strings.Contains(got, "isn't connected") {
		t.Fatalf("reply = %q", got)
	}
}

func TestTaskRunsAndAnswerReplacesAck(t *testing.T) {
	links := &fakeLinks{links: map[string]store.ChatLink{
		"c1": {SandboxID: "sb1", OrgID: "org1"},
	}}
	r := &fakeRunner{out: "did the thing"}
	b, s := newTestBridge(links, r)

	b.Handle(context.Background(), msg("c1", "fix the tests"))
	if len(r.tasks) != 1 || r.tasks[0] != "sb1|fix the tests" {
		t.Fatalf("runner saw %v", r.tasks)
	}
	if got := s.last(); !strings.Contains(got, "did the thing") {
		t.Fatalf("final reply = %q", got)
	}
	if s.edits() != 1 {
		t.Fatalf("answer should EDIT the ack, got %v", s.log)
	}
}

func TestReadOnlyLinkRefusesTasks(t *testing.T) {
	links := &fakeLinks{links: map[string]store.ChatLink{
		"c1": {SandboxID: "sb1", OrgID: "org1", ReadOnly: true},
	}}
	r := &fakeRunner{}
	b, s := newTestBridge(links, r)

	b.Handle(context.Background(), msg("c1", "rm -rf the repo"))
	if len(r.tasks) != 0 {
		t.Fatal("read-only link reached the runner")
	}
	if got := s.last(); !strings.Contains(got, "read-only") {
		t.Fatalf("reply = %q", got)
	}
}

func TestRateLimitBetweenTasks(t *testing.T) {
	links := &fakeLinks{links: map[string]store.ChatLink{
		"c1": {SandboxID: "sb1", OrgID: "org1"},
	}}
	r := &fakeRunner{out: "ok"}
	b, s := newTestBridge(links, r)

	b.Handle(context.Background(), msg("c1", "task one"))
	b.Handle(context.Background(), msg("c1", "task two")) // immediately after
	if len(r.tasks) != 1 {
		t.Fatalf("second task should be rate-limited, runner saw %v", r.tasks)
	}
	if got := s.last(); !strings.Contains(got, "Give me a moment") {
		t.Fatalf("rate-limit reply = %q", got)
	}
}

func TestRiskyTaskOnAutonomousLinkNeedsYes(t *testing.T) {
	links := &fakeLinks{links: map[string]store.ChatLink{
		"c1": {SandboxID: "sb1", OrgID: "org1", Autonomous: true},
	}}
	r := &fakeRunner{out: "gone"}
	b, s := newTestBridge(links, r)

	b.Handle(context.Background(), msg("c1", "drop database prod_users"))
	if len(r.tasks) != 0 {
		t.Fatal("risky task ran without confirmation")
	}
	if got := s.last(); !strings.Contains(got, "irreversible") {
		t.Fatalf("confirm prompt = %q", got)
	}

	b.Handle(context.Background(), msg("c1", "yes"))
	if len(r.tasks) != 1 {
		t.Fatalf("confirmed task did not run: %v", r.tasks)
	}
}

func TestAnythingButYesCancels(t *testing.T) {
	links := &fakeLinks{links: map[string]store.ChatLink{
		"c1": {SandboxID: "sb1", OrgID: "org1", Autonomous: true},
	}}
	r := &fakeRunner{}
	b, s := newTestBridge(links, r)

	b.Handle(context.Background(), msg("c1", "delete the repo entirely"))
	b.Handle(context.Background(), msg("c1", "hmm actually wait"))
	if len(r.tasks) != 0 {
		t.Fatal("cancelled task ran anyway")
	}
	if got := s.last(); !strings.Contains(got, "Cancelled") {
		t.Fatalf("cancel reply = %q", got)
	}
}

func TestYesAfterRevocationDoesNotRun(t *testing.T) {
	links := &fakeLinks{links: map[string]store.ChatLink{
		"c1": {SandboxID: "sb1", OrgID: "org1", Autonomous: true},
	}}
	r := &fakeRunner{}
	b, s := newTestBridge(links, r)

	b.Handle(context.Background(), msg("c1", "run rm -rf /workspace"))
	delete(links.links, "c1") // revoked from the dashboard meanwhile
	b.Handle(context.Background(), msg("c1", "yes"))
	if len(r.tasks) != 0 {
		t.Fatal("revoked link still ran the parked task")
	}
	if got := s.last(); !strings.Contains(got, "no longer connected") {
		t.Fatalf("reply = %q", got)
	}
}

// Non-autonomous links keep the agent's own approval prompts, so the bridge adds none.
func TestRiskyTaskWithoutAutonomyRunsDirectly(t *testing.T) {
	links := &fakeLinks{links: map[string]store.ChatLink{
		"c1": {SandboxID: "sb1", OrgID: "org1", Autonomous: false},
	}}
	r := &fakeRunner{out: "reported, did not act"}
	b, _ := newTestBridge(links, r)

	b.Handle(context.Background(), msg("c1", "drop the production database"))
	if len(r.tasks) != 1 {
		t.Fatalf("non-autonomous risky task should run (agent keeps its own prompts): %v", r.tasks)
	}
}

func TestStatusAndUnlink(t *testing.T) {
	links := &fakeLinks{links: map[string]store.ChatLink{
		"c1": {SandboxID: "sb1", OrgID: "org1", ReadOnly: true},
	}}
	b, s := newTestBridge(links, &fakeRunner{})

	b.Handle(context.Background(), msg("c1", "!status"))
	if got := s.last(); !strings.Contains(got, "sb1") || !strings.Contains(got, "read-only") {
		t.Fatalf("status = %q", got)
	}
	b.Handle(context.Background(), msg("c1", "!unlink"))
	if _, ok := links.links["c1"]; ok {
		t.Fatal("unlink did not delete the link")
	}
}

func TestBotAndEmptyMessagesIgnored(t *testing.T) {
	b, s := newTestBridge(&fakeLinks{links: map[string]store.ChatLink{}}, &fakeRunner{})
	b.Handle(context.Background(), msg("c1", "   "))
	if s.last() != "" {
		t.Fatalf("empty message produced a reply: %q", s.last())
	}
}
