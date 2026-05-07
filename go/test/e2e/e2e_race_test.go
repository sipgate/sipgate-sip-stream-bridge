package e2e

import (
	"context"
	"sync"
	"testing"

	"github.com/sipgate/sipgate-sip-stream-bridge/internal/bridge"
)

// TestE2ERace_CancelByeSimultaneous verifies the CANCEL/BYE race
// guarantee: CANCEL/BYE arriving simultaneously on the same CallSession
// never produce double-BYE or panic. Two layers of sync.Once collapse
// the race:
//
//   1. CallSession.terminateOnce (session.go:178) — guards markTerminated
//      so the terminal-callback emission + state stamp run exactly once.
//   2. Per-Leg terminateOnce (leg.go:67) — guards each leg.bye(ctx)
//      invocation so dlg.Bye / byeFunc runs exactly once per leg even
//      under N concurrent Terminate() calls.
//
// Pattern from go/internal/bridge/dual_leg_test.go:272-315: 5-trial loop
// × N concurrent goroutines releasing simultaneously via startGate so
// the race window is hit at least once per CI run.
//
// Must run under -race so the race detector also surfaces any latent
// data race (the sync.Once guards are correctness-critical, not just
// behavioural — a release without -race wouldn't catch a hypothetical
// regression that uses an unsync map for legs).
func TestE2ERace_CancelByeSimultaneous(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e race test skipped in -short mode")
	}
	t.Parallel()

	br := newInProcessBridge(t)
	defer br.Cleanup()

	cm := br.CallManager()
	stub := br.StubDownstream()

	const trials = 5
	const concurrency = 32

	for trial := 0; trial < trials; trial++ {
		callSid := callSidFor(trial)
		leg0Counter := stub.ByeCounterFor(callSid + ":leg0")
		leg1Counter := stub.ByeCounterFor(callSid + ":leg1")
		sess := bridge.NewDualLegTestSessionInManager(cm,
			callSid, br.AccountSid(),
			func(_ context.Context) error { leg0Counter.Add(1); return nil },
			func(_ context.Context) error { leg1Counter.Add(1); return nil },
		)

		// startGate idiom from dual_leg_test.go:272-315: every goroutine
		// blocks on Wait() until the main goroutine fires Done(),
		// releasing all concurrent terminators simultaneously and
		// maximising the chance of hitting the race window.
		var startGate sync.WaitGroup
		startGate.Add(1)
		var wg sync.WaitGroup
		wg.Add(concurrency)
		for i := 0; i < concurrency; i++ {
			i := i
			go func() {
				defer wg.Done()
				startGate.Wait()
				// Half SIP CANCEL surrogate (sess.Terminate); half REST
				// modify-call hangup surrogate (also sess.Terminate via
				// midCallAdapter in production). Both go through the
				// same Terminate code path — the production CANCEL/BYE
				// race is whatever path Terminate winds up calling.
				_ = sess.Terminate("test-trial-" + string(rune('0'+i)))
			}()
		}
		startGate.Done()
		wg.Wait()

		// Verify EXACTLY one BYE per leg even after 32 concurrent
		// Terminate calls — the per-leg sync.Once must collapse the
		// race. A regression that weakens terminateOnce (by removing
		// the once.Do guard, for example) would surface as count > 1
		// here.
		if got := stub.ByeCountFor(callSid + ":leg0"); got != 1 {
			t.Errorf("trial %d: leg0 BYE count = %d, want 1 (per-leg sync.Once should collapse races)",
				trial, got)
		}
		if got := stub.ByeCountFor(callSid + ":leg1"); got != 1 {
			t.Errorf("trial %d: leg1 BYE count = %d, want 1 (per-leg sync.Once should collapse races on legs[1] too)",
				trial, got)
		}
		// Belt-and-braces: session is terminal post-trial.
		if sess.IsActive() {
			t.Errorf("trial %d: session still IsActive() after %d concurrent Terminate calls",
				trial, concurrency)
		}
	}
}
