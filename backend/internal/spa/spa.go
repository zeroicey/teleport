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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// indexFile is the SPA shell served for client-side routes.
const indexFile = "index.html"

// assetsDir is the directory Vite writes content-hashed files into.
const assetsDir = "assets"

// fontsDir holds the self-hosted webfonts, copied verbatim from the frontend's
// `public/` directory.
//
// It needs its own rule because `public/` files never pass through Vite: their
// names are chosen by `scripts/fonts/fetch_fonts.py`, not by the bundler. That
// script puts a content hash in every binary's filename precisely so the files
// can earn the immutable cache below (see isContentHashed). `fonts/fonts.css`
// deliberately does not, and therefore revalidates.
const fontsDir = "fonts"

// stableAssetFiles names the non-hashed files under assetsDir, for the test
// that pins this list to the filename the build and the share page agree on.
// It is documentation and an assertion target, not the cache decision itself —
// isContentHashed keys on the name's shape, so a name missing from here is
// uncached rather than incorrectly immortalised.
var stableAssetFiles = map[string]struct{}{
	assetsDir + "/share.js": {},
}

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

	// ETag cache for non-content-addressed files. Guarded because a single
	// Handler serves concurrent requests.
	etagOnce sync.Once
	etagMu   sync.Mutex
	etags    map[string]string

	// securityHeaders are applied to every response this handler writes, before
	// the handler's own Cache-Control/Content-Type work.
	//
	// They live here rather than in the transport chain because they constrain the
	// FRONTEND specifically: the share page already sets its own, much stricter
	// policy, and the `format: "html"` report must stay free of any policy at all.
	// A header set in the shared chain would hit all three.
	securityHeaders http.Header
}

// SetSecurityHeaders installs headers to add to every response this handler
// writes, replacing any previous set.
//
// The headers are applied lazily, at the moment the response is committed (see
// headerWriter), not when ServeHTTP is entered. That ordering is the whole
// point: a header set on entry would already be present when the handler set
// Content-Type, so the two would collide and the security header would win — a
// `text/plain` CSP on a JavaScript bundle. Applying at commit time means "fill
// in what nothing else set" is actually true.
func (h *Handler) SetSecurityHeaders(headers map[string]string) {
	hdr := make(http.Header, len(headers))
	for k, v := range headers {
		hdr.Set(k, v)
	}
	h.securityHeaders = hdr
}

// headerWriter commits the configured security headers just before the status
// line is written, skipping any key a handler in between already set.
//
// It must forward Flush, or a streaming response (http.ServeContent uses it for
// ranges) would stop flushing through this wrapper.
type headerWriter struct {
	http.ResponseWriter
	head      http.Header
	committed bool
}

func (w *headerWriter) commit() {
	if w.committed {
		return
	}
	w.committed = true
	dst := w.Header()
	for key, values := range w.head {
		if len(values) == 0 || dst.Get(key) != "" {
			continue
		}
		dst.Set(key, values[0])
	}
}

func (w *headerWriter) WriteHeader(status int) {
	w.commit()
	w.ResponseWriter.WriteHeader(status)
}

func (w *headerWriter) Write(p []byte) (int, error) {
	// A handler that writes without calling WriteHeader commits implicitly.
	if !w.committed {
		w.commit()
		w.ResponseWriter.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *headerWriter) Flush() {
	w.commit()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// applySecurityHeaders wraps w when any header is configured, and returns w
// unchanged otherwise so the unconfigured path costs nothing.
func (h *Handler) applySecurityHeaders(w http.ResponseWriter) http.ResponseWriter {
	if len(h.securityHeaders) == 0 {
		return w
	}
	return &headerWriter{ResponseWriter: w, head: h.securityHeaders}
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
	w = h.applySecurityHeaders(w)

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

	// 1. Assets: exact matches only, so a miss stays a hard 404 instead of
	// falling back to the shell (HTML for a missing script turns a clear 404
	// into a confusing MIME-type error).
	if rel == assetsDir || strings.HasPrefix(rel, assetsDir+"/") {
		if h.serveFile(w, r, rel, isContentHashed(rel)) {
			return
		}
		http.NotFound(w, r)
		return
	}

	// 2. A real file in the build (favicon.ico, robots.txt, fonts/*, ...).
	//
	// The cache decision is the same shape rule as for assets, not a hardcoded
	// "no": the self-hosted fonts live here (they come from `public/`, so Vite
	// never puts them under assets/) and are content-addressed by the vendoring
	// script precisely so they can be immutable. Leaving this `false` made every
	// page load re-download the font slices — and because embedded files report
	// a zero modtime, the revalidation could not even resolve to a 304.
	if rel != "" && h.serveFile(w, r, rel, isContentHashed(rel)) {
		return
	}

	// 3. Never let an API, share or key hand-off path fall through to the SPA
	// shell.
	//
	// The router registers these more specifically, so this is the second
	// layer: a path under one of these prefixes that no route matched is a
	// miss, not a client-side route. `/k/` earns a place here because a
	// truncated hand-off link must render an explanation rather than an app
	// shell that silently does nothing.
	if strings.HasPrefix(rel, "api/") || strings.HasPrefix(rel, "s/") ||
		strings.HasPrefix(rel, "k/") || rel == "k" {
		http.NotFound(w, r)
		return
	}

	// 4. Client-side route: hand back the shell, uncached.
	h.serveIndex(w, r)
}

// isContentHashed reports whether a build-relative path names a file whose URL
// is versioned by its own filename, making `immutable` safe.
//
// The default is "not safe": only names that carry a content hash earn the
// year-long cache. Everything else — including `assets/share.js`,
// `fonts/fonts.css` and any future stable asset — revalidates.
//
// This direction matters. The earlier version asked "is it under assets/?" and
// answered yes for the whole directory, which silently gave `immutable` to a
// stable name. Whitelisting the shape means a new stable file is uncached by
// default, and the failure mode of forgetting to update anything is a cheap
// revalidation rather than a year of stale bytes.
//
// Two directories, two producers, one rule:
//
//   - `assets/` — written by Vite as `[name]-[hash].js`.
//   - `fonts/`  — written by scripts/fonts/fetch_fonts.py as
//     `<slug>-<NNN>-<sha256[:8]>.woff2`. Public-directory files never reach the
//     bundler, so this rule is the only thing that keeps them from being
//     re-downloaded in full on every page load: embedded files report a zero
//     modtime, so `no-cache` cannot even resolve to a 304.
func isContentHashed(rel string) bool {
	var name string
	switch {
	case strings.HasPrefix(rel, assetsDir+"/"):
		name = strings.TrimPrefix(rel, assetsDir+"/")
	case strings.HasPrefix(rel, fontsDir+"/"):
		name = strings.TrimPrefix(rel, fontsDir+"/")
		// Fonts are only trusted when the digest is actually present in the
		// shape the fetch script guarantees. `fonts/fonts.css` has a hyphen-free
		// stem and so falls through to revalidation, which is what it wants.
		if !fontNameHasContentHash(name) {
			return false
		}
	default:
		return false
	}

	// The hash is base64url-ish and the stem may itself contain hyphens, so
	// anchor on the last hyphen before the extension.
	dot := strings.LastIndex(name, ".")
	if dot <= 0 {
		return false
	}
	stem := name[:dot]
	if strings.ContainsAny(stem, "/") {
		return false
	}
	return strings.Contains(stem, "-")
}

// fontNameHasContentHash reports whether a name under fontsDir matches the
// `<slug>-<NNN>-<8 hex>.woff2` form the vendoring script emits.
//
// Matching the digest rather than merely "contains a hyphen" is the point:
// `no-cache` is the safe failure, so an unrecognised font name must fall back to
// revalidation instead of being immortalised.
func fontNameHasContentHash(name string) bool {
	dot := strings.LastIndex(name, ".")
	if dot <= 0 || name[dot:] != ".woff2" {
		return false
	}
	parts := strings.Split(name[:dot], "-")
	if len(parts) < 3 {
		return false
	}
	digest := parts[len(parts)-1]
	if len(digest) != 8 {
		return false
	}
	if _, err := strconv.ParseUint(digest, 16, 32); err != nil {
		return false
	}
	index := parts[len(parts)-2]
	if len(index) != 3 {
		return false
	}
	_, err := strconv.Atoi(index)
	return err == nil
}

// etagFor returns a strong validator for a non-hashable build file, computing it
// on first use and caching it for the process lifetime.
//
// The build is embedded and immutable for the life of the binary, so one hash
// per file is enough: a deploy is a new process with a new cache. Computing
// lazily rather than at startup keeps `New` cheap and avoids hashing ~110 font
// files that no client will ever revalidate (those are content-addressed and
// carry `immutable`, so they never reach here).
func (h *Handler) etagFor(name string) (string, bool) {
	h.etagOnce.Do(func() { h.etags = map[string]string{} })

	h.etagMu.Lock()
	defer h.etagMu.Unlock()
	if tag, ok := h.etags[name]; ok {
		return tag, tag != ""
	}

	f, err := h.fsys.Open(name)
	if err != nil {
		h.etags[name] = ""
		return "", false
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		h.etags[name] = ""
		return "", false
	}
	tag := `"` + hex.EncodeToString(sum.Sum(nil)[:16]) + `"`
	h.etags[name] = tag
	return tag, true
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

	// A content-hashed URL's bytes can never change, so it is cached for a
	// year. Stable names (index.html, assets/share.js, fonts/fonts.css)
	// revalidate instead, so a deploy is picked up immediately.
	//
	// Deployments swap the whole binary, so the "changed bytes at a stable URL"
	// window is the same one a no-cache header exists to cover.
	if immutable {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
		// `no-cache` is only cheap if the revalidation can actually succeed.
		// Embedded files report a ZERO modtime, so ServeContent emits no
		// Last-Modified and the browser has no validator to send — which turned
		// every revalidation into a full re-download. That is tolerable for a
		// 1 KB favicon and unacceptable for `fonts/fonts.css`, which carries a
		// @font-face block per CJK slice and is ~107 KB.
		//
		// An ETag restores the intended behaviour: the browser revalidates, the
		// server answers 304, and the body is never sent again.
		if etag, ok := h.etagFor(name); ok {
			w.Header().Set("ETag", etag)
		}
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
	//
	// "Never cache" still means "revalidate", and the same zero-modtime problem
	// applies here as to fonts.css — so it carries a validator too, and a
	// repeat navigation costs a 304 instead of the whole document.
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if etag, ok := h.etagFor(indexFile); ok {
		w.Header().Set("ETag", etag)
	}

	body, err := io.ReadAll(f)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// ServeContent, not a bare Write: a handler that writes directly gets no
	// conditional-request handling from net/http, so the ETag set above would
	// be advertised and then ignored — the browser would revalidate, be handed
	// a 200 with the full document, and the validator would have bought
	// nothing. ServeContent compares If-None-Match and answers 304. It also
	// handles HEAD, which the previous explicit branch did by hand.
	http.ServeContent(w, r, indexFile, time.Time{}, bytes.NewReader(body))
}
