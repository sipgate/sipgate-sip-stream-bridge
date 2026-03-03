import { config } from './config/index.js';
import { createChildLogger } from './logger/index.js';

const log = createChildLogger({ component: 'main' });

log.info(
  { event: 'startup', sipUser: config.SIP_USER, sipDomain: config.SIP_DOMAIN },
  'audio-dock starting — config validated',
);

// Phase 2 will call createSipUserAgent here.
// For Phase 1, keep the process alive so Docker can observe it running.
log.info({ event: 'ready' }, 'Waiting for SIP setup (Phase 2)');
// process stays alive via event loop; no explicit blocking needed.
