/**
 * Toll-fraud guardrails for outbound <Dial> — port of go/internal/sip/guardrails.go.
 *
 * Enforces a default-deny allow-list plus per-session and global rolling-minute
 * rate limits. Checked from the SIP forwarder BEFORE the outbound INVITE is
 * constructed, so blocking happens even if the TwiML parser is bypassed.
 *
 * Pure in-process state — no I/O. The clock is injectable so the rolling-minute
 * window can be exercised deterministically in tests.
 */

/**
 * Rejection reason. Mirrors the Go typed errors and maps to Twilio codes /
 * metric labels in the forwarder:
 *
 *   toll_fraud  → not in allow-list      → Twilio 21215
 *   rate_limit  → session or global cap  → Twilio 21220
 */
export enum GuardrailReason {
  TollFraud = 'toll_fraud',
  RateLimit = 'rate_limit',
}

/** Error thrown by {@link Guardrails.checkDial} when a dial attempt is rejected. */
export class GuardrailError extends Error {
  readonly reason: GuardrailReason;

  constructor(reason: GuardrailReason, message: string) {
    super(message);
    this.name = 'GuardrailError';
    this.reason = reason;
  }
}

export interface GuardrailsOptions {
  /**
   * Allow-list of normalized E.164 prefixes (from DIAL_ALLOWED_PREFIXES CSV).
   * EMPTY = DENY ALL (default-deny). Entries are lower-cased + trimmed here to
   * match config.Load normalization in the Go implementation.
   */
  allowedPrefixes?: string[];
  /** Max successful dials per caller session. Default 3. */
  maxPerSession?: number;
  /** Max successful dials globally per rolling 60s window. Default 60. */
  maxPerMinute?: number;
  /** Injectable clock in milliseconds (defaults to Date.now). */
  now?: () => number;
}

/**
 * Enforces toll-fraud allow-list + rate limits.
 *
 * On a successful {@link checkDial} BOTH the per-session counter and the global
 * rolling-minute counter are incremented; if the global gate then fails the
 * session counter is rolled back, mirroring the Go semantics exactly.
 */
export class Guardrails {
  private readonly allowedPrefixes: string[];
  private readonly maxPerSession: number;
  private readonly maxPerMinute: number;
  private readonly now: () => number;

  /** perSession[callerSid] = successful-dial count; cleared via onSessionEnd. */
  private readonly perSession = new Map<string, number>();

  /**
   * 60 one-second buckets forming a rolling-minute sum. bucketSecs[i] is the
   * Unix-second timestamp the slot was last written; slots older than now-59
   * are excluded from the rolling sum.
   */
  private readonly bucketCounts = new Array<number>(60).fill(0);
  private readonly bucketSecs = new Array<number>(60).fill(0);

  constructor(options: GuardrailsOptions = {}) {
    this.allowedPrefixes = (options.allowedPrefixes ?? [])
      .map((p) => p.trim().toLowerCase())
      .filter((p) => p !== '');
    this.maxPerSession = options.maxPerSession ?? 3;
    this.maxPerMinute = options.maxPerMinute ?? 60;
    this.now = options.now ?? Date.now;
  }

  /**
   * Validate a dial attempt. Throws a {@link GuardrailError} on rejection;
   * returns normally on success. On success, increments BOTH the per-session
   * counter AND the global rolling-minute counter. Call exactly once per
   * <Dial> attempt, BEFORE constructing the outbound INVITE.
   */
  checkDial(callerSid: string, target: string): void {
    const normalized = normalizeTarget(target);
    if (!this.matchAllowList(normalized)) {
      throw new GuardrailError(
        GuardrailReason.TollFraud,
        `guardrails: dial target not in allow-list (toll-fraud defense): target=${maskTarget(normalized)}`,
      );
    }

    // Per-session counter (increment first, roll back on later failure).
    const sessCount = (this.perSession.get(callerSid) ?? 0) + 1;
    this.perSession.set(callerSid, sessCount);
    if (sessCount > this.maxPerSession) {
      this.perSession.set(callerSid, sessCount - 1); // rollback
      throw new GuardrailError(
        GuardrailReason.RateLimit,
        'guardrails: per-session dial limit reached',
      );
    }

    // Global rolling-minute counter.
    if (!this.checkAndIncrementGlobal()) {
      this.perSession.set(callerSid, sessCount - 1); // rollback session count
      throw new GuardrailError(
        GuardrailReason.RateLimit,
        'guardrails: global per-minute dial limit reached',
      );
    }
  }

  /**
   * Clears per-session state for a callerSid. Called from the call-session
   * terminate path. Matches Go's OnSessionEnd: deletes the counter (does not
   * decrement).
   */
  onSessionEnd(callerSid: string): void {
    this.perSession.delete(callerSid);
  }

  /**
   * Returns true if the normalized target starts with any configured prefix.
   * Empty prefix list = default-deny (returns false unconditionally).
   */
  private matchAllowList(target: string): boolean {
    if (this.allowedPrefixes.length === 0) {
      return false; // default-deny
    }
    return this.allowedPrefixes.some((p) => target.startsWith(p));
  }

  /**
   * Returns true if under the rolling-minute limit and increments the current
   * second's bucket; false if at the limit.
   */
  private checkAndIncrementGlobal(): boolean {
    const now = Math.floor(this.now() / 1000); // Unix seconds
    const idx = ((now % 60) + 60) % 60; // non-negative modulo

    // If this slot is from a previous minute, reset it.
    if (this.bucketSecs[idx] !== now) {
      this.bucketCounts[idx] = 0;
      this.bucketSecs[idx] = now;
    }

    // Sum all slots whose timestamp is within the last 60 seconds.
    let sum = 0;
    for (let i = 0; i < 60; i++) {
      if (now - this.bucketSecs[i] < 60) {
        sum += this.bucketCounts[i];
      }
    }
    if (sum >= this.maxPerMinute) {
      return false;
    }
    this.bucketCounts[idx]++;
    return true;
  }
}

/**
 * Strips whitespace + scheme prefix and converts a leading "00" to "+".
 * Lower-cased so the allow-list match is case-insensitive against any
 * scheme/host fragments. Mirrors normalizeTarget in guardrails.go exactly:
 * the sip: scheme strip leaves any `user@host` part intact (no @domain strip).
 */
export function normalizeTarget(target: string): string {
  let t = target.trim().toLowerCase();
  t = stripPrefix(t, 'tel:');
  t = stripPrefix(t, 'sip:');
  // Remove any internal whitespace (operators sometimes paste "+49 30 555").
  t = t.replace(/[ \t]/g, '');
  if (t.startsWith('00')) {
    t = '+' + t.slice(2);
  }
  return t;
}

function stripPrefix(s: string, prefix: string): string {
  return s.startsWith(prefix) ? s.slice(prefix.length) : s;
}

/**
 * Returns a logging-safe form of a phone number (last 4 chars visible) so phone
 * numbers are not leaked in logs/error messages.
 */
export function maskTarget(target: string): string {
  if (target.length <= 4) {
    return '*'.repeat(target.length);
  }
  return '*'.repeat(target.length - 4) + target.slice(target.length - 4);
}
