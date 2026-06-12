import { describe, expect, it } from 'vitest';
import {
  EmptyUrlError,
  NonHttpsError,
  SsrfRejectedError,
  isBlockedIP,
  resolveAndValidate,
  validateCallbackURL,
} from '../src/webhook/ssrf.js';

describe('isBlockedIP', () => {
  const cases: Array<[name: string, ip: string, blocked: boolean]> = [
    // Loopback
    ['loopback ipv4 127.0.0.1', '127.0.0.1', true],
    ['loopback ipv4 127.5.6.7', '127.5.6.7', true],
    ['loopback ipv6 ::1', '::1', true],
    // RFC 1918 (IPv4 private)
    ['rfc1918 10/8', '10.1.2.3', true],
    ['rfc1918 172.16', '172.16.0.1', true],
    ['rfc1918 172.31', '172.31.255.254', true],
    ['rfc1918 192.168', '192.168.1.1', true],
    // RFC 4193 IPv6 ULA
    ['rfc4193 fc00::', 'fc00::1', true],
    ['rfc4193 fdff::', 'fdff::1', true],
    // Link-local
    ['link-local ipv4 169.254 (cloud metadata)', '169.254.169.254', true],
    ['link-local ipv6 fe80::', 'fe80::1', true],
    // Multicast
    ['multicast ipv4 224', '224.0.0.1', true],
    ['multicast ipv6 ff02::', 'ff02::1', true],
    // CGNAT (RFC 6598)
    ['cgnat 100.64', '100.64.0.1', true],
    ['cgnat 100.127', '100.127.255.254', true],
    // Unspecified / "this network"
    ['unspecified ipv4', '0.0.0.0', true],
    ['unspecified ipv6', '::', true],
    ['this-network 0.1.2.3', '0.1.2.3', true],
    // Broadcast
    ['broadcast', '255.255.255.255', true],
    // IPv4-mapped IPv6 — normalized to v4 before classification
    ['ipv4-mapped loopback (dotted)', '::ffff:127.0.0.1', true],
    ['ipv4-mapped loopback (hex)', '::ffff:7f00:1', true],
    ['ipv4-mapped rfc1918', '::ffff:10.0.0.1', true],
    ['ipv4-mapped link-local', '::ffff:169.254.169.254', true],
    ['ipv4-mapped link-local (hex)', '::ffff:a9fe:a9fe', true],
    ['ipv4-mapped cgnat', '::ffff:100.64.0.1', true],
    ['ipv4-mapped public allowed', '::ffff:8.8.8.8', false],
    // PUBLIC — must NOT be blocked
    ['public ipv4 8.8.8.8', '8.8.8.8', false],
    ['public ipv4 1.1.1.1', '1.1.1.1', false],
    ['public ipv6 google', '2001:4860:4860::8888', false],
    // Edge of CGNAT (just outside)
    ['public ipv4 100.63.255.254', '100.63.255.254', false],
    ['public ipv4 100.128.0.1', '100.128.0.1', false],
    // Edge of RFC1918 172.16/12 (just outside)
    ['public ipv4 172.15.255.255', '172.15.255.255', false],
    ['public ipv4 172.32.0.0', '172.32.0.0', false],
  ];

  for (const [name, ip, blocked] of cases) {
    it(`${name} → ${blocked ? 'blocked' : 'allowed'}`, () => {
      expect(isBlockedIP(ip)).toBe(blocked);
    });
  }

  it('empty string is blocked (defensive)', () => {
    expect(isBlockedIP('')).toBe(true);
  });

  it('non-IP garbage is blocked (defensive)', () => {
    expect(isBlockedIP('not-an-ip')).toBe(true);
  });
});

describe('validateCallbackURL', () => {
  it('empty URL → EmptyUrlError', () => {
    expect(() => validateCallbackURL('')).toThrow(EmptyUrlError);
  });

  it('http scheme → NonHttpsError', () => {
    expect(() => validateCallbackURL('http://customer.example/cb')).toThrow(NonHttpsError);
  });

  it('https scheme is case-insensitive', () => {
    expect(() => validateCallbackURL('HTTPS://customer.example/cb')).not.toThrow();
  });

  const rejected: Array<[name: string, url: string]> = [
    ['literal localhost', 'https://localhost/cb'],
    ['literal LOCALHOST case-insensitive', 'https://LOCALHOST/cb'],
    ['literal 127.0.0.1', 'https://127.0.0.1/cb'],
    ['literal 10.0.0.1', 'https://10.0.0.1/cb'],
    ['literal 169.254.169.254', 'https://169.254.169.254/cb'],
    ['literal 100.64.0.1', 'https://100.64.0.1/cb'],
    ['literal ::1', 'https://[::1]/cb'],
    ['literal fe80::1', 'https://[fe80::1]/cb'],
    ['literal ::ffff:127.0.0.1', 'https://[::ffff:127.0.0.1]/cb'],
    ['literal ::ffff:10.0.0.1', 'https://[::ffff:10.0.0.1]/cb'],
    ['literal ::ffff:169.254.169.254', 'https://[::ffff:169.254.169.254]/cb'],
  ];

  for (const [name, url] of rejected) {
    it(`${name} → SsrfRejectedError`, () => {
      expect(() => validateCallbackURL(url)).toThrow(SsrfRejectedError);
    });
  }

  const allowed: Array<[name: string, url: string]> = [
    ['public hostname', 'https://customer.example/cb'],
    ['public hostname with port', 'https://customer.example:8443/cb'],
    ['public hostname with path+query', 'https://customer.example/cb?x=1&y=2'],
    ['public IP literal', 'https://8.8.8.8/cb'],
  ];

  for (const [name, url] of allowed) {
    it(`${name} → passes pre-flight`, () => {
      expect(() => validateCallbackURL(url)).not.toThrow();
    });
  }
});

describe('resolveAndValidate (literal-IP path, no real DNS)', () => {
  it('rejects loopback literal', async () => {
    await expect(resolveAndValidate('127.0.0.1')).rejects.toBeInstanceOf(SsrfRejectedError);
  });

  it('rejects RFC1918 literal', async () => {
    await expect(resolveAndValidate('10.0.0.1')).rejects.toBeInstanceOf(SsrfRejectedError);
  });

  it('rejects link-local cloud-metadata literal', async () => {
    await expect(resolveAndValidate('169.254.169.254')).rejects.toBeInstanceOf(SsrfRejectedError);
  });

  it('rejects CGNAT literal', async () => {
    await expect(resolveAndValidate('100.64.0.1')).rejects.toBeInstanceOf(SsrfRejectedError);
  });

  it('rejects IPv4-mapped loopback literal', async () => {
    await expect(resolveAndValidate('::ffff:127.0.0.1')).rejects.toBeInstanceOf(SsrfRejectedError);
  });

  it('returns a public IP literal unchanged', async () => {
    await expect(resolveAndValidate('8.8.8.8')).resolves.toBe('8.8.8.8');
  });
});
