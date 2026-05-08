package webhook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/sipgate/sipgate-sip-stream-bridge/internal/observability"
)

// ErrQueueFull is returned by Enqueue when the per-CallSid queue is at depth
// perCallQueueDepth (64) and the worker has not yet drained a slot. The caller
// MUST treat this as a permanent loss of that single emission attempt — there
// is no retry semantic at the Enqueue layer (the at-least-once guarantee is
// per-emission, not per-call).
//
// ErrRetryExhausted is the typed-sentinel terminal classification when the
// worker abandons a job after attempt 4 fails. The metrics bucketer
// (observability.BucketStatusCallbackReason) errors.Is on this sentinel
// for the "exhausted_retries" reason label.
var (
	ErrQueueFull      = errors.New("webhook: per-call status callback queue full")
	ErrRetryExhausted = errors.New("webhook: status callback retries exhausted")
)

// perCallQueueDepth is the per-CallSid queue capacity. The acceptance
// criterion is "queue depth 64" — deeper queues are out of scope for this
// release.
const perCallQueueDepth = 64

// statusPerAttemptTimeout caps a single HTTP attempt at 4 seconds.
// http.Client.Timeout is the outer cap; internal context.WithTimeout enforces
// the same value defensively for transport callers that bypass http.Client.
const statusPerAttemptTimeout = 4 * time.Second

// StatusDrainBudget caps the per-CallSid DrainAndClose blocking time.
// Worst-case retry chain for a just-enqueued terminal event:
//
//	1+2+4 backoff + 4×4s per-attempt timeout ≈ 23s.
//
// 30s gives 7s headroom for clock skew + customer-host latency variance.
//
// Single source of truth for the drain budget. forwarder.go (sip-package
// dial-leg cleanup at recordFailure tail) and bridge/session.go (parent-
// leg cleanup at markTerminated) both reference this constant.
const StatusDrainBudget = 30 * time.Second

// statusBackoffs are the pre-delays between attempts 1→2, 2→3, 3→4.
// The first attempt has 0s pre-delay. Total = 4 attempts. Worst-case
// wall clock with full per-attempt timeouts ≈ 23s.
//
// Production has NO jitter — phase-brief decision: the per-call queue depth
// 64 + 3-retries-then-abandon caps thundering-herd risk. Adding production
// jitter is a deferred hardening if upstream operations report retry storms.
var statusBackoffs = [4]time.Duration{
	0,
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
}

// CallbackEvent is the fully-built emission payload, ready for transport.
// Built by the emit helpers in bridge/session.go and sip/handler.go.
// This struct is the contract between the emit-helper layer and the
// transport.
//
// URL: verbatim customer-supplied StatusCallback= bytes — signed verbatim
//      via SignWithContext to defeat req.URL.String() normalization.
//
// Method: "POST" or "GET". GET is rare; Twilio default and recommended is
//         POST. Defaulted to POST in Enqueue if empty.
//
// Form: canonical Twilio form fields — already canonical at *value*
//       level (e.g. CallStatus is the bare lower-case event name like
//       "completed"; Timestamp is RFC 1123Z). Sort+dedupe of multi-value
//       keys happens INSIDE Sign() at signingTransport.RoundTrip time.
//       DO NOT pre-sort or pre-dedupe at the emission site.
type CallbackEvent struct {
	URL    string
	Method string
	Form   url.Values
	// Event is the event-vocab label used for the per-event Prometheus
	// metric (status_callback_attempts_total{event=...}). Distinct from
	// the CallStatus value present in Form: event ∈ {initiated, ringing,
	// answered, in-progress, completed, busy, failed, no-answer, canceled}
	// while CallStatus uses Twilio's status vocabulary (queued, ringing,
	// in-progress, completed, ...). Keeping Event separate ensures every
	// documented metric label increments — a previous code path that
	// read Form.Get("CallStatus") left two of the nine documented metric
	// labels (initiated, answered) permanently at 0 because that field
	// never produces those values.
	//
	// Populated at the emit site (handler.go emitStatusEvent,
	// bridge/session.go emitTerminalStatusCallback,
	// sip/forwarder.go emitDialEvent). NEVER serialised into the outbound
	// HTTP body — Form.Get("CallStatus") remains the Twilio wire-format
	// CallStatus value for the customer.
	Event string

	// Trusted=true marks the URL as operator-supplied (e.g. via the
	// STATUS_CALLBACK_DEFAULT_URL env var) so Enqueue bypasses the
	// pre-flight ValidateCallbackURL and deliverOnce uses the
	// no-SSRF-guard transport. Customer-supplied URLs (REST POST
	// StatusCallback=) MUST leave this false. The producer-site
	// (handler.go onInvite + twiml/dispatch.go modifyCall) decides
	// based on whether the URL came from env or REST.
	Trusted bool
}

// callbackJob is the in-flight envelope on the per-call queue. The CallSid is
// duplicated from CallbackEvent.Form for fast structured logging without a
// map-lookup on every log line.
type callbackJob struct {
	callSid string
	evt     CallbackEvent
}

// StatusClient owns a private outbound transport distinct from the
// voiceWC fetcher and the <Dial>-action poster. This transport isolation
// is the load-bearing invariant: a flapping customer-supplied callback
// host MUST NOT eat connection-pool capacity from the voiceWC Url= fetch
// path.
//
// Lifecycle: Enqueue is non-blocking; returns ErrQueueFull at depth 64. The
// per-CallSid worker context is derived from context.Background() (NOT the
// SIP CallSession's session context) so call cleanup (BYE, port release,
// snapshot creation) proceeds immediately even when the customer's callback
// host is unreachable.
//
// AuthToken handling: the authToken field is the process-global
// cfg.AuthToken (single-tenant). Same value used as REST Basic Auth
// password and HMAC signing key. NEVER log it — the constructor field
// name is the only place authToken appears outside of the signing path.
// CI lints this via grep.
type StatusClient struct {
	// http is the SSRF-guarded transport for customer-supplied callback
	// URLs (REST POST StatusCallback=). Reject-on-dial for RFC1918 +
	// link-local + loopback.
	http *http.Client

	// httpTrusted is the no-SSRF-guard transport for operator-supplied
	// callback URLs (STATUS_CALLBACK_DEFAULT_URL env). Used only when
	// CallbackEvent.Trusted=true. Operators control deployment and may
	// legitimately point at 127.0.0.1 (sidecar pipelines, e2e harnesses).
	httpTrusted *http.Client

	authToken string
	metrics   *observability.Metrics
	log       zerolog.Logger

	// skipValidateURL is set true ONLY by newStatusClientWithTransport (the
	// test-only constructor). When true, Enqueue bypasses ValidateCallbackURL
	// so tests can target httptest.NewTLSServer URLs (which bind to
	// 127.0.0.1 — correctly blocked by the SSRF pre-flight in production).
	// Production NewStatusClient leaves this false; the pre-flight runs
	// unconditionally on every Enqueue. CallbackEvent.Trusted=true also
	// bypasses validation, but per-event rather than per-client.
	skipValidateURL bool

	// perCall holds *perCallState for each active CallSid. Created lazily on
	// first Enqueue, removed by DrainAndClose. sync.Map is correct here —
	// the per-CallSid keys are read-mostly in the steady state (one writer
	// at create, one reader at every Enqueue + every Drain). The leak
	// invariant depends on DrainAndClose removing the entry.
	perCall sync.Map // map[string]*perCallState
}

// perCallState holds the queue + worker control surface for one CallSid.
// done is closed by the worker on exit (used by DrainAndClose to wait).
// once guards the worker goroutine spawn so a racing pair of Enqueue calls
// for a freshly-created callSid spawn exactly one worker.
type perCallState struct {
	queue  chan *callbackJob
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// NewStatusClient constructs a StatusClient with the production transport
// tuning. The supplied authToken is the process-global cfg.AuthToken;
// metrics and log are wired through unchanged.
//
// Transport tuning rationale:
//   - DialContext = newSSRFGuard().DialContext  → resolve-once-validate-all
//     IP-literal dial (defeats DNS rebinding from the customer's
//     StatusCallback= host).
//   - TLSHandshakeTimeout 3s + ResponseHeaderTimeout 4s + Client.Timeout 4s
//     keeps a flapping customer host from monopolizing the per-attempt
//     budget.
//   - MaxIdleConnsPerHost 2 + MaxConnsPerHost 8 caps the FD footprint per
//     customer host — bounded blast radius.
//   - IdleConnTimeout 90s matches the Twilio status-callback delivery
//     cadence (which is bursty during call setup/teardown and quiet in
//     between).
//   - ForceAttemptHTTP2 = true matches the existing webhook.Client; HTTP/2
//     multiplexing helps when the customer host enables it.
func NewStatusClient(authToken string, m *observability.Metrics, log zerolog.Logger) *StatusClient {
	guard := newSSRFGuard()
	guarded := &http.Transport{
		DialContext:           guard.DialContext,
		TLSHandshakeTimeout:   3 * time.Second,
		ResponseHeaderTimeout: 4 * time.Second,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
		MaxConnsPerHost:       8,
	}
	// Trusted transport — same tuning sans the SSRF DialContext wrapper.
	// Used only for operator-supplied URLs (CallbackEvent.Trusted=true).
	trusted := &http.Transport{
		TLSHandshakeTimeout:   3 * time.Second,
		ResponseHeaderTimeout: 4 * time.Second,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
		MaxConnsPerHost:       8,
	}
	return &StatusClient{
		http: &http.Client{
			Transport: &signingTransport{inner: guarded, authToken: authToken},
			Timeout:   statusPerAttemptTimeout,
		},
		httpTrusted: &http.Client{
			Transport: &signingTransport{inner: trusted, authToken: authToken},
			Timeout:   statusPerAttemptTimeout,
		},
		authToken: authToken,
		metrics:   m,
		log:       log,
	}
}

// NewStatusClientForTest is the EXPORTED test-only constructor — same body
// as newStatusClientWithTransport but accessible from sibling test packages
// (e.g. internal/sip_test) so they can wire a StatusClient against an
// httptest.NewTLSServer Transport without re-implementing the surface.
//
// Production code MUST NOT call this — it bypasses the SSRF guard and the
// pre-flight ValidateCallbackURL via skipValidateURL: true. Used by
// internal/sip's Forwarder status-callback emission tests.
func NewStatusClientForTest(tr http.RoundTripper, authToken string, m *observability.Metrics, log zerolog.Logger) *StatusClient {
	return newStatusClientWithTransport(tr, authToken, m, log)
}

// newStatusClientWithTransport is a test-only constructor mirroring
// webhook.newClientWithTransport (client.go). Lets tests inject the TLS
// config of an httptest.NewTLSServer. The supplied tr is wrapped in
// signingTransport so the X-Twilio-Signature header is still set.
//
// Production code MUST NOT call this — it bypasses BOTH:
//
//  1. the SSRF guard at dial time (caller-supplied tr replaces ssrfGuard
//     — needed because httptest TLS servers bind to 127.0.0.1, which the
//     SSRF guard correctly blocks),
//
//  2. the pre-flight ValidateCallbackURL at Enqueue time via
//     skipValidateURL: true (same reason — the URL points at a 127.0.0.1
//     httptest server).
//
// Production-grade pre-flight + dial-time SSRF rejection is verified by
// TestStatusClient_RejectsSSRFAtEnqueue (which uses NewStatusClient
// directly, NOT this constructor).
func newStatusClientWithTransport(tr http.RoundTripper, authToken string, m *observability.Metrics, log zerolog.Logger) *StatusClient {
	signed := &signingTransport{inner: tr, authToken: authToken}
	httpClient := &http.Client{Transport: signed, Timeout: statusPerAttemptTimeout}
	return &StatusClient{
		http:            httpClient,
		httpTrusted:     httpClient, // tests share the single transport
		authToken:       authToken,
		metrics:         m,
		log:             log,
		skipValidateURL: true,
	}
}

// NewStatusClientWithTransport is the exported shim for cross-package tests
// (notably internal/bridge/session_terminate_test.go for the terminal-
// emission tests) that need to point a *StatusClient at an httptest.NewTLSServer
// without going through the production SSRF dial-time guard.
//
// Production code MUST NOT call this — same caveats as the unexported
// newStatusClientWithTransport: it bypasses BOTH the dial-time SSRF guard
// (caller-supplied tr replaces ssrfGuard.DialContext) AND the pre-flight
// ValidateCallbackURL at Enqueue (skipValidateURL=true). The 3-line shim
// exists solely so test packages outside webhook can target httptest TLS
// servers (which bind to 127.0.0.1, correctly blocked by the production
// SSRF surfaces).
func NewStatusClientWithTransport(tr http.RoundTripper, authToken string, m *observability.Metrics, log zerolog.Logger) *StatusClient {
	return newStatusClientWithTransport(tr, authToken, m, log)
}

// Enqueue queues a callback for delivery. Non-blocking — returns
// ErrQueueFull when the per-CallSid queue is at depth perCallQueueDepth.
// Starts the per-CallSid worker on first Enqueue for that CallSid (sync.Once
// guard).
//
// Pre-flight ValidateCallbackURL is invoked at Enqueue time so obviously-bad
// URLs (literal localhost, blocked-CIDR IP literals) never enter the queue.
// The REST modify-call handler also calls ValidateCallbackURL —
// defense in depth.
//
// Returns:
//   - nil on successful enqueue
//   - ErrEmptyURL if evt.URL == ""
//   - ErrSSRFRejected (wrapped) if ValidateCallbackURL rejects pre-flight
//   - ErrNonHTTPS if scheme is not https
//   - ErrQueueFull if the queue is full at depth perCallQueueDepth
//   - generic error if callSid == "" (programmer error)
func (c *StatusClient) Enqueue(callSid string, evt CallbackEvent) error {
	if callSid == "" {
		return errors.New("webhook: Enqueue requires non-empty callSid")
	}
	// Pre-flight URL validation. Bypassed in three cases:
	//   1. The test-only constructor (skipValidateURL=true) — httptest
	//      bind on 127.0.0.1 which the SSRF pre-flight correctly blocks.
	//   2. Operator-supplied URLs (evt.Trusted=true) — STATUS_CALLBACK_-
	//      DEFAULT_URL env var. Operators control deployment and may
	//      legitimately point at 127.0.0.1 (sidecar pipelines, harnesses).
	// Production NewStatusClient + customer-supplied URLs run validation
	// unconditionally.
	if !c.skipValidateURL && !evt.Trusted {
		if err := ValidateCallbackURL(evt.URL); err != nil {
			return err
		}
	} else if evt.URL == "" {
		return ErrEmptyURL
	}
	if evt.Method == "" {
		evt.Method = http.MethodPost
	}

	st := c.getOrCreateState(callSid)
	job := &callbackJob{callSid: callSid, evt: evt}

	select {
	case st.queue <- job:
		st.once.Do(func() { go c.runWorker(callSid, st) })
		return nil
	default:
		// Queue full — record metric and return. "queue_full" is in the
		// bounded enum from 16-07; the metric field is nil-guarded so this
		// path is safe before 16-07 wires the registration.
		if c.metrics != nil && c.metrics.StatusCallbackFailuresTotal != nil {
			c.metrics.StatusCallbackFailuresTotal.WithLabelValues("queue_full").Inc()
		}
		return ErrQueueFull
	}
}

// getOrCreateState atomically loads or creates the per-CallSid state. Uses
// sync.Map.LoadOrStore so a racing pair of Enqueue calls for a freshly-created
// callSid converge on a single state value. The losing goroutine discards its
// own context cancel via the cancel() call below.
func (c *StatusClient) getOrCreateState(callSid string) *perCallState {
	if v, ok := c.perCall.Load(callSid); ok {
		return v.(*perCallState)
	}
	_, cancel := context.WithCancel(context.Background())
	st := &perCallState{
		queue:  make(chan *callbackJob, perCallQueueDepth),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	actual, loaded := c.perCall.LoadOrStore(callSid, st)
	if loaded {
		cancel() // discard — another goroutine won the race
		return actual.(*perCallState)
	}
	return st
}

// runWorker is the per-CallSid drain loop. Single goroutine; serial job
// processing. Exits when queue is closed AND drained.
//
// Worker context derives from context.Background() — NOT from any session
// context — so call cleanup (BYE, port release, snapshot creation) proceeds
// immediately when the customer's callback host is unreachable. RESEARCH
// §5.3 / Pitfall 6.
func (c *StatusClient) runWorker(callSid string, st *perCallState) {
	defer close(st.done)
	for job := range st.queue {
		c.deliverWithRetries(callSid, job)
	}
}

// deliverWithRetries runs up to 4 attempts with pre-delays 0s/1s/2s/4s
// (RESEARCH §5.1). On exhaustion, increments
// status_callback_failures_total{reason="exhausted_retries"}.
//
// Per-attempt classification:
//   2xx                      → success; break
//   408, 429, 5xx            → retry-eligible
//   3xx, 4xx (excl. 408/429) → terminal; abandon
//   ErrSSRFRejected          → terminal; abandon (caller error)
//   network error / timeout  → retry-eligible
func (c *StatusClient) deliverWithRetries(callSid string, job *callbackJob) {
	var lastErr error
	var lastStatus int

	// EVENT-VOCAB label (initiated|ringing|answered|in-progress|completed|
	// busy|failed|no-answer|canceled). Populated at the emit site on the
	// CallbackEvent.Event field — distinct from Form.Get("CallStatus") which
	// carries Twilio's status-vocab values (queued|ringing|in-progress|
	// completed|...). A previous version read Form.Get("CallStatus")
	// here, which mis-attributed increments under the status-vocab values
	// — labels "initiated" and "answered" were permanently 0 because the
	// CallStatus form value never produces those names.
	eventLabel := job.evt.Event

	for attempt, delay := range statusBackoffs {
		if delay > 0 {
			time.Sleep(delay)
		}

		status, err := c.deliverOnce(job)
		lastErr = err
		lastStatus = status

		// Increment attempts metric per attempt (success or fail). nil-guarded
		// so this works before 16-07 wires StatusCallbackAttemptsTotal.
		if c.metrics != nil && c.metrics.StatusCallbackAttemptsTotal != nil && eventLabel != "" {
			// metrics:dynamic-allowed eventLabel is the bounded 9-value
			// vocab populated at emit-site (CallbackEvent.Event); the
			// cardinality lint walker cannot trace the struct-field-load
			// provenance back to the emit-site enum, so an explicit
			// opt-out is the cleanest gate.
			c.metrics.StatusCallbackAttemptsTotal.WithLabelValues(eventLabel).Inc()
		}

		// Success?
		if err == nil && status >= 200 && status < 300 {
			return
		}

		// Permanent failure — abandon without retry.
		if !c.shouldRetry(status, err) {
			c.recordFailure(status, err)
			c.log.Warn().
				Str(observability.FieldCallSid, callSid).
				Str("event", eventLabel).
				Int("status_code", status).
				Err(err).
				Int("attempt", attempt+1).
				Msg("status_callback: abandoning (non-retryable)")
			return
		}
		// Loop continues — next backoff
	}

	// Exhausted retries.
	if c.metrics != nil && c.metrics.StatusCallbackFailuresTotal != nil {
		c.metrics.StatusCallbackFailuresTotal.WithLabelValues("exhausted_retries").Inc()
	}
	c.log.Warn().
		Str(observability.FieldCallSid, callSid).
		Str("event", eventLabel).
		Int("last_status", lastStatus).
		Err(lastErr).
		Msg("status_callback: exhausted retries; abandoning")
}

// deliverOnce performs a single attempt. Returns (httpStatusCode, error).
// status==0 indicates a transport-level failure (no HTTP response received).
//
// Each attempt builds a fresh context.WithTimeout(context.Background(),
// statusPerAttemptTimeout) so the per-attempt budget is enforced even if
// http.Client.Timeout is somehow bypassed. The verbatim URL is stashed via
// SignWithContext so signingTransport signs THAT (not req.URL.String()).
// RESEARCH §1.6 / Pitfall 1.
func (c *StatusClient) deliverOnce(job *callbackJob) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statusPerAttemptTimeout)
	defer cancel()

	var bodyReader io.Reader
	encoded := ""
	if job.evt.Method == http.MethodPost {
		encoded = job.evt.Form.Encode()
		bodyReader = strings.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, job.evt.Method, job.evt.URL, bodyReader)
	if err != nil {
		return 0, fmt.Errorf("status_callback: build request: %w", err)
	}
	if job.evt.Method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.ContentLength = int64(len(encoded))
	}
	// Stash the verbatim URL so signingTransport signs THAT (not req.URL.String()).
	// This is the URL-fidelity guarantee from RESEARCH §1.6 / Pitfall 1.
	req = req.WithContext(SignWithContext(req.Context(), job.evt.URL))

	// Choose the SSRF-guarded transport for customer URLs, the unguarded
	// (operator-trusted) transport for env-supplied URLs.
	httpClient := c.http
	if job.evt.Trusted && c.httpTrusted != nil {
		httpClient = c.httpTrusted
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain body so the connection can be returned to the pool.
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// shouldRetry implements the policy from RESEARCH §5.1. Returns true if the
// (status, err) pair should trigger another retry attempt.
//
// Order matters: ErrSSRFRejected check first (caller error, never retry);
// then any err != nil (network/transport-level → retry); then HTTP status
// classification (408/429/5xx → retry; 3xx/other-4xx → terminal).
func (c *StatusClient) shouldRetry(status int, err error) bool {
	if errors.Is(err, ErrSSRFRejected) {
		return false
	}
	if err != nil {
		// Network-level error (timeout, connect refused, TLS fail). Retry.
		return true
	}
	// HTTP-level outcome.
	switch {
	case status == http.StatusRequestTimeout, status == http.StatusTooManyRequests:
		return true
	case status >= 500:
		return true
	default:
		// 2xx handled by caller; 3xx and other 4xx are non-retryable.
		return false
	}
}

// recordFailure increments status_callback_failures_total{reason} via the
// canonical observability.BucketStatusCallbackReason helper (single source
// of truth for the bounded reason enum). The helper returns "" for
// unclassified inputs (caller treats as "do not record"); for an
// errors.Is(err, ErrSSRFRejected) hit, the wrapped message contains
// "blocked address space" and the helper buckets to "ssrf_rejected".
//
// The classification was previously inlined here; it now lives in the
// observability package so the bounded-enum contract sits alongside the
// metric declaration.
func (c *StatusClient) recordFailure(status int, err error) {
	if c.metrics == nil || c.metrics.StatusCallbackFailuresTotal == nil {
		return
	}
	reason := observability.BucketStatusCallbackReason(err, status)
	if reason == "" {
		return // unclassified — do not increment
	}
	c.metrics.StatusCallbackFailuresTotal.WithLabelValues(reason).Inc()
}

// DrainAndClose closes the per-call queue and waits up to drainTimeout for
// the worker to finish processing remaining jobs (including retries already
// in flight). Returns nil on clean exit, or context.DeadlineExceeded on
// timeout. Always removes the per-call entry from the sync.Map. Idempotent
// — calling DrainAndClose for an unknown CallSid returns nil.
//
// Lifecycle invariant: every CallSession.markTerminated MUST call
// StatusClient.DrainAndClose to avoid a goroutine leak under churn. The
// SIP session finalizer wires this.
func (c *StatusClient) DrainAndClose(callSid string, drainTimeout time.Duration) error {
	v, ok := c.perCall.LoadAndDelete(callSid)
	if !ok {
		return nil
	}
	st := v.(*perCallState)
	// Close the queue so the worker's range loop exits after draining.
	close(st.queue)
	select {
	case <-st.done:
		return nil
	case <-time.After(drainTimeout):
		// Force-cancel — but worker uses its own per-attempt ctx, so this
		// mostly serves to record the timeout for callers. Worker will
		// still finish its current attempt eventually (bounded by
		// statusPerAttemptTimeout) and then exit when the channel drains.
		st.cancel()
		return context.DeadlineExceeded
	}
}
