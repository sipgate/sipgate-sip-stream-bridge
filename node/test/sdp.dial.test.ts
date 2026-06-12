import { describe, expect, it } from 'vitest';
import { acceptSdpAnswer, buildSdpOffer, CodecMismatchError } from '../src/sip/sdp.js';

// ─── Helpers ──────────────────────────────────────────────────────────────────

function pcmuAnswer(): string {
  return [
    'v=0',
    'o=- 1 1 IN IP4 5.6.7.8',
    's=-',
    'c=IN IP4 5.6.7.8',
    't=0 0',
    'm=audio 30000 RTP/AVP 0 101',
    'a=rtpmap:0 PCMU/8000',
    'a=rtpmap:101 telephone-event/8000',
    'a=fmtp:101 0-16',
  ].join('\r\n') + '\r\n';
}

function nonPcmuAnswer(): string {
  // Callee offers PCMA (8) + G722 (9) as audio, PCMU absent → first audio PT is 8.
  return [
    'v=0',
    'o=- 1 1 IN IP4 5.6.7.8',
    's=-',
    'c=IN IP4 5.6.7.8',
    't=0 0',
    'm=audio 30000 RTP/AVP 8 9 101',
    'a=rtpmap:8 PCMA/8000',
    'a=rtpmap:9 G722/8000',
    'a=rtpmap:101 telephone-event/8000',
    'a=fmtp:101 0-16',
  ].join('\r\n') + '\r\n';
}

// ─── buildSdpOffer ────────────────────────────────────────────────────────────

describe('buildSdpOffer — outbound <Dial> leg', () => {
  it('emits a PCMU + telephone-event m=audio line over RTP/AVP', () => {
    const sdp = buildSdpOffer('10.0.0.1', 40000);
    expect(sdp).toContain('m=audio 40000 RTP/AVP 0 101');
  });

  it('emits PCMU and telephone-event rtpmap lines', () => {
    const sdp = buildSdpOffer('10.0.0.1', 40000);
    expect(sdp).toContain('a=rtpmap:0 PCMU/8000');
    expect(sdp).toContain('a=rtpmap:101 telephone-event/8000');
    expect(sdp).toContain('a=fmtp:101 0-16');
  });

  it('uses RTP/AVP (no SRTP) and includes no a=crypto line', () => {
    const sdp = buildSdpOffer('10.0.0.1', 40000);
    expect(sdp).not.toContain('RTP/SAVP');
    expect(sdp).not.toContain('a=crypto:');
  });

  it('embeds the local IP in c= and o= lines', () => {
    const sdp = buildSdpOffer('10.0.0.1', 40000);
    expect(sdp).toContain('c=IN IP4 10.0.0.1');
    expect(sdp).toContain('o=sipgate-sip-stream-bridge 0 0 IN IP4 10.0.0.1');
  });

  it('includes a=sendrecv and a=ptime:20', () => {
    const sdp = buildSdpOffer('10.0.0.1', 40000);
    expect(sdp).toContain('a=sendrecv');
    expect(sdp).toContain('a=ptime:20');
  });

  it('joins lines with CRLF and ends with a trailing CRLF', () => {
    const sdp = buildSdpOffer('10.0.0.1', 40000);
    expect(sdp.endsWith('\r\n')).toBe(true);
    expect(sdp.startsWith('v=0\r\n')).toBe(true);
  });
});

// ─── acceptSdpAnswer ──────────────────────────────────────────────────────────

describe('acceptSdpAnswer — PCMU answer', () => {
  it('extracts remoteIp, remotePort, and dtmfPt', () => {
    const info = acceptSdpAnswer(pcmuAnswer());
    expect(info.remoteIp).toBe('5.6.7.8');
    expect(info.remotePort).toBe(30000);
    expect(info.dtmfPt).toBe(101);
  });
});

describe('acceptSdpAnswer — codec fail-fast', () => {
  it('throws CodecMismatchError when the first audio codec is not PCMU', () => {
    expect(() => acceptSdpAnswer(nonPcmuAnswer())).toThrow(CodecMismatchError);
  });

  it('reports the offending payload type on the error', () => {
    try {
      acceptSdpAnswer(nonPcmuAnswer());
      expect.unreachable('expected CodecMismatchError');
    } catch (err) {
      expect(err).toBeInstanceOf(CodecMismatchError);
      expect((err as CodecMismatchError).negotiatedPt).toBe(8);
    }
  });

  it('throws a plain Error on a malformed answer (no m=audio)', () => {
    const malformed = ['v=0', 'c=IN IP4 5.6.7.8', 't=0 0'].join('\r\n') + '\r\n';
    expect(() => acceptSdpAnswer(malformed)).toThrow(/malformed/);
  });
});
