import { describe, it } from 'vitest';
// These imports will fail until Plan 02 exports these — tests are intentionally RED
// import { makeDrainForTest } from '../src/ws/wsClient.js';

describe('outboundDrain mark sentinel — MRKN-01', () => {
  it.todo('dequeues mark sentinel and calls onMarkReady after preceding audio frames');
  it.todo('empty-queue fast-path: calls onMarkReady immediately when queue idle at mark arrival');
});

describe('outboundDrain sendClear — MRKN-02', () => {
  it.todo('stop() echoes all pending mark sentinels before flushing queue');
});

describe('WsClient.sendMark — MRKN-03', () => {
  it.todo('sends correct Twilio mark event JSON: { event: "mark", mark: { name } }');
});
