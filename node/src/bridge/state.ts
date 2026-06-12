/**
 * Call lifecycle state machine for the v3 control plane.
 *
 * A v2.1 streaming call lives entirely in `Streaming`. The v3 mid-call <Dial>
 * path adds the `DialingOut → Forwarding` transition: the WS stream is closed
 * (privacy gate) while the RTP path stays alive, then the outbound leg's 200 OK
 * promotes the session to `Forwarding` (dual-leg RTP relay).
 *
 *   Dispatching → Streaming        (WS attached after 200 OK / ACK)
 *   Streaming   → DialingOut       (closeStream("dial-forward"); WS torn down)
 *   DialingOut  → Forwarding       (callee 200 OK; RTP relay active)
 *   *           → HungUp           (caller-initiated BYE)
 *   *           → Terminated       (REST-driven / natural teardown)
 */
export enum CallState {
  Dispatching = 'dispatching',
  Streaming = 'streaming',
  DialingOut = 'dialing-out',
  Forwarding = 'forwarding',
  HungUp = 'hung-up',
  Terminated = 'terminated',
}

/** A session is modifiable via REST only while neither terminated nor hung up. */
export function isActiveState(state: CallState): boolean {
  return state !== CallState.Terminated && state !== CallState.HungUp;
}

/**
 * Map an internal CallState (plus optional terminal reason) to the Twilio
 * REST `status` vocabulary: queued | ringing | in-progress | completed | busy |
 * failed | no-answer | canceled.
 *
 * For terminated calls the recorded teardown reason wins (it already carries the
 * Twilio-shaped value, e.g. "busy"/"no-answer"); otherwise the live state maps
 * to the nearest lifecycle status.
 */
export function twilioStatus(state: CallState, terminalReason?: string): string {
  if (state === CallState.Terminated || state === CallState.HungUp) {
    return mapTerminalReason(terminalReason);
  }
  switch (state) {
    case CallState.Dispatching:
      return 'queued';
    case CallState.Streaming:
    case CallState.DialingOut:
    case CallState.Forwarding:
      return 'in-progress';
    default:
      return 'in-progress';
  }
}

function mapTerminalReason(reason?: string): string {
  switch (reason) {
    case 'busy':
      return 'busy';
    case 'no-answer':
      return 'no-answer';
    case 'canceled':
    case 'remote_cancel':
      return 'canceled';
    case 'failed':
    case 'ws_reconnect_failed':
      return 'failed';
    // "completed", "hangup", "remote_bye", "shutdown" and anything else → completed
    default:
      return 'completed';
  }
}
