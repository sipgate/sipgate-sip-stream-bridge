package e2e

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sipgate/sipgate-sip-stream-bridge/internal/observability"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/webhook"
)

// TestE2EGoroutineLeak_NumGoroutineReturnsToBaseline verifies the
// goroutine baseline metric — runtime.NumGoroutine() returns to
// baseline within ~5s of process settle (no goroutine leaks under
// load).
//
// Invariant: typical 8-12 goroutines baseline; after 100 calls + 5s
// settle must return to baseline ± 2.
//
// Production invariant: every per-CallSid worker goroutine spawned
// inside webhook.StatusClient at first Enqueue MUST exit by the time
// DrainAndClose returns. The bridge wires DrainAndClose into
// CallSession.markTerminated so every terminated call triggers per-call
// goroutine cleanup. A regression that drops the DrainAndClose wiring
// would leak one goroutine per CallSid; this test catches that within
// 100 calls.
//
// Pattern from go/internal/webhook/status_leak_test.go (1000-CallSid
// invariant via PerCallCountForTest()==0). For the e2e variant the
// invariant is NumGoroutine() <= baseline+2; PerCallCountForTest is
// not exported across the package boundary so we use the broader
// runtime metric.
func TestE2EGoroutineLeak_NumGoroutineReturnsToBaseline(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e goroutine leak test skipped in -short mode")
	}
	t.Parallel()

	// Stand up a stub status-callback receiver. Real HTTP path so the
	// per-call worker goroutine inside webhook.StatusClient is actually
	// spawned + joined.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// NewStatusClientForTest skips SSRF validation (the URL points at
	// 127.0.0.1) — same convention used by webhook/status_leak_test.go.
	c := webhook.NewStatusClientForTest(srv.Client().Transport, "12345abcdef",
		observability.NewMetrics(), zerolog.Nop())

	// Drive one warm-up iteration so the httptest server's internal
	// goroutines (accept loop, idle-conn cleanup) and the http.Transport's
	// idle-connection manager are fully spun up before we sample the
	// baseline. The invariant is "after 100 calls returns to
	// baseline ± 2"; the baseline must be measured AFTER infrastructure
	// goroutines have stabilised, not before.
	{
		warmupSid := "CAleakwarmup0000000000000000000000"
		form := url.Values{}
		form.Set("CallSid", warmupSid)
		form.Set("AccountSid", "ACtest0123456789abcdef0123456789ab")
		form.Set("CallStatus", "completed")
		form.Set("Timestamp", time.Now().UTC().Format(time.RFC1123Z))
		form.Set("SequenceNumber", "0")
		evt := webhook.CallbackEvent{
			URL: srv.URL + "/cb", Method: http.MethodPost,
			Form: form, Event: "completed",
		}
		if err := c.Enqueue(warmupSid, evt); err != nil {
			t.Fatalf("warm-up Enqueue: %v", err)
		}
		if err := c.DrainAndClose(warmupSid, 5*time.Second); err != nil {
			t.Fatalf("warm-up DrainAndClose: %v", err)
		}
		// Brief settle window for the warm-up's per-call worker to fully
		// exit before baseline sampling.
		time.Sleep(200 * time.Millisecond)
	}

	const N = 100
	const settle = 5 * time.Second

	baseline, post := goroutineBaselineDelta(t, settle, func() {
		// Drive 100 distinct CallSid lifecycles through Enqueue +
		// DrainAndClose. The N here mirrors the plan's "100 calls"
		// acceptance criterion. Each iteration is the production-
		// equivalent emit-then-cleanup sequence the bridge runs at
		// markTerminated.
		for i := 0; i < N; i++ {
			callSid := fmt.Sprintf("CAleak%027d", i)
			form := url.Values{}
			form.Set("CallSid", callSid)
			form.Set("AccountSid", "ACtest0123456789abcdef0123456789ab")
			form.Set("CallStatus", "completed")
			form.Set("Timestamp", time.Now().UTC().Format(time.RFC1123Z))
			form.Set("SequenceNumber", "0")
			evt := webhook.CallbackEvent{
				URL:    srv.URL + "/cb",
				Method: http.MethodPost,
				Form:   form,
				Event:  "completed",
			}
			if err := c.Enqueue(callSid, evt); err != nil {
				t.Fatalf("Enqueue %d (%s): %v", i, callSid, err)
			}
			if err := c.DrainAndClose(callSid, 5*time.Second); err != nil {
				t.Fatalf("DrainAndClose %d (%s): %v", i, callSid, err)
			}
		}
	})

	t.Logf("baseline goroutines: %d, post-drain: %d (delta=%d)",
		baseline, post, post-baseline)
	if post > baseline+2 {
		t.Fatalf("after %d calls + %v settle: NumGoroutine()=%d, baseline=%d (delta=%d > 2 — goroutine leak: webhook.StatusClient per-call workers not drained)",
			N, settle, post, baseline, post-baseline)
	}
}

// Deliberate-break verification for TestE2EGoroutineLeak_NumGoroutineReturnsToBaseline
// is documented in the Task 3 commit body — the executor manually skipped
// the DrainAndClose call inside the primary test loop during development,
// observed NumGoroutine drift to baseline+>2, and reverted before commit.
// A self-checking surrogate test was prototyped but proved unreliable
// under -race + t.Parallel scheduling (other parallel tests' goroutine
// churn drowns the +20 signal in 20 iterations); 100 iterations would be
// reliable but doubles the leak test's CI runtime, which the plan caps at
// 25s. The manual one-shot verification is the same shape used by the
// race + shutdown tests in this package.
