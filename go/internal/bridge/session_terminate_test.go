package bridge

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestSession builds a CallSession with a stub-BYE Leg, no SIP/WS state, no
// goroutines. Sufficient for exercising Terminate/IsActive/markTerminated in
// isolation. byeCount, when non-nil, is incremented on every BYE invocation.
// byeErr controls the error returned by the stub BYE (default nil).
//
// IMPORTANT: tests that mutate state.Load() must use the returned session
// directly (the helper sets state to StateStreaming so IsActive() returns true
// out of the box). Tests that pass a non-nil ctx-cancel observer can still
// observe ctx.cancel via the cancel field — Terminate calls s.cancel if non-nil.
func newTestSession(byeCount *atomic.Int32, byeErr error) *CallSession {
	leg := &Leg{
		byeFunc: func(ctx context.Context) error {
			if byeCount != nil {
				byeCount.Add(1)
			}
			return byeErr
		},
	}
	s := &CallSession{
		callID:     "callid-test",
		callSid:    "CAtest00000000000000000000000000000",
		accountSid: "ACtest00000000000000000000000000000",
		startTime:  time.Now().UTC(),
		legs:       []*Leg{leg},
	}
	s.state.Store(StateStreaming)
	return s
}

// termReasonOf is a test-only helper that reads the post-termination
// reason via the atomic terminal pointer. Returns "" before
// markTerminated stamps the snapshot.
func termReasonOf(s *CallSession) string {
	if t := s.terminal.Load(); t != nil {
		return t.termReason
	}
	return ""
}

// Test 1: IsActive returns true when state is StateStreaming.
func TestCallSession_IsActive_StreamingTrue(t *testing.T) {
	t.Parallel()
	s := newTestSession(nil, nil)
	// Constructor already sets StateStreaming, but be explicit for clarity.
	s.state.Store(StateStreaming)

	if !s.IsActive() {
		t.Errorf("IsActive() = false for StateStreaming, want true")
	}
}

// Test 2: IsActive returns false when state is StateTerminated.
func TestCallSession_IsActive_TerminatedFalse(t *testing.T) {
	t.Parallel()
	s := newTestSession(nil, nil)
	s.state.Store(StateTerminated)

	if s.IsActive() {
		t.Errorf("IsActive() = true for StateTerminated, want false")
	}
}

// Test 3: IsActive returns false when state is StateHungUp.
func TestCallSession_IsActive_HungUpFalse(t *testing.T) {
	t.Parallel()
	s := newTestSession(nil, nil)
	s.state.Store(StateHungUp)

	if s.IsActive() {
		t.Errorf("IsActive() = true for StateHungUp, want false")
	}
}

// Test 4: markTerminated is idempotent — first call wins on both reason and
// endTime. The sync.Once guard means a second call (with any reason) is a
// no-op at the field-set level.
func TestCallSession_MarkTerminated_Idempotent(t *testing.T) {
	t.Parallel()
	s := newTestSession(nil, nil)

	before := time.Now().UTC()
	s.markTerminated("first")
	firstTerm := s.terminal.Load()
	if firstTerm == nil {
		t.Fatalf("after first markTerminated: terminal pointer should be stamped, got nil")
	}
	firstStamp := firstTerm.endTime
	after := time.Now().UTC()

	if firstTerm.termReason != "first" {
		t.Fatalf("after first markTerminated: termReason=%q, want \"first\"", firstTerm.termReason)
	}
	if firstStamp.IsZero() {
		t.Fatalf("after first markTerminated: endTime should be stamped, got zero value")
	}
	if firstStamp.Before(before) || firstStamp.After(after) {
		t.Errorf("first endTime %v out of range [%v, %v]", firstStamp, before, after)
	}

	// Second call MUST NOT mutate the snapshot — sync.Once swallows the closure.
	// Sleep so that if the closure DID run, the new endTime would be visibly later.
	time.Sleep(2 * time.Millisecond)
	s.markTerminated("second")

	secondTerm := s.terminal.Load()
	if secondTerm.termReason != "first" {
		t.Errorf("after second markTerminated: termReason=%q, want \"first\" (first-write-wins)", secondTerm.termReason)
	}
	if !secondTerm.endTime.Equal(firstStamp) {
		t.Errorf("after second markTerminated: endTime=%v, want unchanged %v (first-write-wins)", secondTerm.endTime, firstStamp)
	}
}

// Test 5: Terminate is idempotent — calling twice with different reasons
// keeps the first reason, advances state to StateTerminated, no panic.
//
// Invariant: the BYE itself is per-leg sync.Once-guarded inside Leg.bye().
// The second Terminate() call still reaches Leg.bye(ctx) but the sync.Once
// swallows the closure, so byeCount stays at 1 across multiple Terminate()
// invocations on the same leg. This is a strictly stronger guarantee than
// the legacy "no state-guard" model — sipgo and Twilio both handle
// duplicate BYE gracefully, but emitting one is cleaner and prevents any
// chance of an out-of-dialog BYE 481 reply firing twice in observability
// streams.
func TestCallSession_Terminate_Idempotent(t *testing.T) {
	t.Parallel()

	var byeCount atomic.Int32
	s := newTestSession(&byeCount, nil)

	// Terminate is callable without an established context — s.cancel is nil
	// here because the test bypasses StartSession/run(). The nil-guard inside
	// Terminate covers this.

	if err := s.Terminate("hangup"); err != nil {
		t.Fatalf("first Terminate: unexpected error: %v", err)
	}
	if termReasonOf(s) != "hangup" {
		t.Errorf("after first Terminate: termReason=%q, want \"hangup\"", termReasonOf(s))
	}
	if s.state.Load() != StateTerminated {
		t.Errorf("after first Terminate: state=%v, want StateTerminated", s.state.Load())
	}
	if s.IsActive() {
		t.Errorf("after first Terminate: IsActive()=true, want false")
	}
	if got := byeCount.Load(); got != 1 {
		t.Errorf("after first Terminate: byeCount=%d, want 1", got)
	}

	// Second Terminate with a different reason must not panic, must not change
	// the stamped reason, and must leave state in StateTerminated.
	if err := s.Terminate("anything"); err != nil {
		t.Fatalf("second Terminate: unexpected error: %v", err)
	}
	if termReasonOf(s) != "hangup" {
		t.Errorf("after second Terminate: termReason=%q, want \"hangup\" (idempotent on reason)", termReasonOf(s))
	}
	if s.state.Load() != StateTerminated {
		t.Errorf("after second Terminate: state=%v, want StateTerminated", s.state.Load())
	}
	// Per-leg sync.Once collapses repeat BYEs to one invocation per leg,
	// regardless of how many times Terminate() runs.
	if got := byeCount.Load(); got != 1 {
		t.Errorf("after second Terminate: byeCount=%d, want 1 (per-leg sync.Once collapses dup BYE)", got)
	}
}

// Test 6: Terminate state CAS behavior — when state is StateStreaming, CAS
// hits and state becomes StateTerminated. When state is some OTHER state
// (e.g. StateForwarding from a dual-leg dial), CAS misses but the BYE
// still goes out and termReason is still stamped.
//
// This documents the "CAS-misses-but-still-terminates" path: callers can
// rely on Terminate doing the right thing regardless of where the state
// machine currently sits, while concurrent IsActive()/Status() readers may
// briefly see the pre-Terminate state until other code paths advance it.
func TestCallSession_Terminate_StateCAS(t *testing.T) {
	t.Parallel()

	t.Run("StreamingHits", func(t *testing.T) {
		t.Parallel()
		var byeCount atomic.Int32
		s := newTestSession(&byeCount, nil)
		s.state.Store(StateStreaming)

		if err := s.Terminate("hangup"); err != nil {
			t.Fatalf("Terminate: %v", err)
		}
		if s.state.Load() != StateTerminated {
			t.Errorf("state after Terminate from StateStreaming: %v, want StateTerminated", s.state.Load())
		}
		if byeCount.Load() != 1 {
			t.Errorf("byeCount=%d, want 1", byeCount.Load())
		}
		if termReasonOf(s) != "hangup" {
			t.Errorf("termReason=%q, want \"hangup\"", termReasonOf(s))
		}
	})

	t.Run("ForwardingTerminates", func(t *testing.T) {
		t.Parallel()
		var byeCount atomic.Int32
		s := newTestSession(&byeCount, nil)
		// Terminate CAS-targets each of {Streaming, DialingOut, Forwarding}
		// → Terminated. This subtest exercises the Forwarding source
		// state — the dual-leg dial flow terminates from StateForwarding
		// when the bridge tears down.
		s.state.Store(StateForwarding)

		if err := s.Terminate("hangup"); err != nil {
			t.Fatalf("Terminate: %v", err)
		}
		// StateForwarding is an explicit CAS-target.
		if s.state.Load() != StateTerminated {
			t.Errorf("state after Terminate from StateForwarding: %v, want StateTerminated (CAS now covers Forwarding)", s.state.Load())
		}
		// BYE still went out.
		if byeCount.Load() != 1 {
			t.Errorf("byeCount=%d, want 1", byeCount.Load())
		}
		// And reason was stamped.
		if termReasonOf(s) != "hangup" {
			t.Errorf("termReason=%q, want \"hangup\" (stamping is unconditional)", termReasonOf(s))
		}
		if s.EndTime().IsZero() {
			t.Errorf("endTime is zero, want stamped")
		}
	})
}

// Test 7: Terminate with primary().dlg == nil AND byeFunc == nil — must not
// panic, must return nil. The nil-guard inside Leg.bye() covers this; this
// test is a regression guard against future refactors that drop the guard.
func TestCallSession_Terminate_NilDialog(t *testing.T) {
	t.Parallel()
	// Build a Leg with neither dlg nor byeFunc set — bye() must return nil.
	leg := &Leg{}
	s := &CallSession{
		callID:     "callid-nildlg",
		callSid:    "CAnildlg0000000000000000000000000000",
		accountSid: "ACnildlg0000000000000000000000000000",
		startTime:  time.Now().UTC(),
		legs:       []*Leg{leg},
	}
	s.state.Store(StateStreaming)

	err := s.Terminate("hangup")
	if err != nil {
		t.Errorf("Terminate with nil dlg + nil byeFunc: err=%v, want nil", err)
	}
	if s.state.Load() != StateTerminated {
		t.Errorf("state after Terminate: %v, want StateTerminated", s.state.Load())
	}
	if termReasonOf(s) != "hangup" {
		t.Errorf("termReason=%q, want \"hangup\"", termReasonOf(s))
	}
}

// Test 8: race-test — 50 goroutines call Terminate("api") concurrently with a
// parallel goroutine calling markTerminated("run-natural"). Asserts:
//   - no panic, no race-detector hit
//   - termReason is one of {"api", "run-natural"} (whichever won the
//     sync.Once race; we cannot pin which one)
//   - state ends as StateTerminated (at least one Terminate caller's CAS hits)
//   - byeCount is in [1, 50] — every Terminate sends a BYE, no state-guard
//     suppression in our chosen implementation
//
// Run the body 5 times (count=5) to stress-trip the race detector against
// slightly different schedules. The outer -race -count=3 multiplies this.
func TestCallSession_Terminate_RaceWithRunExit(t *testing.T) {
	t.Parallel()

	for trial := 0; trial < 5; trial++ {
		var byeCount atomic.Int32
		s := newTestSession(&byeCount, nil)
		s.state.Store(StateStreaming)

		const numTerminators = 50
		var startGate sync.WaitGroup
		startGate.Add(1)

		var wg sync.WaitGroup
		wg.Add(numTerminators + 1)

		// 50 goroutines calling Terminate("api").
		for i := 0; i < numTerminators; i++ {
			go func() {
				defer wg.Done()
				startGate.Wait()
				_ = s.Terminate("api")
			}()
		}

		// One goroutine calling markTerminated("run-natural") concurrently —
		// simulates run()'s defer chain stamping "completed" on natural exit
		// while a REST handler is also calling Terminate.
		go func() {
			defer wg.Done()
			startGate.Wait()
			s.markTerminated("run-natural")
		}()

		// Release all goroutines at once.
		startGate.Done()
		wg.Wait()

		// Post-quiescence assertions.
		if termReasonOf(s) != "api" && termReasonOf(s) != "run-natural" {
			t.Errorf("trial %d: termReason=%q, want one of {\"api\", \"run-natural\"}", trial, termReasonOf(s))
		}
		if s.state.Load() != StateTerminated {
			t.Errorf("trial %d: state=%v, want StateTerminated (at least one CAS must have hit)", trial, s.state.Load())
		}
		if got := byeCount.Load(); got < 1 || got > numTerminators {
			t.Errorf("trial %d: byeCount=%d, want in [1, %d]", trial, got, numTerminators)
		}
		// endTime must be stamped (sync.Once ran exactly once).
		if s.EndTime().IsZero() {
			t.Errorf("trial %d: endTime is zero, want stamped exactly once", trial)
		}
		// IsActive must be false (state advanced to terminated).
		if s.IsActive() {
			t.Errorf("trial %d: IsActive()=true, want false post-Terminate", trial)
		}
	}
}

// Bonus regression: Terminate forwards the BYE error from the stub. Confirms
// callers can rely on the returned error to be the BYE error (not a wrapped
// or swallowed one).
func TestCallSession_Terminate_PropagatesBYEError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("simulated bye failure")

	var byeCount atomic.Int32
	s := newTestSession(&byeCount, wantErr)

	err := s.Terminate("hangup")
	if !errors.Is(err, wantErr) {
		t.Errorf("Terminate returned err=%v, want %v", err, wantErr)
	}
	// Stamping and state still happened — BYE error does NOT abort the
	// idempotent stamping path.
	if termReasonOf(s) != "hangup" {
		t.Errorf("termReason=%q despite BYE error, want \"hangup\"", termReasonOf(s))
	}
	if s.state.Load() != StateTerminated {
		t.Errorf("state=%v despite BYE error, want StateTerminated", s.state.Load())
	}
}
