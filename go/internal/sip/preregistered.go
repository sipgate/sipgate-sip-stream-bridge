// Package sip — PreRegisteredSession interface declaration.
//
// This file declares the sip-side interface that *bridge.CallSession satisfies.
// The bridge package imports sip (the existing dependency direction in this
// codebase: bridge → sip via internal/sip.LegConfigurer); declaring the
// interface here keeps that direction intact while letting the sip Handler
// consume it without taking a forbidden bridge dependency.
package sip

import "time"

// PreRegisteredSession is the sip-side interface that bridge.CallSession
// satisfies. The bridge package returns one of these from
// CallManagerIface.PreRegisterSession; the sip Handler consumes it to stamp
// per-call SIP-layer telemetry (answered timestamp + SIP final code) AND to
// gate ghost terminal-only callbacks via MarkEmitted.
//
// Architectural invariant (NOT type-system-enforced — convention only):
//
//   - bridge.CallManager.PreRegisterSession is the ONLY function that
//     constructs a value satisfying this interface in this codebase.
//   - bridge.CallManager.StartSessionWithPreRegistered is the ONLY function
//     that consumes one (via internal type-assertion to *bridge.CallSession,
//     documented as panic-if-misuse).
//
// A sealed-marker method would close this with type-system safety but would
// force a sip→bridge import cycle that does not exist today. The convention
// is sufficient: both producer and consumer are in this codebase; no
// untrusted external implementer can sneak in.
type PreRegisteredSession interface {
	// SetAnsweredAt records the wall-clock instant of 200 OK + ACK confirm.
	// First-write-wins via atomic.CompareAndSwap; subsequent calls are no-ops.
	// Used by terminal-event emission to compute CallDuration / Duration.
	SetAnsweredAt(t time.Time)

	// SetSIPFinalCode records the final SIP response code (200, 486, 487,
	// 408, 500, etc.). First non-zero write wins via atomic.CompareAndSwap;
	// calling with code == 0 is a no-op (the CAS slot stays open for the
	// first real code). Used by terminal-event emission to populate
	// SipResponseCode.
	SetSIPFinalCode(code int)

	// CallSid returns the Twilio-style call identifier (CA + 32 hex chars)
	// minted by the SIP handler at INVITE arrival.
	CallSid() string

	// MarkEmitted is called by every emit helper on FIRST successful Enqueue
	// (CompareAndSwap from false to true). Idempotent across multiple emits.
	// Returns true if THIS call was the one that flipped the flag, false if
	// a prior call already did so. Read by PreRegisterSession's cleanup
	// closure to gate ghost terminal-only callbacks (BLOCKER 3 of 16-10
	// revision — the customer is told "the call ended" only if they were
	// first told "the call started").
	MarkEmitted() bool
}
