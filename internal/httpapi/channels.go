package httpapi

import (
	"net/http"
	"os"
	"strings"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// channelInfo is one way a workstation can be reached from outside the dashboard.
// Unimplemented channels are listed too, marked unavailable.
type channelInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Available is whether this deployment can actually drive the channel.
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	// Linked reports whether this workstation is already reachable on it.
	Linked   bool   `json:"linked"`
	ChatID   string `json:"chat_id,omitempty"`
	ReadOnly bool   `json:"read_only,omitempty"`
	// Setup is one line telling the user what to do next.
	Setup string `json:"setup,omitempty"`
}

// handleChannels reports how a workstation can be reached.
//
//	GET /v1/sandboxes/{id}/channels
func (s *Server) handleChannels(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	links, err := s.st.ChatLinksForSandbox(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not read channel links")
		return
	}
	byChannel := map[string]store.ChatLink{}
	for _, l := range links {
		byChannel[l.Channel] = l
	}

	tgBot := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_USERNAME"))
	tgOK := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")) != "" || tgBot != ""
	// A bot name alone is enough when the bridge runs on the orchestrator and the node only renders the tab.
	dcOK := strings.TrimSpace(os.Getenv("DISCORD_BOT_TOKEN")) != "" ||
		strings.TrimSpace(os.Getenv("DISCORD_BOT_NAME")) != ""

	out := []channelInfo{
		{
			ID: "telegram", Name: "Telegram", Available: tgOK,
			Reason: reasonIf(!tgOK, "this deployment has no Telegram bot configured (TELEGRAM_BOT_TOKEN)"),
			Setup:  "Connect phone mints a one-time link. Tap it and the chat drives this workstation.",
		},
		{
			ID: "discord", Name: "Discord", Available: dcOK,
			Reason: reasonIf(!dcOK, "this deployment has no Discord bot configured (DISCORD_BOT_TOKEN)"),
			// Discord has no deep link, so the user DMs the bot the code themselves.
			Setup: "Connect mints a one-time code. DM the bot `!link CODE` and the chat drives this workstation.",
		},
		{ID: "slack", Name: "Slack", Available: false,
			Reason: "not built yet — needs a Slack Socket Mode driver",
			Setup:  "Telegram works today and uses the same pairing flow."},
		{ID: "whatsapp", Name: "WhatsApp", Available: false,
			Reason: "not built yet — needs WhatsApp Business API access",
			Setup:  "Telegram works today and uses the same pairing flow."},
	}
	for i := range out {
		if l, ok := byChannel[out[i].ID]; ok {
			out[i].Linked = true
			out[i].ChatID = l.ChatID
			out[i].ReadOnly = l.ReadOnly
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": out, "bot_username": tgBot})
}

func reasonIf(cond bool, msg string) string {
	if cond {
		return msg
	}
	return ""
}
