// Package api — security middleware (anti-DoS / response-header hardening).
//
// This file ships two chi-compatible middleware factories:
//
//   - SecurityHeaders() — sets Strict-Transport-Security,
//     Content-Security-Policy, and X-Content-Type-Options on every response.
//   - MaxBytesReader(limit) — bounds the REST request body at `limit` bytes
//     via a two-tier defense: Tier 1 (Content-Length pre-check) emits 413 +
//     Twilio JSON BEFORE reading the body; Tier 2 (http.MaxBytesReader body
//     wrap) catches chunked-encoded oversize bodies that lie about
//     Content-Length or omit it.
//
// Both factories are wired in api/server.go Mount() BEFORE BasicAuth so
// security headers are present on the 401 path and 413 oversize-body refusal
// fires before any credential check (anti-DoS — minimum work to reject).
//
// REST endpoints enforce a 64KB max body and emit security headers; a
// deliberate MaxBytesReader overflow returns 413 with the Twilio JSON
// error body.
package api

import (
	"net/http"
)

// SecurityHeaders returns chi-compatible middleware that sets the three
// locked security headers on every REST response (including 401/4xx error
// responses). Headers are written BEFORE next.ServeHTTP, so even if the next
// handler returns an error, the operator gets HSTS/CSP/nosniff.
//
// Locked header set:
//
//   - Strict-Transport-Security: max-age=63072000; includeSubDomains
//     (2-year HSTS, per OWASP cheatsheet; preload not asserted because the
//     bridge runs behind a K8s ingress that owns public TLS termination).
//   - Content-Security-Policy: default-src 'none'; frame-ancestors 'none'
//     (REST-only API; no HTML/script content; minimum-permissive policy).
//   - X-Content-Type-Options: nosniff (defense against MIME-type sniffing
//     attacks on JSON bodies).
//
// Used by Mount() in api/server.go BEFORE BasicAuth so security headers are
// present even on 401 unauthenticated responses (load-bearing ordering;
// pinned by TestSecurityHeaders_PresentOn401).
func SecurityHeaders() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			h.Set("X-Content-Type-Options", "nosniff")
			next.ServeHTTP(w, r)
		})
	}
}

// MaxBytesReader returns chi-compatible middleware that bounds every REST
// request body at `limit` bytes.
//
// Two-tier defense — both tiers required:
//
//  1. **Tier 1 (Content-Length pre-check):** if r.ContentLength > limit,
//     emit a Twilio-shaped 413 JSON body immediately and return without
//     reading any body bytes. Rejects pre-declared oversize at the cheapest
//     possible point. Covers the canonical attack shape — curl,
//     twilio-python, twilio-go, twilio-node and every other legitimate
//     client all set Content-Length on POSTs.
//
//  2. **Tier 2 (http.MaxBytesReader body wrap):** wrap r.Body with
//     http.MaxBytesReader(w, r.Body, limit). Catches chunked-encoded
//     oversize bodies that lie about Content-Length (Content-Length: 100
//     with a 1MB chunked body) or omit Content-Length entirely
//     (Transfer-Encoding: chunked with no CL). The downstream handler
//     reading r.Body (e.g. modifyCallHandler's r.ParseForm()) receives a
//     *http.MaxBytesError once limit bytes are consumed; the existing
//     parseModifyOpts error path emits 400 invalid_params for that case.
//     Without Tier 2, an attacker trivially bypasses Tier 1 by setting
//     Content-Length: 0 + Transfer-Encoding: chunked + 1GB body.
//
// The 4xx-vs-413 asymmetry on chunked oversize is accepted — chunked
// oversize still gets refused with a 4xx and operator monitoring still
// surfaces the rejection via api_requests_total{status="4xx"}.
//
// Production cap is 64<<10 (65536 bytes) — sized at 16x the largest
// legitimate Twiml= body (4000 chars) plus headroom for status-callback-
// related form fields.
func MaxBytesReader(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Tier 1: cheap Content-Length pre-check.
			//
			// Strict `>`: a request with Content-Length == limit is allowed
			// through. Tier 2's http.MaxBytesReader is inclusive (allows
			// exactly `limit` bytes; errors on the (limit+1)th read), so the
			// boundary stays consistent across the two tiers.
			if r.ContentLength > limit {
				ErrPayloadTooLarge().WriteJSON(w)
				return
			}
			// Tier 2: wrap r.Body so chunked-encoding / fraudulent-CL /
			// unknown-length requests are bounded. Downstream handler reading
			// r.Body receives *http.MaxBytesError once limit bytes are
			// consumed.
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

// Compile-time assertions that the factories satisfy chi's middleware
// signature (which is structurally `func(http.Handler) http.Handler` —
// chi.Router.Use accepts this via Go's interface assignability without a
// chi-typed import). The mount-site assertion happens organically when
// api/server.go calls `r.Use(SecurityHeaders())` and
// `r.Use(MaxBytesReader(64<<10))` on a `chi.Router` — that's the load-bearing
// compatibility check. These assertions are belt-and-suspenders to catch
// signature drift earlier than mount time.
var (
	_ func(http.Handler) http.Handler = SecurityHeaders()
	_ func(http.Handler) http.Handler = MaxBytesReader(64 << 10)
)
