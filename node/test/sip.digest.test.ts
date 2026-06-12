import crypto from 'node:crypto';

import { describe, expect, it } from 'vitest';

import {
  authorizationHeaderName,
  buildDigestAuthorization,
  parseAuthHeader,
} from '../src/sip/digest.js';

// Local MD5 so expectations are computed independently of the module under test.
function md5(s: string): string {
  return crypto.createHash('md5').update(s).digest('hex');
}

// Pull a `key="value"` or `key=value` field out of a built header value.
function field(header: string, key: string): string | undefined {
  const m = header.match(new RegExp(`(?:^|,\\s*)${key}=(?:"([^"]*)"|([^,]+))`));
  if (!m) return undefined;
  return m[1] !== undefined ? m[1] : m[2]?.trim();
}

describe('parseAuthHeader', () => {
  it('parses a realistic sipgate-style WWW-Authenticate Digest challenge', () => {
    const header = 'Digest realm="sipgate.de", nonce="abc", qop="auth", algorithm=MD5';
    const p = parseAuthHeader(header);
    expect(p.realm).toBe('sipgate.de');
    expect(p.nonce).toBe('abc');
    expect(p.qop).toBe('auth');
    expect(p.algorithm).toBe('MD5');
  });

  it('handles a qop list and opaque, mixed quoted/unquoted, odd whitespace', () => {
    const header =
      'Digest realm="example.com",  nonce="dcd98b7102dd2f0e8b11d0f600bfb0c093", qop="auth,auth-int", opaque="5ccc069c403ebaf9f0171e9517f40e41", algorithm = MD5';
    const p = parseAuthHeader(header);
    expect(p.realm).toBe('example.com');
    expect(p.qop).toBe('auth,auth-int');
    expect(p.opaque).toBe('5ccc069c403ebaf9f0171e9517f40e41');
    expect(p.algorithm).toBe('MD5');
  });

  it('lower-cases keys regardless of carrier casing and tolerates no scheme prefix', () => {
    const p = parseAuthHeader('Realm="r", NONCE="n"');
    expect(p.realm).toBe('r');
    expect(p.nonce).toBe('n');
  });
});

describe('buildDigestAuthorization — qop=auth path', () => {
  const opts = {
    username: 'alice',
    password: 's3cr3t',
    method: 'INVITE',
    uri: 'sip:+4915123@sipgate.de',
    realm: 'sipgate.de',
    nonce: 'abc123nonce',
    qop: 'auth',
    algorithm: 'MD5',
    nc: '00000001',
    cnonce: 'deadbeefcafef00d', // injected for determinism
  };

  it('computes response = MD5(HA1:nonce:nc:cnonce:qop:HA2)', () => {
    const ha1 = md5(`${opts.username}:${opts.realm}:${opts.password}`);
    const ha2 = md5(`${opts.method}:${opts.uri}`);
    const expected = md5(`${ha1}:${opts.nonce}:${opts.nc}:${opts.cnonce}:auth:${ha2}`);

    const header = buildDigestAuthorization(opts);
    expect(field(header, 'response')).toBe(expected);
  });

  it('emits qop/nc unquoted and cnonce quoted, and the right values', () => {
    const header = buildDigestAuthorization(opts);
    expect(header).toContain('qop=auth,');
    expect(header).toContain('nc=00000001,');
    expect(header).toContain('cnonce="deadbeefcafef00d"');
    expect(field(header, 'qop')).toBe('auth');
    expect(field(header, 'nc')).toBe('00000001');
    expect(field(header, 'cnonce')).toBe('deadbeefcafef00d');
  });

  it('quotes username/realm/nonce/uri/response/opaque and leaves algorithm unquoted', () => {
    const header = buildDigestAuthorization({ ...opts, opaque: 'op4que' });
    expect(header.startsWith('Digest ')).toBe(true);
    expect(header).toContain(`username="alice"`);
    expect(header).toContain(`realm="sipgate.de"`);
    expect(header).toContain(`nonce="abc123nonce"`);
    expect(header).toContain(`uri="sip:+4915123@sipgate.de"`);
    expect(header).toContain('algorithm=MD5');
    expect(header).toContain('opaque="op4que"');
    // algorithm must NOT be quoted
    expect(header).not.toContain('algorithm="MD5"');
  });

  it('selects auth from a qop list "auth,auth-int"', () => {
    const header = buildDigestAuthorization({ ...opts, qop: 'auth,auth-int' });
    expect(field(header, 'qop')).toBe('auth');
    expect(field(header, 'nc')).toBe('00000001');
  });

  it('generates a random cnonce when none is injected', () => {
    const a = buildDigestAuthorization({ ...opts, cnonce: undefined });
    const b = buildDigestAuthorization({ ...opts, cnonce: undefined });
    expect(field(a, 'cnonce')).not.toBe(field(b, 'cnonce'));
    expect(field(a, 'cnonce')).toMatch(/^[0-9a-f]{16}$/);
  });
});

describe('buildDigestAuthorization — qop-less path', () => {
  const opts = {
    username: 'bob',
    password: 'pw',
    method: 'REGISTER',
    uri: 'sip:sipgate.de',
    realm: 'sipgate.de',
    nonce: 'n0nce',
  };

  it('computes response = MD5(HA1:nonce:HA2) and omits qop/nc/cnonce', () => {
    const ha1 = md5(`${opts.username}:${opts.realm}:${opts.password}`);
    const ha2 = md5(`${opts.method}:${opts.uri}`);
    const expected = md5(`${ha1}:${opts.nonce}:${ha2}`);

    const header = buildDigestAuthorization(opts);
    expect(field(header, 'response')).toBe(expected);
    expect(header).not.toContain('qop=');
    expect(header).not.toContain('nc=');
    expect(header).not.toContain('cnonce=');
  });
});

describe('buildDigestAuthorization — MD5-sess', () => {
  it('folds nonce and cnonce into HA1', () => {
    const opts = {
      username: 'alice',
      password: 's3cr3t',
      method: 'INVITE',
      uri: 'sip:x@sipgate.de',
      realm: 'sipgate.de',
      nonce: 'theNonce',
      qop: 'auth',
      algorithm: 'MD5-sess',
      nc: '00000001',
      cnonce: 'fixedcnonce00000',
    };
    const ha1base = md5(`${opts.username}:${opts.realm}:${opts.password}`);
    const ha1 = md5(`${ha1base}:${opts.nonce}:${opts.cnonce}`);
    const ha2 = md5(`${opts.method}:${opts.uri}`);
    const expected = md5(`${ha1}:${opts.nonce}:${opts.nc}:${opts.cnonce}:auth:${ha2}`);

    const header = buildDigestAuthorization(opts);
    expect(field(header, 'response')).toBe(expected);
    expect(header).toContain('algorithm=MD5-sess');
  });
});

describe('RFC 2617 §3.5 worked example', () => {
  it('reproduces the canonical response hash for qop=auth', () => {
    // From RFC 2617 §3.5: Mufasa / Circle Of Life @ testrealm@host.com,
    // GET /dir/index.html, nc=00000001, cnonce="0a4f113b",
    // nonce="dcd98b7102dd2f0e8b11d0f600bfb0c093",
    // expected response = "6629fae49393a05397450978507c4ef1".
    const header = buildDigestAuthorization({
      username: 'Mufasa',
      password: 'Circle Of Life',
      method: 'GET',
      uri: '/dir/index.html',
      realm: 'testrealm@host.com',
      nonce: 'dcd98b7102dd2f0e8b11d0f600bfb0c093',
      qop: 'auth',
      algorithm: 'MD5',
      nc: '00000001',
      cnonce: '0a4f113b',
    });
    expect(field(header, 'response')).toBe('6629fae49393a05397450978507c4ef1');
  });
});

describe('authorizationHeaderName', () => {
  it('maps 401 to Authorization and 407 to Proxy-Authorization', () => {
    expect(authorizationHeaderName(401)).toBe('Authorization');
    expect(authorizationHeaderName(407)).toBe('Proxy-Authorization');
  });
});
