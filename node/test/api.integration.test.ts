/**
 * Integration test: the REAL CallManager wired into the production createApiHandler
 * over a real http.Server. This exercises the actual production seam (route match →
 * security/auth middleware → manager query), not a stub — so it would catch a
 * missing or mis-wired control-plane mount, derived AccountSid, or auth check.
 */
import http from 'node:http';
import type { AddressInfo } from 'node:net';

import { afterAll, beforeAll, describe, expect, it } from 'vitest';

import { CallManager } from '../src/bridge/callManager.js';
import type { Config } from '../src/config/index.js';
import { createApiHandler } from '../src/api/server.js';
import { deriveAccountSid } from '../src/identity/sid.js';
import { createChildLogger } from '../src/logger/index.js';

// Minimal Config — only the fields the manager + API handler read on these paths.
const config = {
  SIP_USER: 'e12345p0',
  AUTH_TOKEN: 'test-token',
  SIP_LISTEN_ADDR: '0.0.0.0:5060',
} as unknown as Config;

const accountSid = deriveAccountSid(config.SIP_USER);
const authHeader = 'Basic ' + Buffer.from(`${accountSid}:${config.AUTH_TOKEN}`).toString('base64');

let manager: CallManager;
let server: http.Server;
let base: string;

beforeAll(async () => {
  manager = new CallManager(config, createChildLogger({ component: 'test' }));
  const handler = createApiHandler({ manager, config, log: createChildLogger({ component: 'api' }) });
  server = http.createServer((req, res) => {
    if (handler(req, res)) return;
    res.writeHead(404);
    res.end();
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const { port } = server.address() as AddressInfo;
  base = `http://127.0.0.1:${port}`;
});

afterAll(async () => {
  await manager.terminateAll();
  await new Promise<void>((resolve) => server.close(() => resolve()));
});

describe('REST control plane wired into the real CallManager', () => {
  it('lists an empty call set with a well-formed page envelope', async () => {
    const res = await fetch(`${base}/2010-04-01/Accounts/${accountSid}/Calls.json`, {
      headers: { authorization: authHeader },
    });
    expect(res.status).toBe(200);
    const body = (await res.json()) as { calls: unknown[]; page: number; page_size: number };
    expect(body.calls).toEqual([]);
    expect(body.page).toBe(0);
    expect(body.page_size).toBe(50);
  });

  it('rejects missing credentials with 401 + WWW-Authenticate and security headers', async () => {
    const res = await fetch(`${base}/2010-04-01/Accounts/${accountSid}/Calls.json`);
    expect(res.status).toBe(401);
    expect(res.headers.get('www-authenticate')).toContain('Basic');
    expect(res.headers.get('x-content-type-options')).toBe('nosniff');
  });

  it('rejects a wrong AccountSid in the URL with 401 (not 404)', async () => {
    const res = await fetch(`${base}/2010-04-01/Accounts/ACdeadbeefdeadbeefdeadbeefdeadbeef/Calls.json`, {
      headers: { authorization: authHeader },
    });
    expect(res.status).toBe(401);
  });

  it('returns 404+20404 for a malformed CallSid', async () => {
    const res = await fetch(`${base}/2010-04-01/Accounts/${accountSid}/Calls/not-a-sid.json`, {
      headers: { authorization: authHeader },
    });
    expect(res.status).toBe(404);
    const body = (await res.json()) as { code: number };
    expect(body.code).toBe(20404);
  });

  it('returns 404 for an unknown but well-formed CallSid', async () => {
    const res = await fetch(
      `${base}/2010-04-01/Accounts/${accountSid}/Calls/CA00000000000000000000000000000000.json`,
      { headers: { authorization: authHeader } },
    );
    expect(res.status).toBe(404);
  });
});
