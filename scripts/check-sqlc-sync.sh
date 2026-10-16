#!/usr/bin/env bash
#
# Fail if the sqlc-generated code no longer matches the SQL it came from.
#
# The generated package (backend/internal/store/storedb) is checked into the
# repository so that a plain `go build` works on a machine with no code
# generator installed. That only stays honest if drift is caught mechanically,
# which is what `sqlc diff` does: it regenerates in memory and reports any
# difference instead of writing it.
#
# This is the store's counterpart to the schema-snapshot check in check.sh. Both
# exist for the same reason: the SQL is the source of truth, and a checked-in
# artifact that has quietly fallen behind it is a lie the next reader believes.
#
# Unlike the schema check, this one needs the sqlc binary. It fails loudly with
# the install command rather than skipping — a gate that silently skips is a gate
# that never fails.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if ! command -v sqlc >/dev/null 2>&1; then
  cat >&2 <<'EOF'
sqlc is not installed, so generated-code drift cannot be checked.

  go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1

(With GOPROXY=https://goproxy.cn,direct on a mainland network.)
EOF
  exit 1
fi

# sqlc.yaml lives beside the Go module (backend/), because its schema/queries/out
# paths are relative to it.
cd "$REPO/backend"
if ! out="$(sqlc diff 2>&1)"; then
  echo "sqlc: generated code is out of date (run: pnpm run db:gen-sqlc)" >&2
  echo "$out" >&2
  exit 1
fi
echo "sqlc: generated code matches the queries"
