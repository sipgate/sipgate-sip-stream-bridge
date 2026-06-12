import { describe, expect, it } from 'vitest';

import {
  ERROR_CODE_MALFORMED,
  parseResponse,
  parseStatusCallbackEvents,
  TwimlError,
} from '../src/twiml/parse.js';
import { resolveDialTarget, type Dial } from '../src/twiml/verbs.js';

// Go testdata fixtures, inlined.
const DIAL_BARE_TEXT = '<Response><Dial>+4912345</Dial></Response>';
const DIAL_NUMBER_CHILD = '<Response><Dial><Number>+4912345</Number></Dial></Response>';

function firstDial(xml: string): Dial {
  const doc = parseResponse(xml);
  expect(doc.verbs.length).toBeGreaterThan(0);
  const v = doc.verbs[0];
  expect(v.kind).toBe('Dial');
  return v as Dial;
}

describe('parseResponse — root validation', () => {
  it('throws 12100 when root is not <Response>', () => {
    try {
      parseResponse('<NotResponse><Hangup/></NotResponse>');
      throw new Error('expected throw');
    } catch (e) {
      expect(e).toBeInstanceOf(TwimlError);
      expect((e as TwimlError).code).toBe(ERROR_CODE_MALFORMED);
    }
  });

  it('throws 12100 on empty document', () => {
    try {
      parseResponse('');
      throw new Error('expected throw');
    } catch (e) {
      expect(e).toBeInstanceOf(TwimlError);
      expect((e as TwimlError).code).toBe(12100);
      expect((e as TwimlError).message).toContain('12100');
    }
  });

  it('throws 12100 on malformed/truncated XML', () => {
    try {
      parseResponse('<Response><Hangup');
      throw new Error('expected throw');
    } catch (e) {
      expect(e).toBeInstanceOf(TwimlError);
      expect((e as TwimlError).code).toBe(12100);
    }
  });
});

describe('parseResponse — verb walk', () => {
  it('parses self-closing <Hangup/>', () => {
    const doc = parseResponse('<Response><Hangup/></Response>');
    expect(doc.verbs).toHaveLength(1);
    expect(doc.verbs[0].kind).toBe('Hangup');
  });

  it('retains unknown verbs in document order (warn-skip later)', () => {
    const doc = parseResponse('<Response><Say>hi</Say><Hangup/></Response>');
    expect(doc.verbs).toHaveLength(2);
    expect(doc.verbs[0].kind).toBe('Unknown');
    expect(doc.verbs[0].name).toBe('Say');
    expect(doc.verbs[1].kind).toBe('Hangup');
  });

  it('parses <Connect><Stream url=…>', () => {
    const doc = parseResponse('<Response><Connect><Stream url="wss://x/y"/></Connect></Response>');
    expect(doc.verbs).toHaveLength(1);
    expect(doc.verbs[0].kind).toBe('Connect');
    const c = doc.verbs[0];
    if (c.kind !== 'Connect') throw new Error('not connect');
    expect(c.stream?.url).toBe('wss://x/y');
  });

  it('parses <Redirect> + <Reject> attributes', () => {
    const doc = parseResponse(
      '<Response><Redirect method="POST">https://x/twiml</Redirect><Reject reason="busy"/></Response>',
    );
    expect(doc.verbs).toHaveLength(2);
    const r = doc.verbs[0];
    if (r.kind !== 'Redirect') throw new Error('not redirect');
    expect(r.method).toBe('POST');
    expect(r.url).toBe('https://x/twiml');
    const rj = doc.verbs[1];
    if (rj.kind !== 'Reject') throw new Error('not reject');
    expect(rj.reason).toBe('busy');
  });

  it('preserves order with a self-closing root', () => {
    const doc = parseResponse('<Response/>');
    expect(doc.verbs).toHaveLength(0);
  });

  it('handles XML entities in chardata and attrs', () => {
    const doc = parseResponse(
      '<Response><Redirect method="GET">https://x?a=1&amp;b=2&#65;</Redirect></Response>',
    );
    const r = doc.verbs[0];
    if (r.kind !== 'Redirect') throw new Error('not redirect');
    expect(r.url).toBe('https://x?a=1&b=2A');
  });

  it('skips comments and processing instructions', () => {
    const doc = parseResponse(
      '<?xml version="1.0"?><!-- hi --><Response><Hangup/></Response>',
    );
    expect(doc.verbs).toHaveLength(1);
    expect(doc.verbs[0].kind).toBe('Hangup');
  });
});

describe('parseResponse — <Dial> targets (Go testdata)', () => {
  it('bare-text fixture resolves to +4912345', () => {
    const d = firstDial(DIAL_BARE_TEXT);
    expect(resolveDialTarget(d)).toEqual({ target: '+4912345', ambiguous: false });
  });

  it('number-child fixture resolves to +4912345', () => {
    const d = firstDial(DIAL_NUMBER_CHILD);
    expect(resolveDialTarget(d)).toEqual({ target: '+4912345', ambiguous: false });
  });

  it('ambiguous: <Number> wins over bare-text', () => {
    const d: Dial = {
      kind: 'Dial',
      name: 'Dial',
      hangupOnStar: false,
      hasSip: false,
      hasClient: false,
      hasConference: false,
      hasQueue: false,
      numberText: ' +4900000 ',
      number: { text: '+4912345' },
    };
    expect(resolveDialTarget(d)).toEqual({ target: '+4912345', ambiguous: true });
  });

  it('empty <Dial> resolves to empty target', () => {
    const d = firstDial('<Response><Dial></Dial></Response>');
    expect(resolveDialTarget(d)).toEqual({ target: '', ambiguous: false });
  });
});

describe('parseResponse — <Dial> attribute parsing', () => {
  it('parses callerId, hangupOnStar, action, method, answerOnBridge', () => {
    const d = firstDial(
      '<Response><Dial callerId="+49111" hangupOnStar="true" action="https://a" method="GET" answerOnBridge="true">+49</Dial></Response>',
    );
    expect(d.callerId).toBe('+49111');
    expect(d.hangupOnStar).toBe(true);
    expect(d.action).toBe('https://a');
    expect(d.method).toBe('GET');
    expect(d.answerOnBridge).toBe(true);
  });

  it('parses numeric timeout/timeLimit', () => {
    const d = firstDial('<Response><Dial timeout="20" timeLimit="600">+49</Dial></Response>');
    expect(d.timeout).toBe(20);
    expect(d.timeLimit).toBe(600);
  });

  it('silently ignores non-numeric timeout/timeLimit (stays undefined)', () => {
    const d = firstDial('<Response><Dial timeout="abc" timeLimit="">+49</Dial></Response>');
    expect(d.timeout).toBeUndefined();
    expect(d.timeLimit).toBeUndefined();
  });

  it('hangupOnStar matches exactly "true"', () => {
    const d = firstDial('<Response><Dial hangupOnStar="True">+49</Dial></Response>');
    expect(d.hangupOnStar).toBe(false);
  });
});

describe('statusCallbackEvent parsing', () => {
  it('parses space-separated events on <Dial>', () => {
    const d = firstDial(
      '<Response><Dial statusCallback="https://x/cb" statusCallbackMethod="POST" statusCallbackEvent="initiated ringing answered completed">+49</Dial></Response>',
    );
    expect(d.statusCallback).toBe('https://x/cb');
    expect(d.statusCallbackMethod).toBe('POST');
    expect(d.statusCallbackEvents).toEqual(['initiated', 'ringing', 'answered', 'completed']);
  });

  it('parses comma-separated events identically', () => {
    const d = firstDial(
      '<Response><Dial statusCallbackEvent="initiated,ringing,answered,completed">+49</Dial></Response>',
    );
    expect(d.statusCallbackEvents).toEqual(['initiated', 'ringing', 'answered', 'completed']);
  });

  it('parses mixed comma+space separators', () => {
    const d = firstDial(
      '<Response><Dial statusCallbackEvent="initiated, ringing answered,completed">+49</Dial></Response>',
    );
    expect(d.statusCallbackEvents).toEqual(['initiated', 'ringing', 'answered', 'completed']);
  });

  it('rejects unknown event on <Dial> with 12100 citing the value', () => {
    try {
      parseResponse('<Response><Dial statusCallbackEvent="initiated foo">+49</Dial></Response>');
      throw new Error('expected throw');
    } catch (e) {
      expect(e).toBeInstanceOf(TwimlError);
      expect((e as TwimlError).code).toBe(12100);
      expect((e as TwimlError).message).toContain('foo');
    }
  });

  it('rejects unknown event on <Number> citing the value', () => {
    try {
      parseResponse(
        '<Response><Dial><Number statusCallbackEvent="answered ringinX">+49</Number></Dial></Response>',
      );
      throw new Error('expected throw');
    } catch (e) {
      expect(e).toBeInstanceOf(TwimlError);
      expect((e as TwimlError).message).toContain('ringinX');
    }
  });

  it('preserves per-<Number> status fields + text (backward compat)', () => {
    const withCb = firstDial(
      '<Response><Dial><Number statusCallback="https://leg/cb" statusCallbackMethod="GET" statusCallbackEvent="answered completed">+4915123456789</Number></Dial></Response>',
    );
    expect(withCb.number?.text).toBe('+4915123456789');
    expect(withCb.number?.statusCallback).toBe('https://leg/cb');
    expect(withCb.number?.statusCallbackMethod).toBe('GET');
    expect(withCb.number?.statusCallbackEvents).toEqual(['answered', 'completed']);

    const bare = firstDial('<Response><Dial><Number>+4915123456789</Number></Dial></Response>');
    expect(bare.number?.text).toBe('+4915123456789');
    expect(bare.number?.statusCallback).toBeUndefined();
    expect(bare.number?.statusCallbackEvents).toBeUndefined();
  });
});

describe('parseStatusCallbackEvents (direct)', () => {
  it('returns undefined on empty input', () => {
    expect(parseStatusCallbackEvents('')).toBeUndefined();
  });

  it('accepts the full documented enum', () => {
    const full = 'initiated ringing answered in-progress completed busy failed no-answer canceled';
    expect(parseStatusCallbackEvents(full)).toEqual([
      'initiated',
      'ringing',
      'answered',
      'in-progress',
      'completed',
      'busy',
      'failed',
      'no-answer',
      'canceled',
    ]);
  });

  it('throws 12100 on an unknown value', () => {
    expect(() => parseStatusCallbackEvents('initiated bogus')).toThrow(TwimlError);
  });
});
