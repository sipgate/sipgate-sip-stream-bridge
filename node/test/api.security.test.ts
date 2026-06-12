import type { IncomingMessage, ServerResponse } from 'node:http';
import { Readable } from 'node:stream';
import { describe, expect, it } from 'vitest';

import {
  DEFAULT_MAX_BYTES,
  PayloadTooLargeError,
  maxBytes,
  readBody,
  readFormBody,
  securityHeaders,
} from '../src/api/security.js';

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

/** A request fake whose Content-Length header is whatever we declare. */
function reqWithContentLength(length: number): IncomingMessage {
  return { headers: { 'content-length': String(length) } } as unknown as IncomingMessage;
}

/** A readable-stream-backed request fake carrying `payload` as its body. */
function reqWithBody(payload: Buffer | string): IncomingMessage {
  const buf = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const stream = Readable.from([buf]) as unknown as IncomingMessage;
  stream.headers = { 'content-length': String(buf.length) };
  return stream;
}

describe('securityHeaders', () => {
  it('sets all three locked headers byte-exactly', () => {
    const r = fakeRes();
    securityHeaders(r.res);
    expect(r.headers['strict-transport-security']).toBe('max-age=63072000; includeSubDomains');
    expect(r.headers['content-security-policy']).toBe("default-src 'none'; frame-ancestors 'none'");
    expect(r.headers['x-content-type-options']).toBe('nosniff');
  });
});

describe('maxBytes (Tier 1 Content-Length pre-check)', () => {
  it('rejects Content-Length > limit with 413 + Twilio 21617 body containing "64KB"', () => {
    const r = fakeRes();
    const ok = maxBytes(reqWithContentLength(100_000), r.res, DEFAULT_MAX_BYTES);
    expect(ok).toBe(false);
    expect(r.statusCode).toBe(413);
    expect(r.headers['content-type']).toBe('application/json');
    const body = JSON.parse(r.body) as { code: number; status: number; message: string };
    expect(body.code).toBe(21617);
    expect(body.status).toBe(413);
    expect(body.message).toContain('64KB');
  });

  it('allows Content-Length == limit (strict > boundary)', () => {
    const r = fakeRes();
    const ok = maxBytes(reqWithContentLength(DEFAULT_MAX_BYTES), r.res, DEFAULT_MAX_BYTES);
    expect(ok).toBe(true);
    expect(r.statusCode).toBeUndefined();
  });

  it('allows requests with no Content-Length (unknown / chunked)', () => {
    const r = fakeRes();
    const req = { headers: {} } as unknown as IncomingMessage;
    expect(maxBytes(req, r.res, DEFAULT_MAX_BYTES)).toBe(true);
  });
});

describe('readBody (Tier 2 stream cap)', () => {
  it('reads a body at exactly the limit', async () => {
    const payload = Buffer.alloc(16, 0x61);
    const buf = await readBody(reqWithBody(payload), 16);
    expect(buf.length).toBe(16);
  });

  it('rejects an oversize stream with PayloadTooLargeError', async () => {
    const payload = Buffer.alloc(100, 0x61);
    await expect(readBody(reqWithBody(payload), 16)).rejects.toBeInstanceOf(PayloadTooLargeError);
  });
});

describe('readFormBody', () => {
  it('parses application/x-www-form-urlencoded into URLSearchParams', async () => {
    const params = await readFormBody(reqWithBody('Twiml=%3CResponse%2F%3E&Status=completed'), DEFAULT_MAX_BYTES);
    expect(params.get('Twiml')).toBe('<Response/>');
    expect(params.get('Status')).toBe('completed');
  });

  it('caps oversize form bodies with PayloadTooLargeError', async () => {
    const big = 'X=' + 'a'.repeat(1000);
    await expect(readFormBody(reqWithBody(big), 64)).rejects.toBeInstanceOf(PayloadTooLargeError);
  });

  it('does not throw on malformed input (URLSearchParams tolerant)', async () => {
    const params = await readFormBody(reqWithBody('garbage&&=&key'), DEFAULT_MAX_BYTES);
    expect(params.get('key')).toBe('');
  });
});
