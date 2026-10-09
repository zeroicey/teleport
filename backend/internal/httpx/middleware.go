package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"unicode/utf8"

	"github.com/zeroicey/teleport/backend/internal/domain"
)

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeySession
	ctxKeyPrincipal
)

// Session is the authenticated dashboard identity attached to a request.
type Session struct {
	Subject string
	Issued  int64
	Expires int64
}

// Middleware is a standard net/http middleware.
type Middleware func(http.Handler) http.Handler

// Chain composes middleware so that the first listed runs outermost.
func Chain(h http.Handler, middlewares ...Middleware) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		h = middlewares[i](h)
	}
	return h
}

// RequestIDFrom returns the correlation id for this request, if any.
func RequestIDFrom(ctx context.Context) string {
	if id, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return id
	}
	return ""
}

// SessionFrom returns the authenticated session, if any.
func SessionFrom(ctx context.Context) *Session {
	if s, ok := ctx.Value(ctxKeySession).(*Session); ok {
		return s
	}
	return nil
}

// WithSession attaches a session to the context (used by the auth middleware).
func WithSession(ctx context.Context, s *Session) context.Context {
	return context.WithValue(ctx, ctxKeySession, s)
}

// PrincipalFrom returns the authenticated agent principal, if any.
//
// A nil result means the request did not pass RequireAgent: handlers must not
// treat "no principal" as an anonymous identity with any access.
func PrincipalFrom(ctx context.Context) *domain.Principal {
	if p, ok := ctx.Value(ctxKeyPrincipal).(*domain.Principal); ok {
		return p
	}
	return nil
}

// WithPrincipal attaches an agent principal to the context (used by
// RequireAgent). The principal holds a key id and label only — never the token
// or its hash — so it is safe to log or serialize further up the stack.
func WithPrincipal(ctx context.Context, p *domain.Principal) context.Context {
	return context.WithValue(ctx, ctxKeyPrincipal, p)
}

// maxRequestIDLen caps a client-supplied correlation id, in bytes.
//
// It is far longer than any real identifier (a UUID is 36 bytes, a ULID 26, a
// Cloudflare ray id ~20), so honest clients lose nothing. The cap exists
// because `MaxHeaderBytes` lets one request put a megabyte into the log, and on
// a small machine filling the journal is a denial of service for the whole
// service — including the dashboard and share pages — reachable without any
// authentication. An oversized id is truncated rather than rejected: it is
// still useful for correlation, just bounded.
const maxRequestIDLen = 128

// sanitizeRequestID returns a correlation id safe to log and echo, or "" when
// the client value must be discarded (callers then try the next source, and
// finally a generated id).
//
// A client-supplied id is written into logs and into a response header, so it
// is attacker-controlled text headed for a sink. Three things are enforced:
//
//   - control characters are rejected outright, on the raw value, before any
//     trimming. A newline or a DEL in a log field forges log lines and pollutes
//     audit — the injection primitive matters more than the length cap. Doing
//     this before trimming matters too, or a trailing CR would be trimmed into
//     a value that looks clean.
//   - the result must be valid UTF-8: a header value that is not would be
//     echoed to clients and written to the journal as raw invalid bytes.
//   - it is trimmed and truncated to maxRequestIDLen, on a rune boundary so
//     truncation cannot itself produce the invalid UTF-8 the rule above bans.
func sanitizeRequestID(raw string) string {
	for i := 0; i < len(raw); i++ {
		if c := raw[i]; c < 0x20 || c == 0x7f {
			return ""
		}
	}

	id := strings.TrimSpace(raw)
	if id == "" || !utf8.ValidString(id) {
		return ""
	}
	if len(id) > maxRequestIDLen {
		id = id[:maxRequestIDLen]
		// Strip a partially cut multi-byte rune.
		for len(id) > 0 && !utf8.ValidString(id) {
			id = id[:len(id)-1]
		}
	}
	return id
}

// requestIDFrom prefers the id a front end already assigned, so a request can
// be traced across the edge and the origin with one identifier. `Cf-Ray` wins
// when it is usable; an unusable value falls through to `X-Request-Id` rather
// than poisoning the correlation id for the whole request.
func requestIDFrom(r *http.Request) string {
	for _, header := range []string{"Cf-Ray", "X-Request-Id"} {
		if id := sanitizeRequestID(r.Header.Get(header)); id != "" {
			return id
		}
	}
	return ""
}

// RequestID assigns a correlation id and echoes it back.
//
// A client-supplied id is only used after sanitizeRequestID has vetted it, and
// the echoed header always carries the same sanitised value that the logs and
// the request context see.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := requestIDFrom(r)
		if id == "" {
			id = randomHex(16)
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID, id)))
	})
}

// panicError carries a recovered panic value into the 5xx log line.
//
// The log line is shared with every other server fault (one failure, one line),
// so a panic needs a way to stand out that does not depend on the wording of
// the cause string. Matching alerting on the "panic: " prefix would break
// silently the day someone rewords the wrapper; logServerFault turns this type
// into a stable `panic=true` field instead. The value itself still reaches the
// log through Error(), and never reaches the client.
type panicError struct{ value any }

func (e panicError) Error() string { return fmt.Sprintf("panic: %v", e.value) }

// Recover turns a panic into a 500 instead of killing the connection.
//
// The panic value travels as the cause into fail, which logs the 5xx: one line,
// not two. Logging "panic recovered" separately and then letting Fail log the
// same event would double every panic in the log, and an operator counting two
// lines reads two incidents. panicError keeps the distinction without the
// second line.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				fail(w, r, http.StatusInternalServerError, "internal_error",
					"Internal server error", nil, panicError{value: rec})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the response status for access logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Flush keeps streaming responses working through the recorder.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// LogRequests writes one structured access log line per request.
func LogRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"requestId", RequestIDFrom(r.Context()),
		)
	})
}

// SecurityHeaders applies baseline hardening to every response.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		// Share pages are public but must never be indexed or cached by shared
		// proxies: a revoked link must not stay readable from a cache.
		h.Set("X-Robots-Tag", "noindex, nofollow, noarchive")
		next.ServeHTTP(w, r)
	})
}

// CORS is a strict allow-list CORS layer.
//
// Browser traffic reaches this API same-origin (the SPA, the API and the share
// pages are all served under one prefix), so CORS is not on the critical path
// at all here. It exists for a deliberately cross-origin client — a dashboard
// served from somewhere else, or local development against a deployed backend —
// and it never reflects an arbitrary Origin.
func CORS(allowed []string) Middleware {
	allowSet := make(map[string]struct{}, len(allowed))
	for _, o := range allowed {
		allowSet[strings.TrimRight(o, "/")] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := strings.TrimRight(r.Header.Get("Origin"), "/")
			if origin != "" {
				if _, ok := allowSet[origin]; ok {
					h := w.Header()
					h.Set("Access-Control-Allow-Origin", origin)
					h.Add("Vary", "Origin")
					h.Set("Access-Control-Allow-Methods", "GET,POST,PATCH,DELETE,OPTIONS")
					h.Set("Access-Control-Allow-Headers", "Authorization,Content-Type")
					h.Set("Access-Control-Max-Age", "86400")
					// Only meaningful when credentials are cross-origin; harmless
					// same-origin, and required if a caller opts into direct mode.
					h.Set("Access-Control-Allow-Credentials", "true")
				}
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RealIP extracts a *display* client IP, preferring forwarded headers set by
// Caddy or Cloudflare.
//
// Logging and audit display only — NEVER for authorisation, rate limiting or
// any other security decision. Every value it can return is attacker-supplied
// unless the immediate peer is verified (it is not verified here): it trusts
// `Cf-Connecting-Ip` unconditionally even though this deployment has no
// Cloudflare in front, and it takes the *leftmost* `X-Forwarded-For` entry,
// which is exactly the one a client can forge (Caddy appends, it does not
// replace). For anything security-relevant call ClientIP.
func RealIP(r *http.Request) string {
	if cf := r.Header.Get("Cf-Connecting-Ip"); cf != "" {
		return cf
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, found := strings.Cut(xff, ","); found {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// TrustCloudflare enables honouring `Cf-Connecting-Ip` in ClientIP.
//
// It is false by default and must stay false for this deployment: api.hcyj.xyz
// resolves to an Alibaba Cloud address (8.148.233.134), responses carry
// `via: 1.1 Caddy` and no `cf-ray`, and the backend listens only on a docker
// bridge address that Caddy proxies to — there is no Cloudflare anywhere in the
// path. With no CDN in front, `Cf-Connecting-Ip` is just another client-chosen
// header, so trusting it hands an attacker a fresh rate-limit bucket per
// request. Flip this only after confirming a CDN really terminates *every*
// request reaching this process; a half-fronted deployment is worse than none,
// because the header stays forgeable on the direct path.
//
// Set once during startup: ClientIP reads it on a hot path without
// synchronisation, so do not toggle it at runtime.
var TrustCloudflare bool

// trustedProxyPrefixes are the immediate peers whose forwarded headers are
// believed. Caddy reaches the backend over the docker bridge (172.17.x), which
// is inside 172.16.0.0/12. Parsed once at init — never per request.
var trustedProxyPrefixes = mustParsePrefixes(
	"127.0.0.0/8",    // IPv4 loopback
	"::1/128",        // IPv6 loopback
	"10.0.0.0/8",     // RFC 1918
	"172.16.0.0/12",  // RFC 1918 — docker bridge gateway lives here
	"192.168.0.0/16", // RFC 1918
	"fc00::/7",       // IPv6 unique local addresses
)

// ClientIP returns the client address for security decisions: rate limiting,
// audit, attribution.
//
// It is the *exact inverse* of RealIP where it matters — it trusts a forwarded
// header only when the immediate peer is a known proxy, and it reads
// X-Forwarded-For from the right, because that is the end a trusted proxy
// appended to:
//
//  1. The peer is RemoteAddr with the port stripped.
//  2. An untrusted peer returns the peer itself and every header is ignored. A
//     forged `X-Forwarded-For` or `Cf-Connecting-Ip` therefore has no effect at
//     all on a direct connection — this is the property that makes the return
//     value usable as a security boundary.
//  3. A trusted peer walks X-Forwarded-For right to left, skipping proxies in
//     trustedProxyPrefixes, and returns the first untrusted address. Entries
//     that are not single IP literals (bare junk, host:port, CIDRs) are skipped
//     rather than guessed at, so a non-IP string can never become a bucket.
//  4. `Cf-Connecting-Ip` is ignored entirely unless TrustCloudflare is enabled.
//
// If nothing usable is found the peer is returned, so every unparseable case
// collapses into one shared bucket rather than a fresh one per request.
//
// Honest limits: this function makes IP attribution *unspoofable for a direct
// connection*, it does not make IP-keyed rate limiting a defence. An attacker
// with a handful of real addresses — or a single IPv6 /64, which contains 2^64
// addresses — gets a fresh bucket per source address no matter how correct this
// parsing is. Treat per-IP limits as a speed bump that stops casual single-source
// abuse; the actual backstop for the public application endpoint is a global,
// IP-independent cap (KEY_MAX_PENDING), and that cap must be enforced
// atomically or concurrent requests will overshoot it.
func ClientIP(r *http.Request) string {
	peer := peerHost(r.RemoteAddr)
	peerAddr, ok := parseIP(peer)
	if !ok {
		// RemoteAddr is not an IP at all. It comes from the connection, not from
		// a header, so we pass it through unchanged: callers get one shared
		// "unparseable" bucket, which fails closed rather than fresh.
		return peer
	}
	if !isTrustedProxy(peerAddr) {
		// Direct connection: all forwarded headers are attacker-controlled.
		return peerAddr.String()
	}

	if TrustCloudflare {
		if addr, ok := parseIP(r.Header.Get("Cf-Connecting-Ip")); ok {
			return addr.String()
		}
	}

	// Right to left: the rightmost entry is the one the closest trusted proxy
	// appended, so it is the last value an attacker could not have chosen.
	// Anything further left may be forged, which is why the walk stops at the
	// first untrusted address instead of trusting the leftmost one.
	parts := forwardedFor(r)
	for i := len(parts) - 1; i >= 0; i-- {
		addr, ok := parseIP(parts[i])
		if !ok || isTrustedProxy(addr) {
			continue
		}
		return addr.String()
	}
	return peerAddr.String()
}

// forwardedFor flattens every X-Forwarded-For header, in received order.
func forwardedFor(r *http.Request) []string {
	values := r.Header.Values("X-Forwarded-For")
	if len(values) == 0 {
		return nil
	}
	var out []string
	for _, v := range values {
		out = append(out, strings.Split(v, ",")...)
	}
	return out
}

// peerHost strips the port from RemoteAddr, tolerating a bare host.
func peerHost(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return strings.Trim(remoteAddr, "[]")
}

// parseIP parses a single address token. Ports, CIDRs and free-form junk are
// rejected rather than guessed at: a value that is not an address must never
// become a rate-limit bucket.
//
// IPv4-mapped IPv6 forms are unmapped so one host cannot be split into two
// buckets, and any zone is dropped so a scoped literal keys like an unscoped one.
func parseIP(s string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap().WithZone(""), true
}

// isTrustedProxy reports whether addr is one of the proxies whose forwarded
// headers we believe.
func isTrustedProxy(addr netip.Addr) bool {
	for _, prefix := range trustedProxyPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func mustParsePrefixes(cidrs ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		prefix, err := netip.ParsePrefix(c)
		if err != nil {
			// Constant input: a typo here is a programming error, and failing at
			// init is better than silently trusting nothing (or everything).
			panic("httpx: invalid trusted proxy CIDR " + c + ": " + err.Error())
		}
		out = append(out, prefix.Masked())
	}
	return out
}

// LogError logs an error together with the request correlation id. It is a
// general-purpose helper for a handler that wants to record a failure of its own
// — a background job, a best-effort cleanup, an operation that does not produce
// a response.
//
// It is NOT how a 5xx response is logged: that happens in fail, the single choke
// point every 5xx passes through, so a server fault gets exactly one line.
// Calling this in addition, from a handler that also writes a 5xx, produces the
// duplicate line that arrangement exists to prevent.
func LogError(r *http.Request, msg string, err error) {
	slog.Error(msg, "requestId", RequestIDFrom(r.Context()), "path", r.URL.Path, "error", err)
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(buf)
}
