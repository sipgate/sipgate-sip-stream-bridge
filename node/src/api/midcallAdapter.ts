/**
 * MidCallAdapter — wraps a live CallSession as a twiml.DialTarget so the mid-call
 * modify-call handler can dispatch parsed TwiML (<Hangup/>, <Dial>) against the
 * running call. Port of go/internal/api/midcall_adapter.go.
 *
 * Cycle-avoidance: this adapter lives in the api package (bridge ← api → twiml/sip
 * is a clean DAG). terminate() routes through CallManager so BYE + snapshot
 * bookkeeping stays centralized. The <Dial> path enforces the privacy gate
 * (close the WS stream BEFORE the outbound INVITE) via CallManager.closeStream.
 */

import type { Logger } from 'pino';

import type { CallManager, CallSession } from '../bridge/callManager.js';
import type { RtpHandler } from '../rtp/rtpHandler.js';
import type { Forwarder } from '../sip/forwarder.js';
import type { DialHandle, DialOpts, DialResult, DialTarget } from '../twiml/dispatch.js';

export class MidCallAdapter implements DialTarget {
  private readonly session: CallSession;
  private readonly manager: CallManager;
  private readonly forwarder: Forwarder;
  private readonly logger: Logger;
  /** Callee-leg RTP allocated in prepareDial, consumed by performDial. */
  private calleeRtp: RtpHandler | null = null;

  constructor(session: CallSession, manager: CallManager, forwarder: Forwarder, log: Logger) {
    this.session = session;
    this.manager = manager;
    this.forwarder = forwarder;
    this.logger = session.log ?? log;
  }

  /**
   * Terminate the call. Idempotent at the manager level — terminateByCallSid is a
   * no-op for an already-terminated call. <Hangup> drives this with "hangup".
   */
  terminate(reason: string): void {
    this.manager.terminateByCallSid(this.session.callSid, reason);
  }

  log(): Logger {
    return this.logger;
  }

  // ── DialTarget ──────────────────────────────────────────────────────────────

  /**
   * Privacy gate + leg allocation, run BEFORE the outbound INVITE:
   *   1. close the WS stream cleanly (the bot must not hear the forwarded call),
   *   2. allocate the callee-leg RTP socket.
   */
  async prepareDial(_opts: DialOpts): Promise<DialHandle> {
    this.manager.closeStream(this.session, 'dial-forward');
    this.calleeRtp = await this.forwarder.createCalleeLeg();
    const rtp = this.calleeRtp;
    return {
      release: (): void => {
        rtp.dispose(); // idempotent — safe even if the relay already disposed it
      },
    };
  }

  /** Send the outbound INVITE and bridge until the call ends. */
  async performDial(target: string, opts: DialOpts, _handle: DialHandle): Promise<DialResult> {
    if (!this.calleeRtp) throw new Error('performDial called before prepareDial');
    const outcome = await this.forwarder.run(this.session, target, opts, this.calleeRtp);
    return {
      status: outcome.status,
      reason: outcome.reason,
      dialCallSid: outcome.dialCallSid,
      durationMs: outcome.durationMs,
      sipFinalCode: outcome.sipFinalCode,
      dialedTarget: target,
    };
  }
}
