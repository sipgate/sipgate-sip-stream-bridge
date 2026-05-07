package twiml

import (
	"context"
	"time"

	"github.com/rs/zerolog"
)

// MidCallTarget is the narrow interface that mid-call TwiML verb handlers
// need from the per-call adapter. Minimal surface: Terminate (used by
// <Hangup>) and Log (used by warn-and-skip paths).
//
// Implementations must be safe to call from any goroutine — Dispatch runs
// synchronously on the caller's goroutine, but the underlying CallSession
// termination path may cross goroutines (e.g. cancel ctx then BYE).
type MidCallTarget interface {
	// Terminate ends the call cleanly with the given reason; the bridge's
	// implementation (api.midCallAdapter wrapping *bridge.CallSession) calls
	// session.Terminate(reason) which sends BYE + cleans up. Idempotent —
	// second call is a no-op.
	Terminate(reason string) error

	// Log returns a per-call structured logger for verb-side warn-and-skip
	// entries.
	Log() *zerolog.Logger
}

// DialOpts captures the per-<Dial> knobs derived from TwiML attributes.
// These are mirror types of sip.DialOpts — twiml cannot import sip (twiml is
// a leaf; sip imports bridge which imports sip via CallManagerIface). The
// api-layer adapter (midCallAdapter.PerformDial) translates DialOpts to
// sip.DialOpts before calling Forwarder.Dial.
type DialOpts struct {
	// CallerID is the value from TwiML <Dial callerId="...">.
	CallerID string

	// Timeout is the ring timeout (Twilio default 30s). Zero = use adapter default.
	Timeout time.Duration

	// TimeLimit is the maximum talk time (Twilio default 14400s = 4h). Zero = unlimited.
	TimeLimit time.Duration

	// HangupOnStar: when true and the caller presses '*', the callee leg is BYE'd.
	HangupOnStar bool

	// Action is the post-dial action callback URL (Twilio <Dial action="...">).
	Action string

	// Method is the HTTP method for the action callback (default "POST").
	Method string

	// StatusCallback is the per-call status-callback URL: Number-leg
	// overrides parent-Dial via firstNonEmpty precedence. The lifecycle
	// emit path reads this; empty string = no subscription.
	StatusCallback string

	// StatusCallbackMethod is "POST" | "GET"; empty string defaults to POST
	// at emission time per Twilio convention.
	StatusCallbackMethod string

	// StatusCallbackEvents is the tokenized event-name slice. Empty/nil =
	// no explicit subscription; emission code treats nil as "default"
	// subset (terminal events for parent calls).
	StatusCallbackEvents []string
}

// DialResult is the terminal-state report from a single <Dial> invocation.
// Status values mirror sip.DialResult.Status (set by the Forwarder):
//
//	"answered"    → dial was answered (talk time elapsed, then BYE)
//	"no-answer"   → ring timeout expired / callee sent 408/480
//	"busy"        → callee sent 486 / 600
//	"failed"      → error (guardrails, codec mismatch, network failure, etc.)
//	"canceled"    → outer context canceled (caller leg hung up during ring)
//	"hangup-star" → caller pressed '*' while HangupOnStar was set
type DialResult struct {
	Status       string
	Reason       string
	DialCallSid  string
	Duration     time.Duration
	SIPFinalCode int
	DialedTarget string
}

// DialHandle is the opaque resource handle returned by DialTarget.PrepareDial.
// dialHandler defers Release() so RTP ports and callee-leg resources are
// always freed regardless of whether the dial succeeds or fails.
type DialHandle interface {
	// Release frees the callee leg + releases the acquired RTP port.
	// Idempotent; safe to call from a defer.
	Release()
}

// DialTarget is the wider interface required by the <Dial> verb handler.
// It embeds MidCallTarget (Terminate + Log) and adds the Dial-specific
// surface (PrepareDial + PerformDial). verb_hangup only needs MidCallTarget;
// verb_dial requires DialTarget — cleaner separation than extending
// MidCallTarget directly.
//
// midCallAdapter (api package) implements both surfaces.
type DialTarget interface {
	MidCallTarget

	// PrepareDial runs the Privacy Gate (closes WS stream cleanly) and
	// allocates the outbound callee leg + RTP port. Called before the
	// outbound INVITE is sent. On error the call should be terminated;
	// the returned DialHandle is nil on error.
	PrepareDial(opts DialOpts) (DialHandle, error)

	// PerformDial sends the outbound INVITE, waits for the dialog to end
	// (answered / busy / no-answer / failed / canceled / hangup-star), fires
	// the optional action callback (if opts.Action != ""), and returns the
	// terminal DialResult. The context carries the ring timeout; cancelling it
	// causes a CANCEL to be sent (no-answer / canceled).
	PerformDial(ctx context.Context, target string, opts DialOpts, handle DialHandle) (*DialResult, error)
}

// Dispatch walks doc.Verbs in order and invokes each verb's handler against
// the target. Synchronous on the caller's goroutine — no goroutines spawned.
// Returns nil on a successful walk (terminal Hangup OR no terminal verb), or
// a non-nil error if a verb handler errors out (e.g. Terminate fails to send
// BYE — caller in api.modifyCallHandler logs and translates to a Twilio
// error code; the call may be in a degraded state but the dispatcher itself
// is done).
//
// <Hangup/> is terminal: any verb after it is unreachable (matches Twilio's
// documented behavior). The dispatcher returns immediately after Hangup runs.
//
// <Dial>: a fully-functional terminal verb when the target implements
// DialTarget. Falls back to warn-and-skip when the target only implements
// MidCallTarget (e.g. unit tests that did not wire DialTarget).
//
// Unknown verbs and non-implemented verbs (Connect, Reject, Redirect) trigger
// a warn log via target.Log() and are SKIPPED — TWIML-05 warn-and-skip
// semantic. The dispatch returns nil even if all verbs in the document were
// skipped (Twilio-correct: never fail a webhook because of unknown verbs).
//
// nil doc returns nil (defensive — caller may have failed to parse).
func Dispatch(ctx context.Context, doc *Response, t MidCallTarget) error {
	if doc == nil {
		return nil
	}
	for _, v := range doc.Verbs {
		switch verb := v.(type) {
		case *Hangup:
			return hangupHandler(ctx, t)
		case *Dial:
			dt, ok := t.(DialTarget)
			if !ok {
				t.Log().Warn().Msg("twiml: <Dial> requires DialTarget — falling back to warn-and-skip (adapter does not implement DialTarget)")
				continue
			}
			return dialHandler(ctx, verb, dt)
		case *Connect, *Reject, *Redirect, unknownVerb:
			t.Log().Warn().
				Str("verb", v.XMLName().Local).
				Msg("twiml verb not implemented — skipped")
		default:
			t.Log().Warn().
				Str("verb", v.XMLName().Local).
				Msg("twiml: unknown verb type — skipped")
		}
	}
	return nil
}
