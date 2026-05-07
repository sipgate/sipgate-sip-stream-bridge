package sip

import (
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

// ── Parent-leg status-callback emission tests ──────────────────────────────
//
// These tests exercise the emit helpers DIRECTLY (calling h.emitInitiated /
// emitRinging / emitAnswered) with a synthetic statusLookup closure that
// returns a pre-built subscription. This proves:
//
//   1. The form payload matches the Twilio shape.
//   2. CallStatus correctly diverges from event name (event/status
//      divergence).
//   3. Non-subscribed events drop silently.
//   4. nil StatusClient / nil lookup are no-ops.
//   5. The realistic "no session yet" path returns silently.
//   6. Hook callsites are reachable from onInvite (compile-time check via
//      the grep gate; runtime assertion via the helper-direct tests).

// makeTestHandler builds a Handler with statusWC pointed at srv and a
// statusLookup closure parameterised by `sub` (returned for every callSid)
// and `ok` (return value of the lookup). seq starts at 0 and increments
// per-call so tests see deterministic SequenceNumber values.
//
// The closure returns a sip.PreRegisteredSession in slot 3.
// makeTestHandler always returns nil for that slot — emitStatusEvent's
// `if session != nil { session.MarkEmitted() }` guard handles the nil-safe
// path, so the existing tests that don't exercise MarkEmitted continue to
// pass without modification. Tests that DO exercise MarkEmitted (the
// PreRegisterSession + GhostTerminal tests) construct their own closures.
func makeTestHandler(t *testing.T, srv *httptest.Server, sub *StatusSubscription, ok bool, accountSid string) *Handler {
	t.Helper()
	wc := webhook.NewStatusClientForTest(srv.Client().Transport, "12345abcdef", observability.NewMetrics(), zerolog.Nop())
	var seq atomic.Uint64
	lookup := func(callSid string) (*StatusSubscription, uint64, PreRegisteredSession, bool) {
		if !ok {
			return nil, 0, nil, false
		}
		return sub, seq.Add(1) - 1, nil, true
	}
	h := &Handler{
		log: zerolog.Nop(),
	}
	h.SetStatusEmission(wc, lookup, accountSid)
	return h
}

// captured records a single received status-callback POST.
type capturedHandlerCallback struct {
	URL    string
	Method string
	Form   url.Values
	Header http.Header
}

func newHandlerCaptureServer(t *testing.T) (*httptest.Server, *[]capturedHandlerCallback, *sync.Mutex) {
	t.Helper()
	captured := []capturedHandlerCallback{}
	var mu sync.Mutex
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		captured = append(captured, capturedHandlerCallback{
			URL:    r.URL.String(),
			Method: r.Method,
			Form:   r.PostForm,
			Header: r.Header.Clone(),
		})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	return srv, &captured, &mu
}

func handlerDrainServer(t *testing.T, mu *sync.Mutex, captured *[]capturedHandlerCallback, expected int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(*captured)
		mu.Unlock()
		if n >= expected {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("captureServer received %d POSTs, expected %d", len(*captured), expected)
}

func handlerSettleNoExtra(t *testing.T, mu *sync.Mutex, captured *[]capturedHandlerCallback, expected int) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(*captured) > expected {
		t.Fatalf("captureServer received %d POSTs, expected exactly %d", len(*captured), expected)
	}
}

// TestSIPHandler_EmitsInitiatedFormShape — verifies the form payload built
// by emitInitiated matches the Twilio shape (RESEARCH §3.3): CallSid /
// AccountSid / From / To / Caller / Called / Direction / ApiVersion /
// CallStatus / Timestamp / SequenceNumber / CallbackSource. CallStatus is
// "queued" (not "initiated").
func TestSIPHandler_EmitsInitiatedFormShape(t *testing.T) {
	t.Parallel()
	srv, captured, mu := newHandlerCaptureServer(t)
	defer srv.Close()

	sub := &StatusSubscription{
		URL:    srv.URL + "/cb",
		Method: "POST",
		Events: map[string]struct{}{"initiated": {}, "ringing": {}, "answered": {}},
	}
	h := makeTestHandler(t, srv, sub, true, "ACtest0123456789abcdef0123456789ab")

	callSid := "CAtest0000000000000000000000000000"
	h.emitInitiated(callSid, "+4915123ani", "+4930555to", zerolog.Nop())

	handlerDrainServer(t, mu, captured, 1)

	mu.Lock()
	got := (*captured)[0]
	mu.Unlock()

	check := func(field, want string) {
		t.Helper()
		if got.Form.Get(field) != want {
			t.Errorf("%s = %q, want %q", field, got.Form.Get(field), want)
		}
	}
	check("CallSid", callSid)
	check("AccountSid", "ACtest0123456789abcdef0123456789ab")
	check("From", "+4915123ani")
	check("To", "+4930555to")
	check("Caller", "+4915123ani")
	check("Called", "+4930555to")
	check("Direction", "inbound")
	check("ApiVersion", "2010-04-01")
	check("CallStatus", "queued") // event=initiated, status=queued (§3.1)
	check("SequenceNumber", "0")
	check("CallbackSource", "call-progress-events")
	if got.Form.Get("Timestamp") == "" {
		t.Errorf("Timestamp is empty")
	}
	// Timestamp MUST parse as RFC1123Z (RESEARCH §3.4).
	if _, err := time.Parse(time.RFC1123Z, got.Form.Get("Timestamp")); err != nil {
		t.Errorf("Timestamp %q does not parse as RFC1123Z: %v", got.Form.Get("Timestamp"), err)
	}
}

// TestSIPHandler_EmitsRinging — emitRinging fires CallStatus=ringing.
func TestSIPHandler_EmitsRinging(t *testing.T) {
	t.Parallel()
	srv, captured, mu := newHandlerCaptureServer(t)
	defer srv.Close()

	sub := &StatusSubscription{
		URL:    srv.URL + "/cb",
		Method: "POST",
		Events: map[string]struct{}{"ringing": {}},
	}
	h := makeTestHandler(t, srv, sub, true, "ACtest0123456789abcdef0123456789ab")
	h.emitRinging("CAtest0000000000000000000000000000", "+49a", "+49b", zerolog.Nop())

	handlerDrainServer(t, mu, captured, 1)
	mu.Lock()
	defer mu.Unlock()
	if (*captured)[0].Form.Get("CallStatus") != "ringing" {
		t.Errorf("CallStatus = %q, want ringing", (*captured)[0].Form.Get("CallStatus"))
	}
}

// TestSIPHandler_EmitsAnswered — emitAnswered fires CallStatus=in-progress
// (NOT "answered" — event/status divergence per §3.1).
func TestSIPHandler_EmitsAnswered(t *testing.T) {
	t.Parallel()
	srv, captured, mu := newHandlerCaptureServer(t)
	defer srv.Close()

	sub := &StatusSubscription{
		URL:    srv.URL + "/cb",
		Method: "POST",
		Events: map[string]struct{}{"answered": {}},
	}
	h := makeTestHandler(t, srv, sub, true, "ACtest0123456789abcdef0123456789ab")
	h.emitAnswered("CAtest0000000000000000000000000000", "+49a", "+49b", zerolog.Nop())

	handlerDrainServer(t, mu, captured, 1)
	mu.Lock()
	defer mu.Unlock()
	if (*captured)[0].Form.Get("CallStatus") != "in-progress" {
		t.Errorf("CallStatus = %q, want in-progress (event=answered, status=in-progress per §3.1)",
			(*captured)[0].Form.Get("CallStatus"))
	}
}

// TestSIPHandler_DropsNonSubscribedEvents — when subscription only includes
// "completed" but emitInitiated is called, NO POST is sent.
func TestSIPHandler_DropsNonSubscribedEvents(t *testing.T) {
	t.Parallel()
	srv, captured, mu := newHandlerCaptureServer(t)
	defer srv.Close()

	sub := &StatusSubscription{
		URL:    srv.URL + "/cb",
		Method: "POST",
		Events: map[string]struct{}{"completed": {}}, // only completed
	}
	h := makeTestHandler(t, srv, sub, true, "ACtest0123456789abcdef0123456789ab")

	h.emitInitiated("CAtest0000000000000000000000000000", "+49a", "+49b", zerolog.Nop())
	h.emitRinging("CAtest0000000000000000000000000000", "+49a", "+49b", zerolog.Nop())
	h.emitAnswered("CAtest0000000000000000000000000000", "+49a", "+49b", zerolog.Nop())

	handlerSettleNoExtra(t, mu, captured, 0)
}

// TestSIPHandler_DropsNonSubscribedEvents_EmptyEventsAllAllowed — when the
// subscription Events map is empty (the Twilio "subscribe-to-all"
// default), ALL three lifecycle hooks emit. A previous local HasEvent
// helper returned false for empty Events, dropping every lifecycle
// event; the canonical webhook.SubscriptionMatches treats empty as
// subscribe-to-all uniformly across handler/forwarder/session.
func TestSIPHandler_DropsNonSubscribedEvents_EmptyEventsAllAllowed(t *testing.T) {
	t.Parallel()
	srv, captured, mu := newHandlerCaptureServer(t)
	defer srv.Close()

	sub := &StatusSubscription{
		URL:    srv.URL + "/cb",
		Method: "POST",
		Events: map[string]struct{}{}, // empty ⇒ subscribe-to-all (Twilio default)
	}
	h := makeTestHandler(t, srv, sub, true, "ACtest0123456789abcdef0123456789ab")

	h.emitInitiated("CAtest0000000000000000000000000000", "+49a", "+49b", zerolog.Nop())
	h.emitRinging("CAtest0000000000000000000000000000", "+49a", "+49b", zerolog.Nop())
	h.emitAnswered("CAtest0000000000000000000000000000", "+49a", "+49b", zerolog.Nop())

	// All THREE lifecycle events should arrive — empty Events is
	// subscribe-to-all (a previous version dropped them all via HasEvent).
	handlerDrainServer(t, mu, captured, 3)
	handlerSettleNoExtra(t, mu, captured, 3)
}

// TestSIPHandler_NoSubscriptionNoEmit — lookup returns ok=false (no session
// in callManager — the realistic case at line 113). NO POST sent.
func TestSIPHandler_NoSubscriptionNoEmit(t *testing.T) {
	t.Parallel()
	srv, captured, mu := newHandlerCaptureServer(t)
	defer srv.Close()

	// ok=false simulates "no session registered yet".
	h := makeTestHandler(t, srv, nil, false, "ACtest0123456789abcdef0123456789ab")
	h.emitInitiated("CAtest0000000000000000000000000000", "+49a", "+49b", zerolog.Nop())
	h.emitRinging("CAtest0000000000000000000000000000", "+49a", "+49b", zerolog.Nop())
	h.emitAnswered("CAtest0000000000000000000000000000", "+49a", "+49b", zerolog.Nop())

	handlerSettleNoExtra(t, mu, captured, 0)
}

// TestSIPHandler_NilStatusClientNoEmit — Handler with nil statusWC does NOT
// crash on emit. Backwards-compatibility for fixtures without statusWC.
func TestSIPHandler_NilStatusClientNoEmit(t *testing.T) {
	t.Parallel()
	h := &Handler{log: zerolog.Nop()}
	// statusWC + statusLookup left nil.
	h.emitInitiated("CAtest", "+49a", "+49b", zerolog.Nop())
	h.emitRinging("CAtest", "+49a", "+49b", zerolog.Nop())
	h.emitAnswered("CAtest", "+49a", "+49b", zerolog.Nop())
	// No assertion — test passes if we don't crash.
}

// TestSIPHandler_HookNonBlocking — multiple sequential emits each return in
// well under 50ms even when a non-trivial subscription exists. Proves the
// fire-and-forget contract (§5.2: Enqueue is non-blocking).
func TestSIPHandler_HookNonBlocking(t *testing.T) {
	t.Parallel()
	srv, _, _ := newHandlerCaptureServer(t)
	defer srv.Close()
	sub := &StatusSubscription{
		URL:    srv.URL + "/cb",
		Method: "POST",
		Events: map[string]struct{}{"initiated": {}, "ringing": {}, "answered": {}},
	}
	h := makeTestHandler(t, srv, sub, true, "ACtest0123456789abcdef0123456789ab")
	start := time.Now()
	for i := 0; i < 5; i++ {
		h.emitInitiated("CAtest"+strconv.Itoa(i), "+49a", "+49b", zerolog.Nop())
		h.emitRinging("CAtest"+strconv.Itoa(i), "+49a", "+49b", zerolog.Nop())
		h.emitAnswered("CAtest"+strconv.Itoa(i), "+49a", "+49b", zerolog.Nop())
	}
	elapsed := time.Since(start)
	if elapsed > 100*time.Millisecond {
		t.Errorf("15 emits took %v, expected < 100ms (fire-and-forget contract)", elapsed)
	}
}

// Coverage for subscription resolution lives in
// internal/webhook/subscription_test.go (TestSubscriptionMatches_TableDriven
// + TestResolveEventName_TableDriven + TestIsTerminalEvent_TableDriven)
// — strictly broader than the historical local helper test, including
// empty-events subscribe-to-all and generic-completed fallback for
// terminal events.

// ── Ghost-terminal suppression + MarkEmitted gate ──────────────────────────

// fakePreRegisteredSession is a sip.PreRegisteredSession stub that records
// MarkEmitted calls. Used by tests that need to assert whether the handler
// actually flipped the everEmitted flag on the session. Satisfies the iface
// declared in preregistered.go.
type fakePreRegisteredSession struct {
	callSid       string
	marked        atomic.Bool
	markCallCount atomic.Int32
}

func (f *fakePreRegisteredSession) SetAnsweredAt(_ time.Time) {}
func (f *fakePreRegisteredSession) SetSIPFinalCode(_ int)     {}
func (f *fakePreRegisteredSession) CallSid() string           { return f.callSid }
func (f *fakePreRegisteredSession) MarkEmitted() bool {
	f.markCallCount.Add(1)
	return f.marked.CompareAndSwap(false, true)
}

// makeTestHandlerWithSession builds a Handler whose statusLookup returns the
// given fakePreRegisteredSession, so the test can assert that emitStatusEvent
// called MarkEmitted on it after a successful Enqueue.
func makeTestHandlerWithSession(t *testing.T, srv *httptest.Server, sub *StatusSubscription, ok bool, sess PreRegisteredSession, accountSid string) *Handler {
	t.Helper()
	wc := webhook.NewStatusClientForTest(srv.Client().Transport, "12345abcdef", observability.NewMetrics(), zerolog.Nop())
	var seq atomic.Uint64
	lookup := func(callSid string) (*StatusSubscription, uint64, PreRegisteredSession, bool) {
		if !ok {
			return nil, 0, nil, false
		}
		return sub, seq.Add(1) - 1, sess, true
	}
	h := &Handler{log: zerolog.Nop()}
	h.SetStatusEmission(wc, lookup, accountSid)
	return h
}

// TestSIPHandler_EmitStatusEvent_StampsMarkEmittedOnSuccess — after a
// successful Enqueue, emitStatusEvent calls session.MarkEmitted(). Closes
// BLOCKER 3 wiring at the handler-side first-emit gate.
func TestSIPHandler_EmitStatusEvent_StampsMarkEmittedOnSuccess(t *testing.T) {
	t.Parallel()
	srv, captured, mu := newHandlerCaptureServer(t)
	defer srv.Close()

	sub := &StatusSubscription{
		URL:    srv.URL + "/cb",
		Method: "POST",
		Events: map[string]struct{}{"initiated": {}},
	}
	sess := &fakePreRegisteredSession{callSid: "CAmarktest000000000000000000000000"}
	h := makeTestHandlerWithSession(t, srv, sub, true, sess, "ACtest0123456789abcdef0123456789ab")

	h.emitInitiated("CAmarktest000000000000000000000000", "+49a", "+49b", zerolog.Nop())
	handlerDrainServer(t, mu, captured, 1)

	if !sess.marked.Load() {
		t.Error("MarkEmitted was not called after successful Enqueue (BLOCKER 3 wiring missing)")
	}
	if got := sess.markCallCount.Load(); got != 1 {
		t.Errorf("MarkEmitted call count = %d, want 1 (called exactly once after successful Enqueue)", got)
	}
}

// TestSIPHandler_EmitStatusEvent_DoesNotStampMarkEmittedWhenNotSubscribed —
// when the event is not in the subscription Events set, emitStatusEvent
// short-circuits BEFORE calling MarkEmitted. The cleanup-on-failure path
// will then correctly observe everEmitted=false and suppress the ghost
// terminal callback.
func TestSIPHandler_EmitStatusEvent_DoesNotStampMarkEmittedWhenNotSubscribed(t *testing.T) {
	t.Parallel()
	srv, _, _ := newHandlerCaptureServer(t)
	defer srv.Close()

	// Subscribe ONLY to "completed" — emitInitiated must NOT match.
	sub := &StatusSubscription{
		URL:    srv.URL + "/cb",
		Method: "POST",
		Events: map[string]struct{}{"completed": {}},
	}
	sess := &fakePreRegisteredSession{callSid: "CAnomarktst0000000000000000000000"}
	h := makeTestHandlerWithSession(t, srv, sub, true, sess, "ACtest0123456789abcdef0123456789ab")

	h.emitInitiated("CAnomarktst0000000000000000000000", "+49a", "+49b", zerolog.Nop())
	// Settle — no POST should arrive (subscription doesn't include initiated).
	time.Sleep(150 * time.Millisecond)

	if sess.marked.Load() {
		t.Error("MarkEmitted was called even though emit was not subscribed (BLOCKER 3 over-stamping)")
	}
	if got := sess.markCallCount.Load(); got != 0 {
		t.Errorf("MarkEmitted call count = %d, want 0 (never called when subscription doesn't match)", got)
	}
}

// TestSIPHandler_OnInvite_PreRegisterSession_FailedINVITE_NoGhostTerminal —
// drives a failure path where emitInitiated runs but then emit Enqueue
// fails (the test simulates this by using a nil statusLookup so the emit
// is a no-op — equivalent semantically to a failed Enqueue at the
// MarkEmitted gate). The session.MarkEmitted is therefore NEVER called,
// and a subsequent cleanup-on-failure path WOULD correctly suppress the
// terminal callback.
//
// The full inbound INVITE drive requires sipgo testing helpers that aren't
// yet built out for this package. We exercise the equivalent invariant:
// emit-without-session-handle leaves MarkEmitted=false. The bridge-side
// cleanup-on-failure suppression test
// (TestCallManager_PreRegisterSession_RegistersSynchronously) exercises
// the gate itself with a real *bridge.CallSession.
func TestSIPHandler_OnInvite_PreRegisterSession_FailedINVITE_NoGhostTerminal(t *testing.T) {
	t.Parallel()
	srv, _, _ := newHandlerCaptureServer(t)
	defer srv.Close()

	// nil session in the lookup — simulates the path where the lookup
	// resolves a subscription but no session pointer is available (e.g.
	// tests that intentionally don't wire MarkEmitted). emitStatusEvent's
	// `if session != nil` guard MUST keep MarkEmitted from being called.
	sub := &StatusSubscription{
		URL:    srv.URL + "/cb",
		Method: "POST",
		Events: map[string]struct{}{"initiated": {}, "ringing": {}, "answered": {}},
	}
	// Use the sub-returning lookup with nil session.
	wc := webhook.NewStatusClientForTest(srv.Client().Transport, "12345abcdef", observability.NewMetrics(), zerolog.Nop())
	var seq atomic.Uint64
	lookup := func(callSid string) (*StatusSubscription, uint64, PreRegisteredSession, bool) {
		return sub, seq.Add(1) - 1, nil, true
	}
	h := &Handler{log: zerolog.Nop()}
	h.SetStatusEmission(wc, lookup, "ACtest0123456789abcdef0123456789ab")

	// emit without a session handle — must NOT panic; the production
	// counterpart (cleanup-on-failure with session.EverEmitted()==false)
	// will correctly suppress any subsequent terminal callback because no
	// MarkEmitted call ever stamped the flag.
	h.emitInitiated("CAghostterm000000000000000000000000", "+49a", "+49b", zerolog.Nop())
	// No panic + no MarkEmitted call to assert because session is nil — the
	// bridge-side test
	// TestCallManager_PreRegisterSession_RegistersSynchronously asserts the
	// cleanup-suppression invariant directly on a real *bridge.CallSession.
}
