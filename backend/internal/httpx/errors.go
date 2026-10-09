// Package httpx holds transport-level helpers: the response envelope, the
// structured error type and the middleware chain.
//
// The wire format is unchanged from the previous Cloudflare Workers
// implementation, so existing agents and the Vue dashboard keep working:
//
//	{ "ok": true,  "data": { ... }, "requestId": "..." }
//	{ "ok": false, "error": { "code": "...", "message": "..." }, "requestId": "..." }
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

// Error is a structured HTTP error. Handlers return these; the error
// middleware maps them onto the JSON envelope.
type Error struct {
	Status  int
	Code    string
	Message string
	// Details is merged into the response body, e.g. {"field": "title"}.
	Details map[string]any
	// cause is logged but never sent to the client.
	cause error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s (%d): %s: %v", e.Code, e.Status, e.Message, e.cause)
	}
	return fmt.Sprintf("%s (%d): %s", e.Code, e.Status, e.Message)
}

func (e *Error) Unwrap() error { return e.cause }

// WithCause attaches an internal cause for logging only.
func (e *Error) WithCause(err error) *Error {
	e.cause = err
	return e
}

func BadRequest(message string, details map[string]any) *Error {
	return &Error{Status: http.StatusBadRequest, Code: "bad_request", Message: message, Details: details}
}

func Unauthorized(message string) *Error {
	return &Error{Status: http.StatusUnauthorized, Code: "unauthorized", Message: message}
}

// NotFound is used for unknown, revoked and expired tokens alike so a caller
// cannot distinguish "never existed" from "was revoked".
func NotFound(message string) *Error {
	return &Error{Status: http.StatusNotFound, Code: "not_found", Message: message}
}

// Gone marks a share link that existed but has expired.
func Gone(message string) *Error {
	return &Error{Status: http.StatusGone, Code: "gone", Message: message}
}

func PayloadTooLarge(message string) *Error {
	return &Error{Status: http.StatusRequestEntityTooLarge, Code: "payload_too_large", Message: message}
}

func Internal(message string) *Error {
	return &Error{Status: http.StatusInternalServerError, Code: "internal_error", Message: message}
}

// envelope is the canonical response shape.
type envelope struct {
	OK        bool           `json:"ok"`
	Data      any            `json:"data,omitempty"`
	Error     *errorBody     `json:"error,omitempty"`
	RequestID string         `json:"requestId,omitempty"`
	Extra     map[string]any `json:"-"` // reserved; not serialized
}

type errorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// OK writes a 200 (or explicit status) success envelope.
func OK(w http.ResponseWriter, r *http.Request, data any, status ...int) {
	code := http.StatusOK
	if len(status) > 0 {
		code = status[0]
	}
	writeJSON(w, code, envelope{
		OK:        true,
		Data:      data,
		RequestID: RequestIDFrom(r.Context()),
	})
}

// Fail writes an error envelope with an explicit status and code.
//
// This is the single choke point for 5xx responses: fail below logs them, so a
// server fault cannot be written without leaving a log line, whichever caller
// produced it. Use WriteError when the error carries a cause worth preserving;
// use Fail directly when it does not.
func Fail(w http.ResponseWriter, r *http.Request, status int, code, message string, details map[string]any) {
	fail(w, r, status, code, message, details, nil)
}

// fail is Fail plus the cause that belongs in the log and must never reach the
// client.
//
// The cause stays out of the exported signature so existing callers keep
// working, while WriteError can still hand one over. The invariant this buys is
// "a 5xx has exactly one log line": WriteError must not log on its own path, or
// one failure would print twice and read as two incidents.
func fail(w http.ResponseWriter, r *http.Request, status int, code, message string, details map[string]any, cause error) {
	if status >= http.StatusInternalServerError {
		logServerFault(r, status, code, message, cause)
	}
	writeJSON(w, status, envelope{
		OK:        false,
		Error:     &errorBody{Code: code, Message: message, Details: details},
		RequestID: RequestIDFrom(r.Context()),
	})
}

// WriteError maps any error onto the wire format. Structured *Error values keep
// their status and code; everything else becomes an opaque 500 so internal
// messages never leak.
//
// The cause travels to fail, which logs it for 5xx and never puts it in the
// response. An opaque "Internal server error" on the wire is deliberate, but it
// must not be the only record that anything happened — before this, a 500 built
// with Internal(...).WithCause(err) left a log line containing nothing but the
// 500, so a SQLite "database is locked" or any future failure was invisible to
// whoever had to diagnose it. 4xx responses are the caller's problem and are not
// logged at error level, or real faults would drown in 404/401 noise.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		fail(w, r, apiErr.Status, apiErr.Code, apiErr.Message, apiErr.Details, apiErr.cause)
		return
	}
	fail(w, r, http.StatusInternalServerError, "internal_error", "Internal server error", nil, err)
}

// logServerFault records a 5xx together with the cause that produced it.
//
// Called only from fail, so every 5xx gets exactly one line. The cause stays
// here: it is exactly what makes a 500 diagnosable, and exactly what the
// response must never carry. requestId ties the line back to the X-Request-Id
// the client was given, so "I got a 500" can be traced to this occurrence.
//
// Log-injection safety rests on the configured handler quoting control
// characters. main.go installs slog.NewTextHandler, whose needsQuoting path
// runs strconv.AppendQuote, so a cause or path containing "\n" is escaped
// instead of forging a log line. Do NOT swap in a handler that writes values
// verbatim — the cause can carry user-influenced text (a path, a store error
// wrapping a driver message).
//
// Causes attached with WithCause are internal errors — a driver failure, a
// wrapped store error, a crypto/rand failure. They must never carry a token,
// a token hash, a claim secret or a request body, and this logger must never be
// handed one: the log is not a place where credentials get a second life.
//
// A cause of type panicError adds `panic=true`, so alerting can match a stable
// field instead of the "panic: " prefix of a string that is only formatting.
func logServerFault(r *http.Request, status int, code, message string, cause error) {
	attrs := []any{
		"requestId", RequestIDFrom(r.Context()),
		"path", r.URL.Path,
		"status", status,
		"code", code,
		"message", message,
	}
	if cause != nil {
		attrs = append(attrs, "cause", cause)
		var panicked panicError
		if errors.As(cause, &panicked) {
			attrs = append(attrs, "panic", true)
		}
	}
	slog.Error("request failed", attrs...)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(body)
}
