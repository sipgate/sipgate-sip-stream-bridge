/**
 * Tests for createUiHandler (web/server.ts): UI CSP, static serving, and
 * path-traversal safety. The static bundle is served UNauthenticated (no
 * secrets) — auth happens in the UI's own login form against the REST API.
 * Exercised over a real http.Server on an ephemeral port; the bundle root is a
 * temp fixture dir so the tests are decoupled from `make ui-node`.
 */

import fs from 'node:fs';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';
import type { AddressInfo } from 'node:net';

import { afterEach, beforeAll, afterAll, describe, expect, it } from 'vitest';

import { createUiHandler, UI_CONTENT_SECURITY_POLICY } from '../src/web/server.js';

const API_CSP = "default-src 'none'; frame-ancestors 'none'";

let fixtureRoot: string;

beforeAll(() => {
  fixtureRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'ui-fixture-'));
  fs.writeFileSync(
    path.join(fixtureRoot, 'index.html'),
    '<!doctype html><title>sip-stream-bridge test bundle</title>',
  );
  fs.mkdirSync(path.join(fixtureRoot, 'assets'));
  fs.writeFileSync(path.join(fixtureRoot, 'assets', 'index-abc.js'), 'export const ok = 1;\n');
});

afterAll(() => {
  fs.rmSync(fixtureRoot, { recursive: true, force: true });
});

interface Harness {
  url: string;
  close: () => Promise<void>;
}

async function startServer(): Promise<Harness> {
  const uiHandler = createUiHandler({ uiRoot: fixtureRoot });
  const server = http.createServer((req, res) => {
    if (uiHandler(req, res)) return;
    res.writeHead(404, { 'Content-Length': '0' });
    res.end();
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const { port } = server.address() as AddressInfo;
  return {
    url: `http://127.0.0.1:${port}`,
    close: () => new Promise<void>((resolve) => server.close(() => resolve())),
  };
}

describe('createUiHandler', () => {
  let h: Harness;
  afterEach(async () => {
    await h.close();
  });

  it('serves index.html at /ui/ without credentials (static, no secrets)', async () => {
    h = await startServer();
    const resp = await fetch(`${h.url}/ui/`);
    expect(resp.status).toBe(200);
    expect(resp.headers.get('content-type')).toContain('text/html');
    expect(await resp.text()).toContain('sip-stream-bridge test bundle');
  });

  it('serves the bare /ui path (no trailing slash) as the index', async () => {
    h = await startServer();
    const resp = await fetch(`${h.url}/ui`);
    expect(resp.status).toBe(200);
    expect(await resp.text()).toContain('sip-stream-bridge test bundle');
  });

  it('serves hashed assets with a javascript content-type', async () => {
    h = await startServer();
    const resp = await fetch(`${h.url}/ui/assets/index-abc.js`);
    expect(resp.status).toBe(200);
    expect(resp.headers.get('content-type')).toContain('javascript');
  });

  it('sets the UI CSP (distinct from the API CSP) on served responses', async () => {
    h = await startServer();
    const resp = await fetch(`${h.url}/ui/`);
    const csp = resp.headers.get('content-security-policy');
    expect(csp).toBe(UI_CONTENT_SECURITY_POLICY);
    expect(csp).not.toBe(API_CSP);
    expect(csp).toContain("script-src 'self'");
    expect(resp.headers.get('x-content-type-options')).toBe('nosniff');
  });

  it('refuses path traversal with a 404 (never escapes the bundle root)', async () => {
    h = await startServer();
    const resp = await fetch(`${h.url}/ui/..%2f..%2f..%2fetc%2fpasswd`);
    expect(resp.status).toBe(404);
  });

  it('does not handle non-/ui paths', async () => {
    h = await startServer();
    const resp = await fetch(`${h.url}/health`);
    expect(resp.status).toBe(404); // fell through to the test server's 404
  });
});
