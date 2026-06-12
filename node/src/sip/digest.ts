import crypto from 'node:crypto';

/**
 * SIP Digest authentication (RFC 2617 / RFC 7616, MD5) for outbound requests.
 *
 * The REGISTER path in {@link ../sip/userAgent.ts} only handles the qop-LESS
 * MD5 case. Outbound INVITE against modern SIP trunks (sipgate's SBC included)
 * is challenged with `qop="auth"`, which requires a client nonce (cnonce), a
 * nonce-count (nc) and an extended response hash. This module implements both
 * the qop and qop-less paths as pure functions so they are unit-testable; the
 * only nondeterminism (the cnonce) is injectable for deterministic tests.
 */

// ── parsing ────────────────────────────────────────────────────────────────

/**
 * Parse a `WWW-Authenticate` / `Proxy-Authenticate` Digest challenge into its
 * fields. Strips a leading `Digest ` scheme token if present. Handles both
 * quoted (`realm="sipgate.de"`) and unquoted (`algorithm=MD5`, `qop=auth`)
 * values, comma separation, and whitespace. Keys are lower-cased so callers
 * can rely on canonical names regardless of carrier casing.
 *
 * `qop` may be a comma-separated list inside the quotes
 * (`qop="auth,auth-int"`); it is returned verbatim — selection is done in
 * {@link buildDigestAuthorization}.
 */
export function parseAuthHeader(header: string): Record<string, string> {
  const result: Record<string, string> = {};
  // Drop a leading scheme token ("Digest"), if present.
  const stripped = header.replace(/^\s*Digest\s+/i, '');
  // Match `key="quoted value"` OR `key=unquoted` (unquoted stops at comma/ws).
  const re = /(\w+)\s*=\s*(?:"([^"]*)"|([^,\s]+))/g;
  let m: RegExpExecArray | null;
  while ((m = re.exec(stripped)) !== null) {
    const key = m[1].toLowerCase();
    const value = m[2] !== undefined ? m[2] : m[3];
    result[key] = value ?? '';
  }
  return result;
}

// ── hashing helpers ──────────────────────────────────────────────────────────

function md5(s: string): string {
  return crypto.createHash('md5').update(s).digest('hex');
}

function randomCnonce(): string {
  return crypto.randomBytes(8).toString('hex');
}

// ── building ───────────────────────────────────────────────────────────────

/**
 * Inputs for {@link buildDigestAuthorization}. `realm` and `nonce` come from
 * the parsed challenge; `qop`, `opaque` and `algorithm` are optional and echoed
 * only when the challenge supplied them.
 */
export interface DigestOptions {
  username: string;
  password: string;
  /** SIP method of the request being authorized, e.g. "INVITE". */
  method: string;
  /** Request-URI (digest-uri), e.g. "sip:+4915123456789@sipgate.de". */
  uri: string;
  realm: string;
  nonce: string;
  /** Raw `qop` value from the challenge — may be a list ("auth,auth-int"). */
  qop?: string;
  /** Opaque value from the challenge, echoed verbatim if present. */
  opaque?: string;
  /** Algorithm from the challenge: "MD5" (default) or "MD5-sess". */
  algorithm?: string;
  /** Nonce-count. Defaults to "00000001". Caller increments on reuse. */
  nc?: string;
  /** Injectable client nonce — for deterministic tests. Random otherwise. */
  cnonce?: string;
}

/**
 * Compute the Digest `response` for the qop-less path (RFC 2617 §3.2.2.1).
 *   HA1 = MD5(username:realm:password)            [MD5-sess folds in nonce/cnonce]
 *   HA2 = MD5(method:uri)
 *   response = MD5(HA1:nonce:HA2)
 */
function computeResponseNoQop(ha1: string, nonce: string, ha2: string): string {
  return md5(`${ha1}:${nonce}:${ha2}`);
}

/**
 * Compute the Digest `response` for qop="auth" (RFC 2617 §3.2.2.1).
 *   response = MD5(HA1:nonce:nc:cnonce:qop:HA2)
 */
function computeResponseQop(
  ha1: string,
  nonce: string,
  nc: string,
  cnonce: string,
  qop: string,
  ha2: string,
): string {
  return md5(`${ha1}:${nonce}:${nc}:${cnonce}:${qop}:${ha2}`);
}

/**
 * Build the value of an `Authorization` / `Proxy-Authorization` header.
 *
 * Quoting follows the RFC 2617 §3.5 example for maximum SIP-trunk interop:
 *   quoted   — username, realm, nonce, uri, response, opaque, cnonce
 *   unquoted — qop, nc, algorithm
 *
 * Algorithm:
 *   - "MD5" (default): HA1 = MD5(user:realm:pass)
 *   - "MD5-sess":      HA1 = MD5(MD5(user:realm:pass):nonce:cnonce)
 *
 * qop handling: if the challenge advertised "auth" (alone or in a list) we
 * select "auth", emit cnonce/nc/qop and use the extended response hash. Any
 * other / absent qop falls back to the qop-less hash and omits those fields.
 * (auth-int is not implemented — it requires the entity body and no sipgate
 * trunk requests it; we degrade to auth or qop-less rather than send a wrong
 * hash.)
 */
export function buildDigestAuthorization(opts: DigestOptions): string {
  const algorithm = opts.algorithm ?? 'MD5';
  const isSess = algorithm.toLowerCase() === 'md5-sess';

  // Select qop=auth if offered (challenge may send a comma-separated list).
  const offered = (opts.qop ?? '')
    .split(',')
    .map((q) => q.trim().toLowerCase())
    .filter((q) => q.length > 0);
  const useQop = offered.includes('auth');

  const nc = opts.nc ?? '00000001';
  // cnonce is needed for qop=auth and for MD5-sess HA1.
  const cnonce = opts.cnonce ?? (useQop || isSess ? randomCnonce() : '');

  let ha1 = md5(`${opts.username}:${opts.realm}:${opts.password}`);
  if (isSess) {
    ha1 = md5(`${ha1}:${opts.nonce}:${cnonce}`);
  }
  const ha2 = md5(`${opts.method}:${opts.uri}`);

  const response = useQop
    ? computeResponseQop(ha1, opts.nonce, nc, cnonce, 'auth', ha2)
    : computeResponseNoQop(ha1, opts.nonce, ha2);

  // RFC 2617 §3.5 ordering / quoting.
  const parts: string[] = [
    `username="${opts.username}"`,
    `realm="${opts.realm}"`,
    `nonce="${opts.nonce}"`,
    `uri="${opts.uri}"`,
    `response="${response}"`,
  ];
  if (useQop) {
    parts.push('qop=auth');
    parts.push(`nc=${nc}`);
    parts.push(`cnonce="${cnonce}"`);
  }
  parts.push(`algorithm=${algorithm}`);
  if (opts.opaque !== undefined && opts.opaque !== '') {
    parts.push(`opaque="${opts.opaque}"`);
  }

  return `Digest ${parts.join(', ')}`;
}

/**
 * The request header name to carry the credentials for a given challenge
 * status: 401 → `Authorization`, 407 → `Proxy-Authorization`.
 */
export function authorizationHeaderName(status: number): 'Authorization' | 'Proxy-Authorization' {
  return status === 407 ? 'Proxy-Authorization' : 'Authorization';
}
