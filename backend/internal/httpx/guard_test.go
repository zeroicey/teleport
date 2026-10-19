package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"time"
)

// --- Limiter ---------------------------------------------------------------

// TestLimiterAllowsUpToTheLimitThenRefuses pins the budget itself: N allowed,
// the next refused. Without this the whole middleware could be a no-op that
// still returns the right envelope shape.
func TestLimiterAllowsUpToTheLimitThenRefuses(t *testing.T) {
	l := NewLimiter(3, time.Minute)

	for i := 0; i < 3; i++ {
		if allowed, _ := l.Allow("a"); !allowed {
			t.Fatalf("request %d refused; the limit is 3", i+1)
		}
	}
	allowed, wait := l.Allow("a")
	if allowed {
		t.Fatal("4th request allowed; the limit is 3")
	}
	if wait <= 0 {
		t.Errorf("wait = %v, want a positive time until the oldest hit ages out", wait)
	}
	if wait > time.Minute {
		t.Errorf("wait = %v, want at most the window", wait)
	}
}

// TestLimiterKeysAreIndependent is the property that makes the control per-IP
// rather than global: one exhausted bucket must not affect another address.
func TestLimiterKeysAreIndependent(t *testing.T) {
	l := NewLimiter(1, time.Minute)

	if allowed, _ := l.Allow("a"); !allowed {
		t.Fatal("first request from a refused")
	}
	if allowed, _ := l.Allow("a"); allowed {
		t.Fatal("second request from a refused")
	}
	if allowed, _ := l.Allow("b"); !allowed {
		t.Fatal("first request from b refused; b has its own budget")
	}
}

// TestLimiterWindowSlides is the reason this is a sliding window and not a
// fixed one: a caller must get its budget back as the oldest hits age out, and
// the age-out must be per-hit rather than all at once at a window boundary.
//
// Note the absence of a refused request in the middle. A refusal records a hit
// too (see TestLimiterRefusedRequestsKeepTheBucketFull), so mixing one in here
// would test that behaviour instead of this one.
func TestLimiterWindowSlides(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := NewLimiter(2, time.Minute)
	l.now = func() time.Time { return now }

	if allowed, _ := l.Allow("a"); !allowed {
		t.Fatal("first request refused")
	}
	now = now.Add(30 * time.Second)
	if allowed, _ := l.Allow("a"); !allowed {
		t.Fatal("second request refused; the limit is 2")
	}

	// t+61: the hit at t+0 has aged out, the one at t+30 has not. Exactly one
	// slot is free, which a fixed window would not have released yet (it would
	// still be counting both).
	now = now.Add(31 * time.Second)
	if allowed, _ := l.Allow("a"); !allowed {
		t.Fatal("request refused after the oldest hit aged out; the window is not sliding")
	}
	// And only one slot: the bucket is full again.
	if allowed, _ := l.Allow("a"); allowed {
		t.Fatal("two requests allowed after only one slot freed up")
	}
}

// TestLimiterRefusedRequestsKeepTheBucketFull guards the failure mode where a
// flood becomes free: if a refusal did not record a hit, an attacker hammering a
// full bucket would see the window drain on its own and get a free request every
// time the oldest hit expired.
func TestLimiterRefusedRequestsKeepTheBucketFull(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := NewLimiter(1, time.Minute)
	l.now = func() time.Time { return now }

	if allowed, _ := l.Allow("a"); !allowed {
		t.Fatal("first request refused")
	}
	// Hammer for 50 seconds. Each refusal must push the drain point forward.
	for i := 0; i < 50; i++ {
		now = now.Add(time.Second)
		if allowed, _ := l.Allow("a"); allowed {
			t.Fatalf("request %d allowed while the caller was still hammering", i+2)
		}
	}
}

// TestLimiterDisabledNeverRejects is the safety property of the zero value: a
// disabled limiter must allow everything. A disabled control that rejects is an
// outage, and it is the one failure mode a "safety feature" must not have.
func TestLimiterDisabledNeverRejects(t *testing.T) {
	for _, l := range []*Limiter{nil, NewLimiter(0, time.Minute), NewLimiter(-1, time.Minute)} {
		for i := 0; i < 100; i++ {
			if allowed, _ := l.Allow("a"); !allowed {
				t.Fatalf("disabled limiter refused request %d", i+1)
			}
		}
	}
}

// TestLimiterSweepForgetsIdleKeys bounds memory: a flood from many addresses
// must not grow the map without limit. Asserted on the map size rather than on
// a duration, so the test measures the thing that would actually exhaust memory.
func TestLimiterSweepForgetsIdleKeys(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := NewLimiter(1, time.Minute)
	l.now = func() time.Time { return now }

	for i := 0; i < 1000; i++ {
		l.Allow(strconv.Itoa(i))
	}
	if got := len(l.hits); got != 1000 {
		t.Fatalf("hits = %d, want 1000 before any sweep", got)
	}

	// One more request past the window triggers the sweep.
	now = now.Add(2 * time.Minute)
	l.Allow("trigger")

	if got := len(l.hits); got != 1 {
		t.Errorf("hits = %d after the sweep, want 1 (only the live key)", got)
	}
}

// TestLimiterIsConcurrencySafe is run under -race; it also asserts the budget is
// never exceeded under real contention, which a check-then-act implementation
// would fail.
func TestLimiterIsConcurrencySafe(t *testing.T) {
	const limit = 5
	l := NewLimiter(limit, time.Minute)

	var (
		mu      sync.Mutex
		allowed int
		wg      sync.WaitGroup
	)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, _ := l.Allow("same")
			if ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if allowed != limit {
		t.Errorf("%d requests allowed, want exactly %d", allowed, limit)
	}
}

// --- BanList ---------------------------------------------------------------

// TestBanListBansAfterThreshold walks the escalation the user asked for:
// repeated refusals convert into a temporary block.
func TestBanListBansAfterThreshold(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	b := NewBanList(nil, 3, 10*time.Minute, 30*time.Minute)
	b.now = func() time.Time { return now }

	addr := netip.MustParseAddr("203.0.113.7")
	for i := 0; i < 2; i++ {
		b.Strike(addr)
		if banned, _ := b.Banned(addr); banned {
			t.Fatalf("banned after %d strikes; the threshold is 3", i+1)
		}
	}
	b.Strike(addr)
	if banned, _ := b.Banned(addr); !banned {
		t.Fatal("not banned after 3 strikes")
	}
}

// TestBanListExpiresOnItsOwn is what keeps a shared egress address from being
// locked out forever because one device behind it misbehaved.
func TestBanListExpiresOnItsOwn(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	b := NewBanList(nil, 1, 10*time.Minute, 30*time.Minute)
	b.now = func() time.Time { return now }

	addr := netip.MustParseAddr("203.0.113.7")
	b.Strike(addr)
	if banned, _ := b.Banned(addr); !banned {
		t.Fatal("not banned after one strike with threshold 1")
	}

	now = now.Add(31 * time.Minute)
	if banned, _ := b.Banned(addr); banned {
		t.Error("still banned after BanTTL elapsed; a temporary ban must be temporary")
	}
}

// TestBanListNeverBansATrustedProxy is the self-inflicted-outage guard.
//
// When the deployment loses its X-Forwarded-For header, ClientIP collapses every
// client onto the proxy's own address. Banning that address takes the entire
// service down in one request. Rate limiting still applies, which is the correct
// response to that misconfiguration; a ban is not.
func TestBanListNeverBansATrustedProxy(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "172.17.0.1", "10.1.2.3", "192.168.1.1", "::1"} {
		addr := netip.MustParseAddr(raw)
		b := NewBanList(nil, 1, 10*time.Minute, 30*time.Minute)
		for i := 0; i < 100; i++ {
			b.Strike(addr)
		}
		if banned, _ := b.Banned(addr); banned {
			t.Errorf("%s was banned; a trusted proxy must never be, or one bad request takes the service down", raw)
		}
	}
}

// TestBanListPermanentEntriesNeverExpire covers the operator list: it is the one
// control here that is not self-healing, so it must actually stick.
func TestBanListPermanentEntriesNeverExpire(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	permanent := []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")}
	b := NewBanList(permanent, 0, time.Minute, time.Minute)
	b.now = func() time.Time { return now }

	for _, raw := range []string{"198.51.100.1", "198.51.100.254"} {
		addr := netip.MustParseAddr(raw)
		banned, wait := b.Banned(addr)
		if !banned {
			t.Errorf("%s not blocked by a /24 permanent entry", raw)
		}
		if wait != 0 {
			t.Errorf("%s reported wait %v; a permanent entry has no expiry", raw, wait)
		}
	}

	// A year later it is still blocked — this is the difference from a temp ban.
	now = now.Add(365 * 24 * time.Hour)
	if banned, _ := b.Banned(netip.MustParseAddr("198.51.100.1")); !banned {
		t.Error("permanent block expired")
	}

	// An address outside the prefix is untouched.
	if banned, _ := b.Banned(netip.MustParseAddr("198.51.101.1")); banned {
		t.Error("198.51.101.1 blocked by a 198.51.100.0/24 entry")
	}
}

// TestBanListIPv4MappedMatchesTheIPv4Entry is the bucket-splitting guard, the
// same one ClientIP has: one host must not be able to appear as two.
func TestBanListIPv4MappedMatchesTheIPv4Entry(t *testing.T) {
	b := NewBanList([]netip.Prefix{netip.MustParsePrefix("203.0.113.7/32")},
		0, time.Minute, time.Minute)

	if banned, _ := b.Banned(netip.MustParseAddr("::ffff:203.0.113.7")); !banned {
		t.Error("an IPv4-mapped form of a blocked address was not blocked")
	}
}

// TestBanListZeroThresholdStillEnforcesPermanentEntries: BAN_AFTER_REFUSALS=0 is
// documented as "disable automatic banning", not "disable the blocklist".
func TestBanListZeroThresholdStillEnforcesPermanentEntries(t *testing.T) {
	b := NewBanList([]netip.Prefix{netip.MustParsePrefix("203.0.113.7/32")},
		0, time.Minute, time.Minute)
	addr := netip.MustParseAddr("203.0.113.7")

	for i := 0; i < 100; i++ {
		b.Strike(addr)
	}
	if banned, _ := b.Banned(addr); !banned {
		t.Error("permanent entry not enforced with BAN_AFTER_REFUSALS=0")
	}

	// And an address not on the list is never auto-banned when the threshold is 0.
	other := netip.MustParseAddr("203.0.113.8")
	for i := 0; i < 100; i++ {
		b.Strike(other)
	}
	if banned, _ := b.Banned(other); banned {
		t.Error("auto-banned with BAN_AFTER_REFUSALS=0")
	}
}

// --- ClientIPAddrOK --------------------------------------------------------

// TestClientIPAddrOKIgnoresForwardedHeadersFromAnUntrustedPeer is the frozen
// contract restated for the new entry point: if this function ever started
// believing a header, the ban list would become forgeable and an attacker could
// ban an arbitrary third party.
func TestClientIPAddrOKIgnoresForwardedHeadersFromAnUntrustedPeer(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.9:1234"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	r.Header.Set("Cf-Connecting-Ip", "5.6.7.8")

	addr, ok := ClientIPAddrOK(r)
	if !ok {
		t.Fatal("ok = false for a plain address")
	}
	if got, want := addr.String(), "203.0.113.9"; got != want {
		t.Errorf("ClientIPAddrOK = %q, want %q (forwarded headers from an untrusted peer must be ignored)", got, want)
	}
	if got := ClientIP(r); got != addr.String() {
		t.Errorf("ClientIP = %q but ClientIPAddrOK = %q; they must agree or the limiter and the ban list disagree about who the caller is", got, addr.String())
	}
}

// TestClientIPAddrOKReadsForwardedForFromATrustedPeer keeps the other half: a
// real proxy in front must still be attributed correctly, or every client shares
// one bucket.
func TestClientIPAddrOKReadsForwardedForFromATrustedPeer(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "172.17.0.1:1234"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.9")

	addr, ok := ClientIPAddrOK(r)
	if !ok {
		t.Fatal("ok = false")
	}
	if got, want := addr.String(), "203.0.113.9"; got != want {
		t.Errorf("ClientIPAddrOK = %q, want %q (rightmost untrusted entry)", got, want)
	}
}

// TestClientIPAddrOKReportsNoAddressForJunk keeps the ok=false contract that
// stops a non-IP string from becoming a key.
func TestClientIPAddrOKReportsNoAddressForJunk(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "not-an-address"

	if _, ok := ClientIPAddrOK(r); ok {
		t.Error("ok = true for a RemoteAddr that is not an address")
	}
	// And the string form still degrades to the shared bucket rather than
	// inventing one.
	if got := ClientIP(r); got != "not-an-address" {
		t.Errorf("ClientIP = %q, want the raw peer", got)
	}
}

// --- RateLimit / Blocked middleware ---------------------------------------

// TestRateLimitMiddlewareRefusesWithTheEnvelopeAndRetryAfter pins the wire
// contract: the refusal must be this service's JSON envelope, not a bare 429.
func TestRateLimitMiddlewareRefusesWithTheEnvelopeAndRetryAfter(t *testing.T) {
	l := NewLimiter(1, time.Minute)
	h := RateLimit(l, NewBanList(nil, 0, time.Minute, time.Minute))(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

	send := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "203.0.113.9:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	if rec := send(); rec.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", rec.Code)
	}

	rec := send()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Error("429 without Retry-After")
	} else if n, err := strconv.Atoi(got); err != nil || n < 1 {
		t.Errorf("Retry-After = %q, want a positive integer", got)
	}

	var body struct {
		OK    bool `json:"ok"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if body.OK {
		t.Error("ok = true on a 429")
	}
	if body.Error.Code != "rate_limited" {
		t.Errorf("error.code = %q, want rate_limited", body.Error.Code)
	}
}

// TestRateLimitMiddlewareDoesNotRunTheHandlerWhenRefusing is the property that
// makes the limiter a defence rather than a speed bump: a refused request must
// not reach the handler, or the expensive work (PBKDF2, a DB write) still
// happens.
func TestRateLimitMiddlewareDoesNotRunTheHandlerWhenRefusing(t *testing.T) {
	l := NewLimiter(1, time.Minute)
	ran := 0
	h := RateLimit(l, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran++
	}))

	for i := 0; i < 5; i++ {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "203.0.113.9:1234"
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	if ran != 1 {
		t.Errorf("handler ran %d times, want 1: a refused request must not reach it", ran)
	}
}

// TestRateLimitMiddlewareEscalatesToABan ties the two controls together: a
// refusal feeds the ban list, and once banned the Blocked middleware refuses
// every path.
func TestRateLimitMiddlewareEscalatesToABan(t *testing.T) {
	bans := NewBanList(nil, 3, 10*time.Minute, 30*time.Minute)
	limited := RateLimit(NewLimiter(1, time.Minute), bans)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	addr := "203.0.113.9"
	hit := func(h http.Handler) int {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = addr + ":1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}

	// One allowed, then enough refusals to cross the threshold of 3.
	for i := 0; i < 4; i++ {
		hit(limited)
	}

	blocked := bans.Blocked()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	if code := hit(blocked); code != http.StatusForbidden {
		t.Errorf("status = %d after crossing the ban threshold, want 403", code)
	}
}

// TestBlockedMiddlewareRefusesEveryPath covers the "every path" requirement: a
// banned scanner must not be able to keep probing for free.
func TestBlockedMiddlewareRefusesEveryPath(t *testing.T) {
	bans := NewBanList([]netip.Prefix{netip.MustParsePrefix("203.0.113.9/32")},
		0, time.Minute, time.Minute)
	h := bans.Blocked()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, path := range []string{"/", "/api/health", "/does-not-exist", "/s/tok"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = "203.0.113.9:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", path, rec.Code)
		}
	}
}

// TestBlockedMiddlewareDoesNotTouchUnlistedAddresses is the counterpart: a ban
// list that refuses everyone would pass every test above.
func TestBlockedMiddlewareDoesNotTouchUnlistedAddresses(t *testing.T) {
	bans := NewBanList([]netip.Prefix{netip.MustParsePrefix("203.0.113.9/32")},
		0, time.Minute, time.Minute)
	h := bans.Blocked()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "198.51.100.1:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d for an address that is not on the list, want 200", rec.Code)
	}
}

// TestNilGuardsAreInert is the disabled-state guard: when SECURITY_MIDDLEWARE is
// off, both middlewares must be pure pass-throughs.
func TestNilGuardsAreInert(t *testing.T) {
	var bans *BanList
	h := Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }),
		bans.Blocked(),
		RateLimit(nil, bans),
	)

	for i := 0; i < 200; i++ {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "203.0.113.9:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d with guards disabled, want 200", i+1, rec.Code)
		}
	}
}

// TestBanListSweepsStaleEntriesRatherThanRefusingForever covers the at-capacity
// path: a full map must make room for a new offender rather than silently
// refusing to record one.
//
// The throttle is deliberately NOT due here (lastSweep is placed in the present),
// so the ONLY way the new entry can get a slot is the at-capacity sweep. An
// earlier version of this test let the throttled sweep run first, which pruned
// everything and made the assertion pass with the at-capacity branch deleted
// entirely — a test that could not fail. Time is advanced only in the seeded
// entries, not in lastSweep.
func TestBanListSweepsStaleEntriesRatherThanRefusingForever(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	b := NewBanList(nil, 1, 10*time.Minute, 30*time.Minute)
	b.now = func() time.Time { return now }
	b.maxEntries = 8

	// Seed a full map of genuinely stale entries, and mark the last sweep as
	// having just happened so the throttled sweep will be skipped.
	staleMS := now.Add(-31 * time.Minute).UnixMilli()
	b.entries = make(map[string]*banEntry, b.maxEntries)
	for i := 0; i < b.maxEntries; i++ {
		key := netip.AddrFrom4([4]byte{203, 0, 113, byte(i + 1)}).String()
		b.entries[key] = &banEntry{expiresMS: staleMS, strikes: 0, lastHitMS: staleMS}
	}
	b.lastSweep = now.UnixMilli()

	fresh := netip.MustParseAddr("198.51.100.9")
	b.Strike(fresh)

	if _, ok := b.entries[fresh.String()]; !ok {
		t.Error("a new offender got no slot while the map held only stale entries; " +
			"the at-capacity path must sweep before giving up")
	}
	if got := len(b.entries); got != 1 {
		t.Errorf("entries = %d after the at-capacity sweep, want 1 (only the fresh offender)", got)
	}
}

// TestBanListThrottlesItsSweep keeps the O(n) sweep off the refusal path.
//
// The clock is advanced between the first strike and the flood on purpose: with
// frozen time, `lastSweep` reads the same whether or not each strike swept, so
// the assertion would hold even with the throttle removed (measured — the first
// version of this test passed against an unthrottled sweep).
func TestBanListThrottlesItsSweep(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	b := NewBanList(nil, 1, 10*time.Minute, 30*time.Minute)
	b.now = func() time.Time { return now }

	b.Strike(netip.MustParseAddr("203.0.113.1"))
	first := b.lastSweep
	if first == 0 {
		t.Fatal("the first strike did not sweep")
	}

	// Move the clock, so an unthrottled sweep becomes observable as lastSweep
	// following the clock.
	now = now.Add(time.Second)
	for i := 0; i < 500; i++ {
		b.Strike(netip.MustParseAddr("203.0.113.2"))
	}
	if b.lastSweep != first {
		t.Errorf("lastSweep moved from %d to %d inside one strike window; "+
			"the O(n) sweep is running on every refusal", first, b.lastSweep)
	}

	// Past the window it sweeps again, which is what keeps the map pruned.
	now = now.Add(11 * time.Minute)
	b.Strike(netip.MustParseAddr("203.0.113.3"))
	if b.lastSweep == first {
		t.Error("no sweep after the strike window elapsed; the map would grow without bound")
	}
}

// --- global bucket ---------------------------------------------------------

// TestGlobalBucketIsAddressIndependent is the property that distinguishes this
// from every other control here.
//
// It is the whole reason the backstop exists: a per-IP bucket is address-scoped,
// and one IPv6 /64 holds 2^64 addresses, so a caller with many addresses gets a
// fresh budget from every one of them. If this test ever passes with the bucket
// keyed per address, the backstop is decorative.
func TestGlobalBucketIsAddressIndependent(t *testing.T) {
	h := RateLimitGlobal(NewLimiter(3, time.Minute))(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	// Six requests from six DIFFERENT addresses against a limit of 3.
	codes := make([]int, 0, 6)
	for i := 0; i < 6; i++ {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = netip.AddrFrom4([4]byte{203, 0, 113, byte(i + 1)}).String() + ":1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		codes = append(codes, rec.Code)
	}

	refused := 0
	for _, c := range codes {
		if c == http.StatusTooManyRequests {
			refused++
		}
	}
	if refused != 3 {
		t.Errorf("refused %d of 6 requests spread across 6 addresses with a limit of 3 (%v); "+
			"the global bucket must be shared by every address, not per address", refused, codes)
	}
}

// NOTE: there is deliberately no "the global bucket does not ban" test here.
// RateLimitGlobal takes no ban list, so at this level that property is enforced by
// the signature and any test of it would pass vacuously (measured: it did, even
// with the attribution guard removed). The property that can actually regress is
// at the assembly level, where one BanList is shared by every class — see
// TestGlobalRateLimitDoesNotBanTheCaller in internal/api/security_test.go.

// TestGlobalBucketExemptsExactPaths covers the liveness probe.
//
// The exemption is an exact path match, so it cannot be widened by a suffix or a
// prefix trick.
func TestGlobalBucketExemptsExactPaths(t *testing.T) {
	h := RateLimitGlobal(NewLimiter(1, time.Minute), "/p/api/health")(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

	hit := func(path string) int {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = "203.0.113.9:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}

	// The exempt path keeps working, well past the limit of 1.
	for i := 0; i < 20; i++ {
		if code := hit("/p/api/health"); code != http.StatusOK {
			t.Fatalf("exempt path refused on request %d (status %d); the liveness probe must "+
				"never be rate limited", i+1, code)
		}
	}

	// Everything else is still limited.
	if code := hit("/p/api/healthz"); code != http.StatusOK {
		t.Fatalf("first request to a non-exempt path refused (status %d)", code)
	}
	if code := hit("/p/api/healthz"); code != http.StatusTooManyRequests {
		t.Errorf("status %d for a second request to a non-exempt path with limit 1, want 429; "+
			"the exemption is too wide", code)
	}
	// A path that merely CONTAINS the exempt path must not be exempt.
	if code := hit("/x/p/api/health"); code != http.StatusTooManyRequests {
		t.Errorf("status %d for a path that only contains the exempt path, want 429; "+
			"the exemption must be an exact match", code)
	}
}
