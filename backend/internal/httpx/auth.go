package httpx

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/zeroicey/teleport/backend/internal/domain"
)

// SessionCookieName is the dashboard session cookie.
const SessionCookieName = "teleport_session"

// AuthConfig carries everything the auth middleware needs.
type AuthConfig struct {
	AgentSecretKey string
	SessionSecret  string
	CookieSecure   bool
	CookiePath     string
	SessionTTL     time.Duration
}

// KeyResolver resolves an agent key from its stored hash.
//
// tokenHash is the lowercase sha256 hex digest of the presented bearer token —
// never the plaintext, so a resolver implementation cannot log a credential it
// was never given. Implementations must return ok=false for unknown, revoked
// and expired keys alike: the three are indistinguishable to the caller, and
// callers must not be handed the difference.
type KeyResolver interface {
	Resolve(ctx context.Context, tokenHash string) (domain.Principal, bool)
}

// agentAuthMessage is the one and only message this middleware emits on
// failure. Missing header, non-Bearer scheme, wrong root key, unknown key, a
// resolver miss and an identityless resolver hit all produce a byte-identical
// 401 body, so a client cannot use the response to learn *why* it failed.
const agentAuthMessage = "Invalid agent credentials"

func agentUnauthorized() *Error { return Unauthorized(agentAuthMessage) }

// RequireAgent authenticates an agent request with `Authorization: Bearer <key>`.
//
// Two credential paths are accepted:
//
//  1. the root/break-glass AGENT_SECRET_KEY in cfg (Principal.Root = true), and
//  2. any key registered with resolver, matched by the sha256 hex of the token.
//
// Both paths are evaluated before the decision is made. Short-circuiting on a
// root miss would make "the root key did not match" measurably faster than "the
// hash is not in the table", which tells an attacker whether a guessed token is
// the root key — a signal the pre-existing single-key code did not emit.
//
// A nil resolver disables path 2 entirely and keeps the historical env-only
// behaviour, which callers (and the older single-key tests) rely on.
func RequireAgent(cfg AuthConfig, resolver KeyResolver) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			provided, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				WriteError(w, r, agentUnauthorized())
				return
			}

			// Path 1 — root key.
			//
			// Known and accepted trade-off: subtle.ConstantTimeCompare returns 0
			// as soon as the lengths differ, so the *length* of AGENT_SECRET_KEY
			// is observable. That exposes the size of a fixed server-side secret,
			// never its content, and it gives an attacker no oracle with which to
			// test a guess — the search space is unchanged. Comparing digests
			// instead would hide the length but also hide the fact that it is
			// observable; a visible, documented constant beats an invisible one.
			// What this boolean must never do is decide whether path 2 runs.
			rootMatch := subtle.ConstantTimeCompare([]byte(provided), []byte(cfg.AgentSecretKey)) == 1

			// Path 2 — hashed lookup. Runs unconditionally whenever a resolver is
			// wired in, including when path 1 already succeeded, so the success
			// path and both failure paths execute the same work. The token is
			// hashed here and only the digest is handed to the resolver.
			var stored domain.Principal
			resolved := false
			if resolver != nil {
				stored, resolved = resolver.Resolve(r.Context(), hashToken(provided))
			}

			var principal domain.Principal
			switch {
			case rootMatch:
				// Root exists only in the environment, never in agent_keys.
				principal = domain.Principal{Root: true}
			case resolved && stored.KeyID != "":
				// Do not delete this as "redundant with agentkey's invariant" or
				// as "the resolver already sets Root=false". Root is a property
				// of the env credential alone, minted in exactly one place above.
				// Without this line, any resolver that reports Root=true — a
				// store bug, a new query branch, a test double that leaks into
				// production — silently upgrades a named key to the break-glass
				// identity that can read and revoke every report in the system.
				// One assignment is cheap insurance against a whole-system
				// privilege escalation.
				stored.Root = false
				principal = stored
			default:
				// Two ways to land here: nothing matched, or the resolver claimed
				// a hit that carries no identity (empty KeyID).
				//
				// An empty KeyID is reserved by the contract to mean "root" and
				// is also what root-published reports store as owner_key_id. A
				// non-root principal with an empty KeyID therefore has no
				// legitimate identity: ownership checks compare owner_key_id
				// against KeyID, so it would inherit root's reports. The store
				// cannot currently produce one — ids come from agentkey.NewID() —
				// but leaning on another package's invariant for an ownership
				// boundary is how "unreachable today" becomes reachable later.
				// Fail closed rather than degrade into root.
				WriteError(w, r, agentUnauthorized())
				return
			}

			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), &principal)))
		})
	}
}

// hashToken returns the lowercase sha256 hex digest used as agent_keys.token_hash.
//
// The plaintext token never leaves this function: it is not logged, not echoed
// and not attached to the request context.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// cfAccessEmailHeader is the identity header a Cloudflare Access front end
// injects once it has authenticated the request at the edge.
//
// Nothing about it is trustworthy on its own: it is an ordinary client-supplied
// request header, so honouring it without a trust gate is a full authentication
// bypass of the dashboard.
const cfAccessEmailHeader = "Cf-Access-Authenticated-User-Email"

// maxCFAccessEmailLen is RFC 5321's limit on a forward-path: 320 octets.
const maxCFAccessEmailLen = 320

// TrustCFAccess enables honouring cfAccessEmailHeader in RequireSession.
//
// It is false by default and must stay false for this deployment, for exactly
// the reason TrustCloudflare is false: there is no Cloudflare in front of
// api.hcyj.xyz, so this header is pure client input. While RequireSession
// honoured it unconditionally,
//
//	curl -H 'Cf-Access-Authenticated-User-Email: anyone@example.invalid' \
//	     .../api/admin/session
//
// answered 200, and the same request could POST /api/admin/keys and be handed a
// plaintext key — a complete bypass with no password. The old comment ("not used
// in the new deployment, but honoured if a reverse proxy is later configured")
// described an intent that had no matching origin check in the code.
//
// Enable it only when BOTH hold:
//
//   - Cloudflare Access authenticates *every* request before it reaches Caddy,
//     and
//   - Caddy forwards this header unchanged to this process.
//
// The trusted-peer check in cfAccessEmail is necessary but not sufficient: it
// proves the header arrived from the local proxy, not that Cloudflare is
// genuinely in the path. If an attacker can reach Caddy or this process
// directly, the header stays forgeable and the flag must remain off.
//
// Set once during startup: the value is read on a hot path without
// synchronisation, so do not toggle it at runtime.
var TrustCFAccess bool

// cfAccessAcceptedOnce keeps the "trust is live" warning to one line per
// process rather than one per request.
var cfAccessAcceptedOnce sync.Once

// WarnIfTrustingCFAccess logs a loud warning when the CF Access shortcut is
// enabled, so switching it on can never be silent. Call it once, early in
// startup (before serving), and again after any dynamic configuration load.
func WarnIfTrustingCFAccess() {
	if TrustCFAccess {
		slog.Warn("SECURITY: trusting " + cfAccessEmailHeader +
			" — safe only when Cloudflare Access fronts every path to this process and Caddy forwards the header")
	}
}

// cfAccessEmail returns the identity asserted by Cloudflare Access, or "" when
// the header must be ignored entirely.
//
// All three gates must pass. A failure means "fall back to the cookie", never
// "trust the header anyway", so a valid session still works:
//
//  1. TrustCFAccess is enabled (see its doc for why it is off by default),
//  2. the immediate peer is a trusted proxy, so the header can only be believed
//     when it was handed over by our own front end, and
//  3. the value looks like an email address: free of control characters, then
//     trimmed, at most 320 bytes, with a non-empty local part and domain.
//
// The control-character scan runs on the *raw* header, before trimming: a
// trailing CR would otherwise be trimmed into a perfectly valid-looking
// identity, and a newline in a value that reaches logs or audit records is an
// injection primitive (Go's server does not re-validate a header we set
// ourselves). Anything that ever contained one is rejected outright rather than
// silently normalised.
func cfAccessEmail(r *http.Request) string {
	if !TrustCFAccess {
		return ""
	}
	peer, ok := parseIP(peerHost(r.RemoteAddr))
	if !ok || !isTrustedProxy(peer) {
		return ""
	}

	raw := r.Header.Get(cfAccessEmailHeader)
	for i := 0; i < len(raw); i++ {
		if c := raw[i]; c < 0x20 || c == 0x7f {
			return ""
		}
	}

	email := strings.TrimSpace(raw)
	if email == "" || len(email) > maxCFAccessEmailLen {
		return ""
	}
	at := strings.IndexByte(email, '@')
	if at <= 0 || at == len(email)-1 {
		return ""
	}
	return email
}

// RequireSession enforces a valid signed session cookie.
//
// A Cloudflare Access identity header is accepted as an alternative, but only
// behind the explicit TrustCFAccess opt-in and the checks in cfAccessEmail; an
// ignored or malformed header falls through to the cookie path.
func RequireSession(cfg AuthConfig) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if email := cfAccessEmail(r); email != "" {
				cfAccessAcceptedOnce.Do(func() {
					WarnIfTrustingCFAccess()
				})
				now := time.Now().Unix()
				s := &Session{Subject: email, Issued: now, Expires: now + 60}
				next.ServeHTTP(w, r.WithContext(WithSession(r.Context(), s)))
				return
			}

			cookie, err := r.Cookie(SessionCookieName)
			if err != nil || cookie.Value == "" {
				WriteError(w, r, Unauthorized("Dashboard session required"))
				return
			}
			session, err := VerifySession(cookie.Value, cfg.SessionSecret)
			if err != nil || session == nil {
				WriteError(w, r, Unauthorized("Dashboard session required"))
				return
			}
			next.ServeHTTP(w, r.WithContext(WithSession(r.Context(), session)))
		})
	}
}

func bearerToken(header string) (string, bool) {
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

// BuildSession creates a session payload valid for ttl.
//
// A non-positive ttl is treated as "already expired", which would mint a cookie
// that fails verification on the very next request. That is a silent
// misconfiguration, so a sane fallback is applied instead of producing an
// unusable session.
func BuildSession(subject string, ttl time.Duration) *Session {
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	now := time.Now().Unix()
	return &Session{Subject: subject, Issued: now, Expires: now + int64(ttl.Seconds())}
}

// signPayload returns base64url(payload) + "." + base64url(HMAC-SHA256).
func signPayload(payload []byte, secret string) string {
	mac := hmacSHA256([]byte(secret), payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac)
}

// verifyPayload recomputes the HMAC over the *received* payload bytes and
// compares in constant time, then decodes the payload.
func verifyPayload(value, secret string) ([]byte, bool) {
	body, sig, found := strings.Cut(value, ".")
	if !found || body == "" || sig == "" {
		return nil, false
	}
	rawPayload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, false
	}
	expected := hmacSHA256([]byte(secret), rawPayload)
	provided, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return nil, false
	}
	if subtle.ConstantTimeCompare(expected, provided) != 1 {
		return nil, false
	}
	return rawPayload, true
}

// SignSession serializes and signs a session.
func SignSession(s *Session, secret string) (string, error) {
	payload, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return signPayload(payload, secret), nil
}

// VerifySession returns the payload, or an error when the cookie is malformed,
// forged or expired.
func VerifySession(cookieValue, secret string) (*Session, error) {
	payload, ok := verifyPayload(cookieValue, secret)
	if !ok {
		return nil, errors.New("invalid session signature")
	}
	var s Session
	if err := json.Unmarshal(payload, &s); err != nil {
		return nil, err
	}
	if s.Expires == 0 || time.Now().Unix() >= s.Expires {
		return nil, errors.New("session expired")
	}
	return &s, nil
}

// SessionCookie serializes the Set-Cookie header value for a session.
//
// HttpOnly + SameSite=Lax; `Secure` is configurable so plain-HTTP local
// development works while production stays strict.
func SessionCookie(value string, maxAge time.Duration, secure bool, path string) string {
	return serializeCookie(value, maxAge, secure, path)
}

// ClearSessionCookie expires the session cookie.
func ClearSessionCookie(secure bool, path string) string {
	return serializeCookie("", 0, secure, path)
}

func serializeCookie(value string, maxAge time.Duration, secure bool, path string) string {
	var b strings.Builder
	b.WriteString(SessionCookieName)
	b.WriteString("=")
	b.WriteString(value)
	b.WriteString("; Path=")
	if path == "" {
		path = "/"
	}
	b.WriteString(path)
	b.WriteString("; HttpOnly; SameSite=Lax")
	if secure {
		b.WriteString("; Secure")
	}
	if maxAge <= 0 {
		b.WriteString("; Max-Age=0")
	} else {
		b.WriteString("; Max-Age=")
		b.WriteString(itoa(int(maxAge.Seconds())))
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
