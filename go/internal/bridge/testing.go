package bridge

import (
	"context"
	"time"
)

// NewTestSession constructs a *CallSession suitable for cross-package tests
// (notably internal/api/midcall_adapter_test.go) that need to exercise the
// public Terminate / IsActive / Status / CallSid / AccountSid surface without
// importing internal SIP / RTP / WS state.
//
// The session is wired with a single Leg whose BYE is replaced by byeFunc
// (callable with any context.Context); production code never goes through
// this helper. callSid and accountSid populate the corresponding fields so
// midCallAdapter's logger enrichment is exercised end-to-end.
//
// state is initialized to StateStreaming so IsActive() returns true on a
// freshly-constructed instance — callers can then drive Terminate("…") and
// observe the state advance to StateTerminated, mirroring the production
// REST POST /Calls/{Sid}.json flow.
//
// Production should NEVER call this constructor — there is no INVITE, no
// SDP, no port acquisition, no WS connection. The function exists purely so
// internal/api can build CallSession fixtures without sharing test internals
// across the package boundary (Go does not allow exporting unexported
// helpers from _test.go files to other packages).
func NewTestSession(callSid, accountSid string, byeFunc func(ctx context.Context) error) *CallSession {
	leg := &Leg{
		byeFunc: byeFunc,
	}
	s := &CallSession{
		callID:     "test-callid-" + callSid,
		callSid:    callSid,
		accountSid: accountSid,
		startTime:  time.Now().UTC(),
		legs:       []*Leg{leg},
	}
	s.state.Store(StateStreaming)
	return s
}

// MarkTestTerminated exposes the unexported markTerminated helper for
// cross-package tests that need to construct a session in the terminal state
// without going through Terminate (which also issues a BYE). Callers that
// want a session whose IsActive() returns false but whose endTime / status
// reflect a specific reason use this entry point.
//
// Production should NEVER call this; it bypasses the BYE side-effect that
// real terminations require. The wrapper exists solely for unit-test
// determinism inside other packages.
func MarkTestTerminated(s *CallSession, reason string) {
	s.markTerminated(reason)
	s.state.Store(StateTerminated)
}

// AddSessionInStateForTest stores a freshly-built CallSession in
// CallManager.sessions (the same map ActiveCount + ActiveForwardCount range
// over) with a specific state. Used by /health contract tests
// (cmd/sipgate-sip-stream-bridge/main_test.go) so a real CallManager can be
// populated with a known mix of states without driving a real B2BUA <Dial>
// sequence.
//
// Production should NEVER call this — there is no INVITE, no SDP, no port
// acquisition, no WS connection, no goroutine. The function exists purely so
// the cmd-package contract test can build CallManager fixtures without
// sharing test internals across the package boundary (Go does not allow
// exporting helpers from _test.go files to other packages).
func AddSessionInStateForTest(cm *CallManager, callID, callSid string, st State) *CallSession {
	s := &CallSession{
		callID:     callID,
		callSid:    callSid,
		accountSid: cm.accountSid,
		startTime:  time.Now().UTC(),
	}
	s.state.Store(st)
	cm.sessions.Store(callID, s)
	return s
}

// NewDualLegTestSessionInManager constructs a *CallSession with TWO legs each
// wired to its own byeFunc, registers the session into the supplied
// CallManager (both sessions and callSidIdx), and returns the session so
// callers can drive Terminate("…") / DrainAll(…) and observe per-leg BYE
// counters.
//
// Used by go/test/e2e/ tests:
//   - TestE2EShutdown_DualLegDrain populates the manager with N dual-leg
//     sessions, then calls DrainAll, asserting BOTH legs of each session
//     received exactly one BYE within the drain budget.
//   - TestE2ERace_CancelByeSimultaneous drives concurrent Terminate()
//     calls against the session and asserts byeCount == 1 per leg
//     (terminateOnce + per-leg sync.Once collapse races).
//
// Production code MUST NEVER use this constructor — it bypasses port
// acquisition, SDP negotiation, WS dial, RTP socket setup, and the real
// dlg.Bye transaction. It exists solely so the e2e harness can drive the
// real CallManager.DrainAll dual-leg discipline
// (s.Terminate("shutdown") replacing the legacy primary().dlg.Bye())
// without spinning up a sipgo agent.
//
// Idempotent on the same callSid+manager: re-calling overwrites the existing
// entry (same as Store semantics on sync.Map) — callers should not depend
// on this; the API is single-call-per-(cm, callSid).
func NewDualLegTestSessionInManager(
	cm *CallManager,
	callSid, accountSid string,
	leg0Bye, leg1Bye func(ctx context.Context) error,
) *CallSession {
	leg0 := &Leg{byeFunc: leg0Bye}
	leg1 := &Leg{byeFunc: leg1Bye}
	callID := "test-callid-" + callSid
	s := &CallSession{
		callID:     callID,
		callSid:    callSid,
		accountSid: accountSid,
		startTime:  time.Now().UTC(),
		legs:       []*Leg{leg0, leg1},
	}
	s.state.Store(StateForwarding)
	cm.sessions.Store(callID, s)
	cm.callSidIdx.Store(callSid, s)
	return s
}
