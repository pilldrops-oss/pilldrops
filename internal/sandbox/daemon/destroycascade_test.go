package daemon

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// Drives StoreAdapter.DeleteSandbox directly: it is the funnel shared by the
// HTTP handler, the TTL reaper and the abandonment sweep.
func TestDeleteSandboxCascades(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := t.Context()
	a := StoreAdapter{S: st}

	// Count rows off the raw file, not through the store's own readers.
	db, err := sql.Open("sqlite", filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer db.Close()

	const id, org = "sb-doomed", "org-1"
	const keep = "sb-survivor"

	seed := func(sandbox string) {
		t.Helper()
		if err := st.SaveSandbox(ctx, store.SandboxRecord{
			ID: sandbox, Template: "base", OrgID: org, State: "running",
			CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix(),
		}); err != nil {
			t.Fatalf("save sandbox: %v", err)
		}
		if _, err := st.MergeSandboxConfig(ctx, sandbox, org, map[string]string{"K": "v"}, "m", ""); err != nil {
			t.Fatalf("config: %v", err)
		}
		if err := st.SetSandboxIntegrations(ctx, sandbox, org, []string{"github"}); err != nil {
			t.Fatalf("integrations: %v", err)
		}
		if err := st.AppendChatMessage(ctx, sandbox, org, "user", "hello"); err != nil {
			t.Fatalf("chat: %v", err)
		}
		if _, err := st.CreateCronJob(ctx, store.CronJob{
			OrgID: org, SandboxID: sandbox, Schedule: "* * * * *", Command: "echo hi",
		}, time.Now().Unix()); err != nil {
			t.Fatalf("cron: %v", err)
		}
		code, err := st.CreateLinkCode(ctx, sandbox, org, false, false)
		if err != nil {
			t.Fatalf("link code: %v", err)
		}
		if _, err := st.RedeemLinkCode(ctx, code, "telegram", "chat-"+sandbox); err != nil {
			t.Fatalf("redeem: %v", err)
		}
		if _, err := st.CreateAgentWebhook(ctx, store.AgentWebhook{
			SandboxID: sandbox, OrgID: org, URL: "https://example.invalid/hook", Enabled: true,
		}, "secret", 0); err != nil {
			t.Fatalf("webhook: %v", err)
		}
	}
	seed(id)
	seed(keep)

	// The agent mirror deliberately outlives the sandbox.
	if err := st.MirrorTurn(ctx, store.MirroredTurn{
		ID: "turn-1", SandboxID: id, OrgID: org, State: "done",
	}); err != nil {
		t.Fatalf("mirror turn: %v", err)
	}

	if err := a.DeleteSandbox(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}

	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := db.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	for _, c := range []struct {
		what  string
		query string
	}{
		{"the sandbox row", `SELECT COUNT(*) FROM sandboxes WHERE id = ?`},
		{"its config", `SELECT COUNT(*) FROM sandbox_config WHERE sandbox_id = ?`},
		{"its integrations", `SELECT COUNT(*) FROM sandbox_integrations WHERE sandbox_id = ?`},
		{"its transcript", `SELECT COUNT(*) FROM chat_messages WHERE sandbox_id = ?`},
		{"its cron jobs", `SELECT COUNT(*) FROM cron_jobs WHERE sandbox_id = ?`},
		{"its chat links", `SELECT COUNT(*) FROM chat_links WHERE sandbox_id = ?`},
		{"its webhooks", `SELECT COUNT(*) FROM agent_webhooks WHERE sandbox_id = ?`},
	} {
		if n := count(c.query, id); n != 0 {
			t.Errorf("%s survived the destroy (%d rows) — it is now unreachable and will never be cleaned", c.what, n)
		}
		if n := count(c.query, keep); n != 1 {
			t.Errorf("%s of an UNRELATED sandbox was destroyed too (%d rows, want 1)", c.what, n)
		}
	}

	if n := count(`SELECT COUNT(*) FROM agent_turns WHERE sandbox_id = ?`, id); n != 1 {
		t.Errorf("the agent mirror was deleted with the sandbox (%d turns left, want 1) — "+
			"'what did this agent do, and who let it' is asked AFTER the box is gone", n)
	}
}
