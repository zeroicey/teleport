package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"strings"
)

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeySession
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

// RequestID assigns a correlation id and echoes it back. It prefers the
// `Cf-Ray` header when present (set by Cloudflare) so a request can be traced
// across the edge and the origin with a single identifier.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("Cf-Ray")
		if id == "" {
			id = r.Header.Get("X-Request-Id")
		}
		if id == "" {
			id = randomHex(16)
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID, id)))
	})
}

// Recover turns a panic into a 500 instead of killing the connection.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic recovered", "requestId", RequestIDFrom(r.Context()), "panic", rec)
				Fail(w, r, http.StatusInternalServerError, "internal_error", "Internal server error", nil)
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

// RealIP extracts the client IP, preferring forwarded headers set by Caddy or
// Cloudflare. Used for logging only — never for authorisation.
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

// LogError logs an error together with the request correlation id.
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
