package twiml

import (
	"context"
	"time"
)

// dialHandler dispatches <Dial> mid-call. Terminal verb: Twilio's model
// specifies that <Dial> stops the verb chain; subsequent verbs in the document
// are unreachable. dialHandler returns after the dial completes (success or
// failure) by calling t.Terminate with the appropriate reason.
//
// Privacy Gate ordering:
//  1. t.PrepareDial     — closes WS stream FIRST, then allocates callee leg + RTP port
//  2. t.PerformDial     — outbound INVITE → dialog → BYE; fires action callback
//  3. t.Terminate       — stamps termReason on the caller leg; Twilio REST clients
//     see the final status via Status() on the next GET /Calls/{Sid}.json
//
// Handle release is deferred immediately after PrepareDial succeeds so RTP
// ports are freed regardless of the PerformDial outcome (TWIML-04 / threat
// model: RTP port leak when PerformDial fails).
func dialHandler(ctx context.Context, dial *Dial, t DialTarget) error {
	t.Log().Info().Msg("twiml: dialHandler invoked — resolving <Dial> target")

	target, ambiguous := dial.ResolveDialTarget()
	if ambiguous {
		t.Log().Warn().Msg("twiml: <Dial> has both bare-text and <Number> child — <Number> wins (Twilio precedence)")
	}
	if target == "" {
		t.Log().Warn().Msg("twiml: <Dial> has no target — skipping")
		return nil
	}

	t.Log().Info().
		Str("target", target).
		Bool("hangup_on_star", dial.HangupOnStar).
		Str("action", dial.Action).
		Msg("twiml: <Dial> target resolved — invoking PrepareDial")
	if dial.HasSip || dial.HasClient || dial.HasConference || dial.HasQueue {
		t.Log().Warn().Msg("twiml: <Dial> has unsupported child (Sip/Client/Conference/Queue) — only <Number> + bare-text are supported")
		// Continue with the resolved target if any; handler falls through to dial it.
		// If the target resolved from NumberText/Number is empty, the early-return above applies.
	}

	// Resolve per-<Number> overrides over parent <Dial>. Twilio's
	// documented per-leg precedence: if a <Number statusCallback*>
	// attribute is non-empty, it wins. Empty values fall back to the
	// parent <Dial> attribute.
	statusCallback := dial.StatusCallback
	statusCallbackMethod := dial.StatusCallbackMethod
	statusCallbackEvents := dial.StatusCallbackEvents
	if dial.Number != nil {
		if dial.Number.StatusCallback != "" {
			statusCallback = dial.Number.StatusCallback
		}
		if dial.Number.StatusCallbackMethod != "" {
			statusCallbackMethod = dial.Number.StatusCallbackMethod
		}
		if len(dial.Number.StatusCallbackEvents) > 0 {
			statusCallbackEvents = dial.Number.StatusCallbackEvents
		}
	}

	opts := DialOpts{
		CallerID:             dial.CallerID,
		Timeout:              time.Duration(intPtrOr(dial.Timeout, 30)) * time.Second,
		TimeLimit:            time.Duration(intPtrOr(dial.TimeLimit, 14400)) * time.Second,
		HangupOnStar:         dial.HangupOnStar,
		Action:               dial.Action,
		Method:               dial.Method,
		StatusCallback:       statusCallback,
		StatusCallbackMethod: statusCallbackMethod,
		StatusCallbackEvents: statusCallbackEvents,
	}

	handle, err := t.PrepareDial(opts)
	if err != nil {
		t.Log().Error().Err(err).Msg("twiml: PrepareDial failed — terminating caller with reason=failed")
		return t.Terminate("failed")
	}
	defer handle.Release()

	result, err := t.PerformDial(ctx, target, opts, handle)
	if err != nil {
		t.Log().Error().Err(err).Msg("twiml: PerformDial failed — terminating caller")
		reason := twilioReasonFromDialResult(result, err)
		return t.Terminate(reason)
	}

	return t.Terminate(twilioReasonFromDialResult(result, nil))
}

// intPtrOr returns *p if non-nil, else def.
func intPtrOr(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

// twilioReasonFromDialResult maps DialResult.Status (or error fallback) to a
// bridge.CallSession termReason string that twilioStatusFromTermReason then
// maps to the Twilio CallStatus wire-format enum.
//
//	"answered"     → "completed"   (talk time elapsed; callee BYE'd)
//	"busy"         → "busy"        (callee 486/600)
//	"no-answer"    → "no-answer"   (ring timeout / 408/480)
//	"canceled"     → "canceled"    (caller hung up during ring)
//	"hangup-star"  → "completed"   (caller pressed '*')
//	nil result     → "failed"      (error before dial attempted)
//	default        → "failed"      (unrecognized status)
func twilioReasonFromDialResult(r *DialResult, err error) string {
	if r == nil {
		return "failed"
	}
	switch r.Status {
	case "answered":
		return "completed"
	case "busy":
		return "busy"
	case "no-answer":
		return "no-answer"
	case "canceled":
		return "canceled"
	case "hangup-star":
		return "completed"
	default:
		return "failed"
	}
}
