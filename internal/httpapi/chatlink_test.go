//go:build !core

package httpapi

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestChatLinkCodeRequiresOwnership(t *testing.T) {
	srv, st, key := newTestServerWithStore(t)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`)
	if code != http.StatusCreated {
		t.Fatalf("setup create = %d (%v)", code, out)
	}
	id, _ := out["id"].(string)

	code, out = do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/chat-link", key, `{}`)
	if code != http.StatusOK {
		t.Fatalf("mint = %d (%v)", code, out)
	}
	got, _ := out["code"].(string)
	if len(got) < 8 {
		t.Errorf("code %q is too short to be unguessable", got)
	}
	if ro, _ := out["read_only"].(bool); ro {
		t.Error("an absent body must default to a read-write link")
	}

	_, otherKey, err := st.CreateKey(t.Context(), "other", "org_other")
	if err != nil {
		t.Fatal(err)
	}
	code, _ = do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/chat-link", otherKey, `{}`)
	if code != http.StatusNotFound {
		t.Errorf("foreign org minting a link code = %d, want 404", code)
	}

	link, err := st.RedeemLinkCode(t.Context(), got, "telegram", "chat_1")
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if link.SandboxID != id {
		t.Errorf("code bound to %q, want %q", link.SandboxID, id)
	}
}

func TestChatLinkCodeReadOnly(t *testing.T) {
	srv, st, key := newTestServerWithStore(t)
	_, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`)
	id, _ := out["id"].(string)

	_, out = do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/chat-link", key, `{"read_only":true}`)
	got, _ := out["code"].(string)
	link, err := st.RedeemLinkCode(t.Context(), got, "telegram", "chat_ro")
	if err != nil {
		t.Fatal(err)
	}
	if !link.ReadOnly {
		t.Error("read_only was lost between minting and redeeming — the link would silently gain write access")
	}
}

func TestChatLinkDeepLink(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_USERNAME", "@example_bot")
	srv, _, key := newTestServerWithStore(t)
	_, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`)
	id, _ := out["id"].(string)

	_, out = do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/chat-link", key, `{}`)
	url, _ := out["telegram_url"].(string)
	code, _ := out["code"].(string)
	if !strings.HasPrefix(url, "https://t.me/example_bot?start=") {
		t.Errorf("deep link = %q", url)
	}
	if !strings.HasSuffix(url, code) {
		t.Errorf("deep link %q does not carry the code %q", url, code)
	}
}

func TestChatLinkDeepLinkAbsentWithoutBotName(t *testing.T) {
	os.Unsetenv("TELEGRAM_BOT_USERNAME")
	srv, _, key := newTestServerWithStore(t)
	_, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`)
	id, _ := out["id"].(string)

	_, out = do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/chat-link", key, `{}`)
	if u, _ := out["telegram_url"].(string); u != "" {
		t.Errorf("emitted a deep link with no bot configured: %q", u)
	}
	if c, _ := out["code"].(string); c == "" {
		t.Error("no code either — the feature is unusable rather than degraded")
	}
}

func TestChatLinkRevoke(t *testing.T) {
	srv, st, key := newTestServerWithStore(t)
	_, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`)
	id, _ := out["id"].(string)

	_, out = do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/chat-link", key, `{}`)
	c, _ := out["code"].(string)
	if _, err := st.RedeemLinkCode(t.Context(), c, "telegram", "chat_lost"); err != nil {
		t.Fatal(err)
	}

	code, _ := do(t, "DELETE", srv.URL+"/v1/sandboxes/"+id+"/chat-link", key, "")
	if code != http.StatusOK {
		t.Fatalf("revoke = %d", code)
	}
	if _, ok, _ := st.ChatLinkFor(t.Context(), "telegram", "chat_lost"); ok {
		t.Error("link survived revocation")
	}
}

// A later sandbox reusing the id must not inherit a stranger's phone.
func TestChatLinksDieWithSandbox(t *testing.T) {
	srv, st, key := newTestServerWithStore(t)
	_, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`)
	id, _ := out["id"].(string)

	_, out = do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/chat-link", key, `{}`)
	c, _ := out["code"].(string)
	if _, err := st.RedeemLinkCode(t.Context(), c, "telegram", "chat_doomed"); err != nil {
		t.Fatal(err)
	}

	if code, _ := do(t, "DELETE", srv.URL+"/v1/sandboxes/"+id, key, ""); code != http.StatusOK {
		t.Fatalf("destroy = %d", code)
	}
	if _, ok, _ := st.ChatLinkFor(t.Context(), "telegram", "chat_doomed"); ok {
		t.Error("a chat link outlived its destroyed workstation")
	}
}
