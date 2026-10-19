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
# `${VAR:-default}` substitutes on UNSET **or EMPTY**, which is exactly the
# env()-falls-back-to-the-default trap this variable is about: `P4_ROUTE_PREFIX=`
# would silently run the OLD-prefix shape and go green, i.e. a false green for the
# one property a root-path run exists to check. Use `${VAR-default}` (unset only)
# and normalise "" and "/" to the root path explicitly.
case "${P4_ROUTE_PREFIX-/yeciorez/teleport}" in
  ""|"/") PREFIX="/" ;;
  *)      PREFIX="${P4_ROUTE_PREFIX-/yeciorez/teleport}" ;;
esac
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
# The cleanup trap is installed AFTER the symbols it uses are defined: it reads
# SCHEMA_DROPPABLE, SCHEMA and the psql_q function, and a signal arriving in the
# window between `trap` and their definitions made the trap itself fail under
# `set -u` (masking the original failure and skipping the schema drop).
# ---------------------------------------------------------------- database ----
PGPW="${P4_PGPASSWORD:-}"
if [ -z "$PGPW" ] && [ -r /data/services/teleport/.pgpassword.tmp ]; then
  PGPW="$(cat /data/services/teleport/.pgpassword.tmp)"
fi
if [ -z "$PGPW" ] && sudo -n true 2>/dev/null; then
  PGPW="$(sudo -n cat /data/services/teleport/.pgpassword.tmp 2>/dev/null || true)"
fi
SCHEMA_DROPPABLE=0

psql_q() { # <db> <sql>
  if command -v psql >/dev/null 2>&1; then
    PGPASSWORD="$PGPW" psql -h 127.0.0.1 -p 5432 -U teleport -d "$1" -tAc "$2"
  else
    docker exec -e PGPASSWORD="$PGPW" postgres psql -U teleport -d "$1" -tAc "$2"
  fi
}
trap cleanup EXIT

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
P4_ADMIN_PASSWORD="$ADMIN_PASSWORD" bash scripts/verify-p4-contract.sh "$BASE" "$URL_PREFIX" "$AGENT_SECRET_KEY" 2>&1 | tee "$CONTRACT_LOG"
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
  P4_ADMIN_PASSWORD="$ADMIN_PASSWORD" node scripts/verify-teleport-browser.cjs "$BASE" "$URL_PREFIX" "$BROWSER_SHARE" 2>&1 | tee "$BROWSER_LOG"
  BROWSER=${PIPESTATUS[0]}
  [ "$BROWSER" = 0 ] || die "the browser verification failed (full output: ${BROWSER_LOG#$REPO/})"
fi

# ------------------------------------------------------------ abuse controls ----
# Every phase above runs with ENVIRONMENT=development, where the abuse controls are
# deliberately OFF. That proves the guards do not break the product; it cannot prove
# they are wired to the PRODUCTION defaults, which is the whole point of the feature
# and the one thing no in-process test can show (those build a Config by hand and so
# never exercise config.Load()'s production branch).
#
# So the server is restarted in production mode and the controls are exercised over a
# real socket. Every request carries an X-Forwarded-For, because the peer here IS
# 127.0.0.1 — a trusted proxy — so ClientIP takes the forwarded value exactly as it
# does behind Caddy. That path (trusted peer → rightmost untrusted XFF entry) is the
# one the whole control depends on and the one a "looks like it works" test misses.
if [ "${P4_SKIP_ABUSE:-0}" = "1" ]; then
  say "abuse-control phase SKIPPED (P4_SKIP_ABUSE=1)"
else
  say "abuse controls over a real socket (production defaults)"

  kill "$SERVER_PID" 2>/dev/null; wait "$SERVER_PID" 2>/dev/null
  SERVER_PID=""

  ABUSE_PORT="${P4_ABUSE_PORT:-$((PORT + 1))}"
  ABUSE_BASE="http://127.0.0.1:$ABUSE_PORT"
  ABUSE_LOG="$REPO/bin/e2e/p4-abuse.log"
  # Deliberately tiny budgets: the point is the wiring, not the numbers.
  ABUSE_LOGIN_LIMIT=3
  BLOCKED_ADDR="203.0.113.7"
  OK_ADDR="198.51.100.5"

  : >"$ABUSE_LOG"
  env LISTEN_ADDR="127.0.0.1:$ABUSE_PORT" \
      ROUTE_PREFIX="$PREFIX" \
      PUBLIC_BASE_URL="$ABUSE_BASE" \
      DATABASE_URL="$DB_URL" \
      DB_MAX_CONNS=4 \
      ADMIN_PASSWORD_HASH="$ADMIN_HASH" \
      ENVIRONMENT=production \
      COOKIE_SECURE=false \
      SECURITY_MIDDLEWARE=true \
      LOGIN_RATE_LIMIT="$ABUSE_LOGIN_LIMIT" \
      BLOCKED_IPS="$BLOCKED_ADDR" \
      GLOBAL_RATE_LIMIT=5000 \
      "$BIN" >>"$ABUSE_LOG" 2>&1 &
  SERVER_PID=$!

  for _ in $(seq 1 60); do
    if curl -sf -o /dev/null -H "X-Forwarded-For: $OK_ADDR" "$ABUSE_BASE$URL_PREFIX/api/health"; then break; fi
    if ! kill -0 "$SERVER_PID" 2>/dev/null; then
      echo "--- server log ---"; cat "$ABUSE_LOG"; die "the server exited during startup (production mode)"
    fi
    sleep 0.5
  done

  abuse_fail=0
  abuse_check() { # <label> <expected-status> <actual-status> [detail]
    if [ "$2" = "$3" ]; then
      printf 'PASS  %s\n' "$1"
    else
      printf 'FAIL  %s — status %s, want %s%s\n' "$1" "$3" "$2" "${4:+ ($4)}"
      abuse_fail=1
    fi
  }

  # The startup log is evidence: production mode must say so, and an explicit
  # SECURITY_MIDDLEWARE=true must be in effect rather than inferred.
  grep -q 'environment=production' "$ABUSE_LOG" || { echo "--- server log ---"; cat "$ABUSE_LOG"; die "the server did not start in production mode"; }
  echo "log: started with environment=production"

  # 1. The permanent blocklist refuses a listed address...
  status=$(curl -s -o /tmp/p4-abuse-blocked.json -w '%{http_code}' \
    -H "X-Forwarded-For: $BLOCKED_ADDR" "$ABUSE_BASE$URL_PREFIX/api/health")
  abuse_check "a blocked address (via a trusted proxy's XFF) is refused" 403 "$status"
  grep -q '"ip_blocked"' /tmp/p4-abuse-blocked.json \
    && echo "      envelope: ip_blocked" \
    || { echo "FAIL  the 403 body is not the ip_blocked envelope: $(cat /tmp/p4-abuse-blocked.json)"; abuse_fail=1; }

  # 2. ...while an unlisted address is not, which is what proves check 1 was the
  #    blocklist and not "everything is 403".
  status=$(curl -s -o /dev/null -w '%{http_code}' \
    -H "X-Forwarded-For: $OK_ADDR" "$ABUSE_BASE$URL_PREFIX/api/health")
  abuse_check "an unlisted address is served normally" 200 "$status"

  # 3. The login budget is spent, then refused — with Retry-After and the envelope.
  login_status=""
  for _ in $(seq 1 $((ABUSE_LOGIN_LIMIT + 1))); do
    login_status=$(curl -s -o /tmp/p4-abuse-login.json -w '%{http_code}' \
      -X POST -H 'Content-Type: application/json' -H "X-Forwarded-For: $OK_ADDR" \
      --data '{"password":"definitely-wrong"}' "$ABUSE_BASE$URL_PREFIX/api/admin/login")
  done
  abuse_check "the login budget is enforced (429 after $ABUSE_LOGIN_LIMIT attempts)" 429 "$login_status"
  grep -q '"rate_limited"' /tmp/p4-abuse-login.json \
    && echo "      envelope: rate_limited" \
    || { echo "FAIL  the 429 body is not the rate_limited envelope: $(cat /tmp/p4-abuse-login.json)"; abuse_fail=1; }

  # 4. A *correct* password must also be refused: the limiter runs before the handler,
  #    so a flood costs no PBKDF2 work. If the password were accepted here, the limiter
  #    would be sitting after verification and the CPU vector would still be open.
  status=$(curl -s -o /dev/null -w '%{http_code}' \
    -X POST -H 'Content-Type: application/json' -H "X-Forwarded-For: $OK_ADDR" \
    --data "{\"password\":\"$ADMIN_PASSWORD\"}" "$ABUSE_BASE$URL_PREFIX/api/admin/login")
  abuse_check "a CORRECT password is refused too (limiter runs before verification)" 429 "$status"

  # 5. Budgets are per address: a different source is unaffected by that flood.
  status=$(curl -s -o /dev/null -w '%{http_code}' \
    -X POST -H 'Content-Type: application/json' -H "X-Forwarded-For: 192.0.2.44" \
    --data '{"password":"definitely-wrong"}' "$ABUSE_BASE$URL_PREFIX/api/admin/login")
  abuse_check "another address has its own login budget" 401 "$status"

  # 6. The dashboard CSP goes out report-only on the real binary, and never enforcing.
  csp_headers="$(curl -s -D- -o /dev/null -H "X-Forwarded-For: $OK_ADDR" \
    "$ABUSE_BASE$URL_PREFIX/dashboard")"
  printf '%s' "$csp_headers" | grep -qi '^content-security-policy-report-only:' \
    && echo "PASS  the dashboard CSP is served report-only" \
    || { echo "FAIL  no report-only CSP on the dashboard shell"; abuse_fail=1; }
  if printf '%s' "$csp_headers" | grep -qi '^content-security-policy:'; then
    echo "FAIL  the dashboard CSP is enforcing; it must stay report-only until verified"; abuse_fail=1
  else
    echo "PASS  no enforcing CSP on the dashboard"
  fi

  # 7. The GLOBAL bucket is shared, not per address — and the health probe is
  #    exempt from it. Both are properties that were wrong in the first
  #    implementation of this feature (it was keyed per IP, which makes the backstop
  #    useless against a caller with many addresses), and both are invisible to a
  #    check that only looks at one address.
  #
  #    This restarts the server once more with GLOBAL_RATE_LIMIT=2 so the ceiling is
  #    reachable from a handful of requests.
  kill "$SERVER_PID" 2>/dev/null; wait "$SERVER_PID" 2>/dev/null
  SERVER_PID=""
  GLOBAL_PORT="${P4_GLOBAL_PORT:-$((PORT + 2))}"
  GLOBAL_BASE="http://127.0.0.1:$GLOBAL_PORT"
  GLOBAL_LOG="$REPO/bin/e2e/p4-global.log"
  : >"$GLOBAL_LOG"
  env LISTEN_ADDR="127.0.0.1:$GLOBAL_PORT" \
      ROUTE_PREFIX="$PREFIX" \
      PUBLIC_BASE_URL="$GLOBAL_BASE" \
      DATABASE_URL="$DB_URL" \
      DB_MAX_CONNS=4 \
      ADMIN_PASSWORD_HASH="$ADMIN_HASH" \
      ENVIRONMENT=production \
      COOKIE_SECURE=false \
      SECURITY_MIDDLEWARE=true \
      GLOBAL_RATE_LIMIT=2 \
      BAN_AFTER_REFUSALS=0 \
      "$BIN" >>"$GLOBAL_LOG" 2>&1 &
  SERVER_PID=$!
  for _ in $(seq 1 60); do
    if curl -sf -o /dev/null -H "X-Forwarded-For: 192.0.2.44" "$GLOBAL_BASE$URL_PREFIX/api/health"; then break; fi
    if ! kill -0 "$SERVER_PID" 2>/dev/null; then
      echo "--- server log ---"; cat "$GLOBAL_LOG"; die "the server exited during the global-bucket phase"
    fi
    sleep 0.5
  done

  # Distinct source addresses, so a per-address bucket would allow every one of
  # them while a shared bucket refuses after the ceiling. Ten tight requests against
  # a ceiling of 2/second cannot all fit in one window on any plausible machine, so
  # "no refusal at all" means the bucket is genuinely per address.
  global_refused=0
  for i in $(seq 1 10); do
    code=$(curl -s -o /dev/null -w '%{http_code}' -H "X-Forwarded-For: 198.51.100.$i" \
      "$GLOBAL_BASE$URL_PREFIX/ai.md")
    [ "$code" = "429" ] && global_refused=$((global_refused + 1))
  done
  if [ "$global_refused" -gt 0 ]; then
    printf 'PASS  the global bucket is shared across addresses (%s of 10 refused with a ceiling of 2)\n' "$global_refused"
  else
    printf 'FAIL  ten requests from ten addresses were all allowed with GLOBAL_RATE_LIMIT=2;\n'
    printf '      the global bucket is keyed per address, so it cannot bound a caller with many addresses\n'
    abuse_fail=1
  fi

  # The health probe must never be refused by the global bucket, or the liveness
  # signal becomes a way to look dead — and, since the limit state is in-process, a
  # way to get the counters cleared by a supervisor restart.
  #
  # The ceiling is re-exhausted immediately before probing, so "health was served"
  # cannot be explained by the window having drained in the meantime. If /api/health
  # were NOT exempt this would be several 429s.
  for i in 1 2 3 4; do
    curl -s -o /dev/null -H "X-Forwarded-For: 192.0.2.77" "$GLOBAL_BASE$URL_PREFIX/ai.md"
  done
  health_refused=0
  for i in $(seq 1 12); do
    code=$(curl -s -o /dev/null -w '%{http_code}' -H "X-Forwarded-For: 192.0.2.99" \
      "$GLOBAL_BASE$URL_PREFIX/api/health")
    [ "$code" = "429" ] && health_refused=$((health_refused + 1))
  done
  if [ "$health_refused" = 0 ]; then
    echo "PASS  the health probe is exempt from the global ceiling"
  else
    printf 'FAIL  /api/health was rate limited %s time(s); a liveness probe that answers 429\n' "$health_refused"
    printf '      reads as "down" to its poller, and a restart would clear every window and ban\n'
    abuse_fail=1
  fi

  if [ "$abuse_fail" != 0 ]; then
    echo "--- server log ---"; tail -20 "$ABUSE_LOG"
    die "the abuse-control checks failed (full log: ${ABUSE_LOG#$REPO/})"
  fi
  echo "----- abuse controls: all checks passed -----"
fi

say "P4 PASSED — the assembled binary serves the contract and renders in a browser"
echo "  database: $DBNAME.$SCHEMA (the schema is dropped on exit)"
echo "  log:      ${LOG#$REPO/}"
echo "  artifacts: bin/e2e/"
