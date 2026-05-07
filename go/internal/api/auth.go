package api

import (
	"crypto/subtle"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// BasicAuth returns chi-compatible middleware that enforces HTTP Basic Auth
// with the configured AccountSid as username and authToken as password. Both
// comparisons use subtle.ConstantTimeCompare to defeat timing oracles that
// would otherwise let an attacker recover a few characters per request.
//
// The middleware ALSO validates that the {AccountSid} URL-path param matches
// the configured AccountSid. Mismatch returns 401 (NOT 404) — auth boundary;
// 404 would leak which AccountSid values exist on the server, enabling
// account enumeration.
//
// Routes mounted under this middleware MUST extract {AccountSid} via
// chi.URLParam to participate in path validation. Routes that have no
// {AccountSid} URL-param (e.g. /health, /metrics) skip the path check
// automatically — but those routes should not be mounted under BasicAuth in
// the first place; this skip is a safety net, not the contract.
//
// 401 responses include both:
//   - WWW-Authenticate: Basic realm="Twilio API" header
//   - Twilio-shaped JSON error body (ErrAuthRequired)
func BasicAuth(accountSid, authToken string) func(http.Handler) http.Handler {
	sidBytes := []byte(accountSid)
	tokBytes := []byte(authToken)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, pass, ok := r.BasicAuth()
			if !ok {
				unauth(w)
				return
			}
			// Compare both fields BEFORE branching on the result so the
			// total wall-clock time is independent of which field (if any)
			// is wrong. Using bitwise AND on the two int results keeps the
			// comparison branch-free at the assembly level.
			userOK := subtle.ConstantTimeCompare([]byte(user), sidBytes) == 1
			passOK := subtle.ConstantTimeCompare([]byte(pass), tokBytes) == 1
			if !(userOK && passOK) {
				unauth(w)
				return
			}
			// URL-path AccountSid validation. chi.URLParam returns "" if the
			// route has no {AccountSid} param — in that case we skip
			// (intended for sub-routers that don't carry the SID in the URL).
			if pathSid := chi.URLParam(r, "AccountSid"); pathSid != "" {
				if subtle.ConstantTimeCompare([]byte(pathSid), sidBytes) != 1 {
					unauth(w)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// unauth writes the 401 response: WWW-Authenticate header THEN the JSON body
// via ErrAuthRequired (which sets Content-Type and status itself). Header
// must be set before WriteJSON calls WriteHeader; net/http buffers
// pre-WriteHeader header mutations, so this ordering is correct.
func unauth(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="Twilio API"`)
	ErrAuthRequired().WriteJSON(w)
}
