import { describe, expect, it } from 'vitest';
import { buildSdpAnswer, parseSdpOffer } from '../src/sip/sdp.js';

// ─── Helpers ──────────────────────────────────────────────────────────────────

function plainAvpOffer(): string {
  return [
    'v=0',
    'o=- 1 1 IN IP4 1.2.3.4',
    's=-',
    'c=IN IP4 1.2.3.4',
    't=0 0',
    'm=audio 20000 RTP/AVP 0 113',
    'a=rtpmap:0 PCMU/8000',
    'a=rtpmap:113 telephone-event/8000',
    'a=fmtp:113 0-16',
  ].join('\r\n') + '\r\n';
}

function savpOffer(): string {
  // 16-byte key + 14-byte salt = 30 bytes → 40 base64 chars
  const keyAndSalt = Buffer.from(Array.from({ length: 30 }, (_, i) => i + 1));
  const inline = keyAndSalt.toString('base64');
  return [
    'v=0',
    'o=- 1 1 IN IP4 1.2.3.4',
    's=-',
    'c=IN IP4 1.2.3.4',
    't=0 0',
    'm=audio 20000 RTP/SAVP 0 113',
    `a=crypto:1 AES_128_CM_HMAC_SHA1_80 inline:${inline}`,
    'a=rtpmap:0 PCMU/8000',
    'a=rtpmap:113 telephone-event/8000',
    'a=fmtp:113 0-16',
  ].join('\r\n') + '\r\n';
}

// ─── parseSdpOffer SRTP tests ─────────────────────────────────────────────────

describe('parseSdpOffer — plain RTP/AVP', () => {
  it('returns isSrtp=false for RTP/AVP offer', () => {
    const result = parseSdpOffer(plainAvpOffer());
    expect(result).not.toBeNull();
    expect(result!.isSrtp).toBe(false);
    expect(result!.remoteSrtpKey).toBeUndefined();
    expect(result!.remoteSrtpSalt).toBeUndefined();
  });

  it('parses remotePort and remoteIp correctly', () => {
    const result = parseSdpOffer(plainAvpOffer());
    expect(result!.remotePort).toBe(20000);
    expect(result!.remoteIp).toBe('1.2.3.4');
  });
});

describe('parseSdpOffer — SRTP RTP/SAVP', () => {
  it('returns isSrtp=true for RTP/SAVP offer', () => {
    const result = parseSdpOffer(savpOffer());
    expect(result).not.toBeNull();
    expect(result!.isSrtp).toBe(true);
  });

  it('extracts 16-byte remote SRTP key from a=crypto:', () => {
    const result = parseSdpOffer(savpOffer());
    expect(result!.remoteSrtpKey).toBeInstanceOf(Buffer);
    expect(result!.remoteSrtpKey!.length).toBe(16);
    // First 16 bytes of our test vector (bytes 1..16)
    expect(result!.remoteSrtpKey![0]).toBe(1);
    expect(result!.remoteSrtpKey![15]).toBe(16);
  });

  it('extracts 14-byte remote SRTP salt from a=crypto:', () => {
    const result = parseSdpOffer(savpOffer());
    expect(result!.remoteSrtpSalt).toBeInstanceOf(Buffer);
    expect(result!.remoteSrtpSalt!.length).toBe(14);
    // Salt bytes are 17..30 of our test vector
    expect(result!.remoteSrtpSalt![0]).toBe(17);
    expect(result!.remoteSrtpSalt![13]).toBe(30);
  });
});

// ─── buildSdpAnswer SRTP tests ────────────────────────────────────────────────

describe('buildSdpAnswer — plain RTP/AVP (srtpEnabled=false)', () => {
  it('uses RTP/AVP protocol when srtpEnabled=false', () => {
    const offer = parseSdpOffer(plainAvpOffer())!;
    const { sdp } = buildSdpAnswer('10.0.0.1', 12000, offer, 'twilio', false);
    expect(sdp).toContain('RTP/AVP');
    expect(sdp).not.toContain('RTP/SAVP');
  });

  it('returns no local SRTP keys when srtpEnabled=false', () => {
    const offer = parseSdpOffer(plainAvpOffer())!;
    const { localSrtpKey, localSrtpSalt } = buildSdpAnswer('10.0.0.1', 12000, offer, 'twilio', false);
    expect(localSrtpKey).toBeUndefined();
    expect(localSrtpSalt).toBeUndefined();
  });

  it('does not include a=crypto: when srtpEnabled=false', () => {
    const offer = parseSdpOffer(plainAvpOffer())!;
    const { sdp } = buildSdpAnswer('10.0.0.1', 12000, offer, 'twilio', false);
    expect(sdp).not.toContain('a=crypto:');
  });
});

describe('buildSdpAnswer — SRTP negotiation (srtpEnabled=true, SAVP offer)', () => {
  it('uses RTP/SAVP when offer is SAVP and srtpEnabled=true', () => {
    const offer = parseSdpOffer(savpOffer())!;
    const { sdp } = buildSdpAnswer('10.0.0.1', 12000, offer, 'twilio', true);
    expect(sdp).toContain('RTP/SAVP');
  });

  it('includes a=crypto: with AES_128_CM_HMAC_SHA1_80 when SRTP negotiated', () => {
    const offer = parseSdpOffer(savpOffer())!;
    const { sdp } = buildSdpAnswer('10.0.0.1', 12000, offer, 'twilio', true);
    expect(sdp).toContain('a=crypto:');
    expect(sdp).toContain('AES_128_CM_HMAC_SHA1_80');
  });

  it('returns 16-byte local SRTP key when SRTP negotiated', () => {
    const offer = parseSdpOffer(savpOffer())!;
    const { localSrtpKey } = buildSdpAnswer('10.0.0.1', 12000, offer, 'twilio', true);
    expect(localSrtpKey).toBeInstanceOf(Buffer);
    expect(localSrtpKey!.length).toBe(16);
  });

  it('returns 14-byte local SRTP salt when SRTP negotiated', () => {
    const offer = parseSdpOffer(savpOffer())!;
    const { localSrtpSalt } = buildSdpAnswer('10.0.0.1', 12000, offer, 'twilio', true);
    expect(localSrtpSalt).toBeInstanceOf(Buffer);
    expect(localSrtpSalt!.length).toBe(14);
  });

  it('encodes local key+salt as base64 in a=crypto: inline value', () => {
    const offer = parseSdpOffer(savpOffer())!;
    const { sdp, localSrtpKey, localSrtpSalt } = buildSdpAnswer('10.0.0.1', 12000, offer, 'twilio', true);
    const expectedInline = Buffer.concat([localSrtpKey!, localSrtpSalt!]).toString('base64');
    expect(sdp).toContain(`inline:${expectedInline}`);
  });

  it('falls back to RTP/AVP when offer is plain AVP even with srtpEnabled=true', () => {
    const offer = parseSdpOffer(plainAvpOffer())!;
    const { sdp, localSrtpKey, localSrtpSalt } = buildSdpAnswer('10.0.0.1', 12000, offer, 'twilio', true);
    expect(sdp).toContain('RTP/AVP');
    expect(sdp).not.toContain('RTP/SAVP');
    expect(sdp).not.toContain('a=crypto:');
    expect(localSrtpKey).toBeUndefined();
    expect(localSrtpSalt).toBeUndefined();
  });

  it('uses pre-computed local SRTP keys when provided', () => {
    const offer = parseSdpOffer(savpOffer())!;
    const preKey  = Buffer.alloc(16, 0xAA);
    const preSalt = Buffer.alloc(14, 0xBB);
    const { sdp, localSrtpKey, localSrtpSalt } = buildSdpAnswer(
      '10.0.0.1', 12000, offer, 'twilio', true, preKey, preSalt,
    );
    expect(localSrtpKey).toBe(preKey);
    expect(localSrtpSalt).toBe(preSalt);
    const expectedInline = Buffer.concat([preKey, preSalt]).toString('base64');
    expect(sdp).toContain(`inline:${expectedInline}`);
  });
});
