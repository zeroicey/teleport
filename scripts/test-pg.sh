#!/usr/bin/env bash
#
# The gate that actually runs the PostgreSQL-backed tests.
#
# Why this exists as a second script rather than as the default gate:
# scripts/check.sh must stay runnable on a machine with no database, so its
# PostgreSQL tests SKIP when TEST_PG_DSN is unset. That skip is exactly how a
# "green" run came to mean nothing on 2026-10-16: P1 was recorded as verified
# against the real database while the test database had drifted away from the
# committed migration and one of those tests was in fact failing
# (.ai/pitfalls/cases/2026-10-16-merged-baseline-hides-a-stale-test-db.md).
#
# So this script refuses to run without a database, refuses to run against a
# database that does not look like a test database, and — because a guard nobody
# has watched fail is a guard nobody knows is wired up — checks that the harness
# itself still refuses the wrong database.
#
# Usage:
#   TEST_PG_DSN='postgres://teleport:PASSWORD@127.0.0.1:5432/teleport_test?sslmode=disable' \
#     pnpm run backend:test-pg
#
# See .ai/decisions/2026-10-16-p2-test-strategy.md for the isolation strategy
# (one throwaway schema per test).
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO/backend"

if [ -z "${TEST_PG_DSN:-}" ]; then
  cat >&2 <<'EOF'
TEST_PG_DSN is not set, so the PostgreSQL-backed tests would be skipped.

This script exists so that never happens silently. Set it:

  TEST_PG_DSN='postgres://teleport:PASSWORD@127.0.0.1:5432/teleport_test?sslmode=disable' \
    pnpm run backend:test-pg

The password is in /data/services/teleport/.pgpassword.tmp on this host (root-only).
EOF
  exit 1
fi

case "$TEST_PG_DSN" in
  *test*) ;;
  *)
    echo "refusing to run: TEST_PG_DSN names a database without 'test' in it" >&2
    exit 1
    ;;
esac

echo "==> harness self-check: a non-test database must be refused"
# The harness creates and drops schemas in whatever database it is pointed at.
# Point it at the production-intent name and require a refusal: a pass here would
# mean the guard is not wired up, and a skip would mean it degrades to silence.
bad_dsn='postgres://teleport:x@127.0.0.1:5432/teleport?sslmode=disable'
if bad_out="$(TEST_PG_DSN="$bad_dsn" go test ./internal/store/ -count=1 -run TestPGTimestampsAreBigint 2>&1)"; then
  echo "the harness ACCEPTED a DSN naming the production database 'teleport'." >&2
  echo "It would have created and dropped schemas there. Refusing to continue." >&2
  exit 1
fi
case "$bad_out" in
  *"does not contain"*) ;;
  *)
    echo "the harness failed against a non-test database, but not by refusing it:" >&2
    echo "$bad_out" >&2
    exit 1
    ;;
esac

echo "==> go test (real database, one schema per test)"
# -count=1 disables the test cache: several guards read files from outside their
# package, and the cache cannot see those change.
go test ./... -count=1 "$@"

echo
echo "postgres gate passed"
