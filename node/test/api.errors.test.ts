import { describe, expect, it } from 'vitest';
import { PassThrough } from 'node:stream';
import { ServerResponse } from 'node:http';
import { IncomingMessage } from 'node:http';
import { Socket } from 'node:net';
import {
  ApiError,
  ErrAuthRequired,
  ErrCallNotInProgress,
  ErrHttpRetrievalFailure,
  ErrInvalidParams,
  ErrNotFound,
  ErrPayloadTooLarge,
  ErrTooManyRequests,
  ErrTwimlParseFailure,
} from '../src/api/errors.js';

describe('ApiError factory constructors', () => {
  const cases: Array<{
    name: string;
    err: ApiError;
    code: number;
    status: number;
    message: string;
  }> = [
    {
      name: 'ErrAuthRequired',
      err: ErrAuthRequired(),
      code: 20003,
      status: 401,
      message: 'Authentication Error - No credentials provided',
    },
    {
      name: 'ErrNotFound',
      err: ErrNotFound('/Calls/CAxxxx.json'),
      code: 20404,
      status: 404,
      message: 'The requested resource /Calls/CAxxxx.json was not found',
    },
    {
      name: 'ErrInvalidParams',
      err: ErrInvalidParams('Url'),
      code: 21218,
      status: 400,
      message: 'Invalid parameters: Url',
    },
    {
      name: 'ErrCallNotInProgress',
      err: ErrCallNotInProgress(),
      code: 21220,
      status: 400,
      message: 'Invalid call state for the requested operation',
    },
    {
      name: 'ErrTwimlParseFailure',
      err: ErrTwimlParseFailure(),
      code: 12100,
      status: 400,
      message: 'Document parse failure',
    },
    {
      name: 'ErrHttpRetrievalFailure',
      err: ErrHttpRetrievalFailure('dns lookup failed'),
      code: 11200,
      status: 400,
      message: 'HTTP retrieval failure: dns lookup failed',
    },
    {
      name: 'ErrPayloadTooLarge',
      err: ErrPayloadTooLarge(),
      code: 21617,
      status: 413,
      message: 'Request body exceeds 64KB limit',
    },
    {
      name: 'ErrTooManyRequests',
      err: ErrTooManyRequests(),
      code: 20429,
      status: 429,
      message: 'Too Many Requests',
    },
  ];

  for (const tc of cases) {
    it(`${tc.name}: code/status/message/more_info`, () => {
      expect(tc.err.code).toBe(tc.code);
      expect(tc.err.status).toBe(tc.status);
      expect(tc.err.message).toBe(tc.message);
      expect(tc.err.more_info).toBe(`https://www.twilio.com/docs/errors/${tc.code}`);
    });
  }

  it('all codes are unique', () => {
    const seen = new Set<number>();
    for (const tc of cases) {
      expect(seen.has(tc.err.code)).toBe(false);
      seen.add(tc.err.code);
    }
  });

  it('more_info always matches Twilio canonical pattern', () => {
    const re = /^https:\/\/www\.twilio\.com\/docs\/errors\/\d+$/;
    for (const tc of cases) {
      expect(tc.err.more_info).toMatch(re);
    }
  });

  it('toJSON emits exactly the four snake_case wire fields', () => {
    const e = ErrAuthRequired();
    expect(JSON.parse(JSON.stringify(e))).toEqual({
      code: 20003,
      message: 'Authentication Error - No credentials provided',
      more_info: 'https://www.twilio.com/docs/errors/20003',
      status: 401,
    });
  });
});

/**
 * Drive writeJSON against a real node:http ServerResponse backed by a
 * PassThrough socket so we capture the raw bytes on the wire. This proves
 * Content-Type, status line, and the JSON body all match Twilio's shape.
 */
function captureWriteJSON(err: ApiError): Promise<{ raw: string }> {
  return new Promise((resolve) => {
    const socket = new PassThrough();
    const chunks: Buffer[] = [];
    socket.on('data', (c: Buffer) => chunks.push(c));

    const req = new IncomingMessage(socket as unknown as Socket);
    const res = new ServerResponse(req);
    res.assignSocket(socket as unknown as Socket);
    res.on('finish', () => {
      // Allow the socket flush to complete on the next tick.
      setImmediate(() => resolve({ raw: Buffer.concat(chunks).toString('utf8') }));
    });

    err.writeJSON(res);
  });
}

describe('ApiError.writeJSON', () => {
  it('writes Content-Type, status, and Twilio-shape JSON body', async () => {
    const { raw } = await captureWriteJSON(ErrAuthRequired());

    expect(raw).toMatch(/^HTTP\/1\.1 401\b/);
    expect(raw.toLowerCase()).toContain('content-type: application/json');

    const bodyStart = raw.indexOf('\r\n\r\n');
    expect(bodyStart).toBeGreaterThan(-1);
    const body = raw.slice(bodyStart + 4);
    const decoded = JSON.parse(body) as Record<string, unknown>;
    expect(decoded).toEqual({
      code: 20003,
      message: 'Authentication Error - No credentials provided',
      more_info: 'https://www.twilio.com/docs/errors/20003',
      status: 401,
    });
  });

  it('uses the configured status for a 413', async () => {
    const { raw } = await captureWriteJSON(ErrPayloadTooLarge());
    expect(raw).toMatch(/^HTTP\/1\.1 413\b/);
  });
});
