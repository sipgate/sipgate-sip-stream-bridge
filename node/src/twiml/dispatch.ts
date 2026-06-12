/**
 * TwiML dispatch interfaces + the dispatch walker. Port of
 * go/internal/twiml/dispatch.go.
 *
 * The verb handlers need a narrow per-call surface from the bridge adapter:
 * MidCallTarget (terminate + log) for <Hangup>, widened to DialTarget
 * (prepareDial + performDial) for <Dial>. The api-layer adapter implements
 * both.
 */

import type { Logger } from 'pino';

import { dialHandler } from './verbDial.js';
import { hangupHandler } from './verbHangup.js';
import type { Response } from './verbs.js';

/**
 * MidCallTarget is the narrow interface mid-call verb handlers need from the
 * per-call adapter. terminate (used by <Hangup>) + log (warn-and-skip paths).
 *
 * Implementations must be safe to call from any context — dispatch runs on the
 * caller's async path, but the underlying termination may cross boundaries.
 */
export interface MidCallTarget {
  /**
   * terminate ends the call cleanly with the given reason. Idempotent — a
   * second call is a no-op. May return a Promise (async BYE) or void.
   */
  terminate(reason: string): Promise<void> | void;

  /** log returns a per-call structured logger for verb-side entries. */
  log(): Logger;
}

/**
 * DialOpts captures the per-<Dial> knobs derived from TwiML attributes. The
 * api-layer adapter translates these to sip.DialOpts before calling the
 * forwarder. Durations are milliseconds (Node convention).
 */
export interface DialOpts {
  /** Value from <Dial callerId="…">. */
  callerId?: string;
  /** Ring timeout in milliseconds (Twilio default 30_000). */
  timeoutMs: number;
  /** Max talk time in milliseconds (Twilio default 14_400_000 = 4h). */
  timeLimitMs: number;
  /** When true and caller presses '*', the callee leg is BYE'd. */
  hangupOnStar: boolean;
  /** Post-dial action callback URL (<Dial action="…">). */
  action?: string;
  /** HTTP method for the action callback (default "POST"). */
  method: string;
  /** Per-call status-callback URL; empty/undefined = no subscription. */
  statusCallback?: string;
  /** "POST" | "GET" (default "POST" at emission time). */
  statusCallbackMethod: string;
  /** Tokenized event-name list; empty/undefined = default subset. */
  statusCallbackEvents?: string[];
}

/**
 * DialResult is the terminal-state report from a single <Dial> invocation.
 * status mirrors sip.DialResult.Status set by the forwarder:
 *   "answered"    → answered (talk time elapsed, then BYE)
 *   "no-answer"   → ring timeout / callee 408/480
 *   "busy"        → callee 486/600
 *   "failed"      → error (guardrails, codec mismatch, network, …)
 *   "canceled"    → caller leg hung up during ring
 *   "hangup-star" → caller pressed '*' while hangupOnStar was set
 */
export interface DialResult {
  status: string;
  reason?: string;
  dialCallSid?: string;
  durationMs?: number;
  sipFinalCode?: number;
  dialedTarget?: string;
}

/**
 * DialHandle is the opaque resource handle returned by prepareDial. dialHandler
 * always releases it so RTP ports / callee-leg resources are freed regardless
 * of dial outcome.
 */
export interface DialHandle {
  /** release frees the callee leg + acquired RTP port. Idempotent. */
  release(): void;
}

/**
 * DialTarget is the wider interface required by the <Dial> handler. It extends
 * MidCallTarget and adds the Dial-specific surface.
 */
export interface DialTarget extends MidCallTarget {
  /**
   * prepareDial runs the Privacy Gate (closes the WS stream cleanly) and
   * allocates the outbound callee leg + RTP port. Called before the outbound
   * INVITE. On rejection the call should be terminated.
   */
  prepareDial(opts: DialOpts): Promise<DialHandle>;

  /**
   * performDial sends the outbound INVITE, waits for the dialog to end, fires
   * the optional action callback, and resolves with the terminal DialResult.
   */
  performDial(target: string, opts: DialOpts, handle: DialHandle): Promise<DialResult>;
}

/**
 * isDialTarget duck-types a MidCallTarget into a DialTarget. The bridge adapter
 * may implement only MidCallTarget (e.g. unit-test wiring), in which case
 * <Dial> warn-and-skips rather than throwing.
 */
export function isDialTarget(t: MidCallTarget): t is DialTarget {
  const c = t as Partial<DialTarget>;
  return typeof c.prepareDial === 'function' && typeof c.performDial === 'function';
}

/**
 * dispatch walks doc.verbs in order and invokes each verb's handler against the
 * target. Resolves when the walk completes — a terminal verb runs then returns,
 * or the walk reaches the end.
 *
 * <Hangup> is terminal: any verb after it is unreachable (Twilio-correct).
 * <Dial> is terminal when the target implements DialTarget; otherwise it
 * warn-and-skips. Unknown / Connect / Reject / Redirect verbs warn-and-skip via
 * target.log(). dispatch resolves even if every verb was skipped (never fail a
 * webhook because of unknown verbs).
 */
export async function dispatch(doc: Response, target: MidCallTarget): Promise<void> {
  for (const verb of doc.verbs) {
    switch (verb.kind) {
      case 'Hangup':
        await hangupHandler(target);
        return;
      case 'Dial':
        if (!isDialTarget(target)) {
          target
            .log()
            .warn(
              'twiml: <Dial> requires DialTarget — falling back to warn-and-skip (adapter does not implement DialTarget)',
            );
          continue;
        }
        await dialHandler(verb, target);
        return;
      default:
        target.log().warn({ verb: verb.name }, 'twiml verb not implemented — skipped');
    }
  }
}
