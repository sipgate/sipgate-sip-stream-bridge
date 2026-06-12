/**
 * <Dial> verb handler. Port of go/internal/twiml/verb_dial.go.
 *
 * Privacy Gate ordering:
 *   1. prepareDial — closes WS stream FIRST, then allocates callee leg + RTP port
 *   2. performDial — outbound INVITE → dialog → BYE; fires action callback
 *   3. terminate   — stamps the terminal reason on the caller leg
 *
 * The handle is released regardless of the performDial outcome so RTP ports are
 * never leaked.
 */

import type { DialOpts, DialResult, DialTarget } from './dispatch.js';
import type { Dial } from './verbs.js';
import { resolveDialTarget } from './verbs.js';

const DEFAULT_TIMEOUT_MS = 30_000; // Twilio default ring timeout: 30s
const DEFAULT_TIME_LIMIT_MS = 14_400_000; // Twilio default talk time: 4h
const DEFAULT_METHOD = 'POST';

/**
 * dialHandler dispatches <Dial> mid-call. Terminal verb: resolves the target,
 * builds DialOpts (applying defaults + per-<Number> overrides), runs the
 * prepare/perform/terminate sequence, and always releases the handle.
 */
export async function dialHandler(dial: Dial, target: DialTarget): Promise<void> {
  target.log().info('twiml: dialHandler invoked — resolving <Dial> target');

  const { target: dialed, ambiguous } = resolveDialTarget(dial);
  if (ambiguous) {
    target
      .log()
      .warn('twiml: <Dial> has both bare-text and <Number> child — <Number> wins (Twilio precedence)');
  }
  if (dialed === '') {
    target.log().warn('twiml: <Dial> has no target — skipping');
    return;
  }

  target
    .log()
    .info(
      { target: dialed, hangupOnStar: dial.hangupOnStar, action: dial.action },
      'twiml: <Dial> target resolved — invoking prepareDial',
    );
  if (dial.hasSip || dial.hasClient || dial.hasConference || dial.hasQueue) {
    target
      .log()
      .warn(
        'twiml: <Dial> has unsupported child (Sip/Client/Conference/Queue) — only <Number> + bare-text are supported',
      );
  }

  // Per-<Number> overrides win over the parent <Dial> values (Twilio per-leg
  // precedence): a non-empty <Number> attribute beats the parent.
  let statusCallback = dial.statusCallback;
  let statusCallbackMethod = dial.statusCallbackMethod;
  let statusCallbackEvents = dial.statusCallbackEvents;
  if (dial.number !== undefined) {
    if (dial.number.statusCallback !== undefined && dial.number.statusCallback !== '') {
      statusCallback = dial.number.statusCallback;
    }
    if (
      dial.number.statusCallbackMethod !== undefined &&
      dial.number.statusCallbackMethod !== ''
    ) {
      statusCallbackMethod = dial.number.statusCallbackMethod;
    }
    if (
      dial.number.statusCallbackEvents !== undefined &&
      dial.number.statusCallbackEvents.length > 0
    ) {
      statusCallbackEvents = dial.number.statusCallbackEvents;
    }
  }

  const opts: DialOpts = {
    callerId: dial.callerId,
    timeoutMs: dial.timeout !== undefined ? dial.timeout * 1000 : DEFAULT_TIMEOUT_MS,
    timeLimitMs: dial.timeLimit !== undefined ? dial.timeLimit * 1000 : DEFAULT_TIME_LIMIT_MS,
    hangupOnStar: dial.hangupOnStar,
    action: dial.action,
    method: dial.method !== undefined && dial.method !== '' ? dial.method : DEFAULT_METHOD,
    statusCallback,
    statusCallbackMethod:
      statusCallbackMethod !== undefined && statusCallbackMethod !== ''
        ? statusCallbackMethod
        : DEFAULT_METHOD,
    statusCallbackEvents,
  };

  let handle;
  try {
    handle = await target.prepareDial(opts);
  } catch (err) {
    target.log().error({ err }, 'twiml: prepareDial failed — terminating caller with reason=failed');
    await target.terminate('failed');
    return;
  }

  try {
    let result: DialResult | undefined;
    try {
      result = await target.performDial(dialed, opts, handle);
    } catch (err) {
      target.log().error({ err }, 'twiml: performDial failed — terminating caller');
      await target.terminate(twilioReasonFromDialResult(undefined));
      return;
    }
    await target.terminate(twilioReasonFromDialResult(result));
  } finally {
    handle.release();
  }
}

/**
 * twilioReasonFromDialResult maps DialResult.status to the termination reason:
 *   "answered"    → "completed"
 *   "busy"        → "busy"
 *   "no-answer"   → "no-answer"
 *   "canceled"    → "canceled"
 *   "hangup-star" → "completed"
 *   undefined     → "failed"
 *   default       → "failed"
 */
function twilioReasonFromDialResult(r: DialResult | undefined): string {
  if (r === undefined) return 'failed';
  switch (r.status) {
    case 'answered':
      return 'completed';
    case 'busy':
      return 'busy';
    case 'no-answer':
      return 'no-answer';
    case 'canceled':
      return 'canceled';
    case 'hangup-star':
      return 'completed';
    default:
      return 'failed';
  }
}
