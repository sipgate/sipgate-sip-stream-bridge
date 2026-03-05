import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { makeDrainForTest, createWsClient } from '../src/ws/wsClient.js';
import { WebSocket } from 'ws';

describe('outboundDrain mark sentinel — MRKN-01', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('dequeues mark sentinel and calls onMarkReady after preceding audio frames', async () => {
    const sent: Buffer[] = [];
    const marked: string[] = [];
    const drain = makeDrainForTest(
      (pkt) => sent.push(pkt),
      (name) => marked.push(name),
    );

    // Enqueue 2 audio buffers then a mark sentinel
    drain.enqueue(Buffer.alloc(160, 0x01));
    drain.enqueue(Buffer.alloc(160, 0x02));
    drain.enqueueMark('hello');

    // Mark should not be called yet (audio frames still pending)
    expect(marked).toHaveLength(0);

    // Advance timers to drain 2 audio frames
    await vi.advanceTimersByTimeAsync(40);

    // After both audio frames have fired, the mark should have been called
    expect(sent).toHaveLength(2);
    expect(marked).toEqual(['hello']);
  });

  it('empty-queue fast-path: calls onMarkReady immediately when queue idle at mark arrival', () => {
    const marked: string[] = [];
    const drain = makeDrainForTest(
      (_pkt) => {},
      (name) => marked.push(name),
    );

    // Queue is idle — mark should be echoed synchronously
    drain.enqueueMark('fast');

    expect(marked).toEqual(['fast']);
  });
});

describe('outboundDrain sendClear — MRKN-02', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('stop() echoes all pending mark sentinels before flushing queue', () => {
    const marked: string[] = [];
    const drain = makeDrainForTest(
      (_pkt) => {},
      (name) => marked.push(name),
    );

    // Enqueue 1 audio buffer and 2 mark sentinels
    drain.enqueue(Buffer.alloc(160, 0x01));
    drain.enqueueMark('m1');
    drain.enqueueMark('m2');

    // call stop() — should echo both marks and flush the queue
    drain.stop();

    expect(marked).toEqual(['m1', 'm2']);
  });
});

describe('WsClient.sendMark — MRKN-03', () => {
  it('sends correct Twilio mark event JSON: { event: "mark", mark: { name } }', async () => {
    // Create a minimal mock WebSocket server
    const { WebSocketServer } = await import('ws');
    const wss = new WebSocketServer({ port: 0 });
    const port = (wss.address() as { port: number }).port;

    const receivedMessages: string[] = [];
    wss.on('connection', (socket) => {
      socket.on('message', (data) => {
        receivedMessages.push(data.toString());
      });
    });

    const log = {
      info: () => {},
      warn: () => {},
      error: () => {},
      debug: () => {},
      child: () => log,
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any;

    const ws = await createWsClient(
      `ws://127.0.0.1:${port}`,
      {
        streamSid: 'MZtest',
        callSid: 'CAtest',
        from: 'sip:alice@example.com',
        to: 'sip:bob@example.com',
        sipCallId: 'test-call-id',
      },
      log,
    );

    // Register onAudio so the message handler is attached
    ws.onAudio(() => {});

    // Send a mark
    ws.sendMark('test-mark');

    // Allow the event loop to process the send
    await new Promise((res) => setTimeout(res, 10));

    wss.close();

    // Find the mark event message (skip connected + start events)
    const markMsg = receivedMessages
      .map((m) => JSON.parse(m) as { event: string; mark?: { name: string }; sequenceNumber?: string })
      .find((m) => m.event === 'mark');

    expect(markMsg).toBeDefined();
    expect(markMsg?.mark?.name).toBe('test-mark');
    expect(markMsg?.sequenceNumber).toMatch(/^\d+$/);
  });
});
