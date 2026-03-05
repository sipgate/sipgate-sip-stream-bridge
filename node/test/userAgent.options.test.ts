import { describe, expect, it } from 'vitest';
import { applyOptionsResponse } from '../src/sip/userAgent.js';

describe('applyOptionsResponse — OPTN-01/02/03', () => {
  it('200 OK resets consecutive failures, no re-register', () => {
    expect(applyOptionsResponse(0, 200, false)).toEqual({ newCount: 0, triggerRegister: false });
  });

  it('1st failure increments counter, no re-register', () => {
    expect(applyOptionsResponse(0, 503, false)).toEqual({ newCount: 1, triggerRegister: false });
  });

  it('2nd consecutive failure triggers re-register, resets counter', () => {
    expect(applyOptionsResponse(1, 503, false)).toEqual({ newCount: 0, triggerRegister: true });
  });

  it('timeout (isError) triggers re-register on 2nd failure', () => {
    expect(applyOptionsResponse(1, 0, true)).toEqual({ newCount: 0, triggerRegister: true });
  });

  it('401 resets counter, no re-register (server alive)', () => {
    expect(applyOptionsResponse(2, 401, false)).toEqual({ newCount: 0, triggerRegister: false });
  });

  it('407 resets counter, no re-register (server alive)', () => {
    expect(applyOptionsResponse(0, 407, false)).toEqual({ newCount: 0, triggerRegister: false });
  });

  it('404 counts as failure', () => {
    expect(applyOptionsResponse(0, 404, false)).toEqual({ newCount: 1, triggerRegister: false });
  });

  it('500 counts as failure', () => {
    expect(applyOptionsResponse(0, 500, false)).toEqual({ newCount: 1, triggerRegister: false });
  });
});
