//go:build !core

package httpapi

import (
	"net/http"
	"testing"
)

func TestChannelsListsEveryChannelHonestly(t *testing.T) {
	srv, key, sb, _ := newCronSandbox(t)

	code, out := do(t, "GET", srv+"/v1/sandboxes/"+sb+"/channels", key, "")
	if code != http.StatusOK {
		t.Fatalf("channels = %d %v", code, out)
	}
	chans, _ := out["channels"].([]any)
	if len(chans) < 4 {
		t.Fatalf("got %d channels, want the full list", len(chans))
	}
	seen := map[string]map[string]any{}
	for _, c := range chans {
		m := c.(map[string]any)
		seen[m["id"].(string)] = m
	}
	for _, want := range []string{"telegram", "discord", "slack", "whatsapp"} {
		if seen[want] == nil {
			t.Errorf("%s missing — a channel we cannot do yet must still be listed, or the user hunts for a setting that is not there", want)
		}
	}
	for _, id := range []string{"discord", "slack", "whatsapp"} {
		if seen[id]["available"] != false {
			t.Errorf("%s claims to be available", id)
		}
		if seen[id]["reason"] == "" {
			t.Errorf("%s is unavailable with no reason given", id)
		}
	}
	if seen["telegram"]["linked"] != false {
		t.Error("a fresh workstation reports an existing Telegram link")
	}
}

func TestChannelsReflectsAnExistingLink(t *testing.T) {
	srv, key, sb, st := newCronSandbox(t)

	code, err := st.CreateLinkCode(t.Context(), sb, "default", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RedeemLinkCode(t.Context(), code, "telegram", "chat-42"); err != nil {
		t.Fatal(err)
	}

	_, out := do(t, "GET", srv+"/v1/sandboxes/"+sb+"/channels", key, "")
	for _, c := range out["channels"].([]any) {
		m := c.(map[string]any)
		if m["id"] != "telegram" {
			continue
		}
		if m["linked"] != true {
			t.Error("telegram not reported as linked after redeeming a code")
		}
		if m["chat_id"] != "chat-42" {
			t.Errorf("chat_id = %v, want chat-42", m["chat_id"])
		}
	}
}

func TestChannelsOwnership(t *testing.T) {
	srv, _, sb, st := newCronSandbox(t)
	_, otherKey, _ := st.CreateKey(t.Context(), "other", "org_other")
	if code, _ := do(t, "GET", srv+"/v1/sandboxes/"+sb+"/channels", otherKey, ""); code != http.StatusNotFound {
		t.Errorf("cross-tenant channels = %d, want 404", code)
	}
}
