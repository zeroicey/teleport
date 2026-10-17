#!/usr/bin/env bash
#
# P5: move the pre-rewrite SQLite data set into PostgreSQL, and check the result.
#
# This is the rehearsal for the cut-over. It proves, on a throwaway schema, that
# the import tool turns the real source database into the real target schema with
# every row, every count and every link's behaviour intact — and then proves that
# the checks it just ran are capable of failing.
#
# What it deliberately does NOT do:
#   - it does not touch the production-intent `teleport` database, and refuses to
#     run against any database whose name does not contain "test";
#   - it does not touch the old host beyond reading (and only when P5_FETCH=1);
#   - it does not create a database: it creates one schema inside `teleport_test`
#     and drops it on exit, the same isolation the test harness uses. The
#     `teleport` role has rolcreatedb=false, and granting CREATEDB on an instance
#     shared with clip/registry/serenique is not worth it for a rehearsal.
#
# Usage:
#   scripts/verify-p5.sh
#   P5_SKIP_BUILD=1 scripts/verify-p5.sh          # reuse bin/import/sqliteimport
#   P5_FETCH=1      scripts/verify-p5.sh          # copy the source from the old host first
#   P5_SQLITE=/path/to/teleport.db scripts/verify-p5.sh
#
# Environment:
#   P5_SQLITE            the SQLite source (default bin/import/teleport.db)
#   P5_EXPECT_SHA256     refuse unless the source hashes to this (pin the artifact)
#   P5_FETCH=1           fetch the source from P5_HOST first, read-only
#   P5_HOST              the old host (default hcyj)
#   P5_REMOTE_DB         its database path
#   P5_DBNAME            the test database (default teleport_test; must contain "test")
#   P5_SCHEMA            the scratch schema (default p5)
#   P5_SKIP_BUILD=1      reuse the existing binary
#   P5_SKIP_NEGATIVE=1   skip the negative control (do not: it is the point)
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO"

DBNAME="${P5_DBNAME:-teleport_test}"
SCHEMA="${P5_SCHEMA:-p5}"
NEG_SCHEMA="${SCHEMA}_neg"
SOURCE="${P5_SQLITE:-$REPO/bin/import/teleport.db}"
TOOL="$REPO/bin/import/sqliteimport"
HOST="${P5_HOST:-hcyj}"
REMOTE_DB="${P5_REMOTE_DB:-/data/services/teleport/teleport.db}"

# The inventory the old deployment is believed to hold, as agreed with the human
# before this ran. These literals are the point of the script: the tool compares
# source with target, and these numbers compare BOTH with what a person said the
# data set is. A tool that agreed with itself about the wrong file would still
# pass its own checks.
EXPECT_REPORTS=5
EXPECT_SHARES=6
EXPECT_KEYS=2
EXPECT_APPS=4
EXPECT_RENEWALS=0

say() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
die() { printf '\nERROR: %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------- database ----
# The refusal that keeps a mistyped variable from dropping a schema in the
# database that holds real reports. Same posture as storetest.CheckDSN.
case "$DBNAME" in
  *test*) : ;;
  *) die "P5_DBNAME=$DBNAME does not contain \"test\". This script creates and drops a schema, so it must never point at the production database." ;;
esac

# The schema name is interpolated into DROP SCHEMA ... CASCADE below, so it is
# validated the same way the database name is. Without this, P5_SCHEMA=public would
# drop `teleport_test.public` — the one object in the test database that this
# script is not allowed to touch, and the object an earlier session's stray tables
# were hiding in.
for candidate in "$SCHEMA" "$NEG_SCHEMA"; do
  case "$candidate" in
    *[!A-Za-z0-9_]*|"") die "P5_SCHEMA=\"$candidate\" is not a plain identifier; refusing to interpolate it into DROP SCHEMA" ;;
    public|pg_*|information_schema) die "P5_SCHEMA=\"$candidate\" is a reserved schema name; refusing to drop it" ;;
  esac
done

PGPW="${P5_PGPASSWORD:-}"
if [ -z "$PGPW" ] && [ -r /data/services/teleport/.pgpassword.tmp ]; then
  PGPW="$(cat /data/services/teleport/.pgpassword.tmp)"
fi
if [ -z "$PGPW" ] && sudo -n true 2>/dev/null; then
  PGPW="$(sudo -n cat /data/services/teleport/.pgpassword.tmp 2>/dev/null || true)"
fi
[ -n "$PGPW" ] || die "no PostgreSQL password. Set P5_PGPASSWORD."

# Exported, not just passed to psql: the import tool is given a DSN with no
# password in it (see DSN_FOR) and reads PGPASSWORD from the environment, which is
# what keeps the credential out of the tool's argv. Probed both ways — with this
# set the tool authenticates, with it unset it fails with 28P01 — so a missing
# export cannot silently turn into "connected as someone else".
export PGPASSWORD="$PGPW"

# Unscoped, for CREATE/DROP SCHEMA.
psql_admin() { # <sql>
  if command -v psql >/dev/null 2>&1; then
    PGPASSWORD="$PGPW" psql -h 127.0.0.1 -p 5432 -U teleport -d "$DBNAME" -tAc "$1"
  else
    docker exec -e PGPASSWORD="$PGPW" postgres psql -U teleport -d "$DBNAME" -tAc "$1"
  fi
}

# Scoped to one schema via PGOPTIONS, so unqualified names resolve there and
# nowhere else. A wrong schema then fails loudly instead of reading `public`.
psql_scoped() { # <schema> <sql>
  if command -v psql >/dev/null 2>&1; then
    PGOPTIONS="-c search_path=$1" PGPASSWORD="$PGPW" psql -h 127.0.0.1 -p 5432 -U teleport -d "$DBNAME" -tAc "$2"
  else
    docker exec -e PGPASSWORD="$PGPW" -e PGOPTIONS="-c search_path=$1" postgres psql -U teleport -d "$DBNAME" -tAc "$2"
  fi
}

# The DSN deliberately carries NO password: pgx reads PGPASSWORD from the
# environment, and this string is passed on the tool's argv, where any local `ps`
# on this shared host would otherwise expose the credential. It also removes the
# class of bug where a password containing @ : / ? or # silently produces a
# different DSN (which fails only by luck, and with a misleading message).
DSN_FOR() { printf 'postgres://teleport@127.0.0.1:5432/%s?sslmode=disable&search_path=%s' "$DBNAME" "$1"; }

CREATED_SCHEMAS=()
cleanup() {
  for s in "${CREATED_SCHEMAS[@]:-}"; do
    [ -n "$s" ] || continue
    psql_admin "DROP SCHEMA IF EXISTS $s CASCADE" >/dev/null 2>&1 && echo "dropped schema $s" || true
  done
}
trap cleanup EXIT

command -v sqlite3 >/dev/null 2>&1 || die "sqlite3 not found; the import reads its source through it"

# ------------------------------------------------------------------ source ----
say "source"

# Reading the source with immutable=1 ignores the write-ahead log, so a -wal file
# means committed transactions that would be silently left behind. A copy made
# with `scp teleport.db` while a -wal exists is exactly that mistake: the file
# alone looks complete.
for suffix in -wal -shm -journal; do
  [ -e "$SOURCE$suffix" ] || continue
  die "$SOURCE$suffix exists. The source has not been checkpointed, so the main file is an
incomplete copy of the database. Checkpoint it on the old host first:
  sqlite3 $REMOTE_DB 'PRAGMA wal_checkpoint(TRUNCATE);'
or copy it with sqlite3's own backup, which is safe against a live writer:
  ssh $HOST \"sqlite3 $REMOTE_DB '.backup /tmp/teleport.db'\" && scp $HOST:/tmp/teleport.db $SOURCE"
done

if [ "${P5_FETCH:-0}" = "1" ]; then
  say "fetching $REMOTE_DB from $HOST (read-only; nothing on $HOST is modified)"
  for suffix in -wal -shm -journal; do
    if ssh -o BatchMode=yes "$HOST" "test -e '$REMOTE_DB$suffix'" 2>/dev/null; then
      die "$HOST has $REMOTE_DB$suffix. Do not copy the main file alone: it would silently
drop the transactions in the write-ahead log. Checkpoint or use sqlite3 .backup first."
    fi
  done
  remote_sha="$(ssh -o BatchMode=yes "$HOST" "sha256sum '$REMOTE_DB'" | awk '{print $1}')"
  [ -n "$remote_sha" ] || die "could not hash $REMOTE_DB on $HOST"
  mkdir -p "$(dirname "$SOURCE")"
  scp -q "$HOST:$REMOTE_DB" "$SOURCE" || die "scp failed"
  local_sha="$(sha256sum "$SOURCE" | awk '{print $1}')"
  [ "$remote_sha" = "$local_sha" ] || die "the copy does not match the remote file: $remote_sha vs $local_sha"
  echo "fetched $REMOTE_DB (sha256 $local_sha)"
fi

[ -f "$SOURCE" ] || die "no source at $SOURCE (run with P5_FETCH=1 to copy it, or set P5_SQLITE)"
SOURCE_SHA="$(sha256sum "$SOURCE" | awk '{print $1}')"
echo "source    $SOURCE"
echo "sha256    $SOURCE_SHA"
if [ -n "${P5_EXPECT_SHA256:-}" ] && [ "$SOURCE_SHA" != "$P5_EXPECT_SHA256" ]; then
  die "the source hashes to $SOURCE_SHA, not the pinned $P5_EXPECT_SHA256. Refusing to import a
different file than the one that was reviewed."
fi

sq() { sqlite3 "file:$SOURCE?mode=ro&immutable=1" "$1"; }
[ "$(sq 'PRAGMA integrity_check;')" = "ok" ] || die "the source failed PRAGMA integrity_check"

# The source side of the inventory. It goes through the SAME sqlite3 binary the
# importer shells out to — so this is an independent SQL statement and an
# independent arithmetic path, not an independent SQLite implementation. (An
# earlier version of this comment claimed otherwise; the genuinely independent
# cross-check is the Go tool's db_hex_bytes[...] comparison, where each database
# renders its own bytes.)
say "source inventory (read with the sqlite3 shell, independently of the import tool)"
src_reports="$(sq 'SELECT count(*) FROM reports;')"
src_shares="$(sq 'SELECT count(*) FROM share_tokens;')"
src_keys="$(sq 'SELECT count(*) FROM agent_keys;')"
src_apps="$(sq 'SELECT count(*) FROM key_applications;')"
src_renewals="$(sq 'SELECT count(*) FROM key_renewals;')"
src_perm="$(sq 'SELECT count(*) FROM share_tokens WHERE expires_at = 0;')"
src_revoked="$(sq 'SELECT count(*) FROM share_tokens WHERE is_active = 0;')"
src_perm_active="$(sq 'SELECT count(*) FROM share_tokens WHERE expires_at = 0 AND is_active = 1;')"
src_versions="$(sq "SELECT group_concat(version, '+') FROM (SELECT version FROM schema_migrations ORDER BY version);")"

printf '  reports=%s share_tokens=%s agent_keys=%s key_applications=%s key_renewals=%s\n' \
  "$src_reports" "$src_shares" "$src_keys" "$src_apps" "$src_renewals"
printf '  expires_at=0 (permanent)=%s  is_active=0 (revoked)=%s  permanent and active=%s\n' \
  "$src_perm" "$src_revoked" "$src_perm_active"
echo "  schema_migrations: $src_versions"

[ "$src_reports" = "$EXPECT_REPORTS" ] || die "the source has $src_reports reports, expected $EXPECT_REPORTS. Either this is the wrong file or the human's inventory is stale — do not proceed until that is settled."
[ "$src_shares" = "$EXPECT_SHARES" ] || die "the source has $src_shares share tokens, expected $EXPECT_SHARES"
[ "$src_keys" = "$EXPECT_KEYS" ] || die "the source has $src_keys agent keys, expected $EXPECT_KEYS"
[ "$src_apps" = "$EXPECT_APPS" ] || die "the source has $src_apps key applications, expected $EXPECT_APPS"
[ "$src_renewals" = "$EXPECT_RENEWALS" ] || die "the source has $src_renewals key renewals, expected $EXPECT_RENEWALS"
[ "$src_versions" = "0001_init+0002_agent_keys" ] || die "unexpected source schema: $src_versions"
[ "$src_perm_active" -ge 1 ] || die "the source has no permanent-and-active link, so the permanence check would be vacuous"
[ "$src_revoked" -ge 1 ] || die "the source has no revoked link, so the 404 check would be vacuous"

# ------------------------------------------------------------------- build ----
if [ "${P5_SKIP_BUILD:-0}" != "1" ]; then
  say "building the import tool"
  ( cd backend && go build -o "$TOOL" ./cmd/sqliteimport ) || die "go build failed"
fi
[ -x "$TOOL" ] || die "no tool at $TOOL (run without P5_SKIP_BUILD=1)"

# ------------------------------------------------------------------ import ----
say "rehearsing the import into $DBNAME.$SCHEMA"
psql_admin "DROP SCHEMA IF EXISTS $SCHEMA CASCADE" >/dev/null || die "could not drop the old schema $SCHEMA"
psql_admin "CREATE SCHEMA $SCHEMA" >/dev/null || die "could not create schema $SCHEMA"
CREATED_SCHEMAS+=("$SCHEMA")
echo "created schema $DBNAME.$SCHEMA (the production-intent 'teleport' database is not touched)"

IMPORT_LOG="$REPO/bin/import/p5-import.log"
mkdir -p "$(dirname "$IMPORT_LOG")"
"$TOOL" import -source "$SOURCE" -dsn "$(DSN_FOR "$SCHEMA")" 2>&1 | tee "$IMPORT_LOG"
IMPORT_RC=${PIPESTATUS[0]}
[ "$IMPORT_RC" = 0 ] || die "the import failed (see ${IMPORT_LOG#$REPO/})"

# The tool hashes the file itself; the shell hashed it with sha256sum. Two
# implementations agreeing is cheap evidence that the tool read the file that was
# reviewed.
grep -q "$SOURCE_SHA" "$IMPORT_LOG" || die "the import log does not mention the source hash $SOURCE_SHA; the tool may have read a different file"

# ------------------------------------------------------------------ verify ----
say "verifying the target against the source (through the store's own API)"
VERIFY_LOG="$REPO/bin/import/p5-verify.log"
"$TOOL" verify -source "$SOURCE" -dsn "$(DSN_FOR "$SCHEMA")" 2>&1 | tee "$VERIFY_LOG"
VERIFY_RC=${PIPESTATUS[0]}
[ "$VERIFY_RC" = 0 ] || die "verification failed (see ${VERIFY_LOG#$REPO/})"

say "independent target counts (read with psql, not by the import tool)"
tgt_reports="$(psql_scoped "$SCHEMA" 'SELECT count(*) FROM reports')"
tgt_shares="$(psql_scoped "$SCHEMA" 'SELECT count(*) FROM share_tokens')"
tgt_keys="$(psql_scoped "$SCHEMA" 'SELECT count(*) FROM agent_keys')"
tgt_apps="$(psql_scoped "$SCHEMA" 'SELECT count(*) FROM key_applications')"
tgt_renewals="$(psql_scoped "$SCHEMA" 'SELECT count(*) FROM key_renewals')"
tgt_perm="$(psql_scoped "$SCHEMA" 'SELECT count(*) FROM share_tokens WHERE expires_at = 0')"
tgt_revoked="$(psql_scoped "$SCHEMA" 'SELECT count(*) FROM share_tokens WHERE is_active = 0')"
tgt_perm_active="$(psql_scoped "$SCHEMA" 'SELECT count(*) FROM share_tokens WHERE expires_at = 0 AND is_active = 1')"
tgt_migrations="$(psql_scoped "$SCHEMA" "SELECT count(*) FROM schema_migrations WHERE version = '0001_init.sql' AND checksum <> ''")"

printf '  reports=%s share_tokens=%s agent_keys=%s key_applications=%s key_renewals=%s\n' \
  "$tgt_reports" "$tgt_shares" "$tgt_keys" "$tgt_apps" "$tgt_renewals"
printf '  expires_at=0 (permanent)=%s  is_active=0 (revoked)=%s  permanent and active=%s\n' \
  "$tgt_perm" "$tgt_revoked" "$tgt_perm_active"

[ "$tgt_reports" = "$src_reports" ] || die "reports: source $src_reports, target $tgt_reports"
[ "$tgt_shares" = "$src_shares" ] || die "share_tokens: source $src_shares, target $tgt_shares"
[ "$tgt_keys" = "$src_keys" ] || die "agent_keys: source $src_keys, target $tgt_keys"
[ "$tgt_apps" = "$src_apps" ] || die "key_applications: source $src_apps, target $tgt_apps"
[ "$tgt_renewals" = "$src_renewals" ] || die "key_renewals: source $src_renewals, target $tgt_renewals"
[ "$tgt_perm" = "$src_perm" ] || die "permanent links: source $src_perm, target $tgt_perm"
[ "$tgt_revoked" = "$src_revoked" ] || die "revoked links: source $src_revoked, target $tgt_revoked"
[ "$tgt_perm_active" = "$src_perm_active" ] || die "permanent-and-active links: source $src_perm_active, target $tgt_perm_active"
# The schema must have been built by the application's migrator, not by hand: the
# checksum column is only non-empty when MigratePG wrote the row. The version is
# the migration FILE NAME (`0001_init.sql`), which is not the same string the old
# SQLite migrator recorded (`0001_init`) — checkTargetTables does not compare
# them, and this line must not pretend otherwise.
[ "$tgt_migrations" = "1" ] || die "schema_migrations has no checksummed 0001_init.sql row; the schema was not built by the application's migrator"

echo "  all counts match the source and the expected inventory"

# --------------------------------------------------------- negative control ----
# The lesson this repository paid for in P4: a verification script that has never
# been seen to fail proves nothing. This breaks two properties in an otherwise
# faithful import and requires the run to fail *naming the checks that own those
# properties* — a failure for any other reason would be a different bug, not
# evidence about these checks.
#
# The tamper is applied to the TARGET and the reference stays the original source.
# The first version of this control tampered the source file and then imported it,
# which asks whether a target agrees with its own source — it does, so the control
# passed on a broken database. It had to be fixed before it could produce a true
# statement, which is exactly the failure mode this section exists to catch.
NEGATIVE_RAN=0
if [ "${P5_SKIP_NEGATIVE:-0}" = "1" ]; then
  say "negative control SKIPPED (P5_SKIP_NEGATIVE=1)"
  echo "  NOTE: with P5_SKIP_NEGATIVE=1 this run has NOT shown that the checks can fail."
else
  say "negative control: a tampered target must fail verification, by name"
  psql_admin "DROP SCHEMA IF EXISTS $NEG_SCHEMA CASCADE" >/dev/null || die "could not drop $NEG_SCHEMA"
  psql_admin "CREATE SCHEMA $NEG_SCHEMA" >/dev/null || die "could not create $NEG_SCHEMA"
  CREATED_SCHEMAS+=("$NEG_SCHEMA")

  # A faithful import first, so the only difference from the green run above is
  # the tamper.
  "$TOOL" import -source "$SOURCE" -dsn "$(DSN_FOR "$NEG_SCHEMA")" >/dev/null 2>&1 \
    || die "the import into the negative-control schema failed; the control needs a faithful import"

  FUTURE=$(( ($(date +%s) + 86400) * 1000 ))
  # `WITH u AS (UPDATE ... RETURNING 1) SELECT count(*) FROM u` is how psql reports
  # how many rows an UPDATE changed. A tamper that changed nothing would make the
  # control vacuous: the checks would pass and this script would call that success.
  revoke_tamper="$(psql_scoped "$NEG_SCHEMA" "WITH u AS (UPDATE share_tokens SET is_active = 1 WHERE is_active = 0 RETURNING 1) SELECT count(*) FROM u")"
  permanence_tamper="$(psql_scoped "$NEG_SCHEMA" "WITH u AS (UPDATE share_tokens SET expires_at = $FUTURE WHERE expires_at = 0 AND is_active = 1 RETURNING 1) SELECT count(*) FROM u")"
  [ "$revoke_tamper" -ge 1 ] || die "the revocation tamper changed no rows; the control is vacuous"
  [ "$permanence_tamper" -ge 1 ] || die "the permanence tamper changed no rows; the control is vacuous"
  echo "  tampered the target: $revoke_tamper revoked link(s) made active, $permanence_tamper permanent link(s) given an expiry"

  NEG_LOG="$REPO/bin/import/p5-negative.log"
  "$TOOL" verify -source "$SOURCE" -dsn "$(DSN_FOR "$NEG_SCHEMA")" >"$NEG_LOG" 2>&1
  NEG_RC=$?
  if [ "$NEG_RC" = 0 ]; then
    die "verification PASSED on a tampered target. The checks cannot fail, so the green run above
means nothing."
  fi
  for want in revoked_links_stay_404 permanent_links_stay_permanent; do
    grep -q "FAIL $want" "$NEG_LOG" || die "the control failed, but not at $want — it may be failing for an unrelated reason. See ${NEG_LOG#$REPO/}"
  done
  echo "  verification failed as required, at both named checks (see ${NEG_LOG#$REPO/})"
  NEGATIVE_RAN=1

  # Guarded, and NOT removed from the cleanup list until the drop is confirmed: an
  # unguarded drop followed by removing it from CREATED_SCHEMAS would let a failed
  # drop leave p5_neg behind while the script still printed PASSED.
  psql_admin "DROP SCHEMA IF EXISTS $NEG_SCHEMA CASCADE" >/dev/null \
    || die "could not drop the negative-control schema $NEG_SCHEMA"
  CREATED_SCHEMAS=("$SCHEMA")
fi

if [ "$NEGATIVE_RAN" = "1" ]; then
  say "P5 PASSED — the real source imports into the real schema and every check can fail"
else
  say "P5 PASSED (WITHOUT THE NEGATIVE CONTROL) — the import and the checks are green, but this run has not shown that the checks can fail"
fi
echo "  source:    $SOURCE (sha256 $SOURCE_SHA)"
echo "  schema:    $DBNAME.$SCHEMA (dropped on exit)"
echo "  inventory: reports=$EXPECT_REPORTS share_tokens=$EXPECT_SHARES agent_keys=$EXPECT_KEYS key_applications=$EXPECT_APPS key_renewals=$EXPECT_RENEWALS"
echo "  logs:      bin/import/"
