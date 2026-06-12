import type { IncomingHttpHeaders, ServerResponse } from 'node:http';
import { describe, expect, it } from 'vitest';

import { basicAuth } from '../src/api/auth.js';
import { securityHeaders } from '../src/api/security.js';

const TEST_SID = 'ACdeadbeefdeadbeefdeadbeefdeadbeef';
const TEST_TOKEN = 'supersecret-auth-token';

/** Minimal node:http response fake capturing headers, status, and body. */
interface FakeRes {
  res: ServerResponse;
  headers: Record<string, string>;
  statusCode: number | undefined;
  body: string;
}

function fakeRes(): FakeRes {
  const state: FakeRes = {
    res: undefined as unknown as ServerResponse,
    headers: {},
    statusCode: undefined,
    body: '',
  };
  state.res = {
    setHeader(name: string, value: string): void {
      state.headers[name.toLowerCase()] = value;
    },
    getHeader(name: string): string | undefined {
      return state.headers[name.toLowerCase()];
    },
    writeHead(status: number): ServerResponse {
      state.statusCode = status;
      return state.res;
    },
    end(chunk?: string): void {
      if (chunk !== undefined) state.body += chunk;
    },
  } as unknown as ServerResponse;
  return state;
}

/** Build the request headers carrying an Authorization: Basic header. */
function authHeaders(user: string, pass: string): IncomingHttpHeaders {
  const encoded = Buffer.from(`${user}:${pass}`, 'utf8').toString('base64');
  return { authorization: `Basic ${encoded}` };
}

function fakeReq(headers: IncomingHttpHeaders = {}): { headers: IncomingHttpHeaders } {
  return { headers };
}

describe('basicAuth', () => {
  it('passes with valid credentials and matching URL AccountSid', () => {
    const r = fakeRes();
    const ok = basicAuth(
      fakeReq(authHeaders(TEST_SID, TEST_TOKEN)) as never,
      r.res,
      TEST_SID,
      TEST_TOKEN,
      TEST_SID,
    );
    expect(ok).toBe(true);
    expect(r.statusCode).toBeUndefined();
    expect(r.body).toBe('');
  });

  it('passes with valid credentials and no URL AccountSid (route without param)', () => {
    const r = fakeRes();
    const ok = basicAuth(fakeReq(authHeaders(TEST_SID, TEST_TOKEN)) as never, r.res, TEST_SID, TEST_TOKEN);
    expect(ok).toBe(true);
  });

  it('rejects a wrong username with 401', () => {
    const r = fakeRes();
    const ok = basicAuth(
      fakeReq(authHeaders('wrong-user', TEST_TOKEN)) as never,
      r.res,
      TEST_SID,
      TEST_TOKEN,
      TEST_SID,
    );
    expect(ok).toBe(false);
    expect(r.statusCode).toBe(401);
  });

  it('rejects a wrong password with 401', () => {
    const r = fakeRes();
    const ok = basicAuth(
      fakeReq(authHeaders(TEST_SID, 'wrong-token')) as never,
      r.res,
      TEST_SID,
      TEST_TOKEN,
      TEST_SID,
    );
    expect(ok).toBe(false);
    expect(r.statusCode).toBe(401);
  });

  it('rejects URL AccountSid mismatch with 401 (NOT 404 — anti-enumeration)', () => {
    const r = fakeRes();
    const otherSid = 'ACcafebabecafebabecafebabecafebabe';
    const ok = basicAuth(
      fakeReq(authHeaders(TEST_SID, TEST_TOKEN)) as never,
      r.res,
      TEST_SID,
      TEST_TOKEN,
      otherSid,
    );
    expect(ok).toBe(false);
    expect(r.statusCode).toBe(401);
    expect(r.statusCode).not.toBe(404);
  });

  it('rejects missing credentials with 401 + WWW-Authenticate + Twilio 20003 body', () => {
    const r = fakeRes();
    const ok = basicAuth(fakeReq() as never, r.res, TEST_SID, TEST_TOKEN, TEST_SID);
    expect(ok).toBe(false);
    expect(r.statusCode).toBe(401);
    expect(r.headers['www-authenticate']).toBe('Basic realm="Twilio API"');
    const body = JSON.parse(r.body) as { code: number; status: number };
    expect(body.code).toBe(20003);
    expect(body.status).toBe(401);
  });

  it('rejects a non-Basic scheme with 401', () => {
    const r = fakeRes();
    const ok = basicAuth(
      fakeReq({ authorization: 'Bearer sometoken' }) as never,
      r.res,
      TEST_SID,
      TEST_TOKEN,
      TEST_SID,
    );
    expect(ok).toBe(false);
    expect(r.statusCode).toBe(401);
  });

  it('does not throw on length-mismatched credentials (constant-time guard)', () => {
    const r = fakeRes();
    // Password far longer than the secret — timingSafeEqual would throw if the
    // length guard were missing.
    const longPass = 'a'.repeat(256);
    expect(() => basicAuth(fakeReq(authHeaders(TEST_SID, longPass)) as never, r.res, TEST_SID, TEST_TOKEN, TEST_SID)).not.toThrow();
    expect(r.statusCode).toBe(401);
  });

  it('ORDERING: security headers present on a 401 response', () => {
    // Pin the compose order securityHeaders -> basicAuth: headers must be set
    // before basicAuth writes its 401 so they survive on the failure path.
    const r = fakeRes();
    securityHeaders(r.res);
    const ok = basicAuth(fakeReq() as never, r.res, TEST_SID, TEST_TOKEN, TEST_SID);
    expect(ok).toBe(false);
    expect(r.statusCode).toBe(401);
    expect(r.headers['strict-transport-security']).toBe('max-age=63072000; includeSubDomains');
    expect(r.headers['content-security-policy']).toBe("default-src 'none'; frame-ancestors 'none'");
    expect(r.headers['x-content-type-options']).toBe('nosniff');
  });
});
