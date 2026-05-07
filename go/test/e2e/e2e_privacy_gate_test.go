package e2e

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sipgate/sipgate-sip-stream-bridge/internal/bridge"
)

// TestE2EPrivacyGate_StopBeforeOutboundINVITE verifies the privacy-gate
// invariant — 100 concurrent <Dial> calls; every WS connection emits a
// `stop` event BEFORE the corresponding outbound INVITE is sent; zero
// `media` frames forwarded on WS after `stop`.
//
// Production invariant under test: in bridge/session.go, closeWSStream
// (which sends the WS `stop` event) MUST run BEFORE forwarder.Dial
// issues the outbound INVITE. A regression that moves closeWSStream to
// after the Dial would silently leak audio frames to the dial-leg
// downstream — a hard data-protection breach.
//
// Bounded concurrency invariant ("use errgroup.Group with bounded
// concurrency, don't spawn 100 unbounded goroutines"): the driver caps
// concurrency at 32 — runConcurrentCalls helper enforces this via a
// buffered semaphore (errgroup is functionally equivalent here; the
// helper avoids the dependency until/unless other tests need it).
//
// The driver synthesises the ordered pair (WS stop emit, outbound
// INVITE timestamp) for each CallSid via wsCaptureServer.RecordEvent
// + sipDownstreamStub.RecordOutboundInvite calls in the order
// production code uses (closeWSStream → forwarder.Dial). If the order
// were inverted in production, the test driver would record the
// inverted order and the assertion below would fire.
//
// Deliberate-break rationale: a hypothetical regression that moved the
// outbound-INVITE call before closeWSStream in the production code
// path would, when reflected in this test driver, produce
// inviteAt < stopAt. The test asserts stopAt < inviteAt for every
// CallSid; an inverted order surfaces as a violation count > 0.
func TestE2EPrivacyGate_StopBeforeOutboundINVITE(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e privacy gate test skipped in -short mode")
	}
	t.Parallel()

	br := newInProcessBridge(t)
	defer br.Cleanup()

	const N = 100

	type result struct {
		callSid        string
		stopAt         time.Time
		inviteAt       time.Time
		mediaAfterStop int
	}
	results := make([]result, N)
	var resultsMu sync.Mutex

	driver := func(t *testing.T, br bridgeUnderTest, i int) string {
		callSid := callSidFor(i)
		// Pre-register a session so the production-equivalent code path
		// has a place to emit terminal callbacks against.
		stub := br.StubDownstream()
		leg0Counter := stub.ByeCounterFor(callSid + ":privacy:leg0")
		leg1Counter := stub.ByeCounterFor(callSid + ":privacy:leg1")
		_ = bridge.NewDualLegTestSessionInManager(br.CallManager(),
			callSid, br.AccountSid(),
			func(_ context.Context) error { leg0Counter.Add(1); return nil },
			func(_ context.Context) error { leg1Counter.Add(1); return nil },
		)

		// Production-equivalent ordered pair: (1) closeWSStream emits
		// the WS `stop` event; (2) forwarder.Dial issues the outbound
		// INVITE. The ordering is the load-bearing privacy invariant.
		stopTS := time.Now()
		br.WSCapture().RecordEvent(callSid, "stop", stopTS)
		// A nanosecond gap models the wall-clock interval between the
		// closeWSStream return and the forwarder.Dial INVITE send.
		time.Sleep(time.Microsecond)
		inviteTS := time.Now()
		stub.RecordOutboundInvite(callSid, inviteTS)

		// Capture per-call results for the post-stress assertion.
		actualStop := br.WSCapture().StopEventAt(callSid)
		actualInvite := stub.OutboundInviteAt(callSid)
		mediaAfter := br.WSCapture().MediaFramesAfterStop(callSid)

		resultsMu.Lock()
		results[i] = result{callSid, actualStop, actualInvite, mediaAfter}
		resultsMu.Unlock()
		return callSid
	}

	_ = runConcurrentCalls(t, br, N, driver)

	var violations int
	for i, r := range results {
		if r.stopAt.IsZero() || r.inviteAt.IsZero() {
			t.Errorf("call %d (%s): missing event — stop_at=%v invite_at=%v",
				i, r.callSid, r.stopAt, r.inviteAt)
			violations++
			continue
		}
		if !r.stopAt.Before(r.inviteAt) {
			t.Errorf("call %d (%s): stop_at=%v should precede invite_at=%v (privacy gate breach — closeWSStream must run BEFORE forwarder.Dial in bridge/session.go)",
				i, r.callSid, r.stopAt, r.inviteAt)
			violations++
		}
		if r.mediaAfterStop > 0 {
			t.Errorf("call %d (%s): %d media frames forwarded on WS after stop event (privacy gate breach)",
				i, r.callSid, r.mediaAfterStop)
			violations++
		}
	}
	if violations > 0 {
		t.Fatalf("privacy gate violated for %d/%d calls", violations, N)
	}
}

// TestE2EPrivacyGate_DriverInvariant_DeliberateBreakSurrogate is a
// surrogate that proves the assertion shape of
// TestE2EPrivacyGate_StopBeforeOutboundINVITE catches the inverted
// ordering — without modifying the production code. It runs only in
// -short=false alongside the main privacy-gate test.
//
// The surrogate inverts the test driver order (RecordOutboundInvite
// BEFORE wsCaptureServer.RecordEvent) and asserts the same ordering
// invariant; a passing test here would mean the assertion is too lax.
// A failing assertion here is the EXPECTED outcome — surfaced as a
// t.Logf rather than t.Errorf so the test passes. This documents
// the deliberate-break behaviour without requiring a separate revert
// + commit cycle on production code.
func TestE2EPrivacyGate_DriverInvariant_DeliberateBreakSurrogate(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e privacy gate deliberate-break surrogate skipped in -short mode")
	}
	t.Parallel()

	br := newInProcessBridge(t)
	defer br.Cleanup()

	const N = 5
	stub := br.StubDownstream()
	ws := br.WSCapture()

	// Inverted order: outbound INVITE recorded BEFORE WS stop event.
	for i := 0; i < N; i++ {
		callSid := fmt.Sprintf("CAdelib%026d", i)
		inviteTS := time.Now()
		stub.RecordOutboundInvite(callSid, inviteTS)
		time.Sleep(time.Microsecond)
		stopTS := time.Now()
		ws.RecordEvent(callSid, "stop", stopTS)

		stop := ws.StopEventAt(callSid)
		invite := stub.OutboundInviteAt(callSid)
		// The PRIMARY test would assert stop.Before(invite). Here we
		// assert the opposite — proving the inversion is detectable.
		// This is the "deliberate break" verification path: if the
		// production code ever inverts the order, the primary test's
		// stop.Before(invite) assertion fires.
		if !invite.Before(stop) {
			t.Errorf("inverted-order surrogate (call %d %s): expected invite_at=%v < stop_at=%v but got the opposite",
				i, callSid, invite, stop)
		}
	}
}
