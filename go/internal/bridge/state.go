package bridge

import "sync/atomic"

// State enumerates the high-level phases of a CallSession.
//
// The enum drives REST API state reporting and the B2BUA <Dial> dual-leg
// state machine.
//
// Zero value is StateDispatching, which matches a session that has just been
// created and has not yet started streaming media.
//
// Dual-leg state machine:
//
//	Dispatching ─dispatcher attaches WS────────────> Streaming
//	Streaming   ─CloseStream("dial-forward") ──────> DialingOut
//	DialingOut  ─SetLeg(1, calleeLeg) + state.CAS ─> Forwarding
//	Forwarding  ─Terminate(reason) ────────────────> Terminated
//	Streaming   ─Terminate(reason) ────────────────> Terminated  (existing single-leg path)
//
// CAS-based transitions: a failed CAS is non-fatal (the calling code observes
// the current state and either retries or aborts). State enum members are
// append-only — never reorder or remove a value, since AtomicState stores
// the underlying int32.
//
// StateForwardingSetup (legacy reservation) is preserved for backward
// compatibility but is not used by the current forwarder; the dual-leg
// path uses StateDialingOut as the explicit "WS torn down, INVITE pending"
// intermediate phase.
type State int32

const (
	StateDispatching State = iota
	StateStreaming
	StateForwardingSetup
	StateForwarding
	StateRedirected
	StateHungUp
	StateTerminated
	// StateDialingOut is the transient state between
	// Streaming → Forwarding. It is entered by CloseStream("dial-forward")
	// after the WS goroutines have drained, and exited by SetLeg(1, calleeLeg)
	// + state.CAS(DialingOut, Forwarding) once the outbound 200 OK arrives
	// and the callee leg is wired. RTP goroutines stay live through this
	// transient state so port-pool churn is avoided. Appended to the end of
	// the enum to preserve int32 backward compatibility.
	StateDialingOut
)

func (s State) String() string {
	switch s {
	case StateDispatching:
		return "dispatching"
	case StateStreaming:
		return "streaming"
	case StateForwardingSetup:
		return "forwarding-setup"
	case StateForwarding:
		return "forwarding"
	case StateRedirected:
		return "redirected"
	case StateHungUp:
		return "hung-up"
	case StateTerminated:
		return "terminated"
	case StateDialingOut:
		return "dialing-out"
	default:
		return "unknown"
	}
}

// AtomicState supports concurrent state reads (HTTP /health, REST /Calls).
type AtomicState struct{ v atomic.Int32 }

func (a *AtomicState) Load() State             { return State(a.v.Load()) }
func (a *AtomicState) Store(s State)           { a.v.Store(int32(s)) }
func (a *AtomicState) CAS(old, new State) bool { return a.v.CompareAndSwap(int32(old), int32(new)) }
