package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// newWaitlistHandler builds the public POST /waitlist handler. The forward to
// the orchestrator is synchronous and is the durability: there is no local
// copy, so the caller hears "ok" only once the signup is persisted.
func newWaitlistHandler(orchURL, orchKey, webhookURL string) http.HandlerFunc {
	client := &http.Client{Timeout: 8 * time.Second}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var in struct {
			Email   string `json:"email"`
			Source  string `json:"source"`
			Note    string `json:"note"`
			Website string `json:"website"` // honeypot; real users leave this empty
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil {
			http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
			return
		}
		if in.Website != "" { // a bot filled the honeypot; pretend success
			writeBytes(w, []byte(`{"ok":true}`))
			return
		}
		email := strings.ToLower(strings.TrimSpace(in.Email))
		if email == "" || len(email) > 254 || !strings.Contains(email, "@") {
			http.Error(w, `{"error":"a valid email is required"}`, http.StatusBadRequest)
			return
		}
		source := strings.TrimSpace(in.Source)
		if source == "" {
			source = "landing"
		}
		note := strings.TrimSpace(in.Note)
		if len(note) > 2000 {
			note = note[:2000]
		}

		if orchURL == "" || orchKey == "" {
			http.Error(w, `{"error":"waitlist is temporarily unavailable — please try again shortly"}`, http.StatusServiceUnavailable)
			return
		}
		payload, err := json.Marshal(map[string]string{"email": email, "source": source, "note": note})
		if err != nil {
			http.Error(w, `{"error":"waitlist is temporarily unavailable"}`, http.StatusInternalServerError)
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(orchURL, "/")+"/v1/waitlist", bytes.NewReader(payload))
		if err != nil {
			http.Error(w, `{"error":"waitlist is temporarily unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		req.Header.Set("Authorization", "Bearer "+orchKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("[waitlist] forward failed: %v", err)
			http.Error(w, `{"error":"waitlist is temporarily unavailable — please try again shortly"}`, http.StatusServiceUnavailable)
			return
		}
		defer closeQuietly(resp.Body, "waitlist")
		if resp.StatusCode >= 300 {
			// The body is only for the log line, so a partial read still logs what it got.
			body, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			if rerr != nil {
				log.Printf("[waitlist] reading orchestrator error body: %v", rerr)
			}
			log.Printf("[waitlist] orchestrator returned %d: %s", resp.StatusCode, body)
			http.Error(w, `{"error":"waitlist is temporarily unavailable"}`, http.StatusBadGateway)
			return
		}

		// Best-effort real-time ping.
		if webhookURL != "" {
			text := fmt.Sprintf("\U0001F4DD Waitlist signup (%s): %s", source, email)
			go func() {
				p, err := json.Marshal(map[string]string{"content": text, "text": text})
				if err != nil {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				hr, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(p))
				if err != nil {
					return
				}
				hr.Header.Set("Content-Type", "application/json")
				if resp, err := http.DefaultClient.Do(hr); err == nil { //nolint:bodyclose // closed by closeQuietly on the next line
					closeQuietly(resp.Body, "waitlist webhook")
				}
			}()
		}
		writeBytes(w, []byte(`{"ok":true}`))
	}
}
