/**
 * SDP offer/answer helpers for sipgate-sip-stream-bridge.
 * Pure functions — no side effects, no I/O.
 */

export interface SdpOffer {
  remoteIp: string;
  remotePort: number;
  dtmfPt: number;    // telephone-event PT from offer (sipgate uses 113 at 8kHz)
  audioPts: number[]; // all non-DTMF audio PTs from offer, in order
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
 * Returns null if either the connection line (c=) or the audio media line
 * (m=audio) is missing — callers must treat this as a malformed offer.
 */
export function parseSdpOffer(sdpBody: string): SdpOffer | null {
  const cMatch = sdpBody.match(/^c=IN IP4 (\S+)/m);
  if (!cMatch) return null;
  const remoteIp = cMatch[1];

  const mMatch = sdpBody.match(/^m=audio\s+(\d+)\s+RTP\/AVP\s+([\d ]+)/m);
  if (!mMatch) return null;
  const remotePort = parseInt(mMatch[1], 10);
  const payloadTypes = mMatch[2].trim().split(/\s+/);

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

  return { remoteIp, remotePort, dtmfPt, audioPts };
}

/**
 * Build an SDP answer advertising the negotiated audio codec and telephone-event.
 *
 * twilio mode: always PCMU (PT 0) + telephone-event (from offer's dtmfPt).
 * best mode: G.722 > PCMA > PCMU preference order, filtered to what caller offered.
 *
 * Lines are joined with CRLF and a trailing CRLF is appended per RFC 4566.
 */
export function buildSdpAnswer(
  localIp: string,
  localRtpPort: number,
  offer: SdpOffer,
  mode: string,
): string {
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

  const lines = [
    'v=0',
    `o=sipgate-sip-stream-bridge 0 0 IN IP4 ${localIp}`,
    's=sipgate-sip-stream-bridge',
    `c=IN IP4 ${localIp}`,
    't=0 0',
    `m=audio ${localRtpPort} RTP/AVP ${fmtPts}`,
    ...preferredCodecs.map((c) => `a=rtpmap:${c.pt} ${c.name}/8000`),
    `a=rtpmap:${dtmfPt} telephone-event/8000`,
    `a=fmtp:${dtmfPt} 0-16`,
    'a=ptime:20',
    'a=sendrecv',
  ];
  return lines.join('\r\n') + '\r\n';
}
