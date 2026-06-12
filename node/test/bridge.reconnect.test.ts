/**
 * Regression test for the WS-reconnect privacy invariant.
 *
 * A real-trunk test showed that the privacy gate's ws.stop() fired the WS
 * reconnect loop (the close looked like a transient drop), rejoining the bot to
 * a call that was being forwarded. Reconnect must be refused unless the session
 * is still registered AND actively streaming.
 */
import { describe, expect, it } from 'vitest';

import { shouldReconnectWs } from '../src/bridge/callManager.js';
import { CallState } from '../src/bridge/state.js';

describe('shouldReconnectWs', () => {
  it('allows reconnect only while streaming and present', () => {
    expect(shouldReconnectWs(CallState.Streaming, true)).toBe(true);
  });

  it('refuses reconnect during the dial privacy gate and forwarding', () => {
    expect(shouldReconnectWs(CallState.DialingOut, true)).toBe(false);
    expect(shouldReconnectWs(CallState.Forwarding, true)).toBe(false);
  });

  it('refuses reconnect once terminated/hung up', () => {
    expect(shouldReconnectWs(CallState.Terminated, true)).toBe(false);
    expect(shouldReconnectWs(CallState.HungUp, true)).toBe(false);
  });

  it('refuses reconnect when the session is gone, even if state says streaming', () => {
    expect(shouldReconnectWs(CallState.Streaming, false)).toBe(false);
  });
});
