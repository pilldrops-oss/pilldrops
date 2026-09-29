//go:build !core

package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/extras/agent/webhook"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// Webhook routes are org-key only: a delegated token must not be able to
// point the server at an arbitrary address, or read the agent past its own lifetime.

// webhookTestLimiter allows 5 test sends per minute per hook, since each one
// makes the server call a third party.
var webhookTestLimiter = newRateLimiter(5.0/60.0, 5)

// handleAgentWebhooks lists or creates hooks.
//
//	GET  /v1/sandboxes/{id}/agent/webhooks
//	POST /v1/sandboxes/{id}/agent/webhooks
func (s *Server) handleAgentWebhooks(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.agentGate(r)
	if !ok {
		writeErrCode(w, http.StatusNotFound, codeAgentNotSupported, "sandbox not found")
		return
	}

	if r.Method == http.MethodGet {
		hooks, err := s.st.ListAgentWebhooks(r.Context(), orgID(r), id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if hooks == nil {
			hooks = []store.AgentWebhook{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"webhooks":           hooks,
			"deliverable_events": deliverableEvents(),
			"signature": map[string]any{
				"header":           webhook.SignatureHeader,
				"timestamp_header": webhook.TimestampHeader,
				"delivery_header":  webhook.DeliveryIDHeader,
				"max_skew_seconds": int(webhook.MaxSkew.Seconds()),
				"signed_material":  "timestamp + \".\" + body, HMAC-SHA256, hex, prefixed sha256=",
				"note": "Reject a timestamp outside the skew window and dedupe on the " +
					"delivery id: retries reuse it, so that is what makes at-least-once " +
					"delivery safe to act on.",
			},
		})
		return
	}

	var req struct {
		URL    string   `json:"url"`
		Events []string `json:"events"`
	}
	if !decodeBody(r, &req) {
		writeErr(w, http.StatusBadRequest, "invalid json body")
		return
	}

	// SSRF check at create time too, so the refusal is visible to the caller.
	if _, _, err := webhook.ResolveAndPin(req.URL); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.Events) == 0 {
		writeErr(w, http.StatusBadRequest,
			"events is required: name at least one event type, or the hook would never fire")
		return
	}
	for _, e := range req.Events {
		if !webhook.EventEligible(e) {
			writeErr(w, http.StatusBadRequest, webhook.IneligibleReason(e))
			return
		}
	}

	secret := store.NewWebhookSecret()
	hook, err := s.st.CreateAgentWebhook(r.Context(), store.AgentWebhook{
		SandboxID: id, OrgID: orgID(r), URL: req.URL, Events: req.Events,
		CreatedBy: keyIDOf(r),
	}, secret)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.auditDetail(r, "agent.webhook.create", id, map[string]any{
		"webhook_id": hook.ID, "url": hook.URL, "events": req.Events,
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"webhook":    hook,
		"secret":     secret,
		"shown_once": true,
		"note": "Store this now: it signs every delivery and is not returned again. " +
			"Verify with HMAC-SHA256 over timestamp + \".\" + body.",
	})
}

// handleAgentWebhookDelete removes one hook.
//
//	DELETE /v1/sandboxes/{id}/agent/webhooks/{hookID}
func (s *Server) handleAgentWebhookDelete(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.agentGate(r)
	if !ok {
		writeErrCode(w, http.StatusNotFound, codeAgentNotSupported, "sandbox not found")
		return
	}
	hookID := r.PathValue("hookID")
	if err := s.st.DeleteAgentWebhook(r.Context(), orgID(r), id, hookID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "no such webhook for this sandbox")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.auditDetail(r, "agent.webhook.delete", id, map[string]any{"webhook_id": hookID})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": hookID})
}

// handleAgentWebhookTest sends one signed probe.
//
//	POST /v1/sandboxes/{id}/agent/webhooks/{hookID}/test
func (s *Server) handleAgentWebhookTest(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.agentGate(r)
	if !ok {
		writeErrCode(w, http.StatusNotFound, codeAgentNotSupported, "sandbox not found")
		return
	}
	hookID := r.PathValue("hookID")
	if !webhookTestLimiter.allow("whtest:" + hookID) {
		w.Header().Set("Retry-After", "60")
		writeErr(w, http.StatusTooManyRequests,
			"test sends are limited; this endpoint makes the server call your endpoint on demand")
		return
	}

	hooks, err := s.st.ListAgentWebhooks(r.Context(), orgID(r), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var hook *store.AgentWebhook
	for i := range hooks {
		if hooks[i].ID == hookID {
			hook = &hooks[i]
		}
	}
	if hook == nil {
		writeErr(w, http.StatusNotFound, "no such webhook for this sandbox")
		return
	}

	secret, err := s.st.AgentWebhookSecret(r.Context(), hookID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not read the signing secret")
		return
	}

	status, sendErr := s.sendWebhookProbe(r.Context(), hook.URL, secret)
	out := map[string]any{"webhook_id": hookID, "status": status}
	if sendErr != nil {
		// A failing customer endpoint is a 200 with ok=false, not a 5xx.
		out["ok"] = false
		out["error"] = sendErr.Error()
	} else {
		out["ok"] = status >= 200 && status < 300
		if !out["ok"].(bool) {
			out["error"] = "your endpoint answered " + http.StatusText(status)
			out["retryable"] = webhook.Retryable(status)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// sendWebhookProbe delivers a signed test payload.
func (s *Server) sendWebhookProbe(ctx context.Context, rawURL, secret string) (int, error) {
	u, ips, err := webhook.ResolveAndPin(rawURL)
	if err != nil {
		return 0, err
	}
	body, _ := json.Marshal(map[string]any{
		"event": "webhook.test",
		"note":  "A test delivery. Verify the signature and reject stale timestamps.",
		"at":    time.Now().UTC().Format(time.RFC3339),
	})
	ts := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(webhook.SignatureHeader, webhook.Sign(secret, ts, body))
	req.Header.Set(webhook.TimestampHeader, strconv.FormatInt(ts.Unix(), 10))
	// Unique per test so a deduping receiver does not drop repeats.
	req.Header.Set(webhook.DeliveryIDHeader, "whd_test_"+strconv.FormatInt(ts.UnixNano(), 10))

	resp, err := webhook.PinnedClient(ips, webhook.DeliveryTimeout).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// handleAgentWebhookFailures is the dead-letter view.
//
//	GET /v1/sandboxes/{id}/agent/webhooks/failures
func (s *Server) handleAgentWebhookFailures(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.agentGate(r)
	if !ok {
		writeErrCode(w, http.StatusNotFound, codeAgentNotSupported, "sandbox not found")
		return
	}
	rows, err := s.st.ListFailedDeliveries(r.Context(), orgID(r), id, 50)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rows == nil {
		rows = []store.AgentWebhookDelivery{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"failed": rows,
		"note": "Deliveries that never got through after " +
			strconv.Itoa(webhook.MaxAttempts) + " attempts. A 4xx other than 408/429 stops " +
			"immediately, because the receiver understood the request and refused it.",
	})
}

func deliverableEvents() []string {
	all := []string{
		"turn.queued", "turn.started", "turn.completed", "turn.failed", "turn.interrupted",
		"approval.requested", "approval.granted", "approval.denied", "approval.timeout",
		"spend.threshold", "session.degraded", "session.ready", "session.stopped",
	}
	// Filter through the predicate so the list cannot advertise an event the sender refuses.
	out := make([]string, 0, len(all))
	for _, e := range all {
		if webhook.EventEligible(e) {
			out = append(out, e)
		}
	}
	return out
}
