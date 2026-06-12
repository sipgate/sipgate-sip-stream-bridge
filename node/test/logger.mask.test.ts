import { Writable } from 'node:stream';
import { describe, it, expect } from 'vitest';
import { newSecretMaskWriter, maskSecrets, Field, createMaskedLogger } from '../src/logger/index.js';

/** A Writable that accumulates everything written into a string buffer. */
class BufferSink extends Writable {
  data = '';
  override _write(chunk: Buffer | string, _enc: BufferEncoding, cb: (e?: Error | null) => void): void {
    this.data += typeof chunk === 'string' ? chunk : chunk.toString('utf8');
    cb();
  }
}

describe('newSecretMaskWriter — passthrough when no secrets', () => {
  it('returns the underlying writer unchanged (pointer identity)', () => {
    const sink = new BufferSink();
    const w = newSecretMaskWriter(sink);
    expect(w).toBe(sink);
  });

  it('filters empty secrets and still redacts real ones', () => {
    const sink = new BufferSink();
    const w = newSecretMaskWriter(sink, '', 'real_token', '');
    expect(w).not.toBe(sink);
    w.write('{"t":"real_token"} hello');
    expect(sink.data).not.toContain('real_token');
    expect(sink.data).toContain(' hello');
    expect((sink.data.match(/\*\*\*/g) ?? []).length).toBe(1);
  });
});

describe('newSecretMaskWriter — redaction', () => {
  it('replaces a secret in a log line with *** regardless of field', () => {
    const sink = new BufferSink();
    const secret = 'ABCDEFGHsupersecret';
    const w = newSecretMaskWriter(sink, secret);
    w.write(`{"level":"info","msg":"token=${secret} embedded"}\n`);
    w.write(`{"level":"info","auth_token":"${secret}","msg":"oops"}\n`);
    w.write(`{"level":"info","random_field":"${secret}","msg":"more oops"}\n`);
    expect(sink.data).not.toContain(secret);
    expect((sink.data.match(/\*\*\*/g) ?? []).length).toBe(3);
  });

  it('masks the longer secret first (longest-first ordering)', () => {
    const sink = new BufferSink();
    const w = newSecretMaskWriter(sink, 'PASSWORD', 'PASSWORD123');
    w.write('{"k":"PASSWORD123"}');
    expect(sink.data).not.toContain('PASSWORD');
    expect(sink.data).not.toContain('123');
    expect((sink.data.match(/\*\*\*/g) ?? []).length).toBe(1);
  });
});

describe('maskSecrets helper', () => {
  it('passes through when no secrets', () => {
    expect(maskSecrets('hello', [])).toBe('hello');
    expect(maskSecrets('hello', [''])).toBe('hello');
  });
  it('honors longest-first ordering', () => {
    expect(maskSecrets('PASSWORD123', ['PASSWORD', 'PASSWORD123'])).toBe('***');
  });
});

describe('Field constants', () => {
  it('exposes the Go correlation field names, with call_sid distinct from call_id', () => {
    expect(Field.CallSid).toBe('call_sid');
    expect(Field.AccountSid).toBe('account_sid');
    expect(Field.SIPCallID).toBe('call_id');
    expect(Field.ForwardLegID).toBe('forward_leg_id');
    expect(Field.CallSid).not.toBe(Field.SIPCallID);
  });
});

describe('createMaskedLogger — end to end', () => {
  it('redacts secrets emitted by a pino logger', () => {
    const sink = new BufferSink();
    const authToken = 'TEST_AUTH_TOKEN_LITERAL';
    const sipPw = 'TEST_SIP_PASSWORD_LITERAL';
    const logger = createMaskedLogger([authToken, sipPw], sink);
    logger.info({ auth_token: authToken, data: sipPw }, `emitting token=${authToken} pw=${sipPw}`);
    expect(sink.data).not.toContain(authToken);
    expect(sink.data).not.toContain(sipPw);
    expect((sink.data.match(/\*\*\*/g) ?? []).length).toBeGreaterThanOrEqual(4);
  });
});
