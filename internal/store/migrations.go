package store

// migration is one versioned schema change. Statements are pre-split per
// dialect because pgx's Exec runs one statement per call.
type migration struct {
	version  int
	name     string
	sqlite   []string
	postgres []string
	// objectsMayBeGone makes "no such table/column" benign so a migration that
	// drops something, or whose target a later migration removes, survives a
	// replay. Opt-in so a reused version number still fails loudly.
	objectsMayBeGone bool
}

// migrations is the ordered, append-only list of schema versions. Never edit
// a shipped migration; add a new version, checked against every branch.
var migrations = []migration{
	{
		version:  1,
		name:     "initial_schema",
		sqlite:   append(splitSQL(sqliteSchema), sqliteRetrofits...),
		postgres: splitSQL(postgresSchema),
	},
	{
		version: 2,
		name:    "sandbox_history",
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS sandbox_history (
				id            TEXT PRIMARY KEY,
				org_id        TEXT NOT NULL DEFAULT 'default',
				owner         TEXT NOT NULL DEFAULT '',
				template      TEXT NOT NULL DEFAULT '',
				image         TEXT NOT NULL DEFAULT '',
				node          TEXT NOT NULL DEFAULT '',
				exit_state    TEXT NOT NULL DEFAULT '',
				created_at    INTEGER NOT NULL DEFAULT 0,
				destroyed_at  INTEGER NOT NULL,
				duration_secs INTEGER NOT NULL DEFAULT 0,
				vcpu          INTEGER NOT NULL DEFAULT 0,
				mem_mib       INTEGER NOT NULL DEFAULT 0,
				logs          TEXT NOT NULL DEFAULT ''
			)`,
			`CREATE INDEX IF NOT EXISTS idx_history_org_at ON sandbox_history(org_id, destroyed_at)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS sandbox_history (
				id            TEXT PRIMARY KEY,
				org_id        TEXT NOT NULL DEFAULT 'default',
				owner         TEXT NOT NULL DEFAULT '',
				template      TEXT NOT NULL DEFAULT '',
				image         TEXT NOT NULL DEFAULT '',
				node          TEXT NOT NULL DEFAULT '',
				exit_state    TEXT NOT NULL DEFAULT '',
				created_at    BIGINT NOT NULL DEFAULT 0,
				destroyed_at  BIGINT NOT NULL,
				duration_secs BIGINT NOT NULL DEFAULT 0,
				vcpu          BIGINT NOT NULL DEFAULT 0,
				mem_mib       BIGINT NOT NULL DEFAULT 0,
				logs          TEXT NOT NULL DEFAULT ''
			)`,
			`CREATE INDEX IF NOT EXISTS idx_history_org_at ON sandbox_history(org_id, destroyed_at)`,
		},
	},
	{
		version: 3,
		name:    "history_cost",
		sqlite: []string{
			`ALTER TABLE sandbox_history ADD COLUMN vcpu_seconds REAL NOT NULL DEFAULT 0`,
			`ALTER TABLE sandbox_history ADD COLUMN ram_gb_seconds REAL NOT NULL DEFAULT 0`,
			`ALTER TABLE sandbox_history ADD COLUMN cost_usd REAL NOT NULL DEFAULT 0`,
		},
		postgres: []string{
			`ALTER TABLE sandbox_history ADD COLUMN IF NOT EXISTS vcpu_seconds DOUBLE PRECISION NOT NULL DEFAULT 0`,
			`ALTER TABLE sandbox_history ADD COLUMN IF NOT EXISTS ram_gb_seconds DOUBLE PRECISION NOT NULL DEFAULT 0`,
			`ALTER TABLE sandbox_history ADD COLUMN IF NOT EXISTS cost_usd DOUBLE PRECISION NOT NULL DEFAULT 0`,
		},
	},
	{
		version: 4,
		name:    "sandbox_events",
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS sandbox_events (
				id           INTEGER PRIMARY KEY AUTOINCREMENT,
				org_id       TEXT NOT NULL DEFAULT 'default',
				sandbox_id   TEXT NOT NULL DEFAULT '',
				event        TEXT NOT NULL DEFAULT '',
				template     TEXT NOT NULL DEFAULT '',
				active_after INTEGER NOT NULL DEFAULT 0,
				at           INTEGER NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_events_org_at ON sandbox_events(org_id, at)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS sandbox_events (
				id           BIGSERIAL PRIMARY KEY,
				org_id       TEXT NOT NULL DEFAULT 'default',
				sandbox_id   TEXT NOT NULL DEFAULT '',
				event        TEXT NOT NULL DEFAULT '',
				template     TEXT NOT NULL DEFAULT '',
				active_after BIGINT NOT NULL DEFAULT 0,
				at           BIGINT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_events_org_at ON sandbox_events(org_id, at)`,
		},
	},
	{
		version: 5,
		name:    "sandbox_idle_timeout",
		sqlite: []string{
			`ALTER TABLE sandboxes ADD COLUMN idle_timeout INTEGER NOT NULL DEFAULT 0`,
		},
		postgres: []string{
			`ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS idle_timeout BIGINT NOT NULL DEFAULT 0`,
		},
	},
	{
		version: 6,
		name:    "users",
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS users (
				owner_id    TEXT PRIMARY KEY,
				user_id     TEXT NOT NULL DEFAULT '',
				email       TEXT NOT NULL DEFAULT '',
				first_seen  INTEGER NOT NULL,
				last_seen   INTEGER NOT NULL,
				login_count INTEGER NOT NULL DEFAULT 0
			)`,
			`CREATE INDEX IF NOT EXISTS idx_users_last_seen ON users(last_seen)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS users (
				owner_id    TEXT PRIMARY KEY,
				user_id     TEXT NOT NULL DEFAULT '',
				email       TEXT NOT NULL DEFAULT '',
				first_seen  BIGINT NOT NULL,
				last_seen   BIGINT NOT NULL,
				login_count BIGINT NOT NULL DEFAULT 0
			)`,
			`CREATE INDEX IF NOT EXISTS idx_users_last_seen ON users(last_seen)`,
		},
	},
	{
		version: 7,
		name:    "login_events",
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS login_events (
				id       INTEGER PRIMARY KEY AUTOINCREMENT,
				owner_id TEXT NOT NULL DEFAULT '',
				email    TEXT NOT NULL DEFAULT '',
				at       INTEGER NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_login_events_at ON login_events(at)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS login_events (
				id       BIGSERIAL PRIMARY KEY,
				owner_id TEXT NOT NULL DEFAULT '',
				email    TEXT NOT NULL DEFAULT '',
				at       BIGINT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_login_events_at ON login_events(at)`,
		},
	},
	{
		// source is "agent" or "human"; empty means unknown.
		version: 8,
		name:    "sandbox_source",
		sqlite: []string{
			`ALTER TABLE sandboxes ADD COLUMN source TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE sandbox_history ADD COLUMN source TEXT NOT NULL DEFAULT ''`,
		},
		postgres: []string{
			`ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE sandbox_history ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		version: 9,
		name:    "audit_detail",
		sqlite: []string{
			`ALTER TABLE audit_log ADD COLUMN detail TEXT NOT NULL DEFAULT ''`,
			`CREATE INDEX IF NOT EXISTS idx_audit_target ON audit_log(target)`,
		},
		postgres: []string{
			`ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS detail TEXT NOT NULL DEFAULT ''`,
			`CREATE INDEX IF NOT EXISTS idx_audit_target ON audit_log(target)`,
		},
	},
	{
		// client is the raw client tag (web, cli, mcp, sdk, api); empty means unknown.
		version: 10,
		name:    "sandbox_client",
		sqlite: []string{
			`ALTER TABLE sandboxes ADD COLUMN client TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE sandbox_history ADD COLUMN client TEXT NOT NULL DEFAULT ''`,
		},
		postgres: []string{
			`ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS client TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE sandbox_history ADD COLUMN IF NOT EXISTS client TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		version: 11,
		name:    "waitlist",
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS waitlist (
				email      TEXT PRIMARY KEY,
				source     TEXT NOT NULL DEFAULT '',
				note       TEXT NOT NULL DEFAULT '',
				created_at INTEGER NOT NULL
			)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS waitlist (
				email      TEXT PRIMARY KEY,
				source     TEXT NOT NULL DEFAULT '',
				note       TEXT NOT NULL DEFAULT '',
				created_at BIGINT NOT NULL
			)`,
		},
	},
	{
		version: 12,
		name:    "usage_storage_gb_seconds",
		sqlite: []string{
			`ALTER TABLE usage_records ADD COLUMN storage_gb_seconds REAL NOT NULL DEFAULT 0`,
		},
		postgres: []string{
			`ALTER TABLE usage_records ADD COLUMN IF NOT EXISTS storage_gb_seconds DOUBLE PRECISION NOT NULL DEFAULT 0`,
		},
	},
	{
		// run_seconds is wall-clock; it cannot be derived from vcpu_seconds once
		// sandboxes of different sizes are summed.
		version: 13,
		name:    "usage_run_seconds",
		sqlite: []string{
			`ALTER TABLE usage_records ADD COLUMN run_seconds REAL NOT NULL DEFAULT 0`,
		},
		postgres: []string{
			`ALTER TABLE usage_records ADD COLUMN IF NOT EXISTS run_seconds DOUBLE PRECISION NOT NULL DEFAULT 0`,
		},
	},
	{
		// A chat link is a credential, so link codes are single-use with a TTL
		// and kept apart from the live bindings.
		version: 14,
		name:    "chat_links",
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS chat_links (
				channel    TEXT NOT NULL,
				chat_id    TEXT NOT NULL,
				sandbox_id TEXT NOT NULL,
				org_id     TEXT NOT NULL DEFAULT 'default',
				read_only  INTEGER NOT NULL DEFAULT 0,
				created_at INTEGER NOT NULL,
				PRIMARY KEY (channel, chat_id)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_chat_links_sandbox ON chat_links(sandbox_id)`,
			`CREATE TABLE IF NOT EXISTS chat_link_codes (
				code       TEXT PRIMARY KEY,
				sandbox_id TEXT NOT NULL,
				org_id     TEXT NOT NULL DEFAULT 'default',
				read_only  INTEGER NOT NULL DEFAULT 0,
				created_at INTEGER NOT NULL,
				expires_at INTEGER NOT NULL
			)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS chat_links (
				channel    TEXT NOT NULL,
				chat_id    TEXT NOT NULL,
				sandbox_id TEXT NOT NULL,
				org_id     TEXT NOT NULL DEFAULT 'default',
				read_only  SMALLINT NOT NULL DEFAULT 0,
				created_at BIGINT NOT NULL,
				PRIMARY KEY (channel, chat_id)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_chat_links_sandbox ON chat_links(sandbox_id)`,
			`CREATE TABLE IF NOT EXISTS chat_link_codes (
				code       TEXT PRIMARY KEY,
				sandbox_id TEXT NOT NULL,
				org_id     TEXT NOT NULL DEFAULT 'default',
				read_only  SMALLINT NOT NULL DEFAULT 0,
				created_at BIGINT NOT NULL,
				expires_at BIGINT NOT NULL
			)`,
		},
	},
	{
		// autonomous defaults to 0, so the agent keeps its approval prompts.
		// The CREATE TABLEs give the ALTERs a table to hit on databases that
		// recorded version 14 without these tables.
		version: 15,
		name:    "chat_link_autonomy",
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS chat_links (
				channel    TEXT NOT NULL,
				chat_id    TEXT NOT NULL,
				sandbox_id TEXT NOT NULL,
				org_id     TEXT NOT NULL DEFAULT 'default',
				read_only  INTEGER NOT NULL DEFAULT 0,
				created_at INTEGER NOT NULL,
				PRIMARY KEY (channel, chat_id)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_chat_links_sandbox ON chat_links(sandbox_id)`,
			`CREATE TABLE IF NOT EXISTS chat_link_codes (
				code       TEXT PRIMARY KEY,
				sandbox_id TEXT NOT NULL,
				org_id     TEXT NOT NULL DEFAULT 'default',
				read_only  INTEGER NOT NULL DEFAULT 0,
				created_at INTEGER NOT NULL,
				expires_at INTEGER NOT NULL
			)`,
			`ALTER TABLE chat_links ADD COLUMN autonomous INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE chat_link_codes ADD COLUMN autonomous INTEGER NOT NULL DEFAULT 0`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS chat_links (
				channel    TEXT NOT NULL,
				chat_id    TEXT NOT NULL,
				sandbox_id TEXT NOT NULL,
				org_id     TEXT NOT NULL DEFAULT 'default',
				read_only  SMALLINT NOT NULL DEFAULT 0,
				created_at BIGINT NOT NULL,
				PRIMARY KEY (channel, chat_id)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_chat_links_sandbox ON chat_links(sandbox_id)`,
			`CREATE TABLE IF NOT EXISTS chat_link_codes (
				code       TEXT PRIMARY KEY,
				sandbox_id TEXT NOT NULL,
				org_id     TEXT NOT NULL DEFAULT 'default',
				read_only  SMALLINT NOT NULL DEFAULT 0,
				created_at BIGINT NOT NULL,
				expires_at BIGINT NOT NULL
			)`,
			`ALTER TABLE chat_links ADD COLUMN IF NOT EXISTS autonomous SMALLINT NOT NULL DEFAULT 0`,
			`ALTER TABLE chat_link_codes ADD COLUMN IF NOT EXISTS autonomous SMALLINT NOT NULL DEFAULT 0`,
		},
	},
	{
		// Dead columns on a table v43 drops; kept so older databases migrate
		// through in order. objectsMayBeGone covers the replay.
		version:          16,
		name:             "org_trials",
		objectsMayBeGone: true,
		sqlite: []string{
			`ALTER TABLE org_credits ADD COLUMN trial_ends_at INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE org_credits ADD COLUMN trial_stage TEXT NOT NULL DEFAULT ''`,
		},
		postgres: []string{
			`ALTER TABLE org_credits ADD COLUMN IF NOT EXISTS trial_ends_at BIGINT NOT NULL DEFAULT 0`,
			`ALTER TABLE org_credits ADD COLUMN IF NOT EXISTS trial_stage TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		// next_run_at is denormalised so the sweeper selects due jobs by index
		// instead of parsing every schedule on every tick.
		version: 17,
		name:    "cron_jobs",
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS cron_jobs (
				id TEXT PRIMARY KEY,
				org_id TEXT NOT NULL,
				sandbox_id TEXT NOT NULL,
				name TEXT NOT NULL DEFAULT '',
				schedule TEXT NOT NULL,
				timezone TEXT NOT NULL DEFAULT 'UTC',
				command TEXT NOT NULL,
				enabled INTEGER NOT NULL DEFAULT 1,
				next_run_at INTEGER NOT NULL DEFAULT 0,
				last_run_at INTEGER NOT NULL DEFAULT 0,
				last_status TEXT NOT NULL DEFAULT '',
				last_output TEXT NOT NULL DEFAULT '',
				created_at INTEGER NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_cron_due ON cron_jobs (enabled, next_run_at)`,
			`CREATE INDEX IF NOT EXISTS idx_cron_org ON cron_jobs (org_id)`,
			`CREATE INDEX IF NOT EXISTS idx_cron_sandbox ON cron_jobs (sandbox_id)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS cron_jobs (
				id TEXT PRIMARY KEY,
				org_id TEXT NOT NULL,
				sandbox_id TEXT NOT NULL,
				name TEXT NOT NULL DEFAULT '',
				schedule TEXT NOT NULL,
				timezone TEXT NOT NULL DEFAULT 'UTC',
				command TEXT NOT NULL,
				enabled SMALLINT NOT NULL DEFAULT 1,
				next_run_at BIGINT NOT NULL DEFAULT 0,
				last_run_at BIGINT NOT NULL DEFAULT 0,
				last_status TEXT NOT NULL DEFAULT '',
				last_output TEXT NOT NULL DEFAULT '',
				created_at BIGINT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_cron_due ON cron_jobs (enabled, next_run_at)`,
			`CREATE INDEX IF NOT EXISTS idx_cron_org ON cron_jobs (org_id)`,
			`CREATE INDEX IF NOT EXISTS idx_cron_sandbox ON cron_jobs (sandbox_id)`,
		},
	},
	{
		version: 18,
		name:    "chat_messages",
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS chat_messages (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				sandbox_id TEXT NOT NULL,
				org_id TEXT NOT NULL,
				role TEXT NOT NULL,
				text TEXT NOT NULL,
				at INTEGER NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_chat_sandbox_at ON chat_messages (sandbox_id, at)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS chat_messages (
				id BIGSERIAL PRIMARY KEY,
				sandbox_id TEXT NOT NULL,
				org_id TEXT NOT NULL,
				role TEXT NOT NULL,
				text TEXT NOT NULL,
				at BIGINT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_chat_sandbox_at ON chat_messages (sandbox_id, at)`,
		},
	},
	{
		version: 19,
		name:    "sandbox_integrations",
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS sandbox_integrations (
				sandbox_id TEXT PRIMARY KEY,
				org_id TEXT NOT NULL,
				servers TEXT NOT NULL DEFAULT '',
				updated_at INTEGER NOT NULL
			)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS sandbox_integrations (
				sandbox_id TEXT PRIMARY KEY,
				org_id TEXT NOT NULL,
				servers TEXT NOT NULL DEFAULT '',
				updated_at BIGINT NOT NULL
			)`,
		},
	},
	{
		version: 20,
		name:    "sandbox_config",
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS sandbox_config (
				sandbox_id TEXT PRIMARY KEY,
				org_id TEXT NOT NULL,
				env TEXT NOT NULL DEFAULT '{}',
				model TEXT NOT NULL DEFAULT '',
				base_url TEXT NOT NULL DEFAULT '',
				updated_at INTEGER NOT NULL
			)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS sandbox_config (
				sandbox_id TEXT PRIMARY KEY,
				org_id TEXT NOT NULL,
				env TEXT NOT NULL DEFAULT '{}',
				model TEXT NOT NULL DEFAULT '',
				base_url TEXT NOT NULL DEFAULT '',
				updated_at BIGINT NOT NULL
			)`,
		},
	},
	{
		version: 21,
		name:    "sandbox_name",
		sqlite: []string{
			`ALTER TABLE sandboxes ADD COLUMN name TEXT NOT NULL DEFAULT ''`,
		},
		postgres: []string{
			`ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS name TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		// prompt is user-authored and scrubbed of credential-shaped text before
		// it reaches here (see scrubSecrets).
		version: 22,
		name:    "build_requests",
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS build_requests (
				id         INTEGER PRIMARY KEY AUTOINCREMENT,
				org_id     TEXT NOT NULL DEFAULT 'default',
				owner_id   TEXT NOT NULL DEFAULT '',
				prompt     TEXT NOT NULL,
				outcome    TEXT NOT NULL,
				agent_id   TEXT NOT NULL DEFAULT '',
				sandbox_id TEXT NOT NULL DEFAULT '',
				detail     TEXT NOT NULL DEFAULT '',
				used_model INTEGER NOT NULL DEFAULT 0,
				at         INTEGER NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_build_req_at ON build_requests(at)`,
			`CREATE INDEX IF NOT EXISTS idx_build_req_outcome ON build_requests(outcome, at)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS build_requests (
				id         BIGSERIAL PRIMARY KEY,
				org_id     TEXT NOT NULL DEFAULT 'default',
				owner_id   TEXT NOT NULL DEFAULT '',
				prompt     TEXT NOT NULL,
				outcome    TEXT NOT NULL,
				agent_id   TEXT NOT NULL DEFAULT '',
				sandbox_id TEXT NOT NULL DEFAULT '',
				detail     TEXT NOT NULL DEFAULT '',
				used_model BOOLEAN NOT NULL DEFAULT FALSE,
				at         BIGINT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_build_req_at ON build_requests(at)`,
			`CREATE INDEX IF NOT EXISTS idx_build_req_outcome ON build_requests(outcome, at)`,
		},
	},
	{
		version: 23,
		name:    "projects_and_runs",
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS projects (
				id          TEXT PRIMARY KEY,
				org_id      TEXT NOT NULL DEFAULT 'default',
				owner       TEXT NOT NULL DEFAULT '',
				name        TEXT NOT NULL DEFAULT '',
				plan        TEXT NOT NULL DEFAULT '',
				created_at  INTEGER NOT NULL DEFAULT 0,
				last_run_at INTEGER NOT NULL DEFAULT 0,
				archived    INTEGER NOT NULL DEFAULT 0
			)`,
			`CREATE INDEX IF NOT EXISTS idx_projects_org ON projects(org_id, archived, last_run_at DESC)`,
			`CREATE TABLE IF NOT EXISTS project_runs (
				id            TEXT PRIMARY KEY,
				project_id    TEXT NOT NULL,
				org_id        TEXT NOT NULL DEFAULT 'default',
				plan_snapshot TEXT NOT NULL DEFAULT '',
				services      TEXT NOT NULL DEFAULT '[]',
				started_at    INTEGER NOT NULL DEFAULT 0,
				ended_at      INTEGER NOT NULL DEFAULT 0,
				cost_usd      REAL NOT NULL DEFAULT 0
			)`,
			`CREATE INDEX IF NOT EXISTS idx_runs_project ON project_runs(org_id, project_id, started_at DESC)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS projects (
				id          TEXT PRIMARY KEY,
				org_id      TEXT NOT NULL DEFAULT 'default',
				owner       TEXT NOT NULL DEFAULT '',
				name        TEXT NOT NULL DEFAULT '',
				plan        TEXT NOT NULL DEFAULT '',
				created_at  BIGINT NOT NULL DEFAULT 0,
				last_run_at BIGINT NOT NULL DEFAULT 0,
				archived    BOOLEAN NOT NULL DEFAULT FALSE
			)`,
			`CREATE INDEX IF NOT EXISTS idx_projects_org ON projects(org_id, archived, last_run_at DESC)`,
			`CREATE TABLE IF NOT EXISTS project_runs (
				id            TEXT PRIMARY KEY,
				project_id    TEXT NOT NULL,
				org_id        TEXT NOT NULL DEFAULT 'default',
				plan_snapshot TEXT NOT NULL DEFAULT '',
				services      TEXT NOT NULL DEFAULT '[]',
				started_at    BIGINT NOT NULL DEFAULT 0,
				ended_at      BIGINT NOT NULL DEFAULT 0,
				cost_usd      DOUBLE PRECISION NOT NULL DEFAULT 0
			)`,
			`CREATE INDEX IF NOT EXISTS idx_runs_project ON project_runs(org_id, project_id, started_at DESC)`,
		},
	},
	{
		version: 24,
		name:    "threads",
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS threads (
				id         TEXT PRIMARY KEY,
				org_id     TEXT NOT NULL DEFAULT 'default',
				owner      TEXT NOT NULL DEFAULT '',
				title      TEXT NOT NULL DEFAULT '',
				turns      TEXT NOT NULL DEFAULT '[]',
				created_at INTEGER NOT NULL DEFAULT 0,
				updated_at INTEGER NOT NULL DEFAULT 0
			)`,
			`CREATE INDEX IF NOT EXISTS idx_threads_org ON threads(org_id, updated_at DESC)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS threads (
				id         TEXT PRIMARY KEY,
				org_id     TEXT NOT NULL DEFAULT 'default',
				owner      TEXT NOT NULL DEFAULT '',
				title      TEXT NOT NULL DEFAULT '',
				turns      TEXT NOT NULL DEFAULT '[]',
				created_at BIGINT NOT NULL DEFAULT 0,
				updated_at BIGINT NOT NULL DEFAULT 0
			)`,
			`CREATE INDEX IF NOT EXISTS idx_threads_org ON threads(org_id, updated_at DESC)`,
		},
	},
	{
		version: 25,
		name:    "launch_timings",
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS launch_timings (
				id       INTEGER PRIMARY KEY AUTOINCREMENT,
				template TEXT NOT NULL,
				owner    TEXT NOT NULL DEFAULT '',
				ms       INTEGER NOT NULL DEFAULT 0,
				at       INTEGER NOT NULL DEFAULT 0
			)`,
			`CREATE INDEX IF NOT EXISTS idx_launch_timings ON launch_timings(template, at DESC)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS launch_timings (
				id       BIGSERIAL PRIMARY KEY,
				template TEXT NOT NULL,
				owner    TEXT NOT NULL DEFAULT '',
				ms       BIGINT NOT NULL DEFAULT 0,
				at       BIGINT NOT NULL DEFAULT 0
			)`,
			`CREATE INDEX IF NOT EXISTS idx_launch_timings ON launch_timings(template, at DESC)`,
		},
	},
	{
		// Re-adds the v12/v13 columns for databases whose 12 and 13 were
		// claimed by another branch.
		version: 26,
		name:    "repair_usage_columns",
		sqlite: []string{
			`ALTER TABLE usage_records ADD COLUMN storage_gb_seconds REAL NOT NULL DEFAULT 0`,
			`ALTER TABLE usage_records ADD COLUMN run_seconds REAL NOT NULL DEFAULT 0`,
		},
		postgres: []string{
			`ALTER TABLE usage_records ADD COLUMN IF NOT EXISTS storage_gb_seconds DOUBLE PRECISION NOT NULL DEFAULT 0`,
			`ALTER TABLE usage_records ADD COLUMN IF NOT EXISTS run_seconds DOUBLE PRECISION NOT NULL DEFAULT 0`,
		},
	},
	{
		version: 27,
		name:    "plan_catalogue",
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS plans (
				id               TEXT PRIMARY KEY,
				name             TEXT NOT NULL,
				price_monthly    REAL NOT NULL DEFAULT 0,
				allowance        REAL NOT NULL DEFAULT 0,
				max_concurrent   INTEGER NOT NULL DEFAULT 0,
				session_ttl_hrs  INTEGER NOT NULL DEFAULT 0,
				max_vcpu         INTEGER NOT NULL DEFAULT 0,
				max_mem_mb       INTEGER NOT NULL DEFAULT 0,
				max_storage_mb   INTEGER NOT NULL DEFAULT 0,
				self_subscribe   INTEGER NOT NULL DEFAULT 0,
				persistent       INTEGER NOT NULL DEFAULT 0,
				included_hours   INTEGER NOT NULL DEFAULT 0,
				max_workstations INTEGER NOT NULL DEFAULT 0,
				sort_order       INTEGER NOT NULL DEFAULT 0,
				updated_at       INTEGER NOT NULL DEFAULT 0
			)`,
			`CREATE TABLE IF NOT EXISTS pricing (
				id               TEXT PRIMARY KEY,
				vcpu_sec         REAL NOT NULL DEFAULT 0,
				gb_sec           REAL NOT NULL DEFAULT 0,
				storage_gb_month REAL NOT NULL DEFAULT 0,
				invoke           REAL NOT NULL DEFAULT 0,
				create_fee       REAL NOT NULL DEFAULT 0,
				free_trial       REAL NOT NULL DEFAULT 0,
				updated_at       INTEGER NOT NULL DEFAULT 0
			)`,
			`CREATE TABLE IF NOT EXISTS billing_cursor (
				id            TEXT PRIMARY KEY,
				last_usage_id INTEGER NOT NULL DEFAULT 0,
				updated_at    INTEGER NOT NULL DEFAULT 0
			)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS plans (
				id               TEXT PRIMARY KEY,
				name             TEXT NOT NULL,
				price_monthly    DOUBLE PRECISION NOT NULL DEFAULT 0,
				allowance        DOUBLE PRECISION NOT NULL DEFAULT 0,
				max_concurrent   BIGINT NOT NULL DEFAULT 0,
				session_ttl_hrs  BIGINT NOT NULL DEFAULT 0,
				max_vcpu         BIGINT NOT NULL DEFAULT 0,
				max_mem_mb       BIGINT NOT NULL DEFAULT 0,
				max_storage_mb   BIGINT NOT NULL DEFAULT 0,
				self_subscribe   SMALLINT NOT NULL DEFAULT 0,
				persistent       SMALLINT NOT NULL DEFAULT 0,
				included_hours   BIGINT NOT NULL DEFAULT 0,
				max_workstations BIGINT NOT NULL DEFAULT 0,
				sort_order       BIGINT NOT NULL DEFAULT 0,
				updated_at       BIGINT NOT NULL DEFAULT 0
			)`,
			`CREATE TABLE IF NOT EXISTS pricing (
				id               TEXT PRIMARY KEY,
				vcpu_sec         DOUBLE PRECISION NOT NULL DEFAULT 0,
				gb_sec           DOUBLE PRECISION NOT NULL DEFAULT 0,
				storage_gb_month DOUBLE PRECISION NOT NULL DEFAULT 0,
				invoke           DOUBLE PRECISION NOT NULL DEFAULT 0,
				create_fee       DOUBLE PRECISION NOT NULL DEFAULT 0,
				free_trial       DOUBLE PRECISION NOT NULL DEFAULT 0,
				updated_at       BIGINT NOT NULL DEFAULT 0
			)`,
			`CREATE TABLE IF NOT EXISTS billing_cursor (
				id            TEXT PRIMARY KEY,
				last_usage_id BIGINT NOT NULL DEFAULT 0,
				updated_at    BIGINT NOT NULL DEFAULT 0
			)`,
		},
	},
	{
		// A slot is claimed before the create, inside the counting transaction,
		// so concurrent creates cannot all pass the cap. Leaked rows age out.
		version: 28,
		name:    "admission_slots",
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS admission_slots (
				id         TEXT PRIMARY KEY,
				org_id     TEXT NOT NULL,
				created_at INTEGER NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_admission_org ON admission_slots(org_id, created_at)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS admission_slots (
				id         TEXT PRIMARY KEY,
				org_id     TEXT NOT NULL,
				created_at BIGINT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_admission_org ON admission_slots(org_id, created_at)`,
		},
	},
	{
		version: 29,
		name:    "credit_ledger",
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS credit_ledger (
				id            INTEGER PRIMARY KEY AUTOINCREMENT,
				org_id        TEXT NOT NULL DEFAULT 'default',
				at            INTEGER NOT NULL,
				kind          TEXT NOT NULL DEFAULT '',
				amount        REAL NOT NULL DEFAULT 0,
				balance_after REAL NOT NULL DEFAULT 0,
				reason        TEXT NOT NULL DEFAULT '',
				ref           TEXT NOT NULL DEFAULT ''
			)`,
			`CREATE INDEX IF NOT EXISTS idx_ledger_org_at ON credit_ledger(org_id, at DESC)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS credit_ledger (
				id            BIGSERIAL PRIMARY KEY,
				org_id        TEXT NOT NULL DEFAULT 'default',
				at            BIGINT NOT NULL,
				kind          TEXT NOT NULL DEFAULT '',
				amount        DOUBLE PRECISION NOT NULL DEFAULT 0,
				balance_after DOUBLE PRECISION NOT NULL DEFAULT 0,
				reason        TEXT NOT NULL DEFAULT '',
				ref           TEXT NOT NULL DEFAULT ''
			)`,
			`CREATE INDEX IF NOT EXISTS idx_ledger_org_at ON credit_ledger(org_id, at DESC)`,
		},
	},
	{
		// A token is bound to one sandbox with an explicit scope set.
		version: 30,
		name:    "agent_tokens",
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS agent_tokens (
				id            TEXT PRIMARY KEY,
				sandbox_id    TEXT NOT NULL,
				org_id        TEXT NOT NULL,
				secret_sha256 TEXT NOT NULL,
				scopes        TEXT NOT NULL DEFAULT '',
				label         TEXT NOT NULL DEFAULT '',
				created_by    TEXT NOT NULL DEFAULT '',
				created_at    INTEGER NOT NULL,
				expires_at    INTEGER NOT NULL,
				revoked_at    INTEGER,
				last_used_at  INTEGER,
				last_used_ip  TEXT NOT NULL DEFAULT '',
				supersedes    TEXT NOT NULL DEFAULT ''
			)`,
			// Unique: a duplicate hash would make revocation ambiguous.
			`CREATE UNIQUE INDEX IF NOT EXISTS agent_tokens_secret ON agent_tokens (secret_sha256)`,
			`CREATE INDEX IF NOT EXISTS agent_tokens_sandbox ON agent_tokens (sandbox_id)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS agent_tokens (
				id            TEXT PRIMARY KEY,
				sandbox_id    TEXT NOT NULL,
				org_id        TEXT NOT NULL,
				secret_sha256 TEXT NOT NULL,
				scopes        TEXT NOT NULL DEFAULT '',
				label         TEXT NOT NULL DEFAULT '',
				created_by    TEXT NOT NULL DEFAULT '',
				created_at    BIGINT NOT NULL,
				expires_at    BIGINT NOT NULL,
				revoked_at    BIGINT,
				last_used_at  BIGINT,
				last_used_ip  TEXT NOT NULL DEFAULT '',
				supersedes    TEXT NOT NULL DEFAULT ''
			)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS agent_tokens_secret ON agent_tokens (secret_sha256)`,
			`CREATE INDEX IF NOT EXISTS agent_tokens_sandbox ON agent_tokens (sandbox_id)`,
		},
	},
	{
		// Audit copy of the in-sandbox approval gate. Raw tool arguments are
		// never stored: args_digest identifies a call, args_preview is redacted.
		// timed_out is its own state so it is not confused with a human denial.
		version: 31,
		name:    "agent_approvals",
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS agent_approvals (
				id           TEXT PRIMARY KEY,
				sandbox_id   TEXT NOT NULL,
				org_id       TEXT NOT NULL,
				turn_id      TEXT NOT NULL DEFAULT '',
				tool         TEXT NOT NULL DEFAULT '',
				tool_use_id  TEXT NOT NULL DEFAULT '',
				signature    TEXT NOT NULL DEFAULT '',
				args_digest  TEXT NOT NULL DEFAULT '',
				args_preview TEXT NOT NULL DEFAULT '',
				requested_at INTEGER NOT NULL DEFAULT 0,
				state        TEXT NOT NULL DEFAULT 'pending',
				decided_by   TEXT NOT NULL DEFAULT '',
				decided_at   INTEGER NOT NULL DEFAULT 0,
				remember     TEXT NOT NULL DEFAULT '',
				reason       TEXT NOT NULL DEFAULT ''
			)`,
			`CREATE INDEX IF NOT EXISTS agent_approvals_sandbox ON agent_approvals (sandbox_id, requested_at)`,
			`CREATE INDEX IF NOT EXISTS agent_approvals_org ON agent_approvals (org_id, requested_at)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS agent_approvals (
				id           TEXT PRIMARY KEY,
				sandbox_id   TEXT NOT NULL,
				org_id       TEXT NOT NULL,
				turn_id      TEXT NOT NULL DEFAULT '',
				tool         TEXT NOT NULL DEFAULT '',
				tool_use_id  TEXT NOT NULL DEFAULT '',
				signature    TEXT NOT NULL DEFAULT '',
				args_digest  TEXT NOT NULL DEFAULT '',
				args_preview TEXT NOT NULL DEFAULT '',
				requested_at BIGINT NOT NULL DEFAULT 0,
				state        TEXT NOT NULL DEFAULT 'pending',
				decided_by   TEXT NOT NULL DEFAULT '',
				decided_at   BIGINT NOT NULL DEFAULT 0,
				remember     TEXT NOT NULL DEFAULT '',
				reason       TEXT NOT NULL DEFAULT ''
			)`,
			`CREATE INDEX IF NOT EXISTS agent_approvals_sandbox ON agent_approvals (sandbox_id, requested_at)`,
			`CREATE INDEX IF NOT EXISTS agent_approvals_org ON agent_approvals (org_id, requested_at)`,
		},
	},
	{
		// Durable mirror of the in-sandbox agent journal. (sandbox_id, seq) is
		// the primary key and every write is an upsert, so a cursor replay after
		// a restart is a no-op.
		version: 32,
		name:    "agent_mirror",
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS agent_turns (
				id           TEXT PRIMARY KEY,
				sandbox_id   TEXT NOT NULL,
				org_id       TEXT NOT NULL,
				thread_id    TEXT NOT NULL DEFAULT '',
				session_id   TEXT NOT NULL DEFAULT '',
				request      TEXT NOT NULL DEFAULT '',
				response     TEXT NOT NULL DEFAULT '',
				state        TEXT NOT NULL DEFAULT '',
				mode         TEXT NOT NULL DEFAULT '',
				source       TEXT NOT NULL DEFAULT '',
				exit_code    INTEGER,
				error        TEXT NOT NULL DEFAULT '',
				-- NULL, never 0, when the agent reported no cost.
				cost_usd     REAL,
				queued_at    INTEGER NOT NULL DEFAULT 0,
				started_at   INTEGER NOT NULL DEFAULT 0,
				ended_at     INTEGER NOT NULL DEFAULT 0,
				seq_start    INTEGER NOT NULL DEFAULT 0,
				seq_end      INTEGER NOT NULL DEFAULT 0
			)`,
			`CREATE INDEX IF NOT EXISTS agent_turns_sandbox ON agent_turns (sandbox_id, queued_at)`,
			`CREATE INDEX IF NOT EXISTS agent_turns_thread ON agent_turns (sandbox_id, session_id)`,
			`CREATE TABLE IF NOT EXISTS agent_events (
				sandbox_id TEXT NOT NULL,
				seq        INTEGER NOT NULL,
				org_id     TEXT NOT NULL,
				turn_id    TEXT NOT NULL DEFAULT '',
				type       TEXT NOT NULL,
				step       INTEGER NOT NULL DEFAULT 0,
				at         INTEGER NOT NULL DEFAULT 0,
				payload    TEXT NOT NULL DEFAULT '',
				PRIMARY KEY (sandbox_id, seq)
			)`,
			`CREATE INDEX IF NOT EXISTS agent_events_turn ON agent_events (sandbox_id, turn_id, seq)`,
			`CREATE INDEX IF NOT EXISTS agent_events_at ON agent_events (at)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS agent_turns (
				id           TEXT PRIMARY KEY,
				sandbox_id   TEXT NOT NULL,
				org_id       TEXT NOT NULL,
				thread_id    TEXT NOT NULL DEFAULT '',
				session_id   TEXT NOT NULL DEFAULT '',
				request      TEXT NOT NULL DEFAULT '',
				response     TEXT NOT NULL DEFAULT '',
				state        TEXT NOT NULL DEFAULT '',
				mode         TEXT NOT NULL DEFAULT '',
				source       TEXT NOT NULL DEFAULT '',
				exit_code    INTEGER,
				error        TEXT NOT NULL DEFAULT '',
				cost_usd     DOUBLE PRECISION,
				queued_at    BIGINT NOT NULL DEFAULT 0,
				started_at   BIGINT NOT NULL DEFAULT 0,
				ended_at     BIGINT NOT NULL DEFAULT 0,
				seq_start    BIGINT NOT NULL DEFAULT 0,
				seq_end      BIGINT NOT NULL DEFAULT 0
			)`,
			`CREATE INDEX IF NOT EXISTS agent_turns_sandbox ON agent_turns (sandbox_id, queued_at)`,
			`CREATE INDEX IF NOT EXISTS agent_turns_thread ON agent_turns (sandbox_id, session_id)`,
			`CREATE TABLE IF NOT EXISTS agent_events (
				sandbox_id TEXT NOT NULL,
				seq        BIGINT NOT NULL,
				org_id     TEXT NOT NULL,
				turn_id    TEXT NOT NULL DEFAULT '',
				type       TEXT NOT NULL,
				step       INTEGER NOT NULL DEFAULT 0,
				at         BIGINT NOT NULL DEFAULT 0,
				payload    TEXT NOT NULL DEFAULT '',
				PRIMARY KEY (sandbox_id, seq)
			)`,
			`CREATE INDEX IF NOT EXISTS agent_events_turn ON agent_events (sandbox_id, turn_id, seq)`,
			`CREATE INDEX IF NOT EXISTS agent_events_at ON agent_events (at)`,
		},
	},
	{
		// A delivery id is shared by every retry so receivers can dedupe; a row
		// left failed after the attempt budget is the DLQ entry. The signing
		// secret is encrypted, not hashed, because signing needs it back.
		version: 33,
		name:    "agent_webhooks",
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS agent_webhooks (
				id          TEXT PRIMARY KEY,
				sandbox_id  TEXT NOT NULL,
				org_id      TEXT NOT NULL,
				url         TEXT NOT NULL,
				secret_enc  TEXT NOT NULL DEFAULT '',
				events      TEXT NOT NULL DEFAULT '',
				enabled     INTEGER NOT NULL DEFAULT 1,
				consecutive_failures INTEGER NOT NULL DEFAULT 0,
				disabled_reason TEXT NOT NULL DEFAULT '',
				created_at  INTEGER NOT NULL DEFAULT 0,
				created_by  TEXT NOT NULL DEFAULT '',
				last_at     INTEGER NOT NULL DEFAULT 0,
				last_status INTEGER NOT NULL DEFAULT 0
			)`,
			`CREATE INDEX IF NOT EXISTS agent_webhooks_sandbox ON agent_webhooks (sandbox_id)`,
			`CREATE TABLE IF NOT EXISTS agent_webhook_deliveries (
				id          TEXT PRIMARY KEY,
				webhook_id  TEXT NOT NULL,
				sandbox_id  TEXT NOT NULL,
				org_id      TEXT NOT NULL,
				event_type  TEXT NOT NULL,
				event_seq   INTEGER NOT NULL DEFAULT 0,
				attempts    INTEGER NOT NULL DEFAULT 0,
				state       TEXT NOT NULL DEFAULT 'pending',
				last_status INTEGER NOT NULL DEFAULT 0,
				last_error  TEXT NOT NULL DEFAULT '',
				next_at     INTEGER NOT NULL DEFAULT 0,
				created_at  INTEGER NOT NULL DEFAULT 0,
				updated_at  INTEGER NOT NULL DEFAULT 0
			)`,
			`CREATE INDEX IF NOT EXISTS agent_webhook_deliveries_due ON agent_webhook_deliveries (state, next_at)`,
			`CREATE INDEX IF NOT EXISTS agent_webhook_deliveries_hook ON agent_webhook_deliveries (webhook_id, created_at)`,
			// One delivery per (hook, event), so delivery_id names the event, not the attempt.
			`CREATE UNIQUE INDEX IF NOT EXISTS agent_webhook_deliveries_event ON agent_webhook_deliveries (webhook_id, sandbox_id, event_seq, event_type)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS agent_webhooks (
				id          TEXT PRIMARY KEY,
				sandbox_id  TEXT NOT NULL,
				org_id      TEXT NOT NULL,
				url         TEXT NOT NULL,
				secret_enc  TEXT NOT NULL DEFAULT '',
				events      TEXT NOT NULL DEFAULT '',
				enabled     BOOLEAN NOT NULL DEFAULT TRUE,
				consecutive_failures BIGINT NOT NULL DEFAULT 0,
				disabled_reason TEXT NOT NULL DEFAULT '',
				created_at  BIGINT NOT NULL DEFAULT 0,
				created_by  TEXT NOT NULL DEFAULT '',
				last_at     BIGINT NOT NULL DEFAULT 0,
				last_status BIGINT NOT NULL DEFAULT 0
			)`,
			`CREATE INDEX IF NOT EXISTS agent_webhooks_sandbox ON agent_webhooks (sandbox_id)`,
			`CREATE TABLE IF NOT EXISTS agent_webhook_deliveries (
				id          TEXT PRIMARY KEY,
				webhook_id  TEXT NOT NULL,
				sandbox_id  TEXT NOT NULL,
				org_id      TEXT NOT NULL,
				event_type  TEXT NOT NULL,
				event_seq   BIGINT NOT NULL DEFAULT 0,
				attempts    BIGINT NOT NULL DEFAULT 0,
				state       TEXT NOT NULL DEFAULT 'pending',
				last_status BIGINT NOT NULL DEFAULT 0,
				last_error  TEXT NOT NULL DEFAULT '',
				next_at     BIGINT NOT NULL DEFAULT 0,
				created_at  BIGINT NOT NULL DEFAULT 0,
				updated_at  BIGINT NOT NULL DEFAULT 0
			)`,
			`CREATE INDEX IF NOT EXISTS agent_webhook_deliveries_due ON agent_webhook_deliveries (state, next_at)`,
			`CREATE INDEX IF NOT EXISTS agent_webhook_deliveries_hook ON agent_webhook_deliveries (webhook_id, created_at)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS agent_webhook_deliveries_event ON agent_webhook_deliveries (webhook_id, sandbox_id, event_seq, event_type)`,
		},
	},
	{
		version: 34,
		name:    "cron_agent_mode",
		// agent_mode empty means a shell command; "guided" or "autonomous" means
		// an agent prompt. Declared, never sniffed from the command text.
		sqlite: []string{
			`ALTER TABLE cron_jobs ADD COLUMN agent_mode TEXT NOT NULL DEFAULT ''`,
		},
		postgres: []string{
			`ALTER TABLE cron_jobs ADD COLUMN IF NOT EXISTS agent_mode TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		// Stored in the DB because any replica may serve the preview request.
		version: 35,
		name:    "sandbox_public_preview",
		sqlite: []string{
			`ALTER TABLE sandboxes ADD COLUMN public_preview INTEGER NOT NULL DEFAULT 0`,
		},
		postgres: []string{
			`ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS public_preview BOOLEAN NOT NULL DEFAULT FALSE`,
		},
	},
	{
		version: 36,
		name:    "drop_billing_tables",
		// No down migration: the data belongs to whatever system does the charging.
		sqlite: []string{
			`DROP TABLE IF EXISTS credit_ledger`,
			`DROP TABLE IF EXISTS plan_catalogue`,
			`DROP TABLE IF EXISTS org_trials`,
		},
		postgres: []string{
			`DROP TABLE IF EXISTS credit_ledger`,
			`DROP TABLE IF EXISTS plan_catalogue`,
			`DROP TABLE IF EXISTS org_trials`,
		},
	},
	{
		version: 37,
		name:    "vault_secrets",
		// value holds ciphertext from the store crypter when SANDBOXD_SECRET_KEY
		// is set; last4 is kept separately so listings never decrypt.
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS vault_secrets (
				org_id     TEXT NOT NULL,
				name       TEXT NOT NULL,
				value      TEXT NOT NULL,
				last4      TEXT NOT NULL DEFAULT '',
				created_at BIGINT NOT NULL,
				updated_at BIGINT NOT NULL,
				PRIMARY KEY (org_id, name)
			)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS vault_secrets (
				org_id     TEXT NOT NULL,
				name       TEXT NOT NULL,
				value      TEXT NOT NULL,
				last4      TEXT NOT NULL DEFAULT '',
				created_at BIGINT NOT NULL,
				updated_at BIGINT NOT NULL,
				PRIMARY KEY (org_id, name)
			)`,
		},
	},
	{
		version: 38,
		name:    "admin_users",
		// Operator console accounts, kept apart from tenant users. pass_hash
		// carries its own PBKDF2 parameters. email is the primary key so two
		// simultaneous first-run claims cannot both insert.
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS admin_users (
				email      TEXT PRIMARY KEY,
				pass_hash  TEXT NOT NULL,
				role       TEXT NOT NULL DEFAULT 'admin',
				created_at INTEGER NOT NULL,
				last_login INTEGER NOT NULL DEFAULT 0
			)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS admin_users (
				email      TEXT PRIMARY KEY,
				pass_hash  TEXT NOT NULL,
				role       TEXT NOT NULL DEFAULT 'admin',
				created_at BIGINT NOT NULL,
				last_login BIGINT NOT NULL DEFAULT 0
			)`,
		},
	},
	{
		version: 39,
		name:    "sandboxes_org_index",
		// (org_id, id) so the id-ordered listing comes back sorted from the index.
		sqlite: []string{
			`CREATE INDEX IF NOT EXISTS idx_sandboxes_org ON sandboxes(org_id, id)`,
		},
		postgres: []string{
			`CREATE INDEX IF NOT EXISTS idx_sandboxes_org ON sandboxes(org_id, id)`,
		},
	},
	{
		version: 40,
		name:    "sandboxes_node_ownership",
		// node_id '' means unowned. Unowned rows stay visible to every node and
		// are stamped on first save, so an upgrade does not orphan a running
		// fleet. See LoadSandboxesForNode.
		sqlite: []string{
			`ALTER TABLE sandboxes ADD COLUMN node_id TEXT NOT NULL DEFAULT ''`,
			`CREATE INDEX IF NOT EXISTS idx_sandboxes_node ON sandboxes(node_id)`,
		},
		postgres: []string{
			`ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS node_id TEXT NOT NULL DEFAULT ''`,
			`CREATE INDEX IF NOT EXISTS idx_sandboxes_node ON sandboxes(node_id)`,
		},
	},
	{
		version: 41,
		name:    "template_builds",
		// CHECKs make "ready without a hashed artifact" unrepresentable. No FK to
		// artifacts since SQLite ignores them without PRAGMA foreign_keys. Size,
		// sha256 and version columns are duplicated to avoid object-store reads.
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS template_builds (
				build_id            TEXT PRIMARY KEY,
				template_id         TEXT NOT NULL,
				org_id              TEXT NOT NULL DEFAULT 'default',
				status              TEXT NOT NULL DEFAULT 'building',
				rootfs_size         BIGINT NOT NULL DEFAULT 0,
				rootfs_sha256       TEXT NOT NULL DEFAULT '',
				total_size          BIGINT NOT NULL DEFAULT 0,
				kernel_version      TEXT NOT NULL DEFAULT '',
				firecracker_version TEXT NOT NULL DEFAULT '',
				envd_version        TEXT NOT NULL DEFAULT '',
				created_at          BIGINT NOT NULL,
				ready_at            BIGINT NOT NULL DEFAULT 0,
				CONSTRAINT template_builds_build_id_len
					CHECK (length(build_id) BETWEEN 1 AND 128),
				CONSTRAINT template_builds_status_known
					CHECK (status IN ('building', 'ready', 'failed')),
				CONSTRAINT template_builds_ready_is_published
					CHECK (status <> 'ready' OR (ready_at > 0 AND rootfs_size > 0 AND length(rootfs_sha256) = 64)),
				CONSTRAINT template_builds_unready_has_no_ready_at
					CHECK (ready_at = 0 OR status = 'ready')
			)`,
			`CREATE INDEX IF NOT EXISTS idx_template_builds_template ON template_builds(template_id, ready_at)`,
			`CREATE INDEX IF NOT EXISTS idx_template_builds_org ON template_builds(org_id, created_at)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS template_builds (
				build_id            TEXT PRIMARY KEY,
				template_id         TEXT NOT NULL,
				org_id              TEXT NOT NULL DEFAULT 'default',
				status              TEXT NOT NULL DEFAULT 'building',
				rootfs_size         BIGINT NOT NULL DEFAULT 0,
				rootfs_sha256       TEXT NOT NULL DEFAULT '',
				total_size          BIGINT NOT NULL DEFAULT 0,
				kernel_version      TEXT NOT NULL DEFAULT '',
				firecracker_version TEXT NOT NULL DEFAULT '',
				envd_version        TEXT NOT NULL DEFAULT '',
				created_at          BIGINT NOT NULL,
				ready_at            BIGINT NOT NULL DEFAULT 0,
				CONSTRAINT template_builds_build_id_len
					CHECK (length(build_id) BETWEEN 1 AND 128),
				CONSTRAINT template_builds_status_known
					CHECK (status IN ('building', 'ready', 'failed')),
				CONSTRAINT template_builds_ready_is_published
					CHECK (status <> 'ready' OR (ready_at > 0 AND rootfs_size > 0 AND length(rootfs_sha256) = 64)),
				CONSTRAINT template_builds_unready_has_no_ready_at
					CHECK (ready_at = 0 OR status = 'ready')
			)`,
			`CREATE INDEX IF NOT EXISTS idx_template_builds_template ON template_builds(template_id, ready_at)`,
			`CREATE INDEX IF NOT EXISTS idx_template_builds_org ON template_builds(org_id, created_at)`,
		},
	},
	{
		version: 42,
		name:    "artifacts",
		// One row per object in the store; "not published" is the absence of a
		// row, hence no DEFAULTs. key is stored, not derived, because GC and
		// index rebuild work key-first.
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS artifacts (
				build_id    TEXT NOT NULL,
				kind        TEXT NOT NULL,
				key         TEXT NOT NULL,
				size        BIGINT NOT NULL,
				sha256      TEXT NOT NULL,
				store_kind  TEXT NOT NULL,
				uploaded_at BIGINT NOT NULL,
				PRIMARY KEY (build_id, kind),
				CONSTRAINT artifacts_build_id_len
					CHECK (length(build_id) BETWEEN 1 AND 128),
				CONSTRAINT artifacts_key_is_derived
					CHECK (key = build_id || '/' || kind),
				CONSTRAINT artifacts_sha256_is_hex64
					CHECK (length(sha256) = 64),
				CONSTRAINT artifacts_size_nonnegative
					CHECK (size >= 0)
			)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS artifacts (
				build_id    TEXT NOT NULL,
				kind        TEXT NOT NULL,
				key         TEXT NOT NULL,
				size        BIGINT NOT NULL,
				sha256      TEXT NOT NULL,
				store_kind  TEXT NOT NULL,
				uploaded_at BIGINT NOT NULL,
				PRIMARY KEY (build_id, kind),
				CONSTRAINT artifacts_build_id_len
					CHECK (length(build_id) BETWEEN 1 AND 128),
				CONSTRAINT artifacts_key_is_derived
					CHECK (key = build_id || '/' || kind),
				CONSTRAINT artifacts_sha256_is_hex64
					CHECK (length(sha256) = 64),
				CONSTRAINT artifacts_size_nonnegative
					CHECK (size >= 0)
			)`,
		},
	},
	{
		version:          43,
		name:             "drop_money",
		objectsMayBeGone: true,
		// org_credits becomes org_plans and the money columns go, except
		// agent_turns.cost_usd, which the agent spend cap enforces against.
		// Create, copy, drop in that order and re-runnable: no transaction.
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS org_plans (
				org_id     TEXT PRIMARY KEY,
				plan_id    TEXT NOT NULL DEFAULT 'free',
				updated_at INTEGER NOT NULL
			)`,
			`INSERT OR IGNORE INTO org_plans (org_id, plan_id, updated_at)
			 SELECT org_id, plan_id, updated_at FROM org_credits`,
			`DROP TABLE IF EXISTS org_credits`,
			`ALTER TABLE sandbox_history DROP COLUMN cost_usd`,
			`ALTER TABLE project_runs DROP COLUMN cost_usd`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS org_plans (
				org_id     TEXT PRIMARY KEY,
				plan_id    TEXT NOT NULL DEFAULT 'free',
				updated_at BIGINT NOT NULL
			)`,
			`INSERT INTO org_plans (org_id, plan_id, updated_at)
			 SELECT org_id, plan_id, updated_at FROM org_credits
			 ON CONFLICT (org_id) DO NOTHING`,
			`DROP TABLE IF EXISTS org_credits`,
			`ALTER TABLE sandbox_history DROP COLUMN IF EXISTS cost_usd`,
			`ALTER TABLE project_runs DROP COLUMN IF EXISTS cost_usd`,
		},
	},
	{
		version: 44,
		name:    "usage_node_class",
		// node_class is operator-declared via NODE_LABELS=class=...; a class
		// rather than a node id so it outlives the node. Empty means unclassified.
		sqlite: []string{
			`ALTER TABLE usage_records ADD COLUMN node_class TEXT NOT NULL DEFAULT ''`,
		},
		postgres: []string{
			`ALTER TABLE usage_records ADD COLUMN IF NOT EXISTS node_class TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		version: 45,
		name:    "usage_totals",
		// Lifetime usage per (org, sandbox), upserted with each detail row, so
		// the summary read scans sandboxes instead of minutes. usage_records
		// stays for windowed reads and becomes prunable.
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS usage_totals (
				org_id             TEXT NOT NULL,
				sandbox_id         TEXT NOT NULL,
				vcpu_seconds       REAL NOT NULL DEFAULT 0,
				ram_gb_seconds     REAL NOT NULL DEFAULT 0,
				storage_gb_seconds REAL NOT NULL DEFAULT 0,
				run_seconds        REAL NOT NULL DEFAULT 0,
				updated_at         INTEGER NOT NULL DEFAULT 0,
				PRIMARY KEY (org_id, sandbox_id),
				CONSTRAINT usage_totals_nonnegative
					CHECK (vcpu_seconds >= 0 AND ram_gb_seconds >= 0
					       AND storage_gb_seconds >= 0 AND run_seconds >= 0),
				CONSTRAINT usage_totals_org_id_present CHECK (org_id <> '')
			)`,
			// Backfill so the rollup is correct for an existing meter.
			`INSERT OR IGNORE INTO usage_totals
				(org_id, sandbox_id, vcpu_seconds, ram_gb_seconds, storage_gb_seconds, run_seconds, updated_at)
			 SELECT org_id, sandbox_id,
			        COALESCE(SUM(vcpu_seconds),0), COALESCE(SUM(ram_gb_seconds),0),
			        COALESCE(SUM(storage_gb_seconds),0), COALESCE(SUM(run_seconds),0),
			        COALESCE(MAX(recorded_at),0)
			   FROM usage_records GROUP BY org_id, sandbox_id`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS usage_totals (
				org_id             TEXT NOT NULL,
				sandbox_id         TEXT NOT NULL,
				vcpu_seconds       DOUBLE PRECISION NOT NULL DEFAULT 0,
				ram_gb_seconds     DOUBLE PRECISION NOT NULL DEFAULT 0,
				storage_gb_seconds DOUBLE PRECISION NOT NULL DEFAULT 0,
				run_seconds        DOUBLE PRECISION NOT NULL DEFAULT 0,
				updated_at         BIGINT NOT NULL DEFAULT 0,
				PRIMARY KEY (org_id, sandbox_id),
				CONSTRAINT usage_totals_nonnegative
					CHECK (vcpu_seconds >= 0 AND ram_gb_seconds >= 0
					       AND storage_gb_seconds >= 0 AND run_seconds >= 0),
				CONSTRAINT usage_totals_org_id_present CHECK (org_id <> '')
			)`,
			`INSERT INTO usage_totals
				(org_id, sandbox_id, vcpu_seconds, ram_gb_seconds, storage_gb_seconds, run_seconds, updated_at)
			 SELECT org_id, sandbox_id,
			        COALESCE(SUM(vcpu_seconds),0), COALESCE(SUM(ram_gb_seconds),0),
			        COALESCE(SUM(storage_gb_seconds),0), COALESCE(SUM(run_seconds),0),
			        COALESCE(MAX(recorded_at),0)
			   FROM usage_records GROUP BY org_id, sandbox_id
			 ON CONFLICT (org_id, sandbox_id) DO NOTHING`,
		},
	},
	{
		version: 46,
		name:    "retention_indexes",
		// Every column a prune ranges over leads its own index; a composite
		// cannot serve a range on its second column. Postgres INCLUDEs the
		// meter columns for index-only scans.
		sqlite: []string{
			`DROP INDEX IF EXISTS idx_usage_org`,
			`CREATE INDEX IF NOT EXISTS idx_usage_org ON usage_records(org_id, recorded_at)`,
			`CREATE INDEX IF NOT EXISTS idx_usage_at ON usage_records(recorded_at)`,
			`CREATE INDEX IF NOT EXISTS idx_events_at ON sandbox_events(at)`,
			`CREATE INDEX IF NOT EXISTS idx_history_at ON sandbox_history(destroyed_at)`,
			`CREATE INDEX IF NOT EXISTS idx_idem_at ON idempotency(at)`,
			`CREATE INDEX IF NOT EXISTS idx_login_events_owner ON login_events(owner_id)`,
			`CREATE INDEX IF NOT EXISTS idx_audit_at ON audit_log(at)`,
			`CREATE INDEX IF NOT EXISTS idx_launch_timings_at ON launch_timings(at)`,
			`CREATE INDEX IF NOT EXISTS agent_webhook_deliveries_created ON agent_webhook_deliveries(created_at)`,
			`DROP INDEX IF EXISTS agent_webhook_deliveries_due`,
			`CREATE INDEX IF NOT EXISTS agent_webhook_deliveries_due
				ON agent_webhook_deliveries (next_at) WHERE state = 'pending'`,
		},
		postgres: []string{
			`DROP INDEX IF EXISTS idx_usage_org`,
			`CREATE INDEX IF NOT EXISTS idx_usage_org ON usage_records(org_id, recorded_at)
				INCLUDE (vcpu_seconds, ram_gb_seconds, storage_gb_seconds, run_seconds)`,
			`CREATE INDEX IF NOT EXISTS idx_usage_at ON usage_records(recorded_at)`,
			`CREATE INDEX IF NOT EXISTS idx_events_at ON sandbox_events(at)`,
			`CREATE INDEX IF NOT EXISTS idx_history_at ON sandbox_history(destroyed_at)`,
			`CREATE INDEX IF NOT EXISTS idx_idem_at ON idempotency(at)`,
			`CREATE INDEX IF NOT EXISTS idx_login_events_owner ON login_events(owner_id)`,
			`CREATE INDEX IF NOT EXISTS idx_audit_at ON audit_log(at)`,
			`CREATE INDEX IF NOT EXISTS idx_launch_timings_at ON launch_timings(at)`,
			`CREATE INDEX IF NOT EXISTS agent_webhook_deliveries_created ON agent_webhook_deliveries(created_at)`,
			`DROP INDEX IF EXISTS agent_webhook_deliveries_due`,
			`CREATE INDEX IF NOT EXISTS agent_webhook_deliveries_due
				ON agent_webhook_deliveries (next_at) WHERE state = 'pending'`,
		},
	},
	{
		version: 47,
		name:    "drop_dead_billing_tables",
		// v36 dropped migration names, not tables; these are the tables v27 created.
		sqlite: []string{
			`DROP TABLE IF EXISTS plans`,
			`DROP TABLE IF EXISTS pricing`,
			`DROP TABLE IF EXISTS billing_cursor`,
		},
		postgres: []string{
			`DROP TABLE IF EXISTS plans`,
			`DROP TABLE IF EXISTS pricing`,
			`DROP TABLE IF EXISTS billing_cursor`,
		},
	},
	{
		version:  48,
		name:     "value_constraints",
		sqlite:   checkStatements(dialectSQLite),
		postgres: checkStatements(dialectPostgres),
	},

	{
		version: 49,
		name:    "boolean_flags",
		// SMALLINT flags become BOOLEAN. The DO block checks the current type
		// first so a replay is a no-op. SQLite has no boolean type, so it
		// normalises the stored values to 0/1 instead.
		sqlite: []string{
			`UPDATE chat_links SET read_only = CASE WHEN read_only <> 0 THEN 1 ELSE 0 END,
			                       autonomous = CASE WHEN autonomous <> 0 THEN 1 ELSE 0 END`,
			`UPDATE chat_link_codes SET read_only = CASE WHEN read_only <> 0 THEN 1 ELSE 0 END,
			                            autonomous = CASE WHEN autonomous <> 0 THEN 1 ELSE 0 END`,
			`UPDATE cron_jobs SET enabled = CASE WHEN enabled <> 0 THEN 1 ELSE 0 END`,
		},
		postgres: []string{
			`DO $$
			 DECLARE r record;
			 BEGIN
			   FOR r IN SELECT * FROM (VALUES
			       ('chat_links','read_only'), ('chat_links','autonomous'),
			       ('chat_link_codes','read_only'), ('chat_link_codes','autonomous'),
			       ('cron_jobs','enabled')) AS t(tbl, col)
			   LOOP
			     IF EXISTS (SELECT 1 FROM information_schema.columns
			                 WHERE table_schema = current_schema() AND table_name = r.tbl
			                   AND column_name = r.col AND data_type <> 'boolean') THEN
			       EXECUTE format('ALTER TABLE %I ALTER COLUMN %I DROP DEFAULT', r.tbl, r.col);
			       EXECUTE format('ALTER TABLE %I ALTER COLUMN %I TYPE BOOLEAN USING %I <> 0', r.tbl, r.col, r.col);
			       EXECUTE format('ALTER TABLE %I ALTER COLUMN %I SET DEFAULT FALSE', r.tbl, r.col);
			     END IF;
			   END LOOP;
			 END $$`,
			// The loop resets every default to FALSE; cron_jobs.enabled defaults to TRUE.
			`ALTER TABLE cron_jobs ALTER COLUMN enabled SET DEFAULT TRUE`,
		},
	},
	{
		version: 50,
		name:    "egress_rules",
		// egress_rules: '' is no policy (unrestricted), '{}' is deny all, anything
		// else is a policy. '' is not valid JSON, so a reader that ignores the
		// distinction fails instead of silently permitting.
		sqlite: []string{
			`ALTER TABLE sandboxes ADD COLUMN egress_rules TEXT NOT NULL DEFAULT ''`,
			`CREATE TABLE IF NOT EXISTS org_egress (
				org_id     TEXT PRIMARY KEY,
				rules      TEXT NOT NULL,
				updated_at INTEGER NOT NULL
			)`,
		},
		postgres: []string{
			`ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS egress_rules TEXT NOT NULL DEFAULT ''`,
			`CREATE TABLE IF NOT EXISTS org_egress (
				org_id     TEXT PRIMARY KEY,
				rules      TEXT NOT NULL,
				updated_at BIGINT NOT NULL
			)`,
		},
	},
	{
		version: 51,
		name:    "sandbox_slug",
		// slug: '' is no slug. The partial unique index makes the store the one
		// arbiter of a name across every node; the row is deleted on destroy,
		// which frees it.
		sqlite: []string{
			`ALTER TABLE sandboxes ADD COLUMN slug TEXT NOT NULL DEFAULT ''`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_sandboxes_slug ON sandboxes(slug) WHERE slug <> ''`,
		},
		postgres: []string{
			`ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS slug TEXT NOT NULL DEFAULT ''`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_sandboxes_slug ON sandboxes(slug) WHERE slug <> ''`,
		},
	},
	{
		version: 52,
		name:    "vault_kind_auto_inject",
		// kind: 'secret' (sealed) or 'env' (readable). The five gateway settings
		// that were hard-coded readable become 'env' rows; the map is gone.
		sqlite: []string{
			`ALTER TABLE vault_secrets ADD COLUMN kind TEXT NOT NULL DEFAULT 'secret'`,
			`ALTER TABLE vault_secrets ADD COLUMN auto_inject INTEGER NOT NULL DEFAULT 0`,
			`UPDATE vault_secrets SET kind = 'env' WHERE name IN
				('ANTHROPIC_BASE_URL','ANTHROPIC_MODEL','OPENAI_BASE_URL','OPENAI_MODEL','GROK_BASE_URL')`,
		},
		postgres: []string{
			`ALTER TABLE vault_secrets ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'secret'`,
			`ALTER TABLE vault_secrets ADD COLUMN IF NOT EXISTS auto_inject INTEGER NOT NULL DEFAULT 0`,
			`UPDATE vault_secrets SET kind = 'env' WHERE name IN
				('ANTHROPIC_BASE_URL','ANTHROPIC_MODEL','OPENAI_BASE_URL','OPENAI_MODEL','GROK_BASE_URL')`,
		},
	},
	{
		version: 53,
		name:    "sandbox_identity",
		// identity_hash is the sha256 of the sandbox's own pd_sb_ token; '' is
		// "no identity" and never matches (SandboxByIdentityHash excludes it).
		// The row is the credential's whole lifetime: destroy deletes the row.
		sqlite: []string{
			`ALTER TABLE sandboxes ADD COLUMN identity_hash TEXT NOT NULL DEFAULT ''`,
			`CREATE INDEX IF NOT EXISTS idx_sandboxes_identity_hash ON sandboxes(identity_hash)`,
		},
		postgres: []string{
			`ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS identity_hash TEXT NOT NULL DEFAULT ''`,
			`CREATE INDEX IF NOT EXISTS idx_sandboxes_identity_hash ON sandboxes(identity_hash)`,
		},
	},
	{
		version: 54,
		name:    "sandbox_networks",
		// networks is a JSON array of tag names; '[]' is "joins no network",
		// which is the default and reaches no peer. Not a join table: the list
		// is read and written whole with the row, and a tag has no existence
		// apart from the sandboxes naming it.
		sqlite: []string{
			`ALTER TABLE sandboxes ADD COLUMN networks TEXT NOT NULL DEFAULT '[]'`,
		},
		postgres: []string{
			`ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS networks TEXT NOT NULL DEFAULT '[]'`,
		},
	},
	{
		version: 55,
		name:    "named_snapshots",
		// snapshots is a name registry over template_builds: one row per saved
		// version, pointing at the build that holds the bytes. Status, size and
		// readiness live on the build row and are never copied here. deleted_at
		// is 0 while the version is live; a superseded or removed version keeps
		// its row so a name's history stays readable after its objects are gone.
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS snapshots (
				id             TEXT PRIMARY KEY,
				org_id         TEXT NOT NULL DEFAULT 'default',
				name           TEXT NOT NULL,
				version        INTEGER NOT NULL,
				build_id       TEXT NOT NULL,
				runtime        TEXT NOT NULL DEFAULT '',
				template       TEXT NOT NULL DEFAULT '',
				vcpu           INTEGER NOT NULL DEFAULT 0,
				mem_mb         INTEGER NOT NULL DEFAULT 0,
				source_sandbox TEXT NOT NULL DEFAULT '',
				created_at     INTEGER NOT NULL,
				deleted_at     INTEGER NOT NULL DEFAULT 0
			)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_snapshots_version ON snapshots(org_id, name, version)`,
			`CREATE INDEX IF NOT EXISTS idx_snapshots_name ON snapshots(org_id, name)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS snapshots (
				id             TEXT PRIMARY KEY,
				org_id         TEXT NOT NULL DEFAULT 'default',
				name           TEXT NOT NULL,
				version        BIGINT NOT NULL,
				build_id       TEXT NOT NULL,
				runtime        TEXT NOT NULL DEFAULT '',
				template       TEXT NOT NULL DEFAULT '',
				vcpu           BIGINT NOT NULL DEFAULT 0,
				mem_mb         BIGINT NOT NULL DEFAULT 0,
				source_sandbox TEXT NOT NULL DEFAULT '',
				created_at     BIGINT NOT NULL,
				deleted_at     BIGINT NOT NULL DEFAULT 0
			)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_snapshots_version ON snapshots(org_id, name, version)`,
			`CREATE INDEX IF NOT EXISTS idx_snapshots_name ON snapshots(org_id, name)`,
		},
	},
	{
		version: 56,
		name:    "custom_domains",
		// One row per customer hostname. status is 'pending' until the verifier
		// sees the DNS record and 'active' once it has; routing reads only
		// 'active' rows. No FK to sandboxes: SQLite ignores one without PRAGMA
		// foreign_keys, so DeleteSandbox removes the rows itself on both
		// dialects. A wildcard row ('*.vms.example.com') has no sandbox_id.
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS custom_domains (
				host       TEXT PRIMARY KEY,
				org_id     TEXT NOT NULL DEFAULT 'default',
				sandbox_id TEXT NOT NULL DEFAULT '',
				port       INTEGER NOT NULL DEFAULT 8000,
				wildcard   INTEGER NOT NULL DEFAULT 0,
				status     TEXT NOT NULL DEFAULT 'pending',
				error      TEXT NOT NULL DEFAULT '',
				created_at INTEGER NOT NULL DEFAULT 0,
				checked_at INTEGER NOT NULL DEFAULT 0,
				CONSTRAINT custom_domains_status_known
					CHECK (status IN ('pending', 'active'))
			)`,
			`CREATE INDEX IF NOT EXISTS idx_custom_domains_org ON custom_domains(org_id)`,
			`CREATE INDEX IF NOT EXISTS idx_custom_domains_sandbox ON custom_domains(sandbox_id)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS custom_domains (
				host       TEXT PRIMARY KEY,
				org_id     TEXT NOT NULL DEFAULT 'default',
				sandbox_id TEXT NOT NULL DEFAULT '',
				port       BIGINT NOT NULL DEFAULT 8000,
				wildcard   BOOLEAN NOT NULL DEFAULT FALSE,
				status     TEXT NOT NULL DEFAULT 'pending',
				error      TEXT NOT NULL DEFAULT '',
				created_at BIGINT NOT NULL DEFAULT 0,
				checked_at BIGINT NOT NULL DEFAULT 0,
				CONSTRAINT custom_domains_status_known
					CHECK (status IN ('pending', 'active'))
			)`,
			`CREATE INDEX IF NOT EXISTS idx_custom_domains_org ON custom_domains(org_id)`,
			`CREATE INDEX IF NOT EXISTS idx_custom_domains_sandbox ON custom_domains(sandbox_id)`,
		},
	},
	{
		version: 57,
		name:    "org_preview_sources",
		// cidrs is a JSON array of CIDRs; no row means no restriction. Same
		// shape as org_egress (50): one row per org, read on the preview path.
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS org_preview_sources (
				org_id     TEXT PRIMARY KEY,
				cidrs      TEXT NOT NULL,
				updated_at INTEGER NOT NULL
			)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS org_preview_sources (
				org_id     TEXT PRIMARY KEY,
				cidrs      TEXT NOT NULL,
				updated_at BIGINT NOT NULL
			)`,
		},
	},
	{
		version: 58,
		name:    "sandbox_hibernate_timeout",
		// The second idle timer. idle_timeout now freezes the sandbox in RAM
		// (standby); hibernate_timeout writes it to disk. 0 means the platform
		// default (4 h) and a negative value means never, as for idle_timeout.
		//
		// 58 because main reached 54, then 55, then 57 while this branch was
		// open. TestNoVersionCollidesWithMain is the only thing that catches a
		// reused number, and a reused one is SKIPPED at deploy with a green
		// report — whoever merges after this renumbers, they do not merge and
		// hope.
		sqlite: []string{
			`ALTER TABLE sandboxes ADD COLUMN hibernate_timeout INTEGER NOT NULL DEFAULT 0`,
		},
		postgres: []string{
			`ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS hibernate_timeout BIGINT NOT NULL DEFAULT 0`,
		},
	},
	{
		version: 59,
		name:    "port_forwards",
		// One row per exposed guest port. public_port carries a UNIQUE index
		// because that index IS the allocator's lock: two replicas racing for
		// the same free port both INSERT, one loses, and it retries.
		//
		// No FOREIGN KEY: SQLite ignores ON DELETE CASCADE without PRAGMA
		// foreign_keys, so DeleteSandbox removes these rows itself (the same
		// reason custom_domains has none).
		//
		// 59 because main reached 58. Written as 60 while another open branch
		// held 59; renumbered on merge, as the note on 58 asks.
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS port_forwards (
				sandbox_id  TEXT NOT NULL,
				guest_port  INTEGER NOT NULL,
				public_port INTEGER NOT NULL,
				tcp         BOOLEAN NOT NULL DEFAULT 1,
				udp         BOOLEAN NOT NULL DEFAULT 0,
				created_at  INTEGER NOT NULL DEFAULT 0,
				PRIMARY KEY (sandbox_id, guest_port)
			)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_port_forwards_public ON port_forwards(public_port)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS port_forwards (
				sandbox_id  TEXT NOT NULL,
				guest_port  BIGINT NOT NULL,
				public_port BIGINT NOT NULL,
				tcp         BOOLEAN NOT NULL DEFAULT TRUE,
				udp         BOOLEAN NOT NULL DEFAULT FALSE,
				created_at  BIGINT NOT NULL DEFAULT 0,
				PRIMARY KEY (sandbox_id, guest_port)
			)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_port_forwards_public ON port_forwards(public_port)`,
		},
	},
	{
		version: 60,
		name:    "sandbox_visibility",
		// The first principal below the org: which key created the sandbox and
		// whether the rest of the org may see it. Both defaults reproduce the
		// behaviour before this migration — every sandbox visible to every key
		// in its org — so an existing row reads back as ('', 'org') and nothing
		// disappears from anyone's list.
		//
		// 60 because main reached 59; see the note on 58 before reusing a number.
		sqlite: []string{
			`ALTER TABLE sandboxes ADD COLUMN created_by TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE sandboxes ADD COLUMN visibility TEXT NOT NULL DEFAULT 'org'`,
		},
		postgres: []string{
			`ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS created_by TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS visibility TEXT NOT NULL DEFAULT 'org'`,
		},
	},
	{
		version: 61,
		name:    "vault_secret_scope",
		// Which sandboxes a secret may be injected into: 'all' (the default,
		// and what every existing row becomes), 'shared' (org-visible
		// sandboxes only) or 'private' (the creating key's private sandboxes
		// only). created_by is the key that stored it, empty for older rows,
		// which is why a 'private' scope has to be set deliberately before it
		// means anything.
		//
		// 61 because 60 is this change's sandbox columns; see the note on 58.
		sqlite: []string{
			`ALTER TABLE vault_secrets ADD COLUMN scope TEXT NOT NULL DEFAULT 'all'`,
			`ALTER TABLE vault_secrets ADD COLUMN created_by TEXT NOT NULL DEFAULT ''`,
		},
		postgres: []string{
			`ALTER TABLE vault_secrets ADD COLUMN IF NOT EXISTS scope TEXT NOT NULL DEFAULT 'all'`,
			`ALTER TABLE vault_secrets ADD COLUMN IF NOT EXISTS created_by TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		version: 62,
		name:    "sandbox_backups",
		// A sandbox's captures in object storage. sha256 and parent_id are here
		// from the first migration on purpose: a backup table without them
		// cannot dedupe retrospectively, because the digests of the rows
		// already written were never recorded and the bytes are gone.
		//
		// object_key is stored rather than derived: retention deletes the
		// object before the row, and a key formula that changes later would
		// leave every older object unreachable and unreclaimable.
		//
		// 62 because main reached 61; see the note on 58 before reusing a number.
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS backups (
				id          TEXT PRIMARY KEY,
				org_id      TEXT NOT NULL,
				sandbox_id  TEXT NOT NULL,
				node_id     TEXT NOT NULL DEFAULT '',
				status      TEXT NOT NULL DEFAULT 'pending',
				kind        TEXT NOT NULL DEFAULT '',
				object_key  TEXT NOT NULL DEFAULT '',
				bytes       INTEGER NOT NULL DEFAULT 0,
				sha256      TEXT NOT NULL DEFAULT '',
				parent_id   TEXT NOT NULL DEFAULT '',
				error       TEXT NOT NULL DEFAULT '',
				created_at  INTEGER NOT NULL DEFAULT 0,
				ready_at    INTEGER NOT NULL DEFAULT 0,
				expires_at  INTEGER NOT NULL DEFAULT 0
			)`,
			`CREATE INDEX IF NOT EXISTS idx_backups_sandbox ON backups(sandbox_id, created_at)`,
			`CREATE INDEX IF NOT EXISTS idx_backups_expiry ON backups(expires_at)`,
			`CREATE INDEX IF NOT EXISTS idx_backups_parent ON backups(parent_id)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS backups (
				id          TEXT PRIMARY KEY,
				org_id      TEXT NOT NULL,
				sandbox_id  TEXT NOT NULL,
				node_id     TEXT NOT NULL DEFAULT '',
				status      TEXT NOT NULL DEFAULT 'pending',
				kind        TEXT NOT NULL DEFAULT '',
				object_key  TEXT NOT NULL DEFAULT '',
				bytes       BIGINT NOT NULL DEFAULT 0,
				sha256      TEXT NOT NULL DEFAULT '',
				parent_id   TEXT NOT NULL DEFAULT '',
				error       TEXT NOT NULL DEFAULT '',
				created_at  BIGINT NOT NULL DEFAULT 0,
				ready_at    BIGINT NOT NULL DEFAULT 0,
				expires_at  BIGINT NOT NULL DEFAULT 0
			)`,
			`CREATE INDEX IF NOT EXISTS idx_backups_sandbox ON backups(sandbox_id, created_at)`,
			`CREATE INDEX IF NOT EXISTS idx_backups_expiry ON backups(expires_at)`,
			`CREATE INDEX IF NOT EXISTS idx_backups_parent ON backups(parent_id)`,
		},
	},
	{
		version: 63,
		name:    "cron_job_kind",
		// What a due job does. 'shell' is every existing row, which is what it
		// already was; 'backup' is dispatched to the daemon's capture instead
		// of to a shell, so the sweeper never has to guess from the command.
		//
		// 63 because 62 is this change's backups table; see the note on 58.
		sqlite: []string{
			`ALTER TABLE cron_jobs ADD COLUMN kind TEXT NOT NULL DEFAULT 'shell'`,
		},
		postgres: []string{
			`ALTER TABLE cron_jobs ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'shell'`,
		},
	},
	{
		version: 64,
		name:    "sandbox_state_standby",
		// add-sandbox-standby introduced a tier below running — a sandbox frozen
		// in RAM — but migration 48's value list is frozen content and predates
		// it, so every standby write failed sandboxes_state_known and the daemon
		// could not persist a sandbox it had just frozen. The state machine and
		// the constraint have to agree; this is the constraint catching up.
		//
		// Recreated rather than edited in place: 48's list is deliberately
		// immutable, and both backends can only replace, not amend.
		//
		// The CREATEs are deliberately unguarded. IF NOT EXISTS could only be
		// true if the DROP above had silently done nothing, and then this
		// migration would leave 48's stale nine-state trigger in place and
		// report success — the exact failure it exists to fix.
		sqlite: []string{
			`DROP TRIGGER IF EXISTS sandboxes_state_known_ins`,
			`DROP TRIGGER IF EXISTS sandboxes_state_known_upd`,
			`CREATE TRIGGER sandboxes_state_known_ins BEFORE INSERT ON sandboxes
FOR EACH ROW WHEN NOT (NEW.state IN ('pending','running','standby','pausing','paused','resuming','stopped','failed','destroyed','error'))
BEGIN SELECT RAISE(ABORT, 'sandboxes_state_known'); END`,
			`CREATE TRIGGER sandboxes_state_known_upd BEFORE UPDATE ON sandboxes
FOR EACH ROW WHEN NOT (NEW.state IN ('pending','running','standby','pausing','paused','resuming','stopped','failed','destroyed','error'))
BEGIN SELECT RAISE(ABORT, 'sandboxes_state_known'); END`,
		},
		postgres: []string{
			`ALTER TABLE sandboxes DROP CONSTRAINT IF EXISTS sandboxes_state_known`,
			`ALTER TABLE sandboxes ADD CONSTRAINT sandboxes_state_known CHECK (state IN ('pending','running','standby','pausing','paused','resuming','stopped','failed','destroyed','error')) NOT VALID`,
		},
	},
	{
		// PLACEHOLDER VERSION — CLAIM A NUMBER AT MERGE. 65 is one above the
		// highest on this branch and other tracks are open; see the note on 58.
		// A reused number is SKIPPED at deploy with a green report, and the
		// symptom would be exactly the bug this fixes: retention silently
		// reverting to BACKUPS_RETAIN_DAYS.
		version: 65,
		name:    "sandbox_backup_retention",
		// Per-sandbox backup retention in seconds; 0 means the deployment
		// default (BACKUPS_RETAIN_DAYS). On the sandbox rather than on the
		// backup cron job: the capture path reads it per sandbox, an on-demand
		// backup has no job at all, and the sandbox row is already reloaded at
		// restore — which is what makes the number survive a node restart.
		sqlite: []string{
			`ALTER TABLE sandboxes ADD COLUMN backup_retention INTEGER NOT NULL DEFAULT 0`,
		},
		postgres: []string{
			`ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS backup_retention BIGINT NOT NULL DEFAULT 0`,
		},
	},
	{
		// PLACEHOLDER VERSION — CLAIM A NUMBER AT MERGE. 66 is one above the
		// highest on this branch and other tracks are open; see the note on 58.
		// A reused number is SKIPPED at deploy with a green report, and the
		// symptom would be exactly the bug this fixes: placement silently
		// ignoring CPU compatibility, so a snapshot lands on a node that kills
		// the guest with SIGILL.
		version: 66,
		name:    "snapshot_cpu_features",
		// The dispatch-relevant instruction set of the host that captured this
		// snapshot, comma-separated and sorted, empty for a capture taken before
		// it was recorded. A frozen guest has already chosen its code paths from
		// CPUID, so a restore on a poorer CPU dies with SIGILL; placement reads
		// this to pick a node that can actually run it.
		//
		// Text rather than a join table: it is read whole on the create path,
		// never queried by element, and a fleet's dispatch set is a couple of
		// hundred bytes.
		sqlite: []string{
			`ALTER TABLE snapshots ADD COLUMN cpu_features TEXT NOT NULL DEFAULT ''`,
		},
		postgres: []string{
			`ALTER TABLE snapshots ADD COLUMN IF NOT EXISTS cpu_features TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		// PLACEHOLDER VERSION — CLAIM A NUMBER AT MERGE. 67 is one above the
		// highest on this branch and other tracks are open; see the note on 58.
		// A reused number is SKIPPED at deploy with a green report, and the
		// symptom would be `history <slug>` still answering 404 on a fleet
		// whose migration report said everything applied.
		version: 67,
		name:    "history_slug",
		// The slug the sandbox answered to, so `history <slug>` works the way
		// the help says a slug works everywhere an id does. It cannot be
		// resolved through the live sandboxes table: a history record exists
		// exactly because that row is gone. Denormalised at record time from
		// the row that is about to be deleted; empty for anything destroyed
		// before this column existed.
		sqlite: []string{
			`ALTER TABLE sandbox_history ADD COLUMN slug TEXT NOT NULL DEFAULT ''`,
			`CREATE INDEX IF NOT EXISTS idx_history_slug ON sandbox_history(slug)`,
		},
		postgres: []string{
			`ALTER TABLE sandbox_history ADD COLUMN IF NOT EXISTS slug TEXT NOT NULL DEFAULT ''`,
			`CREATE INDEX IF NOT EXISTS idx_history_slug ON sandbox_history(slug)`,
		},
	},
	{
		// PLACEHOLDER VERSION — CLAIM A NUMBER AT MERGE, as the note on 58 says.
		version: 68,
		name:    "sandbox_state_lost",
		// "lost" is what the control plane marks a reaped node's sandboxes, so
		// they stop reading as running forever and stop holding their org's
		// concurrency. Recreated like 64, for the same reasons.
		sqlite: []string{
			`DROP TRIGGER IF EXISTS sandboxes_state_known_ins`,
			`DROP TRIGGER IF EXISTS sandboxes_state_known_upd`,
			`CREATE TRIGGER sandboxes_state_known_ins BEFORE INSERT ON sandboxes
FOR EACH ROW WHEN NOT (NEW.state IN ('pending','running','standby','pausing','paused','resuming','stopped','failed','destroyed','error','lost'))
BEGIN SELECT RAISE(ABORT, 'sandboxes_state_known'); END`,
			`CREATE TRIGGER sandboxes_state_known_upd BEFORE UPDATE ON sandboxes
FOR EACH ROW WHEN NOT (NEW.state IN ('pending','running','standby','pausing','paused','resuming','stopped','failed','destroyed','error','lost'))
BEGIN SELECT RAISE(ABORT, 'sandboxes_state_known'); END`,
		},
		postgres: []string{
			`ALTER TABLE sandboxes DROP CONSTRAINT IF EXISTS sandboxes_state_known`,
			`ALTER TABLE sandboxes ADD CONSTRAINT sandboxes_state_known CHECK (state IN ('pending','running','standby','pausing','paused','resuming','stopped','failed','destroyed','error','lost')) NOT VALID`,
		},
	},
	{
		// PLACEHOLDER VERSION — CLAIM A NUMBER AT MERGE, as the note on 58 says.
		version: 69,
		name:    "sandbox_pause_archive",
		// Which archived pause a paused row's state is. It is what lets a
		// resume run without the bucket: rather than deleting the archive before
		// the sandbox runs again (and refusing when the bucket is down), a
		// rehome accepts an archive only when this id matches.
		sqlite: []string{
			`ALTER TABLE sandboxes ADD COLUMN pause_archive TEXT NOT NULL DEFAULT ''`,
		},
		postgres: []string{
			`ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS pause_archive TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		// PLACEHOLDER VERSION — CLAIM A NUMBER AT MERGE, as the note on 58 says.
		version: 70,
		name:    "node_flags",
		// Cordon and drain for a control plane with no Redis, whose node table is
		// in memory. A node with neither flag has no row.
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS node_flags (
				node_id  TEXT PRIMARY KEY,
				cordoned INTEGER NOT NULL DEFAULT 0,
				draining INTEGER NOT NULL DEFAULT 0
			)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS node_flags (
				node_id  TEXT PRIMARY KEY,
				cordoned BOOLEAN NOT NULL DEFAULT FALSE,
				draining BOOLEAN NOT NULL DEFAULT FALSE
			)`,
		},
	},
	{
		// PLACEHOLDER VERSION — CLAIM A NUMBER AT MERGE, as the note on 58 says.
		version: 71,
		name:    "removed_nodes",
		// A node's credentials are derived from NODE_KEY_SECRET (nodeKeyFor,
		// nodeHeartbeatToken), not stored, so there is nothing to delete that
		// revokes them — the derivation still works forever. This table is what
		// actually revokes a removed node: every credential check also refuses
		// an id found here, and a re-join picks a fresh id rather than reusing
		// (and un-revoking) one that is.
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS removed_nodes (
				node_id    TEXT PRIMARY KEY,
				zone       TEXT NOT NULL DEFAULT '',
				removed_at INTEGER NOT NULL
			)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS removed_nodes (
				node_id    TEXT PRIMARY KEY,
				zone       TEXT NOT NULL DEFAULT '',
				removed_at BIGINT NOT NULL
			)`,
		},
	},
	{
		// PLACEHOLDER VERSION — CLAIM A NUMBER AT MERGE, as the note on 58 says.
		version: 72,
		name:    "volumes",
		// A disk that outlives the sandbox it is attached to. It lives on one
		// node, and attached_to is "" while no sandbox holds it.
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS volumes (
				id          TEXT PRIMARY KEY,
				org_id      TEXT NOT NULL,
				name        TEXT NOT NULL DEFAULT '',
				size_mb     INTEGER NOT NULL,
				node_id     TEXT NOT NULL DEFAULT '',
				attached_to TEXT NOT NULL DEFAULT '',
				created_at  INTEGER NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS volumes_org ON volumes (org_id)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS volumes (
				id          TEXT PRIMARY KEY,
				org_id      TEXT NOT NULL,
				name        TEXT NOT NULL DEFAULT '',
				size_mb     INTEGER NOT NULL,
				node_id     TEXT NOT NULL DEFAULT '',
				attached_to TEXT NOT NULL DEFAULT '',
				created_at  BIGINT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS volumes_org ON volumes (org_id)`,
		},
	},
	{
		// PLACEHOLDER VERSION — CLAIM A NUMBER AT MERGE, as the note on 58 says.
		version: 73,
		name:    "legacy_domain_uses",
		// When each legacy sandbox domain last redirected anyone. Its preview
		// hosts keep redirecting until store.LegacyUseWindow passes with no use.
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS legacy_domain_uses (
				domain       TEXT PRIMARY KEY,
				last_used_at INTEGER NOT NULL
			)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS legacy_domain_uses (
				domain       TEXT PRIMARY KEY,
				last_used_at BIGINT NOT NULL
			)`,
		},
	},
	{
		// PLACEHOLDER VERSION — CLAIM A NUMBER AT MERGE, as the note on 58 says.
		version: 74,
		name:    "custom_domain_legacy_target",
		// Nonzero while the row's DNS still names a legacy sandbox domain: when
		// it last served traffic that way. The verifier stops accepting the old
		// target store.LegacyUseWindow after it.
		sqlite: []string{
			`ALTER TABLE custom_domains ADD COLUMN legacy_target_seen_at INTEGER NOT NULL DEFAULT 0`,
		},
		postgres: []string{
			`ALTER TABLE custom_domains ADD COLUMN IF NOT EXISTS legacy_target_seen_at BIGINT NOT NULL DEFAULT 0`,
		},
	},
	{
		// PLACEHOLDER VERSION — CLAIM A NUMBER AT MERGE, as the note on 58 says.
		version: 75,
		name:    "custom_domain_txt",
		// A host that matches only by address also needs a TXT record holding
		// txt_token. txt_required_at is when it becomes required; rows from
		// before this get cron.TXTGrace from the verifier's first ask.
		sqlite: []string{
			`ALTER TABLE custom_domains ADD COLUMN txt_token TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE custom_domains ADD COLUMN txt_required_at INTEGER NOT NULL DEFAULT 0`,
		},
		postgres: []string{
			`ALTER TABLE custom_domains ADD COLUMN IF NOT EXISTS txt_token TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE custom_domains ADD COLUMN IF NOT EXISTS txt_required_at BIGINT NOT NULL DEFAULT 0`,
		},
	},
	{
		// PLACEHOLDER VERSION — CLAIM A NUMBER AT MERGE, as the note on 58 says.
		version: 76,
		name:    "shares",
		// One row per public link to a developer's local app. Rows stay after
		// close so a name is never reassigned and a takedown has a record.
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS shares (
				id           TEXT PRIMARY KEY,
				name         TEXT NOT NULL UNIQUE,
				org_id       TEXT NOT NULL,
				created_by   TEXT NOT NULL DEFAULT '',
				created_at   INTEGER NOT NULL,
				expires_at   INTEGER NOT NULL,
				last_seen_at INTEGER NOT NULL DEFAULT 0,
				closed_at    INTEGER NOT NULL DEFAULT 0,
				blocked_at   INTEGER NOT NULL DEFAULT 0,
				bytes_in     INTEGER NOT NULL DEFAULT 0,
				bytes_out    INTEGER NOT NULL DEFAULT 0
			)`,
			`CREATE INDEX IF NOT EXISTS shares_org ON shares (org_id)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS shares (
				id           TEXT PRIMARY KEY,
				name         TEXT NOT NULL UNIQUE,
				org_id       TEXT NOT NULL,
				created_by   TEXT NOT NULL DEFAULT '',
				created_at   BIGINT NOT NULL,
				expires_at   BIGINT NOT NULL,
				last_seen_at BIGINT NOT NULL DEFAULT 0,
				closed_at    BIGINT NOT NULL DEFAULT 0,
				blocked_at   BIGINT NOT NULL DEFAULT 0,
				bytes_in     BIGINT NOT NULL DEFAULT 0,
				bytes_out    BIGINT NOT NULL DEFAULT 0
			)`,
			`CREATE INDEX IF NOT EXISTS shares_org ON shares (org_id)`,
		},
	},
	{
		version: 77,
		name:    "sandbox_image",
		// What a caller-supplied image booted with, so a resume, restore or fork
		// boots the same thing. Empty on rows created before this.
		sqlite: []string{
			`ALTER TABLE sandboxes ADD COLUMN image TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE sandboxes ADD COLUMN ports TEXT NOT NULL DEFAULT ''`,
		},
		postgres: []string{
			`ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS image TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS ports TEXT NOT NULL DEFAULT ''`,
		},
	},
}
