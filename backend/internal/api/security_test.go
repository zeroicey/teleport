package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zeroicey/teleport/backend/internal/config"
)

// These tests cover the *mounting* of the abuse controls, not their mechanics
// (that is internal/httpx/guard_test.go). The distinction matters because of this
// project's recorded pitfall: a middleware that exists, is correct, and has
// passing unit tests while never being attached to a route provides zero
// security and looks identical to a deliberate public endpoint. Every assertion
// below therefore drives a real request through the assembled handler.
//
// The other half of the same trap is the opposite mistake: a guard mounted too
// widely, so the *frozen* contract (a share page is readable, a hand-off link
// works) breaks under normal use. Several tests here assert that the budget is
// generous enough for real traffic, and one asserts the whole thing is inert when
// switched off.

// securityTestConfig turns the guards on with the smallest budgets that still
// demonstrate each class, so a test cannot pass by exhausting a shared bucket.
func securityTestConfig(login, publicRead, write, global int) func(*config.Config) {
	return func(cfg *config.Config) {
		cfg.SecurityMiddleware = true
		cfg.LoginRateLimit = login
		cfg.PublicReadRateLimit = publicRead
		cfg.WriteRateLimit = write
		cfg.GlobalRateLimit = global
		// Keep the ban list out of the way unless a test is about banning: a
		// temporary ban would make later assertions in the same test confusing.
		cfg.BanAfterRefusals = 0
		cfg.BanStrikeTTL = 10 * time.Minute
		cfg.BanTTL = 30 * time.Minute
	}
}

// TestLoginIsRateLimited is the highest-value guard in the set: one password
// protects the whole dashboard, and each attempt costs a 600k-iteration PBKDF2
// verification.
func TestLoginIsRateLimited(t *testing.T) {
	h, _ := testServerWith(t, securityTestConfig(2, 1000, 1000, 100_000))

	attempt := func() *httptest.ResponseRecorder {
		return do(t, h, http.MethodPost, testPrefix+"/api/admin/login",
			`{"password":"wrong"}`, nil)
	}

	// The first two attempts are *processed* (and answer 401 for a wrong
	// password); only after the budget is spent does the limiter refuse.
	for i := 0; i < 2; i++ {
		if rec := attempt(); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401 (a real, failed login)", i+1, rec.Code)
		}
	}

	rec := attempt()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d after the login budget was spent, want 429: %s",
			rec.Code, rec.Body.String())
	}
	if _, _, code := decodeEnvelope(t, rec); code != "rate_limited" {
		t.Errorf("error code = %q, want rate_limited", code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 without a Retry-After header")
	}
}

// TestLoginRateLimitRunsBeforePasswordVerification is what makes the login limit
// a defence rather than decoration. A limiter placed after the handler's
// verification would still answer 429 while having burned the CPU on every
// attempt — exactly the exhaustion vector this is meant to close.
//
// The assertion is behavioural: with the budget spent, a *correct* password must
// be refused too. If verification ran first, a correct password would succeed.
func TestLoginRateLimitRunsBeforePasswordVerification(t *testing.T) {
	h, _ := testServerWith(t, securityTestConfig(1, 1000, 1000, 100_000))

	// Spend the budget on a wrong password.
	if rec := do(t, h, http.MethodPost, testPrefix+"/api/admin/login",
		`{"password":"wrong"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	// Now the correct password. It must be refused by the limiter, not accepted
	// by the handler.
	rec := do(t, h, http.MethodPost, testPrefix+"/api/admin/login",
		`{"password":"`+testPassword+`"}`, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d with the budget spent and a CORRECT password, want 429 — "+
			"the limiter is running after password verification, so a flood still costs the server "+
			"600k PBKDF2 iterations per attempt: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Header().Get("Set-Cookie"), "teleport_session") {
		t.Error("a rate-limited login issued a session cookie")
	}
}

// TestPublicReadsAreRateLimited covers the anonymous read surface, which is what
// an actual visitor uses and what a scraper would hammer.
func TestPublicReadsAreRateLimited(t *testing.T) {
	h, _ := testServerWith(t, securityTestConfig(1000, 2, 1000, 100_000))

	// /ai.md, not /api/health: the health probe is deliberately NOT in this
	// class (see the note in router.go — it is what systemd and Caddy poll, and
	// limiting it would let an attacker make the service look dead).
	read := func() *httptest.ResponseRecorder {
		return do(t, h, http.MethodGet, testPrefix+"/ai.md", "", nil)
	}

	for i := 0; i < 2; i++ {
		if rec := read(); rec.Code != http.StatusOK {
			t.Fatalf("read %d: status = %d, want 200", i+1, rec.Code)
		}
	}
	if rec := read(); rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d after the read budget was spent, want 429", rec.Code)
	}
}

// TestPublicReadBudgetCoversNormalBrowsing is the "do not break normal use"
// half.
//
// The default is checked against what a real reader does rather than against a
// round number. What is counted in this class is the *documents*: the share page
// HTML, the framed report body, and the JSON share read. Hashed bundles, fonts
// and the favicon are served by the SPA handler, which is not behind any limiter,
// so a page view costs one or two counted requests. The default therefore has to
// clear a whole browsing session by a wide margin, not merely one page load.
func TestPublicReadBudgetCoversNormalBrowsing(t *testing.T) {
	// The production default, not a test-specific number.
	const defaultReadLimit = 240
	// A generous session: opening a couple of reports, reloading, plus the
	// framed body of an html report.
	const requestsInASession = 20

	if defaultReadLimit < requestsInASession*5 {
		t.Fatalf("PUBLIC_READ_RATE_LIMIT default (%d) leaves less than 5x headroom over "+
			"a %d-request browsing session", defaultReadLimit, requestsInASession)
	}

	// And the limiter really does allow a session's worth in a row.
	h, _ := testServerWith(t, securityTestConfig(1000, defaultReadLimit, 1000, 100_000))
	for i := 0; i < requestsInASession; i++ {
		if rec := do(t, h, http.MethodGet, testPrefix+"/ai.md", "", nil); rec.Code != http.StatusOK {
			t.Fatalf("read %d of %d refused; the default read budget is too tight for a real session",
				i+1, requestsInASession)
		}
	}
}

// TestSPAAssetsAreNotRateLimited pins the deliberate exclusion of static assets
// from the read class.
//
// Putting hashed bundles behind a shared per-IP budget would mean a shared egress
// address (a university, a mobile carrier NAT) could exhaust it and be served a
// panel with no stylesheet or no JavaScript — a broken page that looks like an
// application bug. Assets are also the cheapest thing this process serves: the
// bytes come from an embedded filesystem with no database access at all.
func TestSPAAssetsAreNotRateLimited(t *testing.T) {
	// A read budget of ONE: the very next limited request must be refused.
	h, _ := testServerWith(t, securityTestConfig(1000, 1, 1000, 100_000))

	// Spend the one read. Use a route that is in the class.
	do(t, h, http.MethodGet, testPrefix+"/ai.md", "", nil)

	// The SPA fallback (not a limited route) must still answer, however many
	// times it is asked. This is what a bundle request looks like.
	for i := 0; i < 10; i++ {
		rec := do(t, h, http.MethodGet, testPrefix+"/assets/index-abc123.js", "", nil)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("SPA asset request %d was rate limited; assets are not in the read class", i+1)
		}
	}
}

// TestWriteRateLimitAppliesToAgentWrites proves the write class reaches the agent
// surface, which is where a runaway loop would originate.
func TestWriteRateLimitAppliesToAgentWrites(t *testing.T) {
	h, _ := testServerWith(t, securityTestConfig(1000, 1000, 2, 100_000))

	// Two deletes of a nonexistent report: 404, but the handler ran and the
	// quota was consumed.
	for i := 0; i < 2; i++ {
		rec := do(t, h, http.MethodDelete, testPrefix+"/api/reports/missing",
			"", agentHeaders())
		if rec.Code != http.StatusNotFound {
			t.Fatalf("write %d: status = %d, want 404", i+1, rec.Code)
		}
	}

	rec := do(t, h, http.MethodDelete, testPrefix+"/api/reports/missing",
		"", agentHeaders())
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d after the write budget was spent, want 429: %s",
			rec.Code, rec.Body.String())
	}
}

// TestReadsAreNotChargedToTheWriteBudget is the separation guard. If the classes
// shared one bucket, a reader paging through the dashboard would spend the budget
// an agent needs to publish.
func TestReadsAreNotChargedToTheWriteBudget(t *testing.T) {
	h, _ := testServerWith(t, securityTestConfig(1000, 1000, 1, 100_000))

	// Spend the entire write budget.
	do(t, h, http.MethodDelete, testPrefix+"/api/reports/missing", "", agentHeaders())
	if rec := do(t, h, http.MethodDelete, testPrefix+"/api/reports/missing",
		"", agentHeaders()); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 after the write budget was spent", rec.Code)
	}

	// Reads must still work: a different class, a different bucket.
	for i := 0; i < 5; i++ {
		if rec := do(t, h, http.MethodGet, testPrefix+"/api/health", "", nil); rec.Code != http.StatusOK {
			t.Fatalf("read %d refused after the WRITE budget was spent: status = %d. "+
				"The classes share a bucket, so unrelated traffic starves the agent API.",
				i+1, rec.Code)
		}
	}
}

// TestBlockedIPIsRefusedEverywhere is the ban-list mounting guard: it must be on
// the transport chain, so a blocked address is refused on paths that do not even
// exist.
func TestBlockedIPIsRefusedEverywhere(t *testing.T) {
	// httptest's default RemoteAddr is 192.0.2.1, which is not a trusted proxy
	// prefix, so it is attributed directly and can be blocked.
	const testIP = "192.0.2.1"

	h, cfg := testServerWith(t, blockedConfig(testIP))
	if len(cfg.BlockedPrefixes) != 1 {
		t.Fatalf("BlockedPrefixes = %v, want exactly one entry", cfg.BlockedPrefixes)
	}

	for _, path := range []string{
		"/",
		"/api/health",
		"/api/does-not-exist",
		"/s/sometoken",
	} {
		rec := do(t, h, http.MethodGet, path, "", nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403 for a blocked address", path, rec.Code)
			continue
		}
		if _, _, code := decodeEnvelope(t, rec); code != "ip_blocked" {
			t.Errorf("%s: error code = %q, want ip_blocked", path, code)
		}
	}
}

// blockedConfig enables the guards with a generous budget and one blocklist
// entry, resolved exactly the way Load() resolves it.
//
// The resolution has to happen inside the mutate hook because the handler is
// built from the config immediately afterwards: a config whose BlockedIPs is set
// but whose BlockedPrefixes is empty would leave the ban list matching nothing,
// and the test would pass against a feature that does not work.
func blockedConfig(entry string) func(*config.Config) {
	return func(cfg *config.Config) {
		securityTestConfig(1000, 1000, 1000, 100_000)(cfg)
		cfg.BlockedIPs = []string{entry}
		if err := cfg.ResolveBlockedPrefixes(); err != nil {
			panic(err)
		}
	}
}

// TestBlocklistDoesNotBlockOtherAddresses is the counterpart guard. A blocklist
// middleware that refused everyone would satisfy the test above perfectly.
func TestBlocklistDoesNotBlockOtherAddresses(t *testing.T) {
	// Deliberately NOT the httptest peer address (192.0.2.1).
	h, _ := testServerWith(t, blockedConfig("203.0.113.0/24"))

	if rec := do(t, h, http.MethodGet, testPrefix+"/api/health", "", nil); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: a blocklist entry for another address must not affect this one",
			rec.Code)
	}
}

// TestSecurityMiddlewareOffIsInert is the disabled-state guard for the whole
// feature.
//
// It matters for two reasons. In development and in the contract harness the
// guards are off, and any limiting there would read as a regression. And an
// operator who sets SECURITY_MIDDLEWARE=false must get a fully unguarded service
// rather than a half-configured one that refuses everything.
func TestSecurityMiddlewareOffIsInert(t *testing.T) {
	h, _ := testServerWith(t, func(cfg *config.Config) {
		cfg.SecurityMiddleware = false
		// Even a blocklist entry must be inert: off means off, not "off except
		// the parts that are inconvenient to disable".
		cfg.BlockedIPs = []string{"192.0.2.1"}
	})

	// Far more requests than any of the default budgets, all from one address.
	for i := 0; i < 500; i++ {
		rec := do(t, h, http.MethodGet, testPrefix+"/api/health", "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d with the guards disabled, want 200", i+1, rec.Code)
		}
	}
}

// TestDashboardCSPIsReportOnlyByDefault proves the policy reaches the shell and
// that it does not yet enforce.
//
// Both halves matter. A CSP that never reaches the response is decoration; a CSP
// that enforces before it has been verified is a panel that breaks with no
// actionable error. The header name is the only difference between those two
// states, so it is asserted directly.
func TestDashboardCSPIsReportOnlyByDefault(t *testing.T) {
	// The SPA handler only exists when a frontend build is wired in, and these
	// headers live on that handler.
	h, _ := testServerWithFrontend(t)

	// A client-side route, i.e. a real page load rather than an asset.
	rec := do(t, h, http.MethodGet, testPrefix+"/dashboard", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for the SPA shell", rec.Code)
	}

	reportOnly := rec.Header().Get("Content-Security-Policy-Report-Only")
	if reportOnly == "" {
		t.Fatal("no Content-Security-Policy-Report-Only on the dashboard shell; the policy is not reaching the panel")
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != "" {
		t.Errorf("enforcing Content-Security-Policy = %q on the dashboard; it must be report-only until verified", got)
	}
	if !strings.Contains(reportOnly, "default-src 'none'") {
		t.Errorf("report-only policy = %q, want a deny-by-default baseline", reportOnly)
	}
	// The panel needs its own bundle and stylesheet, and nothing else.
	for _, want := range []string{"script-src 'self'", "style-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(reportOnly, want) {
			t.Errorf("policy = %q, missing %q", reportOnly, want)
		}
	}
	// A bound style is the most likely future frontend change to break under
	// this policy, so the policy must NOT be silently permissive about it.
	if strings.Contains(reportOnly, "script-src 'self' 'unsafe-inline'") {
		t.Error("policy grants inline script; the panel has no inline script and never should")
	}

	if got := rec.Header().Get("Permissions-Policy"); got == "" {
		t.Error("no Permissions-Policy on the dashboard")
	}
}

// TestSharePageKeepsItsOwnCSP is the separation guard: the dashboard's policy
// must not leak onto the share page, whose policy is much stricter and whose
// framed report body needs a different one again.
func TestSharePageKeepsItsOwnCSP(t *testing.T) {
	h, _ := testServerWithFrontend(t)

	// An unknown token still renders the share *page* (a 404 body, but the
	// document the browser gets is the share page path), so the header is set.
	rec := do(t, h, http.MethodGet, testPrefix+"/s/unknowntoken", "", nil)
	if csp := rec.Header().Get("Content-Security-Policy-Report-Only"); csp != "" {
		t.Errorf("the dashboard's report-only policy leaked onto a share path: %q", csp)
	}
}

// TestGlobalRateLimitDoesNotBanTheCaller is the false-positive guard for the
// process-wide backstop.
//
// This is the level where the property can actually regress: one BanList is shared
// by every rate-limit class, so if the global bucket reported its refusals into
// that list, a global breach would ban whichever callers happened to be in flight.
// During a flood the backstop would then convert "the service is busy" into "these
// users are banned for BAN_TTL" — an outage made worse by the control meant to
// bound it.
//
// The assertion is decisive rather than statistical: BAN_AFTER_REFUSALS is 1, so a
// single attributed strike bans the address for the whole test.
func TestGlobalRateLimitDoesNotBanTheCaller(t *testing.T) {
	h, _ := testServerWith(t, func(cfg *config.Config) {
		// Everything generous except the global bucket, which is 1/second.
		securityTestConfig(1000, 1000, 1000, 1)(cfg)
		cfg.BanAfterRefusals = 1
		cfg.BanStrikeTTL = 10 * time.Minute
		cfg.BanTTL = 30 * time.Minute
	})

	// Exhaust the one-per-second global budget. /ai.md is a read, so its own class
	// is nowhere near its limit — this can only be the global bucket refusing.
	if rec := do(t, h, http.MethodGet, testPrefix+"/ai.md", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", rec.Code)
	}
	refused := false
	for i := 0; i < 5; i++ {
		if rec := do(t, h, http.MethodGet, testPrefix+"/ai.md", "", nil); rec.Code == http.StatusTooManyRequests {
			refused = true
			break
		}
	}
	if !refused {
		t.Fatal("the global bucket never refused; the limit of 1/second is not in force")
	}

	// One second later the window has moved. The address must be served again —
	// not 403 ip_blocked. A 403 here means the global breach was attributed to the
	// caller and it is now banned for 30 minutes.
	time.Sleep(1100 * time.Millisecond)
	rec := do(t, h, http.MethodGet, testPrefix+"/ai.md", "", nil)
	if rec.Code == http.StatusForbidden {
		t.Fatal("the caller was BANNED by a global-bucket refusal: a saturated service would " +
			"ban its own legitimate users for the whole ban TTL")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d after the global window moved, want 200 (a 403 means the global "+
			"bucket fed the shared ban list): %s", rec.Code, rec.Body.String())
	}
}

// TestGlobalRateLimitIsSharedAcrossAddresses is the api-level counterpart of the
// httpx-level test: it proves the global bucket is wired as the global bucket and
// not accidentally as another per-address class.
func TestGlobalRateLimitIsSharedAcrossAddresses(t *testing.T) {
	h, _ := testServerWith(t, func(cfg *config.Config) {
		securityTestConfig(1000, 1000, 1000, 1)(cfg)
		cfg.BanAfterRefusals = 0
	})

	refused := 0
	for i := 0; i < 6; i++ {
		// A distinct source address each time. httptest's peer is inside no trusted
		// proxy prefix, so each of these is attributed directly.
		addr := "203.0.113." + strconv.Itoa(i+1) + ":1234"
		if rec := doFrom(t, h, addr, http.MethodGet, testPrefix+"/ai.md", "", nil); rec.Code == http.StatusTooManyRequests {
			refused++
		}
	}
	if refused == 0 {
		t.Error("six requests from six different addresses were all allowed with GLOBAL_RATE_LIMIT=1; " +
			"the global bucket is being keyed per address, so it cannot bound a caller with many addresses")
	}
}
