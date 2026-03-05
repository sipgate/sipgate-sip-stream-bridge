import http from 'node:http';

import { config } from './config/index.js';
import { createChildLogger } from './logger/index.js';
import { Metrics } from './observability/metrics.js';
import { createSipUserAgent } from './sip/userAgent.js';
import { CallManager } from './bridge/callManager.js';

const log = createChildLogger({ component: 'main' });

log.info(
  { event: 'startup', sipUser: config.SIP_USER, sipDomain: config.SIP_DOMAIN },
  'audio-dock starting'
);

async function main(): Promise<void> {
  const metrics = new Metrics();

  const callManager = new CallManager(config, createChildLogger({ component: 'call-manager' }), metrics);

  const sipHandle = await createSipUserAgent(
    config,
    createChildLogger({ component: 'sip' }),
    callManager.getCallbacks(),
  );

  callManager.setSipHandle(sipHandle);
  metrics.setSipRegistered(true);

  log.info({ event: 'sip_booted' }, 'SIP registrar started — waiting for calls');

  // HTTP server: /health (OBS-02) and /metrics (OBS-03)
  const httpServer = http.createServer((req, res) => {
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

  // Graceful shutdown handler (LCY-01)
  // CONTEXT.md locked decision: SIGTERM + SIGINT same handler, 5s drain timeout,
  // sequence: BYE all calls + close all WS in parallel → then UNREGISTER → exit
  async function shutdown(signal: string): Promise<void> {
    log.info({ event: 'shutdown_start', signal }, 'Graceful shutdown initiated');

    const drainTimeout = setTimeout(() => {
      log.warn({ event: 'shutdown_forced' }, 'Shutdown drain timeout after 5s — forcing exit');
      process.exit(0);
    }, 5000);

    try {
      httpServer.close(); // stop accepting new HTTP requests
      await callManager.terminateAll(); // BYE + stop event + WS.close in parallel
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
