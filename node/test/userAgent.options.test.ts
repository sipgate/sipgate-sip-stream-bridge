import { describe, it } from 'vitest';
// Import will fail until Plan 03 exports applyOptionsResponse
// import { applyOptionsResponse } from '../src/sip/userAgent.js';

describe('applyOptionsResponse — OPTN-01/02/03', () => {
  it.todo('200 OK resets consecutive failures, no re-register');
  it.todo('1st failure increments counter, no re-register');
  it.todo('2nd consecutive failure triggers re-register, resets counter');
  it.todo('401 resets counter, no re-register (server alive)');
  it.todo('407 resets counter, no re-register (server alive)');
  it.todo('404 counts as failure');
  it.todo('500 counts as failure');
});
