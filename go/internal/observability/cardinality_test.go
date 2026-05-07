package observability_test

// Cardinality bound check.
//
// Four interlocking tests pin the metric-cardinality acceptance contract:
//
//   1. TestCardinalityBound_AllVecsWithinAllowlist
//        Walks Registry.Gather() at runtime + asserts each *Vec's emitted
//        series count equals the size of the documented allowlist after
//        the allowlist is exercised end-to-end.
//   2. TestCardinalityBound_NoAllowlistMissing
//        Counts NewCounterVec / NewHistogramVec declarations in metrics.go
//        and asserts the test fixture has at least one entry per *Vec.
//        Catches "added a new *Vec, forgot to extend phase17Allowlists".
//   3. TestCardinalityBound_AllowlistMatchesComment
//        Parses every // metrics:allowlist comment in metrics.go and asserts
//        the parsed enum equals the fixture's allowed values byte-for-byte.
//        Catches "comment drifts from fixture, or vice-versa".
//   4. TestCardinalityBound_MaxSeriesPerCollector
//        After exercising the full allowlist for every collector, asserts
//        the per-collector series count is <= the documented upper bound.
//        Bounds reflect the reconciled 9-value
//        status_callback_attempts_total{event} vocab and the 72-series
//        APIRequestsTotal cartesian (route × method × status).

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/sipgate/sipgate-sip-stream-bridge/internal/observability"
)

// allowlistFixture mirrors a single // metrics:allowlist comment in
// metrics.go byte-for-byte. Drift in either direction (comment or fixture)
// surfaces as a Test 3 failure.
type allowlistFixture struct {
	// Vector is the *Metrics field name; used only in error messages.
	Vector string
	// PrometheusName is the registered metric family name as reported by
	// Registry.Gather(). Includes namespace prefix where applicable.
	PrometheusName string
	// CommentLabels are the label names declared on the // metrics:allowlist
	// line, in the same order they appear in the comment. Must match the
	// `[]string{...}` argument passed to NewCounterVec / NewHistogramVec.
	CommentLabels []string
	// Allowed lists every documented label-value combination. For a
	// single-label vector each inner slice has length 1; for a multi-label
	// vector each inner slice is one full row of label values.
	Allowed [][]string
	// Apply exercises one row through WithLabelValues so the resulting
	// series materialises in the registry.
	Apply func(m *observability.Metrics, row []string)
	// MaxSeries is the documented upper bound (Test 4).
	MaxSeries int
}

// cartesian returns every combination of one element from each input slice.
// Used to flatten APIRequestsTotal's 3-label cartesian (6 × 4 × 3 = 72) into
// the flat fixture format.
func cartesian(slices ...[]string) [][]string {
	if len(slices) == 0 {
		return [][]string{{}}
	}
	rest := cartesian(slices[1:]...)
	out := make([][]string, 0, len(slices[0])*len(rest))
	for _, head := range slices[0] {
		for _, tail := range rest {
			row := make([]string, 0, 1+len(tail))
			row = append(row, head)
			row = append(row, tail...)
			out = append(out, row)
		}
	}
	return out
}

// phase17Allowlists is the canonical mirror of every // metrics:allowlist
// comment in metrics.go.
var phase17Allowlists = []allowlistFixture{
	{
		Vector:         "TwimlParseErrorsTotal",
		PrometheusName: "twiml_parse_errors_total",
		CommentLabels:  []string{"code"},
		Allowed: [][]string{
			{"12100"}, {"13xxx"}, {"21218"}, {"21220"},
		},
		Apply: func(m *observability.Metrics, row []string) {
			m.TwimlParseErrorsTotal.WithLabelValues(row[0]).Inc()
		},
		MaxSeries: 4,
	},
	{
		Vector:         "APIRequestsTotal",
		PrometheusName: "api_requests_total",
		CommentLabels:  []string{"route", "method", "status"},
		Allowed: cartesian(
			[]string{"list_calls", "get_call", "modify_call", "health", "metrics", "unknown"},
			[]string{"GET", "POST", "PUT", "DELETE"},
			[]string{"2xx", "4xx", "5xx"},
		),
		Apply: func(m *observability.Metrics, row []string) {
			m.APIRequestsTotal.WithLabelValues(row[0], row[1], row[2]).Inc()
		},
		MaxSeries: 72,
	},
	{
		Vector:         "APIRequestDurationSeconds",
		PrometheusName: "api_request_duration_seconds",
		CommentLabels:  []string{"route"},
		Allowed: [][]string{
			{"list_calls"}, {"get_call"}, {"modify_call"},
			{"health"}, {"metrics"}, {"unknown"},
		},
		Apply: func(m *observability.Metrics, row []string) {
			m.APIRequestDurationSeconds.WithLabelValues(row[0]).Observe(0.01)
		},
		MaxSeries: 6,
	},
	{
		Vector:         "TwimlModifyTotal",
		PrometheusName: "twiml_modify_total",
		CommentLabels:  []string{"kind", "outcome"},
		Allowed: cartesian(
			[]string{"twiml", "url", "status_completed"},
			[]string{"ok", "parse_error", "fetch_error", "invalid_params", "terminated", "hangup"},
		),
		Apply: func(m *observability.Metrics, row []string) {
			m.TwimlModifyTotal.WithLabelValues(row[0], row[1]).Inc()
		},
		MaxSeries: 18, // 3 × 6
	},
	{
		Vector:         "ForwardFailedTotal",
		PrometheusName: "sipgate_bridge_forward_failed_total",
		CommentLabels:  []string{"reason"},
		Allowed: [][]string{
			{"busy"}, {"no_answer"}, {"rejected"}, {"unreachable"},
			{"codec_mismatch"}, {"toll_fraud"}, {"rate_limit"},
			{"caller_id_rejected"}, {"auth_failed"}, {"trunk_5xx"},
			{"timeout"}, {"error"},
		},
		Apply: func(m *observability.Metrics, row []string) {
			m.ForwardFailedTotal.WithLabelValues(row[0]).Inc()
		},
		MaxSeries: 12,
	},
	{
		Vector:         "ForwardDurationSeconds",
		PrometheusName: "sipgate_bridge_forward_duration_seconds",
		CommentLabels:  []string{"outcome"},
		// Documented allowlist values only.
		// TestForwardDurationSeconds_OutcomeWithinAllowlist_RawInputs is
		// the raw-input boundary check — that test drives the realistic
		// production raw status values (no-answer, canceled, failed, ...)
		// through observability.BucketOutcome and asserts allowlist
		// membership at /metrics scrape time. Together they form the
		// defense-in-depth check.
		Allowed: [][]string{
			{"answered"}, {"no_answer"}, {"busy"}, {"error"},
		},
		Apply: func(m *observability.Metrics, row []string) {
			m.ForwardDurationSeconds.WithLabelValues(row[0]).Observe(0.5)
		},
		MaxSeries: 4,
	},
	{
		Vector:         "AuthChallengeKind",
		PrometheusName: "sipgate_bridge_auth_challenge_kind_total",
		CommentLabels:  []string{"kind"},
		Allowed: [][]string{
			{"401"}, {"407"},
		},
		Apply: func(m *observability.Metrics, row []string) {
			m.AuthChallengeKind.WithLabelValues(row[0]).Inc()
		},
		MaxSeries: 2,
	},
	{
		Vector:         "StatusCallbackAttemptsTotal",
		PrometheusName: "sipgate_bridge_status_callback_attempts_total",
		CommentLabels:  []string{"event"},
		// 9-value Twilio event vocab — matches the metrics.go allowlist
		// comment exactly.
		Allowed: [][]string{
			{"initiated"}, {"ringing"}, {"answered"}, {"in-progress"},
			{"completed"}, {"busy"}, {"failed"}, {"no-answer"}, {"canceled"},
		},
		Apply: func(m *observability.Metrics, row []string) {
			m.StatusCallbackAttemptsTotal.WithLabelValues(row[0]).Inc()
		},
		MaxSeries: 9,
	},
	{
		Vector:         "StatusCallbackFailuresTotal",
		PrometheusName: "sipgate_bridge_status_callback_failures_total",
		CommentLabels:  []string{"reason"},
		Allowed: [][]string{
			{"timeout"}, {"4xx"}, {"5xx"}, {"connect_error"},
			{"exhausted_retries"}, {"ssrf_rejected"}, {"queue_full"},
		},
		Apply: func(m *observability.Metrics, row []string) {
			m.StatusCallbackFailuresTotal.WithLabelValues(row[0]).Inc()
		},
		MaxSeries: 7,
	},
}

// TestCardinalityBound_AllVecsWithinAllowlist exercises every documented
// allowlist row for every *Vec collector and asserts the resulting series
// count in Registry.Gather() output equals the size of the allowlist
// (no duplicate, no missing, no spurious series).
func TestCardinalityBound_AllVecsWithinAllowlist(t *testing.T) {
	m := observability.NewMetrics()
	for _, fx := range phase17Allowlists {
		for _, row := range fx.Allowed {
			fx.Apply(m, row)
		}
	}

	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("Registry.Gather: %v", err)
	}
	familyByName := make(map[string]int, len(families))
	for _, f := range families {
		familyByName[f.GetName()] = len(f.GetMetric())
	}

	for _, fx := range phase17Allowlists {
		want := len(fx.Allowed)
		got, ok := familyByName[fx.PrometheusName]
		if !ok {
			t.Errorf("vector %s: family %q not found in Registry.Gather() output (got families: %v)",
				fx.Vector, fx.PrometheusName, sortedKeys(familyByName))
			continue
		}
		if got != want {
			t.Errorf("vector %s (%s): expected %d series after exercising allowlist, got %d",
				fx.Vector, fx.PrometheusName, want, got)
		}
	}
}

// TestCardinalityBound_NoAllowlistMissing scans metrics.go for every
// NewCounterVec / NewHistogramVec declaration and asserts the test fixture
// has at least one entry per declaration. Catches "added a new *Vec to
// metrics.go but forgot to extend phase17Allowlists" — a regression that
// would silently bypass Test 1's series-count assertion.
func TestCardinalityBound_NoAllowlistMissing(t *testing.T) {
	src := readPhase17MetricsSource(t)
	counterVecCount := strings.Count(src, "prometheus.NewCounterVec(")
	histogramVecCount := strings.Count(src, "prometheus.NewHistogramVec(")
	totalVec := counterVecCount + histogramVecCount

	if len(phase17Allowlists) < totalVec {
		t.Errorf("phase17Allowlists missing entries: metrics.go has %d *Vec declarations "+
			"(%d NewCounterVec + %d NewHistogramVec), fixture has only %d entries. "+
			"Update phase17Allowlists when adding new *Vec collectors.",
			totalVec, counterVecCount, histogramVecCount, len(phase17Allowlists))
	}
}

// allowlistCommentRE captures the label-clauses of a `// metrics:allowlist`
// comment. Each clause is `<label>=<v1>|<v2>|...`, multiple clauses are
// space-separated. Group 1 captures the entire post-prefix payload.
var allowlistCommentRE = regexp.MustCompile(`(?m)^\s*//\s*metrics:allowlist\s+(.+)$`)

// parseAllowlistComment parses one comment's payload into a label→values map.
// Format: `<label>=<v1>|<v2>|... <label>=<v1>|<v2>|...`.
func parseAllowlistComment(payload string) map[string][]string {
	out := make(map[string][]string)
	for _, clause := range strings.Fields(payload) {
		eq := strings.Index(clause, "=")
		if eq <= 0 || eq == len(clause)-1 {
			continue
		}
		label := clause[:eq]
		out[label] = strings.Split(clause[eq+1:], "|")
	}
	return out
}

// TestCardinalityBound_AllowlistMatchesComment for every fixture entry,
// locates the matching `// metrics:allowlist` comment in metrics.go (matched
// by exact label-name set with the fixture's CommentLabels) and asserts the
// parsed label→values map equals the fixture's Allowed values byte-for-byte.
// This is the strongest interlock — comment is the source of truth, fixture
// mirrors it, any drift fails the test.
func TestCardinalityBound_AllowlistMatchesComment(t *testing.T) {
	src := readPhase17MetricsSource(t)
	matches := allowlistCommentRE.FindAllStringSubmatch(src, -1)
	parsedComments := make([]map[string][]string, 0, len(matches))
	for _, m := range matches {
		parsedComments = append(parsedComments, parseAllowlistComment(m[1]))
	}

	for _, fx := range phase17Allowlists {
		// Build the fixture's expected per-label vocab from Allowed rows.
		fixtureVocab := make(map[string]map[string]struct{}, len(fx.CommentLabels))
		for _, lbl := range fx.CommentLabels {
			fixtureVocab[lbl] = make(map[string]struct{})
		}
		for _, row := range fx.Allowed {
			if len(row) != len(fx.CommentLabels) {
				t.Fatalf("vector %s: row %v has %d values but CommentLabels has %d entries",
					fx.Vector, row, len(row), len(fx.CommentLabels))
			}
			for i, v := range row {
				fixtureVocab[fx.CommentLabels[i]][v] = struct{}{}
			}
		}

		// Find the matching comment by exact label-set match: a comment
		// declares EXACTLY the fixture's CommentLabels (no missing, no extra).
		var matched map[string][]string
		for _, parsed := range parsedComments {
			if len(parsed) != len(fx.CommentLabels) {
				continue
			}
			ok := true
			for _, lbl := range fx.CommentLabels {
				if _, has := parsed[lbl]; !has {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
			// Disambiguation: many fixtures share the same single-label
			// shape (e.g. "code", "kind", "reason", "outcome", "event").
			// Match on label values too — the comment's value set must equal
			// the fixture's value set for at least one label.
			disambiguates := true
			for _, lbl := range fx.CommentLabels {
				commented := stringSet(parsed[lbl])
				if !setsEqual(commented, fixtureVocab[lbl]) {
					disambiguates = false
					break
				}
			}
			if !disambiguates {
				continue
			}
			matched = parsed
			break
		}
		if matched == nil {
			t.Errorf("vector %s: no // metrics:allowlist comment in metrics.go matches labels %v "+
				"with fixture vocab %v",
				fx.Vector, fx.CommentLabels, sortedFixtureVocab(fixtureVocab))
			continue
		}

		// Compare commented vocab vs fixture vocab byte-for-byte per label.
		for _, lbl := range fx.CommentLabels {
			commented := stringSet(matched[lbl])
			fixture := fixtureVocab[lbl]
			if !setsEqual(commented, fixture) {
				t.Errorf("vector %s, label %q: comment vocab %v does not equal fixture vocab %v",
					fx.Vector, lbl, sortedSet(commented), sortedSet(fixture))
			}
		}
	}
}

// TestCardinalityBound_MaxSeriesPerCollector exercises every allowlist row
// (same as Test 1) and asserts the per-collector emitted-series count is
// less-than-or-equal to the documented upper bound. The bounds are
// deliberately tight — they reflect the reconciled state of the production
// code, not theoretical maxima.
//
// Bound provenance:
//   - TwimlParseErrorsTotal       <= 4   — code ∈ {12100, 13xxx, 21218, 21220}
//   - APIRequestsTotal            <= 72  — 6 routes × 4 methods × 3 status buckets
//   - APIRequestDurationSeconds   <= 6   — same route enum
//   - TwimlModifyTotal            <= 18  — 3 kinds × 6 outcomes
//   - ForwardFailedTotal          <= 12  — reason enum (incl. "error" fallback)
//   - ForwardDurationSeconds      <= 4   — outcome enum
//   - AuthChallengeKind           <= 2   — 401 | 407
//   - StatusCallbackAttemptsTotal <= 9   — 9-value Twilio event vocab
//   - StatusCallbackFailuresTotal <= 7   — reason enum (incl. queue_full)
func TestCardinalityBound_MaxSeriesPerCollector(t *testing.T) {
	m := observability.NewMetrics()
	for _, fx := range phase17Allowlists {
		for _, row := range fx.Allowed {
			fx.Apply(m, row)
		}
	}

	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("Registry.Gather: %v", err)
	}
	familyByName := make(map[string]int, len(families))
	for _, f := range families {
		familyByName[f.GetName()] = len(f.GetMetric())
	}

	for _, fx := range phase17Allowlists {
		got, ok := familyByName[fx.PrometheusName]
		if !ok {
			t.Errorf("vector %s: family %q not found in Registry.Gather()", fx.Vector, fx.PrometheusName)
			continue
		}
		if got > fx.MaxSeries {
			t.Errorf("vector %s (%s): series count %d exceeds documented upper bound %d",
				fx.Vector, fx.PrometheusName, got, fx.MaxSeries)
		}
	}
}

// TestForwardDurationSeconds_OutcomeWithinAllowlist_RawInputs is the
// defense-in-depth check at the observability-package boundary. Drives
// realistic production raw status values through the canonical bucketer
// (observability.BucketOutcome) and asserts the histogram only sees the
// documented allowlist. Catches the same regression class the sip-package
// TestForwarder_Cardinality_OutcomeAlwaysBucketed test catches, but at
// the observability-package boundary — so a future subsystem-internal
// regression is visible here without depending on sip-package coverage.
func TestForwardDurationSeconds_OutcomeWithinAllowlist_RawInputs(t *testing.T) {
	m := observability.NewMetrics()
	rawInputs := []string{
		// The 4 raw values that production sites historically passed
		// directly to the histogram instead of bucketing.
		"no-answer", "canceled", "failed", "busy",
		// Allowlist member.
		"answered",
		// Future-proofing: hypothetical producers / drift scenarios.
		// Each must bucket into "error", not leak as a raw label.
		"completed", "rejected", "unreachable", "timeout",
		// Empty input (the 2 pre-emit failure callers in forwarder.go).
		"",
	}
	for _, raw := range rawInputs {
		// Production chokepoint contract: any caller observing
		// forward_duration_seconds.WithLabelValues MUST run BucketOutcome
		// first. recordFailure enforces this; this test documents the
		// contract at the observability-package boundary.
		bucketed := observability.BucketOutcome(raw)
		m.ForwardDurationSeconds.WithLabelValues(bucketed).Observe(0.5)
	}

	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	allowlist := map[string]bool{
		"answered":  true,
		"no_answer": true,
		"busy":      true,
		"error":     true,
	}
	var seen []string
	for _, fam := range families {
		if fam.GetName() != "sipgate_bridge_forward_duration_seconds" {
			continue
		}
		for _, metric := range fam.GetMetric() {
			for _, lbl := range metric.GetLabel() {
				if lbl.GetName() == "outcome" {
					seen = append(seen, lbl.GetValue())
					if !allowlist[lbl.GetValue()] {
						t.Errorf("forward_duration_seconds emitted non-allowlist outcome=%q", lbl.GetValue())
					}
				}
			}
		}
	}
	if len(seen) == 0 {
		t.Fatalf("no outcome series observed; fixture did not exercise the histogram")
	}
}

// readPhase17MetricsSource reads metrics.go from the package directory.
// Mirrors readMetricsSource in metrics_test.go but redeclared with a unique
// name to avoid duplicate-symbol conflicts within the package.
func readPhase17MetricsSource(t *testing.T) string {
	t.Helper()
	bytes, err := os.ReadFile(filepath.Clean("metrics.go"))
	if err != nil {
		t.Fatalf("readPhase17MetricsSource: %v", err)
	}
	return string(bytes)
}

// stringSet builds a set from a slice of strings.
func stringSet(in []string) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for _, s := range in {
		out[s] = struct{}{}
	}
	return out
}

// setsEqual reports whether two string-sets contain the same elements.
func setsEqual(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// sortedSet returns the keys of a string-set in sorted order. Used in error
// messages for deterministic output.
func sortedSet(s map[string]struct{}) []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedKeys returns the keys of a map[string]int in sorted order.
func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedFixtureVocab returns a deterministic stringification of a fixture
// vocab map (label → set of values) for error messages.
func sortedFixtureVocab(v map[string]map[string]struct{}) map[string][]string {
	out := make(map[string][]string, len(v))
	for k, set := range v {
		out[k] = sortedSet(set)
	}
	return out
}

