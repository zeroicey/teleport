#!/usr/bin/env bash
#
# The gate for the teleport service.
#
# The old repository's gate was `typecheck + schema-sync + go test`. It never
# checked formatting or vet — so four files were not gofmt-clean in the tree that
# was running in production, and nothing ever said so. Both are in here now.
#
# Usage: scripts/check.sh
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO"

echo "==> frontend typecheck"
pnpm run --silent typecheck

echo "==> gofmt"
unformatted="$(gofmt -l backend | grep -v '^$' || true)"
if [ -n "$unformatted" ]; then
  echo "these files are not gofmt-clean:" >&2
  echo "$unformatted" >&2
  echo >&2
  echo "NOTE: gofmt also reformats doc comments (Go 1.19+), and that pass rewrites" >&2
  echo "a literal '' in a comment into a curly quote. If a comment needs to show an" >&2
  echo "empty string, write it in prose." >&2
  exit 1
fi

echo "==> go vet"
(cd backend && go vet ./...)

echo "==> schema snapshot (generated; cache-proof)"
# This runs the generator into a temp file and compares bytes, rather than
# relying on the Go test that does the same comparison. Reason: the Go test reads
# a file *outside* its package, and `go test` caches a PASS without knowing that
# file changed — so a stale snapshot could be reported green from the cache. A
# shell comparison cannot be cached. (Found by trying to falsify the guard and
# watching it stay green.)
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
shopt -s nullglob
cat backend/internal/store/migrations_pg/*.sql > "$tmp"
if ! cmp -s "$tmp" schema.pg.sql; then
  echo "schema.pg.sql is stale: run scripts/gen-schema-pg.sh and commit the result" >&2
  diff -u schema.pg.sql "$tmp" | head -30 >&2
  exit 1
fi

echo "==> sqlc (generated code must match the SQL)"
"$REPO/scripts/check-sqlc-sync.sh"

echo "==> go test"
# -count=1 disables the test cache. Several guards here read files from outside
# their package (the schema snapshots), and the cache cannot see those change.
#
# The PostgreSQL-backed tests SKIP here when TEST_PG_DSN is unset, so this gate
# stays runnable on a machine with no database. That skip is why
# scripts/test-pg.sh exists and why it is the required gate for any change that
# touches storage or the store SQL: a green run *here* does not mean the
# database paths were exercised.
#
# That distinction is load-bearing and easy to lose, so the run is verbose and the
# skipped count is reported in the closing line. Measured when the store moved to
# PostgreSQL: without a DSN, internal/api skips 76 of its 79 tests and
# internal/store skips 57 of its 63 — i.e. almost every frozen-contract test. A
# bare "gate passed" over that is how "verified" becomes a false claim, which is
# the failure this repository has already paid for twice.
testlog="$(mktemp)"
trap 'rm -f "$tmp" "$testlog"' EXIT
if [ -z "${TEST_PG_DSN:-}" ]; then
  echo "    TEST_PG_DSN is unset: the PostgreSQL-backed tests will SKIP." >&2
  echo "    The real gate is: pnpm run backend:test-pg" >&2
fi
(cd backend && go test ./... -count=1 -v) | tee "$testlog"

skipped="$(grep -c '^--- SKIP' "$testlog" || true)"
echo
if [ "${skipped:-0}" -gt 0 ]; then
  echo "gate passed — WITHOUT DATABASE-BACKED TESTS: $skipped test(s) skipped."
  echo "This does not mean the store, the SQL or the frozen contracts were exercised."
  echo "Run the real gate before trusting this: pnpm run backend:test-pg"
else
  echo "gate passed"
fi
