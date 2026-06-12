/**
 * B2BUA <Dial> forwarder.
 *
 * Orchestrates an outbound call leg and bridges it to the inbound caller:
 *   guardrails → caller-ID resolution → outbound INVITE (Digest) → SDP answer
 *   (codec fail-fast) → dual-leg RTP relay → ring/timeLimit timers → teardown.
 *
 * The privacy gate (closing the WS stream before the INVITE) is enforced by the
 * caller (midcall adapter) via CallManager.closeStream BEFORE createCalleeLeg().
 */
import dns from 'node:dns/promises';
import net from 'node:net';
import os from 'node:os';

import type { Logger } from 'pino';

import type { Config } from '../config/index.js';
import { newCallSid } from '../identity/sid.js';
import type { ForwardReason, Metrics } from '../observability/metrics.js';
import type { CallSession } from '../bridge/callManager.js';
import { CallState } from '../bridge/state.js';
import { createRtpHandler, type RtpHandler } from '../rtp/rtpHandler.js';
import { buildStatusForm } from '../webhook/callbackForm.js';
import { subscriptionMatches } from '../webhook/subscription.js';
import type { StatusClient } from '../webhook/status.js';
import type { DialOpts } from '../twiml/dispatch.js';
import {
  resolveCallerID,
  resolveDisplayCallerID,
  normaliseTrunkCallerID,
  CallerIdRequiredError,
} from './callerId.js';
import { OutboundDialog, type DialStatus } from './dialog.js';
import { Guardrails, GuardrailError } from './guardrails.js';
import { acceptSdpAnswer, buildSdpOffer, CodecMismatchError } from './sdp.js';
import type { SipHandle } from './userAgent.js';

export interface ForwardOutcome {
  status: DialStatus;
  reason?: string;
  dialCallSid: string;
  durationMs: number;
  sipFinalCode: number;
}

export interface ForwarderDeps {
  config: Config;
  sipHandle: SipHandle;
  guardrails: Guardrails;
  statusClient?: StatusClient;
  metrics?: Metrics;
  log: Logger;
}

function getLocalIp(): string {
  for (const ifaces of Object.values(os.networkInterfaces())) {
    for (const iface of ifaces ?? []) {
      if (!iface.internal && iface.family === 'IPv4') return iface.address;
    }
  }
  return '127.0.0.1';
}

/** Extract the user part of a SIP URI / number string (sip:user@host → user). */
function userPart(value: string): string {
  const m = value.match(/^(?:sips?:)?([^@>;\s]+)/i);
  return m ? m[1] : value;
}

const FORWARD_REASONS = new Set<string>([
  'busy', 'no_answer', 'rejected', 'unreachable', 'codec_mismatch', 'toll_fraud',
  'rate_limit', 'caller_id_rejected', 'auth_failed', 'trunk_5xx', 'timeout', 'error',
]);
/** Coerce an arbitrary reason string to a bounded ForwardReason metric label. */
function fwdReason(s: string | undefined): ForwardReason {
  return s !== undefined && FORWARD_REASONS.has(s) ? (s as ForwardReason) : 'error';
}

/** Twilio DialCallStatus mapping for the action callback. */
function dialCallStatus(status: DialStatus): string {
  switch (status) {
    case 'answered':
      return 'completed';
    case 'busy':
      return 'busy';
    case 'no-answer':
      return 'no-answer';
    case 'canceled':
      return 'canceled';
    default:
      return 'failed';
  }
}

export class Forwarder {
  private readonly d: ForwarderDeps;
  private readonly localIp: string;
  private nextHop: { host: string; port: number } | null = null;

  constructor(deps: ForwarderDeps) {
    this.d = deps;
    this.localIp = deps.config.SDP_CONTACT_IP ?? getLocalIp();
  }

  /** Allocate the callee-leg RTP socket (PCMU). Called after the privacy gate. */
  async createCalleeLeg(): Promise<RtpHandler> {
    return createRtpHandler({
      portMin: this.d.config.RTP_PORT_MIN,
      portMax: this.d.config.RTP_PORT_MAX,
      log: this.d.log,
      audioPt: 0, // PCMU
      dtmfPt: 101,
      silenceByte: 0xff,
    });
  }

  private async resolveNextHop(): Promise<{ host: string; port: number }> {
    if (this.nextHop) return this.nextHop;
    const registrar = this.d.config.SIP_REGISTRAR;
    const host = net.isIPv4(registrar) ? registrar : (await dns.resolve4(registrar))[0];
    const port = this.d.config.SIP_OUTBOUND_TARGET_PORT || 5060;
    this.nextHop = { host, port };
    return this.nextHop;
  }

  /**
   * Run the outbound dial and (on answer) the dual-leg relay until the call ends.
   * `calleeRtp` is created via createCalleeLeg() after the privacy gate.
   */
  async run(
    session: CallSession,
    target: string,
    opts: DialOpts,
    calleeRtp: RtpHandler,
  ): Promise<ForwardOutcome> {
    const log = this.d.log;
    const dialCallSid = newCallSid();
    const domain = this.d.config.SIP_DOMAIN;

    // 1. Toll-fraud guardrails (before any signalling or "initiated" event).
    try {
      this.d.guardrails.checkDial(session.callSid, target);
    } catch (err) {
      const reason = err instanceof GuardrailError ? err.reason : 'error';
      this.d.metrics?.incForwardFailed(fwdReason(reason));
      log.warn({ event: 'dial_blocked', reason }, 'outbound dial blocked by guardrails');
      return { status: 'failed', reason, dialCallSid, durationMs: 0, sipFinalCode: 0 };
    }

    this.d.metrics?.incForwardAttempts();

    // 2. Caller-ID resolution chain (From addr-spec + display/PPI).
    let fromUser: string;
    try {
      fromUser = userPart(
        resolveCallerID({
          twimlCallerId: opts.callerId,
          dialDefaultCallerId: this.d.config.DIAL_DEFAULT_CALLER_ID,
          sipUser: this.d.config.SIP_USER,
          callerFrom: session.fromNumber,
        }).callerId,
      );
    } catch (err) {
      if (err instanceof CallerIdRequiredError) {
        this.d.metrics?.incForwardFailed('caller_id_rejected');
        return { status: 'failed', reason: 'caller_id_required', dialCallSid, durationMs: 0, sipFinalCode: 0 };
      }
      throw err;
    }
    const display = resolveDisplayCallerID({
      twimlCallerId: opts.callerId,
      dialDefaultCallerId: this.d.config.DIAL_DEFAULT_CALLER_ID,
      callerFrom: session.fromNumber,
    });
    const ppiUser = display ? normaliseTrunkCallerID(userPart(display), this.d.config.DIAL_CALLER_ID_COUNTRY_CODE) : '';

    // 3. Build URIs + SDP offer, resolve trunk next-hop.
    const targetUri = `sip:${target}@${domain}`;
    const fromUri = `sip:${fromUser}@${domain}`;
    const contactUri = `sip:${this.d.config.SIP_USER}@${this.localIp}:5060`;
    const ppiUri = ppiUser ? `sip:${ppiUser}@${domain}` : undefined;
    const offerSdp = buildSdpOffer(this.localIp, calleeRtp.localPort);
    const nextHop = await this.resolveNextHop();

    // Per-dial-leg status-callback subscription (independent of the inbound call).
    const dialSub = opts.statusCallback
      ? { url: opts.statusCallback, method: opts.statusCallbackMethod, events: new Set(opts.statusCallbackEvents ?? []) }
      : null;
    let dialSeq = 0;
    const emitDial = (event: string, callStatus: string, extras?: { callDurationSec?: number; sipResponseCode?: number }): void => {
      if (!dialSub || !this.d.statusClient) return;
      if (dialSub.events.size > 0 && !subscriptionMatches(dialSub.events, event)) return;
      const form = buildStatusForm({
        callSid: dialCallSid,
        accountSid: session.accountSid,
        from: fromUser,
        to: target,
        direction: 'outbound-dial',
        callStatus,
        sequenceNumber: dialSeq++,
        timestamp: new Date(),
        callDurationSec: extras?.callDurationSec,
        sipResponseCode: extras?.sipResponseCode,
      });
      this.d.statusClient.enqueue(dialCallSid, {
        url: dialSub.url,
        method: dialSub.method,
        form,
        event,
        trusted: false,
      });
    };

    const dialog = new OutboundDialog({
      sipHandle: this.d.sipHandle,
      localIp: this.localIp,
      localSipPort: 5060,
      targetUri,
      targetHost: nextHop.host,
      targetPort: nextHop.port,
      fromUri,
      fromDisplay: ppiUser || undefined,
      ppiUri,
      contactUri,
      offerSdp,
      username: this.d.config.SIP_USER,
      password: this.d.config.SIP_PASSWORD,
      log,
    });
    dialog.onProvisional = (code) => {
      log.info({ event: 'dial_ringing', code }, 'outbound call ringing');
      emitDial('ringing', 'ringing');
    };

    emitDial('initiated', 'queued');
    log.info({ event: 'dial_invite', dialCallSid }, 'sending outbound INVITE');

    const result = await dialog.invite(opts.timeoutMs);

    // 4. Non-answered outcomes — emit terminal dial status + return.
    if (result.status !== 'answered') {
      if (result.status === 'busy' || result.status === 'no-answer') {
        this.d.metrics?.incForwardFailed(result.status === 'busy' ? 'busy' : 'no_answer');
      } else if (result.status === 'failed') {
        this.d.metrics?.incForwardFailed(fwdReason(result.reason));
      }
      emitDial(result.status, dialCallStatus(result.status), { sipResponseCode: result.sipCode });
      await this.flushDialCallbacks(opts, session, dialCallSid, target, fromUser, result, 0);
      return { status: result.status, reason: result.reason, dialCallSid, durationMs: 0, sipFinalCode: result.sipCode };
    }

    // 5. Answered: accept SDP (codec fail-fast), wire the dual-leg relay.
    let remote: { remoteIp: string; remotePort: number; dtmfPt: number };
    try {
      remote = acceptSdpAnswer(result.answerSdp ?? '');
    } catch (err) {
      const reason = err instanceof CodecMismatchError ? 'codec_mismatch' : 'sdp_error';
      log.warn({ event: 'dial_codec_mismatch', reason }, 'outbound SDP answer rejected — hanging up callee');
      this.d.metrics?.incForwardFailed('codec_mismatch');
      await dialog.bye();
      emitDial('failed', 'failed', { sipResponseCode: result.sipCode });
      await this.flushDialCallbacks(opts, session, dialCallSid, target, fromUser, { status: 'failed', sipCode: result.sipCode, reason }, 0);
      return { status: 'failed', reason, dialCallSid, durationMs: 0, sipFinalCode: result.sipCode };
    }

    calleeRtp.setRemote(remote.remoteIp, remote.remotePort);
    calleeRtp.startForwarding();
    calleeRtp.on('audio', (payload: Buffer) => {
      this.d.metrics?.incRtpTx();
      session.rtp.sendAudio(payload); // callee → caller
    });

    session.forwardRtp = calleeRtp; // caller → callee (routed in CallManager's rtp handler)
    session.forwardDialog = dialog;
    session.hangupOnStar = opts.hangupOnStar;
    session.state = CallState.Forwarding;
    this.d.metrics?.incForwardSuccess();
    const answeredAt = Date.now();
    log.info({ event: 'dial_answered', dialCallSid }, 'outbound call answered — relaying media');
    emitDial('answered', 'in-progress');

    // 6. Wait for the call to end: callee BYE, timeLimit, or caller hangup.
    await new Promise<void>((resolve) => {
      let done = false;
      const end = (): void => {
        if (done) return;
        done = true;
        if (timeLimitTimer) clearTimeout(timeLimitTimer);
        resolve();
      };
      session.onForwardEnd = end; // caller-initiated teardown (CallManager.terminateSession)
      dialog.onRemoteBye = end; // callee hung up
      const timeLimitTimer =
        opts.timeLimitMs > 0
          ? setTimeout(() => {
              log.info({ event: 'dial_time_limit' }, 'dial timeLimit reached — hanging up');
              void dialog.bye();
              end();
            }, opts.timeLimitMs)
          : null;
    });

    const durationMs = Date.now() - answeredAt;
    const durationSec = Math.max(0, Math.round(durationMs / 1000));
    emitDial('completed', 'completed', { callDurationSec: durationSec });
    await this.flushDialCallbacks(
      opts,
      session,
      dialCallSid,
      target,
      fromUser,
      { status: 'answered', sipCode: 200 },
      durationSec,
    );
    return { status: 'answered', dialCallSid, durationMs, sipFinalCode: 200 };
  }

  /**
   * Fire the <Dial action> callback (best-effort, signed + SSRF-guarded via the
   * StatusClient transport), then drain the per-dial-leg callback queue.
   */
  private async flushDialCallbacks(
    opts: DialOpts,
    session: CallSession,
    dialCallSid: string,
    target: string,
    fromUser: string,
    result: { status: DialStatus; sipCode: number; reason?: string },
    durationSec: number,
  ): Promise<void> {
    const sc = this.d.statusClient;
    if (opts.action && sc) {
      const form: Record<string, string> = {
        DialCallSid: dialCallSid,
        DialCallStatus: dialCallStatus(result.status),
        DialCallDuration: String(durationSec),
        Direction: 'outbound-dial',
        CallSid: session.callSid,
        AccountSid: session.accountSid,
        From: fromUser,
        To: target,
      };
      sc.enqueue(dialCallSid, {
        url: opts.action,
        method: opts.method || 'POST',
        form,
        event: 'completed',
        trusted: false,
      });
    }
    if (sc) await sc.drainAndClose(dialCallSid);
  }
}
