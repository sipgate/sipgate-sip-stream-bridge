/**
 * Status-callback delivery client (port of go/internal/webhook/status.go).
 *
 * Owns a private outbound HTTP transport distinct from the voice-WC fetcher and
 * the <Dial>-action poster: a flapping customer-supplied callback host MUST NOT
 * eat connection capacity from other paths. Per-CallSid serial worker + bounded
 * queue (depth 64) gives an at-least-once-per-emission guarantee with bounded
 * blast radius.
 *
 * Delivery is retried with pre-delays [0, 1000, 2000, 4000] ms (4 attempts);
 * each attempt is capped at 4000 ms. Retry-eligible: network/timeout error,
 * HTTP 408, 429, >=500. SSRF rejections are NEVER retried — and for
 * customer-supplied (non-trusted) URLs the SSRF guard runs at BOTH enqueue
 * (cheap pre-flight) and dial time (resolve-then-validate every IP, dial a
 * validated IP — defeats DNS rebinding).
 *
 * Lifecycle invariant (status_leak_test.go): every CallSid that ever enqueues
 * MUST be torn down via drainAndClose, which stops the worker and clears all
 * timers — no leaked handles, so vitest exits cleanly.
 */

import { Agent as HttpAgent, request as httpRequest } from 'node:http';
import { Agent as HttpsAgent, request as httpsRequest } from 'node:https';
import type { IncomingMessage } from 'node:http';
import { isIP, type LookupFunction } from 'node:net';
import type { Logger } from 'pino';

import type { Metrics } from '../observability/metrics.js';
import { bucketStatusCallbackReason } from '../observability/metrics.js';
import { sign } from './signer.js';
import {
  SsrfRejectedError,
  isBlockedIP,
  resolveAndValidate,
  validateCallbackURL,
} from './ssrf.js';

/**
 * Fully-built emission payload, ready for transport. Built by the emit helpers
 * (session / forwarder layer). Mirrors Go's CallbackEvent.
 *
 *  - url:     verbatim customer-supplied StatusCallback= bytes; signed verbatim
 *             (NOT normalized) so the signature matches what the customer's
 *             RequestValidator recomputes.
 *  - method:  "POST" or "GET". POST is the Twilio default + recommendation.
 *  - form:    canonical Twilio form fields, already value-canonical (CallStatus
 *             bare lower-case event, Timestamp RFC1123Z, ...). Sort/dedupe of
 *             multi-value keys happens inside sign().
 *  - event:   event-vocab label for the per-event metric. Distinct from
 *             Form.CallStatus (status-vocab). One of: initiated, ringing,
 *             answered, in-progress, completed, busy, failed, no-answer,
 *             canceled.
 *  - trusted: true marks the URL operator-supplied (STATUS_CALLBACK_DEFAULT_URL)
 *             so enqueue bypasses the SSRF pre-flight and delivery skips the
 *             dial-time SSRF guard. Customer URLs MUST leave this false.
 */
export interface CallbackEvent {
  url: string;
  method: string;
  form: Record<string, string>;
  event: string;
  trusted: boolean;
}

/** Per-CallSid queue capacity. Acceptance criterion: depth 64. */
const PER_CALL_QUEUE_DEPTH = 64;

/** Per-attempt HTTP timeout: a single attempt is capped at 4 seconds. */
const PER_ATTEMPT_TIMEOUT_MS = 4000;

/**
 * Pre-delays before attempts 1→2, 2→3, 3→4 (the first attempt has 0 pre-delay).
 * 4 attempts total. No jitter (matches Go) — queue depth 64 + 3-retries-then-
 * abandon caps thundering-herd risk.
 */
const BACKOFFS_MS: readonly number[] = [0, 1000, 2000, 4000];

/** Default drain budget; worst-case retry chain (1+2+4 backoff + 4×4s) ≈ 23s. */
const DEFAULT_DRAIN_TIMEOUT_MS = 30000;

/** In-flight envelope on the per-call queue. */
interface CallbackJob {
  callSid: string;
  evt: CallbackEvent;
}

/**
 * Queue + worker control surface for one CallSid. `closed` flips when
 * drainAndClose is called: the worker finishes draining `queue` then exits and
 * resolves `done`. New enqueues are rejected once closed.
 */
interface PerCallState {
  queue: CallbackJob[];
  closed: boolean;
  /** Resolves the promise that wakes the worker when a job arrives or on close. */
  wake: (() => void) | null;
  /** Resolved when the worker loop has exited (used by drainAndClose). */
  done: Promise<void>;
  resolveDone: () => void;
  /** Tracks the active per-attempt timeout timer so drain can be reasoned about. */
  activeTimer: NodeJS.Timeout | null;
}

/** Outcome of a single delivery attempt: HTTP status (0 == transport failure) + optional error. */
interface AttemptResult {
  status: number;
  err: Error | null;
}

/**
 * StatusClient — public API:
 *   - new StatusClient(authToken, metrics?, log)
 *   - enqueue(callSid, evt): void   (non-blocking; drops + records metric on
 *                                    SSRF reject / queue-full)
 *   - drainAndClose(callSid, timeoutMs?): Promise<void>  (idempotent)
 */
export class StatusClient {
  private readonly authToken: string;
  private readonly metrics: Metrics | undefined;
  private readonly log: Logger;

  /** Keyed agents reuse connections per host while bounding the FD footprint. */
  private readonly httpsAgent = new HttpsAgent({ keepAlive: true, maxSockets: 8, maxFreeSockets: 2 });
  private readonly httpAgent = new HttpAgent({ keepAlive: true, maxSockets: 8, maxFreeSockets: 2 });

  private readonly perCall = new Map<string, PerCallState>();

  constructor(authToken: string, metrics: Metrics | undefined, log: Logger) {
    this.authToken = authToken;
    this.metrics = metrics;
    this.log = log;
  }

  /**
   * Queue a callback for delivery. Non-blocking. Starts the per-CallSid worker
   * on first enqueue for that CallSid.
   *
   * Drops (does NOT throw) on:
   *   - SSRF pre-flight reject (non-trusted URL) → failures{ssrf_rejected}, warn
   *   - queue full at depth 64                   → failures{queue_full}, warn
   * Matches Go's enqueue-time log-and-drop for SSRF.
   */
  enqueue(callSid: string, evt: CallbackEvent): void {
    if (callSid === '') {
      this.log.warn('status_callback: enqueue requires non-empty callSid; dropping');
      return;
    }

    const event: CallbackEvent = { ...evt, method: evt.method === '' ? 'POST' : evt.method };

    // Pre-flight URL validation for customer-supplied URLs. Trusted (operator)
    // URLs skip it — they may legitimately point at 127.0.0.1 (sidecars, e2e).
    if (!event.trusted) {
      try {
        validateCallbackURL(event.url);
      } catch (err) {
        this.metrics?.incStatusCallbackFailures('ssrf_rejected');
        this.log.warn(
          { callSid, event: event.event, err: errMessage(err) },
          'status_callback: SSRF pre-flight rejected callback URL; dropping',
        );
        return;
      }
    }

    const st = this.getOrCreateState(callSid);
    if (st.closed) {
      this.log.warn({ callSid, event: event.event }, 'status_callback: enqueue after close; dropping');
      return;
    }

    if (st.queue.length >= PER_CALL_QUEUE_DEPTH) {
      this.metrics?.incStatusCallbackFailures('queue_full');
      this.log.warn({ callSid, event: event.event }, 'status_callback: per-call queue full; dropping');
      return;
    }

    st.queue.push({ callSid, evt: event });
    // Wake the worker if it is parked waiting for a job.
    if (st.wake) {
      const w = st.wake;
      st.wake = null;
      w();
    }
  }

  /**
   * Stop accepting new jobs for callSid, let the worker drain remaining jobs up
   * to timeoutMs, then resolve. Idempotent: an unknown callSid resolves
   * immediately. Removes the per-call entry so the map returns to empty under
   * churn (leak invariant). No timers are left running after this resolves.
   */
  async drainAndClose(callSid: string, timeoutMs: number = DEFAULT_DRAIN_TIMEOUT_MS): Promise<void> {
    const st = this.perCall.get(callSid);
    if (st === undefined) {
      return;
    }
    // Remove eagerly so re-entrant drainAndClose is idempotent and the map
    // returns to zero entries immediately.
    this.perCall.delete(callSid);

    st.closed = true;
    // Wake a parked worker so it observes `closed` and exits its loop.
    if (st.wake) {
      const w = st.wake;
      st.wake = null;
      w();
    }

    let timer: NodeJS.Timeout | undefined;
    const timeout = new Promise<void>((resolve) => {
      timer = setTimeout(resolve, timeoutMs);
      // Do not keep the event loop alive solely for the drain budget.
      timer.unref();
    });

    await Promise.race([st.done, timeout]);
    if (timer !== undefined) {
      clearTimeout(timer);
    }
  }

  // ── internals ──

  private getOrCreateState(callSid: string): PerCallState {
    const existing = this.perCall.get(callSid);
    if (existing !== undefined) {
      return existing;
    }
    let resolveDone!: () => void;
    const done = new Promise<void>((resolve) => {
      resolveDone = resolve;
    });
    const st: PerCallState = {
      queue: [],
      closed: false,
      wake: null,
      done,
      resolveDone,
      activeTimer: null,
    };
    this.perCall.set(callSid, st);
    // Spawn the single serial worker for this CallSid.
    void this.runWorker(st);
    return st;
  }

  /**
   * Per-CallSid serial drain loop. Processes one job at a time. Exits when the
   * queue is empty AND the state is closed, then resolves `done`.
   */
  private async runWorker(st: PerCallState): Promise<void> {
    try {
      for (;;) {
        const job = st.queue.shift();
        if (job === undefined) {
          if (st.closed) {
            return;
          }
          // Park until enqueue or drainAndClose wakes us.
          await new Promise<void>((resolve) => {
            st.wake = resolve;
          });
          continue;
        }
        await this.deliverWithRetries(st, job);
      }
    } finally {
      st.resolveDone();
    }
  }

  /**
   * Up to 4 attempts with pre-delays 0/1/2/4s. Increments attempts{event} per
   * delivered attempt (matching Go: per-attempt, not per-event). On a
   * non-retryable failure, records the bucketed failure and abandons. On
   * retry exhaustion, increments failures{exhausted_retries}.
   */
  private async deliverWithRetries(st: PerCallState, job: CallbackJob): Promise<void> {
    const eventLabel = job.evt.event;
    let last: AttemptResult = { status: 0, err: null };

    for (let attempt = 0; attempt < BACKOFFS_MS.length; attempt++) {
      const delay = BACKOFFS_MS[attempt] ?? 0;
      if (delay > 0) {
        await sleep(delay);
      }

      const result = await this.deliverOnce(job);
      last = result;

      // Per-attempt metric (success or fail). Event label is the bounded
      // 9-value event vocab populated at the emit site.
      if (eventLabel !== '') {
        this.metrics?.incStatusCallbackAttempts(eventLabel as StatusCallbackEventLabel);
      }

      // Success.
      if (result.err === null && result.status >= 200 && result.status < 300) {
        return;
      }

      // Non-retryable → abandon.
      if (!shouldRetry(result.status, result.err)) {
        this.recordFailure(result.status, result.err);
        this.log.warn(
          {
            callSid: job.callSid,
            event: eventLabel,
            statusCode: result.status,
            attempt: attempt + 1,
            err: errMessage(result.err),
          },
          'status_callback: abandoning (non-retryable)',
        );
        return;
      }
      // else: retry on next loop iteration
    }

    // Exhausted retries.
    this.metrics?.incStatusCallbackFailures('exhausted_retries');
    this.log.warn(
      { callSid: job.callSid, event: eventLabel, lastStatus: last.status, err: errMessage(last.err) },
      'status_callback: exhausted retries; abandoning',
    );
  }

  /**
   * Single delivery attempt. Returns the HTTP status (0 on transport failure)
   * plus any transport error. Per-attempt timeout enforced via a timer that
   * destroys the request. For non-trusted URLs the SSRF DNS-pinning lookup is
   * installed on the request so the socket connects only to a validated IP.
   */
  private async deliverOnce(job: CallbackJob): Promise<AttemptResult> {
    const { evt } = job;
    let target: URL;
    try {
      target = new URL(evt.url);
    } catch {
      return { status: 0, err: new SsrfRejectedError(`status_callback: parse URL: ${evt.url}`) };
    }

    const isPost = evt.method.toUpperCase() === 'POST';
    let body = '';
    if (isPost) {
      body = encodeForm(evt.form);
    } else {
      // GET: append form values as query params on the wire (form is not sent
      // as a body). The SIGNED url remains the verbatim evt.url (Go signs GET
      // with nil params over the verbatim URL).
      for (const [k, v] of Object.entries(evt.form)) {
        target.searchParams.append(k, v);
      }
    }

    // X-Twilio-Signature: verbatim evt.url, with form params for POST, none for
    // GET. NEVER sign the query-appended URL.
    const signParams = isPost ? formToMultiMap(evt.form) : {};
    const signature = sign(this.authToken, evt.url, signParams);

    const isHttps = target.protocol === 'https:';

    // SSRF DNS-pinning for non-trusted URLs: provide a custom lookup that
    // resolve-then-validates EVERY A/AAAA record and hands the socket a single
    // validated IP literal. A blocked IP (or an IP literal host already in a
    // blocked range) rejects the connection before any bytes leave the box.
    let lookup: LookupFunction | undefined;
    if (!evt.trusted) {
      lookup = this.makeSsrfLookup();
    }

    return await this.doRequest(target, evt.method.toUpperCase(), isHttps, isPost, body, signature, lookup);
  }

  /**
   * Build a node:dns-shaped lookup callback that resolves the host, validates
   * every returned IP via the SSRF guard, and connects only to a validated IP.
   * Throwing/erroring here aborts the socket connection (classified as a
   * connect_error, never retried into a blocked host).
   */
  private makeSsrfLookup(): LookupFunction {
    return (hostname, _options, callback): void => {
      // IP-literal hosts: validate directly (no DNS) so a literal blocked IP is
      // rejected and a literal public IP is dialed as-is.
      const kind = isIP(hostname);
      if (kind !== 0) {
        if (isBlockedIP(hostname)) {
          callback(new SsrfRejectedError(`ssrf: literal IP ${hostname} in blocked range`), '');
          return;
        }
        callback(null, hostname, kind);
        return;
      }
      resolveAndValidate(hostname).then(
        (ip) => {
          callback(null, ip, isIP(ip) === 6 ? 6 : 4);
        },
        (err: unknown) => {
          const e =
            err instanceof Error
              ? (err as NodeJS.ErrnoException)
              : (new SsrfRejectedError(String(err)) as NodeJS.ErrnoException);
          callback(e, '');
        },
      );
    };
  }

  /** Perform the HTTP(S) request with a hard per-attempt timeout. */
  private doRequest(
    target: URL,
    method: string,
    isHttps: boolean,
    isPost: boolean,
    body: string,
    signature: string,
    lookup: LookupFunction | undefined,
  ): Promise<AttemptResult> {
    return new Promise<AttemptResult>((resolve) => {
      const headers: Record<string, string> = { 'X-Twilio-Signature': signature };
      if (isPost) {
        headers['Content-Type'] = 'application/x-www-form-urlencoded';
        headers['Content-Length'] = String(Buffer.byteLength(body));
      }

      const requestFn = isHttps ? httpsRequest : httpRequest;
      const agent = isHttps ? this.httpsAgent : this.httpAgent;

      let settled = false;
      const finish = (result: AttemptResult): void => {
        if (settled) {
          return;
        }
        settled = true;
        clearTimeout(timer);
        resolve(result);
      };

      const req = requestFn(
        target,
        { method, headers, agent, ...(lookup ? { lookup } : {}) },
        (res: IncomingMessage) => {
          const status = res.statusCode ?? 0;
          // Drain so the socket can be reused / freed.
          res.on('data', () => {});
          res.on('end', () => finish({ status, err: null }));
          res.on('error', (err) => finish({ status, err }));
        },
      );

      const timer = setTimeout(() => {
        req.destroy(new Error('status_callback: per-attempt timeout (deadline exceeded)'));
      }, PER_ATTEMPT_TIMEOUT_MS);
      timer.unref();

      req.on('error', (err: Error) => finish({ status: 0, err }));

      if (isPost && body !== '') {
        req.write(body);
      }
      req.end();
    });
  }

  /**
   * Record a bucketed failure for a non-retryable outcome. Delegates the bounded
   * reason classification to the canonical bucketer; '' means "do not record".
   */
  private recordFailure(status: number, err: Error | null): void {
    if (this.metrics === undefined) {
      return;
    }
    const reason = bucketStatusCallbackReason(err, status);
    if (reason === '') {
      return;
    }
    this.metrics.incStatusCallbackFailures(reason);
  }
}

// ── module-level helpers ──

/** The bounded 9-value event vocabulary for the attempts metric label. */
type StatusCallbackEventLabel = Parameters<Metrics['incStatusCallbackAttempts']>[0];

/**
 * Retry policy (matches Go shouldRetry). Order matters: SSRF rejection is never
 * retried; any transport error retries; HTTP 408/429/>=500 retry; 3xx and other
 * 4xx are terminal.
 */
function shouldRetry(status: number, err: Error | null): boolean {
  if (err instanceof SsrfRejectedError) {
    return false;
  }
  if (err !== null) {
    return true;
  }
  if (status === 408 || status === 429) {
    return true;
  }
  return status >= 500;
}

/** application/x-www-form-urlencoded encoding of the form map (stable: key insertion order). */
function encodeForm(form: Record<string, string>): string {
  const params = new URLSearchParams();
  for (const [k, v] of Object.entries(form)) {
    params.append(k, v);
  }
  return params.toString();
}

/** Convert the single-value form map into the multi-map shape sign() expects. */
function formToMultiMap(form: Record<string, string>): Record<string, string[]> {
  const out: Record<string, string[]> = {};
  for (const [k, v] of Object.entries(form)) {
    out[k] = [v];
  }
  return out;
}

/** A promise-based delay. */
function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => {
    const t = setTimeout(resolve, ms);
    t.unref();
  });
}

/** Extract a message string from an unknown error-ish value (undefined if none). */
function errMessage(err: unknown): string | undefined {
  if (err === null || err === undefined) {
    return undefined;
  }
  if (err instanceof Error) {
    return err.message;
  }
  return String(err);
}
