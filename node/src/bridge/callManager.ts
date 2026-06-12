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
import { listenParts } from '../config/listenAddr.js';
import { createChildLogger } from '../logger/index.js';
import type { Metrics } from '../observability/metrics.js';
import { createRtpHandler, type RtpHandler } from '../rtp/rtpHandler.js';
import { buildSdpAnswer, parseSdpOffer, selectAudioCodec } from '../sip/sdp.js';
import type { SipCallbacks, SipHandle } from '../sip/userAgent.js';
import { createWsClient, type WsCallParams, type WsClient } from '../ws/wsClient.js';
import { deriveAccountSid } from '../identity/sid.js';
import { USER_AGENT } from '../version.js';
import type { OutboundDialog } from '../sip/dialog.js';
import type { StatusClient, CallbackEvent } from '../webhook/status.js';
import { buildStatusForm } from '../webhook/callbackForm.js';
import { subscriptionMatches } from '../webhook/subscription.js';
import { CallState, isActiveState, twilioStatus } from './state.js';

/** Per-call status-callback subscription (operator default or REST-supplied). */
export interface StatusCallbackConfig {
  url: string;
  method: string;
  events: Set<string>;
  /** Operator-trusted (bypasses SSRF guard) — true only for the configured default. */
  trusted: boolean;
}

// ── Public types ──────────────────────────────────────────────────────────────

/**
 * Read-only projection of a call for the REST control plane (active sessions and
 * recently-terminated snapshots share this shape). Field names/types line up with
 * the api/json.ts CallView serializer input.
 */
export interface BridgeCall {
  callSid: string;
  accountSid: string;
  from: string;
  to: string;
  status: string;
  direction: string;
  startTime: Date | null;
  endTime: Date | null;
  duration: number | null;
  answeredBy: string | null;
  parentCallSid: string | null;
}

/** Immutable snapshot retained after teardown so GET /Calls/{Sid} still resolves. */
interface TerminatedCall extends BridgeCall {
  terminatedAt: number; // epoch ms — for TTL sweep
}

/** Recently-terminated retention window + sweep cadence (mirrors Go v3). */
const RECENTLY_TERMINATED_TTL_MS = 5 * 60_000;
const RECENTLY_TERMINATED_SWEEP_MS = 30_000;

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
  /** Our dialog-local URI = the INVITE To-header URI (From-URI of our BYE) */
  localUri: string;
  /** Contact URI from INVITE (for BYE routing per RFC 3261 §12.2) */
  remoteTarget: string;
  /** Record-Route set from the INVITE — the in-dialog route set for our BYE */
  recordRoutes: string[];
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

  // ── v3 control-plane metadata ───────────────────────────────────────────────
  /** AC… account identifier (derived once from SIP_USER) */
  accountSid: string;
  /** Caller number/URI (From, or P-Asserted-Identity when present) */
  fromNumber: string;
  /** Dialed number/URI (Request-URI) */
  toNumber: string;
  /** Wall-clock at INVITE acceptance */
  startTime: Date;
  /** Wall-clock when the call was answered (200 OK) — for CallDuration */
  answeredAt: Date | null;
  /** Lifecycle state (drives REST status + mid-call modify gating) */
  state: CallState;
  /** Teardown reason recorded at termination (maps to terminal Twilio status) */
  terminalReason: string | null;
  /** Final SIP response code captured for the call, if any */
  sipFinalCode: number;
  /** Monotonic per-call status-callback sequence counter (0-indexed) */
  seqCounter: number;
  /** Active status-callback subscription, or null if none */
  statusCallback: StatusCallbackConfig | null;

  // ── v3 B2BUA <Dial> forwarding ──────────────────────────────────────────────
  /** Callee-leg RTP handler while forwarding; null in streaming mode. Audio from
   *  the caller is relayed here instead of to the WS once set. */
  forwardRtp: RtpHandler | null;
  /** Active outbound dialog while forwarding (for dual-leg BYE on teardown). */
  forwardDialog: OutboundDialog | null;
  /** Resolver the forwarder installs so a caller-initiated teardown ends the dial. */
  onForwardEnd: (() => void) | null;
  /** <Dial hangupOnStar> — caller pressing '*' during forwarding ends the dial. */
  hangupOnStar: boolean;
}

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
  /** Record-Route set from the INVITE (document order); routed in reverse per RFC 3261 §12.2. */
  recordRoutes?: string[];
}

export function buildBye(p: BuildByeParams): string {
  const branch = 'z9hG4bK' + crypto.randomBytes(6).toString('hex');
  const lines = [
    `BYE ${p.remoteTarget} SIP/2.0`,
    `Via: SIP/2.0/UDP ${p.localIp}:${p.localSipPort};branch=${branch};rport`,
    'Max-Forwards: 70',
    `From: <${p.fromUri}>;tag=${p.fromTag}`,
    `To: <${p.toUri}>;tag=${p.toTag}`,
    `Call-ID: ${p.callId}`,
    `CSeq: ${p.cseq} BYE`,
  ];
  // In-dialog routing (loose routing): echo the INVITE's Record-Route set as Route
  // headers IN RECEIVED ORDER. sipgate prepends each proxy, so RR[0] is the proxy
  // adjacent to us (it delivered the INVITE and accepted our 200 OK) and the set
  // already reads in the direction the BYE travels back to the caller. The BYE is
  // sent to RR[0]; without this it bypasses the proxies → sipgate 404 "unknown
  // domain" / drop, and the caller leg lingers.
  if (p.recordRoutes?.length) {
    for (const rr of p.recordRoutes) lines.push(`Route: ${rr}`);
  }
  lines.push(`User-Agent: ${USER_AGENT}`, 'Content-Length: 0', '');
  return lines.join('\r\n') + '\r\n';
}

/**
 * WS reconnect is permitted ONLY for a still-registered session that is actively
 * streaming. It must be refused during the <Dial> privacy gate, forwarding, and
 * teardown — otherwise the intentional ws.close() would be treated as a transient
 * drop and rejoin the bot to a forwarded call (privacy leak).
 */
export function shouldReconnectWs(state: CallState, sessionPresent: boolean): boolean {
  return sessionPresent && state === CallState.Streaming;
}

/**
 * Resolve the transport target for an in-dialog request. With a route set, send
 * to the topmost (first) Record-Route — the proxy adjacent to us, which is also
 * where our 200 OK was accepted. Otherwise send to the remote Contact.
 */
export function byeSendTarget(recordRoutes: string[], remoteTarget: string): [string, number] {
  if (recordRoutes.length > 0) {
    return parseContactTarget(recordRoutes[0]);
  }
  return parseContactTarget(remoteTarget);
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
  /** CallSid → live session, for the REST control plane (active calls only) */
  private readonly callSidIdx = new Map<string, CallSession>();
  /** CallSid → snapshot of recently-terminated calls (TTL-swept) */
  private readonly recentlyTerminated = new Map<string, TerminatedCall>();
  /** Call-IDs currently being set up (between first INVITE and 200 OK stored) — dedup retransmissions */
  private readonly pendingInvites = new Set<string>();
  /** Call-IDs for which CANCEL was received before 200 OK was sent */
  private readonly cancelledInvites = new Set<string>();
  private readonly config: Config;
  /** AC… account identifier, derived once from SIP_USER */
  readonly accountSid: string;
  /** Local SIP port (Via/Contact) from SIP_LISTEN_ADDR. */
  private readonly localSipPort: number;
  private sipHandle!: SipHandle; // set via setSipHandle() before any calls arrive
  private readonly log: Logger;
  private readonly metrics?: Metrics;
  private statusClient?: StatusClient;
  /** Set to true during terminateAll() — causes new INVITEs to receive 503 (LCY-01) */
  private isShuttingDown = false;
  private readonly sweepTimer: ReturnType<typeof setInterval>;

  constructor(config: Config, log: Logger, metrics?: Metrics) {
    this.config = config;
    this.log = log;
    this.metrics = metrics;
    this.accountSid = deriveAccountSid(config.SIP_USER);
    this.localSipPort = listenParts(config.SIP_LISTEN_ADDR).port;
    // Sweep expired terminated-call snapshots. unref() so the timer never keeps
    // the process (or a test runner) alive on its own.
    this.sweepTimer = setInterval(() => this.sweepRecentlyTerminated(), RECENTLY_TERMINATED_SWEEP_MS);
    this.sweepTimer.unref?.();
  }

  private sweepRecentlyTerminated(): void {
    const cutoff = Date.now() - RECENTLY_TERMINATED_TTL_MS;
    for (const [callSid, snap] of this.recentlyTerminated) {
      if (snap.terminatedAt < cutoff) this.recentlyTerminated.delete(callSid);
    }
  }

  /** Wire the SipHandle produced by createSipUserAgent into the manager */
  setSipHandle(handle: SipHandle): void {
    this.sipHandle = handle;
  }

  /** Wire the StatusClient used to deliver call-progress callbacks. */
  setStatusClient(client: StatusClient): void {
    this.statusClient = client;
  }

  /** Build the operator-default StatusCallback subscription, if configured. */
  private defaultStatusCallback(): StatusCallbackConfig | null {
    const url = this.config.STATUS_CALLBACK_DEFAULT_URL;
    if (!url) return null;
    const events = new Set(
      this.config.STATUS_CALLBACK_DEFAULT_EVENTS.split(',')
        .map((e) => e.trim())
        .filter((e) => e.length > 0),
    );
    return { url, method: this.config.STATUS_CALLBACK_DEFAULT_METHOD, events, trusted: true };
  }

  /** Replace a session's status-callback subscription (REST StatusCallback=). */
  setStatusCallback(session: CallSession, cfg: StatusCallbackConfig | null): void {
    session.statusCallback = cfg;
  }

  /**
   * Emit a single status-callback event if the session has a matching subscription.
   * SequenceNumber is monotonic per call; terminal events carry CallDuration.
   */
  private emitStatusEvent(
    session: CallSession,
    event: string,
    callStatus: string,
    extras?: { callDurationSec?: number; sipResponseCode?: number },
  ): void {
    const sub = session.statusCallback;
    if (!sub || !this.statusClient) return;
    if (!subscriptionMatches(sub.events, event)) return;

    const form = buildStatusForm({
      callSid: session.callSid,
      accountSid: session.accountSid,
      from: session.fromNumber,
      to: session.toNumber,
      direction: 'inbound',
      callStatus,
      sequenceNumber: session.seqCounter++,
      timestamp: new Date(),
      callDurationSec: extras?.callDurationSec,
      sipResponseCode: extras?.sipResponseCode,
    });
    const evt: CallbackEvent = {
      url: sub.url,
      method: sub.method,
      form,
      event,
      trusted: sub.trusted,
    };
    this.statusClient.enqueue(session.callSid, evt);
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

  // ── v3 control-plane query + modify surface ─────────────────────────────────

  /** Build the read-only BridgeCall projection from a live session. */
  private toBridgeCall(s: CallSession, endTime?: Date): BridgeCall {
    const end = endTime ?? null;
    const durationSec =
      end && s.answeredAt ? Math.max(0, Math.round((end.getTime() - s.answeredAt.getTime()) / 1000)) : null;
    return {
      callSid: s.callSid,
      accountSid: s.accountSid,
      from: s.fromNumber,
      to: s.toNumber,
      status: twilioStatus(s.state, s.terminalReason ?? undefined),
      direction: 'inbound',
      startTime: s.startTime,
      endTime: end,
      duration: durationSec,
      answeredBy: null,
      parentCallSid: null,
    };
  }

  /** List active + recently-terminated calls, most-recently-started first. */
  list(): BridgeCall[] {
    const out: BridgeCall[] = [];
    for (const s of this.sessions.values()) out.push(this.toBridgeCall(s));
    for (const t of this.recentlyTerminated.values()) out.push(t);
    out.sort((a, b) => (b.startTime?.getTime() ?? 0) - (a.startTime?.getTime() ?? 0));
    return out;
  }

  /** Look up a call by CallSid (active wins over a stale terminated snapshot). */
  getByCallSid(callSid: string): BridgeCall | undefined {
    const live = this.callSidIdx.get(callSid);
    if (live) return this.toBridgeCall(live);
    return this.recentlyTerminated.get(callSid);
  }

  /** Resolve the live session for mid-call modify; undefined if not active. */
  getSessionByCallSid(callSid: string): CallSession | undefined {
    return this.callSidIdx.get(callSid);
  }

  /** True while the session is modifiable (neither terminated nor hung up). */
  isActive(callSid: string): boolean {
    const s = this.callSidIdx.get(callSid);
    return s !== undefined && isActiveState(s.state);
  }

  /**
   * Privacy gate primitive: close the WS stream cleanly (bot disconnected) while
   * leaving the RTP path alive, so a subsequent <Dial> can relay media. Idempotent.
   */
  closeStream(session: CallSession, reason: string): void {
    if (session.state === CallState.DialingOut || !isActiveState(session.state)) return;
    session.state = CallState.DialingOut;
    if (session.silenceInterval !== null) {
      clearInterval(session.silenceInterval);
      session.silenceInterval = null;
    }
    session.ws.stop(); // sends stop event (reason carried in logs) then closes WS
    session.log.info({ event: 'stream_closed', reason }, 'WS stream closed (privacy gate)');
  }

  /**
   * REST-driven termination by CallSid (sends BYE to the caller). Returns true if
   * a live session was found and torn down. Idempotent for already-terminated calls.
   */
  terminateByCallSid(callSid: string, reason: string): boolean {
    const session = this.callSidIdx.get(callSid);
    if (!session) return false;
    this.terminateSession(session, reason, true);
    return true;
  }

  /** Terminate all active sessions in parallel — for graceful shutdown */
  async terminateAll(): Promise<void> {
    this.isShuttingDown = true; // reject new INVITEs with 503 during drain (LCY-01)
    clearInterval(this.sweepTimer);
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
        localSipPort: this.localSipPort,
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
    // localUri: the To-header URI we were addressed as — this is the dialog's
    // local URI and MUST be the From-URI of any in-dialog request we originate.
    const localUri =
      toHeader.match(/<([^>]+)>/)?.[1] ?? toHeader.replace(/;.*$/, '').trim();
    // Dialog headers retained for the in-dialog BYE route set (RFC 3261 §12.2).
    // Logged at debug only — carries phone numbers, never emitted at info.
    this.log.debug(
      {
        event: 'dialog_headers',
        sipCallId,
        toHeader,
        contact: remoteContact,
        recordRoutes: extractAllRecordRoutes(raw),
      },
      'inbound dialog headers',
    );

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

    // Determine whether SRTP will be negotiated: enabled in config AND offered by caller.
    const useSrtp = this.config.SRTP_ENABLED && sdpOffer.isSrtp;

    // Generate local SRTP key+salt before allocating the RTP handler so both the
    // handler and the SDP answer use the same keying material.
    let localSrtpKey: Buffer | undefined;
    let localSrtpSalt: Buffer | undefined;
    if (useSrtp) {
      localSrtpKey = crypto.randomBytes(16);
      localSrtpSalt = crypto.randomBytes(14);
    }

    // 4. Allocate RTP handler — must succeed before we commit to the call
    const callLog = createChildLogger({ component: 'call', callId: sipCallId });
    if (this.config.SRTP_ENABLED && !sdpOffer.isSrtp) {
      callLog.warn({ event: 'srtp_fallback', sipCallId }, 'SRTP desired but caller offered plain RTP/AVP — proceeding unencrypted');
    }
    const rtp = await createRtpHandler({
      portMin: this.config.RTP_PORT_MIN,
      portMax: this.config.RTP_PORT_MAX,
      log: callLog,
      audioPt: codecInfo.pt,
      dtmfPt: sdpOffer.dtmfPt,
      silenceByte: codecInfo.silenceByte,
      localSrtpKey,
      localSrtpSalt,
      remoteSrtpKey: sdpOffer.remoteSrtpKey,
      remoteSrtpSalt: sdpOffer.remoteSrtpSalt,
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
    const { sdp: sdpAnswerStr } = buildSdpAnswer(
      localIp, rtp.localPort, sdpOffer, this.config.AUDIO_MODE,
      this.config.SRTP_ENABLED, localSrtpKey, localSrtpSalt,
    );
    const ok = buildResponse({
      status: 200,
      reason: 'OK',
      vias,
      recordRoutes,
      from,
      to: `${toHeader};tag=${localTag}`,
      callId: sipCallId,
      cseq,
      sdpBody: sdpAnswerStr,
      localIp,
      sipUser: this.config.SIP_USER,
      sipDomain: this.config.SIP_DOMAIN,
      localSipPort: this.localSipPort,
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
      recordRoutes,
      localUri,
      remoteRtpIp: sdpOffer.remoteIp,
      remoteRtpPort: sdpOffer.remotePort,
      localRtpPort: rtp.localPort,
      cseq: 1,
      log: callLog,
      okRetransmitTimer: null,
      wsReconnecting: false,
      silenceInterval: null,
      sdpAnswer: sdpAnswerStr,
      silenceByte: rtp.silenceByte,
      accountSid: this.accountSid,
      fromNumber: fromUri,
      toNumber: toUri,
      startTime: new Date(),
      answeredAt: new Date(),
      state: CallState.Streaming,
      terminalReason: null,
      sipFinalCode: 200,
      seqCounter: 0,
      statusCallback: this.defaultStatusCallback(),
      forwardRtp: null,
      forwardDialog: null,
      onForwardEnd: null,
      hangupOnStar: false,
    };
    this.sessions.set(sipCallId, session);
    this.callSidIdx.set(callSid, session);
    this.metrics?.incActiveCalls();

    // Lifecycle status callbacks for the inbound (auto-answered) call. These
    // collapse to near-instant for a streaming bridge; subscription filtering
    // decides which actually POST.
    this.emitStatusEvent(session, 'initiated', 'queued');
    this.emitStatusEvent(session, 'ringing', 'ringing');
    this.emitStatusEvent(session, 'answered', 'in-progress');

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
    if (useSrtp) {
      callLog.info({ event: 'srtp_negotiated' }, 'SRTP negotiated — media encrypted with AES-128-CM-HMAC-SHA1-80');
    }

    // 10. Wire audio bridge AFTER session is stored
    // RTP audio → WS backend (route via session.ws so post-reconnect handler picks up new WsClient)
    let firstRtp = true;
    rtp.on('audio', (payload: Buffer) => {
      this.metrics?.incRtpRx();
      // Forwarding (B2BUA <Dial>): relay caller audio to the callee leg, not the WS.
      if (session.forwardRtp) {
        session.forwardRtp.sendAudio(payload);
        return;
      }
      if (session.wsReconnecting) return; // drop during reconnect window — WSR-03
      if (firstRtp) {
        callLog.info({ event: 'first_rtp_audio', bytes: payload.length }, 'First RTP audio packet received from sipgate');
        firstRtp = false;
      }
      session.ws.sendAudio(payload);
    });
    // DTMF → WS backend in streaming mode; suppressed while forwarding (hangupOnStar
    // is handled by the forwarder watching the caller leg directly).
    rtp.on('dtmf', ({ digit }: { digit: string }) => {
      if (session.forwardRtp) {
        if (session.hangupOnStar && digit === '*') {
          callLog.info({ event: 'dial_hangup_on_star' }, 'caller pressed * — ending forwarded call');
          session.onForwardEnd?.();
        }
        return;
      }
      session.ws.sendDtmf(digit);
    });
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
      // Only reconnect on a transient drop while streaming. An intentional close
      // (privacy gate for <Dial>, or teardown) leaves state != Streaming and MUST
      // NOT reconnect — otherwise the bot rejoins a forwarded call (privacy leak).
      if (!shouldReconnectWs(session.state, this.sessions.has(session.callId))) return;
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
    this.callSidIdx.delete(session.callSid);
    // Record terminal state + retain a read-only snapshot so GET /Calls/{Sid}
    // resolves for a grace window after teardown.
    session.state = reason === 'remote_bye' ? CallState.HungUp : CallState.Terminated;
    session.terminalReason = reason;
    const endTime = new Date();
    this.recentlyTerminated.set(session.callSid, {
      ...this.toBridgeCall(session, endTime),
      terminatedAt: endTime.getTime(),
    });
    this.metrics?.decActiveCalls();

    // Terminal status callback (carries CallDuration), then drain + close the
    // per-call delivery queue so the worker doesn't leak past teardown.
    if (this.statusClient && session.statusCallback) {
      const terminalStatus = twilioStatus(session.state, reason);
      const callDurationSec = session.answeredAt
        ? Math.max(0, Math.round((endTime.getTime() - session.answeredAt.getTime()) / 1000))
        : undefined;
      this.emitStatusEvent(session, terminalStatus, terminalStatus, {
        callDurationSec,
        sipResponseCode: session.sipFinalCode,
      });
      void this.statusClient.drainAndClose(session.callSid);
    }

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
        localSipPort: this.localSipPort,
        fromUri: session.localUri, // dialog-local URI (the INVITE To-URI), NOT SIP_USER
        fromTag: session.localTag,
        toUri: session.remoteUri,
        toTag: session.remoteTag,
        callId: session.callId,
        cseq: session.cseq,
        recordRoutes: session.recordRoutes,
      });
      // Route via the dialog route set (first proxy) when present, else the Contact.
      const [byeHost, byePort] = byeSendTarget(session.recordRoutes, session.remoteTarget);
      this.sipHandle.sendRaw(Buffer.from(bye), byePort, byeHost);
      // sendTo is the proxy IP:port (no phone) — safe at info. Full BYE at debug.
      session.log.info({ event: 'bye_sent', sendTo: `${byeHost}:${byePort}` }, 'SIP BYE sent');
      session.log.debug({ event: 'bye_message', bye }, 'BYE message');
    }

    // Dual-leg teardown: hang up the callee and release its RTP socket.
    if (session.forwardDialog) {
      void session.forwardDialog.bye();
      session.forwardDialog = null;
    }
    if (session.forwardRtp) {
      session.forwardRtp.dispose();
      session.forwardRtp = null;
    }
    // Unblock any in-flight performDial() awaiting the call to end (caller hangup).
    const onEnd = session.onForwardEnd;
    session.onForwardEnd = null;
    onEnd?.();

    session.ws.stop();     // sends stop event then closes WS (idempotent post-closeStream)
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
      // Bail if the session was terminated, OR left Streaming (privacy gate /
      // forwarding) — never reconnect the bot into a non-streaming call.
      if (!shouldReconnectWs(session.state, this.sessions.has(session.callId))) {
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
          if (!shouldReconnectWs(session.state, this.sessions.has(session.callId))) return;
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
