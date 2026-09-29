package store

import (
	"testing"
	"time"
)

func TestChatLinkRedeemBindsChat(t *testing.T) {
	s := openTemp(t)

	code, err := s.CreateLinkCode(t.Context(), "sb_1", "org_1", false, false)
	if err != nil {
		t.Fatalf("create code: %v", err)
	}
	if len(code) < 8 {
		t.Errorf("code %q is too short to be unguessable", code)
	}

	link, err := s.RedeemLinkCode(t.Context(), code, "telegram", "chat_99")
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if link.SandboxID != "sb_1" || link.OrgID != "org_1" || link.ChatID != "chat_99" {
		t.Fatalf("link = %+v", link)
	}

	got, ok, err := s.ChatLinkFor(t.Context(), "telegram", "chat_99")
	if err != nil || !ok {
		t.Fatalf("lookup = %v, %v", ok, err)
	}
	if got.SandboxID != "sb_1" {
		t.Errorf("looked up sandbox %q, want sb_1", got.SandboxID)
	}
}

func TestChatLinkCodeIsSingleUse(t *testing.T) {
	s := openTemp(t)
	code, _ := s.CreateLinkCode(t.Context(), "sb_1", "org_1", false, false)

	if _, err := s.RedeemLinkCode(t.Context(), code, "telegram", "chat_a"); err != nil {
		t.Fatalf("first redeem: %v", err)
	}
	if _, err := s.RedeemLinkCode(t.Context(), code, "telegram", "chat_b"); err == nil {
		t.Fatal("a link code was redeemed twice — a shoulder-surfed code would grant a stranger shell access")
	}
	if _, ok, _ := s.ChatLinkFor(t.Context(), "telegram", "chat_b"); ok {
		t.Fatal("second chat got linked despite the failed redeem")
	}
}

func TestChatLinkCodeExpires(t *testing.T) {
	s := openTemp(t)
	code, _ := s.CreateLinkCode(t.Context(), "sb_1", "org_1", false, false)
	if _, err := s.exec(t.Context(), `UPDATE chat_link_codes SET expires_at = ? WHERE code = ?`,
		time.Now().Unix()-1, code); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemLinkCode(t.Context(), code, "telegram", "chat_a"); err == nil {
		t.Fatal("an expired code was accepted")
	}
	// The expired code must also be burned.
	var n int
	if err := s.queryRow(t.Context(), `SELECT COUNT(*) FROM chat_link_codes WHERE code = ?`, code).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("an expired code survived redemption — it must burn either way")
	}
}

func TestChatLinkRebindReplaces(t *testing.T) {
	s := openTemp(t)
	c1, _ := s.CreateLinkCode(t.Context(), "sb_1", "org_1", false, false)
	c2, _ := s.CreateLinkCode(t.Context(), "sb_2", "org_1", false, false)

	if _, err := s.RedeemLinkCode(t.Context(), c1, "telegram", "chat_x"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemLinkCode(t.Context(), c2, "telegram", "chat_x"); err != nil {
		t.Fatal(err)
	}
	got, ok, _ := s.ChatLinkFor(t.Context(), "telegram", "chat_x")
	if !ok || got.SandboxID != "sb_2" {
		t.Fatalf("after rebind sandbox = %q (ok=%v), want sb_2", got.SandboxID, ok)
	}
}

func TestChatLinkReadOnlyRoundTrips(t *testing.T) {
	s := openTemp(t)
	code, _ := s.CreateLinkCode(t.Context(), "sb_1", "org_1", true, false)
	link, err := s.RedeemLinkCode(t.Context(), code, "telegram", "chat_ro")
	if err != nil {
		t.Fatal(err)
	}
	if !link.ReadOnly {
		t.Error("read-only lost on redeem")
	}
	got, _, _ := s.ChatLinkFor(t.Context(), "telegram", "chat_ro")
	if !got.ReadOnly {
		t.Error("read-only lost on lookup — the link would silently gain write access")
	}
}

func TestChatLinksDeletedWithSandbox(t *testing.T) {
	s := openTemp(t)
	c1, _ := s.CreateLinkCode(t.Context(), "sb_doomed", "org_1", false, false)
	c2, _ := s.CreateLinkCode(t.Context(), "sb_doomed", "org_1", false, false)
	if _, err := s.RedeemLinkCode(t.Context(), c1, "telegram", "chat_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemLinkCode(t.Context(), c2, "telegram", "chat_2"); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteChatLinksForSandbox(t.Context(), "sb_doomed"); err != nil {
		t.Fatal(err)
	}
	for _, chat := range []string{"chat_1", "chat_2"} {
		if _, ok, _ := s.ChatLinkFor(t.Context(), "telegram", chat); ok {
			t.Errorf("%s still linked to a destroyed sandbox", chat)
		}
	}
}

func TestDeleteChatLinkIdempotent(t *testing.T) {
	s := openTemp(t)
	if err := s.DeleteChatLink(t.Context(), "telegram", "never_linked"); err != nil {
		t.Errorf("unlink of an unlinked chat errored: %v", err)
	}
}

func TestRedeemRejectsBlankInput(t *testing.T) {
	s := openTemp(t)
	for _, tc := range [][3]string{{"", "telegram", "c"}, {"CODE", "", "c"}, {"CODE", "telegram", ""}} {
		if _, err := s.RedeemLinkCode(t.Context(), tc[0], tc[1], tc[2]); err == nil {
			t.Errorf("redeem accepted blank input %v", tc)
		}
	}
}

func TestChatLinkAutonomyRoundTrips(t *testing.T) {
	s := openTemp(t)
	for _, tc := range []struct{ readOnly, autonomous bool }{
		{false, false}, {false, true}, {true, false},
	} {
		code, err := s.CreateLinkCode(t.Context(), "sb_1", "org_1", tc.readOnly, tc.autonomous)
		if err != nil {
			t.Fatal(err)
		}
		link, err := s.RedeemLinkCode(t.Context(), code, "telegram", "chat_a")
		if err != nil {
			t.Fatal(err)
		}
		if link.ReadOnly != tc.readOnly || link.Autonomous != tc.autonomous {
			t.Errorf("redeem %+v gave read_only=%v autonomous=%v", tc, link.ReadOnly, link.Autonomous)
		}
		got, ok, _ := s.ChatLinkFor(t.Context(), "telegram", "chat_a")
		if !ok || got.Autonomous != tc.autonomous {
			t.Errorf("lookup %+v gave autonomous=%v", tc, got.Autonomous)
		}
	}
}

func TestChatLinkDefaultsToNonAutonomous(t *testing.T) {
	s := openTemp(t)
	code, _ := s.CreateLinkCode(t.Context(), "sb_1", "org_1", false, false)
	link, err := s.RedeemLinkCode(t.Context(), code, "telegram", "chat_d")
	if err != nil {
		t.Fatal(err)
	}
	if link.Autonomous {
		t.Error("a link defaulted to autonomous")
	}
}
