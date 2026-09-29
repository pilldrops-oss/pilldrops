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

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// APIBoxes drives sandboxes over the public v1 API, since a chat's workstation
// can be on any node. It authenticates with an operator key and names the
// customer's org per request via X-Org-Override.
type APIBoxes struct {
	BaseURL string // e.g. http://127.0.0.1:8080
	Key     string // operator key
	HTTP    *http.Client
	// Source attributes submitted turns ("telegram", "discord"); the API only honours allowlisted values.
	Source string
}

// NewAPIBoxes builds a client. The timeout must exceed the longest agent run because Exec is synchronous.
func NewAPIBoxes(baseURL, key string) *APIBoxes {
	return &APIBoxes{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Key:     key,
		HTTP:    &http.Client{Timeout: 16 * time.Minute},
	}
}

// org is threaded per call so one bridge serves every customer.
type orgKey struct{}

// WithOrg returns a context naming the org whose sandbox is being driven.
func WithOrg(ctx context.Context, org string) context.Context {
	return context.WithValue(ctx, orgKey{}, org)
}

func orgFrom(ctx context.Context) string {
	s, _ := ctx.Value(orgKey{}).(string)
	return s
}

func (a *APIBoxes) do(ctx context.Context, method, path string, payload, out any) error {
	var body []byte
	if payload != nil {
		var err error
		if body, err = json.Marshal(payload); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, a.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.Key)
	req.Header.Set("Content-Type", "application/json")
	if org := orgFrom(ctx); org != "" {
		req.Header.Set("X-Org-Override", org)
	}
	if a.Source != "" {
		req.Header.Set("X-Agent-Source", a.Source)
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return fmt.Errorf("%s", e.Error)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (a *APIBoxes) Get(id string) (SandboxState, bool) {
	// Get has no ctx on the Sandboxes interface; bound it so a hung control plane cannot wedge a chat.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var out struct {
		Template string `json:"template"`
		Status   string `json:"status"`
		State    string `json:"state"`
	}
	if err := a.do(ctx, http.MethodGet, "/v1/sandboxes/"+id, nil, &out); err != nil {
		return SandboxState{}, false
	}
	state := out.State
	if state == "" {
		state = out.Status
	}
	return SandboxState{Template: out.Template, Paused: state == "paused"}, true
}

// ListForOrg returns the org's sandbox ids.
func (a *APIBoxes) ListForOrg(ctx context.Context, orgID string) ([]string, error) {
	var out struct {
		Sandboxes []struct {
			ID string `json:"id"`
		} `json:"sandboxes"`
	}
	if err := a.do(WithOrg(ctx, orgID), http.MethodGet, "/v1/sandboxes", nil, &out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Sandboxes))
	for _, sb := range out.Sandboxes {
		ids = append(ids, sb.ID)
	}
	return ids, nil
}

// PauseFor pauses one of an org's sandboxes.
func (a *APIBoxes) PauseFor(ctx context.Context, orgID, id string) error {
	return a.do(WithOrg(ctx, orgID), http.MethodPost, "/v1/sandboxes/"+id+"/pause", map[string]any{}, nil)
}

// DestroyFor removes one of an org's sandboxes.
func (a *APIBoxes) DestroyFor(ctx context.Context, orgID, id string) error {
	return a.do(WithOrg(ctx, orgID), http.MethodDelete, "/v1/sandboxes/"+id, nil, nil)
}

func (a *APIBoxes) Resume(ctx context.Context, id string) error {
	return a.do(ctx, http.MethodPost, "/v1/sandboxes/"+id+"/resume", map[string]any{}, nil)
}

// WriteFile writes bytes into a sandbox: raw body, path in the query.
func (a *APIBoxes) WriteFile(ctx context.Context, sandboxID, path string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.BaseURL+"/v1/sandboxes/"+sandboxID+"/files?path="+url.QueryEscape(path), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.Key)
	req.Header.Set("Content-Type", "application/octet-stream")
	if org := orgFrom(ctx); org != "" {
		req.Header.Set("X-Org-Override", org)
	}
	if a.Source != "" {
		req.Header.Set("X-Agent-Source", a.Source)
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("write file: %s", resp.Status)
	}
	return nil
}

func (a *APIBoxes) Exec(ctx context.Context, id string, spec rt.ExecSpec) (rt.ExecResult, error) {
	var out struct {
		Stdout    string `json:"stdout"`
		Stderr    string `json:"stderr"`
		ExitCode  int    `json:"exit_code"`
		Truncated bool   `json:"truncated"`
	}
	err := a.do(ctx, http.MethodPost, "/v1/sandboxes/"+id+"/exec", map[string]any{
		"cmd":     spec.Cmd,
		"timeout": int(spec.Timeout / time.Second),
	}, &out)
	if err != nil {
		return rt.ExecResult{}, err
	}
	return rt.ExecResult{
		Stdout: out.Stdout, Stderr: out.Stderr,
		ExitCode: out.ExitCode, Truncated: out.Truncated,
	}, nil
}
