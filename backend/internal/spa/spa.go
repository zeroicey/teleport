// Package spa serves the built frontend as static files.
//
// Why this exists at all: the frontend used to be deployed to Cloudflare
// Workers Assets, with a Worker reverse-proxying /api and /s back to this
// backend. That topology is unreachable from mainland China, because the
// anycast IPv4 addresses Cloudflare assigns to a free-plan zone are
// TCP-blocked there (DNS resolves fine, ICMP replies, 443 never opens).
// Serving the same static build from this host removes the blocked hop
// entirely, and the host is already reachable — it is the same origin that
// answers the API.
//
// The handler is mounted under the route prefix, so the browser sees one
// origin for everything (shell, assets, API, share pages). That keeps the
// session cookie first-party and lets the share page's `script-src 'self'`
// policy stay meaningful.
//
// The filesystem is an `fs.FS` rather than a directory path so the same code
// serves either an embedded build (the normal deployment: one self-contained
// binary) or a directory (for swapping assets without a rebuild).
package spa

import (
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

// indexFile is the SPA shell served for client-side routes.
const indexFile = "index.html"

// assetsDir is the directory Vite writes content-hashed files into.
const assetsDir = "assets"

// Handler serves a Vite build at `prefix`.
//
// Routing rules, in order:
//
//  1. Anything under /assets/ is served strictly from the build. A miss is a
//     real 404 — never the SPA shell. Vite content-hashes these names, so a
//     missing one means a stale page referencing a deleted build, and answering
//     with HTML would turn a clear 404 into a confusing MIME-type error.
//  2. An existing file is served as-is (favicon, robots.txt, ...).
//  3. /s/ and /api/ are NOT handled here — the router registers those as more
//     specific patterns, which win over this prefix catch-all. This guard is a
//     safety net in case that wiring ever changes: returning a JSON 404 for an
//     API path beats silently returning an HTML page to an XHR client.
//  4. Everything else falls back to index.html so client-side routes such as
//     /dashboard/reports/<id> survive a hard refresh.
type Handler struct {
	fsys   fs.FS
	source string
	prefix string
}

// New builds a handler over a frontend build filesystem mounted at prefix
// (which must be "" or start with "/" and not end with one).
//
// A build without index.html is a startup error rather than a silent
// degradation: a backend that answers 404 for the SPA shell while the API works
// is far more confusing to diagnose than one that refuses to boot.
func New(fsys fs.FS, prefix, source string) (*Handler, error) {
	if fsys == nil {
		return nil, fs.ErrNotExist
	}
	if _, err := fs.Stat(fsys, indexFile); err != nil {
		return nil, err
	}
	return &Handler{fsys: fsys, source: source, prefix: prefix}, nil
}

// NewDir builds a handler over a build directory on disk.
func NewDir(dir, prefix string) (*Handler, error) {
	if dir == "" {
		return nil, fs.ErrNotExist
	}
	// Reject a directory that is not there up front, so the error names the
	// real problem rather than a missing index.html inside it.
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, &fs.PathError{Op: "open", Path: dir, Err: fs.ErrInvalid}
	}
	return New(os.DirFS(dir), prefix, dir)
}

// Source describes where the build is being served from, for startup logging.
func (h *Handler) Source() string { return h.source }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Strip the mount prefix to get a path relative to the build root.
	rel := strings.TrimPrefix(r.URL.Path, h.prefix)
	rel = strings.TrimPrefix(rel, "/")
	// `path.Clean` collapses any "..", so a traversal attempt can only ever
	// resolve inside the build. This is belt-and-braces (`net/http` already
	// normalises the path) but cheap and explicit. `fs.FS` rejects traversal
	// itself too, so the control is layered.
	rel = strings.TrimPrefix(path.Clean("/"+rel), "/")

	// 1. Hashed assets: exact matches only.
	if rel == assetsDir || strings.HasPrefix(rel, assetsDir+"/") {
		if h.serveFile(w, r, rel, true) {
			return
		}
		http.NotFound(w, r)
		return
	}

	// 2. A real file in the build (favicon.ico, robots.txt, ...).
	if rel != "" && h.serveFile(w, r, rel, false) {
		return
	}

	// 3. Never let an API or share path fall through to the SPA shell.
	if strings.HasPrefix(rel, "api/") || strings.HasPrefix(rel, "s/") {
		http.NotFound(w, r)
		return
	}

	// 4. Client-side route: hand back the shell, uncached.
	h.serveIndex(w, r)
}

// serveFile writes the named file, reporting whether it existed.
func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request, name string, immutable bool) bool {
	info, err := fs.Stat(h.fsys, name)
	if err != nil || info.IsDir() {
		return false
	}

	f, err := h.fsys.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()

	// Vite content-hashes filenames under /assets/, so a given URL's bytes can
	// never change: cache it for a year. index.html and other stable names get
	// revalidated instead, so a deploy is picked up immediately.
	//
	// Deployments swap the whole binary, so the "changed bytes at a stable URL"
	// window is the same one a no-cache header exists to cover.
	if immutable {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}

	// ServeContent handles Content-Type detection, Range requests and HEAD.
	// It needs a ReadSeeker; both embed.FS and os.DirFS provide one, but fall
	// back to a buffered read so the handler never depends on that.
	if rs, ok := f.(io.ReadSeeker); ok {
		http.ServeContent(w, r, name, info.ModTime(), rs)
		return true
	}
	body, err := io.ReadAll(f)
	if err != nil {
		return false
	}
	http.ServeContent(w, r, name, info.ModTime(), strings.NewReader(string(body)))
	return true
}

// serveIndex writes the SPA shell.
func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	f, err := h.fsys.Open(indexFile)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	// Never cache the shell: it references content-hashed bundles, so a cached
	// copy would pin a client to a build whose files may already be gone.
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	body, err := io.ReadAll(f)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}
