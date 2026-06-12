/**
 * HTTP Basic Auth for the Twilio-compatible REST control plane.
 *
 * Ported from the Go chi middleware internal/api/auth.go. The Go version is a
 * `func(http.Handler) http.Handler` factory; this project serves via raw
 * node:http (http.createServer, NOT express/chi), so the middleware is modelled
 * as a plain predicate over (req, res):
 *
 *   basicAuth(req, res, expectedAccountSid, authToken, urlAccountSid): boolean
 *
 * Returns `true` when the request is authenticated and the caller should
 * proceed to the next stage. Returns `false` when it has ALREADY written a 401
 * response (WWW-Authenticate header + Twilio JSON body) — the caller must stop.
 *
 * Both the username/AccountSid and password/authToken comparisons use
 * crypto.timingSafeEqual to defeat timing oracles. The {AccountSid} URL-path
 * param is ALSO compared constant-time against the configured AccountSid;
 * mismatch returns 401 (NOT 404) — a 404 would leak which AccountSid values
 * exist, enabling account enumeration.
 *
 * Compose with the security middleware in this fixed order so headers land on
 * the 401 path and oversize bodies are rejected before any credential work:
 *
 *   securityHeaders(res)  →  maxBytes(req, res)  →  basicAuth(req, res, …)  →  handler
 */

import { timingSafeEqual } from 'node:crypto';
import type { IncomingMessage, ServerResponse } from 'node:http';

import { ErrAuthRequired } from './errors.js';

/** WWW-Authenticate challenge value — byte-identical to the Go middleware. */
const WWW_AUTHENTICATE = 'Basic realm="Twilio API"';

/**
 * Constant-time string equality.
 *
 * timingSafeEqual throws if the two buffers differ in length, which would itself
 * leak length information through the throw/no-throw branch. We guard the length
 * difference by always comparing two equal-length buffers (comparing `a` against
 * itself when the lengths differ) so the function takes a comparison path
 * regardless, then folds in the length check. The result is `false` whenever the
 * lengths differ, with no early return that depends on the secret contents.
 */
function constantTimeEqual(a: string, b: string): boolean {
  const ab = Buffer.from(a, 'utf8');
  const bb = Buffer.from(b, 'utf8');
  const sameLength = ab.length === bb.length;
  // Always run a comparison over equal-length buffers so timingSafeEqual never
  // throws. When lengths differ we compare `ab` to itself (always equal) but
  // AND in `sameLength`, so the overall result is correctly false.
  const cmp = timingSafeEqual(ab, sameLength ? bb : ab);
  return cmp && sameLength;
}

/**
 * Parse an `Authorization: Basic base64(user:pass)` header.
 *
 * Returns the decoded user/pass, or null when the header is absent, not the
 * Basic scheme, or malformed. The split is on the FIRST colon only — passwords
 * may legitimately contain colons (RFC 7617), usernames may not.
 */
function parseBasicAuth(header: string | undefined): { user: string; pass: string } | null {
  if (header === undefined) {
    return null;
  }
  // Scheme token is case-insensitive per RFC 7617; require exactly one space.
  const space = header.indexOf(' ');
  if (space === -1) {
    return null;
  }
  const scheme = header.slice(0, space);
  if (scheme.toLowerCase() !== 'basic') {
    return null;
  }
  const encoded = header.slice(space + 1).trim();
  if (encoded.length === 0) {
    return null;
  }
  const decoded = Buffer.from(encoded, 'base64').toString('utf8');
  const colon = decoded.indexOf(':');
  if (colon === -1) {
    return null;
  }
  return { user: decoded.slice(0, colon), pass: decoded.slice(colon + 1) };
}

/**
 * Enforce HTTP Basic Auth and {AccountSid} URL-path validation.
 *
 * @param req                node:http request (read: headers.authorization)
 * @param res                node:http response (written only on failure)
 * @param expectedAccountSid configured AccountSid — the expected Basic username
 * @param authToken          configured AuthToken — the expected Basic password
 * @param urlAccountSid      the {AccountSid} extracted from the request path, or
 *                           undefined for routes that carry no AccountSid in the
 *                           URL (e.g. /health). Undefined skips the path check —
 *                           a safety net, not the contract.
 * @returns `true` to proceed; `false` after a 401 has been written.
 */
export function basicAuth(
  req: IncomingMessage,
  res: ServerResponse,
  expectedAccountSid: string,
  authToken: string,
  urlAccountSid?: string,
): boolean {
  const creds = parseBasicAuth(req.headers.authorization);
  if (creds === null) {
    unauth(res);
    return false;
  }

  // Compare both fields BEFORE branching on the combined result so the total
  // wall-clock time is independent of which field (if any) is wrong. Bitwise
  // AND of the two booleans keeps the decision branch-free.
  const userOK = constantTimeEqual(creds.user, expectedAccountSid);
  const passOK = constantTimeEqual(creds.pass, authToken);
  if (!(userOK && passOK)) {
    unauth(res);
    return false;
  }

  // URL-path AccountSid validation. undefined / empty means the route carries
  // no {AccountSid} param — skip (matches the Go chi.URLParam == "" branch).
  if (urlAccountSid !== undefined && urlAccountSid !== '') {
    if (!constantTimeEqual(urlAccountSid, expectedAccountSid)) {
      unauth(res);
      return false;
    }
  }

  return true;
}

/**
 * Write the 401 response: WWW-Authenticate header THEN the Twilio JSON body via
 * ErrAuthRequired().writeJSON (which sets Content-Type and status). The header
 * must be set before writeHead is called inside writeJSON.
 */
function unauth(res: ServerResponse): void {
  res.setHeader('WWW-Authenticate', WWW_AUTHENTICATE);
  ErrAuthRequired().writeJSON(res);
}
