// Package api implements the Twilio-compatible REST control plane.
//
// This package ships leaf-level building blocks consumed by the HTTP router:
//   - Error: Twilio-shaped JSON error body + WriteJSON helper, with a set of
//     prebuilt constructors covering the Twilio error codes used by the
//     control-plane handlers.
//   - RFC2822, CallJSON, PageJSON, SerializeCall, SerializePage: Twilio-strict
//     JSON serialization for the Call resource and its pagination envelope.
//   - BasicAuth: chi-compatible HTTP Basic Auth middleware with constant-time
//     credential comparison and {AccountSid} URL-path validation.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Error is the Twilio-shaped REST error body. JSON shape:
//
//	{"code":N,"message":"...","more_info":"https://www.twilio.com/docs/errors/N","status":HHH}
//
// Used for all 4xx/5xx responses from internal/api handlers. The shape is
// byte-identical to the Twilio REST API so SDKs (twilio-go, twilio-python, …)
// validate it against their typed error wrappers.
type Error struct {
	Code     int    `json:"code"`
	Message  string `json:"message"`
	MoreInfo string `json:"more_info"`
	Status   int    `json:"status"`
}

// WriteJSON sets Content-Type, writes the HTTP status, and emits the JSON body.
// Idempotent — safe to call once per response. Logs nothing (callers log).
//
// Order is significant: Header().Set must precede WriteHeader, and WriteHeader
// must precede the body write. Calling WriteJSON twice on the same
// http.ResponseWriter is undefined (Go's net/http will log a warning).
func (e *Error) WriteJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(e)
}

// newError constructs an *Error with the more_info URL filled in to Twilio's
// canonical pattern (https://www.twilio.com/docs/errors/<code>).
func newError(code int, message string, status int) *Error {
	return &Error{
		Code:     code,
		Message:  message,
		MoreInfo: fmt.Sprintf("https://www.twilio.com/docs/errors/%d", code),
		Status:   status,
	}
}

// Prebuilt errors used across the control-plane surface. Each is a
// constructor (not a singleton) so callers can mutate Message safely without
// affecting other requests, and so each call emits a fresh allocation
// matching twilio-go's per-request error semantics.

// ErrAuthRequired returns the Twilio 20003 / 401 "Authenticate" error.
// Used by BasicAuth on missing or invalid credentials.
func ErrAuthRequired() *Error {
	return newError(20003, "Authentication Error - No credentials provided", http.StatusUnauthorized)
}

// ErrNotFound returns the Twilio 20404 / 404 "resource not found" error.
// resource is interpolated into the message for parity with Twilio's wording.
func ErrNotFound(resource string) *Error {
	return newError(20404, fmt.Sprintf("The requested resource %s was not found", resource), http.StatusNotFound)
}

// ErrInvalidParams returns the Twilio 21218 / 400 "Invalid parameters" error.
// detail is appended after a colon for caller-supplied context (e.g. which
// field failed validation).
func ErrInvalidParams(detail string) *Error {
	return newError(21218, "Invalid parameters: "+detail, http.StatusBadRequest)
}

// ErrCallNotInProgress returns the Twilio 21220 / 400 "Invalid call state for
// the requested operation" error. Used by the modify-call handler when
// attempted on an already-terminated call.
func ErrCallNotInProgress() *Error {
	return newError(21220, "Invalid call state for the requested operation", http.StatusBadRequest)
}

// ErrTwimlParseFailure returns the Twilio 12100 / 400 "Document parse failure"
// error. Used when the inline Twiml= body is malformed or exceeds length
// limits.
func ErrTwimlParseFailure() *Error {
	return newError(12100, "Document parse failure", http.StatusBadRequest)
}

// ErrTooManyRequests returns the Twilio 20429 / 429 "Too Many Requests" error.
// Used by the dial rate limiter.
func ErrTooManyRequests() *Error {
	return newError(20429, "Too Many Requests", http.StatusTooManyRequests)
}

// ErrHTTPRetrievalFailure returns the Twilio 11200 / 400 "HTTP retrieval
// failure" error. Used by the modify-call handler when fetching TwiML from
// a customer-provided Url= (and optional FallbackUrl=) fails on every
// attempt — e.g. DNS error, TLS handshake, non-2xx response.
//
// detail is appended to the message verbatim so operators can correlate the
// 11200 response with the underlying transport-layer failure (sanitized at
// the webhook.Client boundary — never carries credentials or internal IPs).
func ErrHTTPRetrievalFailure(detail string) *Error {
	return newError(11200, "HTTP retrieval failure: "+detail, http.StatusBadRequest)
}

// ErrPayloadTooLarge returns the Twilio 21617 / 413 "Body exceeds maximum
// length" error. Emitted by api/security.go MaxBytesReader middleware when a
// REST request body exceeds the 64KB cap (anti-DoS hardening, configured at
// Mount-time via SecurityHeaders + MaxBytesReader chi middleware).
//
// Twilio code 21617 ("Body exceeds maximum length") is the closest semantic
// match in Twilio's published vocabulary for body-size violations — no
// dedicated 413 code exists in the Twilio REST error vocabulary. The code was
// originally defined for SMS body limits but its semantics ("the supplied body
// is larger than the limit the server accepts") map cleanly to the REST
// MaxBytesReader bound. Documented at
// https://www.twilio.com/docs/api/errors/21617.
//
// REST endpoints enforce a 64KB max body and emit security headers;
// deliberate MaxBytesReader overflow returns 413 with the Twilio JSON
// error body.
func ErrPayloadTooLarge() *Error {
	return newError(21617, "Request body exceeds 64KB limit", http.StatusRequestEntityTooLarge)
}
