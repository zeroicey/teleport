#!/usr/bin/env bash
#
# Build the frontend, then compile it into a single self-contained Go binary.
#
# This is THE release build. It produces one file that contains the Go server,
# the SQLite driver and the entire built frontend, so deploying is: copy the
# file, restart the unit. Nothing has to be staged alongside the binary, and the
# assets a running process serves can never drift from the code it is running.
#
# Order matters: Vite writes into backend/internal/webui/dist/, which is the
# `//go:embed` target, so the frontend must exist before `go build` runs. The
# `embed_frontend` tag is what switches webui from the "no frontend" stub to the
# real embedded filesystem.
#
# Usage: scripts/build.sh [output-path]
#        Default output: bin/teleport-linux-amd64
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${1:-$REPO/bin/teleport-linux-amd64}"

# `git describe --always --dirty` gives e.g. "278faa5" or "278faa5-dirty",
# which is exactly the granularity wanted: a short SHA, plus a marker when the
# working tree has uncommitted changes (so nobody mistakes a dirty build for a
# clean one).
VERSION="$(git -C "$REPO" describe --tags --always --dirty 2>/dev/null || echo unknown)"

echo "==> building frontend"
cd "$REPO"
pnpm run --silent build

echo
echo "==> compiling backend (with embedded frontend)"
mkdir -p "$(dirname "$OUT")"
cd "$REPO/backend"

# CGO_ENABLED=0 matters: it is what makes the result a static executable with no
# libc dependency, so it runs on the server regardless of its glibc version.
# (The SQLite driver is `modernc.org/sqlite` — pure Go — precisely so cgo can
# stay off.)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -trimpath \
  -tags embed_frontend \
  -ldflags "-s -w -X main.version=$VERSION" \
  -o "$OUT" \
  .

echo "  version: $VERSION"
echo "  size:    $(du -h "$OUT" | cut -f1)"
echo "  sha256:  $(sha256sum "$OUT" | cut -d' ' -f1)"
echo
echo "  verify:  $("$OUT" version)"
echo "  frontend: $("$OUT" version --frontend 2>/dev/null || echo 'embedded')"
