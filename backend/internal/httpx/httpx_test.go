package httpx

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zeroicey/teleport/backend/internal/domain"
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

// stubResolver is an in-memory KeyResolver that records the digests it was
// asked about, so tests can assert on the two-path contract.
type stubResolver struct {
	calls  []string
	byHash map[string]domain.Principal
}

func (s *stubResolver) Resolve(_ context.Context, tokenHash string) (domain.Principal, bool) {
	s.calls = append(s.calls, tokenHash)
	p, ok := s.byHash[tokenHash]
	return p, ok
}

// tokenDigest computes sha256 hex independently of the implementation, pinning
// the on-the-wire contract that a resolver is handed a digest and nothing else.
func tokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// agentServe drives one request through mw and returns the recorder plus the
// principal the inner handler observed (nil when the request was rejected).
// A nil inner handler just answers 200.
func agentServe(t *testing.T, mw Middleware, authHeader string, inner http.Handler) (*httptest.ResponseRecorder, *domain.Principal) {
	t.Helper()
	if inner == nil {
		inner = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
	}
	var seen *domain.Principal
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = PrincipalFrom(r.Context())
		inner.ServeHTTP(w, r)
	}))
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	if authHeader != "" {
		r.Header.Set("Authorization", authHeader)
	}
	h.ServeHTTP(rec, r)
	return rec, seen
}

// TestRequireAgent covers the bearer-token guard, including that the scheme is
// case-insensitive but the secret compare is exact.
func TestRequireAgent(t *testing.T) {
	cfg := AuthConfig{AgentSecretKey: "s3cret-key"}
	h := RequireAgent(cfg, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

// TestRequireAgentNilResolverIsEnvOnly pins the backwards-compatible path: with
// no resolver wired in, the env key still authenticates and the middleware does
// not panic.
func TestRequireAgentNilResolverIsEnvOnly(t *testing.T) {
	mw := RequireAgent(AuthConfig{AgentSecretKey: "root-key"}, nil)

	rec, p := agentServe(t, mw, "Bearer root-key", nil)
	if rec.Code != http.StatusOK || p == nil || !p.Root || p.KeyID != "" {
		t.Errorf("status = %d, principal = %+v; want 200 + root with empty KeyID", rec.Code, p)
	}
	if rec, _ := agentServe(t, mw, "Bearer other-key", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// TestRequireAgentRootKey verifies the env credential yields a Root principal.
func TestRequireAgentRootKey(t *testing.T) {
	cfg := AuthConfig{AgentSecretKey: "root-key"}
	res := &stubResolver{byHash: map[string]domain.Principal{}}
	mw := RequireAgent(cfg, res)

	rec, p := agentServe(t, mw, "Bearer root-key", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if p == nil {
		t.Fatal("principal missing from context")
	}
	if !p.Root || p.KeyID != "" || p.Name != "" {
		t.Errorf("principal = %+v, want {KeyID:\"\", Name:\"\", Root:true}", p)
	}
}

// TestRequireAgentDatabaseKey verifies path 2: a token whose sha256 hex is in
// the resolver authenticates as a named, non-root principal, and the resolver
// sees the digest rather than the plaintext.
func TestRequireAgentDatabaseKey(t *testing.T) {
	const token = "db-issued-token-abc"
	cfg := AuthConfig{AgentSecretKey: "root-key"}
	res := &stubResolver{byHash: map[string]domain.Principal{
		tokenDigest(token): {KeyID: "key_1", Name: "nightly-bot"},
	}}
	mw := RequireAgent(cfg, res)

	rec, p := agentServe(t, mw, "Bearer "+token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if p == nil || p.Root || p.KeyID != "key_1" || p.Name != "nightly-bot" {
		t.Errorf("principal = %+v, want non-root key_1/nightly-bot", p)
	}
	if len(res.calls) != 1 {
		t.Fatalf("resolver calls = %d, want 1", len(res.calls))
	}
	if res.calls[0] != tokenDigest(token) {
		t.Errorf("resolver saw %q, want the sha256 hex digest", res.calls[0])
	}
	if strings.Contains(res.calls[0], token) {
		t.Error("resolver received the plaintext token")
	}
}

// TestRequireAgentBothPathsAlwaysRun is the timing-contract test: the resolver
// must be consulted even when the env key matched, so no branch can short-circuit
// the second path and leak which failure occurred.
func TestRequireAgentBothPathsAlwaysRun(t *testing.T) {
	const root = "root-key"
	cfg := AuthConfig{AgentSecretKey: root}
	res := &stubResolver{byHash: map[string]domain.Principal{}}
	mw := RequireAgent(cfg, res)

	cases := []struct {
		name       string
		header     string
		wantStatus int
		wantCalls  int
	}{
		{"root matches", "Bearer " + root, http.StatusOK, 1},
		{"root-shaped but wrong", "Bearer root-kez", http.StatusUnauthorized, 1},
		{"unknown long token", "Bearer " + strings.Repeat("x", 200), http.StatusUnauthorized, 1},
		{"unknown short token", "Bearer x", http.StatusUnauthorized, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res.calls = nil
			rec, _ := agentServe(t, mw, tc.header, nil)
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if len(res.calls) != tc.wantCalls {
				t.Errorf("resolver calls = %d, want %d — path 2 must not be skipped",
					len(res.calls), tc.wantCalls)
			}
		})
	}
}

// TestRequireAgentUnauthorizedIsUniform asserts every failure mode is
// byte-identical on the wire, so a client cannot learn *why* it failed.
func TestRequireAgentUnauthorizedIsUniform(t *testing.T) {
	const root = "root-key"
	cfg := AuthConfig{AgentSecretKey: root}
	res := &stubResolver{byHash: map[string]domain.Principal{
		tokenDigest("live"): {KeyID: "key_1", Name: "live"},
		// A resolver hit with no identity: must fail like everything else
		// rather than degrade into root (see the default branch in RequireAgent).
		tokenDigest("ghost"): {Name: "ghost"},
	}}
	mw := RequireAgent(cfg, res)

	type errBody struct {
		OK    bool `json:"ok"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}

	headers := map[string]string{
		"missing header":         "",
		"wrong scheme":           "Basic " + root,
		"empty bearer":           "Bearer ",
		"wrong root, same len":   "Bearer root-kez",
		"wrong root, longer":     "Bearer " + strings.Repeat("r", 64),
		"wrong root, shorter":    "Bearer r",
		"not in table":           "Bearer " + strings.Repeat("z", 40),
		"resolver hit, no KeyID": "Bearer ghost",
	}

	var want errBody
	for name, header := range headers {
		rec, p := agentServe(t, mw, header, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, rec.Code)
			continue
		}
		if p != nil {
			t.Errorf("%s: principal attached on failure: %+v", name, p)
		}
		var got errBody
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s: invalid JSON: %v", name, err)
		}
		if want.Error.Code == "" {
			want = got
			continue
		}
		if got.Error.Code != want.Error.Code || got.Error.Message != want.Error.Message {
			t.Errorf("%s: body = %+v, want %+v", name, got.Error, want.Error)
		}
	}
	if want.Error.Code != "unauthorized" {
		t.Errorf("error code = %q, want unauthorized", want.Error.Code)
	}
	// The message must not hint at which path failed.
	for _, leak := range []string{"root", "agent_keys", "secret", "hash", "table"} {
		if strings.Contains(strings.ToLower(want.Error.Message), leak) {
			t.Errorf("failure message leaks path detail %q: %q", leak, want.Error.Message)
		}
	}
}

// TestRequireAgentResolverCannotMintRoot ensures a resolver result can never
// claim Root, which is a property of the environment credential alone.
func TestRequireAgentResolverCannotMintRoot(t *testing.T) {
	const token = "would-be-root"
	cfg := AuthConfig{AgentSecretKey: "root-key"}
	res := &stubResolver{byHash: map[string]domain.Principal{
		tokenDigest(token): {KeyID: "key_evil", Root: true},
	}}
	mw := RequireAgent(cfg, res)

	rec, p := agentServe(t, mw, "Bearer "+token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if p == nil || p.Root {
		t.Errorf("principal = %+v, resolver must not be able to set Root", p)
	}
	if p != nil && p.KeyID != "key_evil" {
		t.Errorf("KeyID = %q, want key_evil", p.KeyID)
	}
}

// TestRequireAgentIdentitylessResolverHitIsRejected locks the ownership
// boundary: a resolver hit with an empty KeyID carries no identity, and since
// empty KeyID is exactly what root-published reports store as owner_key_id,
// accepting it would hand that key root's reports. It must fail closed with the
// same 401 as every other failure instead of degrading into root.
func TestRequireAgentIdentitylessResolverHitIsRejected(t *testing.T) {
	cfg := AuthConfig{AgentSecretKey: "root-key"}
	res := &stubResolver{byHash: map[string]domain.Principal{
		tokenDigest("ghost"):        {Name: "ghost"},                // no id
		tokenDigest("ghost-rooted"): {Name: "ghost", Root: true},    // no id, claims root
		tokenDigest("real"):         {KeyID: "key_1", Name: "real"}, // control
	}}
	mw := RequireAgent(cfg, res)

	for _, header := range []string{"Bearer ghost", "Bearer ghost-rooted"} {
		rec, p := agentServe(t, mw, header, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%q: status = %d, want 401 — an identityless hit must fail closed", header, rec.Code)
		}
		if p != nil {
			t.Errorf("%q: principal attached on failure: %+v", header, p)
		}
		if !strings.Contains(rec.Body.String(), `"message":"`+agentAuthMessage+`"`) {
			t.Errorf("%q: body = %s, want the uniform 401 message", header, rec.Body.String())
		}
	}

	// Control: a properly identified key still authenticates, so the check above
	// rejects the missing identity rather than the resolver path as a whole.
	rec, p := agentServe(t, mw, "Bearer real", nil)
	if rec.Code != http.StatusOK || p == nil || p.KeyID != "key_1" {
		t.Errorf("control: status = %d, principal = %+v, want 200 + key_1", rec.Code, p)
	}
}

// TestRequireAgentNeverEchoesCredential checks the response carries neither the
// presented token nor any digest of it.
func TestRequireAgentNeverEchoesCredential(t *testing.T) {
	const token = "super-secret-agent-token"
	cfg := AuthConfig{AgentSecretKey: "root-key"}
	res := &stubResolver{byHash: map[string]domain.Principal{}}
	mw := RequireAgent(cfg, res)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "principal=%+v", PrincipalFrom(r.Context()))
	})
	for _, header := range []string{"Bearer " + token, "Bearer wrong"} {
		rec, _ := agentServe(t, mw, header, inner)
		body := rec.Body.String()
		if strings.Contains(body, token) {
			t.Errorf("response contains the plaintext token: %s", body)
		}
		if strings.Contains(body, tokenDigest(token)) {
			t.Errorf("response contains the token digest: %s", body)
		}
	}
}

// TestRequireAgentEmptyRootKeyRejects guards against a missing
// AGENT_SECRET_KEY turning into an authentication bypass.
func TestRequireAgentEmptyRootKeyRejects(t *testing.T) {
	mw := RequireAgent(AuthConfig{}, nil)
	for _, header := range []string{"Bearer x", "Bearer ", ""} {
		rec, _ := agentServe(t, mw, header, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("header %q: status = %d, want 401", header, rec.Code)
		}
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

// sessionRequest drives one request through a RequireSession-wrapped handler,
// with an explicit peer address, CF Access header and cookie. It returns the
// recorder and the subject the handler observed ("" when unauthenticated).
func sessionRequest(t *testing.T, mw Middleware, peer, cfEmail, cookie string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	var subject string
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s := SessionFrom(r.Context()); s != nil {
			subject = s.Subject
		}
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.RemoteAddr = peer
	if cfEmail != "" {
		// Literal header name on purpose: the constant is the implementation's,
		// and a typo in it must fail here rather than silently disable the gate.
		r.Header.Set("Cf-Access-Authenticated-User-Email", cfEmail)
	}
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: cookie})
	}
	h.ServeHTTP(rec, r)
	return rec, subject
}

// TestRequireSessionCFAccessHeaderIsInertByDefault is the regression test for
// the dashboard authentication bypass: while TrustCFAccess is off, a forged
// Cf-Access-Authenticated-User-Email must be worth exactly nothing — no better
// than sending no credential at all — from any peer.
func TestRequireSessionCFAccessHeaderIsInertByDefault(t *testing.T) {
	t.Cleanup(func() { TrustCFAccess = false })
	TrustCFAccess = false

	mw := RequireSession(AuthConfig{SessionSecret: "session-secret", SessionTTL: time.Hour})

	peers := map[string]string{
		"untrusted peer": "1.2.3.4:5",     // the production repro: direct connection
		"public peer":    "203.0.113.7:5", // any other direct connection
		"trusted caddy":  "172.17.0.1:5",  // even the legit front end
		"loopback":       "127.0.0.1:5",   // and even localhost
	}
	for name, peer := range peers {
		t.Run(name, func(t *testing.T) {
			rec, subject := sessionRequest(t, mw, peer, "lead-verify@example.invalid", "")
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401 (body: %s)", rec.Code, rec.Body.String())
			}
			if subject != "" {
				t.Errorf("subject = %q, want empty — the header must not authenticate", subject)
			}
		})
	}
}

// TestRequireSessionCFAccessRequiresBoth flag and trusted peer: enabling the
// shortcut must not let an untrusted source assert an identity.
func TestRequireSessionCFAccessRequiresBoth(t *testing.T) {
	t.Cleanup(func() { TrustCFAccess = false })
	mw := RequireSession(AuthConfig{SessionSecret: "session-secret", SessionTTL: time.Hour})

	TrustCFAccess = true
	t.Cleanup(func() { TrustCFAccess = false })

	// Untrusted peer: the flag alone is not enough.
	rec, subject := sessionRequest(t, mw, "1.2.3.4:5", "attacker@example.invalid", "")
	if rec.Code != http.StatusUnauthorized || subject != "" {
		t.Errorf("untrusted peer: status = %d subject = %q, want 401 with no subject", rec.Code, subject)
	}

	// Trusted peer (Caddy): the header is now accepted.
	rec, subject = sessionRequest(t, mw, "172.17.0.1:5", "admin@zeroicey.me", "")
	if rec.Code != http.StatusOK || subject != "admin@zeroicey.me" {
		t.Errorf("trusted peer: status = %d subject = %q, want 200 with admin@zeroicey.me", rec.Code, subject)
	}
}

// TestRequireSessionCFAccessFallsBackToCookie covers malformed identities: the
// header is ignored and a valid session cookie still works.
func TestRequireSessionCFAccessFallsBackToCookie(t *testing.T) {
	t.Cleanup(func() { TrustCFAccess = false })
	TrustCFAccess = true
	t.Cleanup(func() { TrustCFAccess = false })

	mw := RequireSession(AuthConfig{SessionSecret: "session-secret", SessionTTL: time.Hour})
	cookie, err := SignSession(BuildSession("cookie-admin", time.Hour), "session-secret")
	if err != nil {
		t.Fatalf("SignSession: %v", err)
	}

	// Boundary: exactly 320 bytes is acceptable, 321 is not.
	ok320 := strings.Repeat("a", 316) + "@b.c" // 320 bytes
	bad321 := strings.Repeat("a", 317) + "@b.c"

	bad := map[string]string{
		"blank":        "   ",
		"no at sign":   "not-an-identity",
		"empty local":  "@example.invalid",
		"empty domain": "user@",
		"too long":     bad321,
		"newline":      "user@example.invalid\nX-Injected: 1",
		"carriage ret": "user@example.invalid\r",
		"tab":          "user\t@example.invalid",
		"delete char":  "user@example.invalid\x7f",
	}

	for name, value := range bad {
		t.Run("no cookie/"+name, func(t *testing.T) {
			rec, subject := sessionRequest(t, mw, "172.17.0.1:5", value, "")
			if rec.Code != http.StatusUnauthorized || subject != "" {
				t.Errorf("status = %d subject = %q, want 401 with no subject", rec.Code, subject)
			}
		})
		t.Run("valid cookie/"+name, func(t *testing.T) {
			rec, subject := sessionRequest(t, mw, "172.17.0.1:5", value, cookie)
			if rec.Code != http.StatusOK || subject != "cookie-admin" {
				t.Errorf("status = %d subject = %q, want 200 with cookie-admin", rec.Code, subject)
			}
		})
	}

	t.Run("320 byte address is accepted", func(t *testing.T) {
		rec, subject := sessionRequest(t, mw, "172.17.0.1:5", ok320, "")
		if rec.Code != http.StatusOK || subject != ok320 {
			t.Errorf("status = %d subject = %q, want 200 with the 320-byte address", rec.Code, subject)
		}
	})

	t.Run("header with surrounding spaces is trimmed", func(t *testing.T) {
		rec, subject := sessionRequest(t, mw, "172.17.0.1:5", "  admin@zeroicey.me  ", "")
		if rec.Code != http.StatusOK || subject != "admin@zeroicey.me" {
			t.Errorf("status = %d subject = %q, want the trimmed address", rec.Code, subject)
		}
	})
}

// TestRequireSessionCookieStillWorksWithoutCFAccess guards the other direction:
// turning the shortcut off must not break the cookie path.
func TestRequireSessionCookieStillWorksWithoutCFAccess(t *testing.T) {
	t.Cleanup(func() { TrustCFAccess = false })
	TrustCFAccess = false

	mw := RequireSession(AuthConfig{SessionSecret: "session-secret", SessionTTL: time.Hour})
	cookie, err := SignSession(BuildSession("cookie-admin", time.Hour), "session-secret")
	if err != nil {
		t.Fatalf("SignSession: %v", err)
	}

	rec, subject := sessionRequest(t, mw, "1.2.3.4:5", "", cookie)
	if rec.Code != http.StatusOK || subject != "cookie-admin" {
		t.Errorf("status = %d subject = %q, want 200 with cookie-admin", rec.Code, subject)
	}
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

// requestIDRoundTrip runs one request through RequestID with the given upstream
// headers and returns the id the handler saw plus the id echoed back.
func requestIDRoundTrip(headers map[string]string) (seen, echoed string) {
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	h.ServeHTTP(rec, r)
	return seen, rec.Header().Get("X-Request-Id")
}

// TestRequestIDSanitisesClientValue pins the hardening of a client-supplied
// correlation id: normal ids pass through byte-for-byte, oversized ones are
// truncated, and hostile ones are discarded rather than logged.
func TestRequestIDSanitisesClientValue(t *testing.T) {
	const uuid = "8f3d5a1e-1b2c-4d5e-8f90-1234567890ab"

	t.Run("normal id is unchanged", func(t *testing.T) {
		for _, header := range []string{"Cf-Ray", "X-Request-Id"} {
			seen, echoed := requestIDRoundTrip(map[string]string{header: uuid})
			if seen != uuid || echoed != uuid {
				t.Errorf("%s: seen = %q echoed = %q, want %q byte-for-byte", header, seen, echoed, uuid)
			}
		}
	})

	t.Run("oversized id is truncated to 128 bytes", func(t *testing.T) {
		long := strings.Repeat("a", 4000)
		seen, echoed := requestIDRoundTrip(map[string]string{"Cf-Ray": long})
		want := strings.Repeat("a", 128)
		if seen != want || echoed != want {
			t.Errorf("seen = %d bytes, echoed = %d bytes; want 128 bytes each", len(seen), len(echoed))
		}
		if seen != echoed {
			t.Errorf("echoed %q does not match the id the handler saw", echoed)
		}
	})

	// A control character is a log-injection primitive (a forged newline fakes a
	// log line) and must never survive into the log or the echoed header.
	hostile := map[string]string{
		"newline":     "abc\ndef",
		"cr":          "abc\rdef",
		"tab":         "abc\tdef",
		"delete char": "abc\x7fdef",
		"nul":         "abc\x00def",
		"trailing cr": "abc\r",
		"only space":  "   ",
	}
	for name, value := range hostile {
		t.Run("hostile/"+name, func(t *testing.T) {
			seen, echoed := requestIDRoundTrip(map[string]string{"Cf-Ray": value})
			if seen == "" {
				t.Fatal("no id was assigned")
			}
			if seen == value {
				t.Errorf("hostile value was accepted verbatim: %q", seen)
			}
			for i := 0; i < len(seen); i++ {
				if c := seen[i]; c < 0x20 || c == 0x7f {
					t.Fatalf("control character survived in the id: %q", seen)
				}
			}
			if !utf8.ValidString(seen) {
				t.Errorf("id is not valid UTF-8: %q", seen)
			}
			if echoed != seen {
				t.Errorf("echoed %q does not match the sanitised id %q", echoed, seen)
			}
		})
	}

	t.Run("hostile Cf-Ray falls through to a healthy X-Request-Id", func(t *testing.T) {
		seen, echoed := requestIDRoundTrip(map[string]string{
			"Cf-Ray":       "abc\ndef",
			"X-Request-Id": uuid,
		})
		if seen != uuid || echoed != uuid {
			t.Errorf("seen = %q echoed = %q, want the X-Request-Id fallback %q", seen, echoed, uuid)
		}
	})

	t.Run("truncation does not split a multi-byte rune", func(t *testing.T) {
		// 127 ASCII bytes + a 2-byte rune = 129 bytes, so the cut would land
		// inside the rune.
		value := strings.Repeat("a", 127) + "é"
		seen, echoed := requestIDRoundTrip(map[string]string{"Cf-Ray": value})
		if want := strings.Repeat("a", 127); seen != want {
			t.Errorf("seen = %q, want %q", seen, want)
		}
		if !utf8.ValidString(seen) {
			t.Errorf("id is not valid UTF-8: %q", seen)
		}
		if echoed != seen {
			t.Errorf("echoed %q does not match %q", echoed, seen)
		}
	})

	t.Run("invalid UTF-8 is rejected", func(t *testing.T) {
		seen, echoed := requestIDRoundTrip(map[string]string{"Cf-Ray": "abc\xffdef"})
		if seen == "abc\xffdef" || !utf8.ValidString(seen) {
			t.Errorf("seen = %q, want a sanitised or generated id", seen)
		}
		if echoed != seen {
			t.Errorf("echoed %q does not match %q", echoed, seen)
		}
	})

	t.Run("generated ids are never attacker text", func(t *testing.T) {
		seen, echoed := requestIDRoundTrip(nil)
		if len(seen) != 32 {
			t.Errorf("generated id = %q, want 32 hex characters", seen)
		}
		if echoed != seen {
			t.Errorf("echoed %q does not match %q", echoed, seen)
		}
	})
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

// clientIPRequest builds a request with an explicit peer address and forwarded
// headers, so ClientIP is exercised through the real net/http types rather than
// through its helpers.
func clientIPRequest(remote, xff, cf string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/agent-keys/applications", nil)
	r.RemoteAddr = remote
	if xff != "" {
		for _, v := range strings.Split(xff, "|") {
			r.Header.Add("X-Forwarded-For", v)
		}
	}
	if cf != "" {
		r.Header.Set("Cf-Connecting-Ip", cf)
	}
	return r
}

// TestClientIPSpoofingIsIgnored is the regression test for the rate-limit
// bypass: a direct (untrusted) peer must not be able to influence its own bucket
// with any forwarded header.
func TestClientIPSpoofingIsIgnored(t *testing.T) {
	t.Cleanup(func() { TrustCloudflare = false })
	TrustCloudflare = false

	cases := []struct {
		name   string
		remote string
		xff    string
		cf     string
		want   string
	}{
		{"peer only", "1.2.3.4:5", "", "", "1.2.3.4"},
		{"forged xff ignored", "1.2.3.4:5", "9.9.9.9", "", "1.2.3.4"},
		{"forged cf ignored", "1.2.3.4:5", "", "9.9.9.9", "1.2.3.4"},
		{"both forged ignored", "1.2.3.4:5", "9.9.9.9", "9.9.9.9", "1.2.3.4"},
		{"forged xff chain ignored", "1.2.3.4:5", "8.8.8.8, 9.9.9.9", "8.8.4.4", "1.2.3.4"},
		{"untrusted public peer", "203.0.113.7:443", "10.0.0.5", "", "203.0.113.7"},
		{"untrusted v6 peer", "[2001:db8::5]:99", "9.9.9.9", "", "2001:db8::5"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClientIP(clientIPRequest(tc.remote, tc.xff, tc.cf)); got != tc.want {
				t.Errorf("ClientIP() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClientIPTrustedProxy walks the forwarded chain from the right, which is
// the end a trusted proxy actually appended to.
func TestClientIPTrustedProxy(t *testing.T) {
	t.Cleanup(func() { TrustCloudflare = false })
	TrustCloudflare = false

	cases := []struct {
		name   string
		remote string
		xff    string
		want   string
	}{
		{"single hop", "172.17.0.1:5", "9.9.9.9", "9.9.9.9"},
		{"caddy append: rightmost untrusted", "172.17.0.1:5", "8.8.8.8, 9.9.9.9", "9.9.9.9"},
		{"spoofed leftmost loses to caddy's append", "172.17.0.1:5", "1.2.3.4, 9.9.9.9", "9.9.9.9"},
		{"multi-hop skips trusted", "172.17.0.1:5", "8.8.8.8, 172.17.0.1", "8.8.8.8"},
		{"all hops trusted falls back to peer", "172.17.0.1:5", "10.0.0.1, 192.168.1.1", "172.17.0.1"},
		{"loopback peer", "127.0.0.1:5", "8.8.8.8", "8.8.8.8"},
		{"private peer", "192.168.1.9:5", "8.8.8.8", "8.8.8.8"},
		{"docker bridge peer", "172.18.0.1:5", "8.8.8.8", "8.8.8.8"},
		{"ula peer v6", "[fd00::1]:5", "8.8.8.8", "8.8.8.8"},
		{"loopback v6 peer", "[::1]:5", "2001:db8::1", "2001:db8::1"},
		{"mapped v4 peer is unmapped", "[::ffff:172.17.0.1]:5", "8.8.8.8", "8.8.8.8"},
		{"no header uses peer", "172.17.0.1:5", "", "172.17.0.1"},
		{"bare host peer", "172.17.0.1", "8.8.8.8", "8.8.8.8"},
		{"bracketed v6 peer without port", "[fd00::1]", "8.8.8.8", "8.8.8.8"},
		{"multiple header lines in order", "172.17.0.1:5", "8.8.8.8|9.9.9.9", "9.9.9.9"},
		{"leading/trailing spaces", "172.17.0.1:5", "  8.8.8.8 ,  9.9.9.9  ", "9.9.9.9"},
		{"empty header lines", "172.17.0.1:5", " | ", "172.17.0.1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClientIP(clientIPRequest(tc.remote, tc.xff, "")); got != tc.want {
				t.Errorf("ClientIP() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClientIPRejectsNonIPEntries ensures a malformed forwarded value can never
// become a bucket, and that junk on RemoteAddr cannot panic.
func TestClientIPRejectsNonIPEntries(t *testing.T) {
	t.Cleanup(func() { TrustCloudflare = false })
	TrustCloudflare = false

	cases := []struct {
		name   string
		remote string
		xff    string
		want   string
	}{
		{"host:port entry skipped", "172.17.0.1:5", "9.9.9.9, 1.2.3.4:80", "9.9.9.9"},
		{"junk entry skipped", "172.17.0.1:5", "not-an-ip, 9.9.9.9", "9.9.9.9"},
		{"all junk falls back to peer", "172.17.0.1:5", "not-an-ip, host:99, ", "172.17.0.1"},
		{"cidr entry skipped", "172.17.0.1:5", "8.8.8.0/24, 9.9.9.9", "9.9.9.9"},
		{"v4 with port on remote", "1.2.3.4:5", "", "1.2.3.4"},
		{"garbage remote passes through", "somewhere", "8.8.8.8", "somewhere"},
		{"empty remote passes through", "", "8.8.8.8", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClientIP(clientIPRequest(tc.remote, tc.xff, ""))
			if got != tc.want {
				t.Errorf("ClientIP() = %q, want %q", got, tc.want)
			}
			// Whatever comes back must either be the unparseable peer itself or a
			// real IP literal — never a forwarded string smuggled through.
			if _, err := netip.ParseAddr(got); err != nil && got != tc.remote {
				t.Errorf("ClientIP() = %q is neither an IP nor the raw peer %q", got, tc.remote)
			}
		})
	}
}

// TestClientIPCloudflareModeOffByDefault pins that Cf-Connecting-Ip is inert
// unless the operator has explicitly confirmed a CDN is in front.
func TestClientIPCloudflareModeOffByDefault(t *testing.T) {
	t.Cleanup(func() { TrustCloudflare = false })

	// Default state.
	TrustCloudflare = false
	if got := ClientIP(clientIPRequest("172.17.0.1:5", "", "9.9.9.9")); got != "172.17.0.1" {
		t.Errorf("Cloudflare off: ClientIP() = %q, want the trusted peer", got)
	}
	if got := ClientIP(clientIPRequest("1.2.3.4:5", "", "9.9.9.9")); got != "1.2.3.4" {
		t.Errorf("Cloudflare off, untrusted peer: ClientIP() = %q, want 1.2.3.4", got)
	}
	// XFF still wins over the ignored CF header on a trusted peer.
	if got := ClientIP(clientIPRequest("172.17.0.1:5", "8.8.8.8", "9.9.9.9")); got != "8.8.8.8" {
		t.Errorf("Cloudflare off: ClientIP() = %q, want 8.8.8.8 from XFF", got)
	}

	// Explicitly enabled.
	TrustCloudflare = true
	if got := ClientIP(clientIPRequest("172.17.0.1:5", "8.8.8.8", "9.9.9.9")); got != "9.9.9.9" {
		t.Errorf("Cloudflare on: ClientIP() = %q, want 9.9.9.9 from Cf-Connecting-Ip", got)
	}
	// An untrusted peer gains nothing from the flag.
	if got := ClientIP(clientIPRequest("1.2.3.4:5", "", "9.9.9.9")); got != "1.2.3.4" {
		t.Errorf("Cloudflare on, untrusted peer: ClientIP() = %q, want 1.2.3.4", got)
	}
	// A malformed CF value falls through to the XFF walk.
	if got := ClientIP(clientIPRequest("172.17.0.1:5", "8.8.8.8", "not-an-ip")); got != "8.8.8.8" {
		t.Errorf("Cloudflare on, bad CF header: ClientIP() = %q, want 8.8.8.8", got)
	}
}
