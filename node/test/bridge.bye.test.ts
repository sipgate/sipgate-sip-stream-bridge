/**
 * Regression test for in-dialog BYE routing (RFC 3261 §12.2 / §16.12).
 *
 * A real-trunk test surfaced that a BYE to a proxied caller was sent directly to
 * the caller's Contact, bypassing the SIP proxies, so the caller leg never tore
 * down. The fix derives the route set from the INVITE's Record-Route headers
 * (reversed) and routes the BYE via the topmost proxy.
 */
import { describe, expect, it } from 'vitest';

import { buildBye, byeSendTarget } from '../src/bridge/callManager.js';

const base = {
  remoteTarget: 'sip:021193674951@217.10.77.81:5060',
  localIp: '198.51.100.4',
  localSipPort: 5060,
  fromUri: 'sip:2301086t3@sipconnect.sipgate.de',
  fromTag: 'localtag',
  toUri: 'sip:021193674951@sipconnect.sipgate.de',
  toTag: 'remotetag',
  callId: 'abc@host',
  cseq: 2,
};

describe('in-dialog BYE routing', () => {
  it('emits Route headers in REVERSE Record-Route order', () => {
    const rr = ['<sip:217.10.68.150;lr>', '<sip:217.10.79.9;lr>', '<sip:edge.sipgate.de;lr>'];
    const bye = buildBye({ ...base, recordRoutes: rr });
    const routes = bye.split('\r\n').filter((l) => l.startsWith('Route:'));
    expect(routes).toEqual([
      'Route: <sip:edge.sipgate.de;lr>',
      'Route: <sip:217.10.79.9;lr>',
      'Route: <sip:217.10.68.150;lr>',
    ]);
    // Request-URI stays the remote Contact (loose routing).
    expect(bye.split('\r\n')[0]).toBe('BYE sip:021193674951@217.10.77.81:5060 SIP/2.0');
  });

  it('omits Route headers when there is no Record-Route set', () => {
    const bye = buildBye({ ...base, recordRoutes: [] });
    expect(bye.includes('Route:')).toBe(false);
  });

  it('routes the BYE to the topmost proxy (first route-set hop), not the Contact', () => {
    const rr = ['<sip:217.10.68.150;lr>', '<sip:217.10.79.9;lr>', '<sip:217.10.77.5:5060;lr>'];
    // Route set is RR reversed → topmost hop is the LAST RR header.
    expect(byeSendTarget(rr, base.remoteTarget)).toEqual(['217.10.77.5', 5060]);
  });

  it('falls back to the Contact target when there is no route set', () => {
    expect(byeSendTarget([], base.remoteTarget)).toEqual(['217.10.77.81', 5060]);
  });
});
