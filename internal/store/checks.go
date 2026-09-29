package store

import (
	"fmt"
	"strings"
)

// Value constraints for existing tables, declared once and rendered per
// backend: a CHECK constraint on Postgres, a BEFORE trigger on SQLite (ALTER
// TABLE cannot ADD CONSTRAINT there). Both apply to new writes only; the
// Postgres constraints are NOT VALID so a legacy row cannot fail the migration,
// and an operator can VALIDATE CONSTRAINT later.
//
// Open vocabularies whose writer must never fail (exit_state, event, agent
// event type, artifact kind) are deliberately not constrained.
//
// Never edit this list: it is the frozen content of migration 48. A new or
// changed invariant is a new migration.
var valueChecks = []struct {
	table string
	name  string
	// predicate has `{}` before each column: empty on Postgres, `NEW.` in a SQLite trigger.
	predicate string
	why       string
}{
	{
		table: "sandboxes", name: "sandboxes_state_known",
		predicate: `{}state IN ('pending','running','pausing','paused','resuming','stopped','failed','destroyed','error')`,
		why: "daemon.State is the state machine; an unknown value is a sandbox no transition applies to. " +
			"'destroyed' and 'error' are here because a READER expects them — CountPersistentForOrg filters " +
			"`state NOT IN ('destroyed','error')` — and because a fleet runs mixed node builds, so this build " +
			"must accept a state an older one writes. The set is still closed, which is the point",
	},
	{
		table: "sandboxes", name: "sandboxes_resources_sane",
		predicate: `{}vcpu >= 0 AND {}mem_mib >= 0 AND {}storage_mb >= 0 AND {}public_port BETWEEN 0 AND 65535`,
		why:       "negative resources are how a plan cap gets read as satisfied; a port is 16 bits",
	},
	{
		table: "api_keys", name: "api_keys_role_known",
		predicate: `{}role IN ('user','operator')`,
		why:       "'operator' is the role that reaches /v1/admin/*; a typo must not invent a third one",
	},
	{
		table: "admin_users", name: "admin_users_role_known",
		predicate: `{}role IN ('superadmin','admin')`,
		why:       "console authorization. Adding a role is a migration, which is right: every reader learns it",
	},
	{
		table: "agent_approvals", name: "agent_approvals_state_known",
		predicate: `{}state IN ('pending','allowed','denied','timed_out')`,
		why:       "the durable approval trail; migration 31 turns on `denied` vs `timed_out` being distinct",
	},
	{
		table: "agent_turns", name: "agent_turns_state_known",
		predicate: `{}state IN ('','queued','running','awaiting_approval','done','failed','interrupted')`,
		why:       "agentd.TurnState. '' is a mirrored turn whose state has not arrived yet",
	},
	{
		table: "agent_webhook_deliveries", name: "agent_webhook_deliveries_state_known",
		predicate: `{}state IN ('pending','sent','failed') AND {}attempts >= 0 AND {}last_status BETWEEN 0 AND 599`,
		why:       "'failed' IS the dead-letter marker (migration 33) — a typo silently un-DLQs a delivery",
	},
	{
		table: "agent_webhooks", name: "agent_webhooks_counters_sane",
		predicate: `{}consecutive_failures >= 0 AND {}last_status BETWEEN 0 AND 599`,
		why:       "consecutive_failures drives auto-disable; a negative one disables it",
	},
	{
		table: "cron_jobs", name: "cron_jobs_agent_mode_known",
		predicate: `{}agent_mode IN ('','guided','autonomous')`,
		why:       "'' means the command is a shell command; anything else hands it to an agent as a prompt",
	},
	{
		table: "chat_messages", name: "chat_messages_role_known",
		predicate: `{}role IN ('user','agent')`,
		why:       "a transcript with a third speaker renders as neither side",
	},
	{
		table: "usage_records", name: "usage_records_nonnegative",
		predicate: `{}vcpu_seconds >= 0 AND {}ram_gb_seconds >= 0 AND {}storage_gb_seconds >= 0 AND {}run_seconds >= 0`,
		why:       "a negative meter reading subtracts from consumption, which is a cap bypass with a receipt",
	},
	{
		// Every scoped query coerces an empty org to 'default', so a stored ''
		// is an orphan no tenant can see.
		table: "sandboxes", name: "sandboxes_org_id_present",
		predicate: `{}org_id <> ''`,
		why:       "a sandbox with no tenant is invisible to every list and still consumes a node",
	},
	{
		table: "api_keys", name: "api_keys_org_id_present",
		predicate: `{}org_id <> ''`,
		why:       "a credential scoped to no org authenticates into nothing, or into whatever a future reader assumes",
	},
	{
		table: "usage_records", name: "usage_records_org_id_present",
		predicate: `{}org_id <> ''`,
		why:       "consumption nobody can be shown; the same argument migration 44 makes for node_class",
	},
	{
		table: "functions", name: "functions_limits_sane",
		predicate: `{}port BETWEEN 0 AND 65535 AND {}vcpu >= 0 AND {}mem_mib >= 0 AND {}max_replicas >= 0`,
		why: "same class as sandboxes_resources_sane, for the function path. 0 is allowed for port for the " +
			"same reason it is for vcpu: FunctionRecord uses zero to mean 'template default', and the column " +
			"DEFAULT 8000 never fires because SaveFunction always writes the column",
	},
}

// checkStatements renders valueChecks for one backend.
func checkStatements(d dialect) []string {
	out := make([]string, 0, len(valueChecks)*2)
	for _, c := range valueChecks {
		if d == dialectPostgres {
			out = append(out, fmt.Sprintf(
				`ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s) NOT VALID`,
				c.table, c.name, strings.ReplaceAll(c.predicate, "{}", "")))
			continue
		}
		pred := strings.ReplaceAll(c.predicate, "{}", "NEW.")
		for _, ev := range []string{"INSERT", "UPDATE"} {
			out = append(out, fmt.Sprintf(
				"CREATE TRIGGER IF NOT EXISTS %s_%s BEFORE %s ON %s\n"+
					"FOR EACH ROW WHEN NOT (%s)\n"+
					"BEGIN SELECT RAISE(ABORT, '%s'); END",
				c.name, strings.ToLower(ev[:3]), ev, c.table, pred, c.name))
		}
	}
	return out
}
