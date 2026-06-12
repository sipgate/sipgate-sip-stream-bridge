/**
 * Outbound UAC dialog for B2BUA <Dial> forwarding.
 *
 * Drives a single outbound INVITE transaction on the shared SIP socket (so the
 * REGISTER source-port pinhole is reused) and the resulting dialog:
 *   - INVITE → 1xx/18x (ringing) → 2xx (answer) | 4xx-6xx (failure)
 *   - 401/407 Digest auth retry (qop=auth) — one retry
 *   - ACK on 2xx (end-to-end) and on non-2xx final (same branch as INVITE)
 *   - CANCEL before a final response (caller hangup / ring timeout)
 *   - in-dialog BYE (we hang up the callee) and inbound BYE (callee hangs up)
 *   - INVITE retransmission (UDP) until the first provisional/final response
 *
 * SIP message building mirrors the hand-rolled style of bridge/callManager.ts.
 */
import crypto from 'node:crypto';

import type { Logger } from 'pino';

import { USER_AGENT } from '../version.js';
import {
  authorizationHeaderName,
  buildDigestAuthorization,
  parseAuthHeader,
} from './digest.js';
import type { DialogHandlers, SipHandle } from './userAgent.js';

export type DialStatus = 'answered' | 'busy' | 'no-answer' | 'canceled' | 'failed';

export interface DialResultLite {
  status: DialStatus;
  /** SIP final response code (0 if none received). */
  sipCode: number;
  /** Short machine reason for metrics/callbacks (e.g. "trunk_5xx", "declined"). */
  reason?: string;
  /** SDP answer body (present on "answered"). */
  answerSdp?: string;
}

export interface DialogConfig {
  sipHandle: SipHandle;
  localIp: string;
  localSipPort: number;
  /** Request-URI for the outbound INVITE, e.g. sip:+4915123456@sipconnect.sipgate.de */
  targetUri: string;
  /** Next-hop transport address (the trunk/proxy we send to). */
  targetHost: string;
  targetPort: number;
  /** From addr-spec, e.g. sip:+4930555@sipconnect.sipgate.de */
  fromUri: string;
  /** Optional From display-name (caller-ID presentation). */
  fromDisplay?: string;
  /** Optional P-Preferred-Identity URI (RFC 3325). */
  ppiUri?: string;
  /** Our Contact, e.g. sip:user@localIp:5060 */
  contactUri: string;
  /** SDP offer body. */
  offerSdp: string;
  /** Digest credentials (SIP trunk auth). */
  username: string;
  password: string;
  log: Logger;
}

function randomHex(n: number): string {
  return crypto.randomBytes(n).toString('hex');
}
function newBranch(): string {
  return 'z9hG4bK' + randomHex(8);
}
function extractHeader(raw: string, name: string): string {
  const m = raw.match(new RegExp(`^${name}:\\s*(.+)$`, 'im'));
  return m ? m[1].trim() : '';
}
function statusCode(raw: string): number {
  const m = raw.split('\r\n')[0].match(/^SIP\/2\.0\s+(\d{3})/);
  return m ? parseInt(m[1], 10) : 0;
}
function cseqMethod(raw: string): string {
  const m = extractHeader(raw, 'CSeq').match(/^\d+\s+(\w+)/);
  return m ? m[1].toUpperCase() : '';
}
function toTag(raw: string): string {
  const to = extractHeader(raw, 'To');
  return to.match(/;tag=([^\s;>]+)/i)?.[1] ?? '';
}
function contactUriOf(raw: string): string {
  return raw.match(/^Contact:\s*<([^>]+)>/im)?.[1] ?? '';
}
function bodyOf(raw: string): string {
  const idx = raw.indexOf('\r\n\r\n');
  return idx === -1 ? '' : raw.slice(idx + 4);
}

export class OutboundDialog {
  private readonly cfg: DialogConfig;
  readonly callId: string;
  private readonly fromTag: string;
  private cseq = 1;
  private inviteBranch = newBranch();
  /** From header value (with display-name + tag), constant for the dialog. */
  private readonly fromHeader: string;

  // dialog state
  private answered = false;
  private toHeaderFinal = ''; // To header (incl. remote tag) from the final response
  private remoteTarget = ''; // Contact from 2xx (fallback: targetUri)

  // invite() transaction state
  private settled = false;
  private resolveInvite: ((r: DialResultLite) => void) | null = null;
  private provisionalSeen = false;
  private authAttempted = false;
  private retransmitTimer: ReturnType<typeof setTimeout> | null = null;
  private ringTimer: ReturnType<typeof setTimeout> | null = null;
  private cancelGraceTimer: ReturnType<typeof setTimeout> | null = null;
  private canceledReason: 'timeout' | 'caller' | null = null;

  private byeResolve: (() => void) | null = null;
  private byeTimer: ReturnType<typeof setTimeout> | null = null;

  /** Fired once on the first 18x (ringing). */
  onProvisional: ((code: number) => void) | null = null;
  /** Fired when the callee sends BYE after the call was answered. */
  onRemoteBye: (() => void) | null = null;

  constructor(cfg: DialogConfig) {
    this.cfg = cfg;
    this.callId = `${randomHex(12)}@${cfg.localIp}`;
    this.fromTag = randomHex(6);
    const disp = cfg.fromDisplay ? `"${cfg.fromDisplay}" ` : '';
    this.fromHeader = `${disp}<${cfg.fromUri}>;tag=${this.fromTag}`;
  }

  /**
   * Send the outbound INVITE and resolve when the dialog reaches a final state.
   * On "answered" the dialog stays registered to receive the callee's BYE.
   */
  invite(ringTimeoutMs: number): Promise<DialResultLite> {
    return new Promise<DialResultLite>((resolve) => {
      this.resolveInvite = resolve;
      const handlers: DialogHandlers = {
        onResponse: (raw) => this.handleResponse(raw),
        onRequest: (method, raw, rinfo) => this.handleRequest(method, raw, rinfo.port, rinfo.address),
      };
      this.cfg.sipHandle.registerDialog(this.callId, handlers);
      this.sendInvite();
      this.startRetransmit(500);
      this.ringTimer = setTimeout(() => {
        if (this.settled) return;
        this.canceledReason = 'timeout';
        this.cfg.log.info({ event: 'dial_ring_timeout' }, 'outbound INVITE ring timeout — sending CANCEL');
        this.sendCancel();
        // If no 487 follows promptly, give up as no-answer.
        this.cancelGraceTimer = setTimeout(() => this.finish({ status: 'no-answer', sipCode: 487 }), 2000);
      }, ringTimeoutMs);
    });
  }

  /** Caller (inbound leg) hung up before answer — cancel the pending INVITE. */
  cancel(): void {
    if (this.settled || this.answered) return;
    this.canceledReason = 'caller';
    this.sendCancel();
    this.cancelGraceTimer = setTimeout(() => this.finish({ status: 'canceled', sipCode: 487 }), 2000);
  }

  /** Hang up an answered call: send in-dialog BYE to the callee. */
  bye(): Promise<void> {
    return new Promise<void>((resolve) => {
      if (!this.answered) {
        this.cfg.sipHandle.unregisterDialog(this.callId);
        resolve();
        return;
      }
      this.byeResolve = () => {
        if (this.byeTimer) clearTimeout(this.byeTimer);
        this.byeTimer = null;
        this.cfg.sipHandle.unregisterDialog(this.callId);
        resolve();
      };
      this.cseq += 1;
      this.sendRequest('BYE', this.remoteTarget || this.cfg.targetUri, newBranch(), this.toHeaderFinal);
      // Proceed regardless after 5s (RFC 3261 Timer F-ish) so teardown never hangs.
      this.byeTimer = setTimeout(() => this.byeResolve?.(), 5000);
    });
  }

  // ── response handling ───────────────────────────────────────────────────────

  private handleResponse(raw: string): void {
    const method = cseqMethod(raw);
    const code = statusCode(raw);

    if (method === 'BYE') {
      if (code >= 200) this.byeResolve?.();
      return;
    }
    if (method === 'CANCEL') return; // 200 to our CANCEL — 487 to the INVITE follows
    if (method !== 'INVITE') return;

    if (code >= 100 && code < 200) {
      this.provisionalSeen = true;
      this.stopRetransmit();
      if (code >= 180) {
        const cb = this.onProvisional;
        this.onProvisional = null; // once
        cb?.(code);
      }
      return;
    }

    // Final response to INVITE.
    this.stopRetransmit();

    if (code === 401 || code === 407) {
      this.sendAckFailure(raw); // ACK the challenge (same branch)
      if (this.authAttempted) {
        this.finish({ status: 'failed', sipCode: code, reason: 'auth_failed' });
        return;
      }
      this.authAttempted = true;
      const header = extractHeader(raw, code === 401 ? 'WWW-Authenticate' : 'Proxy-Authenticate');
      const challenge = parseAuthHeader(header);
      const authValue = buildDigestAuthorization({
        username: this.cfg.username,
        password: this.cfg.password,
        method: 'INVITE',
        uri: this.cfg.targetUri,
        realm: challenge['realm'] ?? '',
        nonce: challenge['nonce'] ?? '',
        qop: challenge['qop'],
        opaque: challenge['opaque'],
        algorithm: challenge['algorithm'],
      });
      this.cseq += 1;
      this.inviteBranch = newBranch();
      this.provisionalSeen = false;
      this.cfg.log.debug({ event: 'dial_auth_retry', code }, 'retrying outbound INVITE with Digest auth');
      this.sendInvite(authorizationHeaderName(code), authValue);
      this.startRetransmit(500);
      return;
    }

    if (code >= 200 && code < 300) {
      this.answered = true;
      this.toHeaderFinal = extractHeader(raw, 'To');
      this.remoteTarget = contactUriOf(raw) || this.cfg.targetUri;
      this.sendAck2xx();
      this.finish({ status: 'answered', sipCode: code, answerSdp: bodyOf(raw) });
      return; // stay registered for the callee's BYE
    }

    // Non-2xx final: ACK (same branch) then map the outcome.
    this.sendAckFailure(raw);
    this.finish(this.mapFinal(code));
  }

  private handleRequest(method: string, raw: string, port: number, host: string): void {
    if (method !== 'BYE') return; // only the callee's BYE is expected in-dialog
    // 200 OK to the callee's BYE (echo Via/From/To/Call-ID/CSeq).
    const ok = [
      'SIP/2.0 200 OK',
      `Via: ${extractHeader(raw, 'Via')}`,
      `From: ${extractHeader(raw, 'From')}`,
      `To: ${extractHeader(raw, 'To')}`,
      `Call-ID: ${this.callId}`,
      `CSeq: ${extractHeader(raw, 'CSeq')}`,
      `User-Agent: ${USER_AGENT}`,
      'Content-Length: 0',
      '',
    ].join('\r\n') + '\r\n';
    this.cfg.sipHandle.sendRaw(Buffer.from(ok), port, host);
    this.cfg.log.info({ event: 'dial_remote_bye' }, 'callee sent BYE');
    this.cfg.sipHandle.unregisterDialog(this.callId);
    this.onRemoteBye?.();
  }

  private mapFinal(code: number): DialResultLite {
    if (code === 486 || code === 600) return { status: 'busy', sipCode: code };
    if (code === 408 || code === 480) return { status: 'no-answer', sipCode: code };
    if (code === 487) {
      return this.canceledReason === 'timeout'
        ? { status: 'no-answer', sipCode: code }
        : { status: 'canceled', sipCode: code };
    }
    if (code === 603) return { status: 'busy', sipCode: code, reason: 'declined' };
    if (code >= 500 && code < 600) return { status: 'failed', sipCode: code, reason: 'trunk_5xx' };
    return { status: 'failed', sipCode: code };
  }

  private finish(result: DialResultLite): void {
    if (this.settled) return;
    this.settled = true;
    this.stopRetransmit();
    if (this.ringTimer) clearTimeout(this.ringTimer);
    if (this.cancelGraceTimer) clearTimeout(this.cancelGraceTimer);
    this.ringTimer = null;
    this.cancelGraceTimer = null;
    if (result.status !== 'answered') {
      this.cfg.sipHandle.unregisterDialog(this.callId);
    }
    const resolve = this.resolveInvite;
    this.resolveInvite = null;
    resolve?.(result);
  }

  // ── message senders ─────────────────────────────────────────────────────────

  private startRetransmit(initial: number): void {
    let interval = initial;
    const tick = (): void => {
      if (this.provisionalSeen || this.settled) return;
      this.sendInvite(this.lastAuthHeaderName, this.lastAuthValue);
      interval = Math.min(interval * 2, 4000);
      this.retransmitTimer = setTimeout(tick, interval);
    };
    this.retransmitTimer = setTimeout(tick, initial);
  }
  private stopRetransmit(): void {
    if (this.retransmitTimer) clearTimeout(this.retransmitTimer);
    this.retransmitTimer = null;
  }

  private lastAuthHeaderName?: 'Authorization' | 'Proxy-Authorization';
  private lastAuthValue?: string;

  private sendInvite(authHeaderName?: 'Authorization' | 'Proxy-Authorization', authValue?: string): void {
    this.lastAuthHeaderName = authHeaderName;
    this.lastAuthValue = authValue;
    const lines = [
      `INVITE ${this.cfg.targetUri} SIP/2.0`,
      `Via: SIP/2.0/UDP ${this.cfg.localIp}:${this.cfg.localSipPort};branch=${this.inviteBranch};rport`,
      'Max-Forwards: 70',
      `From: ${this.fromHeader}`,
      `To: <${this.cfg.targetUri}>`,
      `Call-ID: ${this.callId}`,
      `CSeq: ${this.cseq} INVITE`,
      `Contact: <${this.cfg.contactUri}>`,
    ];
    if (this.cfg.ppiUri) lines.push(`P-Preferred-Identity: <${this.cfg.ppiUri}>`);
    if (authHeaderName && authValue) lines.push(`${authHeaderName}: ${authValue}`);
    lines.push(
      `User-Agent: ${USER_AGENT}`,
      'Content-Type: application/sdp',
      `Content-Length: ${Buffer.byteLength(this.cfg.offerSdp)}`,
      '',
    );
    const msg = lines.join('\r\n') + '\r\n' + this.cfg.offerSdp;
    this.cfg.sipHandle.sendRaw(Buffer.from(msg), this.cfg.targetPort, this.cfg.targetHost);
  }

  /** ACK for a 2xx (end-to-end, new branch, To incl. remote tag, to remote target). */
  private sendAck2xx(): void {
    const target = this.remoteTarget || this.cfg.targetUri;
    const msg = [
      `ACK ${target} SIP/2.0`,
      `Via: SIP/2.0/UDP ${this.cfg.localIp}:${this.cfg.localSipPort};branch=${newBranch()};rport`,
      'Max-Forwards: 70',
      `From: ${this.fromHeader}`,
      `To: ${this.toHeaderFinal}`,
      `Call-ID: ${this.callId}`,
      `CSeq: ${this.cseq} ACK`,
      `User-Agent: ${USER_AGENT}`,
      'Content-Length: 0',
      '',
    ].join('\r\n') + '\r\n';
    this.cfg.sipHandle.sendRaw(Buffer.from(msg), this.cfg.targetPort, this.cfg.targetHost);
  }

  /** ACK for a non-2xx final (same branch as the INVITE, To incl. tag from response). */
  private sendAckFailure(raw: string): void {
    const msg = [
      `ACK ${this.cfg.targetUri} SIP/2.0`,
      `Via: SIP/2.0/UDP ${this.cfg.localIp}:${this.cfg.localSipPort};branch=${this.inviteBranch};rport`,
      'Max-Forwards: 70',
      `From: ${this.fromHeader}`,
      `To: ${extractHeader(raw, 'To')}`,
      `Call-ID: ${this.callId}`,
      `CSeq: ${this.cseq} ACK`,
      `User-Agent: ${USER_AGENT}`,
      'Content-Length: 0',
      '',
    ].join('\r\n') + '\r\n';
    this.cfg.sipHandle.sendRaw(Buffer.from(msg), this.cfg.targetPort, this.cfg.targetHost);
  }

  /** CANCEL the pending INVITE (same branch + CSeq number, method CANCEL). */
  private sendCancel(): void {
    const msg = [
      `CANCEL ${this.cfg.targetUri} SIP/2.0`,
      `Via: SIP/2.0/UDP ${this.cfg.localIp}:${this.cfg.localSipPort};branch=${this.inviteBranch};rport`,
      'Max-Forwards: 70',
      `From: ${this.fromHeader}`,
      `To: <${this.cfg.targetUri}>`,
      `Call-ID: ${this.callId}`,
      `CSeq: ${this.cseq} CANCEL`,
      `User-Agent: ${USER_AGENT}`,
      'Content-Length: 0',
      '',
    ].join('\r\n') + '\r\n';
    this.cfg.sipHandle.sendRaw(Buffer.from(msg), this.cfg.targetPort, this.cfg.targetHost);
  }

  /** Generic in-dialog request (BYE) to the remote target. */
  private sendRequest(method: string, target: string, branch: string, toHeader: string): void {
    const msg = [
      `${method} ${target} SIP/2.0`,
      `Via: SIP/2.0/UDP ${this.cfg.localIp}:${this.cfg.localSipPort};branch=${branch};rport`,
      'Max-Forwards: 70',
      `From: ${this.fromHeader}`,
      `To: ${toHeader}`,
      `Call-ID: ${this.callId}`,
      `CSeq: ${this.cseq} ${method}`,
      `User-Agent: ${USER_AGENT}`,
      'Content-Length: 0',
      '',
    ].join('\r\n') + '\r\n';
    this.cfg.sipHandle.sendRaw(Buffer.from(msg), this.cfg.targetPort, this.cfg.targetHost);
  }
}
