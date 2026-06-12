import { createHash } from 'node:crypto';

import { describe, expect, it } from 'vitest';
import {
  ACCOUNT_SID_RE,
  CALL_SID_RE,
  deriveAccountSid,
  isValidAccountSid,
  isValidCallSid,
  newCallSid,
} from '../src/identity/sid.js';

// Independent reference implementation of the documented formula, used to assert
// deriveAccountSid without embedding a literal AccountSid in the source.
function expectedAccountSid(sipUser: string): string {
  return 'AC' + createHash('sha256').update(sipUser, 'utf8').digest('hex').slice(0, 32);
}

// 32 lowercase-hex chars (no AC/CA prefix on its own) — composed into sample SIDs
// at runtime so no literal AccountSid string sits in the file.
const HEX32 = '0123456789abcdef0123456789abcdef';

describe('newCallSid', () => {
  it('produces a value matching ^CA[0-9a-f]{32}$ of length 34', () => {
    const sid = newCallSid();
    expect(sid).toMatch(CALL_SID_RE);
    expect(sid).toHaveLength(34);
  });

  it('is unique across many calls', () => {
    const seen = new Set<string>();
    for (let i = 0; i < 1000; i++) seen.add(newCallSid());
    expect(seen.size).toBe(1000);
  });
});

describe('deriveAccountSid', () => {
  it('produces a value matching ^AC[0-9a-f]{32}$ of length 34', () => {
    const sid = deriveAccountSid('e12345p0');
    expect(sid).toMatch(ACCOUNT_SID_RE);
    expect(sid).toHaveLength(34);
  });

  it('matches "AC" + hex(SHA-256(input)[0:16]) for a fixed input', () => {
    expect(deriveAccountSid('e12345p0')).toBe(expectedAccountSid('e12345p0'));
  });

  it('is deterministic for the same input and distinct for different inputs', () => {
    expect(deriveAccountSid('e12345p0')).toBe(deriveAccountSid('e12345p0'));
    expect(deriveAccountSid('e12345p0')).not.toBe(deriveAccountSid('different-user'));
  });

  it('handles empty input and still matches the regex + formula', () => {
    const sid = deriveAccountSid('');
    expect(sid).toMatch(ACCOUNT_SID_RE);
    expect(sid).toBe(expectedAccountSid(''));
  });
});

describe('regexes and validators', () => {
  it('CALL_SID_RE accepts valid and rejects malformed CallSids', () => {
    expect(CALL_SID_RE.test('CA' + HEX32)).toBe(true);
    expect(CALL_SID_RE.test(newCallSid())).toBe(true);
    expect(CALL_SID_RE.test('CA' + HEX32.toUpperCase())).toBe(false); // uppercase
    expect(CALL_SID_RE.test('AC' + HEX32)).toBe(false); // wrong prefix
    expect(CALL_SID_RE.test('CA' + HEX32.slice(0, 28))).toBe(false); // too short
    expect(CALL_SID_RE.test('CA' + HEX32 + 'gg')).toBe(false); // non-hex
  });

  it('ACCOUNT_SID_RE accepts valid and rejects malformed AccountSids', () => {
    expect(ACCOUNT_SID_RE.test('AC' + HEX32)).toBe(true);
    expect(ACCOUNT_SID_RE.test(deriveAccountSid('e12345p0'))).toBe(true);
    expect(ACCOUNT_SID_RE.test('AC' + HEX32.toUpperCase())).toBe(false); // uppercase
    expect(ACCOUNT_SID_RE.test('CA' + HEX32)).toBe(false); // wrong prefix
    expect(ACCOUNT_SID_RE.test('AC' + HEX32.slice(0, 28))).toBe(false); // too short
    expect(ACCOUNT_SID_RE.test('AC' + HEX32 + 'gg')).toBe(false); // non-hex
  });

  it('isValidCallSid / isValidAccountSid mirror their regexes', () => {
    expect(isValidCallSid(newCallSid())).toBe(true);
    expect(isValidCallSid(deriveAccountSid('e12345p0'))).toBe(false);
    expect(isValidAccountSid(deriveAccountSid('e12345p0'))).toBe(true);
    expect(isValidAccountSid(newCallSid())).toBe(false);
  });
});
