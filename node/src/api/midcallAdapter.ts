/**
 * MidCallAdapter — wraps a live CallSession as a twiml.MidCallTarget so the
 * mid-call modify-call handler can dispatch parsed TwiML against the running
 * call. Port of go/internal/api/midcall_adapter.go (MidCallTarget surface only).
 *
 * Cycle-avoidance: this adapter lives in the api package (bridge ← api → twiml
 * is a clean DAG) rather than in bridge or twiml. It deliberately implements
 * ONLY MidCallTarget (terminate + log) for this milestone — the DialTarget
 * surface (prepareDial / performDial) is NOT implemented, so dispatch()'s
 * isDialTarget duck-type returns false and <Dial> warn-and-skips. That is the
 * intended, correct behavior here: the SIP forwarder (the callee-leg B2BUA) is
 * delivered in a later milestone. See the TODO in prepareDial below.
 */

import type { Logger } from 'pino';

import type { CallManager, CallSession } from '../bridge/callManager.js';
import type { MidCallTarget } from '../twiml/dispatch.js';

/**
 * MidCallAdapter implements the narrow MidCallTarget surface against a live
 * CallSession. terminate() routes through CallManager.terminateByCallSid so the
 * BYE + recently-terminated snapshot bookkeeping stays centralized in the
 * manager (the adapter never mutates session state directly).
 */
export class MidCallAdapter implements MidCallTarget {
  private readonly session: CallSession;
  private readonly manager: CallManager;
  private readonly logger: Logger;

  constructor(session: CallSession, manager: CallManager, log: Logger) {
    this.session = session;
    this.manager = manager;
    // Capture the per-call logger; prefer the session's own child logger (it
    // already carries callId) but fall back to the supplied server logger.
    this.logger = session.log ?? log;
  }

  /**
   * Terminate the call with the given reason. Idempotent at the manager level —
   * terminateByCallSid returns false (no-op) for an already-terminated call.
   * <Hangup> drives this with reason "hangup"/"completed".
   */
  terminate(reason: string): void {
    this.manager.terminateByCallSid(this.session.callSid, reason);
  }

  /** Per-call structured logger used by dispatch's warn-and-skip paths. */
  log(): Logger {
    return this.logger;
  }

  // ── DialTarget (NOT implemented in this milestone) ──────────────────────────
  //
  // TODO(Milestone E — SIP forwarder / B2BUA): implement prepareDial +
  // performDial so this adapter satisfies twiml.DialTarget and <Dial> verbs
  // bridge the caller to an outbound callee leg. Until then, dispatch()'s
  // isDialTarget() duck-type returns false and <Dial> is warn-and-skipped —
  // which is the correct, Twilio-incompatible-but-safe behavior for the
  // streaming-only build. Do NOT stub these methods: a partial DialTarget would
  // make isDialTarget() return true and cause dispatch to attempt a dial against
  // a non-existent forwarder.
}
