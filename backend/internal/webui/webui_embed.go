//go:build embed_frontend

package webui

import (
	"embed"
	"io/fs"
)

// The frontend build is emitted here by `pnpm run build` (Vite writes straight
// into dist/ — see vite.config.ts) before this package is compiled with the
// `embed_frontend` tag by `scripts/build.sh`.
//
// `all:` is required because Vite emits dotfiles and underscore-prefixed files
// (`_headers`) that a bare `embed` pattern would skip.
//
//go:embed all:dist
var embedded embed.FS

// FS returns the embedded frontend build, rooted at the build directory so
// callers see "index.html" and "assets/..." rather than "dist/...".
func FS() (fs.FS, bool) {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		return nil, false
	}
	return sub, true
}
