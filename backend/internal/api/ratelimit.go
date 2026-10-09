package api

import (
	"sync"
	"time"
)

// applyLimiter is the process-local throttle for the public key-application
// endpoint (KEY_APPLY_PER_HOUR per source address).
//
// It is a sliding window over a map of timestamps rather than a token bucket:
// the endpoint is rare, the state is small, and "the last N requests in the past
// hour" is exactly what the configuration knob promises. State lives in memory
// and is lost on restart — an accepted trade-off for a single self-contained
// binary (see the decision record); an attacker cannot restart the process, so
// the limit still bounds a burst.
//
// Zero value is unusable; construct with newApplyLimiter. The mutex makes every
// method safe under -race and under real concurrency.
type applyLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string][]int64
	// lastGC is the last full sweep, so pruning dead addresses costs O(1) per
	// request in the common case instead of O(addresses).
	lastGC int64
	now    func() time.Time
}

// newApplyLimiter builds a limiter allowing limit requests per window.
//
// A non-positive limit disables limiting entirely. Configuration validation
// rejects that in production, but the zero value is what a hand-built Config
// carries, and a disabled limit must never mean "everyone is rejected".
func newApplyLimiter(limit int, window time.Duration) *applyLimiter {
	if window <= 0 {
		window = time.Hour
	}
	return &applyLimiter{
		limit:  limit,
		window: window,
		hits:   make(map[string][]int64),
		now:    time.Now,
	}
}

// allow records one attempt for key and reports whether it is within budget.
func (l *applyLimiter) allow(key string) bool {
	if l == nil || l.limit <= 0 {
		return true
	}
	nowMS := l.now().UnixMilli()
	cutoff := nowMS - l.window.Milliseconds()

	l.mu.Lock()
	defer l.mu.Unlock()

	if nowMS-l.lastGC >= l.window.Milliseconds() {
		l.sweepLocked(cutoff)
		l.lastGC = nowMS
	}

	kept := l.hits[key][:0]
	for _, ts := range l.hits[key] {
		if ts > cutoff {
			kept = append(kept, ts)
		}
	}
	if len(kept) >= l.limit {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, nowMS)
	return true
}

// retryAfterSeconds is the whole window, rounded up. It is an upper bound, not
// the exact time until the oldest hit ages out, which would require leaking the
// caller's own history back to them.
func (l *applyLimiter) retryAfterSeconds() int {
	if l == nil || l.window <= 0 {
		return 1
	}
	seconds := int((l.window + time.Second - 1) / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}

// sweepLocked drops every timestamp at or before cutoff and forgets addresses
// with nothing left, so a flood from many addresses cannot grow the map without
// bound.
func (l *applyLimiter) sweepLocked(cutoff int64) {
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
