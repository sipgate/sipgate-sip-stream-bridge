/**
 * SDP offer/answer helpers for sipgate-sip-stream-bridge.
 * Pure functions — no side effects, no I/O.
 */

import { randomBytes } from 'node:crypto';

export interface SdpOffer {
  remoteIp: string;
  remotePort: number;
  dtmfPt: number;    // telephone-event PT from offer (sipgate uses 113 at 8kHz)
  audioPts: number[]; // all non-DTMF audio PTs from offer, in order
  // SRTP fields — populated when the offer uses RTP/SAVP with SDES crypto (RFC 4568).
  isSrtp: boolean;       // true when offer uses RTP/SAVP protocol
  remoteSrtpKey?: Buffer;  // 16-byte AES-128 master key from caller's a=crypto:
  remoteSrtpSalt?: Buffer; // 14-byte master salt from caller's a=crypto:
}

export interface SdpAnswer {
  sdp: string;
  localSrtpKey?: Buffer;  // 16-byte AES-128 master key for our outbound SRTP (undefined when not SRTP)
  localSrtpSalt?: Buffer; // 14-byte master salt for our outbound SRTP (undefined when not SRTP)
}

export interface CodecInfo {
  pt: number;
  encoding: string;   // 'audio/G722' | 'audio/x-alaw' | 'audio/x-mulaw'
  sampleRate: number; // 16000 | 8000
  silenceByte: number; // 0x00 (G.722) | 0xD5 (PCMA) | 0xFF (PCMU)
}

/**
 * Select the best audio codec to use based on offered PTs and the configured mode.
 *
 * best mode: prefers G.722 (9), then PCMA (8), then PCMU (0).
 * twilio mode: always returns PCMU (0), regardless of offer.
 *
 * Always returns a valid CodecInfo — falls back to PCMU if nothing matches.
 */
export function selectAudioCodec(offered: number[], mode: string): CodecInfo {
  const CODEC_MAP: Record<number, CodecInfo> = {
    9: { pt: 9, encoding: 'audio/G722',   sampleRate: 16000, silenceByte: 0x00 },
    8: { pt: 8, encoding: 'audio/x-alaw', sampleRate: 8000,  silenceByte: 0xD5 },
    0: { pt: 0, encoding: 'audio/x-mulaw',sampleRate: 8000,  silenceByte: 0xFF },
  };
  const PCMU = CODEC_MAP[0];

  if (mode === 'best') {
    for (const pref of [9, 8, 0]) {
      if (offered.includes(pref)) return CODEC_MAP[pref];
    }
    return PCMU;
  }

  // twilio mode: always PCMU
  return PCMU;
}

/**
 * Parse the SDP body from a SIP INVITE and extract the fields needed to
 * set up an outbound RTP socket.
 *
 * Accepts both RTP/AVP (plain RTP) and RTP/SAVP (SRTP) offers.
 * When RTP/SAVP is detected, the isSrtp flag is set and the remote SRTP master
 * key+salt are extracted from the first AES_128_CM_HMAC_SHA1_80 a=crypto: line.
 *
 * Returns null if either the connection line (c=) or the audio media line
 * (m=audio) is missing — callers must treat this as a malformed offer.
 */
export function parseSdpOffer(sdpBody: string): SdpOffer | null {
  const cMatch = sdpBody.match(/^c=IN IP4 (\S+)/m);
  if (!cMatch) return null;
  const remoteIp = cMatch[1];

  // Accept both RTP/AVP (plain RTP) and RTP/SAVP (SRTP); capture the protocol name explicitly.
  const mMatch = sdpBody.match(/^m=audio\s+(\d+)\s+(RTP\/S?AVP)\s+([\d ]+)/m);
  if (!mMatch) return null;
  const remotePort = parseInt(mMatch[1], 10);
  const protocol   = mMatch[2];
  const payloadTypes = mMatch[3].trim().split(/\s+/);

  // Detect SRTP: the matched protocol must be exactly RTP/SAVP.
  const isSrtp = protocol === 'RTP/SAVP';

  // Build PT → codec name map from rtpmap lines
  const rtpmaps: Record<number, string> = {};
  for (const m of sdpBody.matchAll(/^a=rtpmap:(\d+) ([^/\r\n]+)/gm)) {
    rtpmaps[parseInt(m[1], 10)] = m[2].trim().toLowerCase();
  }

  // Separate DTMF telephone-event PTs from audio PTs.
  // If two telephone-event entries exist (e.g. PT 101 at 48kHz and PT 113 at 8kHz),
  // the last one wins — which is the 8kHz entry (PT 113) for sipgate offers.
  let dtmfPt = 101; // fallback to conventional PT
  const audioPts: number[] = [];
  for (const ptStr of payloadTypes) {
    const pt = parseInt(ptStr, 10);
    const codecName = rtpmaps[pt];
    if (codecName === 'telephone-event') {
      dtmfPt = pt;
    } else {
      audioPts.push(pt);
    }
  }

  // Extract remote SRTP master key+salt from a=crypto: when present.
  // Only AES_128_CM_HMAC_SHA1_80 is supported (key+salt = 16+14 = 30 bytes, 40 base64 chars).
  let remoteSrtpKey: Buffer | undefined;
  let remoteSrtpSalt: Buffer | undefined;
  if (isSrtp) {
    for (const m of sdpBody.matchAll(/^a=crypto:\d+\s+AES_128_CM_HMAC_SHA1_80\s+inline:([^\s|]+)/gim)) {
      try {
        const keyAndSalt = Buffer.from(m[1], 'base64');
        if (keyAndSalt.length >= 30) {
          remoteSrtpKey = keyAndSalt.subarray(0, 16);
          remoteSrtpSalt = keyAndSalt.subarray(16, 30);
          break;
        }
      } catch {
        // skip malformed entries
      }
    }
  }

  return { remoteIp, remotePort, dtmfPt, audioPts, isSrtp, remoteSrtpKey, remoteSrtpSalt };
}

/**
 * Build an SDP answer advertising the negotiated audio codec and telephone-event.
 *
 * twilio mode: always PCMU (PT 0) + telephone-event (from offer's dtmfPt).
 * best mode: G.722 > PCMA > PCMU preference order, filtered to what caller offered.
 *
 * When srtpEnabled is true and the offer uses RTP/SAVP, the answer uses RTP/SAVP with
 * an a=crypto: line. When localSrtpKey and localSrtpSalt are provided they are used
 * directly; otherwise a random 16-byte key and 14-byte salt are generated.
 * When the offer is plain AVP or srtpEnabled is false, the answer uses RTP/AVP.
 *
 * Lines are joined with CRLF and a trailing CRLF is appended per RFC 4566.
 */
export function buildSdpAnswer(
  localIp: string,
  localRtpPort: number,
  offer: SdpOffer,
  mode: string,
  srtpEnabled: boolean,
  localSrtpKey?: Buffer,
  localSrtpSalt?: Buffer,
): SdpAnswer {
  const { dtmfPt, audioPts } = offer;

  // Determine which audio codecs to advertise, in preference order.
  const preferredCodecs: Array<{ pt: number; name: string }> = [];
  if (mode === 'best') {
    for (const c of [
      { pt: 9, name: 'G722' },
      { pt: 8, name: 'PCMA' },
      { pt: 0, name: 'PCMU' },
    ]) {
      if (audioPts.includes(c.pt)) preferredCodecs.push(c);
    }
  } else {
    preferredCodecs.push({ pt: 0, name: 'PCMU' });
  }

  const fmtPts = [...preferredCodecs.map((c) => c.pt), dtmfPt].join(' ');

  // Negotiate SRTP when enabled and the offer is also SAVP; otherwise plain AVP.
  const useSrtp = srtpEnabled && offer.isSrtp;
  const protocol = useSrtp ? 'RTP/SAVP' : 'RTP/AVP';

  const lines = [
    'v=0',
    `o=sipgate-sip-stream-bridge 0 0 IN IP4 ${localIp}`,
    's=sipgate-sip-stream-bridge',
    `c=IN IP4 ${localIp}`,
    't=0 0',
    `m=audio ${localRtpPort} ${protocol} ${fmtPts}`,
  ];

  // Add a=crypto: and include/generate local SRTP key+salt when negotiating SRTP.
  let outLocalSrtpKey: Buffer | undefined;
  let outLocalSrtpSalt: Buffer | undefined;
  if (useSrtp) {
    outLocalSrtpKey = localSrtpKey ?? randomBytes(16);
    outLocalSrtpSalt = localSrtpSalt ?? randomBytes(14);
    const keyAndSalt = Buffer.concat([outLocalSrtpKey, outLocalSrtpSalt]);
    lines.push(`a=crypto:1 AES_128_CM_HMAC_SHA1_80 inline:${keyAndSalt.toString('base64')}`);
  }

  lines.push(
    ...preferredCodecs.map((c) => `a=rtpmap:${c.pt} ${c.name}/8000`),
    `a=rtpmap:${dtmfPt} telephone-event/8000`,
    `a=fmtp:${dtmfPt} 0-16`,
    'a=ptime:20',
    'a=sendrecv',
  );

  return { sdp: lines.join('\r\n') + '\r\n', localSrtpKey: outLocalSrtpKey, localSrtpSalt: outLocalSrtpSalt };
}
