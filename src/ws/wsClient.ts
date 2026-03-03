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
      return {
        sendAudio(payload: Buffer): void {
          if (ws.readyState !== WebSocket.OPEN) {
            log.warn({ event: 'ws_send_skipped', reason: 'not_open' }, 'WS not OPEN — dropping audio packet');
            return;
          }
          ws.send(
            JSON.stringify({
              event: 'media',
              sequenceNumber: String(seq++),
              media: {
                track: 'inbound',
                chunk: String(chunk++),
                timestamp: String(rtpTimestamp),
                payload: payload.toString('base64'),
              },
              streamSid,
            }),
          );
          rtpTimestamp += 160; // 20ms at 8kHz
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
          ws.on('message', (data) => {
            let msg: { event: string; media?: { payload: string } };
            try {
              msg = JSON.parse(data.toString()) as { event: string; media?: { payload: string } };
            } catch {
              return;
            }
            if (msg.event === 'media' && msg.media?.payload !== undefined) {
              handler(Buffer.from(msg.media.payload, 'base64'));
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
