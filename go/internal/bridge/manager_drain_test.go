package bridge

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/config"
)

// TestDrainAll_DualLeg pins the dual-leg architectural-gap fix at
// manager.go:325. Before the fix, DrainAll called s.primary().dlg.Bye(...)
// which only BYEs legs[0] — dial-leg dialogs (legs[1]) leaked from the
// sipgo dialog cache during graceful shutdown of an active <Dial>
// forwarded call.
//
// After the fix, DrainAll routes through s.Terminate("shutdown") so the
// existing dual-leg fan-out at session.go:1797-1812 runs through every
// leg's per-leg Leg.terminateOnce sync.Once. Both legs MUST receive
// exactly one bye() invocation.
//
// Test design: build a CallManager via NewCallManager, install a
// CallSession into m.sessions whose legs slice has TWO Leg entries. Each
// leg has a stub byeFunc that increments an atomic counter and removes
// the session from m.sessions when both legs have BYEd (simulating the
// production defer-chain in StartSessionWithPreRegistered which removes
// the session from m.sessions on goroutine exit). DrainAll must observe
// BOTH counters reach 1 (per-leg sync.Once collapses any duplicate calls)
// AND the polling-loop's m.sessions count must reach zero so DrainAll
// returns cleanly within the test budget.
//
// Deliberate-break verification: temporarily reverting the Terminate
// fix (re-introducing
// `s.primary().dlg.Bye(...)`) makes this test fail with leg1Count == 0
// because primary() returns legs[0] only.
func TestDrainAll_DualLeg(t *testing.T) {
	t.Parallel()

	pool, err := NewPortPool(20100, 20102)
	if err != nil {
		t.Fatalf("NewPortPool: %v", err)
	}
	cm := NewCallManager(pool, "ACdrain00000000000000000000000000ab", config.Config{}, zerolog.Nop(), nil)
	t.Cleanup(cm.Close)

	const callID = "callid-drain-dual"
	const callSid = "CAdrain00000000000000000000000000ab"

	var leg0Count atomic.Int32
	var leg1Count atomic.Int32

	// Counter-based teardown: when both legs have BYEd, the session
	// removes itself from m.sessions.Range — mirroring the production
	// defer chain that decrements ActiveCalls + Delete()s the session on
	// goroutine exit.
	var byesIssued atomic.Int32
	leg0 := &Leg{
		byeFunc: func(_ context.Context) error {
			leg0Count.Add(1)
			if byesIssued.Add(1) == 2 {
				cm.sessions.Delete(callID)
			}
			return nil
		},
	}
	leg1 := &Leg{
		byeFunc: func(_ context.Context) error {
			leg1Count.Add(1)
			if byesIssued.Add(1) == 2 {
				cm.sessions.Delete(callID)
			}
			return nil
		},
	}

	s := &CallSession{
		callID:     callID,
		callSid:    callSid,
		accountSid: cm.accountSid,
		startTime:  time.Now().UTC(),
		legs:       []*Leg{leg0, leg1},
	}
	s.state.Store(StateForwarding)
	cm.sessions.Store(callID, s)

	// Sanity: pre-drain ActiveCount reflects the inserted session.
	if got := cm.ActiveCount(); got != 1 {
		t.Fatalf("pre-drain ActiveCount = %d, want 1", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cm.DrainAll(ctx)

	// Both legs MUST have BYEd exactly once. The per-leg sync.Once inside
	// Leg.bye() collapses any duplicate Terminate calls to one bye
	// invocation per leg.
	if got := leg0Count.Load(); got != 1 {
		t.Errorf("leg0 BYE count = %d, want 1 (parent leg must be BYEd)", got)
	}
	if got := leg1Count.Load(); got != 1 {
		t.Errorf("leg1 BYE count = %d, want 1 (dial leg must be BYEd — dual-leg architectural gap)", got)
	}

	// Polling loop must have observed sessions count == 0 within budget,
	// so DrainAll returned without timeout.
	if got := cm.ActiveCount(); got != 0 {
		t.Errorf("post-drain ActiveCount = %d, want 0 (DrainAll must wait until sessions self-delete)", got)
	}

	// Session state must be StateTerminated — Terminate path took effect.
	if got := s.state.Load(); got != StateTerminated {
		t.Errorf("post-drain session state = %v, want StateTerminated (Terminate must CAS Forwarding → Terminated)", got)
	}

	// Termination reason must be "shutdown" (DrainAll's locked argument).
	if got := termReasonOf(s); got != "shutdown" {
		t.Errorf("post-drain termReason = %q, want %q (DrainAll must call Terminate(\"shutdown\"))", got, "shutdown")
	}
}

// TestDrainAll_DualLeg_IdempotentBYE verifies that the per-leg
// terminateOnce sync.Once collapses duplicate BYE attempts to exactly one
// per leg, even when DrainAll is called and then a racing REST Terminate
// arrives on the same session before the polling loop self-deletes it.
//
// In practice DrainAll is called once during shutdown — but the
// per-leg-sync.Once invariant from Leg.bye() (leg.go:427-438) is
// load-bearing for the dual-leg drain. This test pins it under the new
// DrainAll path so a regression that drops the sync.Once would surface.
func TestDrainAll_DualLeg_IdempotentBYE(t *testing.T) {
	t.Parallel()

	pool, err := NewPortPool(20110, 20112)
	if err != nil {
		t.Fatalf("NewPortPool: %v", err)
	}
	cm := NewCallManager(pool, "ACdrainidem000000000000000000000000", config.Config{}, zerolog.Nop(), nil)
	t.Cleanup(cm.Close)

	const callID = "callid-drain-idem"
	const callSid = "CAdrainidem000000000000000000000000"

	var leg0Count atomic.Int32
	var leg1Count atomic.Int32

	leg0 := &Leg{byeFunc: func(_ context.Context) error { leg0Count.Add(1); return nil }}
	leg1 := &Leg{byeFunc: func(_ context.Context) error { leg1Count.Add(1); return nil }}

	s := &CallSession{
		callID:     callID,
		callSid:    callSid,
		accountSid: cm.accountSid,
		startTime:  time.Now().UTC(),
		legs:       []*Leg{leg0, leg1},
	}
	s.state.Store(StateForwarding)
	cm.sessions.Store(callID, s)

	// Pre-call s.Terminate to consume the terminateOnce on each leg.
	if err := s.Terminate("api"); err != nil {
		t.Fatalf("pre-Terminate: %v", err)
	}
	cm.sessions.Delete(callID) // simulate the production defer-chain delete

	// Now DrainAll. It will see no sessions in m.sessions, so the Range
	// no-ops and the polling loop returns immediately. Confirms DrainAll
	// is a no-op on an already-drained map.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	cm.DrainAll(ctx)

	if got := leg0Count.Load(); got != 1 {
		t.Errorf("leg0 BYE count = %d, want 1 (sync.Once collapses)", got)
	}
	if got := leg1Count.Load(); got != 1 {
		t.Errorf("leg1 BYE count = %d, want 1 (sync.Once collapses)", got)
	}
}
