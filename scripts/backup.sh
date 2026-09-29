#!/usr/bin/env bash
# Back up the sandboxd metadata store: pg_dump when SANDBOXD_DATABASE_URL is
# set, otherwise the SQLite file. Prints the artifact path on stdout.
#
# Env:
#   BACKUP_DIR            output dir (default ./backups)
#   SANDBOXD_DATA         data dir (default /var/lib/sandboxd), SQLite lives here
#   SANDBOXD_DB           explicit SQLite path (overrides $SANDBOXD_DATA/state.db)
#   SANDBOXD_DATABASE_URL Postgres DSN; when set, pg_dump is used
#
# Usage: scripts/backup.sh
set -euo pipefail

BACKUP_DIR="${BACKUP_DIR:-./backups}"
SANDBOXD_DATA="${SANDBOXD_DATA:-/var/lib/sandboxd}"
ts="$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$BACKUP_DIR"

if [ -n "${SANDBOXD_DATABASE_URL:-}" ]; then
  out="$BACKUP_DIR/sandboxd-pg-$ts.sql.gz"
  echo "backing up Postgres → $out" >&2
  pg_dump "$SANDBOXD_DATABASE_URL" | gzip >"$out"
else
  db="${SANDBOXD_DB:-$SANDBOXD_DATA/state.db}"
  out="$BACKUP_DIR/sandboxd-sqlite-$ts.db"
  echo "backing up SQLite $db → $out" >&2
  # .backup takes a consistent snapshot even while sandboxd is running (WAL mode).
  sqlite3 "$db" ".backup '$out'"
fi

echo "$out"
