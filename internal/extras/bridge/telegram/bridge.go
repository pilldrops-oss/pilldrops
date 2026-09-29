package telegram

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// Links is the chat-link store surface the bridge needs.
type Links interface {
	RedeemLinkCode(ctx context.Context, code, channel, chatID string) (store.ChatLink, error)
	ChatLinkFor(ctx context.Context, channel, chatID string) (store.ChatLink, bool, error)
	DeleteChatLink(ctx context.Context, channel, chatID string) error
}

// Channel is the link channel this bridge owns.
const Channel = "telegram"

// Bridge polls Telegram and drives workstations.
type Bridge struct {
	Client *Client
	Links  Links
	Runner Runner
	// Voice transcribes voice notes on the workstation; nil means voice is off.
	Voice *Transcriber
	Logf  func(string, ...any)

	// TaskTimeout bounds one agent run so an unbounded run cannot eat a month's hours.
	TaskTimeout time.Duration

	mu sync.Mutex
	// seen records chats that have run a task in this process; the first message
	// opens a fresh conversation and later ones continue it. In-memory, so a
	// restart starts a new conversation.
	seen map[string]bool
	// lastAt rate-limits per chat.
	lastAt map[string]time.Time
	// awaiting holds tasks parked for a confirmation tap.
	awaiting *pendingSet
	// conflictOnce logs the token-conflict explanation once instead of every poll.
	conflictOnce sync.Once
}

// isConflict reports Telegram's 409: another consumer holds this bot token.
func isConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Conflict")
}

// minInterval floors the gap between tasks from one chat; every accepted message spends the customer's hours.
const minInterval = 3 * time.Second

// New builds a bridge with defaults.
func New(c *Client, links Links, runner Runner) *Bridge {
	return &Bridge{
		Client: c, Links: links, Runner: runner,
		Logf:        func(string, ...any) {},
		TaskTimeout: 10 * time.Minute,
		seen:        map[string]bool{},
		lastAt:      map[string]time.Time{},
		awaiting:    newPendingSet(),
	}
}

// Run polls until ctx is cancelled. Updates are handled synchronously because
// two concurrent tasks on one workstation would fight over the working tree.
func (b *Bridge) Run(ctx context.Context) error {
	var offset int64
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		updates, err := b.Client.GetUpdates(ctx, offset, 30)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// 409 Conflict means another process holds this bot token and gets
			// every message. Keep retrying, since it may stop, but log it once.
			if isConflict(err) {
				b.conflictOnce.Do(func() {
					b.Logf("CONFLICT: another process is polling this bot token — " +
						"every message is going to it, not here. Stop that consumer " +
						"or give this bridge its own bot (@BotFather /newbot).")
				})
			} else {
				b.Logf("getUpdates: %v", err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
			continue
		}
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1 // ack: Telegram drops updates below the offset
			}
			b.handle(ctx, u)
		}
	}
}

// handle dispatches one update. Failures are reported into the chat, not returned.
func (b *Bridge) handle(ctx context.Context, u Update) {
	if u.CallbackQuery != nil {
		b.callback(ctx, u)
		return
	}
	if u.Message == nil {
		return
	}
	chatID := u.ChatID()
	chat := fmt.Sprint(chatID)
	text := strings.TrimSpace(u.Message.Text)

	if u.Message.Voice != nil && text == "" {
		text = b.transcribe(ctx, chatID, chat, u.Message.Voice.FileID, u.Message.Voice.Duration)
		if text == "" {
			return // already explained in-chat
		}
	}
	if text == "" {
		return
	}

	if strings.HasPrefix(text, "/") {
		b.command(ctx, chatID, chat, text)
		return
	}
	b.task(ctx, chatID, chat, text)
}

func (b *Bridge) command(ctx context.Context, chatID int64, chat, text string) {
	cmd, arg, _ := strings.Cut(text, " ")
	// Telegram appends @botname to commands in groups.
	cmd, _, _ = strings.Cut(cmd, "@")
	arg = strings.TrimSpace(arg)

	switch cmd {
	case "/start", "/help":
		// t.me/<bot>?start=CODE delivers "/start CODE", so a dashboard link connects a chat in one tap.
		if cmd == "/start" && arg != "" {
			b.link(ctx, chatID, chat, arg)
			return
		}
		b.reply(ctx, chatID, "Your coding agent, in your pocket.\n\n"+
			"/link CODE — connect this chat to a workstation (get a code in the dashboard)\n"+
			"/status — which workstation this chat is connected to\n"+
			"/unlink — disconnect\n\n"+
			"Once linked, just send a message and the agent gets to work.")

	case "/link":
		if arg == "" {
			b.reply(ctx, chatID, "Send /link followed by the code from your dashboard.")
			return
		}
		b.link(ctx, chatID, chat, arg)

	case "/status":
		link, ok, err := b.Links.ChatLinkFor(ctx, Channel, chat)
		if err != nil || !ok {
			b.reply(ctx, chatID, "This chat isn't connected yet. Use /link with a code from your dashboard.")
			return
		}
		mode := "read-write"
		if link.ReadOnly {
			mode = "read-only"
		}
		b.reply(ctx, chatID, fmt.Sprintf("Connected to %s (%s).", link.SandboxID, mode))

	case "/unlink":
		if err := b.Links.DeleteChatLink(ctx, Channel, chat); err != nil {
			b.reply(ctx, chatID, "Couldn't disconnect — try again.")
			return
		}
		b.mu.Lock()
		delete(b.seen, chat)
		b.mu.Unlock()
		b.reply(ctx, chatID, "Disconnected. Your workstation is untouched.")

	default:
		b.reply(ctx, chatID, "I don't know that command. Try /help.")
	}
}

func (b *Bridge) task(ctx context.Context, chatID int64, chat, text string) {
	link, ok, err := b.Links.ChatLinkFor(ctx, Channel, chat)
	if err != nil || !ok {
		b.reply(ctx, chatID, "This chat isn't connected to a workstation yet. Use /link with a code from your dashboard.")
		return
	}
	if link.ReadOnly {
		b.reply(ctx, chatID, "This link is read-only, so I can't run tasks. Re-link with write access from the dashboard.")
		return
	}
	if !b.allow(chat) {
		b.reply(ctx, chatID, "Give me a moment — still working on the last one.")
		return
	}

	b.mu.Lock()
	followUp := b.seen[chat]
	b.mu.Unlock()

	// Only autonomous links can act destructively; a non-autonomous link keeps
	// the agent's own approval prompts.
	if link.Autonomous && IsRisky(text) {
		b.confirm(ctx, chatID, chat, text, followUp)
		return
	}
	b.execute(ctx, chatID, chat, link, text, followUp)
}

// confirm parks a risky task behind a button tap.
func (b *Bridge) confirm(ctx context.Context, chatID int64, chat, text string, followUp bool) {
	id := b.awaiting.add(chat, text, followUp, time.Now())
	_, err := b.Client.SendButtons(ctx, chatID,
		"That looks irreversible:\n\n"+Truncate(text, 500)+"\n\nRun it?",
		[]Button{{Text: "Run it", Data: "go:" + id}, {Text: "Cancel", Data: "no:" + id}})
	if err != nil {
		b.Logf("confirm prompt to %s: %v", chat, err)
	}
}

// callback handles a button tap on a confirmation.
func (b *Bridge) callback(ctx context.Context, u Update) {
	q := u.CallbackQuery
	chatID := u.ChatID()
	chat := fmt.Sprint(chatID)
	action, id, _ := strings.Cut(q.Data, ":")

	// take requires the chat to match, so a forged callback cannot run someone else's parked task.
	p, ok := b.awaiting.take(id, chat, time.Now())
	if !ok {
		_ = b.Client.AnswerCallback(ctx, q.ID, "That confirmation has expired.")
		return
	}
	if action != "go" {
		_ = b.Client.AnswerCallback(ctx, q.ID, "Cancelled.")
		b.reply(ctx, chatID, "Cancelled — nothing ran.")
		return
	}
	_ = b.Client.AnswerCallback(ctx, q.ID, "Running…")

	// Re-read the link so a tap cannot resurrect permission revoked since the prompt.
	link, ok, err := b.Links.ChatLinkFor(ctx, Channel, chat)
	if err != nil || !ok || link.ReadOnly {
		b.reply(ctx, chatID, "That workstation is no longer connected.")
		return
	}
	b.execute(ctx, chatID, chat, link, p.task, p.followUp)
}

// execute runs an approved task and reports the result.
func (b *Bridge) execute(ctx context.Context, chatID int64, chat string, link store.ChatLink, text string, followUp bool) {
	msgID, err := b.Client.SendMessage(ctx, chatID, "Waking your workstation…")
	if err != nil {
		b.Logf("send ack to %s: %v", chat, err)
		return
	}
	b.mu.Lock()
	b.seen[chat] = true
	b.mu.Unlock()

	// The runner must act as the customer's org, not as the operator whose key it holds.
	runCtx, cancel := context.WithTimeout(WithOrg(ctx, link.OrgID), b.TaskTimeout)
	defer cancel()
	out, err := b.Runner.Run(runCtx, link.SandboxID, text, RunOpts{
		FollowUp: followUp, Autonomous: link.Autonomous,
	})
	switch {
	case err != nil:
		b.Logf("task on %s: %v", link.SandboxID, err)
		b.edit(ctx, chatID, msgID, "That didn't finish: "+err.Error())
	case strings.TrimSpace(out) == "":
		b.edit(ctx, chatID, msgID, "Done — the agent produced no output.")
	default:
		b.edit(ctx, chatID, msgID, out)
	}
}

// allow applies the per-chat rate limit and records the attempt.
func (b *Bridge) allow(chat string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if last, ok := b.lastAt[chat]; ok && now.Sub(last) < minInterval {
		return false
	}
	b.lastAt[chat] = now
	return true
}

func (b *Bridge) reply(ctx context.Context, chatID int64, text string) {
	if _, err := b.Client.SendMessage(ctx, chatID, text); err != nil {
		b.Logf("reply to %d: %v", chatID, err)
	}
}

func (b *Bridge) edit(ctx context.Context, chatID, msgID int64, text string) {
	if err := b.Client.EditMessage(ctx, chatID, msgID, text); err != nil {
		// The edit can fail (message too old or deleted), so fall back to a new message.
		b.Logf("edit %d/%d: %v", chatID, msgID, err)
		b.reply(ctx, chatID, text)
	}
}

// link redeems a code and binds this chat. Shared by /link and the t.me deep link.
func (b *Bridge) link(ctx context.Context, chatID int64, chat, code string) {
	l, err := b.Links.RedeemLinkCode(ctx, code, Channel, chat)
	if err != nil {
		b.reply(ctx, chatID, err.Error())
		return
	}
	b.mu.Lock()
	delete(b.seen, chat) // a fresh link starts a fresh conversation
	b.mu.Unlock()
	mode := ""
	if l.ReadOnly {
		mode = "\n\nThis link is read-only: the agent can look and report, but not change anything."
	}
	b.reply(ctx, chatID, "Connected to "+l.SandboxID+"."+mode+"\n\nSend me a task.")
}

// transcribe turns a voice note into task text, reporting any failure into the
// chat. Returns "" when there is nothing to run.
func (b *Bridge) transcribe(ctx context.Context, chatID int64, chat, fileID string, secs int) string {
	if b.Voice == nil {
		b.reply(ctx, chatID, "Voice isn't enabled here — send it as text.")
		return ""
	}
	link, ok, err := b.Links.ChatLinkFor(ctx, Channel, chat)
	if err != nil || !ok {
		b.reply(ctx, chatID, "This chat isn't connected to a workstation yet. Use /link with a code from your dashboard.")
		return ""
	}
	// Transcription wakes the workstation and costs the customer seconds, so say so.
	b.reply(ctx, chatID, "Listening…")

	tctx, cancel := context.WithTimeout(WithOrg(ctx, link.OrgID), 3*time.Minute)
	defer cancel()
	text, err := b.Voice.Transcribe(tctx, link.SandboxID, fileID, secs)
	if err != nil {
		b.Logf("transcribe for %s: %v", chat, err)
		b.reply(ctx, chatID, err.Error())
		return ""
	}
	// Echo the transcript so a misheard instruction is visible.
	b.reply(ctx, chatID, "Heard: "+text)
	return text
}
