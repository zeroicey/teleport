package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeroicey/teleport/backend/internal/config"
	"github.com/zeroicey/teleport/backend/internal/password"
	"github.com/zeroicey/teleport/backend/internal/store"
)

const (
	testAgentSecret = "test-agent-secret"
	testSessionKey  = "test-session-secret-key"
	testPassword    = "correct horse battery staple"
	// testPrefix mirrors production: the app is hosted under a path prefix on a
	// shared public host, not at the origin root.
	testPrefix = "/yeciorez/teleport"
	// testPublicOrigin is the origin only; the app lives under testPrefix.
	testPublicOrigin = "https://api.hcyj.xyz"
)

// testServer builds a fully-wired handler against a throwaway database.
func testServer(t *testing.T) (http.Handler, *config.Config) {
	t.Helper()

	hash, err := password.Hash(testPassword, 10_000) // low cost: tests run often
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}

	cfg := &config.Config{
		Addr:              "127.0.0.1:0",
		RoutePrefix:       testPrefix,
		PublicBaseURL:     testPublicOrigin,
		AssetBaseURL:      testPublicOrigin + testPrefix,
		FrontendOrigins:   []string{testPublicOrigin},
		DBPath:            filepath.Join(t.TempDir(), "test.db"),
		DefaultShareHours: 168,
		MaxContentBytes:   1 << 20,
		AgentSecretKey:    testAgentSecret,
		SessionSecret:     testSessionKey,
		AdminPasswordHash: hash,
		SessionTTL:        12 * time.Hour,
		CookieSecure:      true,
		// Mirrors production: the cookie is scoped to the app prefix because
		// the public host is shared with an unrelated service.
		CookiePath:  testPrefix,
		Environment: "test",
	}

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	handler, err := New(cfg, store.New(db))
	if err != nil {
		t.Fatalf("build handler: %v", err)
	}
	return handler, cfg
}

// do issues a request against the handler.
func do(t *testing.T, h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, reader)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func agentHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + testAgentSecret}
}

// decodeEnvelope parses the standard response envelope.
func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) (ok bool, data map[string]any, code string) {
	t.Helper()
	var body struct {
		OK    bool           `json:"ok"`
		Data  map[string]any `json:"data"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON response (%d): %s", rec.Code, rec.Body.String())
	}
	if body.Error != nil {
		code = body.Error.Code
	}
	return body.OK, body.Data, code
}

// TestHealthEndpoint confirms the DB liveness probe.
func TestHealthEndpoint(t *testing.T) {
	h, _ := testServer(t)
	rec := do(t, h, http.MethodGet, testPrefix+"/api/health", "", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	ok, data, _ := decodeEnvelope(t, rec)
	if !ok || data["status"] != "ok" {
		t.Errorf("unexpected health payload: %+v", data)
	}
}

// TestCreateReportRequiresAgentAuth ensures the ingestion endpoint is protected.
func TestCreateReportRequiresAgentAuth(t *testing.T) {
	h, _ := testServer(t)
	body := `{"title":"t","content":"c"}`

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"no auth", nil, http.StatusUnauthorized},
		{"wrong secret", map[string]string{"Authorization": "Bearer nope"}, http.StatusUnauthorized},
		{"valid", agentHeaders(), http.StatusCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, http.MethodPost, testPrefix+"/api/reports", body, tc.headers)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestCreateReportHappyPath verifies the full ingestion contract including the
// server-built share URL.
func TestCreateReportHappyPath(t *testing.T) {
	h, cfg := testServer(t)
	rec := do(t, h, http.MethodPost, testPrefix+"/api/reports",
		`{"title":"  Pentest Report  ","content":"# Findings","category":"pentest","metadata":{"severity":"high"},"autoShareHours":24}`,
		agentHeaders())

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	ok, data, _ := decodeEnvelope(t, rec)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if data["title"] != "Pentest Report" {
		t.Errorf("title should be trimmed: %v", data["title"])
	}
	if data["category"] != "pentest" || data["format"] != "markdown" {
		t.Errorf("unexpected fields: %+v", data)
	}
	if _, hasContent := data["content"]; hasContent {
		t.Error("create response should not echo the content back")
	}

	share, ok := data["share"].(map[string]any)
	if !ok || share == nil {
		t.Fatalf("expected a share object, got %v", data["share"])
	}
	if share["is_active"] != true {
		t.Errorf("share should be active: %+v", share)
	}
	// Share links must point at the application base (origin + route prefix),
	// not the bare origin: the share page is served by this same process under
	// the prefix, so an origin-root link would 404.
	wantURL := cfg.AppBaseURL() + "/s/" + share["token"].(string)
	if share["url"] != wantURL {
		t.Errorf("share url = %v, want %s", share["url"], wantURL)
	}
	if !strings.Contains(wantURL, cfg.RoutePrefix) && cfg.RoutePrefix != "" {
		t.Errorf("share url %s does not contain the route prefix %s", wantURL, cfg.RoutePrefix)
	}

	// The minted token must actually resolve.
	id := data["id"].(string)
	got := do(t, h, http.MethodGet, testPrefix+"/api/reports/"+id, "", agentHeaders())
	if got.Code != http.StatusOK {
		t.Fatalf("get report: %d %s", got.Code, got.Body.String())
	}
	_, report, _ := decodeEnvelope(t, got)
	if report["content"] != "# Findings" {
		t.Errorf("content not persisted: %v", report["content"])
	}
}

// TestCreateReportValidation pins every validation branch's status code.
func TestCreateReportValidation(t *testing.T) {
	h, _ := testServer(t)

	cases := []struct {
		name string
		body string
		want int
		code string
	}{
		{"not an object", `[]`, http.StatusBadRequest, "bad_request"},
		{"invalid JSON", `{`, http.StatusBadRequest, "bad_request"},
		{"missing title", `{"content":"c"}`, http.StatusBadRequest, "bad_request"},
		{"empty title", `{"title":"   ","content":"c"}`, http.StatusBadRequest, "bad_request"},
		{"title too long", `{"title":"` + strings.Repeat("a", 301) + `","content":"c"}`, http.StatusBadRequest, "bad_request"},
		{"missing content", `{"title":"t"}`, http.StatusBadRequest, "bad_request"},
		{"empty content", `{"title":"t","content":""}`, http.StatusBadRequest, "bad_request"},
		{"bad category", `{"title":"t","content":"c","category":"Bad Case"}`, http.StatusBadRequest, "bad_request"},
		{"bad format", `{"title":"t","content":"c","format":"pdf"}`, http.StatusBadRequest, "bad_request"},
		{"bad metadata", `{"title":"t","content":"c","metadata":"nope"}`, http.StatusBadRequest, "bad_request"},
		{"negative hours", `{"title":"t","content":"c","autoShareHours":-1}`, http.StatusBadRequest, "bad_request"},
		{"hours too large", `{"title":"t","content":"c","autoShareHours":999999}`, http.StatusBadRequest, "bad_request"},
		{"valid minimal", `{"title":"t","content":"c"}`, http.StatusCreated, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, http.MethodPost, testPrefix+"/api/reports", tc.body, agentHeaders())
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, tc.want, rec.Body.String())
			}
			if tc.code != "" {
				_, _, code := decodeEnvelope(t, rec)
				if code != tc.code {
					t.Errorf("error code = %q, want %q", code, tc.code)
				}
			}
		})
	}
}

// TestContentSizeLimit covers the byte-based 413 branch with CJK content, where
// a character count would under-report by 3x.
func TestContentSizeLimit(t *testing.T) {
	h, _ := testServer(t)

	small := do(t, h, http.MethodPost, testPrefix+"/api/reports",
		`{"title":"t","content":"`+strings.Repeat("a", 1000)+`"}`, agentHeaders())
	if small.Code != http.StatusCreated {
		t.Errorf("small content rejected: %d %s", small.Code, small.Body.String())
	}

	// One byte over the 1 MiB limit.
	huge := do(t, h, http.MethodPost, testPrefix+"/api/reports",
		`{"title":"t","content":"`+strings.Repeat("a", (1<<20)+1)+`"}`, agentHeaders())
	if huge.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized content: status = %d, want 413", huge.Code)
	}
	_, _, code := decodeEnvelope(t, huge)
	if code != "payload_too_large" {
		t.Errorf("error code = %q, want payload_too_large", code)
	}
}

// TestPublicShareRead covers the unauthenticated reader, the view counter and
// the semantic 404/410 statuses.
func TestPublicShareRead(t *testing.T) {
	h, _ := testServer(t)

	created := do(t, h, http.MethodPost, testPrefix+"/api/reports",
		`{"title":"Shared","content":"# Body","autoShareHours":24}`, agentHeaders())
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	_, data, _ := decodeEnvelope(t, created)
	token := data["share"].(map[string]any)["token"].(string)

	t.Run("read without auth", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/share/"+token, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		ok, payload, _ := decodeEnvelope(t, rec)
		if !ok {
			t.Fatal("expected ok=true")
		}
		report := payload["report"].(map[string]any)
		if report["title"] != "Shared" || report["content"] != "# Body" {
			t.Errorf("unexpected report: %+v", report)
		}
		if payload["share"].(map[string]any)["token"] != token {
			t.Errorf("token mismatch: %+v", payload["share"])
		}
	})

	t.Run("view counter increments", func(t *testing.T) {
		second := do(t, h, http.MethodGet, testPrefix+"/api/share/"+token, "", nil)
		_, payload, _ := decodeEnvelope(t, second)
		views := payload["share"].(map[string]any)["view_count"].(float64)
		if views < 1 {
			t.Errorf("view_count = %v, want >= 1", views)
		}
	})

	t.Run("unknown token is 404", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/share/aaaaaaaaaaaaaaaaaaaaaa", "", nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("malformed token is 404", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/share/short", "", nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("revoked token is 404", func(t *testing.T) {
		rev := do(t, h, http.MethodPost, testPrefix+"/api/share/"+token+"/revoke", "", agentHeaders())
		if rev.Code != http.StatusOK {
			t.Fatalf("revoke: %d %s", rev.Code, rev.Body.String())
		}
		rec := do(t, h, http.MethodGet, testPrefix+"/api/share/"+token, "", nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("revoked token status = %d, want 404", rec.Code)
		}
	})
}

// TestSharePageRendering is the highest-value integration test: it proves the
// server-rendered page, the CSP and the HTML escaping all work together.
func TestSharePageRendering(t *testing.T) {
	h, _ := testServer(t)

	markdown := "# Heading\n\n```go\npackage main\n```\n\n```mermaid\ngraph TD;\n A-->B;\n```\n\n" +
		"<script>alert('xss')</script>\n\n[link](https://example.com)\n"
	body, _ := json.Marshal(map[string]any{"title": "Page <Title>", "content": markdown})
	created := do(t, h, http.MethodPost, testPrefix+"/api/reports", string(body), agentHeaders())
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	_, data, _ := decodeEnvelope(t, created)
	token := data["share"].(map[string]any)["token"].(string)

	rec := do(t, h, http.MethodGet, testPrefix+"/s/"+token, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	page := rec.Body.String()

	t.Run("content type and cache headers", func(t *testing.T) {
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("Content-Type = %q", ct)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
			t.Errorf("Cache-Control = %q, want no-store", cc)
		}
	})

	t.Run("csp is strict", func(t *testing.T) {
		csp := rec.Header().Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'none'", "script-src 'self'", "frame-ancestors 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("CSP missing %q: %s", want, csp)
			}
		}
	})

	t.Run("title is escaped", func(t *testing.T) {
		if strings.Contains(page, "<h1>Page <Title></h1>") {
			t.Error("title was not HTML-escaped")
		}
		if !strings.Contains(page, "Page &lt;Title&gt;") {
			t.Error("expected the escaped title in the page")
		}
	})

	t.Run("raw html never reaches the page", func(t *testing.T) {
		if strings.Contains(page, "<script>alert('xss')</script>") {
			t.Error("SECURITY: raw HTML from the report was rendered")
		}
	})

	t.Run("markdown and code are rendered", func(t *testing.T) {
		if !strings.Contains(page, "<h1 id=") || !strings.Contains(page, "Heading") {
			t.Error("markdown heading missing")
		}
		if !strings.Contains(page, `<pre class="code-block" data-lang="go">`) {
			t.Error("highlighted code block missing")
		}
	})

	t.Run("mermaid placeholder is present", func(t *testing.T) {
		if !strings.Contains(page, `<pre class="mermaid">`) {
			t.Error("mermaid placeholder missing")
		}
	})

	t.Run("share script tag is present", func(t *testing.T) {
		// The bundle lives in the frontend build under the route prefix, so the
		// reference is absolute and prefix-aware rather than origin-root.
		want := `src="` + testPublicOrigin + testPrefix + `/assets/share.js"`
		if !strings.Contains(page, want) {
			t.Errorf("share.js script tag missing or not prefix-absolute; want %s", want)
		}
	})

	t.Run("meta noindex", func(t *testing.T) {
		if !strings.Contains(page, `name="robots" content="noindex, nofollow, noarchive"`) {
			t.Error("noindex meta missing")
		}
	})

	t.Run("share page is public", func(t *testing.T) {
		// Sanity: it must not require the agent secret.
		if rec.Code == http.StatusUnauthorized {
			t.Error("share page should not require authentication")
		}
	})
}

// TestExpiredSharePageIs410 covers the 410 branch end to end.
func TestExpiredSharePageIs410(t *testing.T) {
	h, _ := testServer(t)

	created := do(t, h, http.MethodPost, testPrefix+"/api/reports",
		`{"title":"t","content":"c","autoShareHours":1}`, agentHeaders())
	_, data, _ := decodeEnvelope(t, created)
	share := data["share"].(map[string]any)
	token := share["token"].(string)

	// Backdate the expiry through the admin API.
	session := login(t, h)
	patched := do(t, h, http.MethodPatch, testPrefix+"/api/admin/shares/"+token,
		`{"expiresAt":1}`, map[string]string{"Cookie": session})
	if patched.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", patched.Code, patched.Body.String())
	}

	for _, path := range []string{testPrefix + "/api/share/" + token, testPrefix + "/s/" + token} {
		rec := do(t, h, http.MethodGet, path, "", nil)
		if rec.Code != http.StatusGone {
			t.Errorf("%s: status = %d, want 410", path, rec.Code)
		}
	}
}

// login authenticates and returns the session cookie header.
func login(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := do(t, h, http.MethodPost, testPrefix+"/api/admin/login",
		`{"password":"`+testPassword+`"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login did not set a cookie")
	}
	return cookies[0].Name + "=" + cookies[0].Value
}

// TestAdminLogin covers credential checking and cookie hardening.
func TestAdminLogin(t *testing.T) {
	h, _ := testServer(t)

	t.Run("wrong password", func(t *testing.T) {
		rec := do(t, h, http.MethodPost, testPrefix+"/api/admin/login", `{"password":"wrong"}`, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
		_, _, code := decodeEnvelope(t, rec)
		if code != "unauthorized" {
			t.Errorf("error code = %q", code)
		}
	})

	t.Run("correct password", func(t *testing.T) {
		rec := do(t, h, http.MethodPost, testPrefix+"/api/admin/login",
			`{"password":"`+testPassword+`"}`, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		setCookie := rec.Header().Get("Set-Cookie")
		for _, want := range []string{"teleport_session=", "HttpOnly", "SameSite=Lax", "Secure"} {
			if !strings.Contains(setCookie, want) {
				t.Errorf("cookie missing %q: %s", want, setCookie)
			}
		}
		_, data, _ := decodeEnvelope(t, rec)
		if data["authenticated"] != true {
			t.Errorf("authenticated = %v", data["authenticated"])
		}
		if _, ok := data["expires_at"]; !ok {
			t.Error("expires_at missing")
		}
	})

	t.Run("missing password field", func(t *testing.T) {
		rec := do(t, h, http.MethodPost, testPrefix+"/api/admin/login", `{}`, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})
}

// TestAdminSessionLifecycle covers login -> session -> logout.
func TestAdminSessionLifecycle(t *testing.T) {
	h, _ := testServer(t)

	t.Run("session without cookie", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/admin/session", "", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	cookie := login(t, h)

	t.Run("session with cookie", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/admin/session", "", map[string]string{"Cookie": cookie})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		_, data, _ := decodeEnvelope(t, rec)
		if data["subject"] != "admin" {
			t.Errorf("subject = %v, want admin", data["subject"])
		}
	})

	t.Run("logout clears the cookie", func(t *testing.T) {
		rec := do(t, h, http.MethodPost, testPrefix+"/api/admin/logout", "", map[string]string{"Cookie": cookie})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=0") {
			t.Errorf("logout did not clear the cookie: %s", rec.Header().Get("Set-Cookie"))
		}
	})

	t.Run("dashboard requires a session", func(t *testing.T) {
		for _, path := range []string{"/api/admin/reports", "/api/admin/reports/anything"} {
			rec := do(t, h, http.MethodGet, testPrefix+path, "", nil)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s: status = %d, want 401", path, rec.Code)
			}
		}
	})
}

// TestAdminReportFlow covers listing, detail with tokens, minting and patching.
func TestAdminReportFlow(t *testing.T) {
	h, _ := testServer(t)
	cookie := map[string]string{"Cookie": login(t, h)}

	// Seed two reports with distinct categories.
	for _, spec := range []struct{ title, category string }{
		{"Alpha", "pentest"},
		{"Beta", "architecture"},
	} {
		body, _ := json.Marshal(map[string]any{
			"title": spec.title, "content": "body of " + spec.title,
			"category": spec.category, "metadata": map[string]any{"k": spec.title},
		})
		rec := do(t, h, http.MethodPost, testPrefix+"/api/reports", string(body), agentHeaders())
		if rec.Code != http.StatusCreated {
			t.Fatalf("seed %s: %d %s", spec.title, rec.Code, rec.Body.String())
		}
	}

	t.Run("list returns an array without content", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/admin/reports", "", cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if len(body.Data) != 2 {
			t.Fatalf("expected 2 reports, got %d", len(body.Data))
		}
		for _, r := range body.Data {
			if _, has := r["content"]; has {
				t.Errorf("list leaked content for %v", r["id"])
			}
			if _, has := r["metadata"]; !has {
				t.Errorf("metadata missing in list row %v", r["id"])
			}
		}
	})

	t.Run("category filter", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/admin/reports?category=pentest", "", cookie)
		var body struct {
			Data []map[string]any `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if len(body.Data) != 1 || body.Data[0]["title"] != "Alpha" {
			t.Errorf("filter wrong: %+v", body.Data)
		}
	})

	t.Run("detail includes share_tokens array", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/admin/reports", "", cookie)
		var list struct {
			Data []map[string]any `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &list)
		id := list.Data[0]["id"].(string)

		detail := do(t, h, http.MethodGet, testPrefix+"/api/admin/reports/"+id, "", cookie)
		if detail.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", detail.Code, detail.Body.String())
		}
		_, data, _ := decodeEnvelope(t, detail)
		if data["content"] == "" {
			t.Error("detail should include content")
		}
		tokens, ok := data["share_tokens"].([]any)
		if !ok {
			t.Fatalf("share_tokens should be an array, got %T", data["share_tokens"])
		}
		if len(tokens) != 1 {
			t.Errorf("expected 1 token, got %d", len(tokens))
		}
	})

	t.Run("mint an additional token", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/admin/reports", "", cookie)
		var list struct {
			Data []map[string]any `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &list)
		id := list.Data[0]["id"].(string)

		minted := do(t, h, http.MethodPost, testPrefix+"/api/admin/reports/"+id+"/shares",
			`{"expiresInHours":48}`, cookie)
		if minted.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201: %s", minted.Code, minted.Body.String())
		}
		_, data, _ := decodeEnvelope(t, minted)
		if data["url"] == "" || data["token"] == "" {
			t.Errorf("minted token incomplete: %+v", data)
		}
		token := data["token"].(string)

		t.Run("patch expiry", func(t *testing.T) {
			patched := do(t, h, http.MethodPatch, testPrefix+"/api/admin/shares/"+token,
				`{"expiresInHours":0}`, cookie)
			if patched.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", patched.Code, patched.Body.String())
			}
			_, pdata, _ := decodeEnvelope(t, patched)
			if pdata["expires_at"].(float64) != 0 {
				t.Errorf("expires_at = %v, want 0 (never)", pdata["expires_at"])
			}
		})

		t.Run("patch isActive", func(t *testing.T) {
			patched := do(t, h, http.MethodPatch, testPrefix+"/api/admin/shares/"+token,
				`{"isActive":false}`, cookie)
			if patched.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", patched.Code, patched.Body.String())
			}
			_, pdata, _ := decodeEnvelope(t, patched)
			if pdata["is_active"] != false {
				t.Errorf("is_active = %v, want false", pdata["is_active"])
			}
		})

		t.Run("patch with an empty body is rejected", func(t *testing.T) {
			patched := do(t, h, http.MethodPatch, testPrefix+"/api/admin/shares/"+token, `{}`, cookie)
			if patched.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", patched.Code)
			}
		})

		t.Run("delete revokes", func(t *testing.T) {
			deleted := do(t, h, http.MethodDelete, testPrefix+"/api/admin/shares/"+token, "", cookie)
			if deleted.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", deleted.Code, deleted.Body.String())
			}
			// A revoked token can no longer be read publicly.
			pub := do(t, h, http.MethodGet, testPrefix+"/api/share/"+token, "", nil)
			if pub.Code != http.StatusNotFound {
				t.Errorf("revoked token public status = %d, want 404", pub.Code)
			}
		})

		t.Run("patching an unknown token is 404", func(t *testing.T) {
			rec := do(t, h, http.MethodPatch, testPrefix+"/api/admin/shares/zzzzzzzzzzzzzzzzzzzzzz",
				`{"isActive":true}`, cookie)
			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", rec.Code)
			}
		})
	})

	t.Run("minting for an unknown report is 404", func(t *testing.T) {
		rec := do(t, h, http.MethodPost, testPrefix+"/api/admin/reports/does-not-exist/shares", `{}`, cookie)
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})
}

// TestUnknownRouteReturnsEnvelope ensures a bad path yields JSON, not Go's
// plain-text 404, so clients can parse every response.
func TestUnknownRouteReturnsEnvelope(t *testing.T) {
	h, _ := testServer(t)
	rec := do(t, h, http.MethodGet, testPrefix+"/api/nope", "", nil)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", ct)
	}
	_, _, code := decodeEnvelope(t, rec)
	if code != "not_found" {
		t.Errorf("error code = %q, want not_found", code)
	}
}

// TestUnknownRouteWithSPAStaysJSON is the regression test for a real bug: once
// the SPA handler owns the prefix catch-all, an unknown /api/... path would
// otherwise be answered with the SPA's HTML 404. That is worse than it looks —
// an XHR client calling response.json() gets a parse error and reports "bad
// response" instead of the actual 404.
//
// The fix is explicit `p+"/api/"` and `p+"/s/"` catch-alls, which net/http
// prefers over the SPA's `p+"/"`. This test would fail without them.
func TestUnknownRouteWithSPAStaysJSON(t *testing.T) {
	h, cfg := testServerWithFrontend(t)
	if cfg.StaticDir == "" {
		t.Fatal("test setup error: expected a frontend to be wired in")
	}

	for _, path := range []string{testPrefix + "/api/nope", testPrefix + "/api/admin/nope"} {
		rec := do(t, h, http.MethodGet, path, "", nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s: Content-Type = %q, want JSON (the SPA must not answer API paths)", path, ct)
		}
		if strings.Contains(rec.Body.String(), "<!DOCTYPE html>") {
			t.Errorf("%s: served the SPA shell for an API path", path)
		}
	}

	// A client-side route must still get the shell.
	rec := do(t, h, http.MethodGet, testPrefix+"/dashboard/reports", "", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("SPA route: status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<!DOCTYPE html>") {
		t.Error("SPA route did not return the shell")
	}
}

// testServerWithFrontend is testServer with a minimal frontend build wired in,
// so the SPA handler — not the JSON fallback — owns the prefix root.
func testServerWithFrontend(t *testing.T) (http.Handler, *config.Config) {
	t.Helper()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatalf("mkdir assets: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!DOCTYPE html><div id=app></div>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "index-abc.js"), []byte("//"), 0o644); err != nil {
		t.Fatalf("write asset: %v", err)
	}

	h, cfg := testServer(t)
	_ = h

	cfg.StaticDir = dir
	db, err := store.Open(filepath.Join(t.TempDir(), "frontend.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	handler, err := New(cfg, store.New(db))
	if err != nil {
		t.Fatalf("build handler with frontend: %v", err)
	}
	return handler, cfg
}

// TestMethodNotAllowed checks that a wrong verb on a known path is not silently
// treated as a 404 by the catch-all.
func TestMethodNotAllowed(t *testing.T) {
	h, _ := testServer(t)
	// GET on the create endpoint: only POST is registered.
	rec := do(t, h, http.MethodGet, testPrefix+"/api/reports", "", agentHeaders())
	if rec.Code == http.StatusCreated || rec.Code == http.StatusOK {
		t.Errorf("GET on a POST-only route unexpectedly succeeded: %d", rec.Code)
	}
}

// TestSecurityHeadersAreAlwaysSet covers the global hardening middleware.
func TestSecurityHeadersAreAlwaysSet(t *testing.T) {
	h, _ := testServer(t)
	rec := do(t, h, http.MethodGet, testPrefix+"/api/health", "", nil)

	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
		"X-Frame-Options":        "DENY",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if !strings.Contains(rec.Header().Get("X-Robots-Tag"), "noindex") {
		t.Errorf("X-Robots-Tag = %q", rec.Header().Get("X-Robots-Tag"))
	}
}

// TestCORSPreflightOnRealRoute verifies the middleware is actually wired in.
func TestCORSPreflightOnRealRoute(t *testing.T) {
	h, _ := testServer(t)

	rec := do(t, h, http.MethodOptions, testPrefix+"/api/health", "", map[string]string{
		"Origin": testPublicOrigin,
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != testPublicOrigin {
		t.Errorf("Allow-Origin = %q, want %q", got, testPublicOrigin)
	}

	// A disallowed origin must not be reflected.
	rec2 := do(t, h, http.MethodOptions, testPrefix+"/api/health", "", map[string]string{
		"Origin": "https://evil.example",
	})
	if rec2.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("disallowed origin was reflected")
	}
}

// TestRoutesAreMountedUnderPrefix is the deployment-critical assertion: the
// prefix Caddy forwards must be exactly what the backend serves, because it
// cannot be stripped at the edge without breaking share links.
func TestRoutesAreMountedUnderPrefix(t *testing.T) {
	h, _ := testServer(t)

	t.Run("prefixed path works", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/health", "", nil)
		if rec.Code != http.StatusOK {
			t.Errorf("prefixed health = %d, want 200", rec.Code)
		}
	})

	t.Run("unprefixed path is not served", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/api/health", "", nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("unprefixed health = %d, want 404 (the prefix is part of the route)", rec.Code)
		}
	})
}

// The AI guide must be reachable at stable paths, and must not be swallowed by
// the SPA catch-all (which owns the prefix root).
func TestAIGuideIsServedEvenWithFrontendMounted(t *testing.T) {
	h, cfg := testServerWithFrontend(t)
	if cfg.StaticDir == "" {
		t.Fatal("test setup error: expected a frontend")
	}

	t.Run("markdown", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/ai.md", "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
			t.Errorf("Content-Type = %q, want text/markdown", ct)
		}
		body := rec.Body.String()
		if !strings.Contains(body, cfg.AppBaseURL()) {
			t.Error("guide does not contain the live base URL")
		}
		if strings.Contains(body, "{{") {
			t.Error("served guide contains an unresolved placeholder")
		}
		if strings.Contains(body, "<div id=app>") {
			t.Error("AI guide path was answered by the SPA shell")
		}
	})

	t.Run("html", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/ai", "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("Content-Type = %q, want text/html", ct)
		}
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
			t.Errorf("CSP = %q, want a deny-by-default policy", csp)
		}
		if strings.Contains(rec.Body.String(), "<div id=app>") {
			t.Error("AI guide HTML path was answered by the SPA shell")
		}
	})

	t.Run("llms.txt", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/llms.txt", "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("Content-Type = %q, want text/plain", ct)
		}
		if !strings.Contains(rec.Body.String(), cfg.AppBaseURL()+"/ai.md") {
			t.Error("llms.txt does not point at the markdown guide")
		}
	})
}

// The guide is public: an agent has to be able to read it *before* it has a key,
// otherwise it cannot discover that a key is what it needs.
func TestAIGuideNeedsNoAuth(t *testing.T) {
	h, _ := testServer(t)
	for _, path := range []string{testPrefix + "/ai", testPrefix + "/ai.md", testPrefix + "/llms.txt"} {
		rec := do(t, h, http.MethodGet, path, "", nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200 without any credentials", path, rec.Code)
		}
	}
}

// The guide is read-only. A non-GET must not reach the handler.
//
// It answers 404 rather than 405: these paths are registered with method-scoped
// patterns, so a POST simply does not match and the request falls through to the
// prefix catch-all. That is the same behaviour every other read route here has;
// what matters is that no guide content is served.
func TestAIGuideRejectsNonGET(t *testing.T) {
	h, _ := testServer(t)
	for _, path := range []string{testPrefix + "/ai", testPrefix + "/ai.md", testPrefix + "/llms.txt"} {
		rec := do(t, h, http.MethodPost, path, "", nil)
		if rec.Code == http.StatusOK {
			t.Errorf("POST %s: status = 200, want the request refused", path)
		}
		if body := rec.Body.String(); strings.Contains(body, "AGENT_KEY") || strings.Contains(body, "# Teleport") {
			t.Errorf("POST %s: served guide content", path)
		}
	}
}

// A public page must never echo server-side secret material.
func TestAIGuideDoesNotLeakSecrets(t *testing.T) {
	h, _ := testServer(t)
	for _, path := range []string{testPrefix + "/ai", testPrefix + "/ai.md", testPrefix + "/llms.txt"} {
		body := do(t, h, http.MethodGet, path, "", nil).Body.String()
		for _, secret := range []string{testAgentSecret, testSessionKey, testPassword} {
			if strings.Contains(body, secret) {
				t.Errorf("%s leaked a credential", path)
			}
		}
	}
}
