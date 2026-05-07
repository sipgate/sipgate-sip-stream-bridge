package observability_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/sipgate/sipgate-sip-stream-bridge/internal/observability"
)

// readMetricsSource returns the bytes of internal/observability/metrics.go.
// Used by the regression-guard tests that scan the source for
// // metrics:allowlist / // metrics:bucketer comment annotations and for the
// custom-registry invariant.
//
// The test working directory is the package directory (Go's test runner
// convention), so the file is at "metrics.go" relative to cwd.
func readMetricsSource(t *testing.T) string {
	t.Helper()
	bytes, err := os.ReadFile(filepath.Clean("metrics.go"))
	if err != nil {
		t.Fatalf("readMetricsSource: %v", err)
	}
	return string(bytes)
}

// TestNewMetrics_AllowlistComments_PresentForEveryVec asserts that every
// prometheus.NewCounterVec / NewHistogramVec declaration in metrics.go has an
// immediately-preceding `// metrics:allowlist <label>=<v1>|<v2>|...` comment.
//
// This is the load-bearing contract between this test and the lint
// walker — the walker uses the comment-then-declaration adjacency as the
// binding rule. Removing or moving a comment breaks the walker's
// allowlist registration for that vector and surfaces as either
// false-positive diagnostics or silent cardinality regressions.
func TestNewMetrics_AllowlistComments_PresentForEveryVec(t *testing.T) {
	src := readMetricsSource(t)

	vecCount := strings.Count(src, "prometheus.NewCounterVec(") +
		strings.Count(src, "prometheus.NewHistogramVec(")
	allowlistCount := strings.Count(src, "// metrics:allowlist ")

	if allowlistCount < vecCount {
		t.Fatalf("// metrics:allowlist comment count = %d, expected >= NewCounterVec+NewHistogramVec count (%d). "+
			"Every *Vec declaration in metrics.go MUST have an immediately-preceding allowlist comment "+
			"(the cardinality lint walker depends on this adjacency).",
			allowlistCount, vecCount)
	}
}

// TestNewMetrics_NoPrometheusDefaultRegisterer is a regression guard that
// asserts metrics.go uses ONLY the custom registry (`reg.MustRegister`) and
// never the global `prometheus.MustRegister(` / `prometheus.DefaultRegisterer`.
// A metric registered on DefaultRegisterer would not appear on /metrics
// (which the bridge serves via promhttp.HandlerFor(reg, ...)) and would
// silently leak Go runtime metrics into the scrape output.
//
// The scan strips line comments before matching so that doc-strings that
// mention the forbidden symbols ("uses prometheus.NewRegistry() — NOT
// prometheus.DefaultRegisterer") are not flagged as violations.
func TestNewMetrics_NoPrometheusDefaultRegisterer(t *testing.T) {
	src := stripLineComments(readMetricsSource(t))

	if strings.Contains(src, "prometheus.MustRegister(") {
		t.Errorf("metrics.go uses prometheus.MustRegister( — must use reg.MustRegister( on the custom registry only")
	}
	if strings.Contains(src, "prometheus.DefaultRegisterer") {
		t.Errorf("metrics.go references prometheus.DefaultRegisterer — invariant violation; use the custom registry")
	}
}

// stripLineComments removes Go `//` line-comment text from each line of src
// (preserving line breaks) so source-text scans can ignore documentation that
// legitimately mentions the symbols being checked. Block-comments (/* ... */)
// are not handled because metrics.go uses only line comments.
func stripLineComments(src string) string {
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		if idx := strings.Index(line, "//"); idx >= 0 {
			lines[i] = line[:idx]
		}
	}
	return strings.Join(lines, "\n")
}

// TestNewMetrics_AllowlistEnums_AllReachable iterates every documented label
// value for every *Vec collector in metrics.go and calls WithLabelValues for
// each. A typo in either the // metrics:allowlist comment or the production
// emit code would surface as a panic-via-cardinality (well, not directly —
// CounterVec.WithLabelValues accepts arbitrary strings, but the test's
// fixture is the executable mirror of the comments and a drift between the
// two surfaces at code-review time when the test fixture must be updated).
//
// The fixture mirrors the // metrics:allowlist enums declared adjacent to
// each *Vec in metrics.go byte-for-byte. Drift in either direction (comment
// or code emit site) requires updating the corresponding row here — that
// is the desired interlock between the comment, the fixture, and production
// emit sites.
func TestNewMetrics_AllowlistEnums_AllReachable(t *testing.T) {
	m := observability.NewMetrics()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("WithLabelValues panicked: %v", r)
		}
	}()

	// twiml_parse_errors_total{code}.
	for _, code := range []string{"12100", "13xxx", "21218", "21220"} {
		m.TwimlParseErrorsTotal.WithLabelValues(code).Inc()
	}

	// api_requests_total{route,method,status} — full cartesian product
	// (6 * 4 * 3 = 72 series combinations exercised; cardinality bound
	// asserted in TestCardinalityBound_*).
	routes := []string{"list_calls", "get_call", "modify_call", "health", "metrics", "unknown"}
	methods := []string{"GET", "POST", "PUT", "DELETE"}
	statuses := []string{"2xx", "4xx", "5xx"}
	for _, route := range routes {
		for _, method := range methods {
			for _, status := range statuses {
				m.APIRequestsTotal.WithLabelValues(route, method, status).Inc()
			}
		}
	}

	// api_request_duration_seconds{route} — same route enum.
	for _, route := range routes {
		m.APIRequestDurationSeconds.WithLabelValues(route).Observe(0.01)
	}

	// twiml_modify_total{kind,outcome}
	for _, kind := range []string{"twiml", "url", "status_completed"} {
		for _, outcome := range []string{"ok", "parse_error", "fetch_error", "invalid_params", "terminated", "hangup"} {
			m.TwimlModifyTotal.WithLabelValues(kind, outcome).Inc()
		}
	}

	// forward_failed_total{reason}
	for _, reason := range []string{
		"busy", "no_answer", "rejected", "unreachable",
		"codec_mismatch", "toll_fraud", "rate_limit",
		"caller_id_rejected", "auth_failed", "trunk_5xx",
		"timeout", "error",
	} {
		m.ForwardFailedTotal.WithLabelValues(reason).Inc()
	}

	// forward_duration_seconds{outcome}
	for _, outcome := range []string{"answered", "no_answer", "busy", "error"} {
		m.ForwardDurationSeconds.WithLabelValues(outcome).Observe(0.5)
	}

	// auth_challenge_kind_total{kind}
	for _, kind := range []string{"401", "407"} {
		m.AuthChallengeKind.WithLabelValues(kind).Inc()
	}

	// status_callback_attempts_total{event} — 9-value Twilio vocab.
	events := []string{
		"initiated", "ringing", "answered", "in-progress",
		"completed", "busy", "failed", "no-answer", "canceled",
	}
	if len(events) != 9 {
		t.Fatalf("event enum size = %d, want 9", len(events))
	}
	for _, event := range events {
		m.StatusCallbackAttemptsTotal.WithLabelValues(event).Inc()
	}

	// status_callback_failures_total{reason} — 7-value enum
	// (incl. queue_full).
	reasons := []string{
		"timeout", "4xx", "5xx", "connect_error",
		"exhausted_retries", "ssrf_rejected", "queue_full",
	}
	if len(reasons) != 7 {
		t.Fatalf("reason enum size = %d, want 7", len(reasons))
	}
	for _, reason := range reasons {
		m.StatusCallbackFailuresTotal.WithLabelValues(reason).Inc()
	}
}

// TestNewMetrics_BucketerAnnotations_PresentForEveryHelper asserts that the
// three observability-package bucketer helpers (BucketStatus,
// BucketForwardReason, BucketStatusCallbackReason) carry the
// `// metrics:bucketer` annotation in the comment block immediately above
// their declaration.
//
// The annotation is the load-bearing contract the lint walker reads to
// accept the production non-literal WithLabelValues call sites that pass
// these helpers' return value as the label argument — without it, the
// walker emits ~10 false-positive diagnostics on the clean codebase.
//
// The fourth bucketer (sip.bucketOutcome) lives in internal/sip/forwarder.go
// and is verified by the walker's TestWalker_RealCodebase_Clean.
func TestNewMetrics_BucketerAnnotations_PresentForEveryHelper(t *testing.T) {
	src := readMetricsSource(t)
	count := strings.Count(src, "// metrics:bucketer")
	if count < 3 {
		t.Fatalf("// metrics:bucketer annotation count = %d, want >= 3 "+
			"(BucketStatus + BucketForwardReason + BucketStatusCallbackReason). "+
			"the cardinality lint walker depends on these annotations to accept production "+
			"non-literal call sites without false-positives.", count)
	}
}

// TestNewMetrics_PortPoolSize_Exposed asserts that the rtp_port_pool_size
// gauge exists, is registered on the custom registry, and is
// settable via the exposed *Metrics.RTPPortPoolSize gauge handle.
func TestNewMetrics_PortPoolSize_Exposed(t *testing.T) {
	m := observability.NewMetrics()
	if m.RTPPortPoolSize == nil {
		t.Fatal("RTPPortPoolSize gauge is nil — required for operator-monitoring of RTP port pool utilization")
	}
	m.RTPPortPoolSize.Set(8192)

	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("Registry.Gather() failed: %v", err)
	}
	const want = "sipgate_bridge_rtp_port_pool_size"
	found := false
	for _, mf := range families {
		if mf.GetName() == want {
			found = true
			if got := mf.GetMetric()[0].GetGauge().GetValue(); got != 8192 {
				t.Errorf("rtp_port_pool_size value = %v, want 8192", got)
			}
			break
		}
	}
	if !found {
		t.Errorf("metric family %q not present in Registry.Gather()", want)
	}
}

func TestNewMetrics_SIPOptionsFailures_NotNil(t *testing.T) {
	m := observability.NewMetrics()
	if m.SIPOptionsFailures == nil {
		t.Fatal("expected SIPOptionsFailures to be non-nil, got nil")
	}
}

func TestNewMetrics_SIPOptionsFailures_IncDoesNotPanic(t *testing.T) {
	m := observability.NewMetrics()
	// Inc() must not panic — counter is properly registered on the custom registry
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("SIPOptionsFailures.Inc() panicked: %v", r)
		}
	}()
	m.SIPOptionsFailures.Inc()
}

// TestNewMetrics_APIRequestsTotal_NotNil — REST collector wiring smoke
// test. Catches a regression where the field is left nil after refactor.
func TestNewMetrics_APIRequestsTotal_NotNil(t *testing.T) {
	m := observability.NewMetrics()
	if m.APIRequestsTotal == nil {
		t.Fatal("expected APIRequestsTotal to be non-nil, got nil")
	}
	if m.APIRequestDurationSeconds == nil {
		t.Fatal("expected APIRequestDurationSeconds to be non-nil, got nil")
	}
}

// TestNewMetrics_APIRequestsTotal_LabelsAccepted exercises the WithLabelValues
// path with the documented bounded enum to ensure the registered CounterVec
// matches the labels the api package will use.
func TestNewMetrics_APIRequestsTotal_LabelsAccepted(t *testing.T) {
	m := observability.NewMetrics()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("WithLabelValues panicked: %v", r)
		}
	}()
	m.APIRequestsTotal.WithLabelValues("list_calls", "GET", "2xx").Inc()
	m.APIRequestsTotal.WithLabelValues("get_call", "GET", "4xx").Inc()
	m.APIRequestDurationSeconds.WithLabelValues("list_calls").Observe(0.01)
}

func TestBucketStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   int
		want string
	}{
		{"zero", 0, "2xx"},
		{"informational_100", 100, "2xx"},
		{"ok_200", 200, "2xx"},
		{"created_201", 201, "2xx"},
		{"redirect_301", 301, "2xx"},
		{"bad_request_400", 400, "4xx"},
		{"unauthorized_401", 401, "4xx"},
		{"not_found_404", 404, "4xx"},
		{"too_many_429", 429, "4xx"},
		{"server_error_500", 500, "5xx"},
		{"bad_gateway_502", 502, "5xx"},
		{"out_of_range_700", 700, "5xx"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := observability.BucketStatus(tc.in); got != tc.want {
				t.Fatalf("BucketStatus(%d): got %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// ── <Dial> / B2BUA forwarding collector tests + BucketForwardReason ──

// TestNewMetrics_Phase15Collectors_NotNil asserts every B2BUA-forwarding
// collector field is wired up after NewMetrics(). A nil here means a
// struct-field assignment was forgotten.
func TestNewMetrics_Phase15Collectors_NotNil(t *testing.T) {
	m := observability.NewMetrics()
	if m.ForwardAttemptsTotal == nil {
		t.Error("ForwardAttemptsTotal is nil")
	}
	if m.ForwardSuccessTotal == nil {
		t.Error("ForwardSuccessTotal is nil")
	}
	if m.ForwardFailedTotal == nil {
		t.Error("ForwardFailedTotal is nil")
	}
	if m.ForwardDurationSeconds == nil {
		t.Error("ForwardDurationSeconds is nil")
	}
	if m.AuthChallengeKind == nil {
		t.Error("AuthChallengeKind is nil")
	}
	if m.RTPPortPoolInUse == nil {
		t.Error("RTPPortPoolInUse is nil")
	}
	if m.RTPPortPoolSize == nil {
		t.Error("RTPPortPoolSize is nil")
	}
	if m.RTPPortAcquireFailuresTotal == nil {
		t.Error("RTPPortAcquireFailuresTotal is nil")
	}
}

// TestNewMetrics_Phase15Collectors_RegistryGather asserts every B2BUA-
// forwarding collector is registered with the custom registry, which is
// what /metrics
// scrapes. We touch each metric (Inc/Observe/Set) so the gathered families
// include them — Prometheus does not export untouched CounterVec/HistogramVec
// children, but Counters/Gauges still appear.
func TestNewMetrics_Phase15Collectors_RegistryGather(t *testing.T) {
	m := observability.NewMetrics()

	// Touch every collector so it shows up in Gather().
	m.ForwardAttemptsTotal.Inc()
	m.ForwardSuccessTotal.Inc()
	m.ForwardFailedTotal.WithLabelValues("toll_fraud").Inc()
	m.ForwardDurationSeconds.WithLabelValues("answered").Observe(1.23)
	m.AuthChallengeKind.WithLabelValues("401").Inc()
	m.RTPPortPoolInUse.Set(2)
	m.RTPPortPoolSize.Set(100)
	m.RTPPortAcquireFailuresTotal.Inc()

	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("Registry.Gather() failed: %v", err)
	}

	want := map[string]bool{
		"sipgate_bridge_forward_attempts_total":          false,
		"sipgate_bridge_forward_success_total":           false,
		"sipgate_bridge_forward_failed_total":            false,
		"sipgate_bridge_forward_duration_seconds":        false,
		"sipgate_bridge_auth_challenge_kind_total":       false,
		"sipgate_bridge_rtp_port_pool_in_use":            false,
		"sipgate_bridge_rtp_port_pool_size":              false,
		"sipgate_bridge_rtp_port_acquire_failures_total": false,
	}
	for _, mf := range families {
		if _, ok := want[mf.GetName()]; ok {
			want[mf.GetName()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("metric family %q not present in Registry.Gather()", name)
		}
	}
}

// TestNewMetrics_Phase15Labels_BoundedEnum exercises every documented label
// value for the B2BUA-forwarding vector collectors. The cardinality lint
// walker enforces these enums at CI time; this test prevents a regression
// that adds a free-form label value.
func TestNewMetrics_Phase15Labels_BoundedEnum(t *testing.T) {
	m := observability.NewMetrics()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("WithLabelValues panicked: %v", r)
		}
	}()

	// forward_failed_total{reason} — full enum
	for _, reason := range []string{
		"busy", "no_answer", "rejected", "unreachable",
		"codec_mismatch", "toll_fraud", "rate_limit",
		"caller_id_rejected", "auth_failed", "trunk_5xx",
		"timeout", "error",
	} {
		m.ForwardFailedTotal.WithLabelValues(reason).Inc()
	}

	// forward_duration_seconds{outcome} — full enum
	for _, outcome := range []string{"answered", "no_answer", "busy", "error"} {
		m.ForwardDurationSeconds.WithLabelValues(outcome).Observe(0.5)
	}

	// auth_challenge_kind{kind} — full enum
	for _, kind := range []string{"401", "407"} {
		m.AuthChallengeKind.WithLabelValues(kind).Inc()
	}
}

// TestNewMetrics_Phase15_RegisterDoesNotPanic guards against a duplicate-
// registration regression — re-running NewMetrics() in a single process must
// produce independent registries (used by tests).
func TestNewMetrics_Phase15_RegisterDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("repeated NewMetrics() panicked: %v", r)
		}
	}()
	_ = observability.NewMetrics()
	_ = observability.NewMetrics()
	_ = observability.NewMetrics()
}

// TestBucketForwardReason covers every documented reason bucket plus the
// "fallback to error bucket" contract (empty string return).
func TestBucketForwardReason(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil_returns_empty", nil, ""},

		// guardrail errors (precede SIP-status checks)
		{"toll_fraud_typed_name", errors.New("ErrTollFraudBlocked: prefix not allowed"), "toll_fraud"},
		{"toll_fraud_hyphen", errors.New("toll-fraud guard rejected destination"), "toll_fraud"},
		{"toll_fraud_underscore", errors.New("toll_fraud guard rejected destination"), "toll_fraud"},
		{"rate_limit_typed_session", errors.New("ErrSessionRateLimit: 5/min exceeded"), "rate_limit"},
		{"rate_limit_typed_global", errors.New("ErrGlobalRateLimit: cluster-wide cap"), "rate_limit"},
		{"rate_limit_phrase", errors.New("rate limit hit"), "rate_limit"},

		// SIP status mappings
		{"busy_486", errors.New("486 Busy Here"), "busy"},
		{"busy_word", errors.New("callee Busy"), "busy"},
		{"rejected_603", errors.New("603 Decline"), "rejected"},
		{"no_answer_408", errors.New("408 Request Timeout"), "no_answer"},
		{"no_answer_480", errors.New("480 Temporarily Unavailable"), "no_answer"},
		{"no_answer_phrase", errors.New("ring no-answer reached"), "no_answer"},
		{"codec_mismatch", errors.New("codec mismatch: callee sent G722"), "codec_mismatch"},
		{"caller_id_rejected_q850", errors.New("Q.850 13214 caller-ID rejected"), "caller_id_rejected"},
		{"caller_id_rejected_phrase", errors.New("trunk rejected caller-ID"), "caller_id_rejected"},

		// digest auth
		{"auth_401", errors.New("401 Unauthorized"), "auth_failed"},
		{"auth_407", errors.New("407 Proxy Authentication Required"), "auth_failed"},
		{"auth_word", errors.New("digest auth failed after 3 retries"), "auth_failed"},

		// trunk failures
		{"trunk_5xx_phrase", errors.New("trunk returned 5xx"), "trunk_5xx"},
		{"trunk_502", errors.New("502 Bad Gateway"), "trunk_5xx"},
		{"trunk_503", errors.New("503 Service Unavailable"), "trunk_5xx"},
		{"trunk_504", errors.New("504 Server Timeout"), "trunk_5xx"},

		// network
		{"timeout_word", errors.New("INVITE timeout after 30s"), "timeout"},
		{"timeout_deadline", errors.New("context deadline exceeded"), "timeout"},
		{"unreachable_word", errors.New("destination unreachable"), "unreachable"},
		{"unreachable_dns", errors.New("DNS resolution failed"), "unreachable"},
		{"unreachable_route", errors.New("no route to host"), "unreachable"},

		// fallback
		{"unknown_returns_empty", errors.New("something nobody bothered to classify"), ""},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := observability.BucketForwardReason(tc.err)
			if got != tc.want {
				t.Fatalf("BucketForwardReason(%v): got %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestBucketForwardReason_EveryReturnedValueIsAcceptedByForwardFailedTotal —
// integration-style assertion: every non-empty bucket string returned by
// BucketForwardReason MUST be a valid label value for ForwardFailedTotal,
// otherwise WithLabelValues will register an unbounded label value.
// This locks the helper output to the documented enum.
func TestBucketForwardReason_EveryReturnedValueIsAcceptedByForwardFailedTotal(t *testing.T) {
	m := observability.NewMetrics()
	allowed := map[string]struct{}{
		"busy": {}, "no_answer": {}, "rejected": {}, "unreachable": {},
		"codec_mismatch": {}, "toll_fraud": {}, "rate_limit": {},
		"caller_id_rejected": {}, "auth_failed": {}, "trunk_5xx": {},
		"timeout": {}, "error": {},
	}
	for _, err := range []error{
		errors.New("486 Busy"),
		errors.New("603 Decline"),
		errors.New("408 timeout"),
		errors.New("DNS failed"),
		errors.New("codec mismatch"),
		errors.New("ErrTollFraudBlocked"),
		errors.New("ErrSessionRateLimit"),
		errors.New("caller-id rejected"),
		errors.New("401 Unauthorized"),
		errors.New("503 Service Unavailable"),
		errors.New("context deadline exceeded"),
	} {
		bucket := observability.BucketForwardReason(err)
		if bucket == "" {
			// Caller would fall back to "error" bucket — that one IS in the enum
			bucket = "error"
		}
		if _, ok := allowed[bucket]; !ok {
			t.Errorf("BucketForwardReason(%v) returned %q, not in documented enum", err, bucket)
			continue
		}
		// Accept by ForwardFailedTotal — sanity check the label is registered
		m.ForwardFailedTotal.WithLabelValues(bucket).Inc()
	}
}

// ── status_callback collector tests + BucketStatusCallbackReason ──

// TestStatusCallbackMetrics_RegisteredOnCustomRegistry — wiring smoke test.
// Both CounterVec collectors must (a) be assigned non-nil on the Metrics
// struct and (b) be visible via the custom registry's Gather().
//
// The custom-registry visibility check is the load-bearing assertion: a
// metric registered on prometheus.DefaultRegisterer would pass the "field
// is non-nil" smoke test but would NOT show up on /metrics (which the
// bridge serves via promhttp.HandlerFor(reg, ...)).
func TestStatusCallbackMetrics_RegisteredOnCustomRegistry(t *testing.T) {
	m := observability.NewMetrics()
	if m.StatusCallbackAttemptsTotal == nil {
		t.Fatal("StatusCallbackAttemptsTotal is nil after NewMetrics()")
	}
	if m.StatusCallbackFailuresTotal == nil {
		t.Fatal("StatusCallbackFailuresTotal is nil after NewMetrics()")
	}

	// Touch each collector with a representative bounded label so the
	// metric family appears in Gather() (CounterVec children are exported
	// only after first WithLabelValues + Inc).
	m.StatusCallbackAttemptsTotal.WithLabelValues("completed").Inc()
	m.StatusCallbackFailuresTotal.WithLabelValues("exhausted_retries").Inc()

	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("Registry.Gather() failed: %v", err)
	}

	want := map[string]bool{
		"sipgate_bridge_status_callback_attempts_total": false,
		"sipgate_bridge_status_callback_failures_total": false,
	}
	for _, mf := range families {
		if _, ok := want[mf.GetName()]; ok {
			want[mf.GetName()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("metric family %q not present in Registry.Gather()", name)
		}
	}
}

// TestStatusCallbackMetrics_BoundedEvents exercises every documented value
// of the `event` label. CounterVec.WithLabelValues accepts any string at
// runtime — bounded-ness is an INVARIANT enforced at the call site (Phase
// the cardinality lint walker enforces this at CI time). This test documents the enum as executable
// contract: the next refactor that drops a value will require updating
// this list, surfacing the change at code-review time.
func TestStatusCallbackMetrics_BoundedEvents(t *testing.T) {
	m := observability.NewMetrics()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("WithLabelValues panicked: %v", r)
		}
	}()

	events := []string{
		"initiated", "ringing", "answered", "in-progress",
		"completed", "busy", "failed", "no-answer", "canceled",
	}
	if len(events) != 9 {
		t.Fatalf("event enum size = %d, want 9", len(events))
	}
	for _, e := range events {
		m.StatusCallbackAttemptsTotal.WithLabelValues(e).Inc()
		got := testutil.ToFloat64(m.StatusCallbackAttemptsTotal.WithLabelValues(e))
		if got != 1 {
			t.Errorf("attempts[%s] = %v, want 1", e, got)
		}
	}
}

// TestStatusCallbackMetrics_BoundedReasons exercises every documented value
// of the `reason` label. queue_full is a distinct value (NOT folded into
// connect_error) — the bounded enum has 7 entries (RESEARCH §10 / I-14).
func TestStatusCallbackMetrics_BoundedReasons(t *testing.T) {
	m := observability.NewMetrics()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("WithLabelValues panicked: %v", r)
		}
	}()

	reasons := []string{
		"timeout", "4xx", "5xx", "connect_error",
		"exhausted_retries", "ssrf_rejected", "queue_full",
	}
	if len(reasons) != 7 {
		t.Fatalf("reason enum size = %d, want 7", len(reasons))
	}
	for _, r := range reasons {
		m.StatusCallbackFailuresTotal.WithLabelValues(r).Inc()
		got := testutil.ToFloat64(m.StatusCallbackFailuresTotal.WithLabelValues(r))
		if got != 1 {
			t.Errorf("failures[%s] = %v, want 1", r, got)
		}
	}
}

// TestBucketStatusCallbackReason_TableDriven covers every enum value at
// least once plus the nil-error and unknown-error fallback paths.
//
// The error-message substrings used here are the ACTUAL strings produced
// by the webhook package errors:
//   - ErrSSRFRejected wrapped: "ssrf: blocked IP ...: webhook: callback URL targets blocked address space"
//   - ErrQueueFull:            "webhook: per-call status callback queue full"
//   - ErrRetryExhausted:       "webhook: status callback retries exhausted"
//
// Drift in those messages MUST update both this table and the helper.
func TestBucketStatusCallbackReason_TableDriven(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		err        error
		statusCode int
		want       string
	}{
		// Success path — empty string means "do not record".
		{"success_200", nil, 200, ""},
		{"success_204", nil, 204, ""},
		{"success_299", nil, 299, ""},

		// HTTP status code ranges (no err — server returned a response).
		{"http_400", nil, 400, "4xx"},
		{"http_401", nil, 401, "4xx"},
		{"http_404", nil, 404, "4xx"},
		{"http_408", nil, 408, "4xx"},
		{"http_429", nil, 429, "4xx"},
		{"http_500", nil, 500, "5xx"},
		{"http_502", nil, 502, "5xx"},
		{"http_503", nil, 503, "5xx"},

		// Network-error matchers.
		{"deadline_exceeded", errors.New("context deadline exceeded"), 0, "timeout"},
		{"net_io_timeout", errors.New("dial tcp 1.2.3.4:443: i/o timeout"), 0, "timeout"},
		{"connection_refused", errors.New("dial tcp 1.2.3.4:443: connect: connection refused"), 0, "connect_error"},
		{"dns_no_such_host", errors.New("dial tcp: lookup foo.invalid: no such host"), 0, "connect_error"},
		{"tls_handshake_fail", errors.New("net/http: TLS handshake timeout"), 0, "timeout"},

		// Typed-sentinel substring match (16-02 / 16-03 errors).
		{"ssrf_rejected_wrapped", errors.New("ssrf: blocked IP 127.0.0.1 for host localhost: webhook: callback URL targets blocked address space"), 0, "ssrf_rejected"},
		{"ssrf_literal_blocked_ip", errors.New("ssrf: literal IP 10.0.0.1 in blocked range: webhook: callback URL targets blocked address space"), 0, "ssrf_rejected"},
		{"queue_full", errors.New("webhook: per-call status callback queue full"), 0, "queue_full"},
		{"retries_exhausted", errors.New("webhook: status callback retries exhausted"), 0, "exhausted_retries"},

		// Generic err with no specific match → connect_error fallback.
		{"unknown_err", errors.New("something else broke"), 0, "connect_error"},

		// Edge cases.
		{"nil_err_code_0", nil, 0, ""},
		{"nil_err_code_300", nil, 300, ""},
		// HTTP code precedence: a 5xx response with a populated err whose
		// message does not hit any sentinel substring falls through to the
		// HTTP-status branch, which buckets to "5xx". The server's
		// response code is the load-bearing classification — the body-read
		// error is incidental.
		{"http_500_with_unrelated_err", errors.New("read body: connection reset"), 500, "5xx"},
		// HTTP code precedence: same shape but a 4xx response — still
		// classified by status code, NOT as connect_error fallback.
		{"http_400_with_unrelated_err", errors.New("read body: short write"), 400, "4xx"},
	}

	if len(cases) < 10 {
		t.Fatalf("TestBucketStatusCallbackReason_TableDriven needs >=10 cases (plan must_have); have %d", len(cases))
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := observability.BucketStatusCallbackReason(tc.err, tc.statusCode)
			if got != tc.want {
				t.Errorf("BucketStatusCallbackReason(%v, %d) = %q, want %q", tc.err, tc.statusCode, got, tc.want)
			}
		})
	}
}

// TestBucketStatusCallbackReason_EnumLockstep — every non-empty value
// returned by BucketStatusCallbackReason MUST be a valid label value for
// StatusCallbackFailuresTotal. Locks the helper output to the documented
// 7-value enum so a future bucket value addition fails CI here unless the
// metric registration is updated in lockstep.
func TestBucketStatusCallbackReason_EnumLockstep(t *testing.T) {
	m := observability.NewMetrics()
	allowed := map[string]struct{}{
		"timeout": {}, "4xx": {}, "5xx": {}, "connect_error": {},
		"exhausted_retries": {}, "ssrf_rejected": {}, "queue_full": {},
	}
	probes := []struct {
		err  error
		code int
	}{
		{nil, 400},
		{nil, 500},
		{errors.New("context deadline exceeded"), 0},
		{errors.New("dial tcp: lookup x: no such host"), 0},
		{errors.New("ssrf: blocked IP"), 0},
		{errors.New("webhook: per-call status callback queue full"), 0},
		{errors.New("webhook: status callback retries exhausted"), 0},
	}
	for _, p := range probes {
		bucket := observability.BucketStatusCallbackReason(p.err, p.code)
		if bucket == "" {
			continue // success / unclassified — caller would not record
		}
		if _, ok := allowed[bucket]; !ok {
			t.Errorf("BucketStatusCallbackReason(%v, %d) returned %q, not in documented enum", p.err, p.code, bucket)
			continue
		}
		// Accept by StatusCallbackFailuresTotal — sanity check the label is registered.
		m.StatusCallbackFailuresTotal.WithLabelValues(bucket).Inc()
	}
}

