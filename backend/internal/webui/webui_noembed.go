//go:build !embed_frontend

package webui

import "io/fs"

// FS reports that no frontend is embedded in this build.
//
// This is the default so that `go build`, `go test` and `go vet` work in a tree
// where the frontend has not been built. Release builds pass the
// `embed_frontend` tag, which replaces this with the real assets.
func FS() (fs.FS, bool) { return nil, false }
