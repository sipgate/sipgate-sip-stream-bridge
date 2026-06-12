/**
 * <Hangup/> verb handler. Port of go/internal/twiml/verb_hangup.go.
 */

import type { MidCallTarget } from './dispatch.js';

/**
 * hangupHandler dispatches <Hangup/> mid-call: calls target.terminate("hangup").
 * Terminal verb in Twilio's model — any TwiML after it is unreachable.
 *
 * The reason "hangup" is recorded on the call's terminated snapshot; the
 * status-callback emission path reads it when computing the CallStatus mapping.
 */
export async function hangupHandler(target: MidCallTarget): Promise<void> {
  target.log().info('twiml dispatch: <Hangup/> — terminating call');
  await target.terminate('hangup');
}
