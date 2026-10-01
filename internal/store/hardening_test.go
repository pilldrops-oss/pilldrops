package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Schema-hardening tests. Several migrations use a different mechanism per
// backend, so these run on both.

// TestEveryMigrationReplaysCleanly re-runs the whole migration list against a
// schema already at head: a version missing from schema_migrations is
// re-applied on the next Open, and a migration that is not re-runnable means a
// store that will not open.
func TestEveryMigrationReplaysCleanly(t *testing.T) {
	s := openTemp(t)
	if _, err := s.exec(t.Context(), `DELETE FROM schema_migrations`); err != nil {
		t.Fatalf("clear schema_migrations: %v", err)
	}
	if err := s.migrate(t.Context()); err != nil {
		t.Fatalf("replaying every migration against a current schema failed: %v\n"+
			"a migration must survive being applied twice — see the objectsMayBeGone note in migrations.go", err)
	}
}

// TestValueConstraintsRejectBadValues covers one damaging value per constrained table.
func TestValueConstraintsRejectBadValues(t *testing.T) {
	s := openTemp(t)
	now := time.Now().Unix()

	for _, c := range []struct {
		what string
		sql  string
		args []any
	}{
		{
			"sandbox state outside the state machine",
			`INSERT INTO sandboxes (id, template, org_id, state, created_at, expires_at, updated_at)
			 VALUES (?, 'base', 'o', 'wedged', ?, 0, ?)`,
			[]any{"sb-bad-state", now, now},
		},
		{
			"negative vcpu",
			`INSERT INTO sandboxes (id, template, org_id, state, vcpu, created_at, expires_at, updated_at)
			 VALUES (?, 'base', 'o', 'running', -1, ?, 0, ?)`,
			[]any{"sb-bad-vcpu", now, now},
		},
		{
			"port above 65535",
			`INSERT INTO sandboxes (id, template, org_id, state, public_port, created_at, expires_at, updated_at)
			 VALUES (?, 'base', 'o', 'running', 99999, ?, 0, ?)`,
			[]any{"sb-bad-port", now, now},
		},
		{
			"api key role the auth layer does not know",
			`INSERT INTO api_keys (id, name, key_hash, org_id, role, created_at) VALUES (?, 'k', ?, 'o', 'superuser', ?)`,
			[]any{"key-bad-role", "hash-bad-role", now},
		},
		{
			"admin role the console does not know",
			`INSERT INTO admin_users (email, pass_hash, role, created_at) VALUES (?, 'h', 'root', ?)`,
			[]any{"bad@example.com", now},
		},
		{
			"approval state that is neither a decision nor a timeout",
			`INSERT INTO agent_approvals (id, sandbox_id, org_id, state, requested_at) VALUES (?, 's', 'o', 'maybe', ?)`,
			[]any{"ap-bad", now},
		},
		{
			"turn state no runner produces",
			`INSERT INTO agent_turns (id, sandbox_id, org_id, state) VALUES (?, 's', 'o', 'thinking')`,
			[]any{"turn-bad"},
		},
		{
			"delivery state that is not the dead-letter marker either",
			`INSERT INTO agent_webhook_deliveries (id, webhook_id, sandbox_id, org_id, event_type, state)
			 VALUES (?, 'w', 's', 'o', 'turn.done', 'maybe')`,
			[]any{"dl-bad"},
		},
		{
			"HTTP status that is not one",
			`INSERT INTO agent_webhook_deliveries (id, webhook_id, sandbox_id, org_id, event_type, state, last_status)
			 VALUES (?, 'w', 's', 'o', 'turn.done', 'failed', 9000)`,
			[]any{"dl-bad-status"},
		},
		{
			"cron agent_mode that is neither shell nor a known agent mode",
			`INSERT INTO cron_jobs (id, org_id, sandbox_id, schedule, command, agent_mode, created_at)
			 VALUES (?, 'o', 's', '* * * * *', 'echo hi', 'yolo', ?)`,
			[]any{"cron-bad", now},
		},
		{
			"chat message from a third speaker",
			`INSERT INTO chat_messages (sandbox_id, org_id, role, text, at) VALUES ('s', 'o', 'system', 'hi', ?)`,
			[]any{now},
		},
		{
			"negative meter reading",
			`INSERT INTO usage_records (sandbox_id, org_id, vcpu_seconds, recorded_at) VALUES ('s', 'o', -5, ?)`,
			[]any{now},
		},
		{
			"negative rollup",
			`INSERT INTO usage_totals (org_id, sandbox_id, run_seconds, updated_at) VALUES ('o', 's', -1, ?)`,
			[]any{now},
		},
		{
			"function port above 65535",
			`INSERT INTO functions (name, org_id, template, port, created_at, updated_at) VALUES ('f', 'o', 'base', 70000, ?, ?)`,
			[]any{now, now},
		},
	} {
		if _, err := s.exec(t.Context(), c.sql, c.args...); err == nil {
			t.Errorf("%s: the database accepted it — the constraint from migration 48 is not enforced", c.what)
		}
	}
}

func TestValueConstraintsAllowGoodValues(t *testing.T) {
	s := openTemp(t)
	now := time.Now().Unix()
	for _, st := range []string{"pending", "running", "pausing", "paused", "resuming", "stopped", "failed"} {
		if err := s.SaveSandbox(t.Context(), SandboxRecord{
			ID: "sb-" + st, Template: "base", OrgID: "o", State: st,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Errorf("state %q rejected, but it is one daemon.State produces: %v", st, err)
		}
	}
	// Port zero means "template default" and must pass the range check.
	if err := s.SaveFunction(t.Context(), FunctionRecord{
		Name: "f", OrgID: "o", Template: "base", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Errorf("a function with port 0 (= template default) was rejected: %v", err)
	}
}

// TestUsageTotalsMatchTheMeter pins that the rollup matches the detail rows
// and survives pruning them.
func TestUsageTotalsMatchTheMeter(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()

	for range 3 {
		if err := s.RecordUsage(ctx, "sb-1", "org-x", UsageDelta{
			VCPUSeconds: 2, RAMGBSeconds: 1, StorageGBSeconds: 0.5, RunSeconds: 1,
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	if err := s.RecordUsageBatch(ctx, []UsageEntry{
		{SandboxID: "sb-2", OrgID: "org-x", Delta: UsageDelta{VCPUSeconds: 4, RunSeconds: 2}},
		// Same sandbox twice in one batch; Postgres rejects an upsert that
		// touches a row twice, so these must be merged first.
		{SandboxID: "sb-3", OrgID: "org-x", Delta: UsageDelta{VCPUSeconds: 1, RunSeconds: 1}},
		{SandboxID: "sb-3", OrgID: "org-x", Delta: UsageDelta{VCPUSeconds: 1, RunSeconds: 1}},
	}); err != nil {
		t.Fatalf("batch: %v", err)
	}

	sum, err := s.UsageSummary(ctx, "org-x")
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum.VCPUSeconds != 12 || sum.RunSeconds != 7 || sum.Sandboxes != 3 {
		t.Fatalf("summary = %+v, want vcpu 12, run 7, 3 sandboxes", sum)
	}

	var usage RetentionTarget
	for _, tg := range RetentionTargets {
		if tg.Name == "usage" {
			usage = tg
		}
	}
	if _, err := s.PruneOlderThan(ctx, usage, time.Now().Unix()+1); err != nil {
		t.Fatalf("prune: %v", err)
	}
	var left int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM usage_records`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d meter rows survived the prune", left)
	}
	after, err := s.UsageSummary(ctx, "org-x")
	if err != nil {
		t.Fatalf("summary after prune: %v", err)
	}
	if after != sum {
		t.Fatalf("pruning the meter changed lifetime usage: %+v -> %+v", sum, after)
	}
}

func TestRecordUsageClampsNegativeDeltas(t *testing.T) {
	s := openTemp(t)
	if err := s.RecordUsage(t.Context(), "sb", "org-neg", UsageDelta{VCPUSeconds: -100, RunSeconds: 5}); err != nil {
		t.Fatalf("record: %v", err)
	}
	sum, err := s.UsageSummary(t.Context(), "org-neg")
	if err != nil {
		t.Fatal(err)
	}
	if sum.VCPUSeconds != 0 || sum.RunSeconds != 5 {
		t.Fatalf("summary = %+v, want the negative floored to 0 and the rest kept", sum)
	}
}

// TestRetentionTargetsAreRealTablesAndColumns runs every declared prune, since
// a typo in a name only ever surfaces as a sweeper log line.
func TestRetentionTargetsAreRealTablesAndColumns(t *testing.T) {
	s := openTemp(t)
	if len(RetentionTargets) == 0 {
		t.Fatal("no retention targets declared")
	}
	for _, tg := range RetentionTargets {
		if _, err := s.PruneOlderThan(t.Context(), tg, 1); err != nil {
			t.Errorf("retention target %q does not match the schema: %v", tg.Name, err)
		}
		if tg.DefaultDays <= 0 {
			t.Errorf("retention target %q defaults to keeping everything, which is what this list exists to stop", tg.Name)
		}
	}
}

func TestRetentionDeletesOnlyOldRows(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()
	now := time.Now().Unix()
	old, recent := now-100_000, now-1

	for _, at := range []int64{old, recent} {
		if err := s.RecordEvent(ctx, EventRecord{
			OrgID: "org-r", SandboxID: fmt.Sprintf("sb-%d", at), Event: "created",
			Template: "base", ActiveAfter: 1, At: at,
		}); err != nil {
			t.Fatalf("record event: %v", err)
		}
	}
	var target RetentionTarget
	for _, tg := range RetentionTargets {
		if tg.Name == "events" {
			target = tg
		}
	}
	n, err := s.PruneOlderThan(ctx, target, now-1000)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned %d rows, want exactly the one older than the cutoff", n)
	}
	var left int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM sandbox_events WHERE org_id = 'org-r'`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Fatalf("%d events left, want the recent one kept", left)
	}
}

// TestBooleanFlagsRoundTrip checks the converted boolean columns on both
// backends, including DueCronJobs filtering with `WHERE enabled`.
func TestBooleanFlagsRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()

	code, err := s.CreateLinkCode(ctx, "sb-1", "org-b", true, true)
	if err != nil {
		t.Fatalf("create code: %v", err)
	}
	link, err := s.RedeemLinkCode(ctx, code, "telegram", "chat-1")
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if !link.ReadOnly || !link.Autonomous {
		t.Fatalf("flags lost through the code: %+v", link)
	}
	got, ok, err := s.ChatLinkFor(ctx, "telegram", "chat-1")
	if err != nil || !ok {
		t.Fatalf("link lookup: err=%v ok=%v", err, ok)
	}
	if !got.ReadOnly || !got.Autonomous {
		t.Fatalf("flags lost on read back: %+v", got)
	}

	job, err := s.CreateCronJob(ctx, CronJob{
		OrgID: "org-b", SandboxID: "sb-1", Schedule: "* * * * *", Kind: CronKindBackup,
	}, time.Now().Unix()-1)
	if err != nil {
		t.Fatalf("create cron: %v", err)
	}
	if !job.Enabled {
		t.Fatal("a new cron job should be enabled")
	}
	due, err := s.DueCronJobs(ctx, "", time.Now().Unix(), 10)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("%d jobs due, want 1 — `WHERE enabled` is not matching the converted column", len(due))
	}
	if _, err := s.SetCronEnabled(ctx, "org-b", job.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	due, err = s.DueCronJobs(ctx, "", time.Now().Unix(), 10)
	if err != nil {
		t.Fatalf("due after disable: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("%d jobs due after disabling, want 0", len(due))
	}
}

func TestDeadBillingTablesAreGone(t *testing.T) {
	s := openTemp(t)
	for _, tbl := range []string{"plans", "pricing", "billing_cursor", "credit_ledger", "org_credits"} {
		if _, err := s.exec(t.Context(), `SELECT 1 FROM `+tbl); err == nil {
			t.Errorf("%s still exists: this core prices nothing, so a rate card in its schema is a trap for the next reader", tbl)
		}
	}
}

// TestNewIndexesExist guards the indexes the retention prunes depend on.
func TestNewIndexesExist(t *testing.T) {
	s := openTemp(t)
	for _, idx := range []string{
		"idx_usage_org", "idx_usage_at", "idx_events_at", "idx_history_at", "idx_idem_at",
		"idx_login_events_owner", "idx_audit_at", "idx_launch_timings_at",
		"agent_webhook_deliveries_due", "agent_webhook_deliveries_created",
	} {
		q := `SELECT name FROM sqlite_master WHERE type='index' AND name = ?`
		if s.dialect == dialectPostgres {
			q = `SELECT indexname AS name FROM pg_indexes WHERE indexname = ?`
		}
		var name string
		if err := s.queryRow(t.Context(), q, idx).Scan(&name); err != nil {
			t.Errorf("index %s missing: %v", idx, err)
		}
	}

	// idx_usage_org must be the composite index, not the single-column one it replaced.
	q := `SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_usage_org'`
	if s.dialect == dialectPostgres {
		q = `SELECT indexdef FROM pg_indexes WHERE indexname='idx_usage_org'`
	}
	var def string
	if err := s.queryRow(t.Context(), q).Scan(&def); err != nil {
		t.Fatalf("read idx_usage_org: %v", err)
	}
	if !strings.Contains(def, "recorded_at") {
		t.Fatalf("idx_usage_org is still org-only: %s", def)
	}
}

// retentionTargetForTest looks a target up by name.
func retentionTargetForTest(t *testing.T, name string) RetentionTarget {
	t.Helper()
	for _, tg := range RetentionTargets {
		if tg.Name == name {
			return tg
		}
	}
	t.Fatalf("no retention target named %q", name)
	return RetentionTarget{}
}
