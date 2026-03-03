/**
 * CallManager — central coordinator for inbound SIP call lifecycle.
 *
 * Orchestrates the full INVITE → audio bridge → BYE sequence:
 *   1. Receive INVITE from sipgate
 *   2. Send 100 Trying (immediate) + 180 Ringing (after SDP validated)
 *   3. Connect to WS backend (2-second timeout → 503 on failure)
 *   4. Send 200 OK with SDP answer
 *   5. Wire bidirectional audio bridge (RTP ↔ WS)
 *   6. Handle BYE (from caller) or WS disconnect (from backend) cleanly
 *
 * Thread safety: all I/O is async via Node.js event loop; Map mutations
 * happen synchronously within microtask boundaries — no data races.
 */

import crypto from 'node:crypto';
import type { RemoteInfo } from 'node:dgram';
import os from 'node:os';

import type { Logger } from 'pino';

import type { Config } from '../config/index.js';
import { createChildLogger } from '../logger/index.js';
import { createRtpHandler, type RtpHandler } from '../rtp/rtpHandler.js';
import { buildSdpAnswer, parseSdpOffer } from '../sip/sdp.js';
import type { SipCallbacks, SipHandle } from '../sip/userAgent.js';
import { createWsClient, type WsClient } from '../ws/wsClient.js';

// ── Public types ──────────────────────────────────────────────────────────────

export interface CallSession {
  /** SIP Call-ID (from INVITE header) */
  callId: string;
  /** CA… identifier (generated) */
  callSid: string;
  /** MZ… identifier (generated) */
  streamSid: string;
  rtp: RtpHandler;
  ws: WsClient;
  /** Our To-tag (generated at 180 Ringing) */
  localTag: string;
  /** Caller's From-tag (from INVITE From: …;tag=xxx) */
  remoteTag: string;
  /** Caller's SIP URI from INVITE From header (without tag) */
  remoteUri: string;
  /** Contact URI from INVITE (for BYE routing per RFC 3261 §12.2) */
  remoteTarget: string;
  remoteRtpIp: string;
  remoteRtpPort: number;
  localRtpPort: number;
  /** Next CSeq value for in-dialog requests (BYE) */
  cseq: number;
  log: Logger;
}

const USER_AGENT = 'audio-dock/0.1.0';

// ── Internal SIP helpers ──────────────────────────────────────────────────────

function extractHeader(raw: string, name: string): string {
  const m = raw.match(new RegExp(`^${name}:\\s*(.+)$`, 'im'));
  return m ? m[1].trim() : '';
}

/** Extract ALL Via headers from a raw SIP message (RFC 3261 §20.42) */
function extractAllVias(raw: string): string[] {
  const vias: string[] = [];
  for (const line of raw.split('\r\n')) {
    const m = line.match(/^Via:\s*(.+)$/i);
    if (m) vias.push(m[1].trim());
  }
  return vias;
}

interface BuildResponseParams {
  status: number;
  reason: string;
  vias: string[];  // ALL Via headers from the request — not just the first
  from: string;
  to: string;
  callId: string;
  cseq: string;
  sdpBody?: string;
  localIp?: string;
  sipUser?: string;
  sipDomain?: string;
  localSipPort?: number;
}

function buildResponse(p: BuildResponseParams): string {
  const lines = [
    `SIP/2.0 ${p.status} ${p.reason}`,
    ...p.vias.map((v) => `Via: ${v}`),
    `From: ${p.from}`,
    `To: ${p.to}`,
    `Call-ID: ${p.callId}`,
    `CSeq: ${p.cseq}`,
    `User-Agent: ${USER_AGENT}`,
  ];
  if (p.status === 200 && p.sdpBody) {
    lines.push(`Contact: <sip:${p.sipUser}@${p.localIp}:${p.localSipPort}>`);
    lines.push('Content-Type: application/sdp');
    lines.push(`Content-Length: ${Buffer.byteLength(p.sdpBody)}`);
    lines.push('');
    lines.push(p.sdpBody);
  } else {
    lines.push('Content-Length: 0', '');
  }
  return lines.join('\r\n') + '\r\n';
}

interface BuildByeParams {
  remoteTarget: string;
  localIp: string;
  localSipPort: number;
  fromUri: string;
  fromTag: string;
  toUri: string;
  toTag: string;
  callId: string;
  cseq: number;
}

function buildBye(p: BuildByeParams): string {
  const branch = 'z9hG4bK' + crypto.randomBytes(6).toString('hex');
  return [
    `BYE ${p.remoteTarget} SIP/2.0`,
    `Via: SIP/2.0/UDP ${p.localIp}:${p.localSipPort};branch=${branch};rport`,
    'Max-Forwards: 70',
    `From: <${p.fromUri}>;tag=${p.fromTag}`,
    `To: <${p.toUri}>;tag=${p.toTag}`,
    `Call-ID: ${p.callId}`,
    `CSeq: ${p.cseq} BYE`,
    `User-Agent: ${USER_AGENT}`,
    'Content-Length: 0',
    '',
  ].join('\r\n') + '\r\n';
}

/**
 * Extract [host, port] from a SIP Contact URI like sip:user@host:port
 * or sip:host:port. Falls back to ['127.0.0.1', 5060] on parse failure.
 */
function getLocalIp(): string {
  for (const ifaces of Object.values(os.networkInterfaces())) {
    for (const iface of ifaces ?? []) {
      if (!iface.internal && iface.family === 'IPv4') return iface.address;
    }
  }
  return '127.0.0.1';
}

function parseContactTarget(uri: string): [string, number] {
  const m = uri.match(/[@:]([a-zA-Z0-9._-]+):(\d+)/);
  if (m) return [m[1], parseInt(m[2], 10)];
  const host = uri.match(/[@:]([a-zA-Z0-9._-]+)/)?.[1] ?? '127.0.0.1';
  return [host, 5060];
}

// ── CallManager ───────────────────────────────────────────────────────────────

export class CallManager {
  private readonly sessions = new Map<string, CallSession>();
  /** Call-IDs currently being set up (between first INVITE and 200 OK stored) — dedup retransmissions */
  private readonly pendingInvites = new Set<string>();
  private readonly config: Config;
  private sipHandle!: SipHandle; // set via setSipHandle() before any calls arrive
  private readonly log: Logger;

  constructor(config: Config, log: Logger) {
    this.config = config;
    this.log = log;
  }

  /** Wire the SipHandle produced by createSipUserAgent into the manager */
  setSipHandle(handle: SipHandle): void {
    this.sipHandle = handle;
  }

  /** Returns SipCallbacks to pass to createSipUserAgent */
  getCallbacks(): SipCallbacks {
    return {
      onInvite: (raw: string, rinfo: RemoteInfo) => {
        // Fire-and-forget — errors are caught internally
        void this.handleInvite(raw, rinfo).catch((err: unknown) => {
          this.log.error({ err, event: 'invite_unhandled_error' }, 'Unhandled error in INVITE handler');
        });
      },
      onAck: (_raw: string, _rinfo: RemoteInfo) => {
        // ACK confirms our 200 OK; audio bridge is already running — no-op
      },
      onBye: (raw: string, rinfo: RemoteInfo) => {
        this.handleBye(raw, rinfo);
      },
    };
  }

  /** Terminate all active sessions in parallel — for graceful shutdown */
  async terminateAll(): Promise<void> {
    const sessions = [...this.sessions.values()];
    // Clear first so terminateSession idempotency guard doesn't block
    this.sessions.clear();
    await Promise.all(
      sessions.map(async (session) => {
        // Re-add transiently so terminateSession idempotency guard passes
        this.sessions.set(session.callId, session);
        this.terminateSession(session, 'shutdown', true);
      }),
    );
  }

  // ── Private: INVITE handler ─────────────────────────────────────────────────

  private async handleInvite(raw: string, rinfo: RemoteInfo): Promise<void> {
    // 1. Extract required headers
    const from = extractHeader(raw, 'From');
    const toHeader = extractHeader(raw, 'To');
    const sipCallId = extractHeader(raw, 'Call-ID');
    const cseq = extractHeader(raw, 'CSeq');

    // Dedup: ignore retransmissions while call is being set up or already established
    if (this.pendingInvites.has(sipCallId)) return;
    const existing = this.sessions.get(sipCallId);
    if (existing) {
      // Re-send 200 OK so sipgate stops retransmitting
      const localIp = this.config.SDP_CONTACT_IP ?? getLocalIp();
      const sdpAnswer = buildSdpAnswer(localIp, existing.localRtpPort);
      const ok200 = buildResponse({
        status: 200, reason: 'OK', vias: extractAllVias(raw), from,
        to: `${toHeader};tag=${existing.localTag}`,
        callId: sipCallId, cseq, sdpBody: sdpAnswer, localIp,
        sipUser: this.config.SIP_USER, sipDomain: this.config.SIP_DOMAIN,
        localSipPort: 5060,
      });
      this.sipHandle.sendRaw(Buffer.from(ok200), rinfo.port, rinfo.address);
      return;
    }
    this.pendingInvites.add(sipCallId);

    // remoteTag: caller's ;tag= from From header
    const remoteTag = raw.match(/;tag=([^\s;>]+)/i)?.[1] ?? '';
    // remoteUri: caller's SIP URI from From header (strip angle brackets)
    const remoteUri = raw.match(/From:\s*(?:<([^>]+)>|([^\s;]+))/i)?.[1] ?? '';
    // remoteContact: Contact URI for BYE routing
    const remoteContact =
      raw.match(/Contact:\s*<([^>]+)>/i)?.[1] ??
      `sip:${rinfo.address}:${rinfo.port}`;

    // P-Asserted-Identity carries the real caller number when From is anonymous
    const pai = extractHeader(raw, 'P-Asserted-Identity');
    const fromUri = pai
      ? (pai.match(/<([^>]+)>/)?.[1] ?? pai.trim())
      : remoteUri;

    // Request-URI contains the actual dialed number (first line of INVITE)
    const requestUri = raw.split('\r\n')[0].match(/INVITE\s+(\S+)\s+SIP/i)?.[1]
      ?? `sip:${this.config.SIP_USER}@${this.config.SIP_DOMAIN}`;
    const toUri = requestUri;

    try {
    // 2. Send 100 Trying immediately (no To-tag)
    const vias = extractAllVias(raw);
    const trying = buildResponse({
      status: 100,
      reason: 'Trying',
      vias,
      from,
      to: toHeader,
      callId: sipCallId,
      cseq,
    });
    this.sipHandle.sendRaw(Buffer.from(trying), rinfo.port, rinfo.address);

    // 3. Parse SDP body
    const sdpBody = raw.split('\r\n\r\n').slice(1).join('\r\n\r\n');
    const sdpOffer = parseSdpOffer(sdpBody);
    if (!sdpOffer || !sdpOffer.hasPcmu) {
      const resp488 = buildResponse({
        status: 488,
        reason: 'Not Acceptable Here',
        vias,
        from,
        to: toHeader,
        callId: sipCallId,
        cseq,
      });
      this.sipHandle.sendRaw(Buffer.from(resp488), rinfo.port, rinfo.address);
      this.log.warn({ event: 'invite_rejected_sdp', sipCallId }, 'INVITE rejected — missing or no PCMU in SDP offer');
      return;
    }

    // 4. Allocate RTP handler — must succeed before we commit to the call
    const callLog = createChildLogger({ component: 'call', callId: sipCallId });
    const rtp = await createRtpHandler({
      portMin: this.config.RTP_PORT_MIN,
      portMax: this.config.RTP_PORT_MAX,
      log: callLog,
    });
    rtp.setRemote(sdpOffer.remoteIp, sdpOffer.remotePort);

    // 5. Generate per-call identifiers
    const callSid = 'CA' + crypto.randomBytes(16).toString('hex');
    const streamSid = 'MZ' + crypto.randomBytes(16).toString('hex');
    const localTag = crypto.randomBytes(6).toString('hex');

    // 6. Send 180 Ringing (with To-tag)
    const ringing = buildResponse({
      status: 180,
      reason: 'Ringing',
      vias,
      from,
      to: `${toHeader};tag=${localTag}`,
      callId: sipCallId,
      cseq,
    });
    this.sipHandle.sendRaw(Buffer.from(ringing), rinfo.port, rinfo.address);

    // 7. Attempt WS connection (2-second timeout handled inside createWsClient)
    let ws: WsClient;
    try {
      ws = await createWsClient(
        this.config.WS_TARGET_URL,
        { streamSid, callSid, from: fromUri, to: toUri, sipCallId },
        callLog,
      );
    } catch (err) {
      callLog.warn({ err, event: 'ws_connect_failed' }, 'WS connect failed — sending 503');
      const resp503 = buildResponse({
        status: 503,
        reason: 'Service Unavailable',
        vias,
        from,
        to: toHeader,
        callId: sipCallId,
        cseq,
      });
      this.sipHandle.sendRaw(Buffer.from(resp503), rinfo.port, rinfo.address);
      rtp.dispose();
      return;
    }

    // 8. Send 200 OK with SDP answer
    const localIp = this.config.SDP_CONTACT_IP ?? getLocalIp();
    const sdpAnswer = buildSdpAnswer(localIp, rtp.localPort);
    const ok = buildResponse({
      status: 200,
      reason: 'OK',
      vias,
      from,
      to: `${toHeader};tag=${localTag}`,
      callId: sipCallId,
      cseq,
      sdpBody: sdpAnswer,
      localIp,
      sipUser: this.config.SIP_USER,
      sipDomain: this.config.SIP_DOMAIN,
      localSipPort: 5060,
    });
    this.sipHandle.sendRaw(Buffer.from(ok), rinfo.port, rinfo.address);

    // 9. Store session in Map
    const session: CallSession = {
      callId: sipCallId,
      callSid,
      streamSid,
      rtp,
      ws,
      localTag,
      remoteTag,
      remoteUri,
      remoteTarget: remoteContact,
      remoteRtpIp: sdpOffer.remoteIp,
      remoteRtpPort: sdpOffer.remotePort,
      localRtpPort: rtp.localPort,
      cseq: 1,
      log: callLog,
    };
    this.sessions.set(sipCallId, session);
    callLog.info({ event: 'call_started', from: fromUri, to: toUri }, 'Call started');

    // 10. Wire audio bridge AFTER session is stored
    // RTP audio → WS backend
    rtp.on('audio', (payload: Buffer) => ws.sendAudio(payload));
    // DTMF → WS backend
    rtp.on('dtmf', ({ digit }: { digit: string }) => ws.sendDtmf(digit));
    // WS backend audio → outbound RTP
    ws.onAudio((payload) => rtp.sendAudio(payload));
    // WS disconnect → send BYE to caller then clean up
    ws.onDisconnect(() => this.terminateSession(session, 'ws_disconnect', true));
    // Enable RTP forwarding — no audio forwarded until here
    rtp.startForwarding();
    } finally {
      this.pendingInvites.delete(sipCallId);
    }
  }

  // ── Private: BYE handler ────────────────────────────────────────────────────

  private handleBye(raw: string, rinfo: RemoteInfo): void {
    const callId = extractHeader(raw, 'Call-ID');
    const session = this.sessions.get(callId);
    if (!session) {
      this.log.warn({ event: 'bye_unknown_call', callId }, 'BYE for unknown call-id — ignoring');
      return;
    }

    // Send 200 OK to BYE (copy Via/From/To/Call-ID/CSeq from the BYE)
    const byeOk = buildResponse({
      status: 200,
      reason: 'OK',
      vias: extractAllVias(raw),
      from: extractHeader(raw, 'From'),
      to: extractHeader(raw, 'To'),
      callId,
      cseq: extractHeader(raw, 'CSeq'),
    });
    this.sipHandle.sendRaw(Buffer.from(byeOk), rinfo.port, rinfo.address);

    // Remote sent BYE — do NOT send our own BYE back (sendBye=false)
    this.terminateSession(session, 'remote_bye', false);
  }

  // ── Private: session teardown ───────────────────────────────────────────────

  /**
   * Tear down a CallSession.
   *
   * @param sendBye - true if we are initiating termination (WS disconnect,
   *   shutdown) and must send SIP BYE to the caller first; false if the remote
   *   already sent BYE and we must NOT send another BYE (protocol violation).
   */
  private terminateSession(session: CallSession, reason: string, sendBye: boolean): void {
    if (!this.sessions.has(session.callId)) return; // idempotent
    this.sessions.delete(session.callId);
    session.log.info({ event: 'call_ended', reason, sendBye }, 'Call terminated');

    if (sendBye) {
      const localIp = this.config.SDP_CONTACT_IP ?? getLocalIp();
      const bye = buildBye({
        remoteTarget: session.remoteTarget,
        localIp,
        localSipPort: 5060,
        fromUri: `sip:${this.config.SIP_USER}@${this.config.SIP_DOMAIN}`,
        fromTag: session.localTag,
        toUri: session.remoteUri,
        toTag: session.remoteTag,
        callId: session.callId,
        cseq: session.cseq,
      });
      const [byeHost, byePort] = parseContactTarget(session.remoteTarget);
      this.sipHandle.sendRaw(Buffer.from(bye), byePort, byeHost);
      session.log.info({ event: 'bye_sent', target: session.remoteTarget }, 'SIP BYE sent');
    }

    session.ws.stop();     // sends stop event then closes WS
    session.rtp.dispose(); // closes dgram socket
  }
}
