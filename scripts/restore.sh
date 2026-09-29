#!/usr/bin/env bash
# Restore a sandboxd metadata store from a backup produced by scripts/backup.sh.
# Stop sandboxd before restoring a live store.
#
# A *.sql.gz artifact restores into Postgres (target = $2 or SANDBOXD_DATABASE_URL);
# any other artifact is treated as a SQLite file and copied into place
# (target = $2 or $SANDBOXD_DB or $SANDBOXD_DATA/state.db).
#
# Usage: scripts/restore.sh <backup-file> [target]
set -euo pipefail

src="${1:?usage: restore.sh <backup-file> [target]}"
[ -f "$src" ] || { echo "no such backup: $src" >&2; exit 1; }

case "$src" in
*.sql.gz)
  target="${2:-${SANDBOXD_DATABASE_URL:?set SANDBOXD_DATABASE_URL or pass a target DSN}}"
  echo "restoring Postgres dump $src → $target" >&2
  gunzip -c "$src" | psql "$target"
  ;;
*)
  target="${2:-${SANDBOXD_DB:-${SANDBOXD_DATA:-/var/lib/sandboxd}/state.db}}"
  echo "restoring SQLite $src → $target" >&2
  mkdir -p "$(dirname "$target")"
  cp "$src" "$target"
  # Drop any stale WAL/SHM sidecars so the restored snapshot is authoritative.
  rm -f "$target-wal" "$target-shm"
  ;;
esac

echo "restored → $target" >&2
