/**
 * SSRF guard for outbound status-callback URLs (port of go/internal/webhook/ssrf.go).
 *
 * Two-stage rejection, matching the Go original:
 *   1. Pre-flight (cheap, NO DNS) — validateCallbackURL is called at REST
 *      Enqueue time: empty → reject, non-https scheme → reject, literal
 *      "localhost" → reject, IP-literal host in a blocked range → reject.
 *   2. Dial-time (DNS resolve-then-validate) — resolveAndValidate resolves a
 *      hostname once, validates EVERY returned IP, and returns the first IP to
 *      dial. This defeats DNS rebinding: a multi-IP A-record where one entry is
 *      public and one is loopback would otherwise let a connect-time second DNS
 *      lookup return a different IP than the validation lookup saw.
 *
 * Pure leaf module: node stdlib only (node:net, node:dns/promises).
 */

import { isIP } from 'node:net';
import { lookup } from 'node:dns/promises';

/**
 * Thrown when a customer-supplied URL targets a blocked address class.
 * Mirrors Go's ErrSSRFRejected. status_callback_failures_total{reason="ssrf_rejected"}
 * is keyed off this sentinel.
 */
export class SsrfRejectedError extends Error {
  constructor(message = 'webhook: callback URL targets blocked address space') {
    super(message);
    this.name = 'SsrfRejectedError';
  }
}

/** Thrown when the callback URL is the empty string. Mirrors Go's ErrEmptyURL. */
export class EmptyUrlError extends Error {
  constructor(message = 'webhook: callback URL is empty') {
    super(message);
    this.name = 'EmptyUrlError';
  }
}

/** Thrown when the callback URL scheme is not https. Mirrors Go's ErrNonHTTPS. */
export class NonHttpsError extends Error {
  constructor(message = 'webhook: callback URL must use https scheme') {
    super(message);
    this.name = 'NonHttpsError';
  }
}

/**
 * Normalize an IPv4-mapped IPv6 address (::ffff:a.b.c.d / ::ffff:wwww:xxxx) to
 * its plain dotted-quad IPv4 form, so attacker URLs of the form
 *   https://[::ffff:7f00:1]/cb      (loopback in mapped form)
 *   https://[::ffff:a9fe:a9fe]/cb   (cloud-metadata in mapped form)
 * do not bypass the IPv4-only range checks. Returns the input unchanged for
 * genuine (non-mapped) IPv6 addresses and for plain IPv4.
 */
function normalizeMappedV4(ip: string): string {
  const lower = ip.toLowerCase();
  const idx = lower.lastIndexOf('::ffff:');
  if (idx !== 0) {
    return ip;
  }
  const tail = ip.slice('::ffff:'.length);
  // Dotted-quad tail (::ffff:127.0.0.1) — already IPv4 text.
  if (isIP(tail) === 4) {
    return tail;
  }
  // Hex tail (::ffff:7f00:1) — two 16-bit groups → dotted quad.
  const groups = tail.split(':');
  if (groups.length === 2) {
    const hi = Number.parseInt(groups[0], 16);
    const lo = Number.parseInt(groups[1], 16);
    if (Number.isInteger(hi) && Number.isInteger(lo) && hi >= 0 && hi <= 0xffff && lo >= 0 && lo <= 0xffff) {
      const v4 = `${(hi >> 8) & 0xff}.${hi & 0xff}.${(lo >> 8) & 0xff}.${lo & 0xff}`;
      if (isIP(v4) === 4) {
        return v4;
      }
    }
  }
  return ip;
}

/** Parse a dotted-quad IPv4 string into a 32-bit unsigned integer. Caller must pre-validate. */
function v4ToUint(ip: string): number {
  const parts = ip.split('.');
  return ((Number(parts[0]) << 24) >>> 0) + (Number(parts[1]) << 16) + (Number(parts[2]) << 8) + Number(parts[3]);
}

/** True if the IPv4 address (as uint32) is within base/maskBits. */
function v4InCidr(addr: number, baseStr: string, maskBits: number): boolean {
  const base = v4ToUint(baseStr);
  const mask = maskBits === 0 ? 0 : (0xffffffff << (32 - maskBits)) >>> 0;
  return (addr & mask) >>> 0 === (base & mask) >>> 0;
}

function isBlockedV4(ip: string): boolean {
  const addr = v4ToUint(ip);
  return (
    v4InCidr(addr, '127.0.0.0', 8) || // loopback
    v4InCidr(addr, '10.0.0.0', 8) || // RFC1918
    v4InCidr(addr, '172.16.0.0', 12) || // RFC1918
    v4InCidr(addr, '192.168.0.0', 16) || // RFC1918
    v4InCidr(addr, '169.254.0.0', 16) || // link-local (incl. 169.254.169.254 cloud metadata)
    v4InCidr(addr, '224.0.0.0', 4) || // multicast 224/4
    addr === 0 || // unspecified 0.0.0.0
    v4InCidr(addr, '100.64.0.0', 10) || // RFC6598 CGNAT
    v4InCidr(addr, '0.0.0.0', 8) || // "this network" 0/8
    addr === 0xffffffff // limited broadcast 255.255.255.255/32
  );
}

/** Expand an IPv6 address to its eight 16-bit groups. Caller must pre-validate via isIP. */
function v6Groups(ip: string): number[] {
  // Strip a zone id (fe80::1%eth0) if present.
  const zone = ip.indexOf('%');
  const bare = zone === -1 ? ip : ip.slice(0, zone);
  const halves = bare.split('::');
  const head = halves[0] ? halves[0].split(':') : [];
  const tail = halves.length > 1 && halves[1] ? halves[1].split(':') : [];
  const missing = 8 - head.length - tail.length;
  const all = [...head, ...Array(Math.max(missing, 0)).fill('0'), ...tail];
  return all.map((g) => Number.parseInt(g || '0', 16));
}

function isBlockedV6(ip: string): boolean {
  const g = v6Groups(ip);
  if (g.length !== 8) {
    return true; // malformed → blocked (defensive)
  }
  // ::1 loopback
  if (g[0] === 0 && g[1] === 0 && g[2] === 0 && g[3] === 0 && g[4] === 0 && g[5] === 0 && g[6] === 0 && g[7] === 1) {
    return true;
  }
  // :: unspecified
  if (g.every((x) => x === 0)) {
    return true;
  }
  const first = g[0];
  // fc00::/7 unique-local (RFC4193) — high 7 bits == 1111110
  if ((first & 0xfe00) === 0xfc00) {
    return true;
  }
  // fe80::/10 link-local unicast — high 10 bits == 1111111010
  if ((first & 0xffc0) === 0xfe80) {
    return true;
  }
  // ff00::/8 multicast
  if ((first & 0xff00) === 0xff00) {
    return true;
  }
  return false;
}

/**
 * Returns true if ip falls into any address class an outbound webhook MUST NOT
 * target. Blocked ranges (matching ssrf.go):
 *   - loopback:      127.0.0.0/8, ::1
 *   - RFC1918:       10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, fc00::/7
 *   - link-local:    169.254.0.0/16 (incl. 169.254.169.254 metadata), fe80::/10
 *   - multicast:     224.0.0.0/4, ff00::/8
 *   - unspecified:   0.0.0.0, ::
 *   - CGNAT:         100.64.0.0/10 (RFC6598)
 *   - this-network:  0.0.0.0/8
 *   - broadcast:     255.255.255.255/32
 * IPv4-mapped IPv6 (::ffff:127.0.0.1) is normalized to plain IPv4 BEFORE
 * classification. An empty / unparseable address is blocked (true).
 */
export function isBlockedIP(ip: string): boolean {
  if (!ip) {
    return true; // defensive — nil/empty cannot be safely dialed
  }
  const normalized = normalizeMappedV4(ip);
  const kind = isIP(normalized);
  if (kind === 4) {
    return isBlockedV4(normalized);
  }
  if (kind === 6) {
    return isBlockedV6(normalized);
  }
  return true; // not a valid IP literal → blocked (defensive)
}

/**
 * Pre-flight (cheap, no-DNS) rejection path, called from REST handlers at
 * Enqueue time. Throws:
 *   - EmptyUrlError    when url === ""
 *   - NonHttpsError    when the scheme is not "https" (case-insensitive)
 *   - SsrfRejectedError when the host is literal "localhost" (case-insensitive)
 *                       OR an IP literal in a blocked range, OR the URL has an
 *                       empty/unparseable host
 * Returns void (URL passed pre-flight; dial-time may still reject hostnames).
 */
export function validateCallbackURL(url: string): void {
  if (url === '') {
    throw new EmptyUrlError();
  }
  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    throw new SsrfRejectedError(`ssrf: parse URL: ${url}`);
  }
  // URL.protocol includes the trailing colon, e.g. "https:".
  if (parsed.protocol.toLowerCase() !== 'https:') {
    throw new NonHttpsError();
  }
  // URL.hostname keeps the brackets around IPv6 literals (e.g. "[::1]") — strip
  // them so isIP / isBlockedIP see the bare address.
  let host = parsed.hostname;
  if (host.startsWith('[') && host.endsWith(']')) {
    host = host.slice(1, -1);
  }
  if (host === '') {
    throw new SsrfRejectedError('ssrf: URL has empty host');
  }
  if (host.toLowerCase() === 'localhost') {
    throw new SsrfRejectedError('ssrf: literal localhost host');
  }
  if (isIP(host) !== 0) {
    if (isBlockedIP(host)) {
      throw new SsrfRejectedError(`ssrf: literal IP ${host} in blocked range`);
    }
  }
  // Hostname — resolveAndValidate will do the DNS check at connect time.
}

/**
 * Resolve a hostname (or IP literal) once, validate EVERY resolved IP, and
 * return the FIRST IP to dial. Anti-DNS-rebinding: dialing the resolved IP
 * directly means a second connect-time lookup cannot swap in a different IP.
 * Throws SsrfRejectedError if any resolved IP is blocked or if resolution
 * yields no addresses. The HTTP client (status.ts) uses this at dial time.
 */
export async function resolveAndValidate(host: string): Promise<string> {
  // dns.lookup accepts IP literals (returned unchanged) and hostnames; all=true
  // returns every A/AAAA record so we can validate the whole set.
  const records = await lookup(host, { all: true });
  if (records.length === 0) {
    throw new SsrfRejectedError(`ssrf: dns returned no addresses for ${host}`);
  }
  for (const { address } of records) {
    if (isBlockedIP(address)) {
      throw new SsrfRejectedError(`ssrf: blocked IP ${address} for host ${host}`);
    }
  }
  return records[0].address;
}
