// MUST be first: polyfill globalThis.WebSocket before SIP.js import
import { WebSocket } from 'ws';
(globalThis as any).WebSocket = WebSocket;

import { UserAgent, Registerer, RegistererState } from 'sip.js';
import type { UserAgentOptions } from 'sip.js';
import type { Config } from '../config/index.js';
import type { Logger } from 'pino';

export interface SipHandle {
  ua: UserAgent;
  registerer: Registerer;
}

export async function createSipUserAgent(config: Config, log: Logger): Promise<SipHandle> {
  const uri = UserAgent.makeURI(`sip:${config.SIP_USER}@${config.SIP_DOMAIN}`);
  if (!uri) throw new Error(`Invalid SIP URI: sip:${config.SIP_USER}@${config.SIP_DOMAIN}`);

  const uaOptions: UserAgentOptions = {
    uri,
    viaHost: config.SDP_CONTACT_IP ?? config.SIP_DOMAIN,
    authorizationUsername: config.SIP_USER,
    authorizationPassword: config.SIP_PASSWORD,
    // transportConstructor defaults to WebSocketTransport — no override needed
    transportOptions: {
      server: config.SIP_REGISTRAR,
      // SIP.js WebSocketTransport sets Sec-WebSocket-Protocol: sip internally per RFC 7118
    },
    logLevel: 'warn',
    logBuiltinEnabled: false,
  };

  const ua = new UserAgent(uaOptions);

  const registerer = new Registerer(ua, {
    expires: config.SIP_EXPIRES,
    refreshFrequency: 90,
  });

  registerer.stateChange.addListener((state: RegistererState) => {
    if (state === RegistererState.Registered) {
      log.info({ event: 'sip_registered', expires: config.SIP_EXPIRES }, 'SIP 200 OK — registration confirmed');
    } else if (state === RegistererState.Unregistered) {
      log.warn({ event: 'sip_unregistered' }, 'SIP registration lost');
    } else if (state === RegistererState.Terminated) {
      log.error({ event: 'sip_terminated' }, 'SIP Registerer terminated');
    } else {
      log.debug({ event: 'registerer_state', state }, `SIP registration state: ${state}`);
    }
  });

  ua.transport.onConnect = () => {
    log.debug({ event: 'transport_connected' }, 'SIP WSS transport connected — sending REGISTER');
    registerer.register().catch((err: Error) => {
      log.error({ err, event: 'register_failed' }, 'SIP REGISTER failed after transport connect');
    });
  };

  ua.transport.onDisconnect = (error?: Error) => {
    if (error) {
      log.warn({ err: error, event: 'transport_disconnected' }, 'SIP WSS transport disconnected with error — will reconnect');
    } else {
      log.debug({ event: 'transport_disconnected' }, 'SIP WSS transport disconnected cleanly');
    }
  };

  log.info({ event: 'ua_starting', registrar: config.SIP_REGISTRAR }, 'Starting SIP UserAgent');
  await ua.start();
  await registerer.register();

  return { ua, registerer };
}
