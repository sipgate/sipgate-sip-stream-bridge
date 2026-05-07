package observability

import (
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds all Prometheus metrics for sipgate-sip-stream-bridge.
// Passed to CallManager and Registrar at construction; never registered globally.
// Metric names are stable contract; do not rename without an operator-visible CHANGELOG entry.
// NOTE: active_calls_total is a Gauge (goes up/down with sessions) — the _total suffix
// matches the operator-facing literal metric name; deviates from Prometheus Counter convention
// but is not enforced by the client library.
type Metrics struct {
	ActiveCalls  prometheus.Gauge   // active_calls_total
	SIPRegStatus prometheus.Gauge   // sip_registration_status (0/1)
	RTPRx        prometheus.Counter // rtp_packets_received_total
	RTPTx        prometheus.Counter // rtp_packets_sent_total
	WSReconnects prometheus.Counter // ws_reconnect_attempts_total
	MarkEchoed         prometheus.Counter // mark_echoed_total
	ClearReceived      prometheus.Counter // clear_received_total
	SIPOptionsFailures prometheus.Counter // sip_options_failures_total

	// TwimlParseErrorsTotal. Bounded label cardinality:
	//   code ∈ {12100, 13xxx, 21218, 21220}
	// Twilio TwiML error-code vocab — incremented by the API handler when a
	// fetched/parsed TwiML document fails validation. The four-value enum
	// covers Twilio's documented TwiML parse-error codes (12100 generic,
	// 13xxx verb-specific, 21218 invalid Url, 21220 invalid Method).
	TwimlParseErrorsTotal *prometheus.CounterVec // twiml_parse_errors_total{code}

	// REST API metrics. Bounded label cardinality:
	//   route  ∈ {list_calls, get_call, modify_call, health, metrics, unknown}
	//   method ∈ {GET, POST, PUT, DELETE}
	//   status ∈ {2xx, 4xx, 5xx}
	// Cardinality enforced at call site; CI gate lints the route enum.
	APIRequestsTotal          *prometheus.CounterVec   // api_requests_total{route,method,status}
	APIRequestDurationSeconds *prometheus.HistogramVec // api_request_duration_seconds{route}

	// TwiML modify-call metrics. Bounded label cardinality:
	//   kind    ∈ {twiml, url, status_completed}
	//   outcome ∈ {ok, parse_error, fetch_error, invalid_params, terminated, hangup}
	// Useful for debugging which modify-call paths customers exercise and the
	// distribution of success/error outcomes per kind.
	TwimlModifyTotal *prometheus.CounterVec // twiml_modify_total{kind,outcome}

	// ── <Dial> / B2BUA forwarding ──

	// ForwardAttemptsTotal: cumulative count of outbound <Dial> attempts that
	// PASSED guardrails and entered the SIP forwarder. Increments BEFORE the
	// outbound INVITE is sent. Pre-guardrail rejections (toll_fraud / rate_limit)
	// are NOT counted here — they hit ForwardFailedTotal directly.
	ForwardAttemptsTotal prometheus.Counter

	// ForwardSuccessTotal: outbound dials that reached "answered" state
	// (callee 200 OK + ACK + first media frame relayed).
	ForwardSuccessTotal prometheus.Counter

	// ForwardFailedTotal: outbound dial failures bucketed by reason. Bounded
	// enum — see BucketForwardReason for the canonical mapping. Reasons:
	// busy, no_answer, rejected, unreachable, codec_mismatch, toll_fraud,
	// rate_limit, caller_id_rejected, auth_failed, trunk_5xx, timeout, error.
	ForwardFailedTotal *prometheus.CounterVec // labels: reason

	// ForwardDurationSeconds: end-to-end time from <Dial> dispatch to terminal
	// state. Useful for latency dashboards. Outcome is bucketed:
	// answered | no_answer | busy | error.
	ForwardDurationSeconds *prometheus.HistogramVec // labels: outcome

	// AuthChallengeKind: sipgate trunk Digest challenge type observed on
	// outbound INVITE. 401 (UAS auth) vs 407 (proxy auth). Set via a metric
	// hook from sipgo's `WaitAnswer` digest handler. Labels: kind ∈ {401,407}.
	AuthChallengeKind *prometheus.CounterVec // labels: kind={401,407}

	// RTPPortPoolInUse: current count of allocated RTP ports. The <Dial>
	// path consumes 2 ports per call (caller leg + callee leg).
	RTPPortPoolInUse prometheus.Gauge

	// RTPPortPoolSize: total RTP ports configured (RTP_PORT_MAX - RTP_PORT_MIN + 1).
	// Static after startup; useful for utilization-ratio alerts.
	RTPPortPoolSize prometheus.Gauge

	// RTPPortAcquireFailuresTotal: count of pool-exhausted AcquirePort calls.
	RTPPortAcquireFailuresTotal prometheus.Counter

	// ── Status-callback delivery ──
	//
	// internal/webhook/status.go compiles against the stable struct shape
	// here. NewMetrics() provides the prometheus.NewCounterVec construction
	// + registry.MustRegister wiring AND the canonical
	// BucketStatusCallbackReason helper that recordFailure calls.
	//
	// StatusCallbackAttemptsTotal — bounded label cardinality:
	//   event ∈ {initiated, ringing, answered, in-progress, completed, busy,
	//            failed, no-answer, canceled}                          (9 values)
	StatusCallbackAttemptsTotal *prometheus.CounterVec // labels: event
	// StatusCallbackFailuresTotal — bounded label cardinality:
	//   reason ∈ {timeout, 4xx, 5xx, connect_error, exhausted_retries,
	//             ssrf_rejected, queue_full}                            (7 values)
	// Total label permutations: 9 + 7 = 16 distinct cardinalities — well
	// under any reasonable cardinality cap.
	StatusCallbackFailuresTotal *prometheus.CounterVec // labels: reason

	Registry *prometheus.Registry
}

// NewMetrics creates a custom registry and registers all required metrics.
// Uses prometheus.NewRegistry() (NOT prometheus.DefaultRegisterer) to exclude Go runtime metrics
// from the /metrics scrape output — only sipgate-sip-stream-bridge metrics are exposed (Research anti-pattern note).
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()

	activeCalls := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "active_calls_total",
		Help: "Number of currently active SIP call sessions.",
	})
	sipReg := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "sip_registration_status",
		Help: "SIP registration status: 1 = registered, 0 = unregistered or failed.",
	})
	rtpRx := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rtp_packets_received_total",
		Help: "Total RTP packets received from the SIP caller (PCMU PT=0 only).",
	})
	rtpTx := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rtp_packets_sent_total",
		Help: "Total RTP packets sent to the SIP caller.",
	})
	wsReconnects := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ws_reconnect_attempts_total",
		Help: "Total WebSocket reconnect attempts across all calls.",
	})
	markEchoed := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "mark_echoed_total",
		Help: "Total mark echo events sent to the WS server.",
	})
	clearReceived := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "clear_received_total",
		Help: "Total clear events received from the WS server.",
	})
	sipOptionsFailures := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "sip_options_failures_total",
		Help: "Total SIP OPTIONS keepalive failures (timeout, 5xx, 404).",
	})

	// metrics:allowlist code=12100|13xxx|21218|21220
	twimlParseErrorsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "twiml_parse_errors_total",
		Help: "Total TwiML document parse failures, bucketed by Twilio error-code (12100|13xxx|21218|21220).",
	}, []string{"code"})

	// metrics:allowlist route=list_calls|get_call|modify_call|health|metrics|unknown method=GET|POST|PUT|DELETE status=2xx|4xx|5xx
	apiRequestsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "api_requests_total",
		Help: "Total HTTP requests to the Twilio-compatible REST API, by route, method, and bucketed status.",
	}, []string{"route", "method", "status"})

	// metrics:allowlist route=list_calls|get_call|modify_call|health|metrics|unknown
	apiRequestDurationSeconds := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "api_request_duration_seconds",
		Help:    "Latency of Twilio-compatible REST API handlers, by route.",
		Buckets: prometheus.DefBuckets,
	}, []string{"route"})

	// metrics:allowlist kind=twiml|url|status_completed outcome=ok|parse_error|fetch_error|invalid_params|terminated|hangup
	twimlModifyTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "twiml_modify_total",
		Help: "Total TwiML modify-call handler invocations, bucketed by body kind and outcome.",
	}, []string{"kind", "outcome"})

	// ── <Dial> / B2BUA forwarding collectors ──
	forwardAttemptsTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "sipgate_bridge",
		Name:      "forward_attempts_total",
		Help:      "Outbound <Dial> attempts that passed guardrails and entered the SIP forwarder.",
	})
	forwardSuccessTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "sipgate_bridge",
		Name:      "forward_success_total",
		Help:      "Outbound dials that reached answered state.",
	})
	// metrics:allowlist reason=busy|no_answer|rejected|unreachable|codec_mismatch|toll_fraud|rate_limit|caller_id_rejected|auth_failed|trunk_5xx|timeout|error
	forwardFailedTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sipgate_bridge",
		Name:      "forward_failed_total",
		Help:      "Outbound dials that failed, bucketed by reason.",
	}, []string{"reason"})
	// metrics:allowlist outcome=answered|no_answer|busy|error
	forwardDurationSeconds := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "sipgate_bridge",
		Name:      "forward_duration_seconds",
		Help:      "End-to-end <Dial> duration in seconds, bucketed by outcome.",
		Buckets:   []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 300, 1800, 7200, 14400},
	}, []string{"outcome"})
	// metrics:allowlist kind=401|407
	authChallengeKind := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sipgate_bridge",
		Name:      "auth_challenge_kind_total",
		Help:      "Outbound-INVITE digest challenge type observed (401 UAS vs 407 proxy).",
	}, []string{"kind"})
	rtpPortPoolInUse := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "sipgate_bridge",
		Name:      "rtp_port_pool_in_use",
		Help:      "Current count of allocated RTP ports.",
	})
	rtpPortPoolSize := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "sipgate_bridge",
		Name:      "rtp_port_pool_size",
		Help:      "Total configured RTP ports (RTP_PORT_MAX - RTP_PORT_MIN + 1).",
	})
	rtpPortAcquireFailuresTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "sipgate_bridge",
		Name:      "rtp_port_acquire_failures_total",
		Help:      "Count of pool-exhausted AcquirePort calls.",
	})

	// ── Status callback delivery collectors ──
	// Bounded label cardinality enforced at call site; CI gate lints the
	// enum at /metrics scrape time. Both metrics use the "sipgate_bridge"
	// namespace to match the existing forward_* / rtp_port_* pattern (the
	// api_* / twiml_* metrics predate the namespace convention).
	// metrics:allowlist event=initiated|ringing|answered|in-progress|completed|busy|failed|no-answer|canceled
	statusCallbackAttemptsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sipgate_bridge",
		Name:      "status_callback_attempts_total",
		Help:      "Outbound Twilio-shape status callback POST attempts, bucketed by event vocabulary (initiated|ringing|answered|in-progress|completed|busy|failed|no-answer|canceled).",
	}, []string{"event"})
	// metrics:allowlist reason=timeout|4xx|5xx|connect_error|exhausted_retries|ssrf_rejected|queue_full
	statusCallbackFailuresTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sipgate_bridge",
		Name:      "status_callback_failures_total",
		Help:      "Outbound status callback failures, bucketed by reason (timeout|4xx|5xx|connect_error|exhausted_retries|ssrf_rejected|queue_full).",
	}, []string{"reason"})

	// Use reg.MustRegister (NOT prometheus.MustRegister) — custom registry only (Research Pitfall 4)
	reg.MustRegister(
		activeCalls, sipReg, rtpRx, rtpTx, wsReconnects, markEchoed, clearReceived, sipOptionsFailures,
		twimlParseErrorsTotal,
		apiRequestsTotal, apiRequestDurationSeconds,
		twimlModifyTotal,
		forwardAttemptsTotal, forwardSuccessTotal, forwardFailedTotal,
		forwardDurationSeconds, authChallengeKind,
		rtpPortPoolInUse, rtpPortPoolSize, rtpPortAcquireFailuresTotal,
		statusCallbackAttemptsTotal, statusCallbackFailuresTotal,
	)

	return &Metrics{
		ActiveCalls:                 activeCalls,
		SIPRegStatus:                sipReg,
		RTPRx:                       rtpRx,
		RTPTx:                       rtpTx,
		WSReconnects:                wsReconnects,
		MarkEchoed:                  markEchoed,
		ClearReceived:               clearReceived,
		SIPOptionsFailures:          sipOptionsFailures,
		TwimlParseErrorsTotal:       twimlParseErrorsTotal,
		APIRequestsTotal:            apiRequestsTotal,
		APIRequestDurationSeconds:   apiRequestDurationSeconds,
		TwimlModifyTotal:            twimlModifyTotal,
		ForwardAttemptsTotal:        forwardAttemptsTotal,
		ForwardSuccessTotal:         forwardSuccessTotal,
		ForwardFailedTotal:          forwardFailedTotal,
		ForwardDurationSeconds:      forwardDurationSeconds,
		AuthChallengeKind:           authChallengeKind,
		RTPPortPoolInUse:            rtpPortPoolInUse,
		RTPPortPoolSize:             rtpPortPoolSize,
		RTPPortAcquireFailuresTotal: rtpPortAcquireFailuresTotal,
		StatusCallbackAttemptsTotal: statusCallbackAttemptsTotal,
		StatusCallbackFailuresTotal: statusCallbackFailuresTotal,
		Registry:                    reg,
	}
}

// metrics:bucketer
//
// BucketStatus maps an HTTP status code to a bounded cardinality label
// ("2xx", "4xx", or "5xx") for use as a Prometheus label value. Keeping the
// label space tiny prevents cardinality blow-up from arbitrary client-driven
// codes.
//
// Mapping rules:
//   - 1xx informational and 2xx/3xx success/redirect → "2xx" (handlers in this
//     code base never emit 3xx; lumping 1xx with success keeps three buckets).
//   - 4xx client-side errors → "4xx".
//   - 5xx server-side errors AND any code ≥ 600 → "5xx".
//   - codes < 100 (e.g. zero from an unflushed ResponseWriter that never wrote
//     a header) default to "2xx" — net/http treats them as implicit 200.
func BucketStatus(code int) string {
	switch {
	case code < 100:
		return "2xx"
	case code < 400:
		return "2xx"
	case code < 500:
		return "4xx"
	default:
		return "5xx"
	}
}

// metrics:bucketer
//
// BucketForwardReason maps a forwarder error to the canonical reason string
// used in forward_failed_total{reason=...}. Returning "" indicates the error
// wasn't classified — callers fall back to the "error" bucket.
//
// The mapping table is the single source of truth — every forwarder
// failure-classification call site uses this instead of hard-coding
// strings.
//
// Implementation note: uses string matching on err.Error() rather than
// errors.Is/As against typed sentinel errors so this package does NOT have
// to import internal/sip (cycle-avoidance — internal/sip imports
// observability already). The forwarder's typed errors must therefore
// embed one of the recognised substrings in their .Error() output.
//
// Match order matters: more specific patterns precede generic ones. The
// "5xx" check runs before "5"/"50" digit fragments would risk false matches,
// the auth check (401/407) runs before any generic "4" digit match, and the
// guardrail substrings ("ErrTollFraudBlocked", "ErrSessionRateLimit",
// "ErrGlobalRateLimit") are matched ahead of generic "rate limit" wording.
//
// Returns "" for nil input — callers should treat nil error as "do not record
// a failure" (i.e. success path).
func BucketForwardReason(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case containsCI(msg, "ErrTollFraudBlocked"), containsCI(msg, "toll-fraud"), containsCI(msg, "toll_fraud"):
		return "toll_fraud"
	case containsCI(msg, "ErrSessionRateLimit"), containsCI(msg, "ErrGlobalRateLimit"), containsCI(msg, "rate limit"), containsCI(msg, "rate-limit"), containsCI(msg, "rate_limit"):
		return "rate_limit"
	case containsCI(msg, "13214"), containsCI(msg, "caller-id"), containsCI(msg, "caller_id"), containsCI(msg, "callerid"):
		return "caller_id_rejected"
	case containsCI(msg, "486"), containsCI(msg, "Busy"):
		return "busy"
	case containsCI(msg, "603"), containsCI(msg, "Decline"):
		return "rejected"
	case containsCI(msg, "408"), containsCI(msg, "480"), containsCI(msg, "no-answer"), containsCI(msg, "no answer"), containsCI(msg, "no_answer"):
		return "no_answer"
	case containsCI(msg, "codec"):
		return "codec_mismatch"
	case containsCI(msg, "401"), containsCI(msg, "407"), containsCI(msg, "auth"):
		return "auth_failed"
	case containsCI(msg, "5xx"), containsCI(msg, "500"), containsCI(msg, "502"), containsCI(msg, "503"), containsCI(msg, "504"):
		return "trunk_5xx"
	case containsCI(msg, "timeout"), containsCI(msg, "deadline"):
		return "timeout"
	case containsCI(msg, "unreachable"), containsCI(msg, "no route"), containsCI(msg, "DNS"):
		return "unreachable"
	default:
		return "" // caller falls back to "error" bucket
	}
}

// containsCI reports whether substr is present in s, case-insensitively.
func containsCI(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

// metrics:bucketer
//
// BucketOutcome maps a sip-layer DialResult.Status onto the bounded
// ForwardDurationSeconds outcome label. Mirrors the unexported
// sip/forwarder.go:bucketOutcome (which now delegates here). Bounded
// enum: answered | no_answer | busy | error.
//
// With the bucketer canonicalised here, both the sip-package chokepoint
// AND the observability boundary tests exercise the same 4-case mapping —
// no drift possible.
//
// Mapping rules:
//   - "answered"   → "answered"
//   - "no-answer"  → "no_answer"   (hyphen on input → underscore on label
//     to keep label values valid Prometheus identifiers)
//   - "busy"       → "busy"
//   - default (incl. "", "failed", "canceled", "completed", "rejected",
//     "unreachable", "timeout", and any future raw status string)
//     → "error"
func BucketOutcome(status string) string {
	switch status {
	case "answered":
		return "answered"
	case "no-answer":
		return "no_answer"
	case "busy":
		return "busy"
	default:
		return "error"
	}
}

// metrics:bucketer
//
// BucketStatusCallbackReason maps a status-callback delivery outcome to the
// canonical reason bucket used in status_callback_failures_total{reason}.
//
// Inputs:
//   - err:        the error returned by StatusClient.deliverOnce, or nil.
//   - statusCode: the HTTP status code returned by the customer's host, or 0
//     if no HTTP response was received (transport-level failure).
//
// Returns one of:
//
//	"timeout", "4xx", "5xx", "connect_error", "exhausted_retries",
//	"ssrf_rejected", "queue_full".
//
// Returns "" when the outcome is success (err == nil AND 200 ≤ statusCode <
// 300) OR when the input is genuinely unclassified — callers MUST treat ""
// as "do not record a failure metric".
//
// Match order matters: typed-sentinel substrings first (most specific), then
// HTTP status code ranges, then generic network-error patterns. Mirrors the
// BucketForwardReason idiom — string matching on err.Error() rather than
// errors.Is/As against typed sentinels so this package does NOT need to
// import internal/webhook (cycle: internal/webhook imports observability
// for the Metrics struct).
//
// The webhook package's typed errors must therefore embed one of the
// recognised substrings in their .Error() output:
//   - ErrSSRFRejected     → "callback URL targets blocked address space" (matched via "blocked address space" / "ssrf")
//   - ErrQueueFull        → "per-call status callback queue full" (matched via "queue full")
//   - ErrRetryExhausted   → "status callback retries exhausted" (matched via "retries exhausted")
//
// Any drift in those error messages MUST be reflected here — the
// TestBucketStatusCallbackReason_TableDriven suite is the regression gate.
func BucketStatusCallbackReason(err error, statusCode int) string {
	if err == nil && statusCode >= 200 && statusCode < 300 {
		return "" // success — do not record failure
	}

	// (1) Typed-sentinel substring match (no cycle on webhook package).
	if err != nil {
		msg := err.Error()
		switch {
		case containsCI(msg, "queue full"), containsCI(msg, "queue_full"):
			return "queue_full"
		case containsCI(msg, "retries exhausted"), containsCI(msg, "exhausted_retries"):
			return "exhausted_retries"
		case containsCI(msg, "ssrf"), containsCI(msg, "blocked address space"), containsCI(msg, "blocked ip"):
			return "ssrf_rejected"
		case containsCI(msg, "timeout"), containsCI(msg, "deadline exceeded"):
			return "timeout"
		case containsCI(msg, "connection refused"),
			containsCI(msg, "no such host"),
			containsCI(msg, "dns lookup"),
			containsCI(msg, "dial tcp"),
			containsCI(msg, "tls handshake"):
			return "connect_error"
		}
	}

	// (2) HTTP status code ranges.
	switch {
	case statusCode >= 500:
		return "5xx"
	case statusCode >= 400:
		return "4xx"
	}

	// (3) Generic err with no specific match → connect_error.
	if err != nil {
		return "connect_error"
	}
	return ""
}
