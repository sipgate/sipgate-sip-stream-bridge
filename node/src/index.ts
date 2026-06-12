import http from 'node:http';

import { config } from './config/index.js';
import { createChildLogger } from './logger/index.js';
import { Metrics } from './observability/metrics.js';
import { createSipUserAgent } from './sip/userAgent.js';
import { CallManager } from './bridge/callManager.js';
import { createApiHandler } from './api/server.js';
import { StatusClient } from './webhook/status.js';
import { Guardrails } from './sip/guardrails.js';
import { Forwarder } from './sip/forwarder.js';

const log = createChildLogger({ component: 'main' });

log.info(
  { event: 'startup', sipUser: config.SIP_USER, sipDomain: config.SIP_DOMAIN, srtpEnabled: config.SRTP_ENABLED },
  'sipgate-sip-stream-bridge starting'
);

async function main(): Promise<void> {
  const metrics = new Metrics();

  const callManager = new CallManager(config, createChildLogger({ component: 'call-manager' }), metrics);

  // Status-callback delivery (X-Twilio-Signature, SSRF-guarded, retrying).
  const statusClient = new StatusClient(config.AUTH_TOKEN, metrics, createChildLogger({ component: 'status-callback' }));
  callManager.setStatusClient(statusClient);

  const sipHandle = await createSipUserAgent(
    config,
    createChildLogger({ component: 'sip' }),
    callManager.getCallbacks(),
  );

  callManager.setSipHandle(sipHandle);
  metrics.setSipRegistered(true);

  log.info({ event: 'sip_booted' }, 'SIP registrar started — waiting for calls');

  // B2BUA <Dial> forwarder (toll-fraud guardrails + outbound leg on the shared socket).
  const guardrails = new Guardrails({
    allowedPrefixes: config.DIAL_ALLOWED_PREFIXES.split(',').map((s) => s.trim()).filter((s) => s.length > 0),
    maxPerSession: config.DIAL_MAX_PER_SESSION,
    maxPerMinute: config.DIAL_MAX_PER_MINUTE,
  });
  const forwarder = new Forwarder({
    config,
    sipHandle,
    guardrails,
    statusClient,
    metrics,
    log: createChildLogger({ component: 'forwarder' }),
  });

  // Twilio-compatible REST control plane (/2010-04-01/Accounts/{Sid}/Calls...).
  // Returns true when it handled an API path; falls through otherwise.
  const apiHandler = createApiHandler({
    manager: callManager,
    config,
    metrics,
    log: createChildLogger({ component: 'api' }),
    forwarder,
  });

  // HTTP server: REST control plane + /health + /metrics.
  const httpServer = http.createServer((req, res) => {
    if (apiHandler(req, res)) return;
    if (req.method === 'GET' && req.url === '/health') {
      const body = JSON.stringify(metrics.getHealth());
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(body);
    } else if (req.method === 'GET' && req.url === '/metrics') {
      const body = metrics.getPrometheus();
      res.writeHead(200, { 'Content-Type': 'text/plain; version=0.0.4' });
      res.end(body);
    } else {
      res.writeHead(404, { 'Content-Length': '0' });
      res.end();
    }
  });

  httpServer.listen(config.HTTP_PORT, () => {
    log.info({ event: 'http_server_started', port: config.HTTP_PORT }, 'HTTP server started');
  });

  // Graceful shutdown: SIGTERM + SIGINT share one handler. Drain budget 15s,
  // sequence: stop new HTTP → BYE all calls (both legs for forwarded calls) +
  // close all WS + drain status callbacks → UNREGISTER → exit.
  const DRAIN_BUDGET_MS = 15_000;
  async function shutdown(signal: string): Promise<void> {
    log.info({ event: 'shutdown_start', signal }, 'Graceful shutdown initiated');

    const drainTimeout = setTimeout(() => {
      log.warn({ event: 'shutdown_forced', budgetMs: DRAIN_BUDGET_MS }, 'Shutdown drain timeout — forcing exit');
      process.exit(0);
    }, DRAIN_BUDGET_MS);

    try {
      httpServer.close(); // stop accepting new HTTP requests
      await callManager.terminateAll(); // dual-leg BYE + stop event + WS.close + status drain
      await sipHandle.unregister();     // REGISTER with Expires:0, Contact:*
      metrics.setSipRegistered(false);
      log.info({ event: 'shutdown_complete' }, 'Graceful shutdown complete');
    } catch (err) {
      log.error({ err, event: 'shutdown_error' }, 'Error during shutdown');
    } finally {
      clearTimeout(drainTimeout);
      process.exit(0);
    }
  }

  process.on('SIGTERM', () => { void shutdown('SIGTERM'); });
  process.on('SIGINT',  () => { void shutdown('SIGINT'); });
}

main().catch((err: Error) => {
  log.error({ err, event: 'fatal' }, 'Fatal startup error');
  process.exit(1);
});
