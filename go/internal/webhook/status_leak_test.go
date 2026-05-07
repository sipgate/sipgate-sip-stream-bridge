package webhook

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sipgate/sipgate-sip-stream-bridge/internal/observability"
)

// Leak regression suite.
//
// Without the markTerminated → DrainAndClose wiring, c.perCall sync.Map
// would grow unbounded by one entry per CallSid that ever subscribes
// (each entry holds: capacity-64 chan + worker goroutine + sync.Map
// slot). This test proves that with the bridge-side wiring in place
// (markTerminated → DrainAndClose), the high-churn lifecycle returns
// the per-call map to zero entries.
//
// Structurally these tests live in package webhook (not bridge) so they
// can directly call the test-only PerCallCountForTest accessor in
// export_test.go without crossing a package boundary (which would force
// the accessor onto the production API surface).

// leakTestEvent builds a minimal Twilio-shape callback event for the
// leak test. The event content is intentionally compact — the leak
// invariant under test is the per-call sync.Map cleanup, not form
// fidelity (which is covered exhaustively by other tests).
func leakTestEvent(callbackURL, callSid string) CallbackEvent {
	form := url.Values{}
	form.Set("CallSid", callSid)
	form.Set("AccountSid", "ACtest0123456789abcdef0123456789ab")
	form.Set("From", "+4915123456789")
	form.Set("To", "+4930111222333")
	form.Set("Direction", "inbound")
	form.Set("ApiVersion", "2010-04-01")
	form.Set("CallStatus", "completed")
	form.Set("Timestamp", time.Now().UTC().Format(time.RFC1123Z))
	form.Set("SequenceNumber", "0")
	form.Set("CallbackSource", "call-progress-events")
	return CallbackEvent{URL: callbackURL, Method: http.MethodPost, Form: form, Event: "completed"}
}

// TestStatusClient_HighChurn_PerCallStateReturnsToZero proves 100 distinct
// CallSid lifecycles drain back to 0 entries in c.perCall. Exercises the
// full path: NewStatusClientForTest → Enqueue → DrainAndClose → assert
// PerCallCountForTest() == 0.
//
// Scaled to 100 CallSids to keep test runtime bounded under 5s while still
// providing strong evidence that the leak is closed; the larger
// 1000-CallSid variant
// (TestStatusClient_VeryHighChurn_PerCallStateReturnsToZero) covers the
// idle-state acceptance criterion.
func TestStatusClient_HighChurn_PerCallStateReturnsToZero(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewStatusClientForTest(srv.Client().Transport, "12345abcdef", observability.NewMetrics(), zerolog.Nop())

	const N = 100
	for i := 0; i < N; i++ {
		callSid := fmt.Sprintf("CAtest%026d", i)
		evt := leakTestEvent(srv.URL+"/cb", callSid)
		if err := c.Enqueue(callSid, evt); err != nil {
			t.Fatalf("Enqueue %d (%s): %v", i, callSid, err)
		}
		if err := c.DrainAndClose(callSid, 5*time.Second); err != nil {
			t.Fatalf("DrainAndClose %d (%s): %v", i, callSid, err)
		}
	}

	if got := c.PerCallCountForTest(); got != 0 {
		t.Fatalf("PerCallCountForTest after %d lifecycles = %d, want 0 (per-call sync.Map should be empty after DrainAndClose)", N, got)
	}
}

// TestStatusClient_VeryHighChurn_PerCallStateReturnsToZero — the
// 1000-CallSid variant per the idle-state acceptance criterion ("an
// idle process after handling 1000 distinct CallSids has
// webhook.StatusClient.perCall map size returning to 0"). Marked
// long-running so -short skips it; the standard count=3 test pass still
// runs both this and the 100-CallSid variant.
func TestStatusClient_VeryHighChurn_PerCallStateReturnsToZero(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 1000-CallSid leak test in short mode")
	}
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewStatusClientForTest(srv.Client().Transport, "12345abcdef", observability.NewMetrics(), zerolog.Nop())

	const N = 1000
	for i := 0; i < N; i++ {
		callSid := fmt.Sprintf("CAchurn%025d", i)
		evt := leakTestEvent(srv.URL+"/cb", callSid)
		if err := c.Enqueue(callSid, evt); err != nil {
			t.Fatalf("Enqueue %d (%s): %v", i, callSid, err)
		}
		if err := c.DrainAndClose(callSid, 5*time.Second); err != nil {
			t.Fatalf("DrainAndClose %d (%s): %v", i, callSid, err)
		}
	}

	if got := c.PerCallCountForTest(); got != 0 {
		t.Fatalf("PerCallCountForTest after %d lifecycles = %d, want 0 (per-call sync.Map should be empty after DrainAndClose)", N, got)
	}
}

// TestStatusClient_VeryHighChurn_DialLegsReturnToZero — extension of the
// per-call leak invariant for the dial-leg slot path. Drives 1000
// distinct dial-leg-shaped CallSids through complete lifecycles
// (Enqueue + DrainAndClose, mirroring what Forwarder.Dial does at
// recordFailure / success-path tail) and asserts c.PerCallCountForTest()
// returns to 0.
//
// Distinct from the parent-leg TestStatusClient_VeryHighChurn_… variant
// only in CallSid shape (CAdialeg... vs CAchurn...): the StatusClient API
// is identity-agnostic, but using a dial-leg-shaped SID makes the
// dial-leg coverage explicit in trace logs / future debugging.
//
// Marked long-running so -short skips it.
func TestStatusClient_VeryHighChurn_DialLegsReturnToZero(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 1000-dial-leg leak test in short mode")
	}
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewStatusClientForTest(srv.Client().Transport, "12345abcdef", observability.NewMetrics(), zerolog.Nop())

	const N = 1000
	for i := 0; i < N; i++ {
		// Dial-leg-shaped CallSid (32 hex chars after CA prefix). Distinct
		// from the parent-leg pattern so trace logs disambiguate the two
		// invariants when both tests run in the same -count=3 pass.
		dialCallSid := fmt.Sprintf("CAdialeg%025d", i)
		evt := leakTestEvent(srv.URL+"/cb", dialCallSid)
		if err := c.Enqueue(dialCallSid, evt); err != nil {
			t.Fatalf("Enqueue %d (%s): %v", i, dialCallSid, err)
		}
		if err := c.DrainAndClose(dialCallSid, 5*time.Second); err != nil {
			t.Fatalf("DrainAndClose %d (%s): %v", i, dialCallSid, err)
		}
	}

	if got := c.PerCallCountForTest(); got != 0 {
		t.Fatalf("PerCallCountForTest after %d dial-leg lifecycles = %d, want 0 (per-call closure proof)", N, got)
	}
}
