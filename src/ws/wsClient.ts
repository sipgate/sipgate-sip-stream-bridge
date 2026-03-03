import { WebSocket } from 'ws';
import type { Logger } from 'pino';

export interface WsCallParams {
  streamSid: string;
  callSid: string;
  from: string;
  to: string;
  sipCallId: string;
}

export interface WsClient {
  /** Forward mulaw payload Buffer from RTP as inbound media event */
  sendAudio(payload: Buffer): void;
  /** Forward DTMF digit string as dtmf event */
  sendDtmf(digit: string): void;
  /** Send stop event, then close the WebSocket */
  stop(): void;
  /** Register handler for outbound audio payload from WS backend (decode base64 → Buffer) */
  onAudio(handler: (payload: Buffer) => void): void;
  /** Register handler for WS close/error — called when WS disconnects for any reason */
  onDisconnect(handler: () => void): void;
}

export function createWsClient(
  url: string,
  params: WsCallParams,
  log: Logger,
): Promise<WsClient> {
  const { streamSid, callSid, from, to, sipCallId } = params;

  return new Promise<WsClient>((resolve, reject) => {
    const ws = new WebSocket(url);

    const timer = setTimeout(() => {
      ws.terminate();
      reject(new Error(`WS connect timeout after 2000ms to ${url}`));
    }, 2000);

    ws.once('open', () => {
      clearTimeout(timer);
      log.info({ event: 'ws_connected', url }, 'WS connected to backend');

      // Send connected event immediately
      ws.send(
        JSON.stringify({
          event: 'connected',
          protocol: 'Call',
          version: '1.0.0',
        }),
      );

      // Send start event immediately (sequenceNumber '1')
      ws.send(
        JSON.stringify({
          event: 'start',
          sequenceNumber: '1',
          start: {
            streamSid,
            accountSid: '',
            callSid,
            tracks: ['inbound', 'outbound'],
            customParameters: {
              From: from,
              To: to,
              sipCallId,
            },
            mediaFormat: {
              encoding: 'audio/x-mulaw',
              sampleRate: 8000,
              channels: 1,
            },
          },
          streamSid,
        }),
      );

      log.info({ event: 'ws_start_sent', streamSid, callSid }, 'connected + start events sent');
      resolve(buildClient());
    });

    ws.once('error', (err) => {
      clearTimeout(timer);
      reject(err);
    });

    // Per-session sequencing state — seq 1 was used by start event
    let seq = 2;
    let chunk = 0;
    let rtpTimestamp = 0;

    function buildClient(): WsClient {
      const PACKET_MS = 20;
      const PACKET_BYTES = 160; // 20 ms of mulaw at 8 kHz

      // ── Smooth drain helper ────────────────────────────────────────────────────
      // Both inbound (RTP → WS) and outbound (WS → RTP) use the same pacing logic:
      //   • self-correcting recursive setTimeout so ±2 ms drift is recovered next tick
      //   • reset-on-stall: if we are more than one full packet behind (V8 GC pause
      //     typically ~100 ms every ~500 ms), reset the baseline instead of bursting.
      //     A brief audio gap is far less audible than jitter-buffer overflow.
      function makeDrain(sendPacket: (pkt: Buffer) => void): {
        enqueue(pkt: Buffer): void;
        stop(): void;
      } {
        const queue: Buffer[] = [];
        let timer: ReturnType<typeof setTimeout> | null = null;
        let base = 0;
        let count = 0;

        function schedule(): void {
          const now = Date.now();
          const deadline = base + count * PACKET_MS;
          const behind = now - deadline;

          let delay: number;
          if (behind > PACKET_MS) {
            base = now;
            count = 0;
            delay = 0;
          } else {
            delay = Math.max(0, deadline - now);
          }

          timer = setTimeout(() => {
            timer = null;
            const pkt = queue.shift();
            if (pkt !== undefined) {
              count++;
              sendPacket(pkt);
              schedule();
            }
            // Queue empty — drain stops; next enqueue() restarts with a fresh baseline
          }, delay);
        }

        return {
          enqueue(pkt: Buffer): void {
            queue.push(pkt);
            if (timer === null) {
              base = Date.now();
              count = 0;
              schedule();
            }
          },
          stop(): void {
            queue.length = 0;
            if (timer !== null) {
              clearTimeout(timer);
              timer = null;
            }
          },
        };
      }

      // ── Inbound drain: RTP → WS ────────────────────────────────────────────────
      // GC pauses cause UDP packets to queue in the OS buffer and arrive in a burst.
      // Without smoothing the WS backend sees 100 ms gaps + 5 packets at 0 ms each.
      const inboundDrain = makeDrain((pkt) => {
        if (ws.readyState !== WebSocket.OPEN) return;
        ws.send(
          JSON.stringify({
            event: 'media',
            sequenceNumber: String(seq++),
            media: {
              track: 'inbound',
              chunk: String(chunk++),
              timestamp: String(rtpTimestamp),
              payload: pkt.toString('base64'),
            },
            streamSid,
          }),
        );
        rtpTimestamp += 160; // 20 ms at 8 kHz
      });

      // ── Outbound drain: WS → RTP ───────────────────────────────────────────────
      // TTS backends send the whole audio clip as one base64 blob. Chunk into 160-byte
      // slices and drain one packet per 20 ms so the phone's jitter buffer stays full.
      let outboundHandler: ((payload: Buffer) => void) | null = null;
      const outboundDrain = makeDrain((pkt) => outboundHandler?.(pkt));

      function enqueueOutbound(decoded: Buffer): void {
        for (let off = 0; off < decoded.length; off += PACKET_BYTES) {
          const slice = decoded.subarray(off, off + PACKET_BYTES);
          if (slice.length < PACKET_BYTES) {
            // Pad short final chunk to a full 20 ms packet with μ-law silence (0xFF).
            // A sub-160-byte RTP packet confuses G.711 decoders and causes a glitch.
            const padded = Buffer.alloc(PACKET_BYTES, 0xff);
            slice.copy(padded);
            outboundDrain.enqueue(padded);
          } else {
            outboundDrain.enqueue(slice);
          }
        }
      }

      return {
        sendAudio(payload: Buffer): void {
          if (ws.readyState !== WebSocket.OPEN) {
            log.warn({ event: 'ws_send_skipped', reason: 'not_open' }, 'WS not OPEN — dropping audio packet');
            return;
          }
          inboundDrain.enqueue(payload);
        },

        sendDtmf(digit: string): void {
          if (ws.readyState !== WebSocket.OPEN) {
            log.warn({ event: 'ws_send_skipped', reason: 'not_open' }, 'WS not OPEN — dropping DTMF digit');
            return;
          }
          ws.send(
            JSON.stringify({
              event: 'dtmf',
              streamSid,
              sequenceNumber: String(seq++),
              dtmf: {
                track: 'inbound_track',
                digit,
              },
            }),
          );
        },

        stop(): void {
          // Discard any queued audio in both directions — call is ending
          inboundDrain.stop();
          outboundDrain.stop();
          if (ws.readyState === WebSocket.OPEN) {
            ws.send(
              JSON.stringify({
                event: 'stop',
                sequenceNumber: String(seq++),
                stop: {
                  accountSid: '',
                  callSid,
                },
                streamSid,
              }),
            );
            ws.close();
          } else {
            ws.terminate();
          }
        },

        onAudio(handler: (payload: Buffer) => void): void {
          outboundHandler = handler;
          ws.on('message', (data) => {
            let msg: { event: string; media?: { payload: string } };
            try {
              msg = JSON.parse(data.toString()) as { event: string; media?: { payload: string } };
            } catch {
              return;
            }
            if (msg.event === 'media' && msg.media?.payload !== undefined) {
              enqueueOutbound(Buffer.from(msg.media.payload, 'base64'));
            }
            // Ignore non-media events (mark, clear, etc.)
          });
        },

        onDisconnect(handler: () => void): void {
          ws.once('close', handler);
          ws.once('error', (err) => {
            log.error({ err, event: 'ws_error' }, 'WS error');
            handler();
          });
        },
      };
    }
  });
}
