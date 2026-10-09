// Package webui exposes the built frontend as a filesystem.
//
// The frontend is normally embedded into the binary so a deployment is a single
// self-contained file: copy it to the server, restart the unit, done. Nothing
// has to be kept in sync on disk next to the binary, and there is no way for
// the running assets to drift from the running code.
//
// Embedding is opt-in via the `embed_frontend` build tag, for one reason:
// `go build`, `go test` and `go vet` must keep working in a tree where the
// frontend has not been built. Without the tag the package reports "no
// frontend" and the server runs API-only. The release build —
// `pnpm run build:release`, i.e. `scripts/build.sh` — builds the frontend into
// `internal/webui/dist/` and then compiles this package with that tag.
package webui
