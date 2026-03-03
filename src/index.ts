import { config } from './config/index.js';
import { createChildLogger } from './logger/index.js';
import { createSipUserAgent } from './sip/userAgent.js';
import { CallManager } from './bridge/callManager.js';

const log = createChildLogger({ component: 'main' });

log.info(
  { event: 'startup', sipUser: config.SIP_USER, sipDomain: config.SIP_DOMAIN },
  'audio-dock starting'
);

async function main(): Promise<void> {
  const callManager = new CallManager(config, createChildLogger({ component: 'call-manager' }));

  const sipHandle = await createSipUserAgent(
    config,
    createChildLogger({ component: 'sip' }),
    callManager.getCallbacks(),
  );

  callManager.setSipHandle(sipHandle);

  log.info({ event: 'sip_booted' }, 'SIP registrar started — waiting for calls');

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
      await callManager.terminateAll(); // BYE + stop event + WS.close in parallel
      await sipHandle.unregister();     // REGISTER with Expires:0, Contact:*
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
