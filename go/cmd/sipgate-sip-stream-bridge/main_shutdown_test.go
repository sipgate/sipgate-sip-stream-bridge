package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// ── shutdown order-of-operations tests ─────────────────────────────────────
//
// Pin the shutdown sequence: handler.SetShutdown → httpServer.Shutdown
// (stop accepting + finish in-flight) → callManager.DrainAll(drainBudget=15s)
// → registrar.Unregister. Total wall-clock budget: 5s + 15s + 5s = 25s ≤
// K8s 30s terminationGracePeriodSeconds.
//
// The previous order (HTTP-shutdown LAST, after UNREGISTER) violated the
// "stop accepting new HTTP → finish in-flight HTTP → DrainAll → UNREGISTER"
// invariant. The risk: a racing modify-call REST handler could reach a
// half-drained CallManager.callSidIdx during shutdown and trigger spurious
// 21220 errors or, worse, panic on a newly-deleted *CallSession.
//
// Test harness: spawn fakes (fakeShutdownHandler, fakeShutdownHTTPServer,
// fakeShutdownCallManager, fakeShutdownRegistrar) each recording a
// monotonic wall-clock timestamp via time.Now() when its corresponding
// method is called. runShutdown is the helper extracted from main()'s
// shutdown block; the test asserts httpShutTS < drainTS < unregTS.
//
// fakeShutdownCallManager.DrainAll sleeps 50ms to make timing assertions
// deterministic against monotonic time-source granularity (sub-microsecond
// on linux/darwin; the sleep adds slack so the strictly-sequential code
// path holds even on a hypothetically clock-jittered runner).

type fakeShutdownHandler struct {
	mu             sync.Mutex
	shutdownCalled bool
	shutdownAt     time.Time
}

func (f *fakeShutdownHandler) SetShutdown() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shutdownCalled = true
	f.shutdownAt = time.Now()
}

type fakeShutdownHTTPServer struct {
	mu          sync.Mutex
	shutdownErr error
	shutdownAt  time.Time
}

func (f *fakeShutdownHTTPServer) Shutdown(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shutdownAt = time.Now()
	return f.shutdownErr
}

type fakeShutdownCallManager struct {
	mu       sync.Mutex
	drainAt  time.Time
	drainCtx context.Context
	active   int32 // mutated via atomic.Int32 for ActiveCount; kept simple here
}

func (f *fakeShutdownCallManager) DrainAll(ctx context.Context) {
	f.mu.Lock()
	f.drainAt = time.Now()
	f.drainCtx = ctx
	f.mu.Unlock()
	// 50ms simulated drain to make timing assertions deterministic vs the
	// monotonic clock. The strict order is sequential-by-construction
	// (runShutdown calls these in a fixed order) — the sleep is just slack.
	time.Sleep(50 * time.Millisecond)
}

func (f *fakeShutdownCallManager) ActiveCount() int { return int(atomic.LoadInt32(&f.active)) }

type fakeShutdownRegistrar struct {
	mu          sync.Mutex
	unregErr    error
	unregAt     time.Time
	unregCalled bool
}

func (f *fakeShutdownRegistrar) Unregister(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unregCalled = true
	f.unregAt = time.Now()
	return f.unregErr
}

// TestShutdown_Order_HTTPStopBeforeDrainBeforeUnregister pins the new
// sequence: SetShutdown → httpServer.Shutdown → DrainAll → Unregister.
// Asserts:
//  1. all four methods are called exactly once
//  2. httpShutdownAt < drainAt < unregAt
//  3. SetShutdown happens before httpServer.Shutdown (rejecting new INVITEs
//     before stopping HTTP listener so the in-flight REST modify can't
//     touch a session whose state is about to advance)
//  4. drainCtx has a deadline of drainBudget (15s) — the const must be
//     hoisted to package scope and threaded into runShutdown
func TestShutdown_Order_HTTPStopBeforeDrainBeforeUnregister(t *testing.T) {
	t.Parallel()

	h := &fakeShutdownHandler{}
	hs := &fakeShutdownHTTPServer{}
	cm := &fakeShutdownCallManager{}
	r := &fakeShutdownRegistrar{}

	// Production runShutdown is called AFTER <-ctx.Done() returns — i.e. the
	// signal-bearing context is already cancelled. Mirror that in the test
	// so runShutdown's logger.Info().Str("signal", ctx.Err().Error()) sees
	// a non-nil ctx.Err() (signal.NotifyContext yields context.Canceled on
	// SIGTERM).
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	logger := zerolog.Nop()
	runShutdown(cancelledCtx, h, hs, cm, r, logger)

	// 1. All four methods called.
	if !h.shutdownCalled {
		t.Errorf("handler.SetShutdown was not called")
	}
	if hs.shutdownAt.IsZero() {
		t.Errorf("httpServer.Shutdown was not called")
	}
	if cm.drainAt.IsZero() {
		t.Errorf("callManager.DrainAll was not called")
	}
	if !r.unregCalled {
		t.Errorf("registrar.Unregister was not called")
	}

	// 2 + 3. Order: SetShutdown < httpServer.Shutdown < DrainAll < Unregister.
	if !h.shutdownAt.Before(hs.shutdownAt) {
		t.Errorf("SetShutdown (%v) must precede httpServer.Shutdown (%v)", h.shutdownAt, hs.shutdownAt)
	}
	if !hs.shutdownAt.Before(cm.drainAt) {
		t.Errorf("httpServer.Shutdown (%v) must precede DrainAll (%v) — locked HTTP-stop-before-drain ordering", hs.shutdownAt, cm.drainAt)
	}
	if !cm.drainAt.Before(r.unregAt) {
		t.Errorf("DrainAll (%v) must precede Unregister (%v) — locked drain-before-unreg ordering", cm.drainAt, r.unregAt)
	}

	// 4. drainCtx must have ~drainBudget deadline (15s); allow generous
	// slack against scheduling jitter. Use the package-level constant
	// drainBudget so a future tweak auto-propagates here.
	deadline, ok := cm.drainCtx.Deadline()
	if !ok {
		t.Fatalf("DrainAll ctx has no deadline; expected drainBudget=%v", drainBudget)
	}
	remaining := time.Until(deadline)
	// Deadline was set right before DrainAll; the ~50ms sleep + drainCtx
	// inheriting from background means remaining ~ drainBudget - 50ms.
	if remaining > drainBudget+100*time.Millisecond || remaining < drainBudget-500*time.Millisecond {
		t.Errorf("DrainAll ctx remaining = %v, want ~%v (drainBudget) ± slack", remaining, drainBudget)
	}
}

// TestShutdown_DrainBudget_Const pins the locked invariant: drainBudget
// MUST be a named const in this package equal to 15*time.Second. Future
// refactors that bury the literal somewhere else, or quietly tweak it,
// fail this test in the PR diff.
//
// Decision: drain budget is a hard-coded const drainBudget = 15 * time.Second
// in cmd/sipgate-sip-stream-bridge/main.go. NOT exposed as env var
// (operators have no good reason to tune it; K8s 30s budget is fixed).
func TestShutdown_DrainBudget_Const(t *testing.T) {
	t.Parallel()
	if drainBudget != 15*time.Second {
		t.Errorf("drainBudget = %v, want 15s", drainBudget)
	}
}

// TestShutdown_HTTPShutdownErrorDoesNotAbort confirms that an HTTP
// shutdown error (e.g. an in-flight Url= TwiML fetch exceeding the 5s
// httpShutCtx) does NOT abort the rest of the sequence — DrainAll +
// Unregister must still run so the K8s 30s grace doesn't expire on the
// SIP side.
func TestShutdown_HTTPShutdownErrorDoesNotAbort(t *testing.T) {
	t.Parallel()

	h := &fakeShutdownHandler{}
	hs := &fakeShutdownHTTPServer{shutdownErr: context.DeadlineExceeded}
	cm := &fakeShutdownCallManager{}
	r := &fakeShutdownRegistrar{}

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	runShutdown(cancelledCtx, h, hs, cm, r, zerolog.Nop())

	if cm.drainAt.IsZero() {
		t.Errorf("DrainAll did not run after httpServer.Shutdown error — sequence must continue")
	}
	if !r.unregCalled {
		t.Errorf("Unregister did not run after httpServer.Shutdown error — sequence must continue")
	}
}

// TestShutdown_UnregisterErrorDoesNotPanic confirms that an UNREGISTER
// error does not panic the shutdown handler. The runShutdown helper logs
// the error and returns cleanly so deferred cleanups (agent.UA.Close,
// callManager.Close) get to run.
func TestShutdown_UnregisterErrorDoesNotPanic(t *testing.T) {
	t.Parallel()

	h := &fakeShutdownHandler{}
	hs := &fakeShutdownHTTPServer{}
	cm := &fakeShutdownCallManager{}
	r := &fakeShutdownRegistrar{unregErr: context.DeadlineExceeded}

	defer func() {
		if rec := recover(); rec != nil {
			t.Errorf("runShutdown panicked on Unregister error: %v", rec)
		}
	}()

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	runShutdown(cancelledCtx, h, hs, cm, r, zerolog.Nop())
}
