package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
)

// ErrAgentUnreachable means agentd did not answer at all.
var ErrAgentUnreachable = errors.New("the agent supervisor is not answering")

// Client is the node's side of the agentd API. It lives here so the wire types
// have exactly one definition.
type Client struct {
	// Addr is host:port.
	Addr string
	// Token authenticates to agentd; empty when the platform runs without per-sandbox tokens.
	Token string
	// HTTP is the transport; nil uses a client with no overall timeout, since the
	// event stream is long-lived and deadlines come from the caller's context.
	HTTP *http.Client
}

// dialTimeout keeps a sandbox whose agentd is down from holding a node request open.
const dialTimeout = 3 * time.Second

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{
		Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: dialTimeout}).DialContext,
			MaxIdleConnsPerHost: 4,
			// No response-header timeout: an idle event stream can be silent for minutes.
		},
	}
}

func (c *Client) url(path string) string { return "http://" + c.Addr + path }

// do performs a request and decodes the JSON response into out. An error agentd
// returned keeps its own message, which is written for a person to read.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(path), rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set(sbtoken.MetadataKey, c.Token)
	}

	resp, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrAgentUnreachable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return &StatusError{Code: resp.StatusCode, Message: e.Error}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// StatusError is an error agentd returned, with its status code so the node can
// pass the same code outward.
type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string { return e.Message }

// Status fetches the session snapshot.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var st Status
	err := c.do(ctx, http.MethodGet, "/status", nil, &st)
	return st, err
}

// Health reports whether agentd is up. Unauthenticated.
func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/health", nil, nil)
}

// SubmitRequest is a new turn.
type SubmitRequest struct {
	Message  string `json:"message"`
	Mode     string `json:"mode,omitempty"`
	Thread   string `json:"thread,omitempty"`
	Source   string `json:"source,omitempty"`
	Priority bool   `json:"priority,omitempty"`
}

// Submit enqueues a turn and returns it. Does not wait for it to run.
func (c *Client) Submit(ctx context.Context, req SubmitRequest) (*Turn, error) {
	var turn Turn
	if err := c.do(ctx, http.MethodPost, "/turns", req, &turn); err != nil {
		return nil, err
	}
	return &turn, nil
}

// Turns lists every turn, newest first.
func (c *Client) Turns(ctx context.Context) ([]*Turn, error) {
	var out struct {
		Turns []*Turn `json:"turns"`
	}
	if err := c.do(ctx, http.MethodGet, "/turns", nil, &out); err != nil {
		return nil, err
	}
	return out.Turns, nil
}

// Turn fetches one turn.
func (c *Client) Turn(ctx context.Context, id string) (*Turn, error) {
	var turn Turn
	if err := c.do(ctx, http.MethodGet, "/turns/"+id, nil, &turn); err != nil {
		return nil, err
	}
	return &turn, nil
}

// Interrupt stops a running turn.
func (c *Client) Interrupt(ctx context.Context, id string, grace time.Duration) error {
	body := map[string]any{"grace_seconds": grace.Seconds()}
	return c.do(ctx, http.MethodPost, "/turns/"+id+"/interrupt", body, nil)
}

// RemoveTurn dequeues a turn that has not started.
func (c *Client) RemoveTurn(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/turns/"+id, nil, nil)
}

// Reorder sets the pending queue's service order.
func (c *Client) Reorder(ctx context.Context, ids []string) error {
	return c.do(ctx, http.MethodPost, "/queue/reorder", map[string]any{"order": ids}, nil)
}

// ModelsResponse is what GET /models reports. Supported is false when the agent
// cannot enumerate its models (most cannot); Current is often known regardless.
type ModelsResponse struct {
	Supported bool     `json:"supported"`
	Models    []string `json:"models"`
	Current   string   `json:"current,omitempty"`
	Reason    string   `json:"reason,omitempty"`
}

// Models lists the agent's models.
func (c *Client) Models(ctx context.Context) (*ModelsResponse, error) {
	var out ModelsResponse
	if err := c.do(ctx, http.MethodGet, "/models", nil, &out); err != nil {
		return nil, err
	}
	if out.Models == nil {
		out.Models = []string{}
	}
	return &out, nil
}

// Threads lists the workstation's conversations.
func (c *Client) Threads(ctx context.Context) ([]Thread, error) {
	var out struct {
		Threads []Thread `json:"threads"`
	}
	if err := c.do(ctx, http.MethodGet, "/threads", nil, &out); err != nil {
		return nil, err
	}
	return out.Threads, nil
}

// Thread returns one conversation's turns.
func (c *Client) Thread(ctx context.Context, id string) ([]*Turn, error) {
	if id == "" {
		id = "unknown"
	}
	var out struct {
		Turns []*Turn `json:"turns"`
	}
	if err := c.do(ctx, http.MethodGet, "/threads/"+id, nil, &out); err != nil {
		return nil, err
	}
	return out.Turns, nil
}

// BudgetResponse is what GET/PATCH /budget reports.
type BudgetResponse struct {
	Enabled bool            `json:"enabled"`
	Budget  *BudgetSnapshot `json:"budget,omitempty"`
	Reason  string          `json:"reason,omitempty"`
}

// Budget reports the workstation's spend cap.
func (c *Client) Budget(ctx context.Context) (*BudgetResponse, error) {
	var out BudgetResponse
	if err := c.do(ctx, http.MethodGet, "/budget", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetBudget sets the cap in USD. Zero clears it.
func (c *Client) SetBudget(ctx context.Context, capUSD float64) (*BudgetResponse, error) {
	var out BudgetResponse
	body := map[string]any{"cap_usd": capUSD}
	if err := c.do(ctx, http.MethodPatch, "/budget", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SnapshotSafe asks whether the sandbox can be paused right now. A transport
// failure answers safe, since an unreachable agentd runs no turn, and the reason
// records that the check could not be made.
func (c *Client) SnapshotSafe(ctx context.Context) (bool, string) {
	var out struct {
		Safe   bool   `json:"safe"`
		Reason string `json:"reason"`
	}
	if err := c.do(ctx, http.MethodGet, "/snapshot-safe", nil, &out); err != nil {
		return true, "could not reach the agent supervisor to check: " + err.Error()
	}
	return out.Safe, out.Reason
}

// Events opens the event stream at sinceSeq and returns the raw body so the node
// can copy SSE frames straight through. The caller closes it.
func (c *Client) Events(ctx context.Context, sinceSeq uint64) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.url("/events?since_seq="+strconv.FormatUint(sinceSeq, 10)), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if c.Token != "" {
		req.Header.Set(sbtoken.MetadataKey, c.Token)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrAgentUnreachable, err)
	}
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		_ = json.NewDecoder(resp.Body).Decode(&e)
		resp.Body.Close()
		if e.Error == "" {
			e.Error = resp.Status
		}
		return nil, &StatusError{Code: resp.StatusCode, Message: e.Error}
	}
	return resp.Body, nil
}

// Approvals lists what is parked, plus the scopes a decision may carry.
func (c *Client) Approvals(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	if err := c.do(ctx, "GET", "/approvals", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Decide answers one parked approval. decision is the wire word "allow" or
// "deny"; "ask" is a policy state a client cannot send.
func (c *Client) Decide(ctx context.Context, id, decision, remember, actor string) (map[string]any, error) {
	var out map[string]any
	body := map[string]string{"decision": decision, "remember": remember, "actor": actor}
	if err := c.do(ctx, "POST", "/approvals/"+id, body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Policy returns the active allowlist and any standing grants.
func (c *Client) Policy(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	if err := c.do(ctx, "GET", "/policy", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ForgetApproval revokes a standing grant by its signature.
func (c *Client) ForgetApproval(ctx context.Context, signature string) (map[string]any, error) {
	var out map[string]any
	if err := c.do(ctx, "DELETE", "/policy/remembered",
		map[string]string{"signature": signature}, &out); err != nil {
		return nil, err
	}
	return out, nil
}
