import { describe, expect, it } from 'vitest';
import {
  CallerIdRequiredError,
  isFromRejectionReason,
  normaliseTrunkCallerID,
  resolveCallerID,
  resolveDisplayCallerID,
} from '../src/sip/callerId.js';

describe('resolveCallerID', () => {
  // Precedence: twimlCallerId → dialDefaultCallerId → sipUser → callerFrom → throw.

  it('1. TwiML callerId wins over everything', () => {
    const out = resolveCallerID({
      twimlCallerId: '+4930111',
      dialDefaultCallerId: '+4930222',
      sipUser: '2301086t3',
      callerFrom: '+4915123ani',
    });
    expect(out).toEqual({ callerId: '+4930111' });
  });

  it('2. DIAL_DEFAULT_CALLER_ID wins when TwiML callerId absent', () => {
    const out = resolveCallerID({
      twimlCallerId: '',
      dialDefaultCallerId: '+4930222',
      sipUser: '2301086t3',
      callerFrom: '+4915123ani',
    });
    expect(out).toEqual({ callerId: '+4930222' });
  });

  it('3. SIP_USER wins when TwiML + default absent', () => {
    const out = resolveCallerID({
      dialDefaultCallerId: '',
      sipUser: '2301086t3',
      callerFrom: '+4915123ani',
    });
    expect(out).toEqual({ callerId: '2301086t3' });
  });

  it('4. inbound From (preserve-ANI) is last-resort when SIP_USER unset', () => {
    const out = resolveCallerID({
      sipUser: '',
      callerFrom: '+4915123ani',
    });
    expect(out).toEqual({ callerId: '+4915123ani' });
  });

  it('5. throws CallerIdRequiredError when all sources empty', () => {
    expect(() => resolveCallerID({ sipUser: '' })).toThrow(CallerIdRequiredError);
  });

  it('5b. CallerIdRequiredError message carries Twilio code 13214', () => {
    try {
      resolveCallerID({ twimlCallerId: '', dialDefaultCallerId: '', sipUser: '', callerFrom: '' });
      throw new Error('expected throw');
    } catch (err) {
      expect(err).toBeInstanceOf(CallerIdRequiredError);
      expect((err as CallerIdRequiredError).message).toContain('13214');
      expect((err as CallerIdRequiredError).name).toBe('CallerIdRequiredError');
    }
  });

  it('treats all-whitespace values as absent and falls through', () => {
    const out = resolveCallerID({
      twimlCallerId: '   ',
      dialDefaultCallerId: '\t',
      sipUser: ' 2301086t3 ',
      callerFrom: '+4915123ani',
    });
    // whitespace twiml/default skipped; sipUser present (trimmed) wins.
    expect(out).toEqual({ callerId: '2301086t3' });
  });

  it('throws when every present source is whitespace-only', () => {
    expect(() =>
      resolveCallerID({ twimlCallerId: '  ', dialDefaultCallerId: ' ', sipUser: '  ', callerFrom: ' ' }),
    ).toThrow(CallerIdRequiredError);
  });
});

describe('resolveDisplayCallerID', () => {
  // Precedence: twimlCallerId → dialDefaultCallerId → callerFrom → "".
  // NB: no SIP_USER step here (auth artifact, never shown to callee).

  it('1. TwiML callerId wins', () => {
    expect(
      resolveDisplayCallerID({
        twimlCallerId: '+4930111',
        dialDefaultCallerId: '+4930222',
        callerFrom: '+4915123ani',
      }),
    ).toBe('+4930111');
  });

  it('2. DIAL_DEFAULT_CALLER_ID wins when TwiML callerId absent', () => {
    expect(
      resolveDisplayCallerID({
        twimlCallerId: '',
        dialDefaultCallerId: '+4930222',
        callerFrom: '+4915123ani',
      }),
    ).toBe('+4930222');
  });

  it('3. inbound From (preserve-ANI) when TwiML + default absent', () => {
    expect(resolveDisplayCallerID({ callerFrom: '+4915123ani' })).toBe('+4915123ani');
  });

  it('4. empty string when nothing specified (skip P-Asserted-Identity)', () => {
    expect(resolveDisplayCallerID({})).toBe('');
  });

  it('4b. empty string when all sources whitespace-only', () => {
    expect(
      resolveDisplayCallerID({ twimlCallerId: ' ', dialDefaultCallerId: '\t', callerFrom: '  ' }),
    ).toBe('');
  });

  it('does NOT consult SIP_USER (no such input on the display path)', () => {
    // Only callerFrom present → preserve-ANI; there is deliberately no sipUser field.
    expect(resolveDisplayCallerID({ callerFrom: '+4915999' })).toBe('+4915999');
  });
});

describe('normaliseTrunkCallerID', () => {
  const cases: Array<[name: string, value: string, cc: string | undefined, expected: string]> = [
    // Strip leading "+".
    ['strip leading +', '+4921193674951', '', '4921193674951'],
    ['strip leading + with cc', '+4921193674951', '49', '4921193674951'],
    // Leading 0 (national) + country code → replace 0 with cc.
    ['national 0 with cc=49', '021193674951', '49', '4921193674951'],
    // Strip leading "00" (international prefix), cc empty → no further change.
    ['strip 00 with cc empty', '00451234', '', '451234'],
    // Strip leading "00" then cc set: "451234" has no leading 0, unchanged.
    ['strip 00 with cc=49 no leading 0 remainder', '00451234', '49', '451234'],
    // Leading 0 without country code stays as-is.
    ['national 0 no cc untouched', '021193674951', '', '021193674951'],
    ['national 0 no cc arg untouched', '021193674951', undefined, '021193674951'],
    // Non-phone SIP username passes through unchanged.
    ['sip username passthrough', '2301086t3', '49', '2301086t3'],
    // Single "0" is NOT rewritten (rule requires length > 1).
    ['lone zero untouched', '0', '49', '0'],
    // Whitespace is trimmed.
    ['trims whitespace', '  +4921193674951  ', '', '4921193674951'],
    // "+" then leading 0 + cc: strip + leaves "0211...", then 0→cc.
    ['plus then national 0 with cc', '+021193674951', '49', '4921193674951'],
    // Already-normalised international number untouched.
    ['already normalised untouched', '4921193674951', '49', '4921193674951'],
    // cc itself whitespace → treated as empty, national 0 untouched.
    ['whitespace cc disables step 3', '021193674951', '  ', '021193674951'],
  ];

  for (const [name, value, cc, expected] of cases) {
    it(name, () => {
      const got = cc === undefined ? normaliseTrunkCallerID(value) : normaliseTrunkCallerID(value, cc);
      expect(got).toBe(expected);
    });
  }
});

describe('isFromRejectionReason', () => {
  const positive = [
    'Username in From Field required',
    'username in from field required', // case-insensitive
    'Bad From header',
    'invalid from-username',
    'P-Asserted-Identity not allowed',
    'caller-id rejected',
    'CallerID mismatch',
    'UNKNOWN CALLERID', // upper-case
  ];
  for (const reason of positive) {
    it(`matches: "${reason}"`, () => {
      expect(isFromRejectionReason(reason)).toBe(true);
    });
  }

  const negative = [
    'Forbidden',
    'Busy Here',
    'Service Unavailable',
    'Decline',
    '',
    'authentication failed', // unrelated 403
  ];
  for (const reason of negative) {
    it(`does not match: "${reason}"`, () => {
      expect(isFromRejectionReason(reason)).toBe(false);
    });
  }
});
