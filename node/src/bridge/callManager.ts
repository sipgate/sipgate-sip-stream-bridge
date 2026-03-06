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
import type { Metrics } from '../observability/metrics.js';
import { createRtpHandler, type RtpHandler } from '../rtp/rtpHandler.js';
import { buildSdpAnswer, parseSdpOffer, selectAudioCodec } from '../sip/sdp.js';
import type { SipCallbacks, SipHandle } from '../sip/userAgent.js';
import { createWsClient, type WsCallParams, type WsClient } from '../ws/wsClient.js';

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
  /** 200 OK retransmit timer handle — cleared on ACK (RFC 3261 §13.3.1.4) */
  okRetransmitTimer: ReturnType<typeof setTimeout> | null;
  /** True while WS reconnect loop is running — gates RTP audio forwarding (WSR-03) */
  wsReconnecting: boolean;
  /** Silence injection interval during WS reconnect — cleared on teardown to prevent send-after-dispose crash */
  silenceInterval: ReturnType<typeof setInterval> | null;
  /** SDP answer sent in 200 OK — reused for retransmission to avoid re-negotiation */
  sdpAnswer: string;
  /** Silence byte for the negotiated codec — used for NAT hole-punch and reconnect silence */
  silenceByte: number;
}

const USER_AGENT = 'sipgate-sip-stream-bridge/0.1.0';

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

/** Extract ALL Record-Route headers (RFC 3261 §12.1.2 — MUST be echoed in 200 OK) */
function extractAllRecordRoutes(raw: string): string[] {
  const rrs: string[] = [];
  for (const line of raw.split('\r\n')) {
    const m = line.match(/^Record-Route:\s*(.+)$/i);
    if (m) rrs.push(m[1].trim());
  }
  return rrs;
}

interface BuildResponseParams {
  status: number;
  reason: string;
  vias: string[];  // ALL Via headers from the request — not just the first
  from: string;
  to: string;
  callId: string;
  cseq: string;
  recordRoutes?: string[]; // Copy from INVITE (RFC 3261 §12.1.2 MUST)
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
  if (p.recordRoutes?.length) {
    lines.push(...p.recordRoutes.map((rr) => `Record-Route: ${rr}`));
  }
  if (p.status === 200 && p.sdpBody) {
    lines.push(`Contact: <sip:${p.sipUser}@${p.localIp}:${p.localSipPort}>`);
    lines.push('Content-Type: application/sdp');
    lines.push(`Content-Length: ${Buffer.byteLength(p.sdpBody)}`);
    lines.push(''); // blank line — body follows directly, no extra \r\n appended
    return lines.join('\r\n') + '\r\n' + p.sdpBody;
  } else {
    lines.push('Content-Length: 0', '');
    return lines.join('\r\n') + '\r\n';
  }
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
  /** Call-IDs for which CANCEL was received before 200 OK was sent */
  private readonly cancelledInvites = new Set<string>();
  private readonly config: Config;
  private sipHandle!: SipHandle; // set via setSipHandle() before any calls arrive
  private readonly log: Logger;
  private readonly metrics?: Metrics;
  /** Set to true during terminateAll() — causes new INVITEs to receive 503 (LCY-01) */
  private isShuttingDown = false;

  constructor(config: Config, log: Logger, metrics?: Metrics) {
    this.config = config;
    this.log = log;
    this.metrics = metrics;
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
      onAck: (raw: string, _rinfo: RemoteInfo) => {
        // ACK received — stop 200 OK retransmission
        const callId = extractHeader(raw, 'Call-ID');
        const session = this.sessions.get(callId);
        if (session?.okRetransmitTimer !== null && session?.okRetransmitTimer !== undefined) {
          clearTimeout(session.okRetransmitTimer);
          session.okRetransmitTimer = null;
          session.log.debug({ event: 'ack_received' }, 'ACK received — 200 OK retransmit stopped');
        }
      },
      onBye: (raw: string, rinfo: RemoteInfo) => {
        this.handleBye(raw, rinfo);
      },
      onCancel: (raw: string, rinfo: RemoteInfo) => {
        this.handleCancel(raw, rinfo);
      },
    };
  }

  /** Terminate all active sessions in parallel — for graceful shutdown */
  async terminateAll(): Promise<void> {
    this.isShuttingDown = true; // reject new INVITEs with 503 during drain (LCY-01)
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

    // Reject new INVITEs during graceful shutdown drain (LCY-01 / OBS-03 §21.5.4)
    if (this.isShuttingDown) {
      const resp503 = buildResponse({
        status: 503, reason: 'Service Unavailable',
        vias: extractAllVias(raw), from, to: toHeader, callId: sipCallId, cseq,
      });
      this.sipHandle.sendRaw(Buffer.from(resp503), rinfo.port, rinfo.address);
      return;
    }

    // Dedup: ignore retransmissions while call is being set up or already established
    if (this.pendingInvites.has(sipCallId)) return;
    const existing = this.sessions.get(sipCallId);
    if (existing) {
      // Re-send 200 OK so sipgate stops retransmitting (reuse stored sdpAnswer — no re-negotiation)
      const localIp = this.config.SDP_CONTACT_IP ?? getLocalIp();
      const ok200 = buildResponse({
        status: 200, reason: 'OK', vias: extractAllVias(raw), from,
        to: `${toHeader};tag=${existing.localTag}`,
        callId: sipCallId, cseq, sdpBody: existing.sdpAnswer, localIp,
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
    // Skip optional display name (e.g. "Alice" <sip:...>) before the angle-bracket URI
    const remoteUri = raw.match(/^From:[^<\r\n]*<([^>]+)>/im)?.[1] ?? '';
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
    const recordRoutes = extractAllRecordRoutes(raw);
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
    const supportedPts = new Set([0, 8, 9]);
    if (!sdpOffer || !sdpOffer.audioPts.some((pt) => supportedPts.has(pt))) {
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
      this.log.warn({ event: 'invite_rejected_sdp', sipCallId }, 'INVITE rejected — no supported codec in SDP offer');
      return;
    }

    // Select negotiated codec based on AUDIO_MODE
    const codecInfo = selectAudioCodec(sdpOffer.audioPts, this.config.AUDIO_MODE);

    // 4. Allocate RTP handler — must succeed before we commit to the call
    const callLog = createChildLogger({ component: 'call', callId: sipCallId });
    const rtp = await createRtpHandler({
      portMin: this.config.RTP_PORT_MIN,
      portMax: this.config.RTP_PORT_MAX,
      log: callLog,
      audioPt: codecInfo.pt,
      dtmfPt: sdpOffer.dtmfPt,
      silenceByte: codecInfo.silenceByte,
    });
    rtp.setRemote(sdpOffer.remoteIp, sdpOffer.remotePort);
    // NAT hole-punch: send one silence packet outbound so the router creates a
    // mapping for this port. sipgate's inbound RTP will then traverse the same rule.
    rtp.sendAudio(Buffer.alloc(160, rtp.silenceByte));

    // Check if CANCEL arrived while we were awaiting RTP allocation
    if (this.cancelledInvites.has(sipCallId)) {
      rtp.dispose();
      return; // 487 already sent by handleCancel
    }

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
        {
          streamSid,
          callSid,
          from: fromUri,
          to: toUri,
          sipCallId,
          mediaFormat: { encoding: codecInfo.encoding, sampleRate: codecInfo.sampleRate, channels: 1 },
        },
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

    // Check if CANCEL arrived while we were awaiting WS connection
    if (this.cancelledInvites.has(sipCallId)) {
      ws.stop();
      rtp.dispose();
      return; // 487 already sent by handleCancel
    }

    // 8. Send 200 OK with SDP answer
    const localIp = this.config.SDP_CONTACT_IP ?? getLocalIp();
    const sdpAnswer = buildSdpAnswer(localIp, rtp.localPort, sdpOffer, this.config.AUDIO_MODE);
    const ok = buildResponse({
      status: 200,
      reason: 'OK',
      vias,
      recordRoutes,
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
    const okBuf = Buffer.from(ok);
    callLog.info(
      { event: '200_ok_sent', dest: `${rinfo.address}:${rinfo.port}`, recordRoutes: recordRoutes.length },
      '200 OK sent',
    );
    this.sipHandle.sendRaw(okBuf, rinfo.port, rinfo.address);

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
      okRetransmitTimer: null,
      wsReconnecting: false,
      silenceInterval: null,
      sdpAnswer,
      silenceByte: rtp.silenceByte,
    };
    this.sessions.set(sipCallId, session);
    this.metrics?.incActiveCalls();

    // 9a. Retransmit 200 OK until ACK arrives (RFC 3261 §13.3.1.4)
    // Timer doubles from T1=500ms up to T2=4000ms per attempt.
    let retransmitInterval = 500; // T1
    const scheduleOkRetransmit = (): void => {
      if (!this.sessions.has(sipCallId)) return; // session gone (BYE before ACK)
      this.sipHandle.sendRaw(okBuf, rinfo.port, rinfo.address);
      callLog.info({ event: '200_ok_retransmit', interval: retransmitInterval }, '200 OK retransmitted');
      retransmitInterval = Math.min(retransmitInterval * 2, 4000); // cap at T2
      session.okRetransmitTimer = setTimeout(scheduleOkRetransmit, retransmitInterval);
    };
    session.okRetransmitTimer = setTimeout(scheduleOkRetransmit, retransmitInterval);
    callLog.info({ event: 'call_started', from: fromUri, to: toUri }, 'Call started');

    // 10. Wire audio bridge AFTER session is stored
    // RTP audio → WS backend (route via session.ws so post-reconnect handler picks up new WsClient)
    let firstRtp = true;
    rtp.on('audio', (payload: Buffer) => {
      this.metrics?.incRtpRx();
      if (session.wsReconnecting) return; // drop during reconnect window — WSR-03
      if (firstRtp) {
        callLog.info({ event: 'first_rtp_audio', bytes: payload.length }, 'First RTP audio packet received from sipgate');
        firstRtp = false;
      }
      session.ws.sendAudio(payload);
    });
    // DTMF → WS backend (route via session.ws so post-reconnect handler picks up new WsClient)
    rtp.on('dtmf', ({ digit }: { digit: string }) => session.ws.sendDtmf(digit));
    // WS backend audio → outbound RTP
    let firstWsAudio = true;
    ws.onAudio((payload) => {
      if (firstWsAudio) {
        callLog.info({ event: 'first_ws_audio', bytes: payload.length }, 'First audio from WS backend — sending to sipgate');
        firstWsAudio = false;
      }
      this.metrics?.incRtpTx();
      rtp.sendAudio(payload);
    });
    // Mark events from outbound drain → echo back to WS backend
    // Routed via session.ws so post-reconnect the handler picks up the new WsClient
    ws.onMark((markName) => {
      session.ws.sendMark(markName);
    });
    // WS disconnect → start reconnect loop (keeps SIP call alive during transient WS drops)
    // Capture params here so the reconnect loop can re-call createWsClient with same identifiers (WSR-02)
    const wsParams: WsCallParams = {
      streamSid: session.streamSid,
      callSid: session.callSid,
      from: fromUri,
      to: toUri,
      sipCallId,
      mediaFormat: { encoding: codecInfo.encoding, sampleRate: codecInfo.sampleRate, channels: 1 },
    };
    ws.onDisconnect(() => {
      session.wsReconnecting = true;
      void this.startWsReconnectLoop(session, wsParams).catch((err: unknown) => {
        session.log.error({ err }, 'Reconnect loop unhandled error');
      });
    });
    // Enable RTP forwarding — no audio forwarded until here
    rtp.startForwarding();
    } finally {
      this.pendingInvites.delete(sipCallId);
      this.cancelledInvites.delete(sipCallId);
    }
  }

  // ── Private: CANCEL handler ─────────────────────────────────────────────────

  private handleCancel(raw: string, rinfo: RemoteInfo): void {
    const callId = extractHeader(raw, 'Call-ID');
    const vias = extractAllVias(raw);
    const from = extractHeader(raw, 'From');
    const to = extractHeader(raw, 'To');
    const cseq = extractHeader(raw, 'CSeq');

    // Always respond 200 OK to CANCEL (RFC 3261 §9.2)
    const cancelOk = buildResponse({ status: 200, reason: 'OK', vias, from, to, callId, cseq });
    this.sipHandle.sendRaw(Buffer.from(cancelOk), rinfo.port, rinfo.address);

    const session = this.sessions.get(callId);
    if (session) {
      // 200 OK was sent but never reached the caller (otherwise they'd send BYE, not CANCEL).
      // Terminate the session and let the WS backend know via the stop event.
      this.log.info({ event: 'cancel_after_200', callId }, 'CANCEL received after 200 OK — terminating session');
      this.terminateSession(session, 'remote_cancel', false);
      return;
    }

    if (this.pendingInvites.has(callId)) {
      // Still setting up — mark as cancelled so handleInvite sends 487 after its next await
      this.cancelledInvites.add(callId);
      const resp487 = buildResponse({
        status: 487,
        reason: 'Request Terminated',
        vias: extractAllVias(raw), from, to,
        callId,
        cseq: extractHeader(raw, 'CSeq').replace('CANCEL', 'INVITE'),
      });
      this.sipHandle.sendRaw(Buffer.from(resp487), rinfo.port, rinfo.address);
      this.log.info({ event: 'cancel_pending', callId }, 'CANCEL received during setup — 487 sent');
      return;
    }

    this.log.warn({ event: 'cancel_unknown', callId }, 'CANCEL for unknown call-id');
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
    this.metrics?.decActiveCalls();

    // Cancel any pending 200 OK retransmit timer
    if (session.okRetransmitTimer !== null) {
      clearTimeout(session.okRetransmitTimer);
      session.okRetransmitTimer = null;
    }

    // Cancel silence injection interval if WS reconnect loop is in progress —
    // prevents ERR_SOCKET_DGRAM_NOT_RUNNING if BYE arrives while the loop is sleeping
    if (session.silenceInterval !== null) {
      clearInterval(session.silenceInterval);
      session.silenceInterval = null;
    }

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

  // ── Private: WS reconnect loop ──────────────────────────────────────────────

  /**
   * Attempt to reconnect the WebSocket with exponential backoff.
   *
   * - Sends μ-law silence to caller every 20ms throughout the reconnect window (WSR-01).
   * - Re-calls createWsClient with the same params so backend receives fresh
   *   connected + start events on reconnect (WSR-02).
   * - Inbound RTP is dropped via session.wsReconnecting gate (WSR-03).
   * - Exits early if caller sends BYE while loop is sleeping (anti-zombie guard).
   * - Calls terminateSession on budget exhaustion (30s) to send SIP BYE.
   */
  private async startWsReconnectLoop(
    session: CallSession,
    params: WsCallParams,
  ): Promise<void> {
    const BUDGET_MS = 30_000;
    const CAP_MS    = 4_000;
    const started   = Date.now();
    let   delay     = 1_000;

    // Send codec-appropriate silence to caller throughout reconnect window — prevents dead-air.
    // Stored on session so terminateSession can clear it if BYE arrives mid-sleep.
    session.silenceInterval = setInterval(() => {
      this.metrics?.incRtpTx();
      session.rtp.sendAudio(Buffer.alloc(160, session.silenceByte));
    }, 20);

    const cleanup = (): void => {
      if (session.silenceInterval !== null) {
        clearInterval(session.silenceInterval);
        session.silenceInterval = null;
      }
    };

    const attempt = async (n: number): Promise<void> => {
      // BYE race guard: if session was terminated externally, exit without zombie reconnect
      if (!this.sessions.has(session.callId)) {
        cleanup();
        return;
      }

      this.metrics?.incWsReconnect();
      session.log.info({ event: 'ws_reconnect_attempt', attempt: n, delay }, 'Attempting WS reconnect');

      try {
        const newWs = await createWsClient(this.config.WS_TARGET_URL, params, session.log);
        // Success: clear silence, reset flag, swap in new WsClient
        cleanup();
        session.wsReconnecting = false;
        session.ws = newWs;

        // Re-wire WS audio → RTP (old ws.onAudio listeners are on the dead socket and won't fire)
        newWs.onAudio((payload) => {
          this.metrics?.incRtpTx();
          session.rtp.sendAudio(payload);
        });

        // Re-wire mark handler on reconnect — session.ws is already updated to newWs above
        newWs.onMark((markName) => {
          session.ws.sendMark(markName);
        });

        // Re-wire disconnect handler recursively so future drops are also handled
        newWs.onDisconnect(() => {
          session.wsReconnecting = true;
          void this.startWsReconnectLoop(session, params).catch((err: unknown) => {
            session.log.error({ err }, 'Reconnect loop unhandled error');
          });
        });

        session.log.info({ event: 'ws_reconnected', attempt: n }, 'WS reconnected — resuming audio bridge');
      } catch {
        const elapsed = Date.now() - started;
        if (elapsed + delay >= BUDGET_MS) {
          cleanup();
          session.log.warn({ event: 'ws_reconnect_failed', elapsed }, 'WS reconnect budget exhausted — sending BYE');
          this.terminateSession(session, 'ws_reconnect_failed', true);
          return;
        }
        session.log.info({ event: 'ws_reconnect_wait', delay, elapsed }, 'WS reconnect failed — waiting before retry');
        await new Promise<void>((res) => setTimeout(res, delay));
        delay = Math.min(delay * 2, CAP_MS);
        await attempt(n + 1);
      }
    };

    await attempt(1);
  }
}
