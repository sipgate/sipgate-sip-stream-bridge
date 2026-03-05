/**
 * SDP offer/answer helpers for sipgate-sip-stream-bridge.
 * Pure functions — no side effects, no I/O.
 */

export interface SdpOffer {
  remoteIp: string;
  remotePort: number;
  hasPcmu: boolean;
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
  const hasPcmu = payloadTypes.includes('0');

  return { remoteIp, remotePort, hasPcmu };
}

/**
 * Build an SDP answer advertising PCMU (payload type 0) and telephone-event
 * (payload type 101) on the given local IP and RTP port.
 *
 * Lines are joined with CRLF and a trailing CRLF is appended, as required by
 * RFC 4566.
 */
export function buildSdpAnswer(localIp: string, localRtpPort: number): string {
  const lines = [
    'v=0',
    `o=sipgate-sip-stream-bridge 0 0 IN IP4 ${localIp}`,
    's=sipgate-sip-stream-bridge',
    `c=IN IP4 ${localIp}`,
    't=0 0',
    `m=audio ${localRtpPort} RTP/AVP 0 113`,
    'a=rtpmap:0 PCMU/8000',
    'a=rtpmap:113 telephone-event/8000',
    'a=fmtp:113 0-16',
    'a=ptime:20',
    'a=sendrecv',
  ];
  return lines.join('\r\n') + '\r\n';
}
