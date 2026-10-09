package spa

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
