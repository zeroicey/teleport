package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeroicey/teleport/backend/internal/agentkey"
	"github.com/zeroicey/teleport/backend/internal/config"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// hex64 matches a sha256 digest. Nothing this API returns may contain one except
// inside a value the caller legitimately owns; token_hash is the thing that must
// never appear.
var hex64 = regexp.MustCompile(`\b[0-9a-f]{64}\b`)

// assertNoHashLeak fails if a response body contains a sha256-shaped digest or
// even names the field.
func assertNoHashLeak(t *testing.T, label, body string) {
	t.Helper()
	if strings.Contains(body, "token_hash") {
		t.Errorf("%s: response mentions token_hash: %s", label, body)
	}
	if m := hex64.FindString(body); m != "" {
		t.Errorf("%s: response contains a 64-hex digest %q: %s", label, m, body)
	}
}

// decodeArray parses an envelope whose data is a JSON array. decodeEnvelope
// cannot be used: it types data as an object, so an array is a fatal error.
func decodeArray(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var body struct {
		OK    bool             `json:"ok"`
		Data  []map[string]any `json:"data"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON array response (%d): %s", rec.Code, rec.Body.String())
	}
	if !body.OK {
		t.Fatalf("expected ok=true, got error %+v (body: %s)", body.Error, rec.Body.String())
	}
	if body.Data == nil {
		t.Fatalf("expected a JSON array, got: %s", rec.Body.String())
	}
	return body.Data
}

func bearerHeaders(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func claimHeaders(secret string) map[string]string {
	return map[string]string{claimHeader: secret}
}

// applyForKey submits a public application and returns its id and claim secret.
func applyForKey(t *testing.T, h http.Handler, label string) (id, secret string) {
	t.Helper()
	rec := do(t, h, http.MethodPost, testPrefix+"/api/agent-keys/applications",
		`{"label":"`+label+`","purpose":"unit test","requestedHours":24}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("apply: status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	ok, data, _ := decodeEnvelope(t, rec)
	if !ok {
		t.Fatalf("apply: ok=false: %s", rec.Body.String())
	}
	id, _ = data["id"].(string)
	secret, _ = data["claim_secret"].(string)
	if id == "" || secret == "" {
		t.Fatalf("apply: missing id/claim_secret in %s", rec.Body.String())
	}
	if data["status"] != "pending" {
		t.Errorf("apply: status = %v, want pending", data["status"])
	}
	if _, ok := data["poll_interval_seconds"]; !ok {
		t.Error("apply: poll_interval_seconds missing")
	}
	return id, secret
}

// approveApplication approves and returns the updated application payload.
func approveApplication(t *testing.T, h http.Handler, cookie map[string]string, id, body string) map[string]any {
	t.Helper()
	if body == "" {
		body = "{}"
	}
	rec := do(t, h, http.MethodPost,
		testPrefix+"/api/admin/key-applications/"+id+"/approve", body, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	_, data, _ := decodeEnvelope(t, rec)
	return data
}

// mintKey creates a named key through the dashboard and returns its plaintext
// token and id.
func mintKey(t *testing.T, h http.Handler, cookie map[string]string, name string) (token, id string) {
	t.Helper()
	rec := do(t, h, http.MethodPost, testPrefix+"/api/admin/keys",
		`{"name":"`+name+`","expiresInHours":24}`, cookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint key: status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	ok, data, _ := decodeEnvelope(t, rec)
	if !ok {
		t.Fatalf("mint key: ok=false: %s", rec.Body.String())
	}
	token, _ = data["token"].(string)
	key, _ := data["key"].(map[string]any)
	id, _ = key["id"].(string)
	if token == "" || id == "" {
		t.Fatalf("mint key: missing token/id in %s", rec.Body.String())
	}
	prefix, _ := key["token_prefix"].(string)
	if prefix == "" || !strings.HasPrefix(token, prefix) {
		t.Errorf("token_prefix %v does not prefix the token", key["token_prefix"])
	}
	return token, id
}

// publishReport posts a report as the given bearer token.
func publishReport(t *testing.T, h http.Handler, token, title string) (reportID, shareToken string) {
	t.Helper()
	rec := do(t, h, http.MethodPost, testPrefix+"/api/reports",
		`{"title":"`+title+`","content":"# `+title+`"}`, bearerHeaders(token))
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish %q: status = %d, want 201: %s", title, rec.Code, rec.Body.String())
	}
	_, data, _ := decodeEnvelope(t, rec)
	reportID, _ = data["id"].(string)
	share, _ := data["share"].(map[string]any)
	shareToken, _ = share["token"].(string)
	if reportID == "" || shareToken == "" {
		t.Fatalf("publish %q: missing id/share token: %s", title, rec.Body.String())
	}
	return reportID, shareToken
}

// ---------------------------------------------------------------------------
// public application lifecycle
// ---------------------------------------------------------------------------

// TestKeyApplicationLifecycle walks submit -> poll -> approve -> claim, which is
// the whole self-service path an agent depends on.
func TestKeyApplicationLifecycle(t *testing.T) {
	h, _ := testServer(t)
	cookie := map[string]string{"Cookie": login(t, h)}

	id, secret := applyForKey(t, h, "lifecycle-agent")

	t.Run("poll without the claim header is 404", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/agent-keys/applications/"+id, "", nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("poll with the wrong claim secret is 404", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/agent-keys/applications/"+id, "",
			claimHeaders("not-the-secret"))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("unknown id is 404 even with a header", func(t *testing.T) {
		rec := do(t, h, http.MethodGet,
			testPrefix+"/api/agent-keys/applications/00000000-0000-4000-8000-000000000000",
			"", claimHeaders(secret))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("pending poll carries no key", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/agent-keys/applications/"+id, "",
			claimHeaders(secret))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		_, data, _ := decodeEnvelope(t, rec)
		if data["status"] != "pending" {
			t.Errorf("status = %v, want pending", data["status"])
		}
		if _, has := data["key"]; has {
			t.Error("pending poll returned a key")
		}
	})

	t.Run("approval records no plaintext and does not mint a key yet", func(t *testing.T) {
		data := approveApplication(t, h, cookie, id, `{"name":"approved-agent","expiresInHours":24}`)
		if data["status"] != "approved" {
			t.Fatalf("status = %v, want approved", data["status"])
		}
		if v, _ := data["decided_at"].(float64); v <= 0 {
			t.Errorf("decided_at = %v, want > 0", data["decided_at"])
		}
		if _, has := data["claim_secret"]; has {
			t.Error("approval echoed a claim secret")
		}
	})

	var derivedToken string
	// The poll that first observes "approved" is the claim: there is no
	// intermediate step where an approved application reports itself without
	// handing over the derived token. That is the whole point of the derivation
	// design — the token is computed from the claim secret the caller presents.
	t.Run("the first poll after approval claims and returns the plaintext once", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/agent-keys/applications/"+id, "",
			claimHeaders(secret))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		_, data, _ := decodeEnvelope(t, rec)
		if data["status"] != "approved" {
			t.Errorf("status = %v, want approved on the claim response", data["status"])
		}
		key, _ := data["key"].(map[string]any)
		if key == nil {
			t.Fatalf("claim response has no key: %s", rec.Body.String())
		}
		derivedToken, _ = key["token"].(string)
		if derivedToken == "" {
			t.Fatal("claim response has an empty token")
		}
		if key["name"] != "approved-agent" {
			t.Errorf("key.name = %v, want approved-agent", key["name"])
		}
		prefix, _ := key["token_prefix"].(string)
		if prefix == "" || !strings.HasPrefix(derivedToken, prefix) {
			t.Errorf("token_prefix %v does not prefix the token", key["token_prefix"])
		}
		if exp, _ := key["expires_at"].(float64); exp <= 0 {
			t.Errorf("expires_at = %v, want a future timestamp", key["expires_at"])
		}
	})

	t.Run("the derived token authenticates", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/agent-keys/me", "", bearerHeaders(derivedToken))
		if rec.Code != http.StatusOK {
			t.Fatalf("me: status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		_, data, _ := decodeEnvelope(t, rec)
		if data["name"] != "approved-agent" {
			t.Errorf("me.name = %v", data["name"])
		}
		if data["root"] != false {
			t.Errorf("me.root = %v, want false", data["root"])
		}
		// The resolver touches the key on every authenticated request, so a
		// count of at least one proves the analytics write is wired in.
		if n, _ := data["request_count"].(float64); n < 1 {
			t.Errorf("request_count = %v, want >= 1", data["request_count"])
		}
	})

	t.Run("second claim is 404", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/agent-keys/applications/"+id, "",
			claimHeaders(secret))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("polling a claimed application is 404", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/agent-keys/applications/"+id, "",
			claimHeaders(secret))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("approving twice conflicts", func(t *testing.T) {
		rec := do(t, h, http.MethodPost,
			testPrefix+"/api/admin/key-applications/"+id+"/approve", `{}`, cookie)
		if rec.Code != http.StatusConflict {
			t.Errorf("status = %d, want 409: %s", rec.Code, rec.Body.String())
		}
	})
}

// TestKeyApplicationRejected checks that a rejection is reported as a status,
// not an error, to the legitimate poller.
func TestKeyApplicationRejected(t *testing.T) {
	h, _ := testServer(t)
	cookie := map[string]string{"Cookie": login(t, h)}

	id, secret := applyForKey(t, h, "rejected-agent")
	rec := do(t, h, http.MethodPost, testPrefix+"/api/admin/key-applications/"+id+"/reject",
		`{"reason":"not now"}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("reject: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	rec = do(t, h, http.MethodGet, testPrefix+"/api/agent-keys/applications/"+id, "",
		claimHeaders(secret))
	if rec.Code != http.StatusOK {
		t.Fatalf("poll: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	_, data, _ := decodeEnvelope(t, rec)
	if data["status"] != "rejected" {
		t.Errorf("status = %v, want rejected", data["status"])
	}
	if _, has := data["key"]; has {
		t.Error("rejected application returned a key")
	}
}

// TestClaimWindowClosedIs410 pins the "approved but too late" outcome.
//
// A negative window makes the deadline already past at approval time, so the
// test needs no sleep and cannot flake.
func TestClaimWindowClosedIs410(t *testing.T) {
	h, _ := testServerWith(t, func(cfg *config.Config) { cfg.KeyClaimWindow = -time.Second })
	cookie := map[string]string{"Cookie": login(t, h)}

	id, secret := applyForKey(t, h, "slow-agent")
	approveApplication(t, h, cookie, id, `{}`)

	rec := do(t, h, http.MethodGet, testPrefix+"/api/agent-keys/applications/"+id, "",
		claimHeaders(secret))
	if rec.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410: %s", rec.Code, rec.Body.String())
	}
	if _, _, code := decodeEnvelope(t, rec); code != "gone" {
		t.Errorf("error code = %q, want gone", code)
	}
}

// TestApplicationTTLExpiryIs410 covers the other 410: the application itself
// timed out before anyone decided.
func TestApplicationTTLExpiryIs410(t *testing.T) {
	h, _ := testServerWith(t, func(cfg *config.Config) { cfg.KeyApplicationTTL = -time.Second })

	id, secret := applyForKey(t, h, "stale-agent")
	rec := do(t, h, http.MethodGet, testPrefix+"/api/agent-keys/applications/"+id, "",
		claimHeaders(secret))
	if rec.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410: %s", rec.Code, rec.Body.String())
	}
}

// TestApplicationRateLimit checks the per-IP hourly budget and that a different
// address has its own.
func TestApplicationRateLimit(t *testing.T) {
	h, _ := testServerWith(t, func(cfg *config.Config) { cfg.KeyApplyPerHour = 2 })

	for i := 0; i < 2; i++ {
		rec := do(t, h, http.MethodPost, testPrefix+"/api/agent-keys/applications",
			`{"label":"burst"}`, nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("request %d: status = %d, want 201: %s", i+1, rec.Code, rec.Body.String())
		}
	}

	rec := do(t, h, http.MethodPost, testPrefix+"/api/agent-keys/applications",
		`{"label":"burst"}`, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", rec.Code, rec.Body.String())
	}
	if _, _, code := decodeEnvelope(t, rec); code != "rate_limited" {
		t.Errorf("error code = %q, want rate_limited", code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 without a Retry-After header")
	}

	t.Run("a forged forwarded header does not buy a fresh bucket", func(t *testing.T) {
		// httptest's default RemoteAddr is 192.0.2.1, which is not a trusted
		// proxy prefix, so every header here is attacker-controlled and must be
		// ignored. This is the regression that made per-IP limiting decorative.
		rec := do(t, h, http.MethodPost, testPrefix+"/api/agent-keys/applications",
			`{"label":"other-ip"}`, map[string]string{"X-Forwarded-For": "203.0.113.9"})
		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("status = %d, want 429 (a forged XFF must not reset the budget): %s",
				rec.Code, rec.Body.String())
		}
	})
}

// doFrom is do() with an explicit RemoteAddr, so a test can decide whether the
// immediate peer is a trusted proxy. The address family is what ClientIP keys on.
func doFrom(t *testing.T, h http.Handler, remoteAddr, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = remoteAddr
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// TestRateLimitUsesClientIPNotForwardedHeaders is the API-level regression for
// the rate-limit bypass: the limiter must bucket on the unspoofable peer when
// there is no trusted proxy, and on the rightmost untrusted XFF entry when there
// is one.
func TestRateLimitUsesClientIPNotForwardedHeaders(t *testing.T) {
	const applyPath = testPrefix + "/api/agent-keys/applications"

	t.Run("untrusted peer ignores Cf-Connecting-Ip", func(t *testing.T) {
		h, _ := testServerWith(t, func(cfg *config.Config) { cfg.KeyApplyPerHour = 2 })

		for i := 0; i < 2; i++ {
			rec := doFrom(t, h, "192.0.2.1:1234", http.MethodPost, applyPath, `{"label":"cf"}`,
				map[string]string{"Cf-Connecting-Ip": "9.9.9.9"})
			if rec.Code != http.StatusCreated {
				t.Fatalf("request %d: status = %d, want 201: %s", i+1, rec.Code, rec.Body.String())
			}
		}
		// A new Cf-Connecting-Ip on every request previously meant a new bucket
		// every request; now the header is inert on a direct connection.
		rec := doFrom(t, h, "192.0.2.1:1234", http.MethodPost, applyPath, `{"label":"cf"}`,
			map[string]string{"Cf-Connecting-Ip": "10.10.10.10"})
		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("status = %d, want 429 (Cf-Connecting-Ip must not reset the budget): %s",
				rec.Code, rec.Body.String())
		}
	})

	t.Run("untrusted peer ignores X-Forwarded-For", func(t *testing.T) {
		h, _ := testServerWith(t, func(cfg *config.Config) { cfg.KeyApplyPerHour = 1 })

		rec := doFrom(t, h, "192.0.2.7:9999", http.MethodPost, applyPath, `{"label":"a"}`,
			map[string]string{"X-Forwarded-For": "203.0.113.1"})
		if rec.Code != http.StatusCreated {
			t.Fatalf("first: status = %d, want 201: %s", rec.Code, rec.Body.String())
		}
		rec = doFrom(t, h, "192.0.2.7:9999", http.MethodPost, applyPath, `{"label":"b"}`,
			map[string]string{"X-Forwarded-For": "203.0.113.2, 203.0.113.3"})
		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("status = %d, want 429 (forged XFF must not reset the budget): %s",
				rec.Code, rec.Body.String())
		}
	})

	t.Run("same untrusted peer is limited after KEY_APPLY_PER_HOUR", func(t *testing.T) {
		h, _ := testServerWith(t, func(cfg *config.Config) { cfg.KeyApplyPerHour = 3 })

		for i := 0; i < 3; i++ {
			rec := doFrom(t, h, "198.51.100.5:443", http.MethodPost, applyPath, `{"label":"peer"}`, nil)
			if rec.Code != http.StatusCreated {
				t.Fatalf("request %d: status = %d, want 201: %s", i+1, rec.Code, rec.Body.String())
			}
		}
		rec := doFrom(t, h, "198.51.100.5:443", http.MethodPost, applyPath, `{"label":"peer"}`, nil)
		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("request 4: status = %d, want 429: %s", rec.Code, rec.Body.String())
		}
		if _, _, code := decodeEnvelope(t, rec); code != "rate_limited" {
			t.Errorf("error code = %q, want rate_limited", code)
		}
	})

	t.Run("trusted proxy buckets on the rightmost untrusted XFF entry", func(t *testing.T) {
		h, _ := testServerWith(t, func(cfg *config.Config) { cfg.KeyApplyPerHour = 1 })
		const peer = "172.17.0.1:5" // docker bridge: inside 172.16.0.0/12

		// Spoofing the leftmost entry does not matter: Caddy appended the real
		// client at the right, and that is the value used.
		rec := doFrom(t, h, peer, http.MethodPost, applyPath, `{"label":"via-proxy"}`,
			map[string]string{"X-Forwarded-For": "1.2.3.4, 8.8.8.8"})
		if rec.Code != http.StatusCreated {
			t.Fatalf("first: status = %d, want 201: %s", rec.Code, rec.Body.String())
		}
		rec = doFrom(t, h, peer, http.MethodPost, applyPath, `{"label":"via-proxy"}`,
			map[string]string{"X-Forwarded-For": "5.6.7.8, 8.8.8.8"})
		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("same rightmost (8.8.8.8): status = %d, want 429: %s", rec.Code, rec.Body.String())
		}

		// A different rightmost value is a different client, so it gets its own
		// bucket even though the trustworthy proxy peer is identical.
		rec = doFrom(t, h, peer, http.MethodPost, applyPath, `{"label":"other-client"}`,
			map[string]string{"X-Forwarded-For": "5.6.7.8, 9.9.9.9"})
		if rec.Code != http.StatusCreated {
			t.Errorf("different rightmost (9.9.9.9): status = %d, want 201: %s", rec.Code, rec.Body.String())
		}
	})
}

// TestRequesterIPIsAuditSafe checks the stored requester_ip comes from the same
// unspoofable source as the limiter: an approver must not see a caller-chosen
// value there.
func TestRequesterIPIsAuditSafe(t *testing.T) {
	h, _ := testServer(t)
	cookie := map[string]string{"Cookie": login(t, h)}
	const applyPath = testPrefix + "/api/agent-keys/applications"

	t.Run("a direct caller cannot forge the recorded IP", func(t *testing.T) {
		rec := doFrom(t, h, "198.51.100.77:5555", http.MethodPost, applyPath, `{"label":"forger"}`,
			map[string]string{
				"X-Forwarded-For":  "1.2.3.4",
				"Cf-Connecting-Ip": "2.3.4.5",
			})
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
		}
		_, data, _ := decodeEnvelope(t, rec)
		id, _ := data["id"].(string)

		rows := decodeArray(t, do(t, h, http.MethodGet,
			testPrefix+"/api/admin/key-applications?status=pending", "", cookie))
		var got string
		for _, row := range rows {
			if row["id"] == id {
				got, _ = row["requester_ip"].(string)
			}
		}
		if got != "198.51.100.77" {
			t.Errorf("requester_ip = %q, want the peer 198.51.100.77 (forged headers ignored)", got)
		}
	})

	t.Run("behind a trusted proxy it records the forwarded client", func(t *testing.T) {
		rec := doFrom(t, h, "172.17.0.1:5", http.MethodPost, applyPath, `{"label":"via-proxy"}`,
			map[string]string{"X-Forwarded-For": "1.2.3.4, 203.0.113.42"})
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
		}
		_, data, _ := decodeEnvelope(t, rec)
		id, _ := data["id"].(string)

		rows := decodeArray(t, do(t, h, http.MethodGet,
			testPrefix+"/api/admin/key-applications?status=pending", "", cookie))
		var got string
		for _, row := range rows {
			if row["id"] == id {
				got, _ = row["requester_ip"].(string)
			}
		}
		if got != "203.0.113.42" {
			t.Errorf("requester_ip = %q, want the rightmost untrusted entry 203.0.113.42", got)
		}
	})
}

// TestMaxPendingApplications checks the queue cap turns into 409, not 500.
func TestMaxPendingApplications(t *testing.T) {
	h, _ := testServerWith(t, func(cfg *config.Config) { cfg.KeyMaxPending = 1 })

	applyForKey(t, h, "first")
	rec := do(t, h, http.MethodPost, testPrefix+"/api/agent-keys/applications",
		`{"label":"second"}`, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if _, _, code := decodeEnvelope(t, rec); code != "too_many_pending" {
		t.Errorf("error code = %q, want too_many_pending", code)
	}
}

// TestConcurrentClaimOnlyOneWins is the atomicity requirement: concurrent
// claimers must see exactly one plaintext hand-over.
func TestConcurrentClaimOnlyOneWins(t *testing.T) {
	h, _ := testServer(t)
	cookie := map[string]string{"Cookie": login(t, h)}

	id, secret := applyForKey(t, h, "race-agent")
	approveApplication(t, h, cookie, id, `{}`)

	const racers = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		winners  int
		tokens   []string
		statuses []int
	)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := do(t, h, http.MethodGet, testPrefix+"/api/agent-keys/applications/"+id,
				"", claimHeaders(secret))
			mu.Lock()
			defer mu.Unlock()
			statuses = append(statuses, rec.Code)
			if rec.Code == http.StatusOK {
				winners++
				var body struct {
					Data struct {
						Key struct {
							Token string `json:"token"`
						} `json:"key"`
					} `json:"data"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err == nil {
					tokens = append(tokens, body.Data.Key.Token)
				}
			}
		}()
	}
	wg.Wait()

	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1 (statuses: %v)", winners, statuses)
	}
	for _, s := range statuses {
		if s != http.StatusOK && s != http.StatusNotFound {
			t.Errorf("racer status = %d, want 200 or 404 (statuses: %v)", s, statuses)
		}
	}
	for _, token := range tokens {
		if token == "" {
			t.Error("winner returned an empty token")
		}
	}
}

// ---------------------------------------------------------------------------
// agent-owned endpoints
// ---------------------------------------------------------------------------

// TestAgentKeyMeAndRenewals covers /me and the renewal request/poll/approve
// cycle, including the human-approval requirement.
func TestAgentKeyMeAndRenewals(t *testing.T) {
	h, _ := testServer(t)
	cookie := map[string]string{"Cookie": login(t, h)}
	token, keyID := mintKey(t, h, cookie, "renewable-agent")

	t.Run("unauthenticated /me is 401", func(t *testing.T) {
		for name, headers := range map[string]map[string]string{
			"none":      nil,
			"bad token": bearerHeaders("nope"),
		} {
			rec := do(t, h, http.MethodGet, testPrefix+"/api/agent-keys/me", "", headers)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s: status = %d, want 401", name, rec.Code)
			}
		}
	})

	t.Run("root reports itself", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/agent-keys/me", "", agentHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		_, data, _ := decodeEnvelope(t, rec)
		if data["root"] != true {
			t.Errorf("root = %v, want true", data["root"])
		}
	})

	t.Run("bearer key sees itself", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/agent-keys/me", "", bearerHeaders(token))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		_, data, _ := decodeEnvelope(t, rec)
		if data["id"] != keyID {
			t.Errorf("id = %v, want %v", data["id"], keyID)
		}
		if data["name"] != "renewable-agent" {
			t.Errorf("name = %v", data["name"])
		}
		if _, has := data["token_hash"]; has {
			t.Error("me returned token_hash")
		}
	})

	var renewalID, renewalSecret string
	t.Run("a renewal is filed pending", func(t *testing.T) {
		rec := do(t, h, http.MethodPost, testPrefix+"/api/agent-keys/renewals",
			`{"requestedHours":48}`, bearerHeaders(token))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
		}
		_, data, _ := decodeEnvelope(t, rec)
		renewalID, _ = data["id"].(string)
		renewalSecret, _ = data["claim_secret"].(string)
		if renewalID == "" || renewalSecret == "" {
			t.Fatalf("missing id/claim_secret: %s", rec.Body.String())
		}
		if data["status"] != "pending" {
			t.Errorf("status = %v, want pending", data["status"])
		}
	})

	t.Run("only the holder may poll the renewal", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/agent-keys/renewals/"+renewalID, "",
			claimHeaders("wrong"))
		if rec.Code != http.StatusNotFound {
			t.Errorf("wrong secret: status = %d, want 404", rec.Code)
		}
		rec = do(t, h, http.MethodGet, testPrefix+"/api/agent-keys/renewals/"+renewalID, "", nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("missing header: status = %d, want 404", rec.Code)
		}
		rec = do(t, h, http.MethodGet, testPrefix+"/api/agent-keys/renewals/"+renewalID, "",
			claimHeaders(renewalSecret))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		_, data, _ := decodeEnvelope(t, rec)
		if data["status"] != "pending" {
			t.Errorf("status = %v, want pending", data["status"])
		}
	})

	t.Run("unknown renewal id is 404", func(t *testing.T) {
		rec := do(t, h, http.MethodGet,
			testPrefix+"/api/agent-keys/renewals/00000000-0000-4000-8000-000000000000", "",
			claimHeaders(renewalSecret))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("a second pending renewal conflicts", func(t *testing.T) {
		rec := do(t, h, http.MethodPost, testPrefix+"/api/agent-keys/renewals",
			`{"requestedHours":48}`, bearerHeaders(token))
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
		}
		if _, _, code := decodeEnvelope(t, rec); code != "renewal_exists" {
			t.Errorf("error code = %q, want renewal_exists", code)
		}
	})

	t.Run("admin sees the pending renewal and approves it", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/admin/key-renewals?status=pending", "", cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		rows := decodeArray(t, rec)
		if len(rows) != 1 || rows[0]["id"] != renewalID {
			t.Fatalf("pending renewals = %+v, want the one just filed", rows)
		}

		rec = do(t, h, http.MethodPost, testPrefix+"/api/admin/key-renewals/"+renewalID+"/approve",
			`{"expiresInHours":72}`, cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("approve: status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		_, data, _ := decodeEnvelope(t, rec)
		if data["status"] != "approved" {
			t.Errorf("status = %v, want approved", data["status"])
		}
		if v, _ := data["granted_expires_at"].(float64); v <= 0 {
			t.Errorf("granted_expires_at = %v, want > 0", data["granted_expires_at"])
		}
	})

	t.Run("the key was actually extended", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/agent-keys/me", "", bearerHeaders(token))
		_, data, _ := decodeEnvelope(t, rec)
		remaining, _ := data["expires_at"].(float64)
		nowMS := float64(time.Now().UnixMilli())
		if remaining-nowMS < float64(70*time.Hour/time.Millisecond) {
			t.Errorf("expires_at is %v ms away, want ~72h", remaining-nowMS)
		}
	})

	t.Run("the renewal poll shows the new expiry", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/agent-keys/renewals/"+renewalID, "",
			claimHeaders(renewalSecret))
		_, data, _ := decodeEnvelope(t, rec)
		if data["status"] != "approved" {
			t.Errorf("status = %v, want approved", data["status"])
		}
	})
}

// TestRevokedKeyCannotAuthenticateOrRenew checks revocation is immediate and
// that a revoked key cannot be brought back by a renewal approval.
func TestRevokedKeyCannotAuthenticateOrRenew(t *testing.T) {
	h, _ := testServer(t)
	cookie := map[string]string{"Cookie": login(t, h)}
	token, keyID := mintKey(t, h, cookie, "doomed-agent")

	rec := do(t, h, http.MethodPost, testPrefix+"/api/agent-keys/renewals",
		`{"requestedHours":24}`, bearerHeaders(token))
	if rec.Code != http.StatusCreated {
		t.Fatalf("file renewal: status = %d: %s", rec.Code, rec.Body.String())
	}
	_, data, _ := decodeEnvelope(t, rec)
	renewalID, _ := data["id"].(string)

	rec = do(t, h, http.MethodPatch, testPrefix+"/api/admin/keys/"+keyID, `{"revoked":true}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: status = %d: %s", rec.Code, rec.Body.String())
	}
	_, data, _ = decodeEnvelope(t, rec)
	if v, _ := data["revoked_at"].(float64); v <= 0 {
		t.Errorf("revoked_at = %v, want > 0", data["revoked_at"])
	}

	rec = do(t, h, http.MethodGet, testPrefix+"/api/agent-keys/me", "", bearerHeaders(token))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("revoked key: status = %d, want 401", rec.Code)
	}

	rec = do(t, h, http.MethodPost, testPrefix+"/api/admin/key-renewals/"+renewalID+"/approve",
		`{}`, cookie)
	if rec.Code != http.StatusNotFound {
		t.Errorf("approving a renewal for a revoked key: status = %d, want 404: %s",
			rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// dashboard key CRUD
// ---------------------------------------------------------------------------

// TestAdminKeyEndpointsRequireSession checks every new dashboard route is
// Session-gated and answers 401 — not the SPA shell — without a cookie.
func TestAdminKeyEndpointsRequireSession(t *testing.T) {
	h, _ := testServer(t)

	cases := []struct{ method, path, body string }{
		{http.MethodGet, "/api/admin/keys", ""},
		{http.MethodPost, "/api/admin/keys", `{"name":"x","expiresInHours":1}`},
		{http.MethodPatch, "/api/admin/keys/abc", `{"name":"y"}`},
		{http.MethodGet, "/api/admin/key-applications", ""},
		{http.MethodPost, "/api/admin/key-applications/abc/approve", `{}`},
		{http.MethodPost, "/api/admin/key-applications/abc/reject", `{}`},
		{http.MethodGet, "/api/admin/key-renewals", ""},
		{http.MethodPost, "/api/admin/key-renewals/abc/approve", `{}`},
		{http.MethodPost, "/api/admin/key-renewals/abc/reject", `{}`},
	}
	for _, tc := range cases {
		rec := do(t, h, tc.method, testPrefix+tc.path, tc.body, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401: %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s %s: Content-Type = %q, want JSON", tc.method, tc.path, ct)
		}
	}
}

// TestAdminKeyCRUD covers mint, list, patch and the two hard caps.
func TestAdminKeyCRUD(t *testing.T) {
	h, _ := testServer(t)
	cookie := map[string]string{"Cookie": login(t, h)}

	token, id := mintKey(t, h, cookie, "crud-agent")

	t.Run("list is a bare array without hashes", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/admin/keys", "", cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		rows := decodeArray(t, rec)
		if len(rows) != 1 {
			t.Fatalf("keys = %d, want 1", len(rows))
		}
		if rows[0]["id"] != id {
			t.Errorf("id = %v, want %v", rows[0]["id"], id)
		}
		assertNoHashLeak(t, "GET /api/admin/keys", rec.Body.String())
	})

	t.Run("rename and re-expire", func(t *testing.T) {
		rec := do(t, h, http.MethodPatch, testPrefix+"/api/admin/keys/"+id,
			`{"name":"renamed-agent","expiresInHours":100}`, cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		_, data, _ := decodeEnvelope(t, rec)
		if data["name"] != "renamed-agent" {
			t.Errorf("name = %v, want renamed-agent", data["name"])
		}
		if v, _ := data["expires_at"].(float64); v-float64(time.Now().UnixMilli()) <
			float64(99*time.Hour/time.Millisecond) {
			t.Errorf("expires_at was not extended: %v", data["expires_at"])
		}
	})

	t.Run("expiresInHours=0 means never", func(t *testing.T) {
		rec := do(t, h, http.MethodPatch, testPrefix+"/api/admin/keys/"+id,
			`{"expiresInHours":0}`, cookie)
		_, data, _ := decodeEnvelope(t, rec)
		if data["expires_at"] != float64(0) {
			t.Errorf("expires_at = %v, want 0 (never)", data["expires_at"])
		}
	})

	t.Run("empty patch is 400", func(t *testing.T) {
		rec := do(t, h, http.MethodPatch, testPrefix+"/api/admin/keys/"+id, `{}`, cookie)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("patching an unknown key is 404", func(t *testing.T) {
		rec := do(t, h, http.MethodPatch, testPrefix+"/api/admin/keys/does-not-exist",
			`{"name":"x"}`, cookie)
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("the minted token works", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/agent-keys/me", "", bearerHeaders(token))
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
	})
}

// TestMaxActiveKeys checks the live-credential cap.
func TestMaxActiveKeys(t *testing.T) {
	h, _ := testServerWith(t, func(cfg *config.Config) { cfg.KeyMaxActive = 1 })
	cookie := map[string]string{"Cookie": login(t, h)}

	mintKey(t, h, cookie, "only-one")
	rec := do(t, h, http.MethodPost, testPrefix+"/api/admin/keys",
		`{"name":"too-many","expiresInHours":1}`, cookie)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if _, _, code := decodeEnvelope(t, rec); code != "too_many_active_keys" {
		t.Errorf("error code = %q, want too_many_active_keys", code)
	}
}

// TestClaimBlockedAtActiveCap covers the claim-time half of KEY_MAX_ACTIVE.
//
// The store enforces the cap atomically when a human mints a key; on the claim
// path the API can only pre-check it, so this pins the observable behaviour and
// the recovery path (revoke one, then the claim goes through).
func TestClaimBlockedAtActiveCap(t *testing.T) {
	h, _ := testServerWith(t, func(cfg *config.Config) { cfg.KeyMaxActive = 1 })
	cookie := map[string]string{"Cookie": login(t, h)}

	_, existingKeyID := mintKey(t, h, cookie, "occupies-the-slot")
	id, secret := applyForKey(t, h, "capped-agent")
	approveApplication(t, h, cookie, id, `{}`)

	rec := do(t, h, http.MethodGet, testPrefix+"/api/agent-keys/applications/"+id, "",
		claimHeaders(secret))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if _, _, code := decodeEnvelope(t, rec); code != "too_many_active_keys" {
		t.Errorf("error code = %q, want too_many_active_keys", code)
	}

	// Freeing a slot must let the same application be claimed: the failed
	// attempt must not have consumed it.
	rec = do(t, h, http.MethodPatch, testPrefix+"/api/admin/keys/"+existingKeyID,
		`{"revoked":true}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: status = %d: %s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, http.MethodGet, testPrefix+"/api/agent-keys/applications/"+id, "",
		claimHeaders(secret))
	if rec.Code != http.StatusOK {
		t.Fatalf("claim after freeing a slot: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// TestAdminKeyApplicationQueue checks the queue endpoint's filtering and that a
// malformed status is rejected rather than silently treated as "all".
func TestAdminKeyApplicationQueue(t *testing.T) {
	h, _ := testServer(t)
	cookie := map[string]string{"Cookie": login(t, h)}

	id, _ := applyForKey(t, h, "queued-agent")

	rec := do(t, h, http.MethodGet, testPrefix+"/api/admin/key-applications", "", cookie)
	rows := decodeArray(t, rec)
	if len(rows) != 1 || rows[0]["id"] != id {
		t.Fatalf("default (pending) queue = %+v", rows)
	}
	if rows[0]["requester_ip"] == "" {
		t.Error("requester_ip missing from the queue row")
	}
	assertNoHashLeak(t, "GET /api/admin/key-applications", rec.Body.String())

	approveApplication(t, h, cookie, id, `{}`)

	rec = do(t, h, http.MethodGet, testPrefix+"/api/admin/key-applications", "", cookie)
	if rows := decodeArray(t, rec); len(rows) != 0 {
		t.Errorf("pending queue still has %d row(s) after approval", len(rows))
	}
	rec = do(t, h, http.MethodGet, testPrefix+"/api/admin/key-applications?status=approved", "", cookie)
	if rows := decodeArray(t, rec); len(rows) != 1 {
		t.Errorf("approved queue = %d rows, want 1", len(rows))
	}

	rec = do(t, h, http.MethodGet, testPrefix+"/api/admin/key-applications?status=bogus", "", cookie)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bogus status: status = %d, want 400", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// ownership
// ---------------------------------------------------------------------------

// TestReportOwnershipIsolation is the binding rule from the decision record: a
// key may read or revoke only its own reports, and a non-owner sees 404.
func TestReportOwnershipIsolation(t *testing.T) {
	h, _ := testServer(t)
	cookie := map[string]string{"Cookie": login(t, h)}

	tokenA, _ := mintKey(t, h, cookie, "agent-a")
	tokenB, _ := mintKey(t, h, cookie, "agent-b")
	reportA, shareA := publishReport(t, h, tokenA, "Alpha secret")
	reportB, shareB := publishReport(t, h, tokenB, "Beta secret")
	rootReport, rootShare := publishReport(t, h, testAgentSecret, "Root only")

	t.Run("the owner reads its own report", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/reports/"+reportA, "", bearerHeaders(tokenA))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		_, data, _ := decodeEnvelope(t, rec)
		if data["title"] != "Alpha secret" {
			t.Errorf("title = %v", data["title"])
		}
	})

	t.Run("another key gets 404", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/reports/"+reportA, "", bearerHeaders(tokenB))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (not 403): %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("root reads every report", func(t *testing.T) {
		for _, id := range []string{reportA, reportB, rootReport} {
			rec := do(t, h, http.MethodGet, testPrefix+"/api/reports/"+id, "", agentHeaders())
			if rec.Code != http.StatusOK {
				t.Errorf("root read %s: status = %d, want 200", id, rec.Code)
			}
		}
	})

	t.Run("an unknown report is 404 for everyone", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/reports/00000000-0000-4000-8000-000000000000",
			"", bearerHeaders(tokenA))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("a non-owner cannot revoke someone else's share", func(t *testing.T) {
		rec := do(t, h, http.MethodPost, testPrefix+"/api/share/"+shareA+"/revoke", "",
			bearerHeaders(tokenB))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (not 403): %s", rec.Code, rec.Body.String())
		}
		// The failed attempt must not have taken effect.
		rec = do(t, h, http.MethodGet, testPrefix+"/api/share/"+shareA, "", nil)
		if rec.Code != http.StatusOK {
			t.Errorf("share link was revoked by a non-owner: status = %d", rec.Code)
		}
	})

	t.Run("the owner can revoke its own share", func(t *testing.T) {
		rec := do(t, h, http.MethodPost, testPrefix+"/api/share/"+shareA+"/revoke", "",
			bearerHeaders(tokenA))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		rec = do(t, h, http.MethodGet, testPrefix+"/api/share/"+shareA, "", nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("revoked share: status = %d, want 404", rec.Code)
		}
	})

	t.Run("root can revoke anyone's share", func(t *testing.T) {
		rec := do(t, h, http.MethodPost, testPrefix+"/api/share/"+shareB+"/revoke", "", agentHeaders())
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		rec = do(t, h, http.MethodPost, testPrefix+"/api/share/"+rootShare+"/revoke", "", agentHeaders())
		if rec.Code != http.StatusOK {
			t.Errorf("root share: status = %d, want 200", rec.Code)
		}
	})
}

// TestCrossKeyReadingIs404Not403ForSharedReports is the leak check for the report
// behind a share link: a key must not be able to confirm its existence either.
func TestCrossKeyReadingIs404Not403ForSharedReports(t *testing.T) {
	h, _ := testServer(t)
	cookie := map[string]string{"Cookie": login(t, h)}
	tokenA, _ := mintKey(t, h, cookie, "reader-a")
	tokenB, _ := mintKey(t, h, cookie, "reader-b")
	_, shareA := publishReport(t, h, tokenA, "Shared")

	// B holds the public share token, which is enough to read the share payload
	// but must not grant the agent view.
	rec := do(t, h, http.MethodGet, testPrefix+"/api/share/"+shareA, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("public share read failed: %d", rec.Code)
	}
	rec = do(t, h, http.MethodGet, testPrefix+"/api/reports/"+shareA, "", bearerHeaders(tokenB))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// leak scan + routing
// ---------------------------------------------------------------------------

// TestKeyEndpointsNeverLeakTokenHash walks every new endpoint (success and
// failure) and asserts no response contains a sha256-shaped digest.
//
// A test that only checks the happy path would miss exactly the endpoint that
// serializes a whole domain row, which is where a json tag mistake leaks.
func TestKeyEndpointsNeverLeakTokenHash(t *testing.T) {
	h, _ := testServer(t)
	cookie := map[string]string{"Cookie": login(t, h)}

	token, keyID := mintKey(t, h, cookie, "leak-scan-agent")
	appID, appSecret := applyForKey(t, h, "leak-scan-app")

	renewalRec := do(t, h, http.MethodPost, testPrefix+"/api/agent-keys/renewals",
		`{"requestedHours":1}`, bearerHeaders(token))
	_, renewalData, _ := decodeEnvelope(t, renewalRec)
	renewalID, _ := renewalData["id"].(string)
	renewalSecret, _ := renewalData["claim_secret"].(string)

	type probe struct {
		label   string
		method  string
		path    string
		body    string
		headers map[string]string
	}
	probes := []probe{
		{"apply", http.MethodPost, "/api/agent-keys/applications", `{"label":"leak-scan-2"}`, nil},
		{"apply rate-limited/malformed", http.MethodPost, "/api/agent-keys/applications", `{}`, nil},
		{"poll pending", http.MethodGet, "/api/agent-keys/applications/" + appID, "", claimHeaders(appSecret)},
		{"poll wrong secret", http.MethodGet, "/api/agent-keys/applications/" + appID, "", claimHeaders("x")},
		{"poll unknown", http.MethodGet, "/api/agent-keys/applications/nope", "", claimHeaders(appSecret)},
		{"approve", http.MethodPost, "/api/admin/key-applications/" + appID + "/approve", `{}`, cookie},
		{"poll approved", http.MethodGet, "/api/agent-keys/applications/" + appID, "", claimHeaders(appSecret)},
		{"claim", http.MethodGet, "/api/agent-keys/applications/" + appID, "", claimHeaders(appSecret)},
		{"claim again", http.MethodGet, "/api/agent-keys/applications/" + appID, "", claimHeaders(appSecret)},
		{"reject", http.MethodPost, "/api/admin/key-applications/" + appID + "/reject", `{}`, cookie},
		{"me", http.MethodGet, "/api/agent-keys/me", "", bearerHeaders(token)},
		{"me root", http.MethodGet, "/api/agent-keys/me", "", agentHeaders()},
		{"me unauthorized", http.MethodGet, "/api/agent-keys/me", "", bearerHeaders("bad")},
		{"renewal poll", http.MethodGet, "/api/agent-keys/renewals/" + renewalID, "", claimHeaders(renewalSecret)},
		{"renewal poll wrong", http.MethodGet, "/api/agent-keys/renewals/" + renewalID, "", claimHeaders("x")},
		{"renewal poll unknown", http.MethodGet, "/api/agent-keys/renewals/nope", "", claimHeaders("x")},
		{"renewal duplicate", http.MethodPost, "/api/agent-keys/renewals", `{"requestedHours":1}`, bearerHeaders(token)},
		{"list keys", http.MethodGet, "/api/admin/keys", "", cookie},
		{"create key", http.MethodPost, "/api/admin/keys", `{"name":"scan-mint","expiresInHours":1}`, cookie},
		{"create key invalid", http.MethodPost, "/api/admin/keys", `{}`, cookie},
		{"patch key", http.MethodPatch, "/api/admin/keys/" + keyID, `{"name":"patched"}`, cookie},
		{"patch missing key", http.MethodPatch, "/api/admin/keys/nope", `{"name":"x"}`, cookie},
		{"list applications", http.MethodGet, "/api/admin/key-applications", "", cookie},
		{"list applications bad status", http.MethodGet, "/api/admin/key-applications?status=bogus", "", cookie},
		{"list renewals", http.MethodGet, "/api/admin/key-renewals", "", cookie},
		{"approve renewal", http.MethodPost, "/api/admin/key-renewals/" + renewalID + "/approve", `{}`, cookie},
		{"approve renewal again", http.MethodPost, "/api/admin/key-renewals/" + renewalID + "/approve", `{}`, cookie},
		{"reject unknown renewal", http.MethodPost, "/api/admin/key-renewals/nope/reject", `{}`, cookie},
		{"unknown agent-key route", http.MethodGet, "/api/agent-keys/whatever", "", nil},
	}

	for _, p := range probes {
		rec := do(t, h, p.method, testPrefix+p.path, p.body, p.headers)
		assertNoHashLeak(t, p.label, rec.Body.String())
	}
}

// TestNewRoutesAreNotSwallowedBySPA is the routing proof: with a frontend build
// mounted, the SPA owns the prefix root, and every new route must still reach
// its handler instead of returning the shell.
func TestNewRoutesAreNotSwallowedBySPA(t *testing.T) {
	h, cfg := testServerWithFrontend(t)
	if cfg.StaticDir == "" {
		t.Fatal("test setup error: expected a frontend to be wired in")
	}

	// The public apply endpoint is the sharpest case: it is a real 201 and a
	// client that received HTML here would parse-fail rather than fail cleanly.
	rec := do(t, h, http.MethodPost, testPrefix+"/api/agent-keys/applications",
		`{"label":"spa-route-agent"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("apply through the SPA stack: status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Errorf("apply Content-Type = %q, want JSON", rec.Header().Get("Content-Type"))
	}
	_, data, _ := decodeEnvelope(t, rec)
	appID, _ := data["id"].(string)

	routes := []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/api/agent-keys/applications/" + appID, http.StatusNotFound}, // no claim header
		{http.MethodGet, "/api/agent-keys/renewals/nope", http.StatusNotFound},
		{http.MethodGet, "/api/agent-keys/me", http.StatusUnauthorized},
		{http.MethodPost, "/api/agent-keys/renewals", http.StatusUnauthorized},
		{http.MethodGet, "/api/admin/keys", http.StatusUnauthorized},
		{http.MethodPost, "/api/admin/keys", http.StatusUnauthorized},
		{http.MethodGet, "/api/admin/key-applications", http.StatusUnauthorized},
		{http.MethodGet, "/api/admin/key-renewals", http.StatusUnauthorized},
		{http.MethodGet, "/api/agent-keys/nope", http.StatusNotFound},
	}
	for _, rt := range routes {
		rec := do(t, h, rt.method, testPrefix+rt.path, "", nil)
		if rec.Code != rt.want {
			t.Errorf("%s %s: status = %d, want %d: %s", rt.method, rt.path, rec.Code, rt.want, rec.Body.String())
		}
		if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Errorf("%s %s: Content-Type = %q, want JSON (SPA must not answer API paths)",
				rt.method, rt.path, rec.Header().Get("Content-Type"))
		}
		if strings.Contains(rec.Body.String(), "<!DOCTYPE html>") {
			t.Errorf("%s %s: served the SPA shell", rt.method, rt.path)
		}
	}

	// A client-side route must still get the shell, so the frontend was not
	// accidentally shadowed either.
	rec = do(t, h, http.MethodGet, testPrefix+"/dashboard/keys", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<!DOCTYPE html>") {
		t.Errorf("SPA route: status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// TestKeyApplicationRejectionOfBadInput covers the request-validation edge that
// matters most: a missing label must not create a row.
func TestKeyApplicationRejectionOfBadInput(t *testing.T) {
	h, _ := testServer(t)

	for name, body := range map[string]string{
		"missing label":    `{}`,
		"blank label":      `{"label":"   "}`,
		"negative hours":   `{"label":"x","requestedHours":-1}`,
		"non-string label": `{"label":42}`,
	} {
		rec := do(t, h, http.MethodPost, testPrefix+"/api/agent-keys/applications", body, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %s", name, rec.Code, rec.Body.String())
		}
	}
}

// TestRenewalClaimSecretIsDerivedNotStored documents the one place the API adds
// a derivation: the schema has no claim column for renewals, so the poll secret
// must be recomputable. Two renewals for the same key must not share a secret.
func TestRenewalClaimSecretIsDerivedNotStored(t *testing.T) {
	s := &Server{kDerive: agentkey.KDerive("session-secret")}
	a := s.renewalClaimSecret("renewal-a", "key-1")
	b := s.renewalClaimSecret("renewal-b", "key-1")
	if a == "" || a == b {
		t.Fatalf("renewal secrets must differ per id: %q vs %q", a, b)
	}
	if again := s.renewalClaimSecret("renewal-a", "key-1"); again != a {
		t.Errorf("derivation is not deterministic: %q vs %q", again, a)
	}
	if other := s.renewalClaimSecret("renewal-a", "key-2"); other == a {
		t.Error("renewal secret does not depend on the key id")
	}
}
