package store

import (
	"context"
	"time"
)

// Web-chat transcripts, stored here rather than in the machine so they survive
// a pause and resume.

// maxChatText bounds one stored message.
const maxChatText = 32 * 1024

// ChatHistoryLimit is how many messages a transcript returns by default.
const ChatHistoryLimit = 200

// ChatMessage is one turn.
type ChatMessage struct {
	Role string `json:"role"` // "user" | "agent"
	Text string `json:"text"`
	At   int64  `json:"at"`
}

// AppendChatMessage records one turn.
func (s *Store) AppendChatMessage(ctx context.Context, sandboxID, orgID, role, text string) error {
	if len(text) > maxChatText {
		text = text[:maxChatText]
	}
	_, err := s.exec(ctx,
		`INSERT INTO chat_messages (sandbox_id, org_id, role, text, at) VALUES (?, ?, ?, ?, ?)`,
		sandboxID, orgID, role, text, time.Now().Unix())
	return err
}

// ListChatMessages returns a sandbox's transcript oldest-first, capped to the
// most recent limit messages.
func (s *Store) ListChatMessages(ctx context.Context, sandboxID string, limit int) ([]ChatMessage, error) {
	if limit <= 0 {
		limit = ChatHistoryLimit
	}
	// Newest-first so LIMIT keeps the recent messages, then reversed for display.
	rows, err := s.query(ctx,
		`SELECT role, text, at FROM chat_messages WHERE sandbox_id = ? ORDER BY at DESC, id DESC LIMIT ?`,
		sandboxID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatMessage
	for rows.Next() {
		var m ChatMessage
		if err := rows.Scan(&m.Role, &m.Text, &m.At); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if out == nil {
		out = []ChatMessage{}
	}
	return out, nil
}

// HasChatMessages reports whether a conversation has started.
func (s *Store) HasChatMessages(ctx context.Context, sandboxID string) bool {
	var n int
	if err := s.queryRow(ctx, `SELECT COUNT(1) FROM chat_messages WHERE sandbox_id = ?`, sandboxID).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// DeleteChatMessagesForSandbox drops a destroyed machine's transcript.
func (s *Store) DeleteChatMessagesForSandbox(ctx context.Context, sandboxID string) error {
	_, err := s.exec(ctx, `DELETE FROM chat_messages WHERE sandbox_id = ?`, sandboxID)
	return err
}
