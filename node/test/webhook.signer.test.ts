import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';
import { sign } from '../src/webhook/signer.js';

// ─── Golden-vector fixtures ─────────────────────────────────────────────────
// These are the cross-language byte-fidelity gate: every expected_signature
// was emitted by upstream twilio-python / twilio-node and the Go port already
// reproduces them. The TS sign() MUST reproduce every one exactly — that is
// what proves byte-compatibility with X-Twilio-Signature.

interface Fixture {
  name: string;
  source_lib: string;
  auth_token: string;
  url: string;
  params: Record<string, string[]>;
  expected_signature: string;
}

function loadFixtures(name: string): Fixture[] {
  const path = fileURLToPath(
    new URL(`../../go/internal/webhook/testdata/${name}`, import.meta.url),
  );
  const out = JSON.parse(readFileSync(path, 'utf8')) as Fixture[];
  // Mirror the Go test's 12-fixture floor for coverage.
  expect(out.length).toBeGreaterThanOrEqual(12);
  return out;
}

const pythonFixtures = loadFixtures('python_fixtures.json');
const nodeFixtures = loadFixtures('node_fixtures.json');

describe('sign — twilio-python golden vectors', () => {
  for (const f of pythonFixtures) {
    it(f.name, () => {
      expect(sign(f.auth_token, f.url, f.params)).toBe(f.expected_signature);
    });
  }
});

describe('sign — twilio-node golden vectors', () => {
  for (const f of nodeFixtures) {
    it(f.name, () => {
      expect(sign(f.auth_token, f.url, f.params)).toBe(f.expected_signature);
    });
  }
});

describe('sign — cross-library parity', () => {
  it('python and node emit identical signatures per shared fixture name', () => {
    const pyMap = new Map(pythonFixtures.map((f) => [f.name, f.expected_signature]));
    for (const n of nodeFixtures) {
      const py = pyMap.get(n.name);
      if (py !== undefined) {
        expect(n.expected_signature, `divergence on ${n.name}`).toBe(py);
      }
    }
  });
});

// ─── Named cases called out by the brief ────────────────────────────────────

describe('sign — load-bearing details', () => {
  it('basic single-value case (fixture_a_basic)', () => {
    expect(
      sign('12345', 'https://mycompany.com/myapp.php?foo=1&bar=2', {
        CallSid: ['CA1234567890ABCDE'],
        Digits: ['1234'],
        From: ['+14158675309'],
        To: ['+18005551212'],
        Caller: ['+14158675309'],
      }),
    ).toBe('RSOYDt4T1cUTdK1PDd93/VVr8B8=');
  });

  it('duplicate_values: dedupe then sort, not submission order', () => {
    expect(
      sign('12345', 'https://mycompany.com/myapp.php?foo=1&bar=2', {
        Sid: ['CA123'],
        SidAccount: ['AC123'],
        Digits: ['5678', '1234', '1234'],
      }),
    ).toBe('IK+Dwps556ElfBT0I3Rgjkr1wJU=');
  });

  it('special_chars_plus: + signed verbatim, not decoded to space', () => {
    expect(
      sign('12345', 'https://customer.example/cb', {
        From: ['+4915123456789'],
        Body: ['hello+world'],
      }),
    ).toBe('RPmvJzz674XdYurPkoSZxwiOiWM=');
  });

  it('utf8_value: German umlauts signed as UTF-8 bytes', () => {
    expect(
      sign('12345', 'https://customer.example/cb', {
        Caller: ['Müller'],
        Greeting: ['Grüß Gott'],
      }),
    ).toBe('1u+8eedolvqsC8ybKmeUbdyKfrU=');
  });

  it('status_callback_completed: realistic terminal-event payload', () => {
    expect(
      sign('12345', 'https://customer.example/status', {
        CallSid: ['CAdeadbeef00000000000000000000abcd'],
        AccountSid: ['ACdeadbeef00000000000000000000abcd'],
        From: ['+4915123456789'],
        To: ['+4930111222333'],
        Direction: ['inbound'],
        ApiVersion: ['2010-04-01'],
        CallStatus: ['completed'],
        Timestamp: ['Mon, 01 May 2026 18:00:00 +0000'],
        SequenceNumber: ['3'],
        CallbackSource: ['call-progress-events'],
        CallDuration: ['42'],
        Duration: ['1'],
        SipResponseCode: ['200'],
        Caller: ['+4915123456789'],
        Called: ['+4930111222333'],
      }),
    ).toBe('4f3RC3ew0KDtE+5ewc92V07QPag=');
  });

  it('url-only: empty params signs the URL bytes only', () => {
    expect(sign('12345', 'https://customer.example/cb', {})).toBe(
      '+xE6fKV5OcQGR58lHUAbQmwfuO8=',
    );
  });

  it('case-sensitive ASCII key sort is order-independent', () => {
    const a = sign('t', 'u', { Z: ['1'], a: ['2'] });
    const b = sign('t', 'u', { a: ['2'], Z: ['1'] });
    expect(a).toBe(b);
  });

  it('base64 standard alphabet with = padding (28 chars)', () => {
    const sig = sign('12345', 'https://customer.example/cb', {});
    expect(sig).toHaveLength(28);
    expect(sig.endsWith('=')).toBe(true);
  });
});

describe('sign — URLSearchParams input', () => {
  it('reproduces fixture_a_basic via URLSearchParams', () => {
    const usp = new URLSearchParams();
    usp.append('CallSid', 'CA1234567890ABCDE');
    usp.append('Digits', '1234');
    usp.append('From', '+14158675309');
    usp.append('To', '+18005551212');
    usp.append('Caller', '+14158675309');
    expect(sign('12345', 'https://mycompany.com/myapp.php?foo=1&bar=2', usp)).toBe(
      'RSOYDt4T1cUTdK1PDd93/VVr8B8=',
    );
  });

  it('handles multi-value keys via URLSearchParams (dedupe+sort)', () => {
    const usp = new URLSearchParams();
    usp.append('Sid', 'CA123');
    usp.append('SidAccount', 'AC123');
    usp.append('Digits', '5678');
    usp.append('Digits', '1234');
    usp.append('Digits', '1234');
    expect(sign('12345', 'https://mycompany.com/myapp.php?foo=1&bar=2', usp)).toBe(
      'IK+Dwps556ElfBT0I3Rgjkr1wJU=',
    );
  });
});
