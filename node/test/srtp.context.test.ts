import { describe, expect, it } from 'vitest';
import { SrtpContext } from '../src/srtp/srtpContext.js';

// ─── Helpers ──────────────────────────────────────────────────────────────────

/** Build a minimal 12-byte RTP header + payload */
function buildRtpPacket(opts: {
  seq: number;
  timestamp: number;
  ssrc: number;
  payload: Buffer;
}): Buffer {
  const { seq, timestamp, ssrc, payload } = opts;
  const header = Buffer.allocUnsafe(12);
  header[0] = 0x80;                      // V=2, no extension, no CSRC
  header[1] = 0x00;                      // PT=0 (PCMU), marker=0
  header.writeUInt16BE(seq, 2);
  header.writeUInt32BE(timestamp, 4);
  header.writeUInt32BE(ssrc, 8);
  return Buffer.concat([header, payload]);
}

// ─── SrtpContext tests ────────────────────────────────────────────────────────

describe('SrtpContext — encrypt/decrypt round-trip', () => {
  const masterKey  = Buffer.alloc(16, 0x01);
  const masterSalt = Buffer.alloc(14, 0x02);

  it('decrypt(encrypt(packet)) returns the original RTP packet', () => {
    const encCtx = new SrtpContext(masterKey, masterSalt);
    const decCtx = new SrtpContext(masterKey, masterSalt);

    const payload = Buffer.from('Hello SRTP');
    const rtpPacket = buildRtpPacket({ seq: 1, timestamp: 160, ssrc: 0xdeadbeef, payload });

    const srtp    = encCtx.encrypt(rtpPacket);
    const plainRtp = decCtx.decrypt(srtp);
    expect(plainRtp).toEqual(rtpPacket);
  });

  it('encrypted packet is longer than original (auth tag appended)', () => {
    const ctx = new SrtpContext(masterKey, masterSalt);
    const payload = Buffer.alloc(160, 0xff);
    const rtp  = buildRtpPacket({ seq: 1, timestamp: 0, ssrc: 1, payload });
    const srtp = ctx.encrypt(rtp);
    expect(srtp.length).toBe(rtp.length + 10); // 10-byte HMAC-SHA1-80 auth tag
  });

  it('encrypted payload differs from plaintext payload', () => {
    const ctx = new SrtpContext(masterKey, masterSalt);
    const payload = Buffer.alloc(160, 0xff);
    const rtp  = buildRtpPacket({ seq: 1, timestamp: 0, ssrc: 1, payload });
    const srtp = ctx.encrypt(rtp);
    // SRTP payload (bytes 12 to length-10) must differ from the original
    const encPayload = srtp.subarray(12, srtp.length - 10);
    expect(encPayload.equals(payload)).toBe(false);
  });

  it('throws on auth tag mismatch (tampered packet)', () => {
    const encCtx = new SrtpContext(masterKey, masterSalt);
    const decCtx = new SrtpContext(masterKey, masterSalt);
    const rtp  = buildRtpPacket({ seq: 1, timestamp: 0, ssrc: 1, payload: Buffer.alloc(160) });
    const srtp = encCtx.encrypt(rtp);
    // Corrupt a byte in the ciphertext
    srtp[12] ^= 0xff;
    expect(() => decCtx.decrypt(srtp)).toThrow();
  });

  it('different master keys produce different ciphertexts', () => {
    const keyA = Buffer.alloc(16, 0xAA);
    const keyB = Buffer.alloc(16, 0xBB);
    const salt = Buffer.alloc(14, 0x00);
    const ctxA = new SrtpContext(keyA, salt);
    const ctxB = new SrtpContext(keyB, salt);
    const rtp = buildRtpPacket({ seq: 1, timestamp: 0, ssrc: 1, payload: Buffer.alloc(40, 0xcc) });
    const srtpA = ctxA.encrypt(rtp);
    const srtpB = ctxB.encrypt(rtp);
    expect(srtpA.equals(srtpB)).toBe(false);
  });

  it('multiple packets with incrementing sequence numbers decrypt correctly', () => {
    const encCtx = new SrtpContext(masterKey, masterSalt);
    const decCtx = new SrtpContext(masterKey, masterSalt);
    for (let seq = 1; seq <= 10; seq++) {
      const payload = Buffer.alloc(160, seq);
      const rtp  = buildRtpPacket({ seq, timestamp: seq * 160, ssrc: 42, payload });
      const srtp = encCtx.encrypt(rtp);
      const plain = decCtx.decrypt(srtp);
      expect(plain).toEqual(rtp);
    }
  });

  it('uses symmetric keys: enc with A, dec with A restores original', () => {
    const keyA  = Buffer.from('0102030405060708090a0b0c0d0e0f10', 'hex');
    const saltA = Buffer.from('0102030405060708090a0b0c0d0e', 'hex');
    const encCtx = new SrtpContext(keyA, saltA);
    const decCtx = new SrtpContext(keyA, saltA);
    const rtp = buildRtpPacket({ seq: 100, timestamp: 16000, ssrc: 0x12345678, payload: Buffer.from('audio data here') });
    expect(decCtx.decrypt(encCtx.encrypt(rtp))).toEqual(rtp);
  });
});
