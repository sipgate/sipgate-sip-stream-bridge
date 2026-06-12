import { Writable } from 'node:stream';
import pino, { type Logger } from 'pino';

/**
 * Correlation field-name constants — port of go/internal/observability/logging.go.
 *
 * The grep-enforceable correlation field names every per-session sub-logger
 * MUST use. FieldSIPCallID ("call_id") is INTENTIONALLY DISTINCT from
 * FieldCallSid ("call_sid"): Twilio CallSid vs the on-the-wire SIP Call-ID
 * header are different identifiers.
 */
export const Field = {
  CallSid: 'call_sid',
  AccountSid: 'account_sid',
  SIPCallID: 'call_id',
  ForwardLegID: 'forward_leg_id',
} as const;

const MASK_REPLACEMENT = '***';

/**
 * Build the list of non-empty secrets sorted by descending length (longest
 * first), so masking "PASSWORD" inside "PASSWORD123" cannot leak the suffix.
 * Returns null when no non-empty secrets are configured (zero-cost passthrough).
 */
function prepareSecrets(secrets: readonly string[]): string[] | null {
  const nonempty = secrets.filter((s) => s !== '');
  if (nonempty.length === 0) return null;
  // Stable sort by descending length so the longest secret is matched first.
  nonempty.sort((a, b) => b.length - a.length);
  return nonempty;
}

/** Redact every occurrence of every secret in `chunk`. */
function maskChunk(chunk: string, secrets: readonly string[]): string {
  let masked = chunk;
  for (const s of secrets) {
    masked = masked.split(s).join(MASK_REPLACEMENT);
  }
  return masked;
}

/**
 * NewSecretMaskWriter — port of Go's NewSecretMaskWriter (logging.go).
 *
 * Wraps a destination Writable and redacts every occurrence of any configured
 * secret in the byte stream before forwarding. Empty secrets are filtered.
 *
 * Zero-cost passthrough: when no non-empty secrets are configured the helper
 * returns the underlying writer unchanged (pointer-identity preserved), exactly
 * like the Go contract.
 *
 * Redaction is field-name agnostic by construction: it scans the serialized
 * JSON byte stream after pino emits it, so a secret leaks through no matter
 * which field name (or message body) it appears under.
 */
export function newSecretMaskWriter(underlying: Writable, ...secrets: string[]): Writable {
  const prepared = prepareSecrets(secrets);
  if (prepared === null) {
    return underlying;
  }
  return new Writable({
    write(chunk: Buffer | string, _encoding: BufferEncoding, callback: (error?: Error | null) => void): void {
      const text = typeof chunk === 'string' ? chunk : chunk.toString('utf8');
      // Forward synchronously, then signal completion immediately. We do NOT
      // chain `callback` onto the underlying write: doing so couples our
      // stream's queue to the underlying writer's drain cycle, which can defer
      // subsequent writes to a later tick. The mask is a pure byte transform,
      // so completion is immediate from our perspective.
      try {
        underlying.write(maskChunk(text, prepared));
      } catch (err) {
        callback(err instanceof Error ? err : new Error(String(err)));
        return;
      }
      callback();
    },
  });
}

/**
 * Mask a string in place (exposed for callers that need to redact a value
 * before logging it through a non-wrapped sink). Honors longest-first order.
 */
export function maskSecrets(input: string, secrets: readonly string[]): string {
  const prepared = prepareSecrets(secrets);
  if (prepared === null) return input;
  return maskChunk(input, prepared);
}

export const rootLogger: Logger = pino({
  level: process.env.LOG_LEVEL ?? 'info',
  base: { service: 'sipgate-sip-stream-bridge' },
});

/**
 * Build a root logger whose output stream redacts the configured secrets
 * (typically AUTH_TOKEN and SIP_PASSWORD). Sub-loggers derived via .child()
 * inherit the wrapped destination, so every per-session sub-logger benefits
 * from the mask without further wiring.
 *
 * Mirrors the Go main.go construction:
 *   logger := zerolog.New(NewSecretMaskWriter(os.Stdout, cfg.AuthToken, cfg.SIPPassword))...
 */
export function createMaskedLogger(secrets: string[], dest: Writable = process.stdout): Logger {
  const sink = newSecretMaskWriter(dest, ...secrets);
  return pino(
    {
      level: process.env.LOG_LEVEL ?? 'info',
      base: { service: 'sipgate-sip-stream-bridge' },
    },
    sink,
  );
}

export function createChildLogger(bindings: {
  component: string;
  callId?: string;
  streamSid?: string;
}): Logger {
  return rootLogger.child(bindings);
}
