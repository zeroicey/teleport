package httpx

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestEnvelopeShape pins the wire format, because agents and the Vue dashboard
// both parse it.
func TestEnvelopeShape(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		OK(rec, r, map[string]any{"id": "abc"})

		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if body["ok"] != true {
			t.Errorf("ok = %v, want true", body["ok"])
		}
		data, ok := body["data"].(map[string]any)
		if !ok || data["id"] != "abc" {
			t.Errorf("unexpected data: %v", body["data"])
		}
	})

	t.Run("error", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		Fail(rec, r, http.StatusBadRequest, "bad_request", "nope", map[string]any{"field": "title"})

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
		var body struct {
			OK    bool `json:"ok"`
			Error struct {
				Code    string         `json:"code"`
				Message string         `json:"message"`
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if body.OK {
			t.Error("ok should be false")
		}
		if body.Error.Code != "bad_request" || body.Error.Message != "nope" {
			t.Errorf("unexpected error: %+v", body.Error)
		}
		if body.Error.Details["field"] != "title" {
			t.Errorf("details lost: %+v", body.Error.Details)
		}
	})
}

// TestWriteErrorHidesInternals ensures an unstructured error never leaks its
// message to the client.
func TestWriteErrorHidesInternals(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	WriteError(rec, r, errSentinel{})

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "database exploded") {
		t.Errorf("internal detail leaked: %s", rec.Body.String())
	}

	// A structured error keeps its status and message.
	rec2 := httptest.NewRecorder()
	WriteError(rec2, r, NotFound("nope"))
	if rec2.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), "nope") {
		t.Errorf("message missing: %s", rec2.Body.String())
	}
}

type errSentinel struct{}

func (errSentinel) Error() string { return "database exploded at /var/lib/secret.db" }

// TestRequireAgent covers the bearer-token guard, including that the scheme is
// case-insensitive but the secret compare is exact.
func TestRequireAgent(t *testing.T) {
	cfg := AuthConfig{AgentSecretKey: "s3cret-key"}
	h := RequireAgent(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"valid", "Bearer s3cret-key", http.StatusOK},
		{"lowercase scheme", "bearer s3cret-key", http.StatusOK},
		{"mixed-case scheme", "BeArEr s3cret-key", http.StatusOK},
		{"missing header", "", http.StatusUnauthorized},
		{"wrong scheme", "Basic s3cret-key", http.StatusUnauthorized},
		{"wrong secret", "Bearer nope", http.StatusUnauthorized},
		{"prefix of secret", "Bearer s3cret", http.StatusUnauthorized},
		{"empty token", "Bearer ", http.StatusUnauthorized},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/x", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			h.ServeHTTP(rec, r)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestSessionRoundTrip covers sign/verify and tamper detection.
func TestSessionRoundTrip(t *testing.T) {
	const secret = "session-secret"

	session := BuildSession("admin", 12*time.Hour)
	value, err := SignSession(session, secret)
	if err != nil {
		t.Fatalf("SignSession: %v", err)
	}

	got, err := VerifySession(value, secret)
	if err != nil {
		t.Fatalf("VerifySession: %v", err)
	}
	if got.Subject != "admin" {
		t.Errorf("subject = %q, want admin", got.Subject)
	}
	// The expiry is rendered in seconds and echoed in ms by the handler.
	if got.Expires <= time.Now().Unix() {
		t.Errorf("session should not already be expired: %d", got.Expires)
	}

	t.Run("wrong secret", func(t *testing.T) {
		if _, err := VerifySession(value, "other-secret"); err == nil {
			t.Error("expected verification to fail with the wrong secret")
		}
	})

	t.Run("tampered payload", func(t *testing.T) {
		body, sig, _ := strings.Cut(value, ".")
		forged := base64.RawURLEncoding.EncodeToString([]byte(`{"Subject":"admin","Expires":99999999999}`)) + "." + sig
		_ = body
		if _, err := VerifySession(forged, secret); err == nil {
			t.Error("expected a forged payload to be rejected")
		}
	})

	t.Run("malformed", func(t *testing.T) {
		for _, bad := range []string{"", "no-dot", ".", "a.b", "!!!.???"} {
			if _, err := VerifySession(bad, secret); err == nil {
				t.Errorf("expected %q to be rejected", bad)
			}
		}
	})

	t.Run("expired", func(t *testing.T) {
		expired := &Session{Subject: "admin", Issued: 1, Expires: time.Now().Unix() - 10}
		v, err := SignSession(expired, secret)
		if err != nil {
			t.Fatalf("SignSession: %v", err)
		}
		if _, err := VerifySession(v, secret); err == nil {
			t.Error("expected an expired session to be rejected")
		}
	})
}

// TestSessionCookieAttributes pins the cookie hardening.
func TestSessionCookieAttributes(t *testing.T) {
	secure := SessionCookie("abc", time.Hour, true, "/")
	for _, want := range []string{"teleport_session=abc", "HttpOnly", "SameSite=Lax", "Secure", "Path=/", "Max-Age=3600"} {
		if !strings.Contains(secure, want) {
			t.Errorf("secure cookie missing %q: %s", want, secure)
		}
	}

	// Plain-HTTP development must be able to omit Secure.
	insecure := SessionCookie("abc", time.Hour, false, "/")
	if strings.Contains(insecure, "Secure") {
		t.Errorf("insecure cookie should omit Secure: %s", insecure)
	}

	clear := ClearSessionCookie(true, "/")
	if !strings.Contains(clear, "Max-Age=0") {
		t.Errorf("clear cookie missing Max-Age=0: %s", clear)
	}
}

// TestRequireSession covers the dashboard guard.
func TestRequireSession(t *testing.T) {
	cfg := AuthConfig{SessionSecret: "session-secret", SessionTTL: time.Hour}
	h := RequireSession(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := SessionFrom(r.Context())
		if s == nil {
			t.Error("session missing from context")
		}
		w.WriteHeader(http.StatusOK)
	}))

	t.Run("valid cookie", func(t *testing.T) {
		value, _ := SignSession(BuildSession("admin", time.Hour), "session-secret")
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: value})
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("no cookie", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("forged cookie", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "forged.value"})
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})
}

// TestCORSIsStrict verifies only an allow-listed origin is ever reflected.
func TestCORSIsStrict(t *testing.T) {
	h := CORS([]string{"https://teleport.zeroicey.me"})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

	t.Run("allowed origin is echoed", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		r.Header.Set("Origin", "https://teleport.zeroicey.me")
		h.ServeHTTP(rec, r)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://teleport.zeroicey.me" {
			t.Errorf("Allow-Origin = %q", got)
		}
	})

	t.Run("unknown origin is not echoed", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		r.Header.Set("Origin", "https://evil.example")
		h.ServeHTTP(rec, r)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("unknown origin was reflected: %q", got)
		}
	})

	t.Run("preflight short-circuits", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodOptions, "/x", nil)
		r.Header.Set("Origin", "https://teleport.zeroicey.me")
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusNoContent {
			t.Errorf("status = %d, want 204", rec.Code)
		}
	})
}

// TestRecoverTurnsPanicInto500 ensures one bad request cannot kill the process.
func TestRecoverTurnsPanicInto500(t *testing.T) {
	h := Recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "internal_error") {
		t.Errorf("expected an error envelope, got %s", rec.Body.String())
	}
}

// TestRequestIDEchoesCfRay prefers Cloudflare's correlation id so a request can
// be traced across the edge and the origin.
func TestRequestIDEchoesCfRay(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.Header.Set("Cf-Ray", "abc123")
	h.ServeHTTP(rec, r)

	if seen != "abc123" {
		t.Errorf("request id = %q, want abc123", seen)
	}
	if rec.Header().Get("X-Request-Id") != "abc123" {
		t.Errorf("response header = %q", rec.Header().Get("X-Request-Id"))
	}

	// With no upstream id, one is generated.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/x", nil))
	if seen == "" || seen == "abc123" {
		t.Errorf("expected a generated request id, got %q", seen)
	}
}

// TestRealIPParsesForwardedHeaders covers client-IP extraction for logging.
func TestRealIPParsesForwardedHeaders(t *testing.T) {
	cases := []struct {
		name   string
		cf     string
		xff    string
		remote string
		want   string
	}{
		{"cf wins", "1.2.3.4", "9.9.9.9", "127.0.0.1:5", "1.2.3.4"},
		{"xff first entry", "", "1.2.3.4, 5.6.7.8", "127.0.0.1:5", "1.2.3.4"},
		{"xff single", "", "1.2.3.4", "127.0.0.1:5", "1.2.3.4"},
		{"remote fallback", "", "", "10.0.0.1:5555", "10.0.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/x", nil)
			r.RemoteAddr = tc.remote
			if tc.cf != "" {
				r.Header.Set("Cf-Connecting-Ip", tc.cf)
			}
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := RealIP(r); got != tc.want {
				t.Errorf("RealIP() = %q, want %q", got, tc.want)
			}
		})
	}
}
