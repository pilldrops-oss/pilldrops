package telegram

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

type fakeEvents struct {
	mu     sync.Mutex
	evs    []store.EventRecord
	links  map[string][]store.ChatLink
	err    error
	latest int64
}

func (f *fakeEvents) EventsAfter(ctx context.Context, after int64, limit int) ([]store.EventRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var out []store.EventRecord
	for _, e := range f.evs {
		if e.ID > after {
			out = append(out, e)
		}
	}
	return out, nil
}
func (f *fakeEvents) LatestEventID(ctx context.Context) (int64, error) { return f.latest, nil }
func (f *fakeEvents) ChatLinksForSandbox(ctx context.Context, id string) ([]store.ChatLink, error) {
	return f.links[id], nil
}

type fakeSender struct {
	mu   sync.Mutex
	sent []string
	err  error
}

func (s *fakeSender) SendMessage(_ context.Context, chatID int64, text string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, s.err
	}
	s.sent = append(s.sent, text)
	return 1, nil
}
func (s *fakeSender) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}

func linkTo(sandbox, chat string) store.ChatLink {
	return store.ChatLink{Channel: "telegram", ChatID: chat, SandboxID: sandbox}
}

func TestNotifierPushesCrash(t *testing.T) {
	ev := &fakeEvents{
		evs:   []store.EventRecord{{ID: 1, SandboxID: "sb_1", Event: "crash"}},
		links: map[string][]store.ChatLink{"sb_1": {linkTo("sb_1", "42")}},
	}
	send := &fakeSender{}
	n := NewNotifier(ev, send)
	n.once(context.Background())

	got := send.messages()
	if len(got) != 1 {
		t.Fatalf("sent %d messages, want 1", len(got))
	}
	if !strings.Contains(got[0], "sb_1") {
		t.Errorf("alert does not name the workstation: %q", got[0])
	}
}

func TestNotifierIgnoresRoutineLifecycle(t *testing.T) {
	ev := &fakeEvents{
		evs: []store.EventRecord{
			{ID: 1, SandboxID: "sb_1", Event: "created"},
			{ID: 2, SandboxID: "sb_1", Event: "paused"},
			{ID: 3, SandboxID: "sb_1", Event: "resumed"},
		},
		links: map[string][]store.ChatLink{"sb_1": {linkTo("sb_1", "42")}},
	}
	send := &fakeSender{}
	n := NewNotifier(ev, send)
	n.once(context.Background())

	if got := send.messages(); len(got) != 0 {
		t.Errorf("pushed routine lifecycle noise: %v", got)
	}
}

func TestNotifierCursorAdvancesPastIgnoredEvents(t *testing.T) {
	ev := &fakeEvents{
		evs: []store.EventRecord{
			{ID: 1, SandboxID: "sb_1", Event: "paused"},
			{ID: 2, SandboxID: "sb_1", Event: "crash"},
		},
		links: map[string][]store.ChatLink{"sb_1": {linkTo("sb_1", "42")}},
	}
	send := &fakeSender{}
	n := NewNotifier(ev, send)
	n.once(context.Background())
	if n.cursor != 2 {
		t.Fatalf("cursor = %d, want 2", n.cursor)
	}
	n.once(context.Background())
	if got := send.messages(); len(got) != 1 {
		t.Errorf("resent alerts on a second poll: %v", got)
	}
}

func TestNotifierUnlinkedSandboxIsQuiet(t *testing.T) {
	ev := &fakeEvents{
		evs:   []store.EventRecord{{ID: 1, SandboxID: "sb_lonely", Event: "crash"}},
		links: map[string][]store.ChatLink{},
	}
	send := &fakeSender{}
	n := NewNotifier(ev, send)
	n.once(context.Background())
	if got := send.messages(); len(got) != 0 {
		t.Errorf("notified nobody's chat: %v", got)
	}
	if n.cursor != 1 {
		t.Errorf("cursor = %d, want 1", n.cursor)
	}
}

func TestNotifierFansOutToEveryLink(t *testing.T) {
	ev := &fakeEvents{
		evs:   []store.EventRecord{{ID: 1, SandboxID: "sb_1", Event: "oom"}},
		links: map[string][]store.ChatLink{"sb_1": {linkTo("sb_1", "42"), linkTo("sb_1", "43")}},
	}
	send := &fakeSender{}
	n := NewNotifier(ev, send)
	n.once(context.Background())
	if got := send.messages(); len(got) != 2 {
		t.Errorf("sent %d messages, want 2 (one per linked phone)", len(got))
	}
}

func TestNotifierSkipsOtherChannels(t *testing.T) {
	l := linkTo("sb_1", "42")
	l.Channel = "whatsapp"
	ev := &fakeEvents{
		evs:   []store.EventRecord{{ID: 1, SandboxID: "sb_1", Event: "crash"}},
		links: map[string][]store.ChatLink{"sb_1": {l}},
	}
	send := &fakeSender{}
	n := NewNotifier(ev, send)
	n.once(context.Background())
	if got := send.messages(); len(got) != 0 {
		t.Errorf("telegram notifier delivered a whatsapp link: %v", got)
	}
}

// Advancing the cursor on a store failure would swallow every alert in the batch.
func TestNotifierStoreErrorDoesNotAdvanceCursor(t *testing.T) {
	ev := &fakeEvents{err: errors.New("db down")}
	n := NewNotifier(ev, &fakeSender{})
	n.once(context.Background())
	if n.cursor != 0 {
		t.Errorf("cursor advanced past an unread batch: %d", n.cursor)
	}
}

// Destroy revokes chat links before recording the event, so a destroyed alert could never fire.
func TestDestroyedIsNotNotifiable(t *testing.T) {
	if _, ok := notifiable["destroyed"]; ok {
		t.Error("destroyed is listed as notifiable, but its links are already revoked when the event lands")
	}
}
