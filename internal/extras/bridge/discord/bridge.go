package discord

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/extras/bridge/telegram"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// Links is the chat-link store surface the bridge needs.
type Links interface {
	RedeemLinkCode(ctx context.Context, code, channel, chatID string) (store.ChatLink, error)
	ChatLinkFor(ctx context.Context, channel, chatID string) (store.ChatLink, bool, error)
	DeleteChatLink(ctx context.Context, channel, chatID string) error
}

// Sender is the outbound half of the REST client.
type Sender interface {
	SendMessage(ctx context.Context, channelID, text string) (string, error)
	EditMessage(ctx context.Context, channelID, messageID, text string) error
}

// minInterval floors the gap between tasks from one chat; every accepted message spends the customer's hours.
const minInterval = 3 * time.Second

// confirmTTL bounds how long a risky task waits for its typed "yes".
const confirmTTL = 2 * time.Minute

// discordMsgCap is Discord's 2000-char content limit minus room for the truncation marker.
const discordMsgCap = 1900

type pending struct {
	task     string
	followUp bool
	at       time.Time
}

// Bridge turns Discord messages into agent runs, with the same pairing,
// rate-limit, read-only and confirmation rules as the Telegram bridge.
type Bridge struct {
	Send   Sender
	Links  Links
	Runner telegram.Runner
	Logf   func(string, ...any)

	TaskTimeout time.Duration

	mu       sync.Mutex
	seen     map[string]bool
	lastAt   map[string]time.Time
	awaiting map[string]pending
}

func New(send Sender, links Links, runner telegram.Runner) *Bridge {
	return &Bridge{
		Send: send, Links: links, Runner: runner,
		Logf:        func(string, ...any) {},
		TaskTimeout: 10 * time.Minute,
		seen:        map[string]bool{},
		lastAt:      map[string]time.Time{},
		awaiting:    map[string]pending{},
	}
}

// Handle processes one inbound Discord message. Failures are reported into the chat, not returned.
func (b *Bridge) Handle(ctx context.Context, m Message) {
	text := strings.TrimSpace(m.Content)
	if text == "" {
		return
	}
	chat := m.ChannelID

	// "!link CODE" is the documented form, but "/link" typed as plain text
	// still arrives as a message since this bot registers no slash commands.
	if strings.HasPrefix(text, "!") || strings.HasPrefix(text, "/") {
		b.command(ctx, chat, text)
		return
	}

	// A parked risky task consumes the next message as its verdict.
	b.mu.Lock()
	p, waiting := b.awaiting[chat]
	if waiting {
		delete(b.awaiting, chat)
	}
	b.mu.Unlock()
	if waiting {
		if time.Since(p.at) > confirmTTL {
			b.say(ctx, chat, "That confirmation expired — send the task again if you still want it.")
			return
		}
		if isYes(text) {
			// Re-read the link so a yes cannot resurrect permission revoked since the prompt.
			link, ok, err := b.Links.ChatLinkFor(ctx, Channel, chat)
			if err != nil || !ok || link.ReadOnly {
				b.say(ctx, chat, "That workstation is no longer connected.")
				return
			}
			b.execute(ctx, chat, link, p.task, p.followUp)
			return
		}
		b.say(ctx, chat, "Cancelled — nothing ran.")
		return
	}

	b.task(ctx, chat, text)
}

func isYes(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "yes", "y", "run it", "confirm", "do it":
		return true
	}
	return false
}

func (b *Bridge) command(ctx context.Context, chat, text string) {
	cmd, arg, _ := strings.Cut(strings.TrimLeft(text, "!/"), " ")
	arg = strings.TrimSpace(arg)

	switch strings.ToLower(cmd) {
	case "start", "help":
		b.say(ctx, chat, "Your coding agent, in your pocket.\n\n"+
			"`!link CODE` — connect this chat to a workstation (get a code in the dashboard)\n"+
			"`!status` — which workstation this chat is connected to\n"+
			"`!unlink` — disconnect\n\n"+
			"Once linked, just send a message and the agent gets to work.")

	case "link":
		if arg == "" {
			b.say(ctx, chat, "Send `!link` followed by the code from your dashboard.")
			return
		}
		link, err := b.Links.RedeemLinkCode(ctx, strings.ToUpper(arg), Channel, chat)
		if err != nil {
			b.say(ctx, chat, "That code didn't work — codes are single-use and expire after 10 minutes. Mint a fresh one in the dashboard.")
			return
		}
		mode := "It can read, write and run tasks."
		if link.ReadOnly {
			mode = "This link is read-only."
		}
		b.say(ctx, chat, "Connected to "+link.SandboxID+". "+mode+" Just type what you want done.")

	case "status":
		link, ok, err := b.Links.ChatLinkFor(ctx, Channel, chat)
		if err != nil || !ok {
			b.say(ctx, chat, "This chat isn't connected yet. Use `!link` with a code from your dashboard.")
			return
		}
		mode := "read-write"
		if link.ReadOnly {
			mode = "read-only"
		}
		b.say(ctx, chat, "Connected to "+link.SandboxID+" ("+mode+").")

	case "unlink":
		if err := b.Links.DeleteChatLink(ctx, Channel, chat); err != nil {
			b.say(ctx, chat, "Couldn't disconnect — try again.")
			return
		}
		b.mu.Lock()
		delete(b.seen, chat)
		b.mu.Unlock()
		b.say(ctx, chat, "Disconnected. Your workstation is untouched.")

	default:
		b.say(ctx, chat, "I don't know that command. Try `!help`.")
	}
}

func (b *Bridge) task(ctx context.Context, chat, text string) {
	link, ok, err := b.Links.ChatLinkFor(ctx, Channel, chat)
	if err != nil || !ok {
		b.say(ctx, chat, "This chat isn't connected to a workstation yet. Use `!link` with a code from your dashboard.")
		return
	}
	if link.ReadOnly {
		b.say(ctx, chat, "This link is read-only, so I can't run tasks. Re-link with write access from the dashboard.")
		return
	}
	if !b.allow(chat) {
		b.say(ctx, chat, "Give me a moment — still working on the last one.")
		return
	}

	b.mu.Lock()
	followUp := b.seen[chat]
	b.mu.Unlock()

	// Only autonomous links can act destructively, so only they need confirmation.
	if link.Autonomous && telegram.IsRisky(text) {
		b.mu.Lock()
		b.awaiting[chat] = pending{task: text, followUp: followUp, at: time.Now()}
		b.mu.Unlock()
		b.say(ctx, chat, "That looks irreversible:\n\n"+telegram.Truncate(text, 500)+
			"\n\nReply **yes** to run it — anything else cancels.")
		return
	}
	b.execute(ctx, chat, link, text, followUp)
}

func (b *Bridge) execute(ctx context.Context, chat string, link store.ChatLink, text string, followUp bool) {
	ackID, err := b.Send.SendMessage(ctx, chat, "Waking your workstation…")
	if err != nil {
		b.Logf("send ack to %s: %v", chat, err)
		return
	}
	b.mu.Lock()
	b.seen[chat] = true
	b.mu.Unlock()

	runCtx, cancel := context.WithTimeout(telegram.WithOrg(ctx, link.OrgID), b.TaskTimeout)
	defer cancel()
	out, err := b.Runner.Run(runCtx, link.SandboxID, text, telegram.RunOpts{
		FollowUp: followUp, Autonomous: link.Autonomous,
	})
	switch {
	case err != nil:
		b.Logf("task on %s: %v", link.SandboxID, err)
		b.editOrSay(ctx, chat, ackID, "That didn't finish: "+err.Error())
	case strings.TrimSpace(out) == "":
		b.editOrSay(ctx, chat, ackID, "Done — the agent produced no output.")
	default:
		b.editOrSay(ctx, chat, ackID, telegram.Truncate(out, discordMsgCap))
	}
}

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

func (b *Bridge) say(ctx context.Context, chat, text string) {
	if _, err := b.Send.SendMessage(ctx, chat, text); err != nil {
		b.Logf("send to %s: %v", chat, err)
	}
}

// editOrSay rewrites the ack into the answer, sending a fresh message when the edit fails.
func (b *Bridge) editOrSay(ctx context.Context, chat, msgID, text string) {
	if err := b.Send.EditMessage(ctx, chat, msgID, text); err != nil {
		b.Logf("edit %s in %s: %v — sending fresh", msgID, chat, err)
		b.say(ctx, chat, text)
	}
}
