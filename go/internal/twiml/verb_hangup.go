package twiml

import (
	"context"
)

// hangupHandler dispatches <Hangup/> mid-call. Calls target.Terminate("hangup")
// — a terminal verb in Twilio's model: any TwiML after it is unreachable.
//
// Returns Terminate's error if BYE failed; the api modify-call handler logs
// and translates to a 5xx Twilio error code, but the call's logical state
// is already advancing toward Terminated regardless (markTerminated stamped
// in step 1 of bridge.CallSession.Terminate).
//
// Reason "hangup" is recorded on the recentlyTerminated snapshot's status
// field; the status-callback emission path reads this reason when
// computing the CallStatus mapping.
//
// ctx is currently observed only via the Info log; Terminate is
// fire-and-forget BYE and does not honor cancellation. The <Dial>
// handler does require ctx for outbound INVITE timeouts.
func hangupHandler(ctx context.Context, t MidCallTarget) error {
	_ = ctx
	t.Log().Info().Msg("twiml dispatch: <Hangup/> — terminating call")
	return t.Terminate("hangup")
}
