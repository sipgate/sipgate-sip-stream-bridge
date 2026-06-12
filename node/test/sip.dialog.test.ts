import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { OutboundDialog } from '../src/sip/dialog.js';
import type { DialogHandlers, SipHandle } from '../src/sip/userAgent.js';

const rinfo = { address: '203.0.113.9', port: 5060, family: 'IPv4', size: 0 } as const;

interface FakeSip {
  handle: SipHandle;
  sent: string[];
  handlers: () => DialogHandlers | null;
  registered: () => boolean;
}

function fakeSip(): FakeSip {
  const sent: string[] = [];
  let handlers: DialogHandlers | null = null;
  let registeredCallId: string | null = null;
  const handle: SipHandle = {
    stop: () => {},
    unregister: () => Promise.resolve(),
    sendRaw: (buf: Buffer) => {
      sent.push(buf.toString());
    },
    registerDialog: (callId: string, h: DialogHandlers) => {
      registeredCallId = callId;
      handlers = h;
    },
    unregisterDialog: (callId: string) => {
      if (registeredCallId === callId) registeredCallId = null;
    },
  };
  return { handle, sent, handlers: () => handlers, registered: () => registeredCallId !== null };
}

function makeDialog(sip: FakeSip): OutboundDialog {
  const log = { info: () => {}, debug: () => {}, warn: () => {}, error: () => {} } as never;
  return new OutboundDialog({
    sipHandle: sip.handle,
    localIp: '198.51.100.4',
    localSipPort: 5060,
    targetUri: 'sip:+4915123456@sipconnect.sipgate.de',
    targetHost: '203.0.113.9',
    targetPort: 5060,
    fromUri: 'sip:+4930555@sipconnect.sipgate.de',
    contactUri: 'sip:user@198.51.100.4:5060',
    offerSdp: 'v=0\r\n',
    username: 'e12345p0',
    password: 'secret',
    log,
  });
}

function resp(code: number, reason: string, extra = ''): string {
  return (
    `SIP/2.0 ${code} ${reason}\r\n` +
    `Via: SIP/2.0/UDP 198.51.100.4:5060;branch=z9hG4bKxxx\r\n` +
    `From: <sip:+4930555@sipconnect.sipgate.de>;tag=local\r\n` +
    `To: <sip:+4915123456@sipconnect.sipgate.de>;tag=callee\r\n` +
    `Call-ID: x\r\n` +
    `CSeq: 1 INVITE\r\n` +
    extra +
    'Content-Length: 0\r\n\r\n'
  );
}

const firstLine = (m: string): string => m.split('\r\n')[0];
const sentMethods = (sip: FakeSip): string[] => sip.sent.map((m) => firstLine(m).split(' ')[0]);

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

describe('OutboundDialog', () => {
  it('sends INVITE, reports ringing, ACKs a 2xx and resolves answered', async () => {
    const sip = fakeSip();
    const d = makeDialog(sip);
    const ringing: number[] = [];
    d.onProvisional = (code) => ringing.push(code);

    const p = d.invite(30_000);
    expect(firstLine(sip.sent[0])).toContain('INVITE sip:+4915123456@sipconnect.sipgate.de');
    expect(sip.registered()).toBe(true);

    sip.handlers()?.onResponse?.(resp(100, 'Trying'));
    sip.handlers()?.onResponse?.(resp(180, 'Ringing'));
    const okBody = 'v=0\r\no=callee 0 0 IN IP4 203.0.113.9\r\nm=audio 40000 RTP/AVP 0 101\r\n';
    sip.handlers()?.onResponse?.(
      `SIP/2.0 200 OK\r\nVia: SIP/2.0/UDP 198.51.100.4:5060;branch=b\r\nFrom: <sip:+4930555@x>;tag=local\r\nTo: <sip:+4915123456@x>;tag=callee\r\nCall-ID: x\r\nCSeq: 1 INVITE\r\nContact: <sip:callee@203.0.113.9:5060>\r\nContent-Type: application/sdp\r\nContent-Length: ${Buffer.byteLength(okBody)}\r\n\r\n${okBody}`,
    );

    const result = await p;
    expect(ringing).toEqual([180]);
    expect(result.status).toBe('answered');
    expect(result.answerSdp).toContain('m=audio 40000');
    expect(sentMethods(sip)).toContain('ACK');
    expect(sip.registered()).toBe(true); // stays registered for the callee BYE
  });

  it('maps 486 to busy and ACKs the failure', async () => {
    const sip = fakeSip();
    const d = makeDialog(sip);
    const p = d.invite(30_000);
    sip.handlers()?.onResponse?.(resp(486, 'Busy Here'));
    const result = await p;
    expect(result.status).toBe('busy');
    expect(result.sipCode).toBe(486);
    expect(sentMethods(sip)).toContain('ACK');
    expect(sip.registered()).toBe(false); // unregistered on failure
  });

  it('retries once with Digest auth on 407 then answers', async () => {
    const sip = fakeSip();
    const d = makeDialog(sip);
    const p = d.invite(30_000);
    sip.handlers()?.onResponse?.(
      resp(407, 'Proxy Authentication Required', 'Proxy-Authenticate: Digest realm="sipgate.de", nonce="abc", qop="auth"\r\n'),
    );
    // Second INVITE must carry Proxy-Authorization.
    const invites = sip.sent.filter((m) => firstLine(m).startsWith('INVITE'));
    expect(invites.length).toBe(2);
    expect(invites[1]).toContain('Proxy-Authorization: Digest');
    expect(invites[1]).toContain('response=');

    sip.handlers()?.onResponse?.(resp(200, 'OK', 'Contact: <sip:callee@203.0.113.9:5060>\r\n'));
    const result = await p;
    expect(result.status).toBe('answered');
  });

  it('CANCELs on ring timeout and resolves no-answer', async () => {
    const sip = fakeSip();
    const d = makeDialog(sip);
    const p = d.invite(1_000);
    vi.advanceTimersByTime(1_001);
    expect(sentMethods(sip)).toContain('CANCEL');
    sip.handlers()?.onResponse?.(resp(487, 'Request Terminated'));
    const result = await p;
    expect(result.status).toBe('no-answer');
  });

  it('responds 200 to the callee BYE and fires onRemoteBye after answer', async () => {
    const sip = fakeSip();
    const d = makeDialog(sip);
    let remoteBye = false;
    d.onRemoteBye = () => {
      remoteBye = true;
    };
    const p = d.invite(30_000);
    sip.handlers()?.onResponse?.(resp(200, 'OK', 'Contact: <sip:callee@203.0.113.9:5060>\r\n'));
    await p;

    const byeRaw =
      'BYE sip:user@198.51.100.4:5060 SIP/2.0\r\nVia: SIP/2.0/UDP 203.0.113.9:5060;branch=z\r\n' +
      'From: <sip:+4915123456@x>;tag=callee\r\nTo: <sip:+4930555@x>;tag=local\r\nCall-ID: x\r\nCSeq: 2 BYE\r\nContent-Length: 0\r\n\r\n';
    sip.handlers()?.onRequest?.('BYE', byeRaw, rinfo);

    expect(remoteBye).toBe(true);
    expect(sip.registered()).toBe(false);
    const last = sip.sent[sip.sent.length - 1];
    expect(firstLine(last)).toBe('SIP/2.0 200 OK'); // 200 to the BYE
  });

  it('bye() sends an in-dialog BYE and resolves on 200', async () => {
    const sip = fakeSip();
    const d = makeDialog(sip);
    const p = d.invite(30_000);
    sip.handlers()?.onResponse?.(resp(200, 'OK', 'Contact: <sip:callee@203.0.113.9:5060>\r\n'));
    await p;

    const byePromise = d.bye();
    const byeMsg = sip.sent.find((m) => firstLine(m).startsWith('BYE'));
    expect(byeMsg).toBeDefined();
    sip.handlers()?.onResponse?.('SIP/2.0 200 OK\r\nCSeq: 2 BYE\r\nCall-ID: x\r\nContent-Length: 0\r\n\r\n');
    await byePromise;
    expect(sip.registered()).toBe(false);
  });
});
