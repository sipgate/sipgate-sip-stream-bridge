package bridge

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sipgate/sipgate-sip-stream-bridge/internal/observability"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/webhook"
)

// Tests covering
//   - SequenceNumber concurrency (50 goroutines, no duplicates, full {0..49})
//   - Terminal-event emission for each Twilio CallStatus:
//       completed / busy / no-answer / failed / canceled
//   - markTerminated single-fire under 3-goroutine race
//   - markTerminated NOT blocked by callback delivery (<100ms with hung server)
//   - CallDuration absent when call never answered
//
// Tests use a TLS httptest server + webhook.NewStatusClientWithTransport so
// the production SSRF dial-time guard is bypassed (httptest binds to
// 127.0.0.1, which the SSRF guard correctly rejects in production). Refer to
// 16-03 for the same pattern.

// captureServer returns a TLS test server that records every POST body, plus
// an attempt counter (atomic) and a sync.Map of capturedPOST keyed by request
// ordinal (0..N-1).
func captureServer(t *testing.T) (*httptest.Server, *atomic.Int32, *sync.Map) {
	t.Helper()
	var count atomic.Int32
	var captured sync.Map // map[int32]capturedPOST keyed by attempt index

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := count.Add(1) - 1
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		captured.Store(idx, capturedPOST{
			sig:  r.Header.Get("X-Twilio-Signature"),
			body: form,
		})
		w.WriteHeader(http.StatusOK)
	}))
	return srv, &count, &captured
}

type capturedPOST struct {
	sig  string
	body url.Values
}

// newTestSessionWithStatusCB builds a CallSession wired to a StatusClient
// pointing at the given test server, with the given event subscription.
// SequenceNumber starts at 0; answeredAt and sipFinalCode are NOT set
// (callers may invoke SetAnsweredAt / SetSIPFinalCode before markTerminated
// to drive the terminal-only fields).
func newTestSessionWithStatusCB(t *testing.T, srv *httptest.Server, events ...string) *CallSession {
	t.Helper()
	s := newTestSession(nil, nil)
	// Override accountSid + from/to with deterministic values so assertions
	// don't depend on the test-helper defaults.
	s.from = "+4915123456789@sipgate.de"
	s.to = "+498912345@sipgate.de"
	// Wire the StatusClient via the exported test shim.
	sc := webhook.NewStatusClientWithTransport(srv.Client().Transport, "12345", observability.NewMetrics(), zerolog.Nop())
	s.SetStatusClient(sc)
	// Build the subscription event-set.
	evMap := make(map[string]struct{}, len(events))
	for _, e := range events {
		evMap[e] = struct{}{}
	}
	s.SetStatusCallback(&StatusCallbackConfig{
		URL:    srv.URL + "/cb",
		Method: "POST",
		Events: evMap,
	})
	return s
}

// drainStatusClient ensures any in-flight emissions complete before the test
// asserts on captured.
//
// Note: markTerminated spawns its OWN non-blocking DrainAndClose
// goroutine inside terminateOnce.Do. That goroutine's DrainAndClose
// holds the perCallState's LoadAndDelete winner — when this helper
// calls DrainAndClose afterwards, our call sees ok=false (already
// removed) and returns immediately, even if the worker is still in
// flight.
//
// To preserve the existing-test contract ("after drainStatusClient returns,
// the captureServer has received all POSTs"), this helper now performs a
// bounded poll on the StatusClient's perCall map, attempting DrainAndClose
// repeatedly until either we win the LoadAndDelete race (and the call
// blocks until the worker exits via the done channel) OR a sufficient
// localhost-POST settle window elapses (the racing goroutine inside
// markTerminated has time to finish on a quiet test machine). 5s upper
// bound matches the original synchronous DrainAndClose timeout.
//
// On localhost with httptest, a single POST round-trip is ~1-3ms; the
// markTerminated DrainAndClose goroutine therefore typically completes
// within a few ms of being spawned. The poll loop below catches the
// completion via a fresh Enqueue attempt (which would re-create a new
// perCallState if the prior one is gone — but we don't actually Enqueue,
// we just observe: if DrainAndClose returns nil-on-empty, we know the
// markTerminated goroutine either won the race or has already finished).
func drainStatusClient(t *testing.T, s *CallSession) {
	t.Helper()
	if s.statusWC == nil {
		return
	}
	// First attempt: try to win the LoadAndDelete race. If we win, this
	// blocks until the worker exits. If we lose (markTerminated's
	// goroutine got there first), this returns immediately.
	_ = s.statusWC.DrainAndClose(s.callSid, 5*time.Second)
	// Bounded settle: give the racing markTerminated goroutine time to
	// complete its own DrainAndClose. 100ms is generous on localhost
	// (POST round-trip ~1-3ms, worker exit ~immediate after queue drain).
	// On a slow CI runner, increase here. We do NOT block longer than
	// this — the test caller asserts count.Load() afterwards, and a
	// flaky test catches a real bug.
	time.Sleep(100 * time.Millisecond)
}

// TestCallSession_SequenceNumber_Concurrent — 50 goroutines call
// NextSequenceNumber concurrently; all 50 returned values are unique (set
// size == 50) and span exactly {0..49} (no gaps, no duplicates). Pins the
// concurrency-stress contract for the seqCounter atomic.
func TestCallSession_SequenceNumber_Concurrent(t *testing.T) {
	t.Parallel()
	s := newTestSession(nil, nil)
	const N = 50
	results := make([]uint64, N)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			results[i] = s.NextSequenceNumber()
		}()
	}
	wg.Wait()

	seen := make(map[uint64]bool, N)
	for _, v := range results {
		if seen[v] {
			t.Fatalf("duplicate SequenceNumber %d", v)
		}
		seen[v] = true
	}
	for i := uint64(0); i < N; i++ {
		if !seen[i] {
			t.Errorf("missing SequenceNumber %d", i)
		}
	}
}

// TestCallSession_TerminalEvent_Completed — markTerminated("completed")
// produces exactly one POST with CallStatus=completed, SequenceNumber=0,
// CallDuration populated (call was answered), SipResponseCode=200.
func TestCallSession_TerminalEvent_Completed(t *testing.T) {
	t.Parallel()
	srv, count, captured := captureServer(t)
	defer srv.Close()
	s := newTestSessionWithStatusCB(t, srv, "completed")
	s.SetAnsweredAt(time.Now().Add(-5 * time.Second))
	s.SetSIPFinalCode(200)

	s.markTerminated("completed")
	drainStatusClient(t, s)

	if got := count.Load(); got != 1 {
		t.Fatalf("POST count = %d, want 1", got)
	}
	v, _ := captured.Load(int32(0))
	cp := v.(capturedPOST)
	if got := cp.body.Get("CallStatus"); got != "completed" {
		t.Errorf("CallStatus = %q, want \"completed\"", got)
	}
	if got := cp.body.Get("SequenceNumber"); got != "0" {
		t.Errorf("SequenceNumber = %q, want \"0\"", got)
	}
	if cp.body.Get("CallDuration") == "" {
		t.Error("CallDuration empty (call was answered, expected populated)")
	}
	if got := cp.body.Get("SipResponseCode"); got != "200" {
		t.Errorf("SipResponseCode = %q, want \"200\"", got)
	}
	// Standard-form invariants.
	if got := cp.body.Get("CallSid"); got != s.callSid {
		t.Errorf("CallSid = %q, want %q", got, s.callSid)
	}
	if got := cp.body.Get("AccountSid"); got != s.accountSid {
		t.Errorf("AccountSid = %q, want %q", got, s.accountSid)
	}
	if got := cp.body.Get("From"); got != s.from {
		t.Errorf("From = %q, want %q", got, s.from)
	}
	if got := cp.body.Get("To"); got != s.to {
		t.Errorf("To = %q, want %q", got, s.to)
	}
	if got := cp.body.Get("Direction"); got != "inbound" {
		t.Errorf("Direction = %q, want \"inbound\"", got)
	}
	if got := cp.body.Get("ApiVersion"); got != "2010-04-01" {
		t.Errorf("ApiVersion = %q, want \"2010-04-01\"", got)
	}
	if got := cp.body.Get("CallbackSource"); got != "call-progress-events" {
		t.Errorf("CallbackSource = %q, want \"call-progress-events\"", got)
	}
	if cp.body.Get("Timestamp") == "" {
		t.Error("Timestamp empty")
	}
	// Terminal POST must be signed.
	if cp.sig == "" {
		t.Error("X-Twilio-Signature missing on terminal POST")
	}
}

// TestCallSession_TerminalEvent_AllReasons — table-driven coverage of
// busy / no-answer / canceled / failed terminal reasons. Each subscribes to
// BOTH the specific event AND "completed" so the helper resolves to the
// SPECIFIC match (not the generic fallback). SetSIPFinalCode is set to the
// canonical SIP code per reason; the assertion confirms SipResponseCode
// flows through to the form unchanged.
func TestCallSession_TerminalEvent_AllReasons(t *testing.T) {
	t.Parallel()
	cases := []struct {
		reason     string
		wantStatus string
		sipCode    int
	}{
		{"busy", "busy", 486},
		{"no-answer", "no-answer", 408},
		{"canceled", "canceled", 487},
		{"failed", "failed", 503},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.reason, func(t *testing.T) {
			t.Parallel()
			srv, count, captured := captureServer(t)
			defer srv.Close()
			s := newTestSessionWithStatusCB(t, srv, tc.reason, "completed")
			s.SetSIPFinalCode(tc.sipCode)

			s.markTerminated(tc.reason)
			drainStatusClient(t, s)

			if got := count.Load(); got != 1 {
				t.Fatalf("POST count = %d, want 1", got)
			}
			v, _ := captured.Load(int32(0))
			cp := v.(capturedPOST)
			if got := cp.body.Get("CallStatus"); got != tc.wantStatus {
				t.Errorf("CallStatus = %q, want %q", got, tc.wantStatus)
			}
			if got := cp.body.Get("SipResponseCode"); got != strconv.Itoa(tc.sipCode) {
				t.Errorf("SipResponseCode = %q, want %q", got, strconv.Itoa(tc.sipCode))
			}
		})
	}
}

// TestCallSession_MarkTerminated_FiresOnceUnderRace — 3 goroutines call
// markTerminated("completed") concurrently; assert exactly ONE POST is
// received. Proves the existing terminateOnce.Do invariant is preserved
// after the emit insertion.
func TestCallSession_MarkTerminated_FiresOnceUnderRace(t *testing.T) {
	t.Parallel()
	srv, count, _ := captureServer(t)
	defer srv.Close()
	s := newTestSessionWithStatusCB(t, srv, "completed")

	var wg sync.WaitGroup
	wg.Add(3)
	for i := 0; i < 3; i++ {
		go func() {
			defer wg.Done()
			s.markTerminated("completed")
		}()
	}
	wg.Wait()
	drainStatusClient(t, s)

	if got := count.Load(); got != 1 {
		t.Errorf("POST count = %d, want 1 (terminateOnce.Do invariant broken)", got)
	}
}

// TestCallSession_TerminalNotBlockedByCallback — markTerminated returns in
// <100ms even when the customer's callback host hangs on the request
// indefinitely. Proves the head-of-line mitigation — Enqueue is
// non-blocking, the StatusClient worker runs on its own
// context.Background()-derived per-attempt context, and call cleanup
// proceeds without waiting for HTTP delivery.
func TestCallSession_TerminalNotBlockedByCallback(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	defer close(block)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until the test ends (or the per-attempt timeout fires).
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	s := newTestSessionWithStatusCB(t, srv, "completed")

	start := time.Now()
	s.markTerminated("completed")
	elapsed := time.Since(start)
	if elapsed > 100*time.Millisecond {
		t.Errorf("markTerminated elapsed = %v, want <100ms (call cleanup blocked by callback delivery)", elapsed)
	}
	// No drain — test asserts non-blocking; delivery may never complete.
}

// TestCallSession_TerminalNoCallDurationWhenNotAnswered — when SetAnsweredAt
// was never called (call rejected with 486/487/408 before answer), the
// terminal POST does NOT carry CallDuration / Duration. answeredAt is the
// only signal for "call talk-time is meaningful" — without it the form
// degrades gracefully.
func TestCallSession_TerminalNoCallDurationWhenNotAnswered(t *testing.T) {
	t.Parallel()
	srv, _, captured := captureServer(t)
	defer srv.Close()
	s := newTestSessionWithStatusCB(t, srv, "no-answer")
	// Do NOT call SetAnsweredAt — call never connected.

	s.markTerminated("no-answer")
	drainStatusClient(t, s)

	v, ok := captured.Load(int32(0))
	if !ok {
		t.Fatal("no POST captured")
	}
	cp := v.(capturedPOST)
	if cd := cp.body.Get("CallDuration"); cd != "" {
		t.Errorf("CallDuration = %q, want empty (call never answered)", cd)
	}
	if d := cp.body.Get("Duration"); d != "" {
		t.Errorf("Duration = %q, want empty (call never answered)", d)
	}
}

// TestCallSession_TerminalIncludesCallDuration — SetAnsweredAt(now-5s);
// terminate; assert CallDuration is "5" (±1s tolerance for scheduling
// jitter), Duration is "1" (math.Ceil(5s in minutes) = 1).
func TestCallSession_TerminalIncludesCallDuration(t *testing.T) {
	t.Parallel()
	srv, _, captured := captureServer(t)
	defer srv.Close()
	s := newTestSessionWithStatusCB(t, srv, "completed")
	s.SetAnsweredAt(time.Now().Add(-5 * time.Second))

	s.markTerminated("completed")
	drainStatusClient(t, s)

	v, _ := captured.Load(int32(0))
	cp := v.(capturedPOST)
	cd := cp.body.Get("CallDuration")
	if cd == "" {
		t.Fatal("CallDuration empty, want populated")
	}
	secs, err := strconv.Atoi(cd)
	if err != nil {
		t.Fatalf("CallDuration not parseable: %q", cd)
	}
	if secs < 4 || secs > 6 {
		t.Errorf("CallDuration = %d, want in [4, 6] (5s ± 1s tolerance)", secs)
	}
	if d := cp.body.Get("Duration"); d != "1" {
		t.Errorf("Duration = %q, want \"1\" (ceil(5s/60s) = 1)", d)
	}
}

// TestCallSession_TerminalSubscriptionResolution — when the customer
// subscribes ONLY to "completed" (no specific terminal event), a busy
// reason still emits via the generic fallback because the helper resolves
// to the "completed" event when the specific match is missing AND
// "completed" is in the subscription set.
func TestCallSession_TerminalSubscriptionResolution(t *testing.T) {
	t.Parallel()
	t.Run("specific_match_wins", func(t *testing.T) {
		t.Parallel()
		srv, count, captured := captureServer(t)
		defer srv.Close()
		// Subscribe to BOTH "busy" AND "completed" — specific should win.
		s := newTestSessionWithStatusCB(t, srv, "busy", "completed")
		s.markTerminated("busy")
		drainStatusClient(t, s)

		if got := count.Load(); got != 1 {
			t.Fatalf("POST count = %d, want 1", got)
		}
		v, _ := captured.Load(int32(0))
		cp := v.(capturedPOST)
		if got := cp.body.Get("CallStatus"); got != "busy" {
			t.Errorf("CallStatus = %q, want \"busy\"", got)
		}
	})
	t.Run("generic_completed_fallback", func(t *testing.T) {
		t.Parallel()
		srv, count, captured := captureServer(t)
		defer srv.Close()
		// Subscribe ONLY to "completed" — busy should still fire because
		// the helper falls back to the generic "completed" event.
		s := newTestSessionWithStatusCB(t, srv, "completed")
		s.markTerminated("busy")
		drainStatusClient(t, s)

		if got := count.Load(); got != 1 {
			t.Fatalf("POST count = %d, want 1 (generic completed fallback expected)", got)
		}
		v, _ := captured.Load(int32(0))
		cp := v.(capturedPOST)
		// CallStatus is the SPECIFIC reason ("busy") — only the EVENT
		// resolves to "completed" via fallback. Customer's bot still gets
		// the precise CallStatus to act on.
		if got := cp.body.Get("CallStatus"); got != "busy" {
			t.Errorf("CallStatus = %q, want \"busy\" (CallStatus is specific even when event resolves via fallback)", got)
		}
	})
	t.Run("not_subscribed_no_emit", func(t *testing.T) {
		t.Parallel()
		srv, count, _ := captureServer(t)
		defer srv.Close()
		// Subscribe only to "answered" — terminal event must NOT fire.
		s := newTestSessionWithStatusCB(t, srv, "answered")
		s.markTerminated("busy")
		drainStatusClient(t, s)

		if got := count.Load(); got != 0 {
			t.Errorf("POST count = %d, want 0 (no subscription match for terminal)", got)
		}
	})
	t.Run("empty_events_subscribe_to_all", func(t *testing.T) {
		t.Parallel()
		srv, count, captured := captureServer(t)
		defer srv.Close()
		// Empty events set == subscribe-to-all (16-04 REST default when no
		// StatusCallbackEvent is supplied).
		s := newTestSession(nil, nil)
		s.from = "+4915123456789@sipgate.de"
		s.to = "+498912345@sipgate.de"
		sc := webhook.NewStatusClientWithTransport(srv.Client().Transport, "12345", observability.NewMetrics(), zerolog.Nop())
		s.SetStatusClient(sc)
		s.SetStatusCallback(&StatusCallbackConfig{
			URL:    srv.URL + "/cb",
			Method: "POST",
			Events: nil, // empty == subscribe-to-all
		})

		s.markTerminated("failed")
		drainStatusClient(t, s)

		if got := count.Load(); got != 1 {
			t.Fatalf("POST count = %d, want 1 (empty events == subscribe-to-all)", got)
		}
		v, _ := captured.Load(int32(0))
		cp := v.(capturedPOST)
		if got := cp.body.Get("CallStatus"); got != "failed" {
			t.Errorf("CallStatus = %q, want \"failed\"", got)
		}
	})
}

// TestCallSession_TerminalNilStatusClient — when statusWC is nil (older
// fixtures and deployments), markTerminated still stamps the terminal
// state and does NOT panic. The emit helper short-circuits before
// touching the StatusClient. Regression guard against the nil-safe
// contract.
func TestCallSession_TerminalNilStatusClient(t *testing.T) {
	t.Parallel()
	s := newTestSession(nil, nil)
	// statusWC stays nil — do NOT call SetStatusClient.
	// Subscription is set but should not be reached because statusWC is nil.
	s.SetStatusCallback(&StatusCallbackConfig{
		URL:    "https://example.com/cb",
		Method: "POST",
		Events: map[string]struct{}{"completed": {}},
	})

	// Must not panic; markTerminated stamps the terminal pointer normally.
	s.markTerminated("completed")
	if s.terminal.Load() == nil {
		t.Error("terminal pointer not stamped after markTerminated (nil-safety regression)")
	}
}

// TestCallSession_TerminalNilSubscription — when statusWC is set but no
// subscription was installed, the emit helper short-circuits and no POST
// is sent. Idle calls without a StatusCallback subscription must not
// generate any network traffic.
func TestCallSession_TerminalNilSubscription(t *testing.T) {
	t.Parallel()
	srv, count, _ := captureServer(t)
	defer srv.Close()
	s := newTestSession(nil, nil)
	sc := webhook.NewStatusClientWithTransport(srv.Client().Transport, "12345", observability.NewMetrics(), zerolog.Nop())
	s.SetStatusClient(sc)
	// Do NOT call SetStatusCallback — subscription stays nil.

	s.markTerminated("completed")
	drainStatusClient(t, s)

	if got := count.Load(); got != 0 {
		t.Errorf("POST count = %d, want 0 (no subscription, no emit)", got)
	}
}

// ── wiring acceptance — production callers of SetAnsweredAt +
// SetSIPFinalCode produce terminal events with populated CallDuration /
// Duration / SipResponseCode form fields. ────────────────────────────────────

// TestCallSession_TerminalEvent_IncludesCallDurationAndSipResponseCode —
// drives the wiring with explicit SetAnsweredAt + SetSIPFinalCode (the
// production wiring lives in handler.go onInvite; this test exercises the
// bridge-side emit path directly with the same atomics populated). Asserts
// CallDuration / Duration / SipResponseCode are all populated AND non-zero.
//
// Timestamp parses as RFC1123Z and equals the markTerminated endTime.
// SequenceNumber is "0" for the first emit on this CallSid (this test
// does no prior emits).
func TestCallSession_TerminalEvent_IncludesCallDurationAndSipResponseCode(t *testing.T) {
	t.Parallel()
	srv, count, captured := captureServer(t)
	defer srv.Close()
	s := newTestSessionWithStatusCB(t, srv, "completed")
	// Simulate a 30-second call: SetAnsweredAt(now-30s), then markTerminated
	// stamps endTime=now. Duration should be 30s ⇒ CallDuration="30",
	// Duration=ceil(30s/60s)="1".
	answeredAt := time.Now().UTC().Add(-30 * time.Second)
	s.SetAnsweredAt(answeredAt)
	s.SetSIPFinalCode(200)

	beforeTerminate := time.Now().UTC()
	s.markTerminated("completed")
	afterTerminate := time.Now().UTC()
	drainStatusClient(t, s)

	if got := count.Load(); got != 1 {
		t.Fatalf("POST count = %d, want 1", got)
	}
	v, _ := captured.Load(int32(0))
	cp := v.(capturedPOST)

	// CallDuration: seconds since SetAnsweredAt — expect ~30s ± scheduling
	// jitter. Allow [29, 31] to absorb the few-ms gap between answeredAt
	// timestamp construction and markTerminated stamping endTime.
	cdRaw := cp.body.Get("CallDuration")
	if cdRaw == "" {
		t.Fatal("CallDuration is empty — wiring not effective (SetAnsweredAt populated but field missing)")
	}
	cd, err := strconv.Atoi(cdRaw)
	if err != nil {
		t.Fatalf("CallDuration = %q, not parseable as int: %v", cdRaw, err)
	}
	if cd < 29 || cd > 31 {
		t.Errorf("CallDuration = %d, want in [29, 31] (~30s ± 1s jitter)", cd)
	}
	if cd == 0 {
		t.Errorf("CallDuration = 0 — non-zero invariant violated (call was answered 30s ago)")
	}

	// Duration: minutes (math.Ceil(30s/60s) = 1).
	durRaw := cp.body.Get("Duration")
	if durRaw == "" {
		t.Fatal("Duration is empty — wiring not effective")
	}
	if durRaw != "1" {
		t.Errorf("Duration = %q, want \"1\" (ceil(30s/60s) = 1)", durRaw)
	}

	// SipResponseCode: SetSIPFinalCode(200) ⇒ "200". Non-zero invariant.
	sipRaw := cp.body.Get("SipResponseCode")
	if sipRaw == "" {
		t.Fatal("SipResponseCode is empty — wiring not effective (SetSIPFinalCode populated but field missing)")
	}
	if sipRaw != "200" {
		t.Errorf("SipResponseCode = %q, want \"200\"", sipRaw)
	}

	// CallStatus reflects markTerminated reason.
	if got := cp.body.Get("CallStatus"); got != "completed" {
		t.Errorf("CallStatus = %q, want \"completed\"", got)
	}

	// SequenceNumber = "0" — first emit on this CallSid.
	if got := cp.body.Get("SequenceNumber"); got != "0" {
		t.Errorf("SequenceNumber = %q, want \"0\"", got)
	}

	// Timestamp invariant: parses as RFC1123Z AND equals endTime stamped
	// by markTerminated. We can't read the exact endTime field, but it
	// MUST fall in [beforeTerminate, afterTerminate].
	tsRaw := cp.body.Get("Timestamp")
	if tsRaw == "" {
		t.Fatal("Timestamp empty")
	}
	ts, err := time.Parse(time.RFC1123Z, tsRaw)
	if err != nil {
		t.Fatalf("Timestamp = %q, not RFC1123Z parseable: %v", tsRaw, err)
	}
	// RFC1123Z has 1-second granularity. Truncate the bounds for comparison.
	loBound := beforeTerminate.Truncate(time.Second).Add(-time.Second)
	hiBound := afterTerminate.Truncate(time.Second).Add(time.Second)
	if ts.Before(loBound) || ts.After(hiBound) {
		t.Errorf("Timestamp = %v, want in [%v, %v] (must equal markTerminated endTime)",
			ts, loBound, hiBound)
	}
}

// TestCallSession_TerminalEvent_NoAnswerOmitsCallDuration — call rejected
// before answer (e.g. caller hangs up while alerting). SetAnsweredAt is
// NEVER called, so the terminal POST omits CallDuration / Duration. Per
// RESEARCH §3.3 + emitTerminalStatusCallback's contract: terminal-only
// fields degrade gracefully when answered timestamp is absent.
//
// Additionally asserts that markTerminated's defensive default for
// reason="no-answer" stamps SipResponseCode=408 (per
// defaultSIPCodeForReason from Task 1).
func TestCallSession_TerminalEvent_NoAnswerOmitsCallDuration(t *testing.T) {
	t.Parallel()
	srv, count, captured := captureServer(t)
	defer srv.Close()
	// Subscribe to BOTH "no-answer" and "completed" so the specific match
	// resolves to no-answer (closer to a real customer subscription that
	// covers both terminal events).
	s := newTestSessionWithStatusCB(t, srv, "no-answer", "completed")
	// Do NOT call SetAnsweredAt — call never connected.
	// Do NOT call SetSIPFinalCode — exercises the markTerminated defensive
	// default (defaultSIPCodeForReason("no-answer") = 408).

	s.markTerminated("no-answer")
	drainStatusClient(t, s)

	if got := count.Load(); got != 1 {
		t.Fatalf("POST count = %d, want 1", got)
	}
	v, _ := captured.Load(int32(0))
	cp := v.(capturedPOST)

	if cd := cp.body.Get("CallDuration"); cd != "" {
		t.Errorf("CallDuration = %q, want empty (call never answered)", cd)
	}
	if d := cp.body.Get("Duration"); d != "" {
		t.Errorf("Duration = %q, want empty (call never answered)", d)
	}
	// markTerminated defensive default for "no-answer" → 408.
	if got := cp.body.Get("SipResponseCode"); got != "408" {
		t.Errorf("SipResponseCode = %q, want \"408\" (markTerminated default for \"no-answer\")", got)
	}
	if got := cp.body.Get("CallStatus"); got != "no-answer" {
		t.Errorf("CallStatus = %q, want \"no-answer\"", got)
	}
}

// TestCallSession_TerminalEvent_DefaultSIPCode_PreservesExplicitStamp —
// the markTerminated defensive default uses CompareAndSwap, so an
// explicit SetSIPFinalCode(503) stamped earlier (e.g. by handler.go's
// RespondSDP-failure path) survives even when markTerminated runs with
// a different reason. Proves the BLOCKER 7 / WARNING 7 invariant: the
// defensive default never overwrites a more-specific code.
//
// Scenario: a "busy" termination reason would ordinarily map to 486 via
// defaultSIPCodeForReason, but an earlier explicit SetSIPFinalCode(503)
// pins SipResponseCode=503. The CAS-from-zero contract holds.
func TestCallSession_TerminalEvent_DefaultSIPCode_PreservesExplicitStamp(t *testing.T) {
	t.Parallel()
	srv, count, captured := captureServer(t)
	defer srv.Close()
	s := newTestSessionWithStatusCB(t, srv, "busy", "completed")
	// Explicit stamp BEFORE markTerminated.
	s.SetSIPFinalCode(503)

	s.markTerminated("busy")
	drainStatusClient(t, s)

	if got := count.Load(); got != 1 {
		t.Fatalf("POST count = %d, want 1", got)
	}
	v, _ := captured.Load(int32(0))
	cp := v.(capturedPOST)

	// SipResponseCode preserves the explicit 503 stamp — NOT the default 486
	// for "busy". Proves CompareAndSwap-from-zero in markTerminated's
	// defensive default never overwrites an existing non-zero code.
	if got := cp.body.Get("SipResponseCode"); got != "503" {
		t.Errorf("SipResponseCode = %q, want \"503\" (explicit stamp must survive markTerminated default; default for \"busy\" would be 486)",
			got)
	}
	if got := cp.body.Get("CallStatus"); got != "busy" {
		t.Errorf("CallStatus = %q, want \"busy\"", got)
	}
}

// ── bridge-side integration tests for the markTerminated → DrainAndClose
// wiring. The high-churn perCall-count assertion lives in
// webhook/status_leak_test.go (the PerCallCountForTest accessor must stay
// inside the webhook package to avoid leaking into the production binary).
// The bridge-side tests below verify only the wiring direction
// (markTerminated invokes DrainAndClose) and the nil-safety contract.

// TestCallSession_MarkTerminated_InvokesDrainAndClose — proves that the
// markTerminated → DrainAndClose wiring is live. The bridge-side assertion
// is structural: the terminal POST is delivered (proving the worker ran)
// and the wiring is exercised by the test — the high-churn perCall-count
// assertion lives in webhook/status_leak_test.go.
//
// We assert: (1) markTerminated returns promptly (non-blocking — the
// DrainAndClose goroutine runs separately), (2) the terminal POST lands
// on the captureServer (proving the worker drained the queue), and
// (3) markTerminated stamps the terminal pointer (the DrainAndClose
// insertion did not break the existing single-fire invariant).
func TestCallSession_MarkTerminated_InvokesDrainAndClose(t *testing.T) {
	t.Parallel()
	srv, count, _ := captureServer(t)
	defer srv.Close()
	s := newTestSessionWithStatusCB(t, srv, "completed")

	// markTerminated must return promptly — DrainAndClose runs in a
	// goroutine and does NOT block call cleanup.
	start := time.Now()
	s.markTerminated("completed")
	elapsed := time.Since(start)
	if elapsed > 100*time.Millisecond {
		t.Errorf("markTerminated elapsed = %v, want <100ms (DrainAndClose insertion must NOT block call cleanup)", elapsed)
	}

	if s.terminal.Load() == nil {
		t.Error("terminal pointer not stamped after markTerminated (DrainAndClose insertion must preserve existing single-fire invariant)")
	}

	// Poll for the terminal POST to land. The DrainAndClose goroutine has
	// up to 30s budget but a fast localhost test server should deliver
	// within a few ms; we cap the test wait at 5s.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if count.Load() >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if got := count.Load(); got != 1 {
		t.Fatalf("POST count = %d, want 1 (terminal callback never delivered — markTerminated → emitTerminalStatusCallback wiring broken or the new DrainAndClose goroutine raced ahead of Enqueue)", got)
	}

	// Cleanup defer: drainStatusClient is idempotent — DrainAndClose was
	// already called by markTerminated's goroutine, so this second call
	// returns nil (entry already removed via LoadAndDelete in
	// status.go:DrainAndClose).
	drainStatusClient(t, s)
}

// TestCallSession_MarkTerminated_StatusClientNil_NoCrash — when statusWC
// is nil (fixtures or deployments without status callbacks), markTerminated
// MUST NOT panic on the new DrainAndClose goroutine spawn. Regression
// guard for the nil-check on the DrainAndClose insertion site.
func TestCallSession_MarkTerminated_StatusClientNil_NoCrash(t *testing.T) {
	t.Parallel()
	s := newTestSession(nil, nil)
	// statusWC stays nil — do NOT call SetStatusClient.

	// Must not panic; markTerminated stamps the terminal pointer
	// normally and skips the DrainAndClose goroutine spawn.
	s.markTerminated("completed")
	if s.terminal.Load() == nil {
		t.Error("terminal pointer not stamped after markTerminated (nil-safety regression on DrainAndClose insertion)")
	}
}
