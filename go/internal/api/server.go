package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/bridge"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/config"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/observability"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/sip"
)

// BridgeQuerier is the read-only surface internal/api needs from CallManager.
// *bridge.CallManager satisfies this interface implicitly via Go's structural
// typing — no extra adapter is required.
//
// We deliberately reference bridge.BridgeCall (the interface defined in the
// bridge package) rather than the api-local CallView (defined in json.go):
// BridgeQuerier is the contract the api package CONSUMES from bridge, so the
// api → bridge dependency direction is natural here. SerializeCall
// internally takes a CallView, but bridge.BridgeCall has the same method
// set — Go will pass any bridge.BridgeCall to SerializeCall transparently.
type BridgeQuerier interface {
	List() []bridge.BridgeCall
	GetByCallSid(callSid string) (bridge.BridgeCall, bool)
}

// pathPrefix is the Twilio-style API root for a given AccountSid:
// "/2010-04-01/Accounts/{AccountSid}". Used to build canonical Call URIs
// embedded in JSON responses (uri, first_page_uri, …).
func pathPrefix(accountSid string) string {
	return "/2010-04-01/Accounts/" + accountSid
}

// Mount registers the Twilio Call REST routes on r:
//
//	GET  /2010-04-01/Accounts/{AccountSid}/Calls.json              → list
//	GET  /2010-04-01/Accounts/{AccountSid}/Calls/{CallSid}.json    → get
//	POST /2010-04-01/Accounts/{AccountSid}/Calls/{CallSid}.json    → modify
//
// All routes are wrapped in BasicAuth (constant-time credential comparison +
// URL-path AccountSid validation) and a per-route metricsMiddleware that
// records api_requests_total + api_request_duration_seconds.
//
// manager is the *bridge.CallManager; it is passed directly (rather than
// through BridgeQuerier) because modifyCallHandler needs AcquirePort /
// ReleasePort for the <Dial> callee-leg RTP allocation. The decision was
// to pass *bridge.CallManager directly rather than widening the interface.
//
// forwarder is the *sip.Forwarder constructed once in main.go; it is threaded
// through to modifyCallHandler so the midCallAdapter.PerformDial call can
// invoke Forwarder.Dial without a global variable.
//
// wc is the outbound HTTP client used by the modify-call handler for the
// Url= TwiML fetch path. The <Dial> action-callback POST goes through a
// separate signed *http.Client (actionPoster) so the two surfaces have
// independent failure domains.
//
// log is the per-server logger; the modify-call handler enriches it
// per-request via midCallAdapter so verb-side warn-and-skip logs always
// carry call_sid + account_sid.
// actionPoster is the signed *http.Client (the signed POSTer for <Dial>
// action callbacks). Mount threads it down into modifyCallHandler.
// Production callers (cmd/.../main.go) build it once via
// api.NewSignedActionPoster(cfg.AuthToken). Tests use the shared
// testActionPoster() helper to keep the per-test boilerplate flat.
// May be nil in legacy fixtures that never exercise the action-callback
// path (midCallAdapter falls back to the unsigned webhookC path in that case).
func Mount(r chi.Router, q BridgeQuerier, manager *bridge.CallManager, accountSid, authToken string, m *observability.Metrics, wc webhookFetcher, actionPoster *http.Client, log zerolog.Logger, forwarder *sip.Forwarder, cfg config.Config) {
	r.Route("/2010-04-01/Accounts/{AccountSid}", func(r chi.Router) {
		// Anti-DoS hardening: security middleware fires BEFORE BasicAuth
		// so HSTS/CSP/nosniff are present on every response (including
		// 401), and 413 oversize-body refusal fires before any credential
		// check (minimum work to reject an unauthenticated 10 MB body POST).
		//
		// Order is load-bearing:
		//   SecurityHeaders → MaxBytesReader → BasicAuth → metricsMiddleware
		//   → handler
		// Pinned by TestMount_AuthRequired_HSTS_Present and
		// TestMount_BodyOverflow_Returns413_BeforeAuth in server_test.go.
		r.Use(SecurityHeaders())
		r.Use(MaxBytesReader(64 << 10))
		r.Use(BasicAuth(accountSid, authToken))
		r.With(metricsMiddleware(m, "list_calls")).Get("/Calls.json", listCallsHandler(q, accountSid))
		r.With(metricsMiddleware(m, "get_call")).Get("/Calls/{CallSid}.json", getCallHandler(q, accountSid))
		r.With(metricsMiddleware(m, "modify_call")).Post("/Calls/{CallSid}.json", modifyCallHandler(q, manager, accountSid, wc, actionPoster, m, log, forwarder, cfg))
	})
}

// rwCapture wraps http.ResponseWriter to capture the status code for the
// metrics middleware. WriteHeader records the explicit code; Write defaults
// to 200 if the handler emits a body without first calling WriteHeader (the
// net/http convention).
type rwCapture struct {
	http.ResponseWriter
	code int
}

func (w *rwCapture) WriteHeader(c int) {
	w.code = c
	w.ResponseWriter.WriteHeader(c)
}

func (w *rwCapture) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// metricsMiddleware records APIRequestsTotal + APIRequestDurationSeconds for
// the given route, capturing the response status via rwCapture.
//
// next.ServeHTTP runs synchronously, so the after-call observation always
// reflects the final state of the response — no race between handler return
// and metrics recording is possible.
//
// The helper accepts a nil *observability.Metrics so test fixtures that
// mount the router without a metrics registry stay simple. Production
// callers always pass a real registry.
func metricsMiddleware(m *observability.Metrics, route string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cw := &rwCapture{ResponseWriter: w}
			start := time.Now()
			next.ServeHTTP(cw, r)
			if m == nil {
				return
			}
			dur := time.Since(start).Seconds()
			// metrics:dynamic-allowed route is the bounded enum from
			// Mount() (list_calls|get_call|modify_call|health|metrics|
			// unknown — passed as a literal at every metricsMiddleware
			// call site); r.Method is bounded by HTTP semantics
			// (GET|POST|PUT|DELETE — the api_requests_total allowlist
			// enumerates the four canonical values). The walker can't
			// trace function-param + struct-field provenance, so an
			// explicit opt-out gates the call.
			m.APIRequestsTotal.WithLabelValues(route, r.Method, observability.BucketStatus(cw.code)).Inc()
			// metrics:dynamic-allowed route — same provenance as above.
			m.APIRequestDurationSeconds.WithLabelValues(route).Observe(dur)
		})
	}
}

// listCallsHandler and getCallHandler live in internal/api/calls.go
// alongside parseIntDefault. They are referenced by Mount above.
