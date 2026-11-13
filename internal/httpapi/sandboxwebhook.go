package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/store"
	"github.com/pilldrops-oss/pilldrops/internal/webhook"
)

// Org-level webhooks report sandbox lifecycle events for every sandbox in the org.

// webhookTestLimiter allows 5 test sends per minute per hook, since each one
// makes the server call a third party.
var webhookTestLimiter = newRateLimiter(5.0/60.0, 5)

// maxOrgWebhooks bounds how many requests one lifecycle event can fan out to.
const maxOrgWebhooks = 20

func (s *Server) registerWebhookRoutes(route func(string, http.Handler)) {
	route("GET /v1/webhooks", s.auth(s.handleWebhooks))
	route("POST /v1/webhooks", s.auth(s.handleWebhooks))
	route("GET /v1/webhooks/deliveries", s.auth(s.handleWebhookDeliveries))
	route("DELETE /v1/webhooks/{hookID}", s.auth(s.handleWebhookDelete))
	route("POST /v1/webhooks/{hookID}/test", s.auth(s.handleWebhookTest))
	route("POST /v1/webhooks/{hookID}/toggle", s.auth(s.handleWebhookToggle))
}

// handleWebhooks lists or creates org-level hooks.
//
//	GET  /v1/webhooks
//	POST /v1/webhooks  {"url":"https://...","events":["sandbox.ended"]}
func (s *Server) handleWebhooks(w http.ResponseWriter, r *http.Request) {
	hooks, err := s.st.ListAgentWebhooks(r.Context(), orgID(r), "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if r.Method == http.MethodGet {
		if hooks == nil {
			hooks = []store.AgentWebhook{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"webhooks":           hooks,
			"deliverable_events": store.SandboxWebhookEvents,
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
	if len(hooks) >= maxOrgWebhooks {
		writeErr(w, http.StatusConflict, "webhook limit reached: delete one before adding another")
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
		if !slices.Contains(store.SandboxWebhookEvents, e) {
			writeErr(w, http.StatusBadRequest, "unknown event "+e+": GET /v1/webhooks lists deliverable_events")
			return
		}
	}

	secret := store.NewWebhookSecret()
	hook, err := s.st.CreateAgentWebhook(r.Context(), store.AgentWebhook{
		OrgID: orgID(r), URL: req.URL, Events: req.Events, CreatedBy: keyIDOf(r),
	}, secret)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.auditDetail(r, "webhook.create", hook.ID, map[string]any{"url": hook.URL, "events": req.Events})
	writeJSON(w, http.StatusCreated, map[string]any{
		"webhook":    hook,
		"secret":     secret,
		"shown_once": true,
		"note": "Store this now: it signs every delivery and is not returned again. " +
			"Verify with HMAC-SHA256 over timestamp + \".\" + body.",
	})
}

// handleWebhookDelete removes one org-level hook.
//
//	DELETE /v1/webhooks/{hookID}
func (s *Server) handleWebhookDelete(w http.ResponseWriter, r *http.Request) {
	hookID := r.PathValue("hookID")
	if err := s.st.DeleteAgentWebhook(r.Context(), orgID(r), "", hookID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "no such webhook")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.auditDetail(r, "webhook.delete", "", map[string]any{"webhook_id": hookID})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": hookID})
}

// handleWebhookTest sends one signed probe to an org-level hook.
//
//	POST /v1/webhooks/{hookID}/test
func (s *Server) handleWebhookTest(w http.ResponseWriter, r *http.Request) {
	hookID := r.PathValue("hookID")
	if !webhookTestLimiter.allow("whtest:" + hookID) {
		w.Header().Set("Retry-After", "60")
		writeErr(w, http.StatusTooManyRequests,
			"test sends are limited; this endpoint makes the server call your endpoint on demand")
		return
	}

	hooks, err := s.st.ListAgentWebhooks(r.Context(), orgID(r), "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	i := slices.IndexFunc(hooks, func(h store.AgentWebhook) bool { return h.ID == hookID })
	if i < 0 {
		writeErr(w, http.StatusNotFound, "no such webhook")
		return
	}
	secret, err := s.st.AgentWebhookSecret(r.Context(), hookID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not read the signing secret")
		return
	}

	now := time.Now()
	body, _ := json.Marshal(map[string]any{
		"type": "webhook.test",
		"note": "A test delivery. Verify the signature and reject stale timestamps.",
		"at":   now.UTC().Format(time.RFC3339),
	})
	// Unique per test so a deduping receiver does not drop repeats.
	status, sendErr := webhookSend(r.Context(), hooks[i].URL, secret, "whd_test_"+strconv.FormatInt(now.UnixNano(), 10), body)
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

// handleWebhookToggle pauses or resumes an org-level hook.
//
//	POST /v1/webhooks/{hookID}/toggle  {"enabled":false}
func (s *Server) handleWebhookToggle(w http.ResponseWriter, r *http.Request) {
	hookID := r.PathValue("hookID")
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeBody(r, &body) {
		writeErr(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if err := s.st.SetAgentWebhookEnabled(r.Context(), orgID(r), "", hookID, body.Enabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "no such webhook")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.auditDetail(r, "webhook.toggle", hookID, map[string]any{"enabled": body.Enabled})
	writeJSON(w, http.StatusOK, map[string]any{"id": hookID, "enabled": body.Enabled})
}

// handleWebhookDeliveries lists the org's recent sandbox-event deliveries.
//
//	GET /v1/webhooks/deliveries
func (s *Server) handleWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.ListOrgWebhookDeliveries(r.Context(), orgID(r), 50)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rows == nil {
		rows = []store.AgentWebhookDelivery{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": rows})
}

// attemptSandboxWebhook makes one attempt at delivering a lifecycle event.
func (s *Server) attemptSandboxWebhook(ctx context.Context, d store.AgentWebhookDelivery) (int, error) {
	hooks, err := s.st.ListAgentWebhooks(ctx, d.OrgID, "")
	if err != nil {
		return 0, err
	}
	i := slices.IndexFunc(hooks, func(h store.AgentWebhook) bool { return h.ID == d.WebhookID })
	if i < 0 || !hooks[i].Enabled {
		return 0, errors.Join(errWebhookUndeliverable, errors.New("the webhook was removed or disabled"))
	}
	body, err := s.sandboxWebhookBody(ctx, d)
	if err != nil {
		return 0, err
	}
	secret, err := s.st.AgentWebhookSecret(ctx, d.WebhookID)
	if err != nil {
		return 0, err
	}
	return webhookSend(ctx, hooks[i].URL, secret, d.ID, body)
}

type sandboxWebhookBody struct {
	// ID is the delivery id, the same on every retry.
	ID      string    `json:"id"`
	Type    string    `json:"type"`
	At      time.Time `json:"at"`
	Sandbox struct {
		ID       string `json:"id"`
		Template string `json:"template,omitempty"`
	} `json:"sandbox"`
	Data map[string]any `json:"data,omitempty"`
}

func (s *Server) sandboxWebhookBody(ctx context.Context, d store.AgentWebhookDelivery) ([]byte, error) {
	at := int64(d.EventSeq)
	body := sandboxWebhookBody{ID: d.ID, Type: d.EventType, At: time.Unix(at, 0).UTC()}
	body.Sandbox.ID = d.SandboxID
	if d.EventType == "sandbox.ended" {
		h, found, err := s.st.GetHistory(ctx, d.SandboxID)
		if err != nil {
			return nil, err
		}
		if found {
			reason := h.ExitState
			if reason == "oom" {
				reason = "out_of_memory"
			}
			body.Sandbox.Template = h.Template
			body.Data = map[string]any{
				"reason":      reason,
				"started_at":  time.Unix(h.CreatedAt, 0).UTC(),
				"ran_seconds": h.DurationSecs,
				"vcpu":        h.VCPU,
				"memory_mb":   h.MemMiB,
			}
		}
	} else {
		template, err := s.st.SandboxEventTemplate(ctx, d.OrgID, d.SandboxID, at)
		if err != nil {
			return nil, err
		}
		body.Sandbox.Template = template
	}
	return json.Marshal(body)
}
