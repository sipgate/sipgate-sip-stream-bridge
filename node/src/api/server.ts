/**
 * Twilio-compatible REST control-plane HTTP handler. Port of
 * go/internal/api/server.go (Mount + metricsMiddleware).
 *
 * The project serves via raw node:http (NOT chi/express), so instead of a
 * router with middleware factories this exposes a single composed handler:
 *
 *   const apiHandler = createApiHandler(deps);
 *   http.createServer((req, res) => {
 *     if (apiHandler(req, res)) return;   // handled an API path
 *     // …fall through to /health, /metrics, 404…
 *   });
 *
 * The handler returns true when the request matched a Call route (and a
 * response was written or will be written asynchronously), false when the path
 * is not an API path so the caller handles it.
 *
 * Middleware order is load-bearing and pinned by tests:
 *   securityHeaders(res) → maxBytes(req,res) → basicAuth(req,res,…) → handler
 * Security headers + the 413 oversize-body refusal MUST precede auth so HSTS/CSP
 * land on the 401 path and an unauthenticated 10MB POST is rejected with minimum
 * work.
 */

import https from 'node:https';
import type { IncomingMessage, ServerResponse } from 'node:http';

import type { Logger } from 'pino';

import type { CallManager } from '../bridge/callManager.js';
import type { Config } from '../config/index.js';
import type { ApiMethod, ApiRoute, Metrics } from '../observability/metrics.js';
import type { Forwarder } from '../sip/forwarder.js';
import { bucketStatus } from '../observability/metrics.js';
import { basicAuth } from './auth.js';
import {
  getCallHandler,
  listCallsHandler,
  modifyCallHandler,
  type TwimlFetcher,
} from './calls.js';
import { matchRoute, type ApiRouteName } from './router.js';
import { maxBytes, securityHeaders } from './security.js';

/** Dependencies for the composed API handler. */
export interface ApiDeps {
  manager: CallManager;
  config: Config;
  metrics?: Metrics;
  log: Logger;
  /**
   * Url= TwiML fetch surface. Defaults to defaultTwimlFetcher (real HTTPS).
   * Tests inject a fake to avoid real network.
   */
  fetcher?: TwimlFetcher;
  /** B2BUA forwarder for <Dial>. When absent, <Dial> warn-skips (streaming-only). */
  forwarder?: Forwarder;
}

/** Map a router route name to the bounded metrics ApiRoute label. */
const ROUTE_METRIC: Record<ApiRouteName, ApiRoute> = {
  list_calls: 'list_calls',
  get_call: 'get_call',
  modify_call: 'modify_call',
};

/**
 * defaultTwimlFetcher fetches TwiML over HTTPS for the Url= modify path. The
 * total budget (primary + optional fallback) is bounded by timeoutMs. A non-2xx
 * primary falls through to the fallback; both failing rejects so the caller
 * emits 11200. Production wiring; tests inject a fake instead.
 */
export const defaultTwimlFetcher: TwimlFetcher = {
  async fetch(opts): Promise<string> {
    const deadline = Date.now() + opts.timeoutMs;

    const once = (targetUrl: string, method: string): Promise<string> =>
      new Promise<string>((resolve, reject) => {
        const remaining = deadline - Date.now();
        if (remaining <= 0) {
          reject(new Error('url fetch budget exhausted'));
          return;
        }
        let parsed: URL;
        try {
          parsed = new URL(targetUrl);
        } catch {
          reject(new Error(`invalid Url: ${targetUrl}`));
          return;
        }
        if (parsed.protocol !== 'https:') {
          reject(new Error('Url must use https scheme'));
          return;
        }
        const reqUp = https.request(
          targetUrl,
          { method: method.toUpperCase(), timeout: remaining },
          (resp) => {
            const status = resp.statusCode ?? 0;
            const chunks: Buffer[] = [];
            resp.on('data', (c: Buffer) => chunks.push(c));
            resp.on('end', () => {
              const body = Buffer.concat(chunks).toString('utf8');
              if (status >= 200 && status < 300) {
                resolve(body);
              } else {
                reject(new Error(`non-2xx status ${status} from ${parsed.host}`));
              }
            });
          },
        );
        reqUp.on('timeout', () => {
          reqUp.destroy(new Error('url fetch timeout'));
        });
        reqUp.on('error', (err) => reject(err));
        reqUp.end();
      });

    try {
      return await once(opts.url, opts.method);
    } catch (primaryErr) {
      if (opts.fallbackUrl !== undefined && opts.fallbackUrl !== '') {
        return once(opts.fallbackUrl, opts.fallbackMethod ?? 'POST');
      }
      throw primaryErr;
    }
  },
};

/**
 * Wrap a ServerResponse to capture the final status code for metrics. writeHead
 * records the explicit code; end()-without-writeHead defaults to 200 (node:http
 * convention). The wrapper proxies through to the underlying response.
 */
function captureStatus(res: ServerResponse): { res: ServerResponse; code: () => number } {
  let code = 0;
  const origWriteHead = res.writeHead.bind(res);
  // eslint compatible: reassign the method to record then delegate.
  res.writeHead = function patchedWriteHead(this: ServerResponse, statusCode: number, ...rest: unknown[]): ServerResponse {
    code = statusCode;
    // @ts-expect-error — variadic node:http overloads; we delegate verbatim.
    return origWriteHead(statusCode, ...rest);
  } as ServerResponse['writeHead'];
  return {
    res,
    code: (): number => (code === 0 ? 200 : code),
  };
}

/**
 * createApiHandler composes the security/auth middleware chain with the route
 * dispatch. Returns a predicate handler: true = this was an API path and has
 * been handled; false = not an API path (caller handles /health, /metrics, 404).
 */
export function createApiHandler(deps: ApiDeps): (req: IncomingMessage, res: ServerResponse) => boolean {
  const { manager, config, metrics, log, forwarder } = deps;
  const fetcher = deps.fetcher ?? defaultTwimlFetcher;

  return (req: IncomingMessage, res: ServerResponse): boolean => {
    const match = matchRoute(req);
    if (match === null) {
      return false; // not an API path — caller handles it
    }

    // ── Middleware chain (order is load-bearing) ──
    // 1. Security headers on EVERY response, including 401/413.
    securityHeaders(res);
    // 2. Tier-1 Content-Length 413 — BEFORE any credential work.
    if (!maxBytes(req, res)) {
      return true; // 413 already written
    }
    // 3. Basic Auth + URL-path AccountSid validation.
    if (!basicAuth(req, res, manager.accountSid, config.AUTH_TOKEN, match.accountSid)) {
      return true; // 401 already written
    }

    // ── Metrics capture wraps the handler dispatch ──
    const route = ROUTE_METRIC[match.route];
    const method = (req.method ?? 'GET').toUpperCase() as ApiMethod;
    const cap = captureStatus(res);
    const start = process.hrtime.bigint();
    const observe = (): void => {
      if (metrics === undefined) {
        return;
      }
      const seconds = Number(process.hrtime.bigint() - start) / 1e9;
      metrics.incApiRequest(route, method, bucketStatus(cap.code()));
      metrics.observeApiRequestDuration(route, seconds);
    };

    switch (match.route) {
      case 'list_calls':
        listCallsHandler(req, res, manager, match.accountSid);
        observe();
        return true;
      case 'get_call':
        getCallHandler(req, res, manager, match.accountSid, match.callSid ?? '');
        observe();
        return true;
      case 'modify_call':
        // Async — observe AFTER the response is written. modifyCallHandler
        // resolves once writeCallJSON has set the status (background TwiML
        // dispatch, when async, continues independently).
        void modifyCallHandler(
          req,
          res,
          manager,
          match.accountSid,
          match.callSid ?? '',
          fetcher,
          metrics,
          log,
          forwarder,
        )
          .catch((err: unknown) => {
            log.error({ err, event: 'modify_call_unhandled' }, 'modifyCallHandler unhandled error');
            if (!res.headersSent) {
              res.writeHead(500, { 'Content-Type': 'application/json' });
              res.end(JSON.stringify({ code: 0, message: 'internal error', more_info: '', status: 500 }));
            }
          })
          .finally(() => {
            observe();
          });
        return true;
      default:
        return false;
    }
  };
}
