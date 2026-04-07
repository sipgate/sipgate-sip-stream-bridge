/**
 * Minimal SRTP context for AES_128_CM_HMAC_SHA1_80 (RFC 3711, RFC 4568).
 *
 * Implements symmetric-key SRTP using Node.js's built-in `crypto` module:
 *   - AES-128-CM (Counter Mode) encryption  — RFC 3711 §4.1.1
 *   - HMAC-SHA1-80 authentication tag       — RFC 3711 §4.2
 *   - Session-key derivation from master key+salt — RFC 3711 §4.3.1
 *
 * This covers the SDES (RFC 4568) key exchange used in SIP INVITE/Answer.
 * One SrtpContext instance handles one direction (encrypt XOR decrypt).
 */

import { createCipheriv, createHmac } from 'node:crypto';

// AES-128-CM key derivation labels (RFC 3711 §4.3.1)
const LABEL_ENC  = 0x00; // session encryption key
const LABEL_AUTH = 0x01; // session authentication key
const LABEL_SALT = 0x02; // session salt

/**
 * Derive a session key using the SRTP pseudo-random function (RFC 3711 §4.3.1).
 *
 * The PRF for AES-128 is AES-128-CTR applied to an all-zeros plaintext with
 * IV = (master_salt_padded XOR (label * 2^16)), where master_salt is left-padded
 * with two 0x00 bytes to reach 16 bytes and label * 2^16 affects byte 13 (0-indexed).
 *
 * @param masterKey  16-byte AES-128 master key
 * @param masterSalt 14-byte master salt
 * @param label      derivation label (0=enc, 1=auth, 2=salt)
 * @param length     desired output length in bytes
 */
function deriveKey(masterKey: Buffer, masterSalt: Buffer, label: number, length: number): Buffer {
  // Left-pad 14-byte salt with 2 zero bytes → 16-byte IV base
  const iv = Buffer.alloc(16, 0);
  masterSalt.copy(iv, 2); // iv[2..15] = masterSalt[0..13]

  // XOR label * 2^16 into the IV.
  // For labels 0,1,2: label * 65536 = label at byte[13] (big-endian 128-bit)
  iv[13] ^= label & 0xff;
  // For labels > 255, byte[12] would also be affected, but we only use 0,1,2.

  // AES-128-CTR with IV and counter=0 generates the pseudo-random key material.
  const blocks = Math.ceil(length / 16);
  const plaintext = Buffer.alloc(blocks * 16, 0);
  const cipher = createCipheriv('aes-128-ctr', masterKey, iv);
  return cipher.update(plaintext).subarray(0, length);
}

/**
 * Compute the 128-bit SRTP IV for AES-128-CM encryption (RFC 3711 §4.1.1):
 *   IV = (k_s * 2^16) XOR (SSRC * 2^64) XOR (i * 2^16)
 * where k_s is the 14-byte session salt (padded to 16 bytes), SSRC is 32 bits,
 * and i is the 48-bit packet index (ROC << 16 | SEQ).
 */
function computeIV(sessionSalt: Buffer, ssrc: number, packetIndex: bigint): Buffer {
  // Pad 14-byte session salt to 16 bytes (k_s * 2^16 in 128-bit big-endian)
  const iv = Buffer.alloc(16, 0);
  sessionSalt.copy(iv, 2); // iv[2..15] = sessionSalt[0..13]

  // XOR SSRC at bytes 4..7 (SSRC * 2^64 shifted to bits 95..64)
  iv[4] ^= (ssrc >>> 24) & 0xff;
  iv[5] ^= (ssrc >>> 16) & 0xff;
  iv[6] ^= (ssrc >>> 8)  & 0xff;
  iv[7] ^= ssrc          & 0xff;

  // XOR packet index (48 bits) at bytes 10..15 (i * 2^16 shifted to bits 63..16)
  const hi = Number(packetIndex >> 32n) & 0xffff; // top 16 bits of 48-bit index
  const lo = Number(packetIndex & 0xffffffffn);    // bottom 32 bits
  iv[10] ^= (hi >> 8)  & 0xff;
  iv[11] ^= hi         & 0xff;
  iv[12] ^= (lo >>> 24) & 0xff;
  iv[13] ^= (lo >>> 16) & 0xff;
  iv[14] ^= (lo >>> 8)  & 0xff;
  iv[15] ^= lo          & 0xff;

  return iv;
}

/** Sequence number range threshold for RFC 3711 §3.3.1 packet index estimation. */
const SEQ_WRAP_THRESHOLD = 32768;

/** SRTP context for one direction of a media stream. */
export class SrtpContext {
  private readonly encKey: Buffer;  // 16-byte session encryption key
  private readonly authKey: Buffer; // 20-byte session authentication key (HMAC-SHA1)
  private readonly saltKey: Buffer; // 14-byte session salt

  // Per-direction packet index tracking (ROC + SEQ → 48-bit packet index)
  private roc = 0;
  private lastSeq = -1;

  /**
   * Create an SRTP context for a single direction.
   *
   * @param masterKey  16-byte AES-128 master key (from the a=crypto: inline value)
   * @param masterSalt 14-byte master salt (from the a=crypto: inline value)
   */
  constructor(masterKey: Buffer, masterSalt: Buffer) {
    this.encKey  = deriveKey(masterKey, masterSalt, LABEL_ENC,  16);
    this.authKey = deriveKey(masterKey, masterSalt, LABEL_AUTH, 20);
    this.saltKey = deriveKey(masterKey, masterSalt, LABEL_SALT, 14);
  }

  /**
   * Encrypt a plain RTP packet into an SRTP packet.
   *
   * The input buffer must contain a valid 12-byte RTP header followed by the payload.
   * Returns a new Buffer: RTP header + encrypted payload + 10-byte HMAC-SHA1-80 auth tag.
   */
  encrypt(rtpPacket: Buffer): Buffer {
    if (rtpPacket.length < 12) throw new Error('SRTP encrypt: RTP packet too short');

    const ssrc = rtpPacket.readUInt32BE(8);
    const seq  = rtpPacket.readUInt16BE(2);
    const packetIndex = this.getPacketIndex(seq);

    const headerLen = this.rtpHeaderLength(rtpPacket);
    const header  = rtpPacket.subarray(0, headerLen);
    const payload = rtpPacket.subarray(headerLen);

    // AES-128-CTR encryption of the payload
    const iv = computeIV(this.saltKey, ssrc, packetIndex);
    const cipher = createCipheriv('aes-128-ctr', this.encKey, iv);
    const encryptedPayload = cipher.update(payload);

    const encPacket = Buffer.concat([header, encryptedPayload]);

    // HMAC-SHA1-80: authenticate (header + encrypted_payload + ROC)
    const rocBuf = Buffer.alloc(4);
    rocBuf.writeUInt32BE(this.roc, 0);
    const hmac = createHmac('sha1', this.authKey);
    hmac.update(encPacket);
    hmac.update(rocBuf);
    const tag = hmac.digest().subarray(0, 10); // 80-bit tag

    return Buffer.concat([encPacket, tag]);
  }

  /**
   * Decrypt an SRTP packet into a plain RTP packet.
   *
   * The input buffer must be a valid SRTP packet: RTP header + encrypted payload + 10-byte auth tag.
   * Returns a new Buffer: RTP header + decrypted payload (auth tag stripped).
   * Throws on authentication failure or malformed input.
   */
  decrypt(srtpPacket: Buffer): Buffer {
    if (srtpPacket.length < 12 + 10) throw new Error('SRTP decrypt: packet too short');

    const tagStart = srtpPacket.length - 10;
    const encPacket = srtpPacket.subarray(0, tagStart);
    const receivedTag = srtpPacket.subarray(tagStart);

    const ssrc = encPacket.readUInt32BE(8);
    const seq  = encPacket.readUInt16BE(2);
    const packetIndex = this.getPacketIndex(seq);

    // Verify HMAC-SHA1-80 auth tag
    const rocBuf = Buffer.alloc(4);
    rocBuf.writeUInt32BE(this.roc, 0);
    const hmac = createHmac('sha1', this.authKey);
    hmac.update(encPacket);
    hmac.update(rocBuf);
    const expectedTag = hmac.digest().subarray(0, 10);
    if (!expectedTag.equals(receivedTag)) {
      throw new Error('SRTP decrypt: authentication tag mismatch');
    }

    const headerLen = this.rtpHeaderLength(encPacket);
    const header    = encPacket.subarray(0, headerLen);
    const encPayload = encPacket.subarray(headerLen);

    // AES-128-CTR decryption (same operation as encryption — CTR mode is symmetric)
    const iv = computeIV(this.saltKey, ssrc, packetIndex);
    const decipher = createCipheriv('aes-128-ctr', this.encKey, iv);
    const decPayload = decipher.update(encPayload);

    return Buffer.concat([header, decPayload]);
  }

  /**
   * Compute the 48-bit SRTP packet index (ROC || SEQ) for the given sequence number,
   * and update the stored ROC and lastSeq on confirmed forward wraps (RFC 3711 §3.3.1).
   *
   * Three cases:
   *   - Normal sequential: diff in [-32768, 32768] → use current ROC, update lastSeq.
   *   - Forward wrap (diff < -32768): seq wrapped high→low → ROC++, update lastSeq.
   *   - Late packet (diff > 32768): seq is from before a previous wrap → use ROC-1
   *     for THIS packet's index only; do not update lastSeq or persistent ROC.
   */
  private getPacketIndex(seq: number): bigint {
    if (this.lastSeq < 0) {
      this.lastSeq = seq;
      return BigInt(this.roc) * 65536n + BigInt(seq);
    }

    const diff = seq - this.lastSeq;
    let roc = this.roc;

    if (diff < -SEQ_WRAP_THRESHOLD) {
      // Forward sequence wrap (e.g., 65535 → 0): increment persistent ROC.
      this.roc++;
      roc = this.roc;
      this.lastSeq = seq;
    } else if (diff > SEQ_WRAP_THRESHOLD) {
      // Late/reordered packet from before the most recent wrap: use ROC-1 for
      // this packet's index only. Do not update lastSeq or the persistent ROC.
      roc = Math.max(0, this.roc - 1);
    } else {
      // Normal sequential packet (or minor reorder within the 32768-sample window).
      this.lastSeq = seq;
    }

    return BigInt(roc) * 65536n + BigInt(seq);
  }

  /** Compute the RTP header length including CSRC list and extension (RFC 3550 §5.1). */
  private rtpHeaderLength(pkt: Buffer): number {
    const csrcCount = pkt[0] & 0x0f;
    const hasExt    = (pkt[0] & 0x10) !== 0;
    let len = 12 + csrcCount * 4;
    if (hasExt && pkt.length >= len + 4) {
      len += 4 + pkt.readUInt16BE(len + 2) * 4;
    }
    return len;
  }
}
