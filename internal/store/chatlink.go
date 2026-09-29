package store

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
	"time"
)

// Chat links bind a messaging account (Telegram, WhatsApp, Slack) to one
// sandbox. A link is a credential: whoever holds it can run commands on the
// customer's machine.

// LinkCodeTTL bounds how long an unredeemed code stays valid.
const LinkCodeTTL = 10 * time.Minute

// ChatLink is one bound (channel, chat) → sandbox pair.
type ChatLink struct {
	Channel   string `json:"channel"` // "telegram" | "whatsapp" | "slack"
	ChatID    string `json:"chat_id"`
	SandboxID string `json:"sandbox_id"`
	OrgID     string `json:"org_id"`
	// ReadOnly links may report but never write or execute.
	ReadOnly bool `json:"read_only"`
	// Autonomous grants the agent its --dangerously-* flags; off by default.
	Autonomous bool  `json:"autonomous"`
	CreatedAt  int64 `json:"created_at"`
}

// newLinkCode returns a short single-use code. Base32 avoids the 0/O and 1/l
// confusions when a human retypes it.
func newLinkCode() (string, error) {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return strings.ToUpper(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)), nil
}

// CreateLinkCode mints a one-time code that binds a chat to this sandbox when redeemed.
func (s *Store) CreateLinkCode(ctx context.Context, sandboxID, orgID string, readOnly, autonomous bool) (string, error) {
	if sandboxID == "" || orgID == "" {
		return "", fmt.Errorf("sandbox and org are required")
	}
	code, err := newLinkCode()
	if err != nil {
		return "", err
	}
	now := time.Now().Unix()
	_, err = s.exec(ctx,
		`INSERT INTO chat_link_codes (code, sandbox_id, org_id, read_only, autonomous, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		code, sandboxID, orgID, readOnly, autonomous, now, now+int64(LinkCodeTTL.Seconds()),
	)
	if err != nil {
		return "", err
	}
	return code, nil
}

// RedeemLinkCode consumes a code and binds it to (channel, chatID). The code is
// deleted whether or not the bind succeeds. Rebinding an already-linked chat
// replaces the old link.
func (s *Store) RedeemLinkCode(ctx context.Context, code, channel, chatID string) (ChatLink, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" || channel == "" || chatID == "" {
		return ChatLink{}, fmt.Errorf("code, channel and chat are required")
	}
	var (
		l                    ChatLink
		readOnly, autonomous bool
		expiresAt            int64
	)
	err := s.queryRow(ctx,
		`SELECT sandbox_id, org_id, read_only, autonomous, expires_at FROM chat_link_codes WHERE code = ?`, code,
	).Scan(&l.SandboxID, &l.OrgID, &readOnly, &autonomous, &expiresAt)
	if err != nil {
		return ChatLink{}, fmt.Errorf("that code is not valid")
	}
	// Burn the code before checking expiry so it can never be replayed.
	if _, derr := s.exec(ctx, `DELETE FROM chat_link_codes WHERE code = ?`, code); derr != nil {
		return ChatLink{}, derr
	}
	if time.Now().Unix() > expiresAt {
		return ChatLink{}, fmt.Errorf("that code has expired — generate a new one")
	}

	l.Channel, l.ChatID = channel, chatID
	l.ReadOnly, l.Autonomous, l.CreatedAt = readOnly, autonomous, time.Now().Unix()
	if _, err := s.exec(ctx, `DELETE FROM chat_links WHERE channel = ? AND chat_id = ?`, channel, chatID); err != nil {
		return ChatLink{}, err
	}
	if _, err := s.exec(ctx,
		`INSERT INTO chat_links (channel, chat_id, sandbox_id, org_id, read_only, autonomous, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		l.Channel, l.ChatID, l.SandboxID, l.OrgID, l.ReadOnly, l.Autonomous, l.CreatedAt,
	); err != nil {
		return ChatLink{}, err
	}
	return l, nil
}

// ChatLinkFor returns the link bound to a chat, ok=false when unlinked.
func (s *Store) ChatLinkFor(ctx context.Context, channel, chatID string) (ChatLink, bool, error) {
	var (
		l                    ChatLink
		readOnly, autonomous bool
	)
	err := s.queryRow(ctx,
		`SELECT channel, chat_id, sandbox_id, org_id, read_only, autonomous, created_at
		 FROM chat_links WHERE channel = ? AND chat_id = ?`, channel, chatID,
	).Scan(&l.Channel, &l.ChatID, &l.SandboxID, &l.OrgID, &readOnly, &autonomous, &l.CreatedAt)
	if err != nil {
		return ChatLink{}, false, nil
	}
	l.ReadOnly, l.Autonomous = readOnly, autonomous
	return l, true, nil
}

// ChatLinksForSandbox returns every messaging link bound to a sandbox.
func (s *Store) ChatLinksForSandbox(ctx context.Context, sandboxID string) ([]ChatLink, error) {
	rows, err := s.query(ctx,
		`SELECT channel, chat_id, sandbox_id, org_id, read_only, autonomous, created_at
		 FROM chat_links WHERE sandbox_id = ?`, sandboxID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatLink
	for rows.Next() {
		var (
			l                    ChatLink
			readOnly, autonomous bool
		)
		if err := rows.Scan(&l.Channel, &l.ChatID, &l.SandboxID, &l.OrgID, &readOnly, &autonomous, &l.CreatedAt); err != nil {
			return nil, err
		}
		l.ReadOnly, l.Autonomous = readOnly, autonomous
		out = append(out, l)
	}
	return out, rows.Err()
}

// DeleteChatLink revokes a link. Idempotent.
func (s *Store) DeleteChatLink(ctx context.Context, channel, chatID string) error {
	_, err := s.exec(ctx, `DELETE FROM chat_links WHERE channel = ? AND chat_id = ?`, channel, chatID)
	return err
}

// DeleteChatLinksForSandbox revokes every link to a sandbox on destroy, so a
// reused id cannot inherit someone else's phone.
func (s *Store) DeleteChatLinksForSandbox(ctx context.Context, sandboxID string) error {
	_, err := s.exec(ctx, `DELETE FROM chat_links WHERE sandbox_id = ?`, sandboxID)
	return err
}
