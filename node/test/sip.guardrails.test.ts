import { describe, expect, it } from 'vitest';

import {
  Guardrails,
  GuardrailError,
  GuardrailReason,
  maskTarget,
  normalizeTarget,
} from '../src/sip/guardrails.js';

// Capture the GuardrailError thrown by fn and return it (fails if none thrown).
function expectReject(fn: () => void): GuardrailError {
  try {
    fn();
  } catch (err) {
    expect(err).toBeInstanceOf(GuardrailError);
    return err as GuardrailError;
  }
  throw new Error('expected checkDial to throw, but it returned normally');
}

describe('normalizeTarget', () => {
  it('applies all documented transformations (matches guardrails.go)', () => {
    const cases: Array<[string, string]> = [
      ['+49 30 555', '+4930555'],
      ['0049 30', '+4930'],
      ['tel:+49', '+49'],
      ['sip:user@host', 'user@host'], // sip: strip keeps the user@host part
      ['  +49301234  ', '+49301234'],
      ['TEL:+4930', '+4930'], // case-insensitive scheme strip
      ['+4930', '+4930'], // already normalized
      ['00441234', '+441234'], // 00 → + international prefix
    ];
    for (const [input, want] of cases) {
      expect(normalizeTarget(input)).toBe(want);
    }
  });
});

describe('maskTarget', () => {
  it('fully masks short strings, keeps last 4 chars otherwise', () => {
    expect(maskTarget('')).toBe('');
    expect(maskTarget('+49')).toBe('***');
    expect(maskTarget('1234')).toBe('****');
    expect(maskTarget('+4930555')).toBe('****0555');
    expect(maskTarget('+12345678901')).toBe('********8901');
  });
});

describe('Guardrails allow-list', () => {
  it('empty allow-list denies everything (default-deny)', () => {
    const g = new Guardrails({ allowedPrefixes: [], maxPerSession: 3, maxPerMinute: 60 });
    const err = expectReject(() => g.checkDial('CA-1', '+4930555'));
    expect(err.reason).toBe(GuardrailReason.TollFraud);
    // empty target is also denied
    expect(expectReject(() => g.checkDial('CA-1', '')).reason).toBe(GuardrailReason.TollFraud);
  });

  it('allows a target matching any configured prefix', () => {
    const g = new Guardrails({ allowedPrefixes: ['+49', '+44'], maxPerSession: 3, maxPerMinute: 60 });
    expect(() => g.checkDial('CA-1', '+493012345')).not.toThrow();
    expect(() => g.checkDial('CA-2', '+441234')).not.toThrow();
  });

  it('rejects a target matching no prefix with toll_fraud reason', () => {
    const g = new Guardrails({ allowedPrefixes: ['+49'], maxPerSession: 3, maxPerMinute: 60 });
    const err = expectReject(() => g.checkDial('CA-1', '+1234567890'));
    expect(err.reason).toBe(GuardrailReason.TollFraud);
  });

  it('applies normalization before matching (00→+, sip:user@host, whitespace)', () => {
    const g = new Guardrails({ allowedPrefixes: ['+49'], maxPerSession: 10, maxPerMinute: 60 });
    // "0049..." normalizes to "+49..." → allowed
    expect(() => g.checkDial('CA-1', '0049 30 555')).not.toThrow();
    // whitespace-laden +49 → allowed
    expect(() => g.checkDial('CA-2', '  +49 30 1234  ')).not.toThrow();
    // sip:user@host normalizes to "user@host" → not +49 → rejected
    expect(expectReject(() => g.checkDial('CA-3', 'sip:user@host')).reason).toBe(
      GuardrailReason.TollFraud,
    );
  });

  it('normalizes configured prefixes (trim + lowercase) like config.Load', () => {
    const g = new Guardrails({ allowedPrefixes: ['  +49 ', ''], maxPerSession: 3, maxPerMinute: 60 });
    expect(() => g.checkDial('CA-1', '+4930555')).not.toThrow();
    // the empty entry must not turn this into an allow-all
    expect(expectReject(() => g.checkDial('CA-2', '+1555')).reason).toBe(GuardrailReason.TollFraud);
  });
});

describe('Guardrails per-session cap', () => {
  it('allows up to maxPerSession then rejects with rate_limit', () => {
    const g = new Guardrails({ allowedPrefixes: ['+49'], maxPerSession: 3, maxPerMinute: 1000 });
    for (let i = 0; i < 3; i++) {
      expect(() => g.checkDial('CA-limit', '+49123')).not.toThrow();
    }
    const err = expectReject(() => g.checkDial('CA-limit', '+49123'));
    expect(err.reason).toBe(GuardrailReason.RateLimit);
  });

  it('onSessionEnd resets the per-session counter', () => {
    const g = new Guardrails({ allowedPrefixes: ['+49'], maxPerSession: 2, maxPerMinute: 1000 });
    g.checkDial('CA-end', '+49123');
    g.checkDial('CA-end', '+49123');
    expect(expectReject(() => g.checkDial('CA-end', '+49123')).reason).toBe(GuardrailReason.RateLimit);

    g.onSessionEnd('CA-end');
    // fresh budget after reset
    expect(() => g.checkDial('CA-end', '+49123')).not.toThrow();
  });
});

describe('Guardrails global rolling-minute cap', () => {
  it('rejects with rate_limit once the global cap is reached within a window', () => {
    let nowMs = 1_000_000_000_000;
    const g = new Guardrails({
      allowedPrefixes: ['+49'],
      maxPerSession: 100,
      maxPerMinute: 2,
      now: () => nowMs,
    });
    expect(() => g.checkDial('CA-a', '+49123')).not.toThrow();
    expect(() => g.checkDial('CA-b', '+49123')).not.toThrow();
    const err = expectReject(() => g.checkDial('CA-c', '+49123'));
    expect(err.reason).toBe(GuardrailReason.RateLimit);
  });

  it('rolls back the per-session counter when the global gate rejects', () => {
    let nowMs = 1_000_000_000_000;
    const g = new Guardrails({
      allowedPrefixes: ['+49'],
      maxPerSession: 100,
      maxPerMinute: 1,
      now: () => nowMs,
    });
    g.checkDial('CA-a', '+49123'); // consumes the only global slot
    expect(expectReject(() => g.checkDial('CA-b', '+49123')).reason).toBe(GuardrailReason.RateLimit);
    // CA-b's session count was rolled back to 0, so once global frees up it has full budget.
    nowMs += 61_000;
    expect(() => g.checkDial('CA-b', '+49123')).not.toThrow();
  });

  it('admits 60 dials in a minute, rejects the 61st, then admits again as old buckets drop out', () => {
    let nowMs = 1_000_000_000_000;
    const g = new Guardrails({
      allowedPrefixes: ['+49'],
      maxPerSession: 1000,
      maxPerMinute: 60,
      now: () => nowMs,
    });
    // 60 dials spread one per second across buckets 0..59 — all within the rolling window.
    for (let i = 0; i < 60; i++) {
      expect(() => g.checkDial(`CA-${i}`, '+49123')).not.toThrow();
      nowMs += 1000;
    }
    // 61st at second 60: bucket 0 (from second 0) is now 60s old → drops out, so the
    // rolling sum is 59 < 60 → admitted, not rejected.
    expect(() => g.checkDial('CA-60', '+49123')).not.toThrow();

    // Stack a second dial into the same current bucket without advancing the clock:
    // rolling sum is back to 60 → next dial rejected.
    const err = expectReject(() => g.checkDial('CA-61', '+49123'));
    expect(err.reason).toBe(GuardrailReason.RateLimit);

    // Advance past the whole window so every bucket is stale → admitted again.
    nowMs += 61_000;
    expect(() => g.checkDial('CA-62', '+49123')).not.toThrow();
  });
});
