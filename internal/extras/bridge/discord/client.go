// Package discord connects Discord to workstations, reusing the Telegram
// bridge's Runner, risk check and chat-link store. Messages arrive over the
// Gateway WebSocket and replies go out over REST.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Channel is the chat-link channel this bridge owns.
const Channel = "discord"

// DefaultAPIBase is Discord's REST root.
const DefaultAPIBase = "https://discord.com/api/v10"

// Client is a minimal Discord REST client.
type Client struct {
	Token   string // bot token; sent as "Bot <token>"
	APIBase string
	HTTP    *http.Client
}

func NewClient(token string) *Client {
	return &Client{
		Token:   token,
		APIBase: DefaultAPIBase,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) call(ctx context.Context, method, path string, payload, out any) error {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.APIBase+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+c.Token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("discord %s %s: %d %s", method, path, resp.StatusCode, truncateForErr(raw))
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func truncateForErr(b []byte) string {
	if len(b) > 200 {
		b = b[:200]
	}
	return string(b)
}

// GetMe verifies the token and returns the bot's user id and username.
// Called before the gateway connects so a bad token fails at startup.
func (c *Client) GetMe(ctx context.Context) (id, username string, err error) {
	var u struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	if err := c.call(ctx, http.MethodGet, "/users/@me", nil, &u); err != nil {
		return "", "", err
	}
	return u.ID, u.Username, nil
}

// SendMessage posts text into a channel and returns the new message id.
// Discord rejects content over 2000 chars, so callers truncate.
func (c *Client) SendMessage(ctx context.Context, channelID, text string) (string, error) {
	var m struct {
		ID string `json:"id"`
	}
	err := c.call(ctx, http.MethodPost, "/channels/"+channelID+"/messages",
		map[string]any{"content": text}, &m)
	return m.ID, err
}

// EditMessage rewrites a previously sent message.
func (c *Client) EditMessage(ctx context.Context, channelID, messageID, text string) error {
	return c.call(ctx, http.MethodPatch, "/channels/"+channelID+"/messages/"+messageID,
		map[string]any{"content": text}, nil)
}
