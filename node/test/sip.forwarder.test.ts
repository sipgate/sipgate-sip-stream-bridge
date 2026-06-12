import { EventEmitter } from 'node:events';

import { describe, expect, it, vi } from 'vitest';

import type { CallManager, CallSession } from '../src/bridge/callManager.js';
import { CallState } from '../src/bridge/state.js';
import type { Config } from '../src/config/index.js';
import { Forwarder } from '../src/sip/forwarder.js';
import { Guardrails } from '../src/sip/guardrails.js';
import type { DialogHandlers, SipHandle } from '../src/sip/userAgent.js';
import type { RtpHandler } from '../src/rtp/rtpHandler.js';
import { MidCallAdapter } from '../src/api/midcallAdapter.js';
import type { DialOpts } from '../src/twiml/dispatch.js';

const log = { info: () => {}, debug: () => {}, warn: () => {}, error: () => {} } as never;
const tick = (): Promise<void> => new Promise((r) => setTimeout(r, 0));

function fakeSip(): { handle: SipHandle; sent: string[]; handlers: () => DialogHandlers | null } {
  const sent: string[] = [];
  let handlers: DialogHandlers | null = null;
  const handle: SipHandle = {
    stop: () => {},
    unregister: () => Promise.resolve(),
    sendRaw: (buf: Buffer) => sent.push(buf.toString()),
    registerDialog: (_c: string, h: DialogHandlers) => {
      handlers = h;
    },
    unregisterDialog: () => {},
  };
  return { handle, sent, handlers: () => handlers };
}

function fakeRtp(port: number): RtpHandler {
  const e = new EventEmitter() as unknown as RtpHandler & EventEmitter;
  Object.assign(e, {
    localPort: port,
    silenceByte: 0xff,
    setRemote: vi.fn(),
    startForwarding: vi.fn(),
    sendAudio: vi.fn(),
    dispose: vi.fn(),
  });
  return e as RtpHandler;
}

const config = {
  SIP_USER: 'e12345p0',
  SIP_PASSWORD: 'secret',
  SIP_DOMAIN: 'sipconnect.sipgate.de',
  SIP_REGISTRAR: '203.0.113.9',
  SIP_OUTBOUND_TARGET_PORT: 0,
  SDP_CONTACT_IP: '198.51.100.4',
  RTP_PORT_MIN: 10000,
  RTP_PORT_MAX: 10099,
} as unknown as Config;

const opts: DialOpts = {
  timeoutMs: 30_000,
  timeLimitMs: 0,
  hangupOnStar: false,
  method: 'POST',
  statusCallbackMethod: 'POST',
};

function makeSession(callerRtp: RtpHandler): CallSession {
  return {
    callSid: 'CA' + 'b'.repeat(32),
    accountSid: 'AC' + 'a'.repeat(32),
    fromNumber: 'sip:+4930111@sipconnect.sipgate.de',
    rtp: callerRtp,
    forwardRtp: null,
    forwardDialog: null,
    onForwardEnd: null,
    hangupOnStar: false,
    state: CallState.Streaming,
    log,
  } as unknown as CallSession;
}

function answer200(): string {
  const body = 'v=0\r\no=c 0 0 IN IP4 203.0.113.9\r\nc=IN IP4 203.0.113.9\r\nm=audio 40000 RTP/AVP 0 101\r\na=rtpmap:0 PCMU/8000\r\n';
  return (
    'SIP/2.0 200 OK\r\nVia: SIP/2.0/UDP 198.51.100.4:5060;branch=b\r\n' +
    'From: <sip:+4930111@x>;tag=local\r\nTo: <sip:+4915123456@x>;tag=callee\r\n' +
    `Call-ID: x\r\nCSeq: 1 INVITE\r\nContact: <sip:callee@203.0.113.9:5060>\r\n` +
    `Content-Type: application/sdp\r\nContent-Length: ${Buffer.byteLength(body)}\r\n\r\n${body}`
  );
}

describe('Forwarder', () => {
  it('blocks a non-allow-listed target before any signalling', async () => {
    const sip = fakeSip();
    const guardrails = new Guardrails({ allowedPrefixes: ['+49'], maxPerSession: 3, maxPerMinute: 60 });
    const fwd = new Forwarder({ config, sipHandle: sip.handle, guardrails, log });
    const outcome = await fwd.run(makeSession(fakeRtp(10000)), '+15551234567', opts, fakeRtp(10002));
    expect(outcome.status).toBe('failed');
    expect(outcome.reason).toBe('toll_fraud');
    expect(sip.sent.length).toBe(0); // no INVITE was sent
  });

  it('dials, answers, wires the dual-leg relay, and ends on callee BYE', async () => {
    const sip = fakeSip();
    const guardrails = new Guardrails({ allowedPrefixes: ['+49'], maxPerSession: 3, maxPerMinute: 60 });
    const fwd = new Forwarder({ config, sipHandle: sip.handle, guardrails, log });
    const callerRtp = fakeRtp(10000);
    const calleeRtp = fakeRtp(10002);
    const session = makeSession(callerRtp);

    const p = fwd.run(session, '+4915123456', opts, calleeRtp);
    await tick(); // let run() reach dialog.invite() and send the INVITE

    const invite = sip.sent.find((m) => m.startsWith('INVITE')) ?? '';
    expect(invite).toBeTruthy();
    // Caller-ID chain: From addr-spec is the SIP_USER (trunk auth identity)…
    expect(invite).toContain('<sip:e12345p0@sipconnect.sipgate.de>');
    // …while the inbound ANI is preserved in P-Preferred-Identity (normalised).
    expect(invite).toContain('P-Preferred-Identity: <sip:4930111@sipconnect.sipgate.de>');
    expect(invite).toContain('Content-Type: application/sdp');

    // Callee answers with a PCMU SDP.
    sip.handlers()?.onResponse?.(answer200());
    await tick();

    // Relay wired: callee leg pointed at the answer's RTP, forwarding enabled.
    expect((calleeRtp.setRemote as ReturnType<typeof vi.fn>)).toHaveBeenCalledWith('203.0.113.9', 40000);
    expect((calleeRtp.startForwarding as ReturnType<typeof vi.fn>)).toHaveBeenCalled();
    expect(session.forwardRtp).toBe(calleeRtp);
    expect(session.forwardDialog).not.toBeNull();
    expect(session.state).toBe(CallState.Forwarding);

    // Callee→caller relay: audio from the callee leg is sent to the caller leg.
    calleeRtp.emit('audio', Buffer.from([0xff, 0xff]));
    expect((callerRtp.sendAudio as ReturnType<typeof vi.fn>)).toHaveBeenCalled();

    // Callee hangs up → run() resolves answered.
    const byeRaw =
      'BYE sip:user@198.51.100.4:5060 SIP/2.0\r\nVia: SIP/2.0/UDP 203.0.113.9:5060;branch=z\r\n' +
      'From: <sip:+4915123456@x>;tag=callee\r\nTo: <sip:+4930111@x>;tag=local\r\nCall-ID: x\r\nCSeq: 2 BYE\r\nContent-Length: 0\r\n\r\n';
    sip.handlers()?.onRequest?.('BYE', byeRaw, { address: '203.0.113.9', port: 5060, family: 'IPv4', size: 0 });

    const outcome = await p;
    expect(outcome.status).toBe('answered');
    expect(outcome.dialCallSid).toMatch(/^CA[0-9a-f]{32}$/);
    expect(outcome.durationMs).toBeGreaterThanOrEqual(0);
  });
});

describe('MidCallAdapter privacy gate', () => {
  it('closes the WS stream BEFORE allocating the callee leg', async () => {
    const order: string[] = [];
    const calleeRtp = fakeRtp(10004);
    const manager = {
      closeStream: () => order.push('closeStream'),
    } as unknown as CallManager;
    const forwarder = {
      createCalleeLeg: async () => {
        order.push('createCalleeLeg');
        return calleeRtp;
      },
    } as unknown as Forwarder;
    const session = makeSession(fakeRtp(10000));

    const adapter = new MidCallAdapter(session, manager, forwarder, log);
    const handle = await adapter.prepareDial(opts);
    expect(order).toEqual(['closeStream', 'createCalleeLeg']);
    handle.release();
    expect((calleeRtp.dispose as ReturnType<typeof vi.fn>)).toHaveBeenCalled();
  });
});
