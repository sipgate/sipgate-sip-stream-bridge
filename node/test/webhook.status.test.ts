import { AddressInfo } from 'node:net';
import { createServer, IncomingMessage, Server, ServerResponse } from 'node:http';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import pino from 'pino';

import { StatusClient, type CallbackEvent } from '../src/webhook/status.js';
import { Metrics } from '../src/observability/metrics.js';
import { sign } from '../src/webhook/signer.js';

const AUTH_TOKEN = '12345abcdef';
// pino silent logger — no I/O, keeps vitest output clean and avoids open handles.
const log = pino({ level: 'silent' });

/** A request observed by the local callback receiver. */
interface RecordedRequest {
  method: string;
  url: string;
  headers: IncomingMessage['headers'];
  body: string;
}

/**
 * A controllable local callback receiver bound to 127.0.0.1 on an ephemeral
 * port. `respond` decides the status code per request (so retry sequencing can
 * be scripted by attempt index).
 */
class CallbackServer {
  readonly requests: RecordedRequest[] = [];
  private server!: Server;
  private respond: (attempt: number) => number = () => 200;

  async start(): Promise<void> {
    this.server = createServer((req: IncomingMessage, res: ServerResponse) => {
      const chunks: Buffer[] = [];
      req.on('data', (c: Buffer) => chunks.push(c));
      req.on('end', () => {
        const attempt = this.requests.length;
        this.requests.push({
          method: req.method ?? '',
          url: req.url ?? '',
          headers: req.headers,
          body: Buffer.concat(chunks).toString('utf8'),
        });
        const status = this.respond(attempt);
        res.statusCode = status;
        res.end('ok');
      });
    });
    await new Promise<void>((resolve) => this.server.listen(0, '127.0.0.1', resolve));
  }

  setResponder(fn: (attempt: number) => number): void {
    this.respond = fn;
  }

  get baseUrl(): string {
    const addr = this.server.address() as AddressInfo;
    return `http://127.0.0.1:${addr.port}`;
  }

  async stop(): Promise<void> {
    await new Promise<void>((resolve, reject) => {
      this.server.close((err) => (err ? reject(err) : resolve()));
    });
  }
}

/** Canonical Twilio-shape form for a callback event. */
function sampleForm(callSid: string, callStatus: string): Record<string, string> {
  return {
    CallSid: callSid,
    AccountSid: 'ACtest0123456789abcdef0123456789ab',
    From: '+4915123456789',
    To: '+4930111222333',
    Direction: 'inbound',
    ApiVersion: '2010-04-01',
    CallStatus: callStatus,
    SequenceNumber: '0',
    CallbackSource: 'call-progress-events',
  };
}

/** A trusted (SSRF-bypassing) POST event pointed at the local server. */
function trustedEvent(url: string, callSid: string, event: string): CallbackEvent {
  return { url, method: 'POST', form: sampleForm(callSid, event), event, trusted: true };
}

describe('StatusClient delivery', () => {
  let srv: CallbackServer;

  beforeEach(async () => {
    srv = new CallbackServer();
    await srv.start();
  });

  afterEach(async () => {
    await srv.stop();
  });

  it('delivers a trusted POST and sets the correct X-Twilio-Signature', async () => {
    srv.setResponder(() => 200);
    const metrics = new Metrics();
    const client = new StatusClient(AUTH_TOKEN, metrics, log);

    const url = `${srv.baseUrl}/cb`;
    const evt = trustedEvent(url, 'CAhappy', 'completed');
    client.enqueue('CAhappy', evt);
    await client.drainAndClose('CAhappy', 5000);

    expect(srv.requests).toHaveLength(1);
    const req = srv.requests[0];
    expect(req.method).toBe('POST');
    expect(req.headers['content-type']).toBe('application/x-www-form-urlencoded');

    // Recompute the expected signature: verbatim url + form multi-map.
    const multiMap: Record<string, string[]> = {};
    for (const [k, v] of Object.entries(evt.form)) {
      multiMap[k] = [v];
    }
    const expected = sign(AUTH_TOKEN, url, multiMap);
    expect(req.headers['x-twilio-signature']).toBe(expected);

    // Body is the urlencoded form; signature is independent of wire field order.
    const sentParams = new URLSearchParams(req.body);
    expect(sentParams.get('CallSid')).toBe('CAhappy');
    expect(sentParams.get('CallStatus')).toBe('completed');

    // Metrics: one attempt for "completed", zero failures.
    expect(metrics.getPrometheus()).toContain(
      'sipgate_bridge_status_callback_attempts_total{event="completed"} 1',
    );
  });

  it('retries on 500 then succeeds', async () => {
    // First attempt 500, second 200.
    srv.setResponder((attempt) => (attempt === 0 ? 500 : 200));
    const metrics = new Metrics();
    const client = new StatusClient(AUTH_TOKEN, metrics, log);

    client.enqueue('CAretry', trustedEvent(`${srv.baseUrl}/cb`, 'CAretry', 'failed'));
    await client.drainAndClose('CAretry', 10000);

    // Two deliveries: the failing one + the successful retry.
    expect(srv.requests).toHaveLength(2);
    // Two attempts counted; no exhausted_retries failure.
    expect(metrics.getPrometheus()).toContain(
      'sipgate_bridge_status_callback_attempts_total{event="failed"} 2',
    );
    expect(metrics.getPrometheus()).not.toContain(
      'sipgate_bridge_status_callback_failures_total{reason="exhausted_retries"}',
    );
  }, 20000);

  it('exhausts retries on persistent 500', async () => {
    srv.setResponder(() => 500);
    const metrics = new Metrics();
    const client = new StatusClient(AUTH_TOKEN, metrics, log);

    client.enqueue('CAfail', trustedEvent(`${srv.baseUrl}/cb`, 'CAfail', 'failed'));
    await client.drainAndClose('CAfail', 30000);

    // 4 attempts (pre-delays 0/1/2/4s).
    expect(srv.requests).toHaveLength(4);
    const prom = metrics.getPrometheus();
    expect(prom).toContain('sipgate_bridge_status_callback_attempts_total{event="failed"} 4');
    expect(prom).toContain(
      'sipgate_bridge_status_callback_failures_total{reason="exhausted_retries"} 1',
    );
    // Per-attempt 5xx is the retry trigger, NOT recorded as a 5xx failure.
    expect(prom).not.toContain('sipgate_bridge_status_callback_failures_total{reason="5xx"}');
  }, 40000);

  it('abandons a non-retryable 404 after one attempt', async () => {
    srv.setResponder(() => 404);
    const metrics = new Metrics();
    const client = new StatusClient(AUTH_TOKEN, metrics, log);

    client.enqueue('CA404', trustedEvent(`${srv.baseUrl}/cb`, 'CA404', 'ringing'));
    await client.drainAndClose('CA404', 5000);

    expect(srv.requests).toHaveLength(1);
    const prom = metrics.getPrometheus();
    expect(prom).toContain('sipgate_bridge_status_callback_attempts_total{event="ringing"} 1');
    expect(prom).toContain('sipgate_bridge_status_callback_failures_total{reason="4xx"} 1');
  });

  it('caps the per-call queue at depth 64', async () => {
    // Hold the server so jobs pile up: never respond until released.
    let release!: () => void;
    const gate = new Promise<void>((r) => {
      release = r;
    });
    const slowSrv = createServer((_req, res) => {
      void gate.then(() => {
        res.statusCode = 200;
        res.end('ok');
      });
    });
    await new Promise<void>((resolve) => slowSrv.listen(0, '127.0.0.1', resolve));
    const port = (slowSrv.address() as AddressInfo).port;
    const url = `http://127.0.0.1:${port}/cb`;

    const metrics = new Metrics();
    const client = new StatusClient(AUTH_TOKEN, metrics, log);

    // First enqueue starts the worker (which pulls one job and blocks on the
    // gated server). Enqueue 64 + 1 more: depth-64 queue must reject the
    // overflow with a queue_full failure.
    for (let i = 0; i < 70; i++) {
      client.enqueue('CAfull', trustedEvent(url, 'CAfull', 'completed'));
    }

    const prom = metrics.getPrometheus();
    expect(prom).toContain('sipgate_bridge_status_callback_failures_total{reason="queue_full"} ');
    // Extract the queue_full count and assert at least one overflow was dropped.
    const match = prom.match(
      /sipgate_bridge_status_callback_failures_total\{reason="queue_full"\} (\d+)/,
    );
    expect(match).not.toBeNull();
    expect(Number(match?.[1])).toBeGreaterThanOrEqual(1);

    // Release the gate and drain so no handles leak.
    release();
    await client.drainAndClose('CAfull', 10000);
    await new Promise<void>((resolve, reject) =>
      slowSrv.close((err) => (err ? reject(err) : resolve())),
    );
  }, 20000);

  it('drainAndClose resolves for an unknown callSid (idempotent)', async () => {
    const client = new StatusClient(AUTH_TOKEN, new Metrics(), log);
    await expect(client.drainAndClose('CAunknown', 1000)).resolves.toBeUndefined();
    // Second call is also a no-op.
    await expect(client.drainAndClose('CAunknown', 1000)).resolves.toBeUndefined();
  });

  it('drainAndClose drains pending jobs and stops the worker', async () => {
    srv.setResponder(() => 200);
    const metrics = new Metrics();
    const client = new StatusClient(AUTH_TOKEN, metrics, log);

    client.enqueue('CAdrain', trustedEvent(`${srv.baseUrl}/cb`, 'CAdrain', 'completed'));
    client.enqueue('CAdrain', trustedEvent(`${srv.baseUrl}/cb`, 'CAdrain', 'completed'));
    await client.drainAndClose('CAdrain', 5000);

    // Both queued jobs delivered before resolve.
    expect(srv.requests).toHaveLength(2);
    // Re-draining the now-closed callSid is a no-op (idempotent, worker gone).
    await expect(client.drainAndClose('CAdrain', 1000)).resolves.toBeUndefined();
  });
});

describe('StatusClient SSRF pre-flight', () => {
  it('drops a non-trusted localhost URL at enqueue (ssrf_rejected, no throw)', async () => {
    const metrics = new Metrics();
    const client = new StatusClient(AUTH_TOKEN, metrics, log);

    // https://localhost is rejected by validateCallbackURL; enqueue logs+drops.
    expect(() =>
      client.enqueue('CAssrf', {
        url: 'https://localhost/cb',
        method: 'POST',
        form: sampleForm('CAssrf', 'completed'),
        event: 'completed',
        trusted: false,
      }),
    ).not.toThrow();

    expect(metrics.getPrometheus()).toContain(
      'sipgate_bridge_status_callback_failures_total{reason="ssrf_rejected"} 1',
    );
    // No worker/queue was created for the dropped event → drain is a no-op.
    await expect(client.drainAndClose('CAssrf', 1000)).resolves.toBeUndefined();
  });

  it('drops a non-trusted RFC1918 URL at enqueue (ssrf_rejected)', async () => {
    const metrics = new Metrics();
    const client = new StatusClient(AUTH_TOKEN, metrics, log);

    client.enqueue('CArfc', {
      url: 'https://10.0.0.5/cb',
      method: 'POST',
      form: sampleForm('CArfc', 'completed'),
      event: 'completed',
      trusted: false,
    });

    expect(metrics.getPrometheus()).toContain(
      'sipgate_bridge_status_callback_failures_total{reason="ssrf_rejected"} 1',
    );
  });
});
