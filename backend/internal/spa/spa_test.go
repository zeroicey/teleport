package spa

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// buildFixture writes a minimal Vite-style build directory:
//
//	index.html               the SPA shell
//	assets/index-abc123.js   a content-hashed bundle
//	assets/share.js          the stable share entry
//	favicon.ico              a non-hashed real file
func buildFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatalf("mkdir assets: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "fonts"), 0o755); err != nil {
		t.Fatalf("mkdir fonts: %v", err)
	}
	files := map[string]string{
		"index.html":             `<!DOCTYPE html><div id="app"></div>`,
		"assets/index-abc123.js": `console.log("bundle")`,
		"assets/share.js":        `console.log("share")`,
		"favicon.ico":            "not-really-an-ico",
		// Content-addressed font (immutable) and the stable stylesheet that
		// references it (must revalidate).
		"fonts/outfit-001-6c18d579.woff2": "wOF2-fake-font-bytes",
		"fonts/fonts.css":                 `@font-face{src:url(./outfit-001-6c18d579.woff2)}`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func newTestHandler(t *testing.T, prefix string) *Handler {
	t.Helper()
	h, err := NewDir(buildFixture(t), prefix)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	return h
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// TestFontCacheControlThroughTheHandler is the test that the unit-level rule
// alone could not give: isContentHashed can be perfectly correct while the
// handler never asks it.
//
// It did exactly that. A hashed font under `fonts/` is not under `assets/`, so
// it reached the generic "serve a real file" branch, which passed a hardcoded
// `immutable=false`. Every page load therefore re-fetched the font slices in
// full — embedded files carry a zero modtime, so `no-cache` cannot even resolve
// to a 304 and the browser has no way to avoid the download. Asserting the
// header through ServeHTTP is what closes that gap.
func TestFontCacheControlThroughTheHandler(t *testing.T) {
	h := newTestHandler(t, "/yeciorez/teleport")

	rec := get(h, "/yeciorez/teleport/fonts/outfit-001-6c18d579.woff2")
	if rec.Code != http.StatusOK {
		t.Fatalf("hashed font status = %d, want 200", rec.Code)
	}
	if got, want := rec.Header().Get("Cache-Control"), "public, max-age=31536000, immutable"; got != want {
		t.Errorf("hashed font Cache-Control = %q, want %q", got, want)
	}

	// The stylesheet is the stable-name manifest the Go templates link. It must
	// keep revalidating even though it lives in the same directory.
	rec = get(h, "/yeciorez/teleport/fonts/fonts.css")
	if rec.Code != http.StatusOK {
		t.Fatalf("fonts.css status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("fonts.css Cache-Control = %q, want %q", got, "no-cache")
	}
}

// TestRevalidationActuallySavesTheBody guards the assumption behind `no-cache`.
//
// `no-cache` does not mean "cheap": it means "revalidate first". Embedded files
// report a zero modtime, so without a validator the browser has nothing to send
// and must re-download the whole body. `fonts/fonts.css` is ~107 KB of
// @font-face rules, so that mistake is paid on every page load. This test fails
// if the ETag ever stops being emitted, or stops being honoured.
func TestRevalidationActuallySavesTheBody(t *testing.T) {
	h := newTestHandler(t, "/yeciorez/teleport")
	path := "/yeciorez/teleport/fonts/fonts.css"

	first := get(h, path)
	if first.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", first.Code)
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on a no-cache response: every revalidation re-downloads the body")
	}
	if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) {
		t.Errorf("ETag %q is not a strong validator (must be quoted)", etag)
	}

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("If-None-Match", etag)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotModified {
		t.Fatalf("conditional GET status = %d, want 304", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("304 carried %d bytes of body; it must be empty", rec.Body.Len())
	}

	// The validator must be content-derived, not a constant: a stale ETag would
	// pin clients to old bytes across a deploy.
	other := get(h, "/yeciorez/teleport/favicon.ico")
	if other.Header().Get("ETag") == etag {
		t.Error("two different files share an ETag; it is not derived from content")
	}
}

func TestNewRejectsBadDirectory(t *testing.T) {
	t.Run("empty path", func(t *testing.T) {
		if _, err := NewDir("", "/p"); err == nil {
			t.Fatal("expected an error for an empty directory")
		}
	})

	t.Run("missing directory", func(t *testing.T) {
		if _, err := NewDir(filepath.Join(t.TempDir(), "nope"), "/p"); err == nil {
			t.Fatal("expected an error for a missing directory")
		}
	})

	t.Run("directory without index.html", func(t *testing.T) {
		// A directory that exists but is not a frontend build must be rejected:
		// serving it would 404 every route with no explanation.
		dir := t.TempDir()
		if _, err := NewDir(dir, "/p"); err == nil {
			t.Fatal("expected an error when index.html is absent")
		}
	})
}

func TestServesIndexAtPrefixRoot(t *testing.T) {
	h := newTestHandler(t, "/yeciorez/teleport")

	for _, path := range []string{"/yeciorez/teleport/", "/yeciorez/teleport"} {
		rec := get(h, path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `id="app"`) {
			t.Errorf("%s: body is not the SPA shell: %q", path, rec.Body.String())
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s: Cache-Control = %q, want no-cache", path, cc)
		}
	}
}

func TestServesHashedAssetsAsImmutable(t *testing.T) {
	h := newTestHandler(t, "/yeciorez/teleport")

	rec := get(h, "/yeciorez/teleport/assets/index-abc123.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "bundle") {
		t.Errorf("wrong body: %q", rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want immutable (content-hashed filename)", cc)
	}
}

// TestServesStableAssetUncached covers the one file under assets/ that is NOT
// content-hashed. The share page hard-codes a reference to it, so its URL must
// stay stable across deploys — which means `immutable` would pin returning
// browsers to a stale build for a year, silently.
//
// This is the case the two neighbouring tests used to skip between them: the
// immutable test used a hashed name, and the no-cache test used a file outside
// assets/. Serving the whole directory as immutable passed both.
func TestServesStableAssetUncached(t *testing.T) {
	h := newTestHandler(t, "/yeciorez/teleport")

	rec := get(h, "/yeciorez/teleport/assets/share.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "share") {
		t.Errorf("wrong body: %q", rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache (stable filename, bytes change per deploy)", cc)
	}
}

// TestStableAssetListMatchesBuild reads the two places that actually decide the
// share entry's filename — vite.config.ts (which emits it) and the share page
// (which requests it) — and asserts they agree with the exception list here.
//
// Without reading those files this test is a tautology: it would stay green
// while a renamed SHARE_ENTRY_FILE silently made the share page 404 its script.
func TestStableAssetListMatchesBuild(t *testing.T) {
	viteCfg, err := os.ReadFile(filepath.Join("..", "..", "..", "vite.config.ts"))
	if err != nil {
		t.Fatalf("read vite.config.ts: %v", err)
	}
	m := regexp.MustCompile(`SHARE_ENTRY_FILE\s*=\s*['"]([^'"]+)['"]`).FindSubmatch(viteCfg)
	if m == nil {
		t.Fatal("could not find SHARE_ENTRY_FILE in vite.config.ts")
	}
	emitted := string(m[1])

	// The share page hard-codes the URL it loads; it must match what Vite emits.
	sharePage, err := os.ReadFile(filepath.Join("..", "views", "sharepage.go"))
	if err != nil {
		t.Fatalf("read sharepage.go: %v", err)
	}
	if !strings.Contains(string(sharePage), emitted) {
		t.Errorf("share page does not reference %q; it would request a missing script", emitted)
	}

	// The exception list must name exactly that file, and it must be uncached.
	if _, ok := stableAssetFiles[emitted]; !ok {
		t.Errorf("stableAssetFiles does not list %q (build emits it, share page loads it)", emitted)
	}
	if len(stableAssetFiles) != 1 {
		t.Errorf("stableAssetFiles = %v; expected only the share entry", stableAssetFiles)
	}
	if isContentHashed(emitted) {
		t.Errorf("isContentHashed(%q) = true; its URL is not versioned", emitted)
	}

	// Hashed chunks must keep the long-lived cache, including nested names.
	for _, hashed := range []string{
		"assets/index-abc123.js",
		"assets/KeyManagementView-DwvNDI-e.js",
		"assets/index-CZ-opjm2.css",
	} {
		if !isContentHashed(hashed) {
			t.Errorf("isContentHashed(%q) = false; hashed chunks must stay immutable", hashed)
		}
	}

	// Anything else revalidates. This is the direction that matters: a new
	// non-hashed asset must not be immortalised by default.
	for _, stable := range []string{
		"assets/share.js",
		"assets/robots.txt",
		"assets/SHARE.js", // case-insensitive filesystems serve share.js here
		"assets/nested/deep/x.js",
		"assets/favicon.ico",
		"index.html",
	} {
		if isContentHashed(stable) {
			t.Errorf("isContentHashed(%q) = true; unhashed names must revalidate", stable)
		}
	}
}

// TestFontCacheRule covers the second producer feeding this rule.
//
// Fonts come from `web/public/`, which Vite copies verbatim, so their names are
// chosen by scripts/fonts/fetch_fonts.py rather than by the bundler. They carry
// a content hash for the same reason Vite's chunks do, and the stakes are
// concrete: embedded files report a zero modtime, so anything that revalidates
// is re-downloaded in full on every page load — hundreds of KB of CJK glyphs.
//
// The negative cases are the point. `fonts/fonts.css` must keep revalidating
// (the Go templates link it by a stable name), and a font file with no digest
// must not be trusted: `no-cache` is the safe failure.
func TestFontCacheRule(t *testing.T) {
	for _, hashed := range []string{
		"fonts/outfit-001-6c18d579.woff2",
		"fonts/noto-sans-sc-100-cf5346b0.woff2",
		"fonts/jetbrains-mono-005-83c005d4.woff2",
	} {
		if !isContentHashed(hashed) {
			t.Errorf("isContentHashed(%q) = false; a content-addressed font must stay immutable", hashed)
		}
	}

	for _, stable := range []string{
		"fonts/fonts.css",                  // stable name by design: the manifest
		"fonts/outfit.woff2",               // no index, no digest
		"fonts/outfit-001.woff2",           // index but no digest
		"fonts/outfit-001-6c18d57.woff2",   // digest one character short
		"fonts/outfit-001-6c18d5790.woff2", // digest one character long
		"fonts/outfit-001-zzzzzzzz.woff2",  // digest is not hex
		"fonts/outfit-1-6c18d579.woff2",    // index is not three digits
		"fonts/outfit-001-6c18d579.ttf",    // not a woff2
		"fonts/nested/deep/outfit-001-6c18d579.woff2",
		"fonts/outfit-001-6c18d579.woff2.bak",
	} {
		if isContentHashed(stable) {
			t.Errorf("isContentHashed(%q) = true; an unproven name must revalidate", stable)
		}
	}
}

// TestShapeHeuristicCoversTheRealBuild checks the name-shape rule against the
// build actually sitting in the embed directory, when there is one.
//
// This is the check that would have caught the original bug directly: it asks
// "does the rule classify every file the way that file deserves?" rather than
// reasoning about what Vite is assumed to emit. The build is gitignored, so a
// clean checkout skips it.
func TestShapeHeuristicCoversTheRealBuild(t *testing.T) {
	dist := filepath.Join("..", "webui", "dist", assetsDir)
	entries, err := os.ReadDir(dist)
	if err != nil {
		t.Skipf("no frontend build at %s (gitignored); skipping", dist)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := assetsDir + "/" + e.Name()
		hashed := isContentHashed(name)
		if _, expected := stableAssetFiles[name]; expected && hashed {
			t.Errorf("%s is a known stable asset but isContentHashed said immutable", name)
		}
		// A filename with no hyphen cannot be Vite's `[name]-[hash].ext`, so it
		// must not be treated as versioned.
		stem := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		if !strings.Contains(stem, "-") && hashed {
			t.Errorf("%s has no hash separator but isContentHashed said immutable", name)
		}
	}
}

func TestMissingAssetIsHardNotFound(t *testing.T) {
	// A miss under /assets/ must NOT fall back to the shell. Returning HTML for
	// a missing script turns a clear 404 into a MIME-type error in the console,
	// and it would also mean a deleted build silently "works" with a broken UI.
	h := newTestHandler(t, "/yeciorez/teleport")

	rec := get(h, "/yeciorez/teleport/assets/index-gone.js")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `id="app"`) {
		t.Error("missing asset fell back to the SPA shell; it must 404")
	}
}

func TestServesRealNonAssetFile(t *testing.T) {
	h := newTestHandler(t, "/yeciorez/teleport")

	rec := get(h, "/yeciorez/teleport/favicon.ico")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "not-really-an-ico" {
		t.Errorf("body = %q", rec.Body.String())
	}
	// Non-hashed names can change between deploys, so they revalidate.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
}

func TestClientRoutesFallBackToShell(t *testing.T) {
	// Deep links must survive a hard refresh; the router resolves them client
	// side after the shell loads.
	h := newTestHandler(t, "/yeciorez/teleport")

	for _, path := range []string{
		"/yeciorez/teleport/dashboard",
		"/yeciorez/teleport/dashboard/reports",
		"/yeciorez/teleport/dashboard/reports/some-uuid",
	} {
		rec := get(h, path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `id="app"`) {
			t.Errorf("%s: expected the SPA shell", path)
		}
	}
}

func TestNeverServesShellForAPIOrSharePaths(t *testing.T) {
	// These are registered as more specific patterns by the router and so never
	// reach this handler in production. The guard matters if that wiring ever
	// changes: an XHR client must get a 404, never an HTML page.
	h := newTestHandler(t, "/yeciorez/teleport")

	for _, path := range []string{
		"/yeciorez/teleport/api/health",
		"/yeciorez/teleport/api/admin/reports",
		"/yeciorez/teleport/s/sometoken",
		// A hand-off link with a truncated or mistyped path: the recipient must
		// get an explanation, not an app shell that silently does nothing.
		"/yeciorez/teleport/k/",
		"/yeciorez/teleport/k",
		"/yeciorez/teleport/k/some-id/extra",
	} {
		rec := get(h, path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), `id="app"`) {
			t.Errorf("%s: returned the SPA shell; must not", path)
		}
	}
}

func TestPathTraversalIsContained(t *testing.T) {
	// Lay out a secret outside the build root and try to reach it.
	parent := t.TempDir()
	buildDir := filepath.Join(parent, "public")
	if err := os.MkdirAll(filepath.Join(buildDir, "assets"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, "index.html"), []byte("shell"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	secret := filepath.Join(parent, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP-SECRET"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	h, err := NewDir(buildDir, "/p")
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}

	// httptest.NewRequest keeps the raw path, but net/http would normally clean
	// it; exercise both the raw and the already-cleaned form.
	for _, path := range []string{
		"/p/../secret.txt",
		"/p/assets/../../secret.txt",
		"/p/%2e%2e/secret.txt",
		"/p/....//secret.txt",
	} {
		rec := get(h, path)
		if strings.Contains(rec.Body.String(), "TOP-SECRET") {
			t.Errorf("%s: leaked a file outside the build root", path)
		}
	}
}

func TestRejectsNonReadMethods(t *testing.T) {
	h := newTestHandler(t, "/p")

	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPut} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/p/dashboard", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); !strings.Contains(allow, "GET") {
			t.Errorf("%s: Allow = %q, want it to list GET", method, allow)
		}
	}
}

func TestHeadRequestIsAllowed(t *testing.T) {
	h := newTestHandler(t, "/p")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/p/dashboard", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestWorksAtEmptyPrefix(t *testing.T) {
	// A prefix-less mount is a valid configuration (local development with
	// ROUTE_PREFIX unset), so the handler must not depend on one.
	h := newTestHandler(t, "")

	if rec := get(h, "/"); rec.Code != http.StatusOK {
		t.Errorf("/: status = %d, want 200", rec.Code)
	}
	if rec := get(h, "/dashboard"); rec.Code != http.StatusOK {
		t.Errorf("/dashboard: status = %d, want 200", rec.Code)
	}
	if rec := get(h, "/assets/index-abc123.js"); rec.Code != http.StatusOK {
		t.Errorf("asset: status = %d, want 200", rec.Code)
	}
}

// TestShellRevalidationReturns304 pins the same contract for index.html.
//
// The shell is served for every client-side route, so it is the single most
// re-fetched document in the app. Advertising an ETag without honouring it
// would be worse than having none: the browser would revalidate and be handed
// the full document anyway.
func TestShellRevalidationReturns304(t *testing.T) {
	h := newTestHandler(t, "/yeciorez/teleport")

	first := get(h, "/yeciorez/teleport/dashboard/reports/rpt_x")
	if first.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", first.Code)
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("shell has no ETag; every route navigation re-downloads the document")
	}
	if !strings.Contains(first.Body.String(), `id="app"`) {
		t.Fatal("shell body missing from the unconditional response")
	}

	req := httptest.NewRequest(http.MethodGet, "/yeciorez/teleport/dashboard/reports/rpt_x", nil)
	req.Header.Set("If-None-Match", etag)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotModified {
		t.Fatalf("conditional shell GET = %d, want 304", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("304 carried %d body bytes", rec.Body.Len())
	}
}

// --- security headers ------------------------------------------------------

// TestSecurityHeadersApplyToEveryPathThisHandlerServes is the mounting guard for
// the dashboard's CSP.
//
// It is asserted through ServeHTTP, and over *every* kind of response this
// handler produces, because the failure mode is a header set on the shell but
// not on the hashed bundles (or vice versa) — a policy that silently stops
// covering part of the surface. A unit assertion on the map would not catch
// that.
func TestSecurityHeadersApplyToEveryPathThisHandlerServes(t *testing.T) {
	h := newTestHandler(t, "/p")
	h.SetSecurityHeaders(map[string]string{
		"Content-Security-Policy":           "default-src 'none'",
		"Permissions-Policy":                "camera=()",
		"X-Permitted-Cross-Domain-Policies": "none",
	})

	for _, path := range []string{
		"/p/",                       // the shell
		"/p/dashboard/reports/xyz",  // a client-side route → shell
		"/p/assets/index-abc123.js", // a hashed bundle
		"/p/fonts/fonts.css",        // a stable asset
		"/p/favicon.ico",            // a real file
	} {
		rec := get(h, path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, rec.Code)
			continue
		}
		for header, want := range map[string]string{
			"Content-Security-Policy":           "default-src 'none'",
			"Permissions-Policy":                "camera=()",
			"X-Permitted-Cross-Domain-Policies": "none",
		} {
			if got := rec.Header().Get(header); got != want {
				t.Errorf("%s: %s = %q, want %q", path, header, got, want)
			}
		}
	}
}

// TestSecurityHeadersDoNotOverwriteTheHandlersOwn keeps the other direction
// honest: a policy that clobbered Content-Type or Cache-Control would break the
// app while looking like hardening.
func TestSecurityHeadersDoNotOverwriteTheHandlersOwn(t *testing.T) {
	h := newTestHandler(t, "/p")
	// Deliberately collides with a header the handler sets for real.
	h.SetSecurityHeaders(map[string]string{
		"Content-Type": "text/plain",
	})

	rec := get(h, "/p/assets/index-abc123.js")
	if got := rec.Header().Get("Content-Type"); strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type = %q; the security headers must not overwrite a decision made closer to the bytes", got)
	}
	if cc := rec.Header().Get("Cache-Control"); cc == "" {
		t.Error("Cache-Control was lost")
	}
}

// TestNoSecurityHeadersConfiguredChangesNothing is the disabled-state guard: the
// zero value must add nothing at all, so the SPA handler behaves exactly as it
// did before this feature existed.
func TestNoSecurityHeadersConfiguredChangesNothing(t *testing.T) {
	h := newTestHandler(t, "/p")

	rec := get(h, "/p/")
	for _, header := range []string{
		"Content-Security-Policy",
		"Content-Security-Policy-Report-Only",
		"Permissions-Policy",
		"X-Permitted-Cross-Domain-Policies",
	} {
		if got := rec.Header().Get(header); got != "" {
			t.Errorf("%s = %q with nothing configured, want it absent", header, got)
		}
	}
}
