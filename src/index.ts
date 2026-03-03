import { config } from './config/index.js';
import { createChildLogger } from './logger/index.js';
import { createSipUserAgent } from './sip/userAgent.js';

const log = createChildLogger({ component: 'main' });

log.info(
  { event: 'startup', sipUser: config.SIP_USER, sipDomain: config.SIP_DOMAIN },
  'audio-dock starting'
);

async function main(): Promise<void> {
  const _sipHandle = await createSipUserAgent(config, createChildLogger({ component: 'sip' }));
  log.info({ event: 'sip_booted' }, 'SIP UserAgent started — waiting for REGISTER confirmation');
  // The SIP WebSocket transport keeps the Node.js event loop alive.
  // Phase 2 will add INVITE handlers here.
}

main().catch((err: Error) => {
  log.error({ err, event: 'fatal' }, 'Fatal startup error');
  process.exit(1);
});
