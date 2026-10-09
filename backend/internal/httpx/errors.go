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
func Fail(w http.ResponseWriter, r *http.Request, status int, code, message string, details map[string]any) {
	writeJSON(w, status, envelope{
		OK:        false,
		Error:     &errorBody{Code: code, Message: message, Details: details},
		RequestID: RequestIDFrom(r.Context()),
	})
}

// WriteError maps any error onto the wire format. Structured *Error values keep
// their status and code; everything else becomes an opaque 500 so internal
// messages never leak.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		Fail(w, r, apiErr.Status, apiErr.Code, apiErr.Message, apiErr.Details)
		return
	}
	LogError(r, "unhandled error", err)
	Fail(w, r, http.StatusInternalServerError, "internal_error", "Internal server error", nil)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(body)
}
