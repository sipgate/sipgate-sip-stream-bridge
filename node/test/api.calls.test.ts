/**
 * Tests for the Twilio-compatible Call REST handlers (calls.ts), exercised
 * end-to-end through createApiHandler over a real http.Server on an ephemeral
 * 127.0.0.1 port. A stub CallManager backs the handlers (no real SIP); the
 * Url= fetch is injected as a fake.
 */

import http from 'node:http';
import type { AddressInfo } from 'node:net';

import pino, { type Logger } from 'pino';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { BridgeCall, CallManager, CallSession } from '../src/bridge/callManager.js';
import { setAsyncDispatch, type TwimlFetcher } from '../src/api/calls.js';
import { createApiHandler } from '../src/api/server.js';
import type { Config } from '../src/config/index.js';

/** Alias the global fetch Response so the helper return types read clearly. */
type Response = globalThis.Response;

const ACCOUNT_SID = 'AC' + '0123456789abcdef0123456789abcdef';
const AUTH_TOKEN = 'test-auth-token-0123456789abcdef';
const CALL_SID = 'CA0123456789abcdef0123456789abcdef';

function silentLogger(): Logger {
  return pino({ level: 'silent' });
}

/** Authorization header for the configured credentials. */
function authHeader(user = ACCOUNT_SID, pass = AUTH_TOKEN): string {
  return 'Basic ' + Buffer.from(`${user}:${pass}`).toString('base64');
}

/**
 * Stub CallManager. Implements only the methods the handlers call. terminate
 * flips the backing call's status/endTime and marks it inactive — so the
 * post-mutation re-fetch in writeCallJSON observes the terminal state.
 */
class StubCallManager {
  readonly accountSid = ACCOUNT_SID;
  private readonly calls = new Map<string, BridgeCall>();
  private readonly active = new Set<string>();
  private readonly sessions = new Map<string, CallSession>();
  terminateLog: Array<{ callSid: string; reason: string }> = [];

  addActiveCall(call: BridgeCall): void {
    this.calls.set(call.callSid, call);
    this.active.add(call.callSid);
    // Minimal CallSession stub — MidCallAdapter only reads callSid + log.
    this.sessions.set(call.callSid, {
      callSid: call.callSid,
      log: silentLogger(),
    } as unknown as CallSession);
  }

  addTerminatedCall(call: BridgeCall): void {
    this.calls.set(call.callSid, call);
  }

  list(): BridgeCall[] {
    return [...this.calls.values()];
  }

  getByCallSid(callSid: string): BridgeCall | undefined {
    return this.calls.get(callSid);
  }

  getSessionByCallSid(callSid: string): CallSession | undefined {
    return this.sessions.get(callSid);
  }

  isActive(callSid: string): boolean {
    return this.active.has(callSid);
  }

  statusCallbackLog: string[] = [];
  setStatusCallback(session: CallSession, _cfg: unknown): void {
    this.statusCallbackLog.push(session.callSid);
  }

  terminateByCallSid(callSid: string, reason: string): boolean {
    this.terminateLog.push({ callSid, reason });
    if (!this.active.has(callSid)) {
      return false; // idempotent — already terminated
    }
    this.active.delete(callSid);
    this.sessions.delete(callSid);
    const call = this.calls.get(callSid);
    if (call !== undefined) {
      const end = new Date();
      this.calls.set(callSid, {
        ...call,
        status: reason === 'remote_bye' ? 'completed' : 'completed',
        endTime: end,
        duration: 0,
      });
    }
    return true;
  }

  closeStream(): void {
    /* unused by these tests */
  }
}

function makeCall(overrides: Partial<BridgeCall> = {}): BridgeCall {
  return {
    callSid: CALL_SID,
    accountSid: ACCOUNT_SID,
    from: 'sip:alice@example.com',
    to: 'sip:bob@example.com',
    status: 'in-progress',
    direction: 'inbound',
    startTime: new Date('2026-04-27T10:00:00Z'),
    endTime: null,
    duration: null,
    answeredBy: null,
    parentCallSid: null,
    ...overrides,
  };
}

const baseConfig = { AUTH_TOKEN } as unknown as Config;

/** Fake fetcher whose behavior is set per-test. */
function makeFetcher(impl: TwimlFetcher['fetch']): TwimlFetcher {
  return { fetch: impl };
}

interface Harness {
  url: string;
  manager: StubCallManager;
  close: () => Promise<void>;
}

async function startServer(
  manager: StubCallManager,
  fetcher?: TwimlFetcher,
): Promise<Harness> {
  const handler = createApiHandler({
    manager: manager as unknown as CallManager,
    config: baseConfig,
    log: silentLogger(),
    fetcher,
  });
  const server = http.createServer((req, res) => {
    if (handler(req, res)) {
      return;
    }
    res.writeHead(404, { 'Content-Length': '0' });
    res.end();
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const { port } = server.address() as AddressInfo;
  return {
    url: `http://127.0.0.1:${port}`,
    manager,
    close: () =>
      new Promise<void>((resolve) => {
        server.close(() => resolve());
      }),
  };
}

function callsPath(): string {
  return `/2010-04-01/Accounts/${ACCOUNT_SID}/Calls.json`;
}
function callPath(callSid = CALL_SID): string {
  return `/2010-04-01/Accounts/${ACCOUNT_SID}/Calls/${callSid}.json`;
}

describe('listCallsHandler', () => {
  let h: Harness;
  afterEach(async () => {
    await h.close();
  });

  it('returns a paginated envelope with the correct shape', async () => {
    const mgr = new StubCallManager();
    for (let i = 0; i < 3; i++) {
      mgr.addActiveCall(makeCall({ callSid: `CA${'0'.repeat(31)}${i}` }));
    }
    h = await startServer(mgr);
    const resp = await fetch(`${h.url}${callsPath()}`, { headers: { authorization: authHeader() } });
    expect(resp.status).toBe(200);
    const body = (await resp.json()) as Record<string, unknown>;
    expect(body.page).toBe(0);
    expect(body.page_size).toBe(50);
    expect(Array.isArray(body.calls)).toBe(true);
    expect((body.calls as unknown[]).length).toBe(3);
    expect(body.first_page_uri).toContain('/Calls.json?Page=0');
    expect(body.next_page_uri).toBeNull();
    expect(body.previous_page_uri).toBeNull();
  });

  it('honors Page and PageSize query params and emits next/prev URIs', async () => {
    const mgr = new StubCallManager();
    for (let i = 0; i < 5; i++) {
      mgr.addActiveCall(makeCall({ callSid: `CA${'0'.repeat(31)}${i}` }));
    }
    h = await startServer(mgr);
    const resp = await fetch(`${h.url}${callsPath()}?Page=1&PageSize=2`, {
      headers: { authorization: authHeader() },
    });
    const body = (await resp.json()) as Record<string, unknown>;
    expect(body.page).toBe(1);
    expect(body.page_size).toBe(2);
    expect((body.calls as unknown[]).length).toBe(2);
    expect(body.next_page_uri).toContain('Page=2');
    expect(body.previous_page_uri).toContain('Page=0');
  });

  it('returns an empty calls array (200) for an out-of-range page', async () => {
    const mgr = new StubCallManager();
    mgr.addActiveCall(makeCall());
    h = await startServer(mgr);
    const resp = await fetch(`${h.url}${callsPath()}?Page=99`, {
      headers: { authorization: authHeader() },
    });
    expect(resp.status).toBe(200);
    const body = (await resp.json()) as Record<string, unknown>;
    expect((body.calls as unknown[]).length).toBe(0);
  });
});

describe('getCallHandler', () => {
  let h: Harness;
  afterEach(async () => {
    await h.close();
  });

  it('returns 200 + the Call resource on a hit', async () => {
    const mgr = new StubCallManager();
    mgr.addActiveCall(makeCall());
    h = await startServer(mgr);
    const resp = await fetch(`${h.url}${callPath()}`, { headers: { authorization: authHeader() } });
    expect(resp.status).toBe(200);
    const body = (await resp.json()) as Record<string, unknown>;
    expect(body.sid).toBe(CALL_SID);
    expect(body.account_sid).toBe(ACCOUNT_SID);
    expect(body.status).toBe('in-progress');
    expect(body.uri).toBe(`/2010-04-01/Accounts/${ACCOUNT_SID}/Calls/${CALL_SID}.json`);
  });

  it('returns 404 + 20404 on a miss', async () => {
    const mgr = new StubCallManager();
    h = await startServer(mgr);
    const resp = await fetch(`${h.url}${callPath()}`, { headers: { authorization: authHeader() } });
    expect(resp.status).toBe(404);
    const body = (await resp.json()) as Record<string, unknown>;
    expect(body.code).toBe(20404);
  });

  it('returns 404 (NOT 400) on a malformed CallSid', async () => {
    const mgr = new StubCallManager();
    h = await startServer(mgr);
    const resp = await fetch(`${h.url}${callPath('CAnothex')}`, {
      headers: { authorization: authHeader() },
    });
    expect(resp.status).toBe(404);
    const body = (await resp.json()) as Record<string, unknown>;
    expect(body.code).toBe(20404);
  });
});

describe('modifyCallHandler — validation', () => {
  let h: Harness;
  afterEach(async () => {
    await h.close();
  });

  async function post(body: string, callSid = CALL_SID): Promise<Response> {
    return fetch(`${h.url}${callPath(callSid)}`, {
      method: 'POST',
      headers: {
        authorization: authHeader(),
        'content-type': 'application/x-www-form-urlencoded',
      },
      body,
    });
  }

  it('rejects when none of {Twiml,Url,Status,StatusCallback} is set → 21218', async () => {
    const mgr = new StubCallManager();
    mgr.addActiveCall(makeCall());
    h = await startServer(mgr);
    const resp = await post('');
    expect(resp.status).toBe(400);
    expect(((await resp.json()) as Record<string, unknown>).code).toBe(21218);
  });

  it('rejects multiple of {Twiml,Url,Status} → 21218', async () => {
    const mgr = new StubCallManager();
    mgr.addActiveCall(makeCall());
    h = await startServer(mgr);
    const resp = await post('Twiml=%3CResponse%2F%3E&Status=completed');
    expect(resp.status).toBe(400);
    expect(((await resp.json()) as Record<string, unknown>).code).toBe(21218);
  });

  it('rejects Twiml longer than 4000 chars → 12100', async () => {
    const mgr = new StubCallManager();
    mgr.addActiveCall(makeCall());
    h = await startServer(mgr);
    const big = '<Response>' + 'x'.repeat(4001) + '</Response>';
    const resp = await post(`Twiml=${encodeURIComponent(big)}`);
    expect(resp.status).toBe(400);
    expect(((await resp.json()) as Record<string, unknown>).code).toBe(12100);
  });

  it('rejects Status != completed → 21218', async () => {
    const mgr = new StubCallManager();
    mgr.addActiveCall(makeCall());
    h = await startServer(mgr);
    const resp = await post('Status=ringing');
    expect(resp.status).toBe(400);
    expect(((await resp.json()) as Record<string, unknown>).code).toBe(21218);
  });

  it('rejects an SSRF-blocked StatusCallback → 21218', async () => {
    const mgr = new StubCallManager();
    mgr.addActiveCall(makeCall());
    h = await startServer(mgr);
    const resp = await post('StatusCallback=' + encodeURIComponent('https://127.0.0.1/cb'));
    expect(resp.status).toBe(400);
    expect(((await resp.json()) as Record<string, unknown>).code).toBe(21218);
  });

  it('rejects an unknown StatusCallbackEvent → 21218', async () => {
    const mgr = new StubCallManager();
    mgr.addActiveCall(makeCall());
    h = await startServer(mgr);
    const resp = await post(
      'StatusCallback=' +
        encodeURIComponent('https://example.com/cb') +
        '&StatusCallbackEvent=bogus',
    );
    expect(resp.status).toBe(400);
    expect(((await resp.json()) as Record<string, unknown>).code).toBe(21218);
  });
});

describe('modifyCallHandler — behavior', () => {
  let h: Harness;
  let restore: boolean;
  beforeEach(() => {
    restore = setAsyncDispatch(false); // sync dispatch for deterministic assertions
  });
  afterEach(async () => {
    setAsyncDispatch(restore);
    await h.close();
  });

  async function post(body: string, callSid = CALL_SID): Promise<Response> {
    return fetch(`${h.url}${callPath(callSid)}`, {
      method: 'POST',
      headers: {
        authorization: authHeader(),
        'content-type': 'application/x-www-form-urlencoded',
      },
      body,
    });
  }

  it('Status=completed terminates an active call and returns 200', async () => {
    const mgr = new StubCallManager();
    mgr.addActiveCall(makeCall());
    h = await startServer(mgr);
    const resp = await post('Status=completed');
    expect(resp.status).toBe(200);
    expect(mgr.terminateLog).toEqual([{ callSid: CALL_SID, reason: 'completed' }]);
    const body = (await resp.json()) as Record<string, unknown>;
    expect(body.status).toBe('completed');
  });

  it('Status=completed on an already-terminated call is idempotent → 200', async () => {
    const mgr = new StubCallManager();
    mgr.addTerminatedCall(makeCall({ status: 'completed', endTime: new Date(), duration: 5 }));
    h = await startServer(mgr);
    const resp = await post('Status=completed');
    expect(resp.status).toBe(200);
    // terminateByCallSid is NOT called when the call is already inactive.
    expect(mgr.terminateLog).toEqual([]);
  });

  it('Twiml against a terminated call → 21220', async () => {
    const mgr = new StubCallManager();
    mgr.addTerminatedCall(makeCall({ status: 'completed', endTime: new Date(), duration: 5 }));
    h = await startServer(mgr);
    const resp = await post('Twiml=' + encodeURIComponent('<Response><Hangup/></Response>'));
    expect(resp.status).toBe(400);
    expect(((await resp.json()) as Record<string, unknown>).code).toBe(21220);
  });

  it('Twiml=<Response><Hangup/></Response> on an active call dispatches and terminates', async () => {
    const mgr = new StubCallManager();
    mgr.addActiveCall(makeCall());
    h = await startServer(mgr);
    const resp = await post('Twiml=' + encodeURIComponent('<Response><Hangup/></Response>'));
    expect(resp.status).toBe(200);
    // sync dispatch ran the <Hangup> handler → terminate("hangup" or similar).
    expect(mgr.terminateLog.length).toBe(1);
    expect(mgr.terminateLog[0].callSid).toBe(CALL_SID);
    const body = (await resp.json()) as Record<string, unknown>;
    expect(body.status).toBe('completed');
  });

  it('malformed Twiml → 12100', async () => {
    const mgr = new StubCallManager();
    mgr.addActiveCall(makeCall());
    h = await startServer(mgr);
    const resp = await post('Twiml=' + encodeURIComponent('<NotResponse/>'));
    expect(resp.status).toBe(400);
    expect(((await resp.json()) as Record<string, unknown>).code).toBe(12100);
  });

  it('StatusCallback-only is valid → 200 + current JSON', async () => {
    const mgr = new StubCallManager();
    mgr.addActiveCall(makeCall());
    h = await startServer(mgr);
    const resp = await post('StatusCallback=' + encodeURIComponent('https://example.com/cb'));
    expect(resp.status).toBe(200);
    expect(mgr.terminateLog).toEqual([]);
    const body = (await resp.json()) as Record<string, unknown>;
    expect(body.sid).toBe(CALL_SID);
  });

  it('Url= fetches TwiML and dispatches; fetch failure → 11200', async () => {
    const mgr = new StubCallManager();
    mgr.addActiveCall(makeCall());
    const fetcher = makeFetcher(async () => {
      throw new Error('connect refused');
    });
    h = await startServer(mgr, fetcher);
    const resp = await post('Url=' + encodeURIComponent('https://example.com/twiml'));
    expect(resp.status).toBe(400);
    expect(((await resp.json()) as Record<string, unknown>).code).toBe(11200);
  });

  it('Url= success path dispatches the fetched TwiML (<Hangup> terminates)', async () => {
    const mgr = new StubCallManager();
    mgr.addActiveCall(makeCall());
    const fetcher = makeFetcher(async () => '<Response><Hangup/></Response>');
    h = await startServer(mgr, fetcher);
    const resp = await post('Url=' + encodeURIComponent('https://example.com/twiml'));
    expect(resp.status).toBe(200);
    expect(mgr.terminateLog.length).toBe(1);
  });
});
