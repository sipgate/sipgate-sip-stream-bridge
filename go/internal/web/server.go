package web

import (
	"io/fs"
	"net/http"
)

// UIContentSecurityPolicy is the CSP served with every operator-UI response.
//
// It MUST stay byte-identical to the Node UI handler (node/src/web/server.ts);
// both backends serve the same bundle and pin this exact string in their tests.
//
// Distinct from the REST API's `default-src 'none'; frame-ancestors 'none'`
// (internal/api/security.go): the UI must load its own hashed script/style/font
// assets and fetch the same-origin REST + /health endpoints. It keeps
// `default-src 'none'` as the floor and opens only `'self'` (+ data: images).
// No 'unsafe-inline' — the vite build emits external hashed assets, with the
// module-preload polyfill disabled, so 'self' is sufficient.
const UIContentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; font-src 'self'; base-uri 'none'; frame-ancestors 'none'"

// Handler serves the embedded operator-UI bundle. Mount it at /ui:
//
//	r.Mount("/ui", api.BasicAuth(accountSid, authToken)(web.Handler()))
//
// It strips the /ui prefix and serves the bundle root (index.html at /ui/).
// Wrap it with api.BasicAuth at the mount site to reuse the existing
// constant-time credential check — the UI route carries no {AccountSid} path
// param, which BasicAuth tolerates by skipping the path check.
func Handler() http.Handler {
	return handlerFor(distSub())
}

// handlerFor builds the UI handler over an arbitrary fs.FS so tests can supply
// a fixture bundle instead of the compile-time embed.
func handlerFor(assets fs.FS) http.Handler {
	return http.StripPrefix("/ui", uiCSP(http.FileServer(http.FS(assets))))
}

// uiCSP sets the UI security headers on every response (including 404s for
// missing assets). Same header set as api.SecurityHeaders, but with the
// UI-specific CSP above.
func uiCSP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", UIContentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		next.ServeHTTP(w, r)
	})
}
