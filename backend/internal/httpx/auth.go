package httpx

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
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

// RequireAgent enforces `Authorization: Bearer <AGENT_SECRET_KEY>`.
//
// The comparison is constant-time so a wrong key cannot be recovered by timing.
func RequireAgent(cfg AuthConfig) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Authorization")
			if header == "" {
				WriteError(w, r, Unauthorized("Missing Authorization header"))
				return
			}
			provided, ok := bearerToken(header)
			if !ok {
				WriteError(w, r, Unauthorized("Authorization header must use the Bearer scheme"))
				return
			}
			if subtle.ConstantTimeCompare([]byte(provided), []byte(cfg.AgentSecretKey)) != 1 {
				WriteError(w, r, Unauthorized("Invalid agent secret"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireSession enforces a valid signed session cookie.
//
// Cloudflare Access is not used in the new deployment, but the
// `Cf-Access-Authenticated-User-Email` header is still honoured if a reverse
// proxy is later configured to supply it.
func RequireSession(cfg AuthConfig) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if email := r.Header.Get("Cf-Access-Authenticated-User-Email"); email != "" {
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
