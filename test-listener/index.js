#!/usr/bin/env node
// Minimal Twilio Media Streams listener for testing sipgate-sip-stream-bridge.
// Usage: node test-listener.js [port]   (default: 8080)
//
// Set WS_TARGET_URL=ws://localhost:8080 in your .env and run:
//   node test-listener.js
//   pnpm dev
//
// Modes (set via env var MODE):
//   log     (default) — log all events, send no audio back
//   echo              — echo caller audio back (loopback test)
//   timing            — like log, but print inter-arrival time for every media packet
//   tone              — send a synthetic 440 Hz tone blob after start (simulates TTS)
//                       TONE_MS=3000  duration of each tone burst (default: 3000 ms)
//                       TONE_HZ=440   frequency (default: 440 Hz)
//                       TONE_REPEAT=8 resend interval in seconds (default: 8 s, 0 = once)

import { WebSocketServer } from 'ws';

const PORT = Number(process.argv[2] ?? process.env.PORT ?? 8080);
const MODE = process.env.MODE ?? 'log';
const TONE_MS = Number(process.env.TONE_MS ?? 3000);
const TONE_HZ = Number(process.env.TONE_HZ ?? 440);
const TONE_REPEAT = Number(process.env.TONE_REPEAT ?? 8);

const wss = new WebSocketServer({ port: PORT });
console.log(`[listener] listening on ws://localhost:${PORT}  mode=${MODE}`);

// ── codec helpers ──────────────────────────────────────────────────────────────

/** Encode a 16-bit linear PCM sample to an 8-bit μ-law byte. */
function pcm16ToMulaw(sample) {
  sample = Math.max(-32768, Math.min(32767, Math.round(sample)));
  let sign = 0;
  if (sample < 0) { sign = 0x80; sample = -sample - 1; }
  sample = Math.min(sample, 32635) + 0x84;
  let exponent = 7;
  for (let mask = 0x4000; (sample & mask) === 0 && exponent > 0; mask >>= 1) exponent--;
  const mantissa = (sample >> (exponent + 3)) & 0x0f;
  return (~(sign | (exponent << 4) | mantissa)) & 0xff;
}

/** Encode a 16-bit linear PCM sample to an 8-bit A-law byte (ITU-T G.711). */
function pcm16ToAlaw(sample) {
  sample = Math.max(-32768, Math.min(32767, Math.round(sample)));
  let sign = 0;
  if (sample >= 0) { sign = 0x80; } else { sample = -sample - 1; }
  let exponent = 7;
  for (let mask = 0x4000; (sample & mask) === 0 && exponent > 0; mask >>= 1) exponent--;
  const mantissa = exponent === 0 ? (sample >> 1) & 0x0f : (sample >> (exponent + 3)) & 0x0f;
  return (sign | (exponent << 4) | mantissa) ^ 0x55;
}

/**
 * Generate `durationMs` ms of a sine-wave tone at `hz` Hz encoded for the given codec.
 * - 'audio/x-mulaw': 8-bit μ-law at 8000 Hz
 * - 'audio/x-alaw':  8-bit A-law at 8000 Hz
 * - 'audio/G722':    silence bytes (0x00) — G.722 ADPCM encoding requires a native library
 * Returns a Buffer whose length is a multiple of 160 bytes (padded with silence).
 */
function generateTone(hz, durationMs, encoding) {
  // All three codecs: 160 bytes = 20 ms packet (G.722: 16kHz × 4-bit ADPCM = 160 bytes/20ms)
  if (encoding === 'audio/G722') {
    const rawSamples = Math.round(16000 * durationMs / 1000);
    const totalSamples = Math.ceil(rawSamples / 160) * 160;
    console.log('[listener]   G.722 tone: sending silence (G.722 ADPCM encoding requires native library)');
    return Buffer.alloc(totalSamples, 0x00); // G.722 ADPCM silence = 0x00
  }

  const sampleRate = 8000;
  const encodeFn = encoding === 'audio/x-alaw' ? pcm16ToAlaw : pcm16ToMulaw;
  const silenceByte = encoding === 'audio/x-alaw' ? 0xD5 : 0xFF;
  const rawSamples = Math.round(sampleRate * durationMs / 1000);
  const totalSamples = Math.ceil(rawSamples / 160) * 160;
  const buf = Buffer.alloc(totalSamples, silenceByte);
  for (let i = 0; i < rawSamples; i++) {
    buf[i] = encodeFn(Math.sin(2 * Math.PI * hz * i / sampleRate) * 16383);
  }
  return buf;
}

// ── connection handler ─────────────────────────────────────────────────────────

wss.on('connection', (ws, req) => {
  console.log(`\n[listener] connection from ${req.socket.remoteAddress}`);
  let callInfo = null;
  let mediaFormat = { encoding: 'audio/x-mulaw', sampleRate: 8000, channels: 1 };
  let mediaCount = 0;
  let lastMediaAt = 0;
  let toneTimer = null;

  ws.on('message', (raw) => {
    let msg;
    try { msg = JSON.parse(raw.toString()); } catch { return; }

    switch (msg.event) {
      case 'connected':
        console.log('[listener] ← connected', msg.protocol, msg.version);
        break;

      case 'start':
        callInfo = msg.start;
        if (msg.start.mediaFormat) mediaFormat = msg.start.mediaFormat;
        console.log('[listener] ← start');
        console.log('           streamSid:', msg.start.streamSid);
        console.log('           callSid  :', msg.start.callSid);
        console.log('           From     :', msg.start.customParameters?.From);
        console.log('           To       :', msg.start.customParameters?.To);
        console.log('           sipCallId:', msg.start.customParameters?.sipCallId);
        console.log('           format   :', JSON.stringify(msg.start.mediaFormat));

        if (MODE === 'tone') {
          const sendTone = () => {
            if (ws.readyState !== ws.constructor.OPEN) return;
            const toneBuf = generateTone(TONE_HZ, TONE_MS, mediaFormat.encoding);
            const payload = toneBuf.toString('base64');
            ws.send(JSON.stringify({
              event: 'media',
              streamSid: msg.start.streamSid,
              media: { payload },
            }));
            console.log(
              `[listener] → tone  ${TONE_HZ} Hz  ${TONE_MS} ms` +
              `  blob=${toneBuf.length} bytes  chunks=${toneBuf.length / 160}`,
            );
          };

          // First tone 1 s after start (let the call settle)
          toneTimer = setTimeout(() => {
            sendTone();
            if (TONE_REPEAT > 0) {
              toneTimer = setInterval(sendTone, TONE_REPEAT * 1000);
            }
          }, 1000);
        }
        break;

      case 'media': {
        mediaCount++;
        const now = Date.now();
        const iat = lastMediaAt ? now - lastMediaAt : 0;
        lastMediaAt = now;
        const bytes = Buffer.from(msg.media.payload, 'base64').length;

        if (MODE === 'timing') {
          // Print every packet so we can see inter-arrival jitter
          const marker = (iat < 15 || iat > 25) ? ' ◄ JITTER' : '';
          console.log(
            `[listener] ← media  chunk=${msg.media.chunk.toString().padStart(4)}` +
            `  iat=${String(iat).padStart(3)} ms  bytes=${bytes}${marker}`,
          );
        } else if (mediaCount === 1 || mediaCount % 50 === 0) {
          console.log(`[listener] ← media  chunk=${msg.media.chunk}  bytes=${bytes}  (total: ${mediaCount})`);
        }

        if (MODE === 'echo' && callInfo) {
          ws.send(JSON.stringify({
            event: 'media',
            streamSid: msg.streamSid,
            media: { payload: msg.media.payload },
          }));
        }
        break;
      }

      case 'dtmf':
        console.log('[listener] ← dtmf   digit:', msg.dtmf?.digit);
        break;

      case 'stop':
        console.log(`[listener] ← stop   (received ${mediaCount} media packets total)`);
        mediaCount = 0;
        callInfo = null;
        lastMediaAt = 0;
        if (toneTimer !== null) { clearTimeout(toneTimer); clearInterval(toneTimer); toneTimer = null; }
        break;

      default:
        console.log('[listener] ← unknown event:', msg.event);
    }
  });

  ws.on('close', () => {
    if (toneTimer !== null) { clearTimeout(toneTimer); clearInterval(toneTimer); toneTimer = null; }
    console.log('[listener] connection closed\n');
  });
  ws.on('error', (err) => console.error('[listener] error:', err.message));
});
