package httpx

import (
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"
)

// This file holds the transport-level abuse controls: a per-key sliding-window
// limiter, a source-address ban list, and the middleware that applies them.
//
// Two properties are load-bearing and are why this is not a general-purpose
// rate-limiting library:
//
//  1. **The bucket key is always httpx.ClientIP.** That is a frozen contract of
//     this project (see the durable constraints in .ai/CURRENT_TASK.md). A
//     limiter keyed on any header-reading helper is decorative: the caller
//     rotates `X-Forwarded-For` and gets a fresh bucket per request. ClientIP
//     ignores forwarded headers unless the immediate peer is a known proxy, so
//     this holds even when the deployment shape changes.
//
//  2. **State is process-local and bounded.** One self-contained binary, no
//     Redis, no background goroutines. A restart clears every window and every
//     temporary ban, which is an accepted trade-off (an attacker cannot restart
//     the process). What must NOT happen is unbounded growth: a flood from many
//     source addresses must not turn into unbounded memory. Both structures
//     therefore sweep on a fixed cadence and refuse to grow past a cap.

// Limiter is a per-key sliding-window rate limiter.
//
// "The last N requests within the window" is exactly what the configuration
// knobs promise, unlike a token bucket, whose burst allowance is a second,
// undocumented parameter. The state is a slice of timestamps per key, which is
// small because the window is short and the limit is low.
//
// The zero value is unusable; construct with NewLimiter. A nil *Limiter allows
// everything, so a disabled control can never mean "reject everyone".
type Limiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string][]int64
	// lastSweep is the last full sweep, so pruning dead keys costs O(1) per
	// request in the common case instead of O(keys).
	lastSweep int64
	now       func() time.Time
}

// NewLimiter builds a limiter allowing limit hits per key per window.
//
// A non-positive limit disables limiting entirely. Configuration validation
// rejects that in production, but a hand-built Config carries the zero value,
// and "disabled" must never mean "everyone is rejected".
func NewLimiter(limit int, window time.Duration) *Limiter {
	if window <= 0 {
		window = time.Minute
	}
	return &Limiter{
		limit:  limit,
		window: window,
		hits:   make(map[string][]int64),
		now:    time.Now,
	}
}

// Allow records one attempt for key and reports whether it is within budget.
//
// When it refuses, the second return value is how long the caller should wait
// before the oldest hit in its window ages out — an upper bound computed from
// its own history, so it reveals nothing about other callers.
//
// The hit is recorded whether or not it was allowed. That is deliberate: a
// caller that keeps hammering a full bucket must keep its window full, or
// waiting out the window would be free once the earliest hit expires while the
// flood continues.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	if l == nil || l.limit <= 0 {
		return true, 0
	}

	nowMS := l.now().UnixMilli()
	cutoff := nowMS - l.window.Milliseconds()

	l.mu.Lock()
	defer l.mu.Unlock()

	if nowMS-l.lastSweep >= l.window.Milliseconds() {
		l.sweepLocked(cutoff)
		l.lastSweep = nowMS
	}

	kept := l.hits[key][:0]
	for _, ts := range l.hits[key] {
		if ts > cutoff {
			kept = append(kept, ts)
		}
	}
	kept = append(kept, nowMS)
	l.hits[key] = kept

	if len(kept) > l.limit {
		// The oldest hit that must age out for the budget to have room again.
		waitMS := kept[0] + l.window.Milliseconds() - nowMS
		if waitMS < 1 {
			waitMS = 1
		}
		return false, time.Duration(waitMS) * time.Millisecond
	}
	return true, 0
}

// sweepLocked forgets every key whose window has closed, so a flood from many
// addresses cannot grow the map without bound.
func (l *Limiter) sweepLocked(cutoff int64) {
	for key, times := range l.hits {
		kept := times[:0]
		for _, ts := range times {
			if ts > cutoff {
				kept = append(kept, ts)
			}
		}
		if len(kept) == 0 {
			delete(l.hits, key)
			continue
		}
		l.hits[key] = kept
	}
}

// banEntry is one temporary ban: the address is refused until ExpiresMS.
//
// strikes is kept across bans so a caller that gets banned, waits it out, and
// immediately resumes is not treated as a first offender again; it is pruned
// once its own strike window has closed.
type banEntry struct {
	expiresMS int64
	strikes   int
	lastHitMS int64
}

// BanList refuses source addresses after repeated refusals, and enforces an
// operator-supplied permanent blocklist.
//
// Two rules make it safe to leave running on a public host:
//
//   - **It never bans a trusted proxy or loopback.** If the deployment loses its
//     X-Forwarded-For header, ClientIP collapses every client onto the proxy's
//     own address; banning that would take the whole service down in one
//     request — a self-inflicted outage that is much worse than the abuse it
//     prevents. Trusted addresses are therefore exempt, and rate limiting (which
//     still applies) is the control for that misconfiguration.
//   - **Memory is bounded.** A flood from forged-but-real source addresses
//     cannot make the map grow without limit: past maxEntries no new temporary
//     ban is created (the per-class limiter keeps refusing those sources
//     anyway).
type BanList struct {
	mu sync.Mutex
	// permanent comes from configuration and is matched as prefixes, so an
	// exact address is simply a /32 or /128 prefix — one code path, and CIDR
	// support costs nothing extra.
	permanent  []netip.Prefix
	entries    map[string]*banEntry
	threshold  int
	strikeTTL  time.Duration
	banTTL     time.Duration
	maxEntries int
	// lastSweep throttles sweeping, because sweepLocked is O(entries). Without
	// it, every refusal from an address already being refused would walk the
	// whole map — turning the ban list into the amplifier under exactly the
	// flood it exists for. Same shape as Limiter.lastSweep.
	lastSweep int64
	now       func() time.Time
}

// NewBanList builds a ban list.
//
// permanent is matched first and never expires. threshold is how many refusals
// within strikeTTL convert into a ban; a non-positive threshold disables
// automatic banning while leaving the permanent list enforced. banTTL is how
// long an automatic ban lasts.
func NewBanList(permanent []netip.Prefix, threshold int, strikeTTL, banTTL time.Duration) *BanList {
	if strikeTTL <= 0 {
		strikeTTL = 10 * time.Minute
	}
	if banTTL <= 0 {
		banTTL = 30 * time.Minute
	}
	return &BanList{
		permanent: permanent,
		entries:   make(map[string]*banEntry),
		threshold: threshold,
		strikeTTL: strikeTTL,
		banTTL:    banTTL,
		// Sized so a full ban list stays a few hundred kilobytes at worst.
		maxEntries: 100_000,
		now:        time.Now,
	}
}

// Banned reports whether addr is currently refused, and for how much longer.
//
// A permanent entry reports a zero duration: "blocked, no expiry".
func (b *BanList) Banned(addr netip.Addr) (bool, time.Duration) {
	if b == nil {
		return false, 0
	}
	addr = addr.Unmap().WithZone("")

	for _, prefix := range b.permanent {
		if prefix.Contains(addr) {
			return true, 0
		}
	}

	nowMS := b.now().UnixMilli()
	b.mu.Lock()
	defer b.mu.Unlock()

	entry, ok := b.entries[addr.String()]
	if !ok || entry.expiresMS <= nowMS {
		return false, 0
	}
	return true, time.Duration(entry.expiresMS-nowMS) * time.Millisecond
}

// Strike records one refusal for addr and bans it once it has been refused
// threshold times inside the strike window.
//
// Called only after a limiter has already refused the request, so a client that
// stays inside its budget is never counted.
func (b *BanList) Strike(addr netip.Addr) {
	if b == nil || b.threshold <= 0 {
		return
	}
	addr = addr.Unmap().WithZone("")
	// See the type doc: banning a proxy is a self-inflicted outage. The
	// permanent list is exempt because that one is an explicit human decision.
	if isTrustedProxy(addr) {
		return
	}
	key := addr.String()

	nowTime := b.now()
	nowMS := nowTime.UnixMilli()
	strikeCutoff := nowMS - b.strikeTTL.Milliseconds()

	b.mu.Lock()
	defer b.mu.Unlock()

	// Sweep at most once per strike window rather than on every strike; see
	// lastSweep. A zero lastSweep means "never swept", so the first strike
	// sweeps — which is also what makes the map self-pruning when idle.
	if nowMS-b.lastSweep >= b.strikeTTL.Milliseconds() {
		b.sweepLocked(nowMS)
		b.lastSweep = nowMS
	}

	entry, ok := b.entries[key]
	if !ok {
		if len(b.entries) >= b.maxEntries {
			// At capacity, sweep unconditionally before giving up: an entry
			// that is only stale would otherwise permanently occupy a slot,
			// and refusing to record new offenders while stale ones linger
			// is the opposite of what this list is for. If the map is still
			// full after a real sweep, this is a genuinely distributed flood
			// and the per-class limiter keeps refusing those sources anyway.
			b.sweepLocked(nowMS)
			b.lastSweep = nowMS
			if len(b.entries) >= b.maxEntries {
				return
			}
		}
		entry = &banEntry{}
		b.entries[key] = entry
	}

	// A strike that arrives long after the previous one starts a new count.
	if entry.lastHitMS < strikeCutoff {
		entry.strikes = 0
	}
	entry.strikes++
	entry.lastHitMS = nowMS

	if entry.strikes >= b.threshold {
		entry.expiresMS = nowMS + b.banTTL.Milliseconds()
		// Reset the count so the TTL is a real second chance rather than an
		// instant re-ban on the next single refusal.
		entry.strikes = 0
	}
}

// sweepLocked drops entries whose ban has expired and whose strikes are stale.
//
// A previously banned address keeps its record until the strike window closes,
// so operators can still see a repeat offender in the logs for a while after
// the ban lapses.
//
// O(len(entries)), and only ever called from the Strike write path (throttled,
// or forced when the map is full). It is deliberately NOT called from Banned:
// that is the per-request read path, and walking the map there would put an
// O(n) cost on every request for the benefit of pruning state nobody asked
// about.
func (b *BanList) sweepLocked(nowMS int64) {
	strikeCutoff := nowMS - b.strikeTTL.Milliseconds()
	for key, entry := range b.entries {
		if entry.expiresMS <= nowMS && entry.lastHitMS < strikeCutoff {
			delete(b.entries, key)
		}
	}
}

// Blocked is the middleware that refuses banned source addresses.
//
// It is mounted on the transport chain, before routing, so a banned caller is
// refused on every path — including paths that do not exist. Refusing only
// known routes would let a banned scanner keep probing for free.
func (b *BanList) Blocked() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if b == nil {
				next.ServeHTTP(w, r)
				return
			}
			client, ok := ClientIPAddrOK(r)
			if !ok {
				// No address to attribute. RemoteAddr is always set by net/http,
				// so this means something is structurally wrong; fall through
				// rather than refuse every request, and let the limiter key this
				// into the shared "unparseable" bucket.
				next.ServeHTTP(w, r)
				return
			}
			if banned, wait := b.Banned(client); banned {
				if wait > 0 {
					w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(wait)))
				}
				Fail(w, r, http.StatusForbidden, "ip_blocked",
					"This address is blocked", nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// proxyBucketOnce keeps the "every client shares one bucket" warning to one line
// per process rather than one per request.
var proxyBucketOnce sync.Once

// warnIfProxyBucket reports the one misconfiguration that turns per-IP limiting
// into a global limit: no usable X-Forwarded-For from the trusted proxy, so
// ClientIP collapses to the proxy's own address and every client shares a single
// bucket.
//
// The request is still limited rather than waved through. A silent weakening is
// worse than a loud strictness: a global 429 is visible in the journal, names
// the cause on the line below, and is fixed by repairing the forwarded header.
func warnIfProxyBucket(addr netip.Addr) {
	if !isTrustedProxy(addr) {
		return
	}
	proxyBucketOnce.Do(func() {
		slog.Warn("SECURITY: every client collapses into one rate-limit bucket"+
			" — the trusted proxy is not sending X-Forwarded-For, so ClientIP returns the proxy itself",
			"proxy", addr.String())
	})
}

// RateLimit builds the middleware for one rate-limit class, keyed per caller.
//
// It applies the limiter first and, on refusal, feeds the ban list so repeated
// abuse escalates from "slow down" to "go away". The refusals are attributed by
// the same ClientIP key that produced the bucket, so the two decisions can
// never disagree about who the caller is.
//
// Every refusal is the same JSON envelope as the rest of the API, with
// `Retry-After` in seconds, so a well-behaved client can back off instead of
// guessing. Refusing before the handler runs is the point: the body is never
// read, never parsed and never reaches the database.
func RateLimit(limiter *Limiter, bans *BanList) Middleware {
	return rateLimit(limiter, bans, ClientIP,
		"Too many requests from this address; try again later")
}

// globalBucketKey is the single key every request shares in the global bucket.
//
// It is not an address, and that is the entire point: see RateLimitGlobal.
const globalBucketKey = "*"

// RateLimitGlobal builds the process-wide backstop: every caller shares one
// bucket, so the total request rate this process will serve is bounded no matter
// how many source addresses the traffic comes from.
//
// This is what covers the gap per-IP limits cannot. A per-IP bucket is
// address-scoped, so a caller with many addresses — trivially obtained from a
// single IPv6 /64, which holds 2^64 of them — gets a fresh budget per address and
// is never limited. An address-independent ceiling is the only control that
// still holds there.
//
// Two deliberate differences from RateLimit:
//
//   - **It never feeds the ban list.** A global breach means "the process is
//     saturated", not "this caller is abusive", and the caller who happens to
//     trip the ceiling is as likely to be a legitimate reader as an attacker.
//     Attributing a global breach to whoever was unlucky enough to be the next
//     request would mass-ban innocent users under precisely the flood the
//     backstop exists for.
//   - **It never applies to exempt paths.** A liveness probe that answers 429
//     reads as "the service is down" to whatever is polling it, and with an
//     in-process limiter a supervisor restart clears every window and ban — so
//     an attacker could use the health route to get the counters reset. The
//     exemption is an exact path match, passed in by the caller that knows the
//     route prefix.
func RateLimitGlobal(limiter *Limiter, exemptPaths ...string) Middleware {
	exempt := make(map[string]struct{}, len(exemptPaths))
	for _, p := range exemptPaths {
		exempt[p] = struct{}{}
	}

	inner := rateLimit(limiter, nil, func(*http.Request) string { return globalBucketKey },
		"Too many requests to this service right now; try again later")
	if len(exempt) == 0 {
		return inner
	}

	return func(next http.Handler) http.Handler {
		h := inner(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := exempt[r.URL.Path]; ok {
				next.ServeHTTP(w, r)
				return
			}
			h.ServeHTTP(w, r)
		})
	}
}

// rateLimit is the shared body of RateLimit and RateLimitGlobal.
//
// keyOf decides identity, and bucketsKeyedByCaller tells this function whether
// that identity is a source address — the only case where a refusal may be
// attributed to a caller and where a collapsed-to-the-proxy bucket is possible.
// A nil bans disables escalation entirely (see RateLimitGlobal).
func rateLimit(limiter *Limiter, bans *BanList, keyOf func(*http.Request) string,
	message string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if limiter == nil {
				next.ServeHTTP(w, r)
				return
			}
			key := keyOf(r)
			perCaller := key != globalBucketKey
			addr, haveAddr := ClientIPAddrOK(r)
			if perCaller && haveAddr {
				warnIfProxyBucket(addr)
			}

			allowed, wait := limiter.Allow(key)
			if allowed {
				next.ServeHTTP(w, r)
				return
			}
			if perCaller && bans != nil && haveAddr {
				bans.Strike(addr)
			}
			w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(wait)))
			Fail(w, r, http.StatusTooManyRequests, "rate_limited", message, nil)
		})
	}
}

// retryAfterSeconds rounds a wait up to whole seconds, never below 1.
//
// A `Retry-After: 0` invites an immediate retry, which is the opposite of what
// the header is for.
func retryAfterSeconds(wait time.Duration) int {
	seconds := int((wait + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return seconds
}
