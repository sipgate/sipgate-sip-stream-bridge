/**
 * Per-call RTP handler.
 *
 * Each call gets its own dgram socket bound to a port drawn from the
 * configured range. The handler parses inbound RTP headers, emits decoded
 * audio and DTMF events, and can send outbound audio with a minimal 12-byte
 * RTP header.
 *
 * Emits:
 *   'audio'  (payload: Buffer)           — PCMU payload, header stripped
 *   'dtmf'   (data: { digit: string })   — telephone-event End=true only
 */

import { randomBytes } from 'node:crypto';
import * as dgram from 'node:dgram';
import { EventEmitter } from 'node:events';
import type { Logger } from 'pino';

// ─── Public types ─────────────────────────────────────────────────────────────

export interface RtpHandler extends EventEmitter {
  readonly localPort: number;
  /** Silence byte for the negotiated codec (used for NAT hole-punch and reconnect silence) */
  readonly silenceByte: number;
  /** Call after WS connected + start event sent — enables payload forwarding */
  startForwarding(): void;
  /** Set the remote RTP endpoint (IP + port) before sending audio */
  setRemote(ip: string, port: number): void;
  /** Send an audio payload Buffer to the remote RTP endpoint */
  sendAudio(payload: Buffer): void;
  /** Release dgram socket and remove all listeners */
  dispose(): void;
}

// ─── DTMF digit map (RFC 4733 §3.4) ─────────────────────────────────────────

const DTMF_DIGIT: Readonly<Record<number, string>> = {
  0: '0',
  1: '1',
  2: '2',
  3: '3',
  4: '4',
  5: '5',
  6: '6',
  7: '7',
  8: '8',
  9: '9',
  10: '*',
  11: '#',
  12: 'A',
  13: 'B',
  14: 'C',
  15: 'D',
};

// ─── Module-level port counter (safe — JS is single-threaded) ────────────────

let nextPort: number | undefined;

// ─── Implementation ───────────────────────────────────────────────────────────

class RtpHandlerImpl extends EventEmitter implements RtpHandler {
  readonly localPort: number;
  readonly silenceByte: number;

  private forwardingEnabled = false;
  private remoteIp = '';
  private remotePort = 0;
  private outSeq = 0;
  private outTimestamp = 0;
  private readonly outSsrc: number;
  /** RTP timestamp of the last emitted DTMF event — deduplicates RFC 4733 redundant End packets */
  private lastDtmfTimestamp = -1;

  constructor(
    private readonly socket: dgram.Socket,
    localPort: number,
    private readonly log: Logger,
    private readonly audioPt: number,
    private readonly dtmfPt: number,
    silenceByte: number,
  ) {
    super();
    this.localPort = localPort;
    this.silenceByte = silenceByte;
    this.outSsrc = randomBytes(4).readUInt32BE(0);

    socket.on('message', (buf: Buffer) => this.onMessage(buf));
    socket.on('error', (err: Error) => {
      log.error({ err }, 'RTP socket error');
    });
  }

  startForwarding(): void {
    this.forwardingEnabled = true;
  }

  setRemote(ip: string, port: number): void {
    this.remoteIp = ip;
    this.remotePort = port;
  }

  sendAudio(payload: Buffer): void {
    if (!this.remoteIp || !this.remotePort) {
      this.log.warn('sendAudio called before setRemote — dropping packet');
      return;
    }

    const header = Buffer.allocUnsafe(12);
    header[0] = 0x80; // V=2, P=0, X=0, CC=0
    header[1] = this.audioPt & 0x7f; // M=0, PT=negotiated audio codec
    header.writeUInt16BE(this.outSeq & 0xffff, 2);
    this.outSeq++;
    header.writeUInt32BE(this.outTimestamp >>> 0, 4);
    this.outTimestamp = (this.outTimestamp + 160) >>> 0; // 20 ms at 8 kHz, keep 32-bit
    header.writeUInt32BE(this.outSsrc, 8);

    const packet = Buffer.concat([header, payload]);
    this.socket.send(packet, this.remotePort, this.remoteIp);
  }

  dispose(): void {
    this.forwardingEnabled = false;
    this.socket.removeAllListeners();
    this.removeAllListeners();
    try {
      this.socket.close();
    } catch {
      // ENOTCONN or already-closed — ignore
    }
  }

  private onMessage(buf: Buffer): void {
    if (buf.length < 12) return; // too short for a valid RTP header

    const byte0 = buf[0];
    const csrcCount = byte0 & 0x0f;
    const extBit = (byte0 & 0x10) !== 0;
    let headerLen = 12 + csrcCount * 4;
    if (extBit) {
      // Extension header: 2-byte profile + 2-byte length (in 32-bit words)
      if (buf.length < headerLen + 4) return;
      headerLen += 4 + buf.readUInt16BE(headerLen + 2) * 4;
    }
    if (buf.length < headerLen) return;

    const payloadType = buf[1] & 0x7f;

    if (!this.forwardingEnabled) return;

    if (payloadType === this.audioPt) {
      // Negotiated audio codec — forward payload to WS
      this.emit('audio', buf.subarray(headerLen));
      return;
    }

    if (payloadType === this.dtmfPt) {
      // RFC 4733 telephone-event
      const payload = buf.subarray(headerLen);
      if (payload.length < 4) return;
      const isEnd = (payload[1] & 0x80) !== 0;
      if (isEnd) {
        const eventCode = payload[0];
        const digit = DTMF_DIGIT[eventCode];
        // RFC 4733 §2.5: senders MUST transmit 3 redundant End packets with the same
        // RTP timestamp. Deduplicate by timestamp so only one event is emitted per keypress.
        const rtpTimestamp = buf.readUInt32BE(4);
        if (digit !== undefined && rtpTimestamp !== this.lastDtmfTimestamp) {
          this.lastDtmfTimestamp = rtpTimestamp;
          this.emit('dtmf', { digit });
        }
      }
      return;
    }

    this.log.trace({ payloadType }, 'ignoring unknown RTP payload type');
  }
}

// ─── Factory ──────────────────────────────────────────────────────────────────

/**
 * Allocate a UDP port and bind a dgram socket.
 *
 * Port selection uses a module-level counter (nextPort) so repeated calls
 * within the same process step through the range without re-trying the same
 * port. On EADDRINUSE the counter advances by one and retries; on exhaustion
 * an error is thrown.
 */
export async function createRtpHandler(opts: {
  portMin: number;
  portMax: number;
  log: Logger;
  audioPt: number;
  dtmfPt: number;
  silenceByte: number;
}): Promise<RtpHandler> {
  const { portMin, portMax, log, audioPt, dtmfPt, silenceByte } = opts;

  // Initialise module counter on first call (or if drifted below range)
  if (nextPort === undefined || nextPort < portMin) {
    nextPort = portMin;
  }

  const rangeSize = portMax - portMin + 1;

  for (let attempt = 0; attempt < rangeSize; attempt++) {
    const port: number = nextPort as number;
    // Advance the counter (wraps within range)
    nextPort = port >= portMax ? portMin : port + 1;

    try {
      const handler = await bindSocketOnPort(port, log, audioPt, dtmfPt, silenceByte);

      // Warn when fewer than 10 ports remain ahead in the range
      const portsAhead = portMax - port;
      if (Math.floor(portsAhead / 2) < 10) {
        log.warn({ portsAhead }, 'fewer than 10 RTP ports remain free');
      }

      return handler;
    } catch (err) {
      const error = err as NodeJS.ErrnoException;
      if (error.code === 'EADDRINUSE') {
        log.trace({ port }, 'RTP port in use, trying next');
        continue;
      }
      // Unexpected error — propagate immediately
      throw err;
    }
  }

  throw new Error(
    `All RTP ports in range ${portMin}–${portMax} are in use. Cannot allocate a port.`,
  );
}

/**
 * Create a udp4 socket, bind it to the given port, and return an RtpHandler.
 * Rejects with an EADDRINUSE error if the port is taken.
 */
function bindSocketOnPort(
  port: number,
  log: Logger,
  audioPt: number,
  dtmfPt: number,
  silenceByte: number,
): Promise<RtpHandler> {
  return new Promise<RtpHandler>((resolve, reject) => {
    const socket = dgram.createSocket('udp4');

    function onError(err: Error): void {
      socket.removeListener('listening', onListening);
      reject(err);
    }

    function onListening(): void {
      socket.removeListener('error', onError);
      const info = socket.address();
      const localPort = typeof info === 'object' ? info.port : port;
      resolve(new RtpHandlerImpl(socket, localPort, log, audioPt, dtmfPt, silenceByte));
    }

    socket.once('error', onError);
    socket.once('listening', onListening);
    socket.bind(port);
  });
}
