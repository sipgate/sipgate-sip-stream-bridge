package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/bridge"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/config"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/identity"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/observability"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/sip"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/twiml"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/webhook"
)

// twimlMaxLen is Twilio's documented inline-Twiml= body limit.
// Bodies above this fail with HTTP 400 + Twilio error 12100 ("Document parse failure").
const twimlMaxLen = 4000

// urlFetchTimeout caps the wall-clock budget for the optional Url= TwiML fetch
// (primary + fallback). Aligns with webhook.NewClient's outer 15s budget so
// the request returns to the caller within a reasonable bound.
const urlFetchTimeout = 15 * time.Second

// modifyOpts is the parsed at-most-one-of body of a POST /Calls/{Sid}.json
// request. Field names match the Twilio body parameters case-sensitively
// (TwiML / Url / Status etc).
//
// Honors {Twiml, Url, Status}; FallbackUrl + FallbackMethod cooperate
// with Url for the primary-then-fallback fetch in webhook.Client.
// StatusCallback / StatusCallbackMethod / StatusCallbackEvent express the
// per-call status-callback subscription. These can be set alongside
// Twiml/Url/Status OR alone (Twilio treats StatusCallback-only as
// "subscribe; do not change call state" — see the relaxed at-least-one-of
// check in parseModifyOpts).
type modifyOpts struct {
	Twiml          string
	URL            string // form param "Url"
	Method         string // "GET" | "POST"; default "POST"
	Status         string // currently only "completed" is honored
	FallbackURL    string // form param "FallbackUrl"
	FallbackMethod string // form param "FallbackMethod"

	// Subscribe to per-call status callbacks via the REST modify-call body.
	// StatusCallbackEvent supports two encoding forms which the parser
	// flattens:
	//   (a) repeated keys: StatusCallbackEvent=initiated&StatusCallbackEvent=ringing
	//   (b) space-OR-comma-separated: StatusCallbackEvent=initiated+ringing
	//                                 or StatusCallbackEvent=initiated,ringing
	StatusCallback       string
	StatusCallbackMethod string
	StatusCallbackEvent  []string
}

// webhookFetcher is the narrow interface modifyCallHandler needs from a
// webhook.Client. The concrete *webhook.Client implements this implicitly via
// Go's structural typing; tests inject a fake to exercise success / failure
// branches without standing up a real httptest.NewTLSServer.
type webhookFetcher interface {
	FetchWithFallback(ctx context.Context, primary, fallback webhook.FetchTarget) (*webhook.FetchResult, error)
}

// parseModifyOpts reads the form-encoded body of a modify-call POST and
// returns a populated modifyOpts. Validation rules (Twilio-strict):
//
//   - Body must parse as application/x-www-form-urlencoded.
//   - At most one of {Twiml, Url, Status} may be set; multi-set returns
//     HTTP 400 + Twilio 21218.
//   - At least one must be set; none-set returns HTTP 400 + Twilio 21218.
//   - len(Twiml) ≤ 4000 chars; over returns HTTP 400 + Twilio 12100 (matches
//     Twilio's own malformed-document mapping for oversized inline TwiML).
//   - Status, when non-empty, must equal "completed" (only this terminal
//     value is currently honored).
//   - Default Method to "POST" when Url is set without an explicit Method.
//
// Returns (opts, nil) on success, (nil, *Error) on validation failure. The
// caller emits the *Error JSON via WriteJSON.
//
// When StatusCallback fails the SSRF pre-flight, this helper additionally
// emits a log.Warn() AND increments
// status_callback_failures_total{reason="ssrf_rejected"} so operator
// dashboards see this distinct from the StatusClient.Enqueue-side SSRF
// rejection. m and logger are nil-safe for legacy fixtures that never
// wired observability.
func parseModifyOpts(r *http.Request, m *observability.Metrics, logger zerolog.Logger) (*modifyOpts, *Error) {
	if err := r.ParseForm(); err != nil {
		return nil, ErrInvalidParams("malformed body")
	}
	opts := &modifyOpts{
		Twiml:          r.PostFormValue("Twiml"),
		URL:            r.PostFormValue("Url"),
		Method:         r.PostFormValue("Method"),
		Status:         r.PostFormValue("Status"),
		FallbackURL:    r.PostFormValue("FallbackUrl"),
		FallbackMethod: r.PostFormValue("FallbackMethod"),
	}

	// StatusCallback / StatusCallbackMethod / StatusCallbackEvent. The
	// Twiml/Url/Status mutual exclusion is unchanged; StatusCallback is
	// independent of that constraint and may be set alongside any of them OR
	// alone (Twilio treats StatusCallback-only as "subscribe; do not change
	// call state").
	opts.StatusCallback = r.PostFormValue("StatusCallback")
	opts.StatusCallbackMethod = r.PostFormValue("StatusCallbackMethod")
	if vs := r.PostForm["StatusCallbackEvent"]; len(vs) > 0 {
		// Twilio accepts BOTH repeated keys AND each key carrying a multi-token
		// value with space OR comma separators. Flatten across keys, then route
		// through twiml.ParseStatusCallbackEvents for unified separator handling
		// + strict enum gate (I-3 fix).
		var events []string
		for _, v := range vs {
			tok, perr := twiml.ParseStatusCallbackEvents(v)
			if perr != nil {
				// Body-param validation error — Twilio code 21218 with the
				// unknown value cited verbatim so customers can diagnose typos.
				return nil, ErrInvalidParams(fmt.Sprintf("StatusCallbackEvent: %s (Twilio code 21218)", perr.Error()))
			}
			events = append(events, tok...)
		}
		opts.StatusCallbackEvent = events
	}

	// Pre-flight URL validation (no DNS).
	if opts.StatusCallback != "" {
		if err := webhook.ValidateCallbackURL(opts.StatusCallback); err != nil {
			// Log + metric on REST-side SSRF rejection so operator
			// dashboards distinguish this surface from the
			// StatusClient.Enqueue SSRF rejection (which also increments
			// the same counter via internal/webhook).
			//
			// metrics:url-allowed callback_host: SSRF-rejected URL is a
			// security event; host-only (no path/query) at WARN level is
			// operator-required. The full URL bytes never enter the log
			// — urlHostMasked extracts host only.
			logger.Warn().
				Err(err).
				Str(observability.FieldCallSid, chi.URLParam(r, "CallSid")).
				Str("callback_host", urlHostMasked(opts.StatusCallback)).
				Msg("status_callback: REST modify-call rejected by SSRF pre-flight")
			if m != nil && m.StatusCallbackFailuresTotal != nil {
				m.StatusCallbackFailuresTotal.WithLabelValues("ssrf_rejected").Inc()
			}
			return nil, ErrInvalidParams("StatusCallback: " + err.Error())
		}
		// Default method to POST per Twilio convention.
		if opts.StatusCallbackMethod == "" {
			opts.StatusCallbackMethod = http.MethodPost
		}
		method := strings.ToUpper(opts.StatusCallbackMethod)
		if method != http.MethodPost && method != http.MethodGet {
			return nil, ErrInvalidParams("StatusCallbackMethod must be POST or GET")
		}
		opts.StatusCallbackMethod = method
	}

	count := 0
	if opts.Twiml != "" {
		count++
	}
	if opts.URL != "" {
		count++
	}
	if opts.Status != "" {
		count++
	}
	if count > 1 {
		return nil, ErrInvalidParams("at most one of {Twiml, Url, Status} may be set")
	}
	// A body with ONLY StatusCallback (no Twiml/Url/Status) is valid —
	// Twilio treats it as "subscribe; do not change call state". The
	// at-least-one-of check therefore includes StatusCallback.
	if count == 0 && opts.StatusCallback == "" {
		return nil, ErrInvalidParams("at least one of {Twiml, Url, Status, StatusCallback} required")
	}
	// Twiml length cap is checked BEFORE the Status enum so an oversized Twiml
	// with no Status still returns 12100 (matches Twilio's behavior — they
	// classify oversized inline TwiML as "Document parse failure").
	if len(opts.Twiml) > twimlMaxLen {
		return nil, ErrTwimlParseFailure()
	}
	if opts.Status != "" && opts.Status != "completed" {
		return nil, ErrInvalidParams("Status must be 'completed' (only this terminal value is supported)")
	}
	if opts.Method == "" {
		opts.Method = http.MethodPost
	}
	return opts, nil
}

// modifyKind returns the bounded `kind` label value for the
// twiml_modify_total counter, derived from a parsed (or partially-parsed)
// modifyOpts. nil opts (parse failure before any field was inspected) maps to
// "twiml" — the default modify path — so the parse-error counter still
// records under a known kind label.
func modifyKind(opts *modifyOpts) string {
	if opts == nil {
		return "twiml"
	}
	if opts.Status != "" {
		return "status_completed"
	}
	if opts.URL != "" {
		return "url"
	}
	return "twiml"
}

// writeCallJSON re-fetches the call from the BridgeQuerier and writes the
// Twilio-shape Call JSON resource at the given HTTP status.
//
// Re-fetching matters: a Status=completed POST advances the call to the
// terminated state via session.Terminate("completed"), which races the
// StartSession defer chain that builds the recentlyTerminated snapshot. By
// the time we write the response, q.GetByCallSid may surface either the
// still-active CallSession (if the goroutine swap has not landed yet) or the
// terminatedCall snapshot — both satisfy bridge.BridgeCall and serialize
// correctly via SerializeCall. The Status() getter rolls up termReason →
// "completed" / "hangup" / etc., so the JSON status field reflects the final
// state regardless of which snapshot wins the race.
//
// 404 + 20404 on the re-fetch covers the "session vanished from both indices
// before the snapshot landed" edge case — extremely narrow but defensively
// correct.
func writeCallJSON(w http.ResponseWriter, q BridgeQuerier, callSid, accountSid string, status int) {
	call, ok := q.GetByCallSid(callSid)
	if !ok {
		ErrNotFound("CallSid " + callSid).WriteJSON(w)
		return
	}
	cj := SerializeCall(call, pathPrefix(accountSid))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(cj)
}

// modifyCallHandler returns the chi-compatible POST handler for
// /2010-04-01/Accounts/{AccountSid}/Calls/{CallSid}.json. Body parameters
// follow the Twilio modify-call REST contract.
//
// Algorithm:
//
//  1. Parse + validate body into modifyOpts (parseModifyOpts).
//     Returning 400+ApprError before resource lookup matches Twilio: a
//     malformed body is rejected regardless of whether the CallSid exists.
//  2. Validate {CallSid} shape via identity.CallSidRE → 404+20404 on miss.
//  3. q.GetByCallSid(callSid) → 404+20404 if neither active session nor
//     recently-terminated snapshot.
//  4. Status=completed branch: idempotent — terminated calls return 200
//     (Twilio behavior); active calls invoke session.Terminate("completed")
//     and return 200. Either way the response body is the refreshed
//     Call JSON.
//  5. Twiml= / Url= branches require an active session
//     (call.(*bridge.CallSession) cast + IsActive check). Inactive returns
//     400+21220.
//  6. Url= branch: webhook.FetchWithFallback(primary, fallback) → on success
//     parse TwiML; on failure return 400+11200.
//  7. Twiml= branch: parse TwiML directly. Parse failure returns 400+12100.
//  8. twiml.Dispatch(ctx, doc, midCallAdapter) walks verbs. Any error from a
//     verb handler (e.g. BYE failed) is logged but not surfaced as 5xx —
//     the call's logical state is already advancing; clients see the final
//     state via the refreshed Call JSON.
//  9. writeCallJSON re-fetches and emits 200 + Twilio JSON.
//
// Metrics: every exit path increments twiml_modify_total{kind, outcome}.
// Status counters (api_requests_total) come via metricsMiddleware.
func modifyCallHandler(q BridgeQuerier, manager *bridge.CallManager, accountSid string, wc webhookFetcher, actionPoster *http.Client, m *observability.Metrics, log zerolog.Logger, forwarder *sip.Forwarder, cfg config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		callSidPath := chi.URLParam(r, "CallSid")
		log.Info().
			Str(observability.FieldCallSid, callSidPath).
			Str("method", r.Method).
			Str("remote_addr", r.RemoteAddr).
			Msg("modifyCallHandler: REST modify-call request received")

		opts, perr := parseModifyOpts(r, m, log)
		if perr != nil {
			recordModifyOutcome(m, modifyKind(opts), "invalid_params")
			perr.WriteJSON(w)
			return
		}

		callSid := chi.URLParam(r, "CallSid")
		if !identity.CallSidRE.MatchString(callSid) {
			ErrNotFound("CallSid " + callSid).WriteJSON(w)
			return
		}
		call, ok := q.GetByCallSid(callSid)
		if !ok {
			ErrNotFound("CallSid " + callSid).WriteJSON(w)
			return
		}

		// StatusCallback subscription install.
		// When StatusCallback is non-empty AND the resolved call is an active
		// *bridge.CallSession, install the subscription via
		// SetStatusCallback. Independent of the Twiml/Url/Status branches —
		// a body with only StatusCallback IS a valid REST request. The
		// lifecycle and terminal emission paths read the stored config at
		// emission time. Subscription on a terminated/snapshot call is a
		// no-op — the cast to *bridge.CallSession fails for terminatedCall
		// snapshots.
		if opts.StatusCallback != "" {
			if liveSession, isLive := call.(*bridge.CallSession); isLive && liveSession.IsActive() {
				events := make(map[string]struct{}, len(opts.StatusCallbackEvent))
				for _, e := range opts.StatusCallbackEvent {
					events[e] = struct{}{}
				}
				// Default subscription if none specified: terminal events only.
				if len(events) == 0 {
					for _, e := range []string{"completed", "busy", "failed", "no-answer", "canceled"} {
						events[e] = struct{}{}
					}
				}
				liveSession.SetStatusCallback(&bridge.StatusCallbackConfig{
					URL:    opts.StatusCallback,
					Method: opts.StatusCallbackMethod,
					Events: events,
				})
				log.Info().
					Str(observability.FieldCallSid, callSid).
					Str("status_callback_url", opts.StatusCallback).
					Str("status_callback_method", opts.StatusCallbackMethod).
					Int("event_count", len(events)).
					Msg("modifyCallHandler: status-callback subscription installed")
			}
			// StatusCallback-only path (no Twiml/Url/Status): return 200 + JSON.
			if opts.Twiml == "" && opts.URL == "" && opts.Status == "" {
				recordModifyOutcome(m, "status_callback", "ok")
				writeCallJSON(w, q, callSid, accountSid, http.StatusOK)
				return
			}
		}

		// Status=completed: idempotent. If the call is already terminated
		// (recentlyTerminated snapshot OR active session whose IsActive
		// returned false), return 200 with the current state. This matches
		// Twilio's documented behavior: a duplicate Status=completed is a
		// no-op-with-200, NOT a 400+21220.
		if opts.Status == "completed" {
			session, isLive := call.(*bridge.CallSession)
			if !isLive || !session.IsActive() {
				recordModifyOutcome(m, "status_completed", "ok")
				writeCallJSON(w, q, callSid, accountSid, http.StatusOK)
				return
			}
			if err := session.Terminate("completed"); err != nil {
				log.Error().Err(err).Str(observability.FieldCallSid, callSid).Msg("Terminate failed — surfacing current state")
				// Continue: the call's logical state is already advancing
				// toward Terminated. Clients see the final state via the
				// refreshed Call JSON below.
			}
			recordModifyOutcome(m, "status_completed", "terminated")
			writeCallJSON(w, q, callSid, accountSid, http.StatusOK)
			return
		}

		// Twiml= or Url= require an active session. Cast must hit
		// *bridge.CallSession AND IsActive must be true; otherwise the call
		// is not eligible for in-place modification → Twilio 21220.
		session, isLive := call.(*bridge.CallSession)
		if !isLive || !session.IsActive() {
			recordModifyOutcome(m, modifyKind(opts), "invalid_params")
			ErrCallNotInProgress().WriteJSON(w)
			return
		}

		adapter := newMidCallAdapter(session, manager, forwarder, wc, actionPoster, cfg, log)

		var rawTwiml []byte
		if opts.Twiml != "" {
			rawTwiml = []byte(opts.Twiml)
		} else if opts.URL != "" {
			ctx, cancel := context.WithTimeout(r.Context(), urlFetchTimeout)
			defer cancel()
			primary := webhook.FetchTarget{URL: opts.URL, Method: opts.Method}
			fallback := webhook.FetchTarget{URL: opts.FallbackURL, Method: opts.FallbackMethod}
			if fallback.Method == "" {
				fallback.Method = http.MethodPost
			}
			result, ferr := wc.FetchWithFallback(ctx, primary, fallback)
			if ferr != nil {
				recordModifyOutcome(m, "url", "fetch_error")
				ErrHTTPRetrievalFailure(ferr.Error()).WriteJSON(w)
				return
			}
			rawTwiml = result.Body
		}

		doc, perr2 := twiml.Parse(rawTwiml)
		if perr2 != nil {
			recordModifyOutcome(m, modifyKind(opts), "parse_error")
			ErrTwimlParseFailure().WriteJSON(w)
			return
		}

		log.Info().
			Str(observability.FieldCallSid, callSid).
			Int("verbs", len(doc.Verbs)).
			Bool("async", asyncDispatchEnabled.Load()).
			Msg("modifyCallHandler: dispatching parsed TwiML")

		// Twilio behaviour: the modify-call POST returns immediately with the
		// call's CURRENT state. The submitted TwiML is dispatched in the
		// background; the eventual outcome (e.g. <Dial> answered/no-answer,
		// <Hangup> completed) is surfaced through status callbacks,
		// not the POST response. Sync dispatch would block the HTTP handler
		// for the entire dial duration — that is non-Twilio-compatible and
		// breaks any client with a sane request timeout.
		//
		// Tests flip asyncDispatchEnabled to false via SetAsyncDispatch so
		// test assertions on post-dispatch state are deterministic.
		//
		// The dispatch ctx is bound to the SESSION lifetime (not r.Context())
		// so the dial keeps running even after the curl client disconnects.
		// SessionContext is cancelled on call end (BYE, Terminate); the
		// goroutine exits cleanly via that path.
		dispatchCtx := session.SessionContext()
		if dispatchCtx == nil {
			dispatchCtx = context.Background() // defensive — pre-run() race
		}
		dispatch := func() {
			if err := twiml.Dispatch(dispatchCtx, doc, adapter); err != nil {
				log.Error().Err(err).Str(observability.FieldCallSid, callSid).Msg("twiml dispatch failed")
			}
		}
		if asyncDispatchEnabled.Load() {
			go dispatch()
			recordModifyOutcome(m, modifyKind(opts), "ok")
			writeCallJSON(w, q, callSid, accountSid, http.StatusOK)
			return
		}
		// Sync path — only reached in tests. Block until dispatch completes,
		// then read post-dispatch state from the response.
		dispatch()
		recordModifyOutcome(m, modifyKind(opts), "ok")
		writeCallJSON(w, q, callSid, accountSid, http.StatusOK)
	}
}

// asyncDispatchEnabled controls whether modifyCallHandler returns the HTTP
// response immediately (Twilio behaviour) or blocks until the TwiML
// dispatch completes (legacy/test behaviour). Production sets this true at
// init; the api package's test suite flips it via SetAsyncDispatch so
// post-dispatch assertions on the response body remain deterministic.
var asyncDispatchEnabled atomic.Bool

func init() { asyncDispatchEnabled.Store(true) }

// SetAsyncDispatch flips the global dispatch mode for modifyCallHandler.
// Production code never calls this — it is a test-only seam so the legacy
// "POST returns post-dispatch state" assertion pattern keeps working
// without a Twilio-incompatible production handler. Returns the previous
// value so callers can restore it via defer.
func SetAsyncDispatch(enabled bool) bool {
	return asyncDispatchEnabled.Swap(enabled)
}

// recordModifyOutcome increments the bounded twiml_modify_total counter when
// the metrics registry is non-nil. Test fixtures that mount the router
// without a metrics registry skip the increment cleanly.
//
// kind and outcome are bounded enums supplied by the modify-call handler:
//
//	kind    ∈ {twiml, url, status_completed}
//	outcome ∈ {ok, parse_error, fetch_error, invalid_params, terminated, hangup}
//
// — both are constant-string callers in modifyCallHandler; the walker can't
// trace the function-param provenance back to the call sites, so an explicit
// // metrics:dynamic-allowed opt-out gates the call.
func recordModifyOutcome(m *observability.Metrics, kind, outcome string) {
	if m == nil || m.TwimlModifyTotal == nil {
		return
	}
	// metrics:dynamic-allowed kind+outcome bounded by the call-site enum
	// (modifyCallHandler emits constant strings into both labels).
	m.TwimlModifyTotal.WithLabelValues(kind, outcome).Inc()
}

// listCallsHandler returns a paginated Twilio-shape envelope of active +
// recently-terminated calls.
//
// Query params:
//   - Page     : 0-indexed page number (Twilio convention). Defaults to 0.
//   - PageSize : items per page. Defaults to 50; clamped to [1, 1000] inside
//     SerializePage so a malicious 999_999 still fits in memory.
//
// Out-of-range Page values produce an empty calls slice but a present (200 +
// empty array) response — Twilio's contract is that pagination boundaries
// return an empty collection, never 404.
func listCallsHandler(q BridgeQuerier, accountSid string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page := parseIntDefault(r.URL.Query().Get("Page"), 0)
		pageSize := parseIntDefault(r.URL.Query().Get("PageSize"), 50)
		items := q.List()
		// bridge.BridgeCall has the same method set as the local CallView,
		// so we lift each element into a CallView slice for SerializePage.
		// The cost is one O(n) interface re-wrap per request; n is bounded by
		// active+recentlyTerminated count (max-rate × 5min).
		views := make([]CallView, len(items))
		for i, it := range items {
			views[i] = it
		}
		envelope := SerializePage(views, pathPrefix(accountSid), page, pageSize)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(envelope)
	}
}

// getCallHandler returns a single Twilio-shape Call resource.
//
// CallSid is shape-validated against ^CA[0-9a-f]{32}$; malformed Sids return
// 404 + Twilio error 20404 (NOT 400 "bad request"). This matches Twilio's
// REST behavior — a syntactically invalid Sid is indistinguishable from one
// that simply does not exist, and SDKs special-case 404 + 20404 as
// "resource not found".
func getCallHandler(q BridgeQuerier, accountSid string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		callSid := chi.URLParam(r, "CallSid")
		if !identity.CallSidRE.MatchString(callSid) {
			ErrNotFound("CallSid " + callSid).WriteJSON(w)
			return
		}
		call, ok := q.GetByCallSid(callSid)
		if !ok {
			ErrNotFound("CallSid " + callSid).WriteJSON(w)
			return
		}
		cj := SerializeCall(call, pathPrefix(accountSid))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(cj)
	}
}

// urlHostMasked extracts the host component of a customer-supplied URL for
// safe inclusion in operator log lines.
//
// Goal: surface enough operator-actionable information about an SSRF-rejected
// callback URL (so on-call can identify the misconfigured customer) WITHOUT
// leaking the full URL bytes (which may carry path/query secrets or full
// customer infrastructure topology).
//
// Behavior:
//   - Parseable URL → returns the host (with port, if present): "example.com",
//     "10.0.0.1:8443".
//   - Failing parse → "<unparseable>" sentinel.
//   - Empty input → "<empty>" sentinel.
//
// Phone numbers / URLs at debug-only is the default policy; this helper is
// the documented exception for SSRF-rejection WARN logs (security events).
// Marked at the call site with a `// metrics:url-allowed callback_host:`
// opt-out comment so the cardinality lint walker recognises it.
func urlHostMasked(rawURL string) string {
	if rawURL == "" {
		return "<empty>"
	}
	u, err := url.Parse(rawURL)
	if err != nil || u == nil || u.Host == "" {
		return "<unparseable>"
	}
	return u.Host
}

// parseIntDefault parses a query-param string into an int; returns def on
// empty input or parse error. Used for Page and PageSize so a malformed
// ?Page=abc silently falls back to the default rather than 400.
func parseIntDefault(s string, def int) int {
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}
