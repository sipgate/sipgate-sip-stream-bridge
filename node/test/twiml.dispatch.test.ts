import pino, { type Logger } from 'pino';
import { describe, expect, it } from 'vitest';

import {
  dispatch,
  type DialHandle,
  type DialOpts,
  type DialResult,
  type DialTarget,
  type MidCallTarget,
} from '../src/twiml/dispatch.js';
import { parseResponse } from '../src/twiml/parse.js';
import { dialHandler } from '../src/twiml/verbDial.js';
import { hangupHandler } from '../src/twiml/verbHangup.js';
import type { Dial } from '../src/twiml/verbs.js';

// Silent logger — pino at 'silent' level produces no output but is a real Logger.
function silentLogger(): Logger {
  return pino({ level: 'silent' });
}

// ── Fakes ───────────────────────────────────────────────────────────────────

class FakeMidCallTarget implements MidCallTarget {
  terminateCalls: string[] = [];
  private readonly logger = silentLogger();

  terminate(reason: string): void {
    this.terminateCalls.push(reason);
  }
  log(): Logger {
    return this.logger;
  }
}

class FakeHandle implements DialHandle {
  releaseCount = 0;
  release(): void {
    this.releaseCount++;
  }
}

class FakeDialTarget implements DialTarget {
  terminateCalls: string[] = [];
  prepareOpts: DialOpts[] = [];
  performCalls = 0;
  performTarget = '';
  prepareError: Error | null = null;
  performError: Error | null = null;
  performResult: DialResult | undefined = { status: 'answered' };
  handle = new FakeHandle();
  private readonly logger = silentLogger();

  log(): Logger {
    return this.logger;
  }
  terminate(reason: string): void {
    this.terminateCalls.push(reason);
  }
  async prepareDial(opts: DialOpts): Promise<DialHandle> {
    this.prepareOpts.push(opts);
    if (this.prepareError) throw this.prepareError;
    return this.handle;
  }
  async performDial(target: string, _opts: DialOpts, _handle: DialHandle): Promise<DialResult> {
    this.performCalls++;
    this.performTarget = target;
    if (this.performError) throw this.performError;
    if (this.performResult === undefined) throw new Error('no result configured');
    return this.performResult;
  }
  lastTerminateReason(): string | undefined {
    return this.terminateCalls.at(-1);
  }
}

// ── dispatch: terminal + warn-and-skip ───────────────────────────────────────

describe('dispatch', () => {
  it('nil-equivalent empty verbs: no terminate, resolves', async () => {
    const t = new FakeMidCallTarget();
    await dispatch({ verbs: [] }, t);
    expect(t.terminateCalls).toEqual([]);
  });

  it('<Hangup/> terminates with reason "hangup"', async () => {
    const t = new FakeMidCallTarget();
    await dispatch(parseResponse('<Response><Hangup/></Response>'), t);
    expect(t.terminateCalls).toEqual(['hangup']);
  });

  it('<Hangup/> is terminal — verb after it is unreachable', async () => {
    const t = new FakeMidCallTarget();
    await dispatch(parseResponse('<Response><Hangup/><Dial>+4912345</Dial></Response>'), t);
    expect(t.terminateCalls).toEqual(['hangup']);
  });

  it('<Dial> warn-and-skips when target is only MidCallTarget', async () => {
    const t = new FakeMidCallTarget();
    await dispatch(parseResponse('<Response><Dial>+4912345</Dial></Response>'), t);
    expect(t.terminateCalls).toEqual([]);
  });

  it('unknown verb warn-and-skips (no terminate)', async () => {
    const t = new FakeMidCallTarget();
    await dispatch(parseResponse('<Response><Say>hi</Say></Response>'), t);
    expect(t.terminateCalls).toEqual([]);
  });

  it('Connect / Reject / Redirect warn-and-skip', async () => {
    for (const xml of [
      '<Response><Connect><Stream url="wss://x/y"/></Connect></Response>',
      '<Response><Reject reason="busy"/></Response>',
      '<Response><Redirect>https://x/twiml</Redirect></Response>',
    ]) {
      const t = new FakeMidCallTarget();
      await dispatch(parseResponse(xml), t);
      expect(t.terminateCalls).toEqual([]);
    }
  });

  it('skipped verbs preceding terminal <Hangup> do not block it', async () => {
    const t = new FakeMidCallTarget();
    await dispatch(
      parseResponse(
        '<Response><Dial>+4912345</Dial><Connect><Stream url="wss://x/y"/></Connect><Hangup/></Response>',
      ),
      t,
    );
    expect(t.terminateCalls).toEqual(['hangup']);
  });

  it('routes <Dial> to dialHandler when target is a DialTarget', async () => {
    const t = new FakeDialTarget();
    await dispatch(parseResponse('<Response><Dial>+4912345</Dial></Response>'), t);
    expect(t.performCalls).toBe(1);
    expect(t.performTarget).toBe('+4912345');
    expect(t.lastTerminateReason()).toBe('completed');
  });
});

// ── hangupHandler ─────────────────────────────────────────────────────────────

describe('hangupHandler', () => {
  it('calls terminate("hangup")', async () => {
    const t = new FakeMidCallTarget();
    await hangupHandler(t);
    expect(t.terminateCalls).toEqual(['hangup']);
  });
});

// ── dialHandler ───────────────────────────────────────────────────────────────

function dialFrom(xml: string): Dial {
  const doc = parseResponse(xml);
  const v = doc.verbs[0];
  if (v.kind !== 'Dial') throw new Error(`expected Dial, got ${v.kind}`);
  return v;
}

describe('dialHandler', () => {
  it('bare-text target: performDial + terminate("completed")', async () => {
    const t = new FakeDialTarget();
    await dialHandler(dialFrom('<Response><Dial>+4912345</Dial></Response>'), t);
    expect(t.performCalls).toBe(1);
    expect(t.performTarget).toBe('+4912345');
    expect(t.lastTerminateReason()).toBe('completed');
    expect(t.handle.releaseCount).toBe(1);
  });

  it('<Number> child target: same outcome', async () => {
    const t = new FakeDialTarget();
    await dialHandler(dialFrom('<Response><Dial><Number>+4912345</Number></Dial></Response>'), t);
    expect(t.performTarget).toBe('+4912345');
    expect(t.lastTerminateReason()).toBe('completed');
  });

  it('ambiguous: <Number> wins over bare-text', async () => {
    const t = new FakeDialTarget();
    const dial: Dial = {
      kind: 'Dial',
      name: 'Dial',
      hangupOnStar: false,
      hasSip: false,
      hasClient: false,
      hasConference: false,
      hasQueue: false,
      numberText: '+4911111',
      number: { text: '+4987654' },
    };
    await dialHandler(dial, t);
    expect(t.performTarget).toBe('+4987654');
  });

  it('empty target: no prepare/perform/terminate', async () => {
    const t = new FakeDialTarget();
    await dialHandler(dialFrom('<Response><Dial></Dial></Response>'), t);
    expect(t.prepareOpts).toHaveLength(0);
    expect(t.performCalls).toBe(0);
    expect(t.terminateCalls).toHaveLength(0);
  });

  it('prepareDial fails → terminate("failed"), no performDial', async () => {
    const t = new FakeDialTarget();
    t.prepareError = new Error('port pool exhausted');
    await dialHandler(dialFrom('<Response><Dial>+4912345</Dial></Response>'), t);
    expect(t.performCalls).toBe(0);
    expect(t.lastTerminateReason()).toBe('failed');
  });

  it('performDial throws → terminate("failed"), handle released', async () => {
    const t = new FakeDialTarget();
    t.performResult = undefined;
    t.performError = new Error('forwarder rejected');
    await dialHandler(dialFrom('<Response><Dial>+4912345</Dial></Response>'), t);
    expect(t.lastTerminateReason()).toBe('failed');
    expect(t.handle.releaseCount).toBe(1);
  });

  it.each([
    ['answered', 'completed'],
    ['busy', 'busy'],
    ['no-answer', 'no-answer'],
    ['canceled', 'canceled'],
    ['hangup-star', 'completed'],
    ['something-else', 'failed'],
  ])('status %s → reason %s', async (status, reason) => {
    const t = new FakeDialTarget();
    t.performResult = { status };
    await dialHandler(dialFrom('<Response><Dial>+4912345</Dial></Response>'), t);
    expect(t.lastTerminateReason()).toBe(reason);
  });

  it('applies defaults: timeout 30s, timeLimit 14400s, method POST', async () => {
    const t = new FakeDialTarget();
    await dialHandler(dialFrom('<Response><Dial>+4912345</Dial></Response>'), t);
    const opts = t.prepareOpts[0];
    expect(opts.timeoutMs).toBe(30_000);
    expect(opts.timeLimitMs).toBe(14_400_000);
    expect(opts.method).toBe('POST');
    expect(opts.statusCallbackMethod).toBe('POST');
  });

  it('honors explicit timeout/timeLimit (seconds → ms)', async () => {
    const t = new FakeDialTarget();
    await dialHandler(
      dialFrom('<Response><Dial timeout="20" timeLimit="600">+49</Dial></Response>'),
      t,
    );
    expect(t.prepareOpts[0].timeoutMs).toBe(20_000);
    expect(t.prepareOpts[0].timeLimitMs).toBe(600_000);
  });

  it('per-<Number> statusCallback overrides parent <Dial>', async () => {
    const t = new FakeDialTarget();
    const dial: Dial = {
      kind: 'Dial',
      name: 'Dial',
      hangupOnStar: false,
      hasSip: false,
      hasClient: false,
      hasConference: false,
      hasQueue: false,
      statusCallback: 'https://parent/cb',
      statusCallbackMethod: 'POST',
      statusCallbackEvents: ['initiated', 'completed'],
      number: {
        text: '+4912345',
        statusCallback: 'https://leg/cb',
        statusCallbackMethod: 'GET',
        statusCallbackEvents: ['answered', 'completed'],
      },
    };
    await dialHandler(dial, t);
    const opts = t.prepareOpts[0];
    expect(opts.statusCallback).toBe('https://leg/cb');
    expect(opts.statusCallbackMethod).toBe('GET');
    expect(opts.statusCallbackEvents).toEqual(['answered', 'completed']);
  });

  it('parent <Dial> statusCallback used when <Number> has none', async () => {
    const t = new FakeDialTarget();
    const dial: Dial = {
      kind: 'Dial',
      name: 'Dial',
      hangupOnStar: false,
      hasSip: false,
      hasClient: false,
      hasConference: false,
      hasQueue: false,
      statusCallback: 'https://parent/cb',
      statusCallbackMethod: 'POST',
      statusCallbackEvents: ['initiated', 'completed'],
      number: { text: '+4912345' },
    };
    await dialHandler(dial, t);
    const opts = t.prepareOpts[0];
    expect(opts.statusCallback).toBe('https://parent/cb');
    expect(opts.statusCallbackMethod).toBe('POST');
  });
});
