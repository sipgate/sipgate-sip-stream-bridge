/**
 * Twilio-compatible Call REST handlers. Port of go/internal/api/calls.go.
 *
 *   listCallsHandler   — GET  …/Calls.json            (paginated envelope)
 *   getCallHandler     — GET  …/Calls/{CallSid}.json  (single resource)
 *   modifyCallHandler  — POST …/Calls/{CallSid}.json  (mid-call modify)
 *
 * These are framework-agnostic functions over (req, res, ...deps) — the project
 * serves via raw node:http. Each is invoked by server.ts AFTER the
 * security/auth middleware chain has run.
 *
 * Validation and behavior mirror calls.go byte-for-byte (XOR among
 * {Twiml,Url,Status}, StatusCallback-only subscribe, 12100 on oversize Twiml,
 * 21218 on bad Status, 21220 on Twiml/Url against an inactive call, idempotent
 * Status=completed, async dispatch with a test-only sync seam).
 */

import type { IncomingMessage, ServerResponse } from 'node:http';

import type { Logger } from 'pino';

import type { CallManager } from '../bridge/callManager.js';
import { parseResponse, parseStatusCallbackEvents, TwimlError } from '../twiml/parse.js';
import { dispatch } from '../twiml/dispatch.js';
import { validateCallbackURL } from '../webhook/ssrf.js';
import type { Metrics, TwimlModifyKind, TwimlModifyOutcome } from '../observability/metrics.js';
import {
  ErrCallNotInProgress,
  ErrHttpRetrievalFailure,
  ErrInvalidParams,
  ErrNotFound,
  ErrPayloadTooLarge,
  ErrTwimlParseFailure,
} from './errors.js';
import { serializeCall, serializePage, type CallView } from './json.js';
import { MidCallAdapter } from './midcallAdapter.js';
import { PayloadTooLargeError, readFormBody } from './security.js';

/** Twilio's documented inline-Twiml= body limit (chars). Over → 12100. */
const TWIML_MAX_LEN = 4000;

/** Validates Twilio-compatible CallSid strings: "CA" + 32 lowercase hex. */
const CALL_SID_RE = /^CA[0-9a-f]{32}$/;

/** Wall-clock budget for the optional Url= TwiML fetch (primary + fallback). */
const URL_FETCH_TIMEOUT_MS = 15_000;

/**
 * The narrow fetch surface modifyCallHandler needs for the Url= TwiML path.
 * Injected so tests exercise success/failure branches without real network.
 * A failure (reject, or non-2xx surfaced as a reject) maps to Twilio 11200.
 */
export interface TwimlFetcher {
  /**
   * Fetch TwiML over HTTPS. method defaults to POST. timeoutMs caps the total
   * budget (primary + fallback). Resolves with the response body text on a 2xx;
   * rejects on transport failure / non-2xx — the caller maps the rejection to
   * ErrHttpRetrievalFailure (11200).
   */
  fetch(opts: {
    url: string;
    method: string;
    fallbackUrl?: string;
    fallbackMethod?: string;
    timeoutMs: number;
  }): Promise<string>;
}

/** Build the AccountSid-bearing route prefix used for response URIs. */
function pathPrefix(accountSid: string): string {
  return `/2010-04-01/Accounts/${accountSid}`;
}

/** Parse a query-param int with a default; non-integer falls back silently. */
function parseIntDefault(value: string | null, def: number): number {
  if (value === null || value === '') {
    return def;
  }
  const n = Number.parseInt(value, 10);
  return Number.isNaN(n) ? def : n;
}

// ── async-dispatch test seam ───────────────────────────────────────────────

/**
 * When true (production default), modifyCallHandler returns the HTTP response
 * immediately and dispatches the parsed TwiML in the background — Twilio
 * behavior. Tests flip this to false via setAsyncDispatch so post-dispatch
 * state (e.g. <Hangup> terminated the call) is observable in the response body.
 * Mirrors Go's asyncDispatchEnabled / SetAsyncDispatch.
 */
let asyncDispatchEnabled = true;

/**
 * setAsyncDispatch flips the dispatch mode. Production never calls this — it is
 * a test-only seam. Returns the previous value so callers can restore via a
 * try/finally.
 */
export function setAsyncDispatch(enabled: boolean): boolean {
  const prev = asyncDispatchEnabled;
  asyncDispatchEnabled = enabled;
  return prev;
}

// ── metric helpers ─────────────────────────────────────────────────────────

function recordModifyOutcome(
  metrics: Metrics | undefined,
  kind: TwimlModifyKind,
  outcome: TwimlModifyOutcome,
): void {
  metrics?.incTwimlModify(kind, outcome);
}

// ── handlers ───────────────────────────────────────────────────────────────

/**
 * listCallsHandler — paginated Twilio-shape envelope of active +
 * recently-terminated calls. Page defaults 0 (clamped ≥0 inside serializePage);
 * PageSize defaults 50, clamped [1,1000] inside serializePage. Out-of-range
 * pages return 200 + an empty calls array, never 404.
 */
export function listCallsHandler(
  req: IncomingMessage,
  res: ServerResponse,
  manager: CallManager,
  accountSid: string,
): void {
  const url = new URL(req.url ?? '', 'http://localhost');
  const page = parseIntDefault(url.searchParams.get('Page'), 0);
  const pageSize = parseIntDefault(url.searchParams.get('PageSize'), 50);
  // BridgeCall is structurally a CallView — pass directly to the serializer.
  const items: CallView[] = manager.list();
  const envelope = serializePage(items, pathPrefix(accountSid), page, pageSize);
  res.setHeader('Content-Type', 'application/json');
  res.writeHead(200);
  res.end(JSON.stringify(envelope));
}

/**
 * getCallHandler — single Twilio-shape Call resource. A malformed CallSid
 * returns 404 + 20404 (NOT 400): a syntactically invalid Sid is indistinguishable
 * from a non-existent one, and SDKs special-case 404+20404 as "not found".
 */
export function getCallHandler(
  _req: IncomingMessage,
  res: ServerResponse,
  manager: CallManager,
  accountSid: string,
  callSid: string,
): void {
  if (!CALL_SID_RE.test(callSid)) {
    ErrNotFound(`CallSid ${callSid}`).writeJSON(res);
    return;
  }
  const call = manager.getByCallSid(callSid);
  if (call === undefined) {
    ErrNotFound(`CallSid ${callSid}`).writeJSON(res);
    return;
  }
  const cj = serializeCall(call, pathPrefix(accountSid));
  res.setHeader('Content-Type', 'application/json');
  res.writeHead(200);
  res.end(JSON.stringify(cj));
}

/** Parsed + validated body of a modify-call POST. */
interface ModifyOpts {
  twiml: string;
  url: string;
  method: string;
  status: string;
  fallbackUrl: string;
  fallbackMethod: string;
  statusCallback: string;
  statusCallbackMethod: string;
  statusCallbackEvent: string[];
}

/** modifyKind derives the bounded twiml_modify_total kind label. */
function modifyKind(opts: ModifyOpts | null): TwimlModifyKind {
  if (opts === null) {
    return 'twiml';
  }
  if (opts.status !== '') {
    return 'status_completed';
  }
  if (opts.url !== '') {
    return 'url';
  }
  return 'twiml';
}

/**
 * parseModifyOpts validates the form body against the Twilio-strict rules
 * (mirrors parseModifyOpts in calls.go). Returns the parsed opts, or an ApiError
 * to write. SSRF rejection of StatusCallback increments the failures metric.
 */
function parseModifyOpts(
  form: URLSearchParams,
  metrics: Metrics | undefined,
): ModifyOpts | { error: ReturnType<typeof ErrInvalidParams> } {
  const opts: ModifyOpts = {
    twiml: form.get('Twiml') ?? '',
    url: form.get('Url') ?? '',
    method: form.get('Method') ?? '',
    status: form.get('Status') ?? '',
    fallbackUrl: form.get('FallbackUrl') ?? '',
    fallbackMethod: form.get('FallbackMethod') ?? '',
    statusCallback: form.get('StatusCallback') ?? '',
    statusCallbackMethod: form.get('StatusCallbackMethod') ?? '',
    statusCallbackEvent: [],
  };

  // StatusCallbackEvent: flatten repeated keys, each routed through the strict
  // enum tokenizer (handles space/comma separators). Unknown value → 21218.
  const rawEvents = form.getAll('StatusCallbackEvent');
  if (rawEvents.length > 0) {
    const events: string[] = [];
    for (const v of rawEvents) {
      let tokens: string[] | undefined;
      try {
        tokens = parseStatusCallbackEvents(v);
      } catch (e) {
        const detail = e instanceof Error ? e.message : String(e);
        return { error: ErrInvalidParams(`StatusCallbackEvent: ${detail} (Twilio code 21218)`) };
      }
      if (tokens !== undefined) {
        events.push(...tokens);
      }
    }
    opts.statusCallbackEvent = events;
  }

  // Pre-flight SSRF validation of StatusCallback (no DNS).
  if (opts.statusCallback !== '') {
    try {
      validateCallbackURL(opts.statusCallback);
    } catch (e) {
      metrics?.incStatusCallbackFailures('ssrf_rejected');
      const detail = e instanceof Error ? e.message : String(e);
      return { error: ErrInvalidParams(`StatusCallback: ${detail}`) };
    }
    if (opts.statusCallbackMethod === '') {
      opts.statusCallbackMethod = 'POST';
    }
    const m = opts.statusCallbackMethod.toUpperCase();
    if (m !== 'POST' && m !== 'GET') {
      return { error: ErrInvalidParams('StatusCallbackMethod must be POST or GET') };
    }
    opts.statusCallbackMethod = m;
  }

  let count = 0;
  if (opts.twiml !== '') count++;
  if (opts.url !== '') count++;
  if (opts.status !== '') count++;
  if (count > 1) {
    return { error: ErrInvalidParams('at most one of {Twiml, Url, Status} may be set') };
  }
  if (count === 0 && opts.statusCallback === '') {
    return { error: ErrInvalidParams('at least one of {Twiml, Url, Status, StatusCallback} required') };
  }
  // Twiml length cap is checked BEFORE the Status enum (matches Twilio).
  if (opts.twiml.length > TWIML_MAX_LEN) {
    return { error: ErrTwimlParseFailure() };
  }
  if (opts.status !== '' && opts.status !== 'completed') {
    return { error: ErrInvalidParams("Status must be 'completed' (only this terminal value is supported)") };
  }
  if (opts.method === '') {
    opts.method = 'POST';
  }
  return opts;
}

/** Re-fetch the call AFTER any mutation and write the Twilio-shape JSON. */
function writeCallJSON(
  res: ServerResponse,
  manager: CallManager,
  callSid: string,
  accountSid: string,
  status: number,
): void {
  const call = manager.getByCallSid(callSid);
  if (call === undefined) {
    ErrNotFound(`CallSid ${callSid}`).writeJSON(res);
    return;
  }
  const cj = serializeCall(call, pathPrefix(accountSid));
  res.setHeader('Content-Type', 'application/json');
  res.writeHead(status);
  res.end(JSON.stringify(cj));
}

/**
 * modifyCallHandler — the POST handler. Parses the form body, validates against
 * the Twilio-strict rules, then routes to the Status / Twiml / Url / StatusCallback
 * branch. See the file header + calls.go for the full algorithm.
 *
 * fetcher is the injected Url= fetch surface (production wires a real HTTPS
 * client; tests inject a fake). metrics may be undefined in test fixtures.
 */
export async function modifyCallHandler(
  req: IncomingMessage,
  res: ServerResponse,
  manager: CallManager,
  accountSid: string,
  callSid: string,
  fetcher: TwimlFetcher,
  metrics: Metrics | undefined,
  log: Logger,
): Promise<void> {
  // 1. Read + parse the form body (catch oversize → 413/21617).
  let form: URLSearchParams;
  try {
    form = await readFormBody(req);
  } catch (e) {
    if (e instanceof PayloadTooLargeError) {
      ErrPayloadTooLarge().writeJSON(res);
      return;
    }
    // Stream/parse failure — treated as a malformed body (21218).
    ErrInvalidParams('malformed body').writeJSON(res);
    return;
  }

  const parsed = parseModifyOpts(form, metrics);
  if ('error' in parsed) {
    recordModifyOutcome(metrics, 'twiml', 'invalid_params');
    parsed.error.writeJSON(res);
    return;
  }
  const opts = parsed;

  // 2. Validate CallSid shape → 404 on malformed.
  if (!CALL_SID_RE.test(callSid)) {
    ErrNotFound(`CallSid ${callSid}`).writeJSON(res);
    return;
  }
  // 3. Resource lookup → 404 on miss.
  const call = manager.getByCallSid(callSid);
  if (call === undefined) {
    ErrNotFound(`CallSid ${callSid}`).writeJSON(res);
    return;
  }

  // 4. StatusCallback subscription install (independent of Twiml/Url/Status).
  if (opts.statusCallback !== '') {
    const session = manager.getSessionByCallSid(callSid);
    if (session !== undefined && manager.isActive(callSid)) {
      // Customer-supplied subscription — NOT operator-trusted, so it stays
      // behind the SSRF guard at delivery time.
      manager.setStatusCallback(session, {
        url: opts.statusCallback,
        method: opts.statusCallbackMethod,
        events: new Set(opts.statusCallbackEvent),
        trusted: false,
      });
      log.info(
        {
          event: 'status_callback_subscribe',
          callSid,
          statusCallbackMethod: opts.statusCallbackMethod,
          eventCount: opts.statusCallbackEvent.length,
        },
        'modifyCallHandler: status-callback subscription installed',
      );
    }
    // StatusCallback-only (no Twiml/Url/Status): valid → 200 + current JSON.
    if (opts.twiml === '' && opts.url === '' && opts.status === '') {
      recordModifyOutcome(metrics, 'twiml', 'ok');
      writeCallJSON(res, manager, callSid, accountSid, 200);
      return;
    }
  }

  // 5. Status=completed: idempotent. Already-terminated → 200 (NOT 21220).
  if (opts.status === 'completed') {
    if (manager.isActive(callSid)) {
      manager.terminateByCallSid(callSid, 'completed');
      recordModifyOutcome(metrics, 'status_completed', 'terminated');
    } else {
      recordModifyOutcome(metrics, 'status_completed', 'ok');
    }
    writeCallJSON(res, manager, callSid, accountSid, 200);
    return;
  }

  // 6. Twiml= / Url= require an active session → otherwise 21220.
  const session = manager.getSessionByCallSid(callSid);
  if (session === undefined || !manager.isActive(callSid)) {
    recordModifyOutcome(metrics, modifyKind(opts), 'invalid_params');
    ErrCallNotInProgress().writeJSON(res);
    return;
  }

  // 7. Obtain the raw TwiML (inline or fetched over HTTPS).
  let rawTwiml: string;
  if (opts.twiml !== '') {
    rawTwiml = opts.twiml;
  } else {
    // Url= branch. Apply the SSRF pre-flight guard to the Url (and FallbackUrl)
    // before any fetch, mirroring the StatusCallback guard.
    try {
      validateCallbackURL(opts.url);
      if (opts.fallbackUrl !== '') {
        validateCallbackURL(opts.fallbackUrl);
      }
    } catch (e) {
      recordModifyOutcome(metrics, 'url', 'fetch_error');
      const detail = e instanceof Error ? e.message : String(e);
      ErrHttpRetrievalFailure(detail).writeJSON(res);
      return;
    }
    try {
      rawTwiml = await fetcher.fetch({
        url: opts.url,
        method: opts.method,
        fallbackUrl: opts.fallbackUrl !== '' ? opts.fallbackUrl : undefined,
        fallbackMethod: opts.fallbackMethod !== '' ? opts.fallbackMethod : undefined,
        timeoutMs: URL_FETCH_TIMEOUT_MS,
      });
    } catch (e) {
      recordModifyOutcome(metrics, 'url', 'fetch_error');
      const detail = e instanceof Error ? e.message : String(e);
      ErrHttpRetrievalFailure(detail).writeJSON(res);
      return;
    }
  }

  // 8. Parse the TwiML → 12100 on failure.
  let doc;
  try {
    doc = parseResponse(rawTwiml);
  } catch (e) {
    recordModifyOutcome(metrics, modifyKind(opts), 'parse_error');
    if (e instanceof TwimlError) {
      metrics?.incTwimlParseError(String(e.code));
    }
    ErrTwimlParseFailure().writeJSON(res);
    return;
  }

  // 9. Dispatch against the mid-call adapter. <Dial> warn-skips (no DialTarget).
  const adapter = new MidCallAdapter(session, manager, log);
  const runDispatch = async (): Promise<void> => {
    try {
      await dispatch(doc, adapter);
    } catch (err) {
      log.error({ err, callSid, event: 'twiml_dispatch_failed' }, 'twiml dispatch failed');
    }
  };

  if (asyncDispatchEnabled) {
    // Twilio behavior: return immediately with current state; dispatch in bg.
    void runDispatch();
    recordModifyOutcome(metrics, modifyKind(opts), 'ok');
    writeCallJSON(res, manager, callSid, accountSid, 200);
    return;
  }
  // Sync path — test-only. Block until dispatch completes, then read state.
  await runDispatch();
  recordModifyOutcome(metrics, modifyKind(opts), 'ok');
  writeCallJSON(res, manager, callSid, accountSid, 200);
}
