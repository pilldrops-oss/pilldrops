package telegram

import (
	"context"
	"strconv"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// Events is the tail a notifier follows.
type Events interface {
	EventsAfter(ctx context.Context, after int64, limit int) ([]store.EventRecord, error)
	LatestEventID(ctx context.Context) (int64, error)
	ChatLinksForSandbox(ctx context.Context, sandboxID string) ([]store.ChatLink, error)
}

// Sender delivers a notification to one chat.
type Sender interface {
	SendMessage(ctx context.Context, chatID int64, text string) (int64, error)
}

// Notifier pushes workstation events to the chats linked to them.
type Notifier struct {
	Events Events
	Send   Sender
	Logf   func(string, ...any)
	// Interval between polls.
	Interval time.Duration

	cursor int64
}

// NewNotifier builds a notifier.
func NewNotifier(ev Events, send Sender) *Notifier {
	return &Notifier{
		Events: ev, Send: send,
		Logf:     func(string, ...any) {},
		Interval: 20 * time.Second,
	}
}

// notifiable maps an event kind to its alert text. created/paused/resumed are
// left out as too noisy; destroyed is left out because Destroy revokes the
// chat links before recording the event, so nobody is left to notify.
var notifiable = map[string]string{
	"crash": "⚠️ Your workstation's app stopped responding.",
	"oom":   "⚠️ Your workstation ran out of memory — a process was killed.",
}

// Run polls until ctx is cancelled.
func (n *Notifier) Run(ctx context.Context) error {
	// Start at the newest event so a restart does not replay stale alerts.
	latest, err := n.Events.LatestEventID(ctx)
	if err != nil {
		n.Logf("notifier: read cursor: %v", err)
	}
	n.cursor = latest

	t := time.NewTicker(n.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			n.once(ctx)
		}
	}
}

// once drains one batch of events.
func (n *Notifier) once(ctx context.Context) {
	evs, err := n.Events.EventsAfter(ctx, n.cursor, 200)
	if err != nil {
		n.Logf("notifier: read events: %v", err)
		return
	}
	for _, ev := range evs {
		// Advance the cursor even for ignored events, or they are re-read on every poll.
		if ev.ID > n.cursor {
			n.cursor = ev.ID
		}
		text, ok := notifiable[ev.Event]
		if !ok {
			continue
		}
		n.fanOut(ctx, ev, text)
	}
}

// fanOut delivers one event to every chat linked to its sandbox.
func (n *Notifier) fanOut(ctx context.Context, ev store.EventRecord, text string) {
	links, err := n.Events.ChatLinksForSandbox(ctx, ev.SandboxID)
	if err != nil {
		n.Logf("notifier: links for %s: %v", ev.SandboxID, err)
		return
	}
	for _, l := range links {
		if l.Channel != Channel {
			continue // another channel's notifier will handle it
		}
		chatID, err := strconv.ParseInt(l.ChatID, 10, 64)
		if err != nil {
			continue
		}
		if _, err := n.Send.SendMessage(ctx, chatID, text+"\n"+ev.SandboxID); err != nil {
			n.Logf("notifier: send to %s: %v", l.ChatID, err)
		}
	}
}
