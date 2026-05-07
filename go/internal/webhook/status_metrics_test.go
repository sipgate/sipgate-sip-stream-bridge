package webhook

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"

	"github.com/sipgate/sipgate-sip-stream-bridge/internal/observability"
)

// TestStatusCallbackMetrics_IntegrationWithStatusClient drives a real
// StatusClient through both the happy path (server returns 200 → attempts++,
// no failures) and the exhausted-retries path (server returns 500 every
// attempt → 4 attempts, 1 exhausted_retries failure). The assertions read
// the metric counter values via prometheus/testutil.ToFloat64 — proving
// 16-03's call sites in deliverWithRetries (StatusCallbackAttemptsTotal.Inc)
// and the exhausted-retries codepath (StatusCallbackFailuresTotal.Inc) are
// wired to the labels declared on the custom registry by 16-07's NewMetrics().
//
// This test is the bookkeeping half of the must_have:
//
//	"After 16-07 lands, status_callback_attempts_total is incremented for
//	 every Enqueue success in StatusClient.deliverWithRetries (16-03) and
//	 status_callback_failures_total is incremented at the exhausted-retries
//	 site — verified via integration test that drives StatusClient + asserts
//	 metric values"
//
// Lives in package webhook (not observability_test) because the test-only
// constructor newStatusClientWithTransport is package-private — the SSRF
// guard would correctly reject httptest.NewTLSServer's 127.0.0.1 host
// otherwise. Mirrors testStatusClient in status_test.go.
func TestStatusCallbackMetrics_IntegrationWithStatusClient(t *testing.T) {
	// ── Happy path: server returns 200 immediately ──
	successSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer successSrv.Close()

	m := observability.NewMetrics()
	log := zerolog.Nop()
	sc := newStatusClientWithTransport(successSrv.Client().Transport, "12345", m, log)

	if err := sc.Enqueue("CAhappy", metricsSampleEvent(successSrv.URL+"/cb", "CAhappy", "completed")); err != nil {
		t.Fatalf("Enqueue success path: %v", err)
	}
	if err := sc.DrainAndClose("CAhappy", 5*time.Second); err != nil {
		t.Fatalf("DrainAndClose success path: %v", err)
	}

	if got := testutil.ToFloat64(m.StatusCallbackAttemptsTotal.WithLabelValues("completed")); got != 1 {
		t.Errorf("after happy path: attempts[completed] = %v, want 1", got)
	}
	for _, reason := range []string{"timeout", "4xx", "5xx", "connect_error", "exhausted_retries", "ssrf_rejected", "queue_full"} {
		if got := testutil.ToFloat64(m.StatusCallbackFailuresTotal.WithLabelValues(reason)); got != 0 {
			t.Errorf("after happy path: failures[%s] = %v, want 0", reason, got)
		}
	}

	// ── Failure path: server returns 500 every attempt ──
	// 16-03 deliverWithRetries does 4 attempts (pre-delays 0s/1s/2s/4s)
	// then increments StatusCallbackFailuresTotal{exhausted_retries} once.
	// Each attempt increments StatusCallbackAttemptsTotal{failed} once.
	failSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failSrv.Close()

	sc2 := newStatusClientWithTransport(failSrv.Client().Transport, "12345", m, log)
	if err := sc2.Enqueue("CAfail", metricsSampleEvent(failSrv.URL+"/cb", "CAfail", "failed")); err != nil {
		t.Fatalf("Enqueue failure path: %v", err)
	}
	// DrainAndClose timeout must exceed worst-case retry wall: pre-delays
	// 1s+2s+4s = 7s plus per-attempt budget 4s*4 = 16s ⇒ ~23s; pad to 30s
	// per RESEARCH §5.1.
	if err := sc2.DrainAndClose("CAfail", 30*time.Second); err != nil {
		t.Fatalf("DrainAndClose failure path: %v", err)
	}

	if got := testutil.ToFloat64(m.StatusCallbackAttemptsTotal.WithLabelValues("failed")); got != 4 {
		t.Errorf("after failure path: attempts[failed] = %v, want 4 (1 + 3 retries)", got)
	}
	if got := testutil.ToFloat64(m.StatusCallbackFailuresTotal.WithLabelValues("exhausted_retries")); got != 1 {
		t.Errorf("after failure path: failures[exhausted_retries] = %v, want 1", got)
	}
	// Per-attempt 5xx response is the retry trigger — recordFailure is NOT
	// called inside the retry loop (only on the abandon-non-retryable
	// branch), so failures[5xx] stays 0 even though every attempt saw a
	// 500. The exhausted_retries bucket is the canonical accounting for
	// retry exhaustion.
	if got := testutil.ToFloat64(m.StatusCallbackFailuresTotal.WithLabelValues("5xx")); got != 0 {
		t.Errorf("after failure path: failures[5xx] = %v, want 0 (retried-then-abandoned via exhausted_retries)", got)
	}

	// ── Per-attempt-non-retryable failure path: server returns 404 ──
	// 16-03 shouldRetry treats 404 as terminal → 1 attempt, recordFailure
	// runs, BucketStatusCallbackReason maps 404 → "4xx".
	notFoundSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer notFoundSrv.Close()

	sc3 := newStatusClientWithTransport(notFoundSrv.Client().Transport, "12345", m, log)
	if err := sc3.Enqueue("CA404", metricsSampleEvent(notFoundSrv.URL+"/cb", "CA404", "ringing")); err != nil {
		t.Fatalf("Enqueue 404 path: %v", err)
	}
	if err := sc3.DrainAndClose("CA404", 5*time.Second); err != nil {
		t.Fatalf("DrainAndClose 404 path: %v", err)
	}
	if got := testutil.ToFloat64(m.StatusCallbackAttemptsTotal.WithLabelValues("ringing")); got != 1 {
		t.Errorf("after 404 path: attempts[ringing] = %v, want 1 (single non-retryable attempt)", got)
	}
	if got := testutil.ToFloat64(m.StatusCallbackFailuresTotal.WithLabelValues("4xx")); got != 1 {
		t.Errorf("after 404 path: failures[4xx] = %v, want 1", got)
	}
}

// metricsSampleEvent builds a canonical Twilio-shape form for arbitrary
// CallSid + CallStatus values. Mirrors sampleEvent in status_test.go but
// parameterized on event so the same helper drives multiple paths.
//
// Event field is set equal to callStatus here because every callStatus
// value used by the existing metric tests ("completed", "failed",
// "ringing") is also a valid event-vocab value.
// TestStatusClient_AttemptsMetric_UsesEventVocab below uses divergent
// values explicitly to prove the field is the metric label source.
func metricsSampleEvent(callbackURL, callSid, callStatus string) CallbackEvent {
	form := url.Values{}
	form.Set("CallSid", callSid)
	form.Set("AccountSid", "ACtest0123456789abcdef0123456789ab")
	form.Set("From", "+4915123456789")
	form.Set("To", "+4930111222333")
	form.Set("Caller", "+4915123456789")
	form.Set("Called", "+4930111222333")
	form.Set("Direction", "inbound")
	form.Set("ApiVersion", "2010-04-01")
	form.Set("CallStatus", callStatus)
	form.Set("Timestamp", time.Now().UTC().Format(time.RFC1123Z))
	form.Set("SequenceNumber", "0")
	form.Set("CallbackSource", "call-progress-events")
	return CallbackEvent{URL: callbackURL, Method: http.MethodPost, Form: form, Event: callStatus}
}

// divergentSampleEvent builds a CallbackEvent whose Event (event-vocab)
// and Form["CallStatus"] (status-vocab) values DIFFER — the canonical
// regression vector for the metric-label source. A previous code path
// read Form.Get("CallStatus") at the metric increment site, so
// increments landed on the status-vocab label. The current code reads
// CallbackEvent.Event so the label is the event-vocab string regardless
// of what CallStatus says.
func divergentSampleEvent(callbackURL, callSid, eventLabel, callStatus string) CallbackEvent {
	form := url.Values{}
	form.Set("CallSid", callSid)
	form.Set("AccountSid", "ACtest0123456789abcdef0123456789ab")
	form.Set("From", "+4915123456789")
	form.Set("To", "+4930111222333")
	form.Set("Caller", "+4915123456789")
	form.Set("Called", "+4930111222333")
	form.Set("Direction", "inbound")
	form.Set("ApiVersion", "2010-04-01")
	form.Set("CallStatus", callStatus)
	form.Set("Timestamp", time.Now().UTC().Format(time.RFC1123Z))
	form.Set("SequenceNumber", "0")
	form.Set("CallbackSource", "call-progress-events")
	return CallbackEvent{URL: callbackURL, Method: http.MethodPost, Form: form, Event: eventLabel}
}

// TestStatusClient_AttemptsMetric_UsesEventVocab asserts that
// status_callback_attempts_total{event=...} is keyed on the event-vocab
// label carried in CallbackEvent.Event, NOT on the status-vocab value
// in Form["CallStatus"]. The two vocabularies diverge at exactly two
// emit sites:
//
//   - "initiated" event ↔ CallStatus="queued"
//   - "answered"  event ↔ CallStatus="in-progress"
//
// A previous version that read Form.Get("CallStatus") made the labels
// "initiated" and "answered" structurally unreachable — increments
// landed on "queued" and "in-progress" instead. Now the labels are
// populated correctly AND the divergent CallStatus values DO NOT appear
// as metric labels.
//
// The in-test negative assertions (event="queued" == 0 and
// event="in-progress" == 0) provide an auto-runnable proof that the
// increments structurally cannot leak into the status-vocab labels.
func TestStatusClient_AttemptsMetric_UsesEventVocab(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := observability.NewMetrics()
	log := zerolog.Nop()
	sc := NewStatusClientForTest(srv.Client().Transport, "test_token", m, log)

	// "initiated" event with CallStatus="queued" — the canonical
	// divergence vector. Pre-fix landed on event="queued"; post-fix
	// MUST land on event="initiated".
	if err := sc.Enqueue("CAinit", divergentSampleEvent(srv.URL+"/cb", "CAinit", "initiated", "queued")); err != nil {
		t.Fatalf("Enqueue initiated path: %v", err)
	}
	if err := sc.DrainAndClose("CAinit", 5*time.Second); err != nil {
		t.Fatalf("DrainAndClose initiated path: %v", err)
	}

	// "answered" event with CallStatus="in-progress" — second canonical
	// divergence vector. Pre-fix landed on event="in-progress"; post-fix
	// MUST land on event="answered".
	sc2 := NewStatusClientForTest(srv.Client().Transport, "test_token", m, log)
	if err := sc2.Enqueue("CAansw", divergentSampleEvent(srv.URL+"/cb", "CAansw", "answered", "in-progress")); err != nil {
		t.Fatalf("Enqueue answered path: %v", err)
	}
	if err := sc2.DrainAndClose("CAansw", 5*time.Second); err != nil {
		t.Fatalf("DrainAndClose answered path: %v", err)
	}

	// Positive assertions — the event-vocab labels MUST increment.
	if got := testutil.ToFloat64(m.StatusCallbackAttemptsTotal.WithLabelValues("initiated")); got != 1 {
		t.Errorf("attempts[event=\"initiated\"] = %v, want 1 (event-vocab labels must increment)", got)
	}
	if got := testutil.ToFloat64(m.StatusCallbackAttemptsTotal.WithLabelValues("answered")); got != 1 {
		t.Errorf("attempts[event=\"answered\"] = %v, want 1 (event-vocab labels must increment)", got)
	}

	// Negative assertions — the divergent CallStatus values MUST NOT
	// appear as metric labels. Under the pre-fix code path these read
	// 1; the structural fix makes them unreachable from the increment
	// site. This is the in-test equivalent of the manual "revert and
	// rerun" confidence check.
	if got := testutil.ToFloat64(m.StatusCallbackAttemptsTotal.WithLabelValues("queued")); got != 0 {
		t.Errorf("attempts[event=\"queued\"] = %v, want 0 (CallStatus form value MUST NOT be the metric label)", got)
	}
	if got := testutil.ToFloat64(m.StatusCallbackAttemptsTotal.WithLabelValues("in-progress")); got != 0 {
		t.Errorf("attempts[event=\"in-progress\"] = %v, want 0 (CallStatus form value MUST NOT be the metric label)", got)
	}

	// Bonus: failures[exhausted_retries] must remain 0 — the server
	// returned 200 OK on every attempt so no retry chain ran.
	if got := testutil.ToFloat64(m.StatusCallbackFailuresTotal.WithLabelValues("exhausted_retries")); got != 0 {
		t.Errorf("failures[exhausted_retries] = %v, want 0 on happy path", got)
	}
}
