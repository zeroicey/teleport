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
	files := map[string]string{
		"index.html":             `<!DOCTYPE html><div id="app"></div>`,
		"assets/index-abc123.js": `console.log("bundle")`,
		"assets/share.js":        `console.log("share")`,
		"favicon.ico":            "not-really-an-ico",
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
		"assets/SHARE.js",        // case-insensitive filesystems serve share.js here
		"assets/nested/deep/x.js",
		"assets/favicon.ico",
		"index.html",
	} {
		if isContentHashed(stable) {
			t.Errorf("isContentHashed(%q) = true; unhashed names must revalidate", stable)
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
