/**
 * Security middleware for the Twilio-compatible REST control plane
 * (response-header hardening + anti-DoS body bounding).
 *
 * Ported from the Go chi middleware internal/api/security.go. The Go version
 * ships two `func(http.Handler) http.Handler` factories; this project serves via
 * raw node:http (http.createServer, NOT express/chi), so each piece is modelled
 * as a plain function over (req, res):
 *
 *   - securityHeaders(res)              — side-effecting; sets the three locked
 *                                          headers on every response.
 *   - maxBytes(req, res, limit): boolean — Tier-1 Content-Length pre-check;
 *                                          returns false after writing 413.
 *   - readFormBody(req, limit)          — Tier-2 stream-bounded body reader that
 *                                          parses application/x-www-form-urlencoded.
 *
 * Compose in this fixed order so headers land on the 401 path and oversize
 * bodies are rejected before any credential work:
 *
 *   securityHeaders(res)  →  maxBytes(req, res)  →  basicAuth(req, res, …)  →  handler
 */

import type { IncomingMessage, ServerResponse } from 'node:http';

import { ErrPayloadTooLarge } from './errors.js';

/** Production REST body cap — 64KiB (64 << 10). Sized at 16x the largest
 * legitimate Twiml= body (4000 chars) plus headroom for status-callback fields. */
export const DEFAULT_MAX_BYTES = 65_536;

/** Thrown by readFormBody when the body stream exceeds the configured cap.
 * Distinguishable via instanceof so callers can emit a 413 / Twilio 21617. */
export class PayloadTooLargeError extends Error {
  readonly limit: number;
  constructor(limit: number) {
    super(`request body exceeds ${limit} byte limit`);
    this.name = 'PayloadTooLargeError';
    this.limit = limit;
  }
}

/**
 * Set the three locked security headers on EVERY response — including 401/413
 * error responses. Call before any stage that may write a response so the
 * headers are buffered ahead of writeHead.
 *
 * Locked set (values byte-identical to the Go middleware):
 *   - Strict-Transport-Security: max-age=63072000; includeSubDomains  (2-year HSTS)
 *   - Content-Security-Policy: default-src 'none'; frame-ancestors 'none'
 *   - X-Content-Type-Options: nosniff
 */
export function securityHeaders(res: ServerResponse): void {
  res.setHeader('Strict-Transport-Security', 'max-age=63072000; includeSubDomains');
  res.setHeader('Content-Security-Policy', "default-src 'none'; frame-ancestors 'none'");
  res.setHeader('X-Content-Type-Options', 'nosniff');
}

/**
 * Parse a Content-Length header into a non-negative integer, or undefined when
 * the header is absent or unparseable (chunked / unknown length). node:http may
 * surface the header as a string or — never, in practice — an array; both are
 * handled defensively.
 */
function parseContentLength(req: IncomingMessage): number | undefined {
  const raw = req.headers['content-length'];
  const value = Array.isArray(raw) ? raw[0] : raw;
  if (value === undefined) {
    return undefined;
  }
  const n = Number(value);
  if (!Number.isInteger(n) || n < 0) {
    return undefined;
  }
  return n;
}

/**
 * Tier-1 Content-Length pre-check.
 *
 * If the declared Content-Length STRICTLY exceeds `limit`, write a Twilio-shaped
 * 413 immediately (before reading any body bytes) and return false. Otherwise
 * return true. A request with Content-Length == limit is allowed through —
 * readFormBody's Tier-2 cap is inclusive (allows exactly `limit` bytes), so the
 * boundary stays consistent across the two tiers.
 *
 * Tier 2 (the stream cap) lives in readFormBody so chunked / fraudulent-CL /
 * unknown-length bodies are bounded when actually consumed.
 */
export function maxBytes(req: IncomingMessage, res: ServerResponse, limit: number = DEFAULT_MAX_BYTES): boolean {
  const contentLength = parseContentLength(req);
  if (contentLength !== undefined && contentLength > limit) {
    ErrPayloadTooLarge().writeJSON(res);
    return false;
  }
  return true;
}

/**
 * Read the full request body, enforcing a hard byte cap on the actual stream
 * (Tier 2). Rejects with PayloadTooLargeError as soon as consumption STRICTLY
 * exceeds `limit` — i.e. exactly `limit` bytes are allowed; the (limit+1)th
 * byte trips the cap. This catches chunked-encoded oversize bodies that lie
 * about Content-Length or omit it, which Tier-1 (maxBytes) cannot see.
 */
export function readBody(req: IncomingMessage, limit: number = DEFAULT_MAX_BYTES): Promise<Buffer> {
  return new Promise<Buffer>((resolve, reject) => {
    const chunks: Buffer[] = [];
    let total = 0;
    let settled = false;

    const cleanup = (): void => {
      req.removeListener('data', onData);
      req.removeListener('end', onEnd);
      req.removeListener('error', onError);
      req.removeListener('aborted', onAborted);
    };
    const fail = (err: Error): void => {
      if (settled) {
        return;
      }
      settled = true;
      cleanup();
      // Stop pulling further bytes from an oversize / failed stream.
      req.destroy();
      reject(err);
    };
    const succeed = (buf: Buffer): void => {
      if (settled) {
        return;
      }
      settled = true;
      cleanup();
      resolve(buf);
    };

    const onData = (chunk: Buffer): void => {
      total += chunk.length;
      if (total > limit) {
        fail(new PayloadTooLargeError(limit));
        return;
      }
      chunks.push(chunk);
    };
    const onEnd = (): void => {
      succeed(Buffer.concat(chunks));
    };
    const onError = (err: Error): void => {
      fail(err);
    };
    const onAborted = (): void => {
      fail(new Error('request aborted'));
    };

    req.on('data', onData);
    req.on('end', onEnd);
    req.on('error', onError);
    req.on('aborted', onAborted);
  });
}

/**
 * Read and parse an application/x-www-form-urlencoded request body, enforcing
 * the Tier-2 byte cap. Used by handlers such as modifyCallHandler.
 *
 * - Oversize → rejects with PayloadTooLargeError (caller emits 413 / 21617).
 * - Stream errors → reject verbatim.
 * - Parsing is delegated to URLSearchParams, which never throws on malformed
 *   input; bare/garbage tokens simply produce empty-value or dropped pairs.
 *   Callers validate required fields downstream and surface 400 invalid_params.
 */
export async function readFormBody(req: IncomingMessage, limit: number = DEFAULT_MAX_BYTES): Promise<URLSearchParams> {
  const buf = await readBody(req, limit);
  return new URLSearchParams(buf.toString('utf8'));
}
