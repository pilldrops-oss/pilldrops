//go:build !core

package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/aat"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// The connect bundle answers "how do I drive this agent?" in one call. Every
// entry is either usable or says why not, as in channels.go.

// agentConnectTokenTTL is short because this token is minted casually while exploring.
const agentConnectTokenTTL = 24 * time.Hour

// handleAgentConnect returns everything needed to drive this agent. Org-key
// only, since a credential that can mint its own successors is not revocable.
//
//	GET /v1/sandboxes/{id}/agent/connect
func (s *Server) handleAgentConnect(w http.ResponseWriter, r *http.Request) {
	id, tmpl, ok := s.agentGate(r)
	if !ok {
		writeErrCode(w, http.StatusNotFound, codeAgentNotSupported, "sandbox not found")
		return
	}

	// Read and write only: console would put a shell behind a pasted link,
	// admin would let it raise its own spend cap.
	set, err := aat.NewSet([]aat.Scope{aat.ScopeRead, aat.ScopeWrite})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	tok, secret, err := s.st.CreateAgentToken(r.Context(), store.AgentTokenRequest{
		SandboxID: id, OrgID: orgID(r), Scopes: set,
		Label: "connect bundle", CreatedBy: keyIDOf(r), TTL: agentConnectTokenTTL,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.auditDetail(r, "agent.connect", id, map[string]any{
		"token_id": tok.ID, "scopes": set.Encode(),
	})

	base := s.publicBase(r)
	agentBase := base + "/v1/sandboxes/" + id + "/agent"
	origin := s.agentOriginURL(r, id)

	writeJSON(w, http.StatusOK, map[string]any{
		"sandbox_id": id,
		"agent":      map[string]any{"id": tmpl.ID, "name": tmpl.Name},
		"token": map[string]any{
			"id": tok.ID, "scopes": scopeList(set),
			"value":      secret,
			"shown_once": true,
			"expires_at": tok.ExpiresAt.UTC().Format(time.RFC3339),
			"note": "Shown once and never again — a later call to this endpoint mints a " +
				"DIFFERENT token and returns value: null for this one. Store it now.",
		},
		"endpoints": connectEndpoints(agentBase, origin),
		"snippets":  connectSnippets(agentBase, secret),
		"mcp":       connectMCP(agentBase, secret),
		"channels":  s.connectChannels(r, id),
		"origin": map[string]any{
			"url": origin,
			"note": "A separate origin per sandbox, so a link shared with someone cannot " +
				"reach the dashboard. Share links land here.",
		},
	})
}

// scopeList renders a scope set for the response.
func scopeList(set aat.Set) []string {
	out := make([]string, 0, len(set))
	for _, sc := range set.Sorted() {
		out = append(out, string(sc))
	}
	return out
}

// connectEndpoints lists what a caller can hit.
func connectEndpoints(agentBase, origin string) map[string]any {
	return map[string]any{
		"status":    agentBase + "/status",
		"submit":    agentBase + "/turns",
		"turns":     agentBase + "/turns",
		"events":    agentBase + "/events?since_seq=0",
		"approvals": agentBase + "/approvals",
		"policy":    agentBase + "/policy",
		"budget":    agentBase + "/budget",
		"threads":   agentBase + "/threads",
		// The OpenAI-shaped adapter.
		"responses_compat": agentBase + "/responses",
		"viewer":           origin + "/",
		"notes": map[string]string{
			"events": "SSE. Send Last-Event-ID (or since_seq) to resume without gaps; a " +
				"paused workstation replays from the control plane's copy instead.",
			"submit": "202 with a turn id — the turn is accepted, not finished. Watch events.",
		},
	}
}

// connectSnippets inlines the token, which is safe because it is short-lived and narrowly scoped.
func connectSnippets(agentBase, secret string) map[string]string {
	return map[string]string{
		"curl_submit": fmt.Sprintf(
			"curl -X POST %s/turns \\\n  -H 'Authorization: Bearer %s' \\\n"+
				"  -H 'Content-Type: application/json' \\\n  -d '{\"message\":\"run the tests\"}'",
			agentBase, secret),
		"curl_watch": fmt.Sprintf(
			"curl -N %s/events?since_seq=0 \\\n  -H 'Authorization: Bearer %s'",
			agentBase, secret),
		"node": fmt.Sprintf(`const r = await fetch("%s/turns", {
  method: "POST",
  headers: { Authorization: "Bearer %s", "Content-Type": "application/json" },
  body: JSON.stringify({ message: "run the tests" }),
});
console.log(await r.json()); // { turn_id, state, seq }`, agentBase, secret),
		"python": fmt.Sprintf(`import requests
r = requests.post(
    "%s/turns",
    headers={"Authorization": "Bearer %s"},
    json={"message": "run the tests"},
)
print(r.json())  # {'turn_id': ..., 'state': ..., 'seq': ...}`, agentBase, secret),
	}
}

// connectMCP is a ready-to-paste MCP client config.
func connectMCP(agentBase, secret string) map[string]any {
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"pilldrops-agent": map[string]any{
				"url":     agentBase + "/responses",
				"headers": map[string]string{"Authorization": "Bearer " + secret},
			},
		},
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return map[string]any{
		"config":    string(b),
		"transport": "http",
		"supported": false,
		"reason": "not built yet — this config drives the OpenAI-Responses-compatible " +
			"endpoint, which suits an HTTP MCP client. A native MCP tool surface for the " +
			"agent is not implemented.",
	}
}

// connectChannels reports chat reachability; an unbuilt channel is listed with a reason.
func (s *Server) connectChannels(r *http.Request, id string) []map[string]any {
	out := []map[string]any{}
	links, err := s.st.ChatLinksForSandbox(r.Context(), id)
	if err != nil {
		// Unknown, not empty: an empty list would claim the workstation is reachable nowhere.
		return []map[string]any{{
			"id": "unknown", "available": false,
			"reason": "the channel links could not be read: " + err.Error(),
		}}
	}
	linked := map[string]bool{}
	for _, l := range links {
		linked[l.Channel] = true
	}
	for _, c := range []struct {
		id, name, reason string
		available        bool
	}{
		{"telegram", "Telegram", "", envStr("TELEGRAM_BOT_TOKEN", "") != "" || envStr("TELEGRAM_BOT_USERNAME", "") != ""},
		{"discord", "Discord", "", envStr("DISCORD_BOT_TOKEN", "") != "" || envStr("DISCORD_BOT_NAME", "") != ""},
		{"slack", "Slack", "not built yet — needs a Slack Socket Mode driver", false},
		{"whatsapp", "WhatsApp", "not built yet — needs WhatsApp Business API access", false},
	} {
		e := map[string]any{
			"id": c.id, "name": c.name, "available": c.available, "linked": linked[c.id],
		}
		switch {
		case c.reason != "":
			e["reason"] = c.reason
		case !c.available:
			e["reason"] = "this deployment has no " + c.name + " bot configured"
		}
		out = append(out, e)
	}
	return out
}

// agentOriginURL builds the per-sandbox agent origin, or the API host without a sandboxDomain.
func (s *Server) agentOriginURL(r *http.Request, id string) string {
	if s.sandboxDomain == "" {
		return s.publicBase(r)
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://agent-" + id + "." + s.sandboxDomain
}

// publicBase is the base URL a caller reached us on.
func (s *Server) publicBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		host = fwd
	}
	return scheme + "://" + host
}

// handleAgentShareQR returns the share link's QR payload, never an API token,
// since a QR ends up photographed and pasted into tickets.
func (s *Server) handleAgentShareQR(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.agentGate(r); !ok {
		writeErrCode(w, http.StatusNotFound, codeAgentNotSupported, "sandbox not found")
		return
	}
	target := strings.TrimSpace(r.URL.Query().Get("url"))
	if target == "" {
		writeErr(w, http.StatusBadRequest,
			"url is required: pass the claim_url from POST /agent/share")
		return
	}
	// Refuse anything that looks like a credential.
	if strings.Contains(target, aat.Prefix) || strings.Contains(target, "sf_live_") {
		writeErr(w, http.StatusBadRequest,
			"refusing to encode a credential in a QR code: a QR is a screenshot-shaped "+
				"secret. Encode the share link (claim_url), whose one-time code is spent on use.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"payload": target,
		"note": "Render this as a QR client-side. It carries a single-use share link, " +
			"never an API token.",
	})
}
