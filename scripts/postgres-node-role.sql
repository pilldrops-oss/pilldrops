-- Least-privilege Postgres role for a NODE.
--
-- Why this exists
-- ---------------
-- A node runs untrusted customer code and is handed a Postgres DSN at join, so
-- "compromising a node yields nothing sensitive" was never true — it shares the
-- control plane's database. Routing every node write through the orchestrator's
-- API would be the stronger fix and is not a small change: node-tier code calls
-- ~75 distinct store methods (keys, audit, usage, agent tokens, chat, cron,
-- webhooks, functions, sandbox config, and the operator dashboard's analytics),
-- and proxying all of them would put the control plane in the hot path of every
-- node operation.
--
-- So the boundary is enforced in the database, which is where a
-- credential-scoped boundary belongs.
--
-- IF YOU RAN AN EARLIER VERSION OF THIS SCRIPT, RE-RUN IT
-- ------------------------------------------------------
-- Until 2026-08-31 this file revoked the deny-list in a single multi-table
-- statement naming `credit_ledger`, which migration 36 drops. On any database
-- migrated to 36 or later that statement errored, and `\set ON_ERROR_STOP on`
-- then aborted the script — AFTER the blanket GRANT below and BEFORE any
-- revoke. The role was left with SELECT, INSERT, UPDATE and DELETE on every
-- table in the schema, including vault_secrets. psql exits non-zero in that
-- case, but a deploy step that did not check the status reported success.
--
-- The revokes are per-table now, so a table that is legitimately gone is a
-- NOTICE rather than an abort. The script is idempotent: re-running it on an
-- affected database restores the intended grants. Verify with the query at the
-- end of this file — it must return zero rows.
--
-- This is a DENY-list, deliberately
-- ---------------------------------
-- An allow-list was written first and was wrong: `sandboxd` is also the
-- single-box product and serves an operator dashboard, so node-tier code
-- legitimately reads users, plans, login_events and even waitlist. A grant-list
-- built from a guess at "what a node needs" breaks fleets on deploy.
--
-- So this revokes only what node-tier code provably never touches. Each table
-- below was checked by taking every exported Store method defined in the file
-- that owns it and grepping all non-test node-tier packages for a call:
--
--   vault_secrets    no calls. Tenants' provider API keys; secret_refs are
--                    resolved on the CONTROL PLANE before a create is
--                    forwarded, so a node has never needed them. This is the
--                    important one. With VAULT_SECRET_KEY also absent from the
--                    join bundle, a node can neither read the ciphertext nor
--                    decrypt it.
--   admission_slots  no calls. The fleet-wide concurrency claim. A node that
--                    could delete slot rows could lift its own tenant's cap —
--                    and admission lives on the control plane precisely because
--                    a node cannot see the whole fleet.
--   credit_ledger    no calls.
--   pricing          no calls. A node meters; it does not decide what anything
--   billing_cursor   no calls. costs.
--
-- credit_ledger, pricing and billing_cursor no longer EXIST — migration 47
-- dropped the last of them. They stay on the list because the loop below
-- treats an absent table as a NOTICE, and a fork that adds billing back will
-- recreate them under the same names.
--
-- Step 3a adds a second, weaker kind of entry: tables a node reads but must
-- never write (template_builds, artifacts). They are separate from the list
-- above because REVOKE ALL would break lazy template fetch, and the reasoning
-- for revoking only the writes is worth stating where it applies.
--
-- `users`, `login_events` and `waitlist` are a special case, handled in step 3b:
-- the node's dashboard reads them, which is correct on a single box and
-- pointless on a fleet node. A clustered node now refuses those endpoints, so a
-- FLEET deployment can revoke them too.
--
-- Usage
-- -----
--   psql "$SUPERUSER_DSN" -v node_password="$NODE_PASSWORD" -f scripts/postgres-node-role.sql
--   # then hand nodes: postgres://pilldrops_node:$NODE_PASSWORD@host/db
--
-- Pass the password RAW. Step 1 uses psql's :'node_password' form, which quotes
-- and escapes the value itself — so a value that arrives already wrapped in
-- single quotes becomes a password that CONTAINS them. This line used to say
-- `-v node_password="'…'"` while the DSN line beside it omitted them, and a
-- deployment that followed both got `password authentication failed for user
-- "pilldrops_node"` on every node, with the role, the grants and the DSN all
-- looking correct. Found while standing up a two-node fleet for tasks.md §4.7.
--
-- Re-runnable. Verify with the query at the bottom.

\set ON_ERROR_STOP on

-- 1. The role.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pilldrops_node') THEN
    CREATE ROLE pilldrops_node LOGIN;
  END IF;
END
$$;

ALTER ROLE pilldrops_node WITH PASSWORD :'node_password';
ALTER ROLE pilldrops_node WITH NOSUPERUSER NOCREATEDB NOCREATEROLE;

-- 2. Everything the schema has today, minus the deny-list in step 3.
GRANT USAGE ON SCHEMA public TO pilldrops_node;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO pilldrops_node;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO pilldrops_node;

-- 3. The deny-list. Verified above as never touched by node-tier code.
--
-- Table-at-a-time inside a DO block rather than one multi-table REVOKE, because
-- a plain REVOKE names every table in one statement and errors if ANY of them is
-- absent — and with ON_ERROR_STOP that aborts the whole script.
--
-- That was not hypothetical. `credit_ledger` was dropped by migration 36 with
-- the rest of the commercial layer, so on any database migrated past 36 this
-- script failed here and stopped: step 2's blanket GRANT had already run, and
-- NONE of the revokes below it did. Measured on 16.11 against a freshly
-- migrated schema, the role the script left behind held SELECT on
-- `vault_secrets` — the entry this file calls "the important one" — and DELETE
-- on `admission_slots`, and the script reported it in one line of psql output.
--
-- (Step 4 was the exception, and only by luck: PostgreSQL 15 removed PUBLIC's
-- CREATE on schema public, so the role had no DDL to revoke. On 14 and earlier
-- it would have kept that too.)
--
-- Absent is therefore a NOTICE, not an error: `pricing` and `billing_cursor`
-- survive, `credit_ledger` does not, and a fork that adds billing back will
-- recreate it. The entry costs nothing and keeps working across all three.
DO $$
DECLARE
  t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['vault_secrets', 'admission_slots', 'credit_ledger', 'pricing', 'billing_cursor'] LOOP
    IF to_regclass('public.' || t) IS NULL THEN
      RAISE NOTICE 'deny-list: % is not in this schema, nothing to revoke', t;
      CONTINUE;
    END IF;
    EXECUTE format('REVOKE ALL ON %I FROM pilldrops_node', t);
  END LOOP;
END
$$;

-- 3a. Read-only, not denied: the artifact index (migrations 41 and 42).
--
--     A node legitimately READS these. It resolves which build a template
--     resolves to and where that build's objects live, then fetches and boots
--     from the store (tasks.md §4.6). Denying the reads would make lazy fetch
--     a control-plane round trip per create for no security gained: the rows
--     carry build ids, sizes, checksums and version strings — no tenant data,
--     no credential.
--
--     A node must never WRITE them, and this is the one grant on this list
--     where the difference is remote code execution rather than disclosure.
--
--     This comment used to say a node can ALREADY put arbitrary bytes into the
--     shared store, because `internal/artifact.Handler` authenticates with the
--     node key, "which is an operator-role credential". Both halves were wrong,
--     and the second was repeated from the Handler's own doc comment, which has
--     since been corrected: the node key is held by EVERY node and is not an
--     operator credential. The node-facing mount is now
--     `artifact.NewReadOnlyHandler` — PUT and DELETE answer 405 — so a node
--     cannot write objects either.
--
--     The reasoning below stands on its own regardless, and is why the revoke
--     is worth keeping even now that object writes are closed: two independent
--     things must both hold for a node's compromise to become the fleet's, and
--     design.md D7's "Postgres owns what is VISIBLE and USABLE" is the second
--     of them — a security boundary, not merely a listing optimisation.
--
--     Give a node INSERT or UPDATE here and that boundary is gone: it could
--     mark its own build ready, or repoint a template at a build whose bytes it
--     wrote, and every subsequent create on any node fetches, checksum-VERIFIES
--     (against the attacker's own header) and boots it. DELETE is the same
--     defect in the other direction — one node could unpublish the fleet's
--     templates.
--
--     Publishing is operator-only and lives on the CONTROL PLANE
--     (POST /v1/templates/{id}/artifacts, tasks.md §4.5). In a fleet the node
--     that runs a build returns its result; the orchestrator writes the rows.
--     So this revoke removes nothing a node does.
--
--     Asserted by TestNodeTierNeverCallsMethodsOnDeniedTables, which fails if
--     node-tier code gains a Store method that writes either table, and if this
--     statement stops matching the list that test holds.
--
--     As with 3b, do NOT apply to a single-box `sandboxd` on Postgres: there the
--     one process IS the control plane, and it publishes.
--
--     Absent is an ERROR here, unlike step 3. Those tables may legitimately have
--     been dropped by a past migration; these two are created by migrations that
--     ship with this code, so missing means the script was pointed at a database
--     that has not been migrated — and silently skipping would leave the node
--     with the write grant this section exists to remove.
DO $$
DECLARE
  t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['template_builds', 'artifacts'] LOOP
    IF to_regclass('public.' || t) IS NULL THEN
      RAISE EXCEPTION 'artifact index table % is missing: run the migrations (41, 42) before granting the node role, or the node keeps INSERT/UPDATE/DELETE on it', t;
    END IF;
    EXECUTE format('REVOKE INSERT, UPDATE, DELETE ON %I FROM pilldrops_node', t);
  END LOOP;
END
$$;

-- 3b. FLEET nodes only. A clustered node refuses the endpoints that read these
--     (see refuseFleetWideTenantData in internal/httpapi): the orchestrator
--     serves the dashboard in a fleet and reads the same database itself, so a
--     node has no reason to enumerate customers.
--
--     Do NOT apply this section to a single-box `sandboxd`, which IS its own
--     dashboard and legitimately reads all three.
--
--   REVOKE ALL ON users, login_events, waitlist FROM pilldrops_node;
--
--     `plans` used to be excluded here on the grounds that PlanFor read it on
--     every create. That has not been true for some time: PlanFor reads
--     `org_plans`, plan LIMITS are Go values (internal/entitlements), and
--     migration 47 dropped the `plans` table outright.

-- 4. No DDL. Migrations belong to the control plane; a node that can ALTER the
--    shared schema can do anything to it. The orchestrator holds the migration
--    advisory lock and runs them.
REVOKE CREATE ON SCHEMA public FROM pilldrops_node;

-- 5. A table added by a LATER migration is granted by step 2 only because that
--    step ran; new tables are not covered retroactively. Make the default
--    explicit so a future table is readable rather than mysteriously not — and
--    re-run this script after adding one that should be denied.
--
--    ALTER DEFAULT PRIVILEGES only covers objects created by the role that runs
--    it, so if migrations run as a DIFFERENT role than this script, step 5 does
--    not cover them and the script must be re-run after every migration that
--    adds a table. `usage_totals` (migration 45) is the one to check first: a
--    node WRITES it on every metering flush, in the same transaction as the
--    usage_records insert, so a missing grant does not degrade metering — it
--    stops it, loudly, in the node's log.
ALTER DEFAULT PRIVILEGES IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO pilldrops_node;

-- 6. Verify. The five fully denied tables must NOT appear, and template_builds
--    and artifacts must appear with SELECT and nothing else:
--
--   SELECT table_name, string_agg(DISTINCT privilege_type, ',' ORDER BY privilege_type)
--     FROM information_schema.table_privileges
--    WHERE grantee = 'pilldrops_node'
--    GROUP BY table_name
--    ORDER BY table_name;
--
-- And this must return zero rows:
--
--   SELECT 1 FROM information_schema.table_privileges
--    WHERE grantee = 'pilldrops_node'
--      AND (table_name IN ('vault_secrets','admission_slots','credit_ledger','pricing','billing_cursor')
--           OR (table_name IN ('template_builds','artifacts') AND privilege_type <> 'SELECT'));
