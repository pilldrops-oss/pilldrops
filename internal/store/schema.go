package store

// sqliteSchema is the version 1 DDL for SQLite.
const sqliteSchema = `
CREATE TABLE IF NOT EXISTS api_keys (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL DEFAULT '',
	key_hash    TEXT NOT NULL UNIQUE,
	org_id      TEXT NOT NULL DEFAULT 'default',
	role        TEXT NOT NULL DEFAULT 'user',
	created_at  INTEGER NOT NULL,
	last_used   INTEGER,
	revoked_at  INTEGER
);
CREATE TABLE IF NOT EXISTS sandboxes (
	id          TEXT PRIMARY KEY,
	template    TEXT NOT NULL,
	name        TEXT NOT NULL DEFAULT '',
	org_id      TEXT NOT NULL DEFAULT 'default',
	state       TEXT NOT NULL,
	snapshot    TEXT NOT NULL DEFAULT '',
	start_cmd   TEXT NOT NULL DEFAULT '',
	public_port INTEGER NOT NULL DEFAULT 0,
	vcpu        INTEGER NOT NULL DEFAULT 0,
	mem_mib     INTEGER NOT NULL DEFAULT 0,
	storage_mb  INTEGER NOT NULL DEFAULT 0,
	env         TEXT NOT NULL DEFAULT '{}',
	created_at  INTEGER NOT NULL,
	expires_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS usage_records (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	sandbox_id    TEXT NOT NULL,
	org_id        TEXT NOT NULL DEFAULT 'default',
	vcpu_seconds  REAL NOT NULL DEFAULT 0,
	ram_gb_seconds REAL NOT NULL DEFAULT 0,
	recorded_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_usage_org ON usage_records(org_id);
CREATE TABLE IF NOT EXISTS functions (
	name        TEXT NOT NULL,
	org_id      TEXT NOT NULL DEFAULT 'default',
	template    TEXT NOT NULL,
	start_cmd   TEXT NOT NULL DEFAULT '',
	port        INTEGER NOT NULL DEFAULT 8000,
	vcpu        INTEGER NOT NULL DEFAULT 0,
	mem_mib     INTEGER NOT NULL DEFAULT 0,
	max_replicas INTEGER NOT NULL DEFAULT 0,
	sandbox_id  TEXT NOT NULL DEFAULT '',
	invokes     INTEGER NOT NULL DEFAULT 0,
	last_invoked_at INTEGER NOT NULL DEFAULT 0,
	env         TEXT NOT NULL DEFAULT '{}',
	cold_starts INTEGER NOT NULL DEFAULT 0,
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL,
	PRIMARY KEY (org_id, name)
);
CREATE TABLE IF NOT EXISTS org_credits (
	org_id     TEXT PRIMARY KEY,
	balance    REAL NOT NULL,
	granted    REAL NOT NULL,
	plan_id    TEXT NOT NULL DEFAULT 'free',
	renews_at  INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS audit_log (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	org_id  TEXT NOT NULL DEFAULT 'default',
	key_id  TEXT NOT NULL DEFAULT '',
	action  TEXT NOT NULL,
	target  TEXT NOT NULL DEFAULT '',
	at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_org_at ON audit_log(org_id, at);
CREATE INDEX IF NOT EXISTS idx_keys_org ON api_keys(org_id);
CREATE TABLE IF NOT EXISTS idempotency (
	org_id   TEXT NOT NULL DEFAULT 'default',
	idem_key TEXT NOT NULL,
	response TEXT NOT NULL,
	at       INTEGER NOT NULL,
	PRIMARY KEY (org_id, idem_key)
);
`

// sqliteRetrofits add columns to SQLite databases that predate them.
var sqliteRetrofits = []string{
	`ALTER TABLE api_keys ADD COLUMN role TEXT NOT NULL DEFAULT 'user'`,
	`ALTER TABLE sandboxes ADD COLUMN start_cmd TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE sandboxes ADD COLUMN public_port INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE sandboxes ADD COLUMN vcpu INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE sandboxes ADD COLUMN mem_mib INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE sandboxes ADD COLUMN storage_mb INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE sandboxes ADD COLUMN env TEXT NOT NULL DEFAULT '{}'`,
	`ALTER TABLE functions ADD COLUMN env TEXT NOT NULL DEFAULT '{}'`,
	`ALTER TABLE functions ADD COLUMN cold_starts INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE functions ADD COLUMN vcpu INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE functions ADD COLUMN mem_mib INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE functions ADD COLUMN max_replicas INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE org_credits ADD COLUMN plan_id TEXT NOT NULL DEFAULT 'free'`,
	`ALTER TABLE org_credits ADD COLUMN renews_at INTEGER NOT NULL DEFAULT 0`,
}

// postgresSchema is the version 1 DDL for Postgres.
const postgresSchema = `
CREATE TABLE IF NOT EXISTS api_keys (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL DEFAULT '',
	key_hash    TEXT NOT NULL UNIQUE,
	org_id      TEXT NOT NULL DEFAULT 'default',
	role        TEXT NOT NULL DEFAULT 'user',
	created_at  BIGINT NOT NULL,
	last_used   BIGINT,
	revoked_at  BIGINT
);
CREATE TABLE IF NOT EXISTS sandboxes (
	id          TEXT PRIMARY KEY,
	template    TEXT NOT NULL,
	name        TEXT NOT NULL DEFAULT '',
	org_id      TEXT NOT NULL DEFAULT 'default',
	state       TEXT NOT NULL,
	snapshot    TEXT NOT NULL DEFAULT '',
	start_cmd   TEXT NOT NULL DEFAULT '',
	public_port BIGINT NOT NULL DEFAULT 0,
	vcpu        BIGINT NOT NULL DEFAULT 0,
	mem_mib     BIGINT NOT NULL DEFAULT 0,
	storage_mb  BIGINT NOT NULL DEFAULT 0,
	env         TEXT NOT NULL DEFAULT '{}',
	created_at  BIGINT NOT NULL,
	expires_at  BIGINT NOT NULL,
	updated_at  BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS usage_records (
	id            BIGSERIAL PRIMARY KEY,
	sandbox_id    TEXT NOT NULL,
	org_id        TEXT NOT NULL DEFAULT 'default',
	vcpu_seconds  DOUBLE PRECISION NOT NULL DEFAULT 0,
	ram_gb_seconds DOUBLE PRECISION NOT NULL DEFAULT 0,
	recorded_at   BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_usage_org ON usage_records(org_id);
CREATE TABLE IF NOT EXISTS functions (
	name        TEXT NOT NULL,
	org_id      TEXT NOT NULL DEFAULT 'default',
	template    TEXT NOT NULL,
	start_cmd   TEXT NOT NULL DEFAULT '',
	port        BIGINT NOT NULL DEFAULT 8000,
	vcpu        BIGINT NOT NULL DEFAULT 0,
	mem_mib     BIGINT NOT NULL DEFAULT 0,
	max_replicas BIGINT NOT NULL DEFAULT 0,
	sandbox_id  TEXT NOT NULL DEFAULT '',
	invokes     BIGINT NOT NULL DEFAULT 0,
	last_invoked_at BIGINT NOT NULL DEFAULT 0,
	env         TEXT NOT NULL DEFAULT '{}',
	cold_starts BIGINT NOT NULL DEFAULT 0,
	created_at  BIGINT NOT NULL,
	updated_at  BIGINT NOT NULL,
	PRIMARY KEY (org_id, name)
);
CREATE TABLE IF NOT EXISTS org_credits (
	org_id     TEXT PRIMARY KEY,
	balance    DOUBLE PRECISION NOT NULL,
	granted    DOUBLE PRECISION NOT NULL,
	plan_id    TEXT NOT NULL DEFAULT 'free',
	renews_at  BIGINT NOT NULL DEFAULT 0,
	updated_at BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS audit_log (
	id      BIGSERIAL PRIMARY KEY,
	org_id  TEXT NOT NULL DEFAULT 'default',
	key_id  TEXT NOT NULL DEFAULT '',
	action  TEXT NOT NULL,
	target  TEXT NOT NULL DEFAULT '',
	at      BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_org_at ON audit_log(org_id, at);
CREATE INDEX IF NOT EXISTS idx_keys_org ON api_keys(org_id);
CREATE TABLE IF NOT EXISTS idempotency (
	org_id   TEXT NOT NULL DEFAULT 'default',
	idem_key TEXT NOT NULL,
	response TEXT NOT NULL,
	at       BIGINT NOT NULL,
	PRIMARY KEY (org_id, idem_key)
);
`
