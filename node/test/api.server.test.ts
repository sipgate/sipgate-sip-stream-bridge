/**
 * Tests for createApiHandler (server.ts): the composed middleware chain and the
 * route fall-through contract. Exercised over a real http.Server on an ephemeral
 * 127.0.0.1 port.
 *
 * Middleware order is load-bearing and pinned here:
 *   securityHeaders → maxBytes (413) → basicAuth (401) → handler
 * Security headers must land on the 401 path; a 413 oversize body must be
 * refused BEFORE auth.
 */

import http from 'node:http';
import type { AddressInfo } from 'node:net';

import pino, { type Logger } from 'pino';
import { afterEach, describe, expect, it } from 'vitest';

import type { BridgeCall, CallManager, CallSession } from '../src/bridge/callManager.js';
import { createApiHandler } from '../src/api/server.js';
import { Metrics } from '../src/observability/metrics.js';
import type { Config } from '../src/config/index.js';

const ACCOUNT_SID = 'AC' + '0123456789abcdef0123456789abcdef';
const AUTH_TOKEN = 'test-auth-token-0123456789abcdef';
const CALL_SID = 'CA0123456789abcdef0123456789abcdef';

function silentLogger(): Logger {
  return pino({ level: 'silent' });
}

function authHeader(user = ACCOUNT_SID, pass = AUTH_TOKEN): string {
  return 'Basic ' + Buffer.from(`${user}:${pass}`).toString('base64');
}

class StubCallManager {
  readonly accountSid = ACCOUNT_SID;
  private readonly calls = new Map<string, BridgeCall>();
  private readonly active = new Set<string>();
  private readonly sessions = new Map<string, CallSession>();

  addActiveCall(call: BridgeCall): void {
    this.calls.set(call.callSid, call);
    this.active.add(call.callSid);
    this.sessions.set(call.callSid, { callSid: call.callSid, log: silentLogger() } as unknown as CallSession);
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
  terminateByCallSid(callSid: string): boolean {
    return this.active.delete(callSid);
  }
  closeStream(): void {
    /* unused */
  }
}

function makeCall(): BridgeCall {
  return {
    callSid: CALL_SID,
    accountSid: ACCOUNT_SID,
    from: 'sip:a@x',
    to: 'sip:b@x',
    status: 'in-progress',
    direction: 'inbound',
    startTime: new Date('2026-04-27T10:00:00Z'),
    endTime: null,
    duration: null,
    answeredBy: null,
    parentCallSid: null,
  };
}

const baseConfig = { AUTH_TOKEN } as unknown as Config;

interface Harness {
  url: string;
  manager: StubCallManager;
  metrics: Metrics;
  fellThrough: () => boolean;
  close: () => Promise<void>;
}

async function startServer(manager: StubCallManager): Promise<Harness> {
  const metrics = new Metrics();
  let fellThrough = false;
  const handler = createApiHandler({
    manager: manager as unknown as CallManager,
    config: baseConfig,
    log: silentLogger(),
    metrics,
  });
  const server = http.createServer((req, res) => {
    if (handler(req, res)) {
      return;
    }
    fellThrough = true;
    res.writeHead(404, { 'Content-Length': '0' });
    res.end();
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const { port } = server.address() as AddressInfo;
  return {
    url: `http://127.0.0.1:${port}`,
    manager,
    metrics,
    fellThrough: () => fellThrough,
    close: () => new Promise<void>((resolve) => server.close(() => resolve())),
  };
}

function callPath(callSid = CALL_SID): string {
  return `/2010-04-01/Accounts/${ACCOUNT_SID}/Calls/${callSid}.json`;
}

describe('createApiHandler — auth', () => {
  let h: Harness;
  afterEach(async () => {
    await h.close();
  });

  it('returns 401 + WWW-Authenticate when no credentials are provided', async () => {
    h = await startServer(new StubCallManager());
    const resp = await fetch(`${h.url}${callPath()}`);
    expect(resp.status).toBe(401);
    expect(resp.headers.get('www-authenticate')).toBe('Basic realm="Twilio API"');
    const body = (await resp.json()) as Record<string, unknown>;
    expect(body.code).toBe(20003);
  });

  it('returns 401 on wrong credentials', async () => {
    h = await startServer(new StubCallManager());
    const resp = await fetch(`${h.url}${callPath()}`, {
      headers: { authorization: authHeader(ACCOUNT_SID, 'wrong') },
    });
    expect(resp.status).toBe(401);
  });

  it('emits the locked security headers on the 401 path (headers before auth)', async () => {
    h = await startServer(new StubCallManager());
    const resp = await fetch(`${h.url}${callPath()}`);
    expect(resp.status).toBe(401);
    expect(resp.headers.get('strict-transport-security')).toBe('max-age=63072000; includeSubDomains');
    expect(resp.headers.get('content-security-policy')).toBe("default-src 'none'; frame-ancestors 'none'");
    expect(resp.headers.get('x-content-type-options')).toBe('nosniff');
  });

  it('accepts valid credentials and serves the resource', async () => {
    const mgr = new StubCallManager();
    mgr.addActiveCall(makeCall());
    h = await startServer(mgr);
    const resp = await fetch(`${h.url}${callPath()}`, { headers: { authorization: authHeader() } });
    expect(resp.status).toBe(200);
  });
});

describe('createApiHandler — body size (413 before auth)', () => {
  let h: Harness;
  afterEach(async () => {
    await h.close();
  });

  it('refuses an oversize Content-Length with 413 BEFORE auth runs', async () => {
    h = await startServer(new StubCallManager());
    // No Authorization header — if auth ran first this would be 401; the 413
    // proves maxBytes fires before basicAuth. The body is genuinely larger than
    // the 64KB cap so the real Content-Length trips the Tier-1 pre-check.
    const oversize = 'x'.repeat(64 * 1024 + 1);
    const resp = await fetch(`${h.url}${callPath()}`, {
      method: 'POST',
      headers: { 'content-type': 'application/x-www-form-urlencoded' },
      body: oversize,
    });
    expect(resp.status).toBe(413);
    const body = (await resp.json()) as Record<string, unknown>;
    expect(body.code).toBe(21617);
    // Security headers present on the 413 path too.
    expect(resp.headers.get('x-content-type-options')).toBe('nosniff');
  });
});

describe('createApiHandler — route fall-through', () => {
  let h: Harness;
  afterEach(async () => {
    await h.close();
  });

  it('returns false (caller 404s) for a non-API path', async () => {
    h = await startServer(new StubCallManager());
    const resp = await fetch(`${h.url}/health`);
    expect(resp.status).toBe(404); // our test server 404s on fall-through
    expect(h.fellThrough()).toBe(true);
  });

  it('handles an API path (does not fall through)', async () => {
    const mgr = new StubCallManager();
    mgr.addActiveCall(makeCall());
    h = await startServer(mgr);
    const resp = await fetch(`${h.url}${callPath()}`, { headers: { authorization: authHeader() } });
    expect(resp.status).toBe(200);
    expect(h.fellThrough()).toBe(false);
  });

  it('records api_requests_total + duration metrics on a handled request', async () => {
    const mgr = new StubCallManager();
    mgr.addActiveCall(makeCall());
    h = await startServer(mgr);
    await fetch(`${h.url}${callPath()}`, { headers: { authorization: authHeader() } });
    const prom = h.metrics.getPrometheus();
    expect(prom).toContain('api_requests_total{route="get_call",method="GET",status="2xx"} 1');
    expect(prom).toContain('api_request_duration_seconds_count{route="get_call"} 1');
  });
});
