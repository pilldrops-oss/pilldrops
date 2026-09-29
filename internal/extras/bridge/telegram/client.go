// Package telegram connects a Telegram chat to a workstation: a message from a
// phone wakes the machine, runs the task on the agent installed there, and
// replies with the result. It is a thin Bot API client with no third-party dependency.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// APIBase is Telegram's Bot API root.
const APIBase = "https://api.telegram.org"

// maxMessageLen is Telegram's cap on a message body; longer sends fail outright.
const maxMessageLen = 4096

// Client talks to the Telegram Bot API.
type Client struct {
	Token   string
	BaseURL string
	HTTP    *http.Client
}

// NewClient builds a client. The HTTP timeout must exceed the long-poll window.
func NewClient(token string) *Client {
	return &Client{
		Token:   token,
		BaseURL: APIBase,
		HTTP:    &http.Client{Timeout: 90 * time.Second},
	}
}

// Update is one Telegram update, modelling only the fields the bridge uses.
type Update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
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
	} `json:"message"`
	// CallbackQuery arrives when an inline button is tapped.
	CallbackQuery *struct {
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
	} `json:"callback_query"`
}

// ChatID returns the originating chat, or 0 when the update carries neither a
// message nor a button tap.
func (u Update) ChatID() int64 {
	if u.Message != nil {
		return u.Message.Chat.ID
	}
	if u.CallbackQuery != nil && u.CallbackQuery.Message != nil {
		return u.CallbackQuery.Message.Chat.ID
	}
	return 0
}

type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
}

func (c *Client) call(ctx context.Context, method string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/bot%s/%s", strings.TrimRight(c.BaseURL, "/"), c.Token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var ar apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return fmt.Errorf("%s: decode: %w", method, err)
	}
	if !ar.OK {
		// Never include the request body in the error; it carries customer text and errors are logged.
		return fmt.Errorf("%s: telegram: %s", method, ar.Description)
	}
	if out != nil && len(ar.Result) > 0 {
		return json.Unmarshal(ar.Result, out)
	}
	return nil
}

// GetUpdates long-polls for updates after offset; timeout is the server-side wait in seconds.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout int) ([]Update, error) {
	var updates []Update
	err := c.call(ctx, "getUpdates", map[string]any{
		"offset":  offset,
		"timeout": timeout,
		// The bridge has no use for edits, reactions or channel posts.
		"allowed_updates": []string{"message", "callback_query"},
	}, &updates)
	return updates, err
}

// SendMessage posts a message and returns its id so a later edit can update it in place.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string) (int64, error) {
	var sent struct {
		MessageID int64 `json:"message_id"`
	}
	err := c.call(ctx, "sendMessage", map[string]any{
		"chat_id": chatID,
		"text":    Truncate(text, maxMessageLen),
		// No parse_mode: agent output is full of markdown characters and
		// Telegram rejects the whole message on a parse error.
		"disable_web_page_preview": true,
	}, &sent)
	return sent.MessageID, err
}

// EditMessage replaces the text of a message already sent.
func (c *Client) EditMessage(ctx context.Context, chatID, messageID int64, text string) error {
	return c.call(ctx, "editMessageText", map[string]any{
		"chat_id":                  chatID,
		"message_id":               messageID,
		"text":                     Truncate(text, maxMessageLen),
		"disable_web_page_preview": true,
	}, nil)
}

// Button is one inline keyboard button.
type Button struct {
	Text string
	Data string
}

// SendButtons posts a message with one row of inline buttons and returns its id.
func (c *Client) SendButtons(ctx context.Context, chatID int64, text string, buttons []Button) (int64, error) {
	row := make([]map[string]string, 0, len(buttons))
	for _, b := range buttons {
		row = append(row, map[string]string{"text": b.Text, "callback_data": b.Data})
	}
	var sent struct {
		MessageID int64 `json:"message_id"`
	}
	err := c.call(ctx, "sendMessage", map[string]any{
		"chat_id":                  chatID,
		"text":                     Truncate(text, maxMessageLen),
		"disable_web_page_preview": true,
		"reply_markup":             map[string]any{"inline_keyboard": [][]map[string]string{row}},
	}, &sent)
	return sent.MessageID, err
}

// AnswerCallback acknowledges a button tap; Telegram shows a spinner until it arrives.
func (c *Client) AnswerCallback(ctx context.Context, id, text string) error {
	return c.call(ctx, "answerCallbackQuery", map[string]any{
		"callback_query_id": id,
		"text":              text,
	}, nil)
}

// GetMe verifies the token and returns the bot's username.
func (c *Client) GetMe(ctx context.Context) (string, error) {
	var me struct {
		Username string `json:"username"`
	}
	if err := c.call(ctx, "getMe", map[string]any{}, &me); err != nil {
		return "", err
	}
	return me.Username, nil
}

// FileURL returns the download URL for a file id.
func (c *Client) FileURL(ctx context.Context, fileID string) (string, error) {
	var f struct {
		FilePath string `json:"file_path"`
	}
	if err := c.call(ctx, "getFile", map[string]any{"file_id": fileID}, &f); err != nil {
		return "", err
	}
	if f.FilePath == "" {
		return "", fmt.Errorf("getFile: no path")
	}
	return fmt.Sprintf("%s/file/bot%s/%s",
		strings.TrimRight(c.BaseURL, "/"), c.Token, url.PathEscape(f.FilePath)), nil
}

// Truncate shortens s to at most n runes, keeping the tail: agent output ends
// with the summary and errors, and the head is setup noise.
func Truncate(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	const marker = "…(truncated, full output on the workstation)\n"
	r := []rune(s)
	keep := n - len([]rune(marker))
	if keep < 0 {
		return string(r[:n])
	}
	return marker + string(r[len(r)-keep:])
}
