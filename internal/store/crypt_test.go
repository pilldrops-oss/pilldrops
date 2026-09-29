package store

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

func TestCrypterRoundTrip(t *testing.T) {
	c, err := newCrypter(hex.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if err != nil || c == nil {
		t.Fatalf("newCrypter: c=%v err=%v", c, err)
	}
	sealed, err := c.seal(`{"API_KEY":"supersecret"}`)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if !strings.HasPrefix(sealed, "enc:v1:") {
		t.Errorf("sealed value missing enc prefix: %q", sealed)
	}
	if strings.Contains(sealed, "supersecret") {
		t.Errorf("plaintext leaked in sealed value: %q", sealed)
	}
	got, err := c.open(sealed)
	if err != nil || got != `{"API_KEY":"supersecret"}` {
		t.Fatalf("open: got=%q err=%v", got, err)
	}
}

func TestCrypterDisabledPassthrough(t *testing.T) {
	c, err := newCrypter("")
	if err != nil || c != nil {
		t.Fatalf("empty key must give nil crypter: c=%v err=%v", c, err)
	}
	if s, _ := c.seal("plain"); s != "plain" {
		t.Errorf("nil seal changed value: %q", s)
	}
	if o, _ := c.open("plain"); o != "plain" {
		t.Errorf("nil open changed value: %q", o)
	}
}

func TestCrypterOpensLegacyPlaintext(t *testing.T) {
	c, _ := newCrypter(hex.EncodeToString(bytes.Repeat([]byte{1}, 32)))
	got, err := c.open(`{"X":"1"}`)
	if err != nil || got != `{"X":"1"}` {
		t.Fatalf("legacy plaintext: got=%q err=%v", got, err)
	}
}

func TestCrypterBadKey(t *testing.T) {
	if _, err := newCrypter("nothex!!"); err == nil {
		t.Error("non-hex key: want error")
	}
	if _, err := newCrypter("aabb"); err == nil {
		t.Error("short key: want error")
	}
}

func TestSandboxEnvEncryptedAtRest(t *testing.T) {
	st := openTemp(t)
	if err := st.SetSecretKey(hex.EncodeToString(bytes.Repeat([]byte{9}, 32))); err != nil {
		t.Fatalf("set key: %v", err)
	}
	rec := SandboxRecord{
		ID: "sb_enc", Template: "base", OrgID: "o", State: "paused",
		Env: `{"DB_PASSWORD":"hunter2"}`, CreatedAt: 1, ExpiresAt: 2, UpdatedAt: 3,
	}
	if err := st.SaveSandbox(t.Context(), rec); err != nil {
		t.Fatalf("save: %v", err)
	}

	var raw string
	if err := st.queryRow(t.Context(), `SELECT env FROM sandboxes WHERE id = ?`, "sb_enc").Scan(&raw); err != nil {
		t.Fatalf("read raw env: %v", err)
	}
	if strings.Contains(raw, "hunter2") {
		t.Errorf("secret stored in plaintext at rest: %q", raw)
	}
	if !strings.HasPrefix(raw, "enc:v1:") {
		t.Errorf("env not encrypted: %q", raw)
	}

	list, err := st.LoadSandboxes(t.Context())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var got string
	for _, r := range list {
		if r.ID == "sb_enc" {
			got = r.Env
		}
	}
	if got != `{"DB_PASSWORD":"hunter2"}` {
		t.Fatalf("decrypted env = %q, want the original JSON", got)
	}
}
