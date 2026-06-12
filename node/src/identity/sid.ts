/**
 * Identity SID minting and validation for sipgate-sip-stream-bridge.
 *
 * AccountSid is deterministically derived from SIP_USER (same input always
 * yields the same output — surfaced via /health and sent on every webhook, so
 * changing it would break customer allow-lists). CallSid is cryptographically
 * random. Pure leaf module: stdlib only, no I/O beyond crypto entropy.
 */

import { randomBytes, createHash } from 'node:crypto';

/** Validates Twilio-compatible CallSid strings: "CA" + 32 lowercase hex chars. */
export const CALL_SID_RE = /^CA[0-9a-f]{32}$/;

/** Validates Twilio-compatible AccountSid strings: "AC" + 32 lowercase hex chars. */
export const ACCOUNT_SID_RE = /^AC[0-9a-f]{32}$/;

/**
 * Mint a unique Twilio-compatible CallSid.
 * Format: "CA" + 32 lowercase hex chars (matches CALL_SID_RE).
 * Uses crypto randomBytes exclusively (never a pseudo-random or counter source);
 * 128 bits of entropy = collision-free for any realistic call volume.
 */
export function newCallSid(): string {
  return 'CA' + randomBytes(16).toString('hex');
}

/**
 * Produce a deterministic Twilio-compatible AccountSid from SIP_USER.
 * Format: "AC" + first 16 bytes of SHA-256(sipUser) as 32 lowercase hex chars
 * (matches ACCOUNT_SID_RE). Deterministic across restarts.
 */
export function deriveAccountSid(sipUser: string): string {
  const sum = createHash('sha256').update(sipUser, 'utf8').digest();
  return 'AC' + sum.subarray(0, 16).toString('hex'); // first 16 bytes = 32 hex chars
}

/** Report whether s is a well-formed Twilio-compatible CallSid. */
export function isValidCallSid(s: string): boolean {
  return CALL_SID_RE.test(s);
}

/** Report whether s is a well-formed Twilio-compatible AccountSid. */
export function isValidAccountSid(s: string): boolean {
  return ACCOUNT_SID_RE.test(s);
}
