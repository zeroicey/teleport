#!/usr/bin/env bash
#
# P4: bring up the REAL assembled binary — Gin + pgx + PostgreSQL + the embedded SPA —
# and verify the whole chain end to end (HTTP contract, then a real browser).
#
# Why this stage exists: every check up to P3 ran either in-process (Go tests against
# httptest) or against the store alone. Neither proves that the thing a user actually
# runs works: one binary that boots, migrates its own schema, serves the SPA out of
# its own bytes, answers a real agent over a real socket and renders in a real browser.
#
# It is deliberately self-contained and reversible:
#   - it uses its OWN schema (`p4`) inside the existing `teleport_test` database, and
#     drops that schema on exit. It does not create a database: the `teleport` role has
#     rolcreatedb=false (measured), and granting it CREATEDB would be a change to a
#     PostgreSQL instance shared with clip/registry/serenique for a smoke test's sake.
#     A dedicated schema is the same isolation for a fraction of the blast radius, and
#     it is exactly what the test harness already does per test. The production-intent
#     `teleport` database is therefore untouched until P5;
#   - it generates throwaway secrets; nothing is read from the deployment's env file;
#   - it listens on a local port only.
#
# Usage:
#   scripts/verify-p4.sh                     # builds, starts, verifies, tears down
#   P4_SKIP_BUILD=1 scripts/verify-p4.sh     # reuse bin/teleport-linux-amd64
#   P4_SKIP_BROWSER=1 scripts/verify-p4.sh   # HTTP contract only
#   P4_PORT=8799 scripts/verify-p4.sh
#
# Environment:
#   P4_DATABASE_URL   full DSN to use instead of the local default
#   P4_PGPASSWORD     password for the `teleport` role (else read from the standard file)
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO"

PORT="${P4_PORT:-8788}"
# The route prefix is overridable so the same whole-chain verification can be run
# against the P6 shape (root path). "/" is what config.normalizePrefix maps to
# "no prefix" — an EMPTY value is treated as "unset" by the env reader and falls
# back to the default prefix, which would silently test the wrong thing
# (measured; see .ai/runbooks/p6-cutover.md risk 13).
PREFIX="${P4_ROUTE_PREFIX:-/yeciorez/teleport}"
# The Go side wants "/" for "no prefix"; the URL-building side wants "" (so that
# "$BASE$URL_PREFIX/api/health" has no double slash).
if [ "$PREFIX" = "/" ]; then URL_PREFIX=""; else URL_PREFIX="$PREFIX"; fi
BASE="http://127.0.0.1:$PORT"
# The existing test database, plus a schema this script owns and drops.
DBNAME="${P4_DBNAME:-teleport_test}"
SCHEMA="${P4_SCHEMA:-p4}"
BIN="$REPO/bin/teleport-linux-amd64"
LOG="$REPO/bin/e2e/p4-server.log"

say() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
die() { printf '\nERROR: %s\n' "$*" >&2; exit 1; }

SERVER_PID=""
cleanup() {
  if [ -n "$SERVER_PID" ] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null
    wait "$SERVER_PID" 2>/dev/null
    echo "server stopped (log: ${LOG#$REPO/})"
  fi
  # Drop the schema even on failure: a leftover schema is exactly the kind of stale
  # state that misled this repository once already (see the merged-baseline pitfall).
  if [ "$SCHEMA_DROPPABLE" = "1" ]; then
    psql_q "$DBNAME" "DROP SCHEMA IF EXISTS $SCHEMA CASCADE" >/dev/null 2>&1 \
      && echo "dropped schema $SCHEMA" || true
  fi
}
trap cleanup EXIT
SCHEMA_DROPPABLE=0

# ---------------------------------------------------------------- database ----
PGPW="${P4_PGPASSWORD:-}"
if [ -z "$PGPW" ] && [ -r /data/services/teleport/.pgpassword.tmp ]; then
  PGPW="$(cat /data/services/teleport/.pgpassword.tmp)"
fi
if [ -z "$PGPW" ] && sudo -n true 2>/dev/null; then
  PGPW="$(sudo -n cat /data/services/teleport/.pgpassword.tmp 2>/dev/null || true)"
fi

psql_q() { # <db> <sql>
  if command -v psql >/dev/null 2>&1; then
    PGPASSWORD="$PGPW" psql -h 127.0.0.1 -p 5432 -U teleport -d "$1" -tAc "$2"
  else
    docker exec -e PGPASSWORD="$PGPW" postgres psql -U teleport -d "$1" -tAc "$2"
  fi
}

if [ -n "${P4_DATABASE_URL:-}" ]; then
  DB_URL="$P4_DATABASE_URL"
  say "using P4_DATABASE_URL"
else
  [ -n "$PGPW" ] || die "no PostgreSQL password. Set P4_PGPASSWORD, or P4_DATABASE_URL."
  say "preparing a private schema '$SCHEMA' in '$DBNAME' (the production-intent 'teleport' DB is not touched)"
  # Clean slate, unconditionally: the migrator records a content hash and refuses to
  # start when the live schema disagrees with the migration files, so a leftover schema
  # from an older revision would fail the run for an unrelated reason.
  psql_q "$DBNAME" "DROP SCHEMA IF EXISTS $SCHEMA CASCADE" >/dev/null \
    || die "could not drop the old schema $SCHEMA in $DBNAME"
  psql_q "$DBNAME" "CREATE SCHEMA $SCHEMA" >/dev/null \
    || die "could not create schema $SCHEMA in $DBNAME"
  SCHEMA_DROPPABLE=1
  echo "created schema $DBNAME.$SCHEMA"
  # search_path as a connection runtime parameter: the app's SQL is unqualified, and
  # this is the same mechanism the test harness uses to confine a test to its schema.
  DB_URL="postgres://teleport:${PGPW}@127.0.0.1:5432/${DBNAME}?sslmode=disable&search_path=${SCHEMA}"
fi

# ------------------------------------------------------------------ build ----
if [ "${P4_SKIP_BUILD:-0}" != "1" ]; then
  say "release build (frontend -> //go:embed -> single binary)"
  # VITE_ROUTE_PREFIX must be passed through: the SPA's asset base is baked in at
  # build time (vite.config.ts), so a run with P4_ROUTE_PREFIX=/ that used the
  # default build would test a root-path backend serving an old-prefix frontend —
  # a mismatch that looks like a product bug. Same pairing the P6 runbook warns
  # about (risk 9/13).
  VITE_ROUTE_PREFIX="$PREFIX" pnpm run --silent build || die "frontend build failed"
  mkdir -p "$(dirname "$BIN")"
  ( cd backend && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
      -trimpath -tags embed_frontend -ldflags "-s -w" -o "$BIN" . ) || die "go build failed"
  echo "built $(basename "$BIN") ($(du -h "$BIN" | cut -f1))"
fi
[ -x "$BIN" ] || die "no binary at $BIN (run without P4_SKIP_BUILD=1)"

say "binary self-report"
"$BIN" version || die "the binary does not run"
FRONTEND="$("$BIN" version --frontend)"
echo "embedded frontend: $FRONTEND"
[ "$FRONTEND" = "embedded" ] || die "the binary has no embedded frontend; P4 would test an API-only build"

# ------------------------------------------------------------------ start ----
say "secrets (throwaway, generated here — nothing from the deployment)"
# Enough entropy that the alphanumeric filter still leaves a long password: base64 of
# 24 bytes loses a few characters to `tr`, so asking for 16 from a smaller draw can
# silently yield a short one.
ADMIN_PASSWORD="p4-$(head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 24)"
[ -n "$ADMIN_PASSWORD" ] || die "could not generate an admin password"

# `hash-password` prints a ready-to-paste env line ("ADMIN_PASSWORD_HASH=pbkdf2$..."),
# not the bare hash. Using its stdout verbatim as the env value therefore embeds the
# literal prefix in the value and every login fails with 401 — which is exactly what
# happened on the first P4 run. The strip is asserted rather than assumed, so a change
# to that output format fails here with a clear message instead of as 20 mystery 401s.
HASH_LINE="$("$BIN" hash-password "$ADMIN_PASSWORD")" || die "hash-password failed"
case "$HASH_LINE" in
  ADMIN_PASSWORD_HASH=*) ADMIN_HASH="${HASH_LINE#ADMIN_PASSWORD_HASH=}" ;;
  *) die "unexpected hash-password output (want ADMIN_PASSWORD_HASH=...): ${HASH_LINE:0:40}" ;;
esac
[ -n "$ADMIN_HASH" ] || die "hash-password produced an empty hash"
# Assigned and exported on separate lines: `export X="$(...)"` masks the command
# substitution's exit status, so a failure generating the secret would go unnoticed
# and the server would start with an empty one.
AGENT_SECRET_KEY="p4-agent-$(head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9')"
SESSION_SECRET="p4-session-$(head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9')"
[ -n "$AGENT_SECRET_KEY" ] && [ -n "$SESSION_SECRET" ] || die "could not generate secrets"
export AGENT_SECRET_KEY SESSION_SECRET
echo "generated an admin password and two secrets (values not printed)"

say "starting the server on $BASE$URL_PREFIX"
mkdir -p "$(dirname "$LOG")"

# Pre-flight: nothing may already be answering at this address. Without this, a server
# left over from an earlier run (or a leftover `go run` from a debugging session) keeps
# the port, our own start fails silently in the background, and the checks then run
# against THAT process — which is pointing at a schema this run has just dropped and
# recreated. The result was a single, unexplained check failure on one run out of
# several. Failing loudly here turns that into an obvious message.
if curl -sf -o /dev/null --max-time 2 "$BASE$URL_PREFIX/api/health" 2>/dev/null; then
  die "something is already serving $BASE$URL_PREFIX (its /api/health answered).
Stop it (e.g. pkill -f teleport-linux-amd64) or choose another port with P4_PORT=."
fi

: > "$LOG"
LISTEN_ADDR="127.0.0.1:$PORT" \
ROUTE_PREFIX="$PREFIX" \
PUBLIC_BASE_URL="$BASE" \
DATABASE_URL="$DB_URL" \
DB_MAX_CONNS=4 \
ADMIN_PASSWORD_HASH="$ADMIN_HASH" \
ENVIRONMENT=development \
COOKIE_SECURE=false \
"$BIN" >>"$LOG" 2>&1 &
SERVER_PID=$!

for _ in $(seq 1 60); do
  if curl -sf -o /dev/null "$BASE$URL_PREFIX/api/health"; then break; fi
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    echo "--- server log ---"; cat "$LOG"; die "the server exited during startup"
  fi
  sleep 0.5
done
curl -sf -o /dev/null "$BASE$URL_PREFIX/api/health" || { echo "--- server log ---"; cat "$LOG"; die "server never became healthy"; }
echo "server is up (pid $SERVER_PID)"

# The startup log is evidence too: a schema migration should have run against a fresh
# database, and no warning about a missing frontend should appear.
grep -q 'database ready' "$LOG" && echo "log: database ready"
grep -qi 'no frontend build' "$LOG" && die "the server warns it has no frontend"
grep -qi 'migration applied' "$LOG" && echo "log: schema applied on a fresh database"
grep -qi 'COOKIE_SECURE is false' "$LOG" && echo "log: warned about the non-Secure cookie (expected on plain HTTP)"

# --------------------------------------------------------------- contract ----
say "HTTP contract over a real socket"
# Kept on disk: a green run's output is not interesting, but a failure that only shows
# up once in several runs is impossible to diagnose after the terminal has scrolled.
CONTRACT_LOG="$REPO/bin/e2e/p4-contract.log"
bash scripts/verify-p4-contract.sh "$BASE" "$URL_PREFIX" "$AGENT_SECRET_KEY" "$ADMIN_PASSWORD" 2>&1 | tee "$CONTRACT_LOG"
CONTRACT=${PIPESTATUS[0]}
[ "$CONTRACT" = 0 ] || die "the HTTP contract check failed (full output: ${CONTRACT_LOG#$REPO/})"

# ---------------------------------------------------------------- browser ----
if [ "${P4_SKIP_BROWSER:-0}" = "1" ]; then
  say "browser verification SKIPPED (P4_SKIP_BROWSER=1)"
else
  # Seed a report whose body contains a Mermaid diagram, so the browser check can
  # prove the diagram is actually rendered by the page's client script under the CSP
  # it is served with. That is the one thing neither the Go tests nor a curl can show:
  # a blocked script still returns a perfectly good HTML page.
  say "seeding a Mermaid report for the browser check"
  # The seed body is built in Python, not by shell string escaping. An earlier
  # version used a printf with backslash-escaped backticks inside single quotes, which
  # yields a literal backslash and therefore ``\`\`\`mermaid`` instead of a real fence —
  # so the report rendered as a paragraph, the page had no .mermaid element, and the
  # browser check failed for a reason that had nothing to do with the product.
  SEED_BODY="$(python3 - "$(date +%s)" <<'PYSEED'
import json, sys
stamp = sys.argv[1]
md = f"""# {stamp} mermaid

Text before the diagram.

```mermaid
graph TD;
  A[agent]-->B[teleport];
  B-->C[share page];
```

Text after the diagram.
"""
print(json.dumps({"title": "p4 mermaid browser check", "content": md, "autoShareHours": 0}))
PYSEED
)"
  SEED="$(curl -sS -X POST -H 'Content-Type: application/json' \
      -H "Authorization: Bearer $AGENT_SECRET_KEY" --data-binary "$SEED_BODY" \
      "$BASE$URL_PREFIX/api/reports")"
  BROWSER_SHARE="$(printf '%s' "$SEED" | python3 -c 'import json,sys
try:
    print(json.load(sys.stdin)["data"]["share"]["token"])
except Exception:
    print("")')"
  [ -n "$BROWSER_SHARE" ] || die "could not seed the Mermaid report: $SEED"
  echo "seeded share token for the browser check"

  say "real browser verification (headless Chromium over CDP)"
  BROWSER_LOG="$REPO/bin/e2e/p4-browser.log"
  node scripts/verify-teleport-browser.cjs "$BASE" "$URL_PREFIX" "$ADMIN_PASSWORD" "$BROWSER_SHARE" 2>&1 | tee "$BROWSER_LOG"
  BROWSER=${PIPESTATUS[0]}
  [ "$BROWSER" = 0 ] || die "the browser verification failed (full output: ${BROWSER_LOG#$REPO/})"
fi

say "P4 PASSED — the assembled binary serves the contract and renders in a browser"
echo "  database: $DBNAME.$SCHEMA (the schema is dropped on exit)"
echo "  log:      ${LOG#$REPO/}"
echo "  artifacts: bin/e2e/"
