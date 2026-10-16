#!/usr/bin/env bash
#
# Regenerate the readable PostgreSQL schema snapshot from the migrations.
#
# The migrations are the single source of truth: they are what the binary embeds
# and applies. `schema.pg.sql` is the snapshot a human reads and can run with
# `psql -f` against a bare database. It is generated rather than hand-maintained
# because a hand-maintained copy of 300 lines of DDL drifts, and the drift is
# invisible until someone trusts the snapshot.
#
# `backend/internal/store/schema_pg_sync_test.go` fails if the two disagree, so
# forgetting to run this after editing a migration is caught by the gate.
#
# Usage: scripts/gen-schema-pg.sh
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="$REPO/backend/internal/store/migrations_pg"
OUT="$REPO/schema.pg.sql"

if [ ! -d "$SRC" ]; then
  echo "no migrations directory at $SRC" >&2
  exit 1
fi

# Filename order is application order (see the naming test in
# schema_pg_sync_test.go), so a plain glob in a C-locale sort is exactly right.
shopt -s nullglob
files=("$SRC"/*.sql)
if [ ${#files[@]} -eq 0 ]; then
  echo "no .sql migrations in $SRC" >&2
  exit 1
fi

cat "${files[@]}" > "$OUT"
echo "wrote $OUT from ${#files[@]} migration(s):"
for f in "${files[@]}"; do echo "  $(basename "$f")"; done
echo "  $(wc -l < "$OUT") lines, sha256 $(sha256sum "$OUT" | cut -d' ' -f1)"
