/**
 * Read-only operator UI handler (Node side). Serves the SAME vite bundle the Go
 * backend embeds — here streamed from disk at dist/ui/ (copied by `make ui-node`
 * / the Dockerfile). Mounted at /ui, same origin.
 *
 * The static bundle is served UNauthenticated — it holds no secrets. The UI
 * logs in via its own form and then calls the REST API with AccountSid:authToken
 * explicitly, so the Twilio API stays strict (no UI special-casing).
 *
 * Mirrors the createApiHandler predicate convention:
 *   const uiHandler = createUiHandler();
 *   if (uiHandler(req, res)) return;   // handled (served or 404'd) a /ui path
 *
 * Returns true when the request targeted /ui* (response written or in flight),
 * false when it is not a /ui path so the caller continues to /health, /metrics, 404.
 */

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import type { IncomingMessage, ServerResponse } from 'node:http';

/**
 * UI Content-Security-Policy. MUST stay byte-identical to the Go UI handler
 * (go/internal/web/server.go UIContentSecurityPolicy) — both backends serve the
 * same bundle and pin this exact string in their tests. Distinct from the REST
 * API's `default-src 'none'`: opens `'self'` for the UI's own hashed assets and
 * same-origin fetches, no 'unsafe-inline'.
 */
export const UI_CONTENT_SECURITY_POLICY =
  "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; font-src 'self'; base-uri 'none'; frame-ancestors 'none'";

/** Minimal extension → Content-Type map (dependency-free, like the rest of Node). */
const CONTENT_TYPES: Record<string, string> = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.svg': 'image/svg+xml',
  '.json': 'application/json; charset=utf-8',
  '.ico': 'image/x-icon',
  '.woff2': 'font/woff2',
  '.woff': 'font/woff',
  '.png': 'image/png',
  '.webmanifest': 'application/manifest+json',
};

export interface UiDeps {
  /**
   * Bundle root. Defaults to dist/ui relative to the compiled module
   * (node/dist/index.js → node/dist/ui). Tests override with a fixture dir.
   */
  uiRoot?: string;
}

/** Default bundle root: dist/ui next to the compiled entrypoint. */
function defaultUiRoot(): string {
  return path.join(path.dirname(fileURLToPath(import.meta.url)), 'ui');
}

export function createUiHandler(
  deps: UiDeps = {},
): (req: IncomingMessage, res: ServerResponse) => boolean {
  const uiRoot = path.resolve(deps.uiRoot ?? defaultUiRoot());

  return (req: IncomingMessage, res: ServerResponse): boolean => {
    const urlPath = (req.url ?? '').split('?')[0];
    if (urlPath !== '/ui' && !urlPath.startsWith('/ui/')) {
      return false; // not a UI path — caller handles it
    }
    // GET/HEAD only; other methods on /ui fall through to the caller's 404.
    const method = (req.method ?? 'GET').toUpperCase();
    if (method !== 'GET' && method !== 'HEAD') {
      return false;
    }

    // UI CSP on every served response (200 + 404-asset), set before writeHead.
    res.setHeader('Content-Security-Policy', UI_CONTENT_SECURITY_POLICY);
    res.setHeader('X-Content-Type-Options', 'nosniff');
    res.setHeader('Strict-Transport-Security', 'max-age=63072000; includeSubDomains');

    const abs = resolveSafe(uiRoot, urlPath);
    if (abs === null) {
      notFound(res); // path traversal attempt
      return true;
    }

    fs.stat(abs, (err, st) => {
      if (err === null && st.isFile()) {
        sendFile(res, abs, method === 'HEAD');
        return;
      }
      // Extension-less miss → SPA index fallback; a missing hashed asset → 404.
      if (path.extname(abs) === '') {
        sendFile(res, path.join(uiRoot, 'index.html'), method === 'HEAD');
      } else {
        notFound(res);
      }
    });
    return true;
  };
}

/**
 * Resolve a /ui* request path to an absolute file under uiRoot, or null if it
 * would escape uiRoot (path traversal). "/ui" and "/ui/" map to index.html.
 */
function resolveSafe(uiRoot: string, urlPath: string): string | null {
  const rel = urlPath === '/ui' ? '' : urlPath.slice('/ui/'.length);
  const decoded = safeDecode(rel);
  if (decoded === null) return null;
  const abs = path.resolve(uiRoot, decoded === '' ? 'index.html' : decoded);
  if (abs !== uiRoot && !abs.startsWith(uiRoot + path.sep)) {
    return null;
  }
  return abs;
}

function safeDecode(s: string): string | null {
  try {
    return decodeURIComponent(s);
  } catch {
    return null;
  }
}

function sendFile(res: ServerResponse, abs: string, headOnly: boolean): void {
  const ctype = CONTENT_TYPES[path.extname(abs).toLowerCase()] ?? 'application/octet-stream';
  if (headOnly) {
    res.writeHead(200, { 'Content-Type': ctype });
    res.end();
    return;
  }
  const stream = fs.createReadStream(abs);
  stream.on('open', () => {
    res.writeHead(200, { 'Content-Type': ctype });
  });
  stream.on('error', () => {
    if (!res.headersSent) {
      notFound(res);
    } else {
      res.destroy();
    }
  });
  stream.pipe(res);
}

function notFound(res: ServerResponse): void {
  res.writeHead(404, { 'Content-Type': 'text/plain; charset=utf-8' });
  res.end('not found');
}
