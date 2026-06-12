/**
 * Caller-ID resolution for the outbound `<Dial>` B2BUA.
 *
 * Pure functions, no I/O — a direct port of the Go ground truth in
 * `go/internal/sip/forwarder.go` (`resolveCallerID`, `resolveDisplayCallerID`,
 * `normaliseTrunkCallerID`, `isFromRejectionReason`).
 *
 * Two distinct caller-ID values come out of `<Dial>`:
 *
 *  - the From URI user-part we send to the trunk (auth-relevant; some carriers
 *    such as sipgate require the registered SIP username here) — see
 *    {@link resolveCallerID};
 *  - the number the callee actually sees (display / P-Asserted-Identity) — see
 *    {@link resolveDisplayCallerID}.
 *
 * Keeping them separate lets us satisfy a trunk's From-auth policy while still
 * surfacing a meaningful caller-ID to the callee (Twilio-style preserve-ANI).
 */

/**
 * Thrown by {@link resolveCallerID} when the caller-ID fallback chain is
 * exhausted (every source empty). Mirrors Go's `ErrCallerIDRequired`; the
 * message carries Twilio error code 13214 for parity with Twilio's REST API.
 */
export class CallerIdRequiredError extends Error {
  constructor(message = 'forwarder: caller-id required (13214)') {
    super(message);
    this.name = 'CallerIdRequiredError';
  }
}

/** Inputs for {@link resolveCallerID}. */
export interface ResolveCallerIdOptions {
  /** TwiML `<Dial callerId="...">` attribute (explicit operator intent). */
  twimlCallerId?: string;
  /** `DIAL_DEFAULT_CALLER_ID` env — operator-wide configured default. */
  dialDefaultCallerId?: string;
  /** Registered SIP authentication username (`SIP_USER`). */
  sipUser: string;
  /** Inbound caller's From URI ("preserve-ANI"). */
  callerFrom?: string;
}

/** Inputs for {@link resolveDisplayCallerID}. */
export interface ResolveDisplayCallerIdOptions {
  /** TwiML `<Dial callerId="...">` attribute. */
  twimlCallerId?: string;
  /** `DIAL_DEFAULT_CALLER_ID` env — operator-wide configured default. */
  dialDefaultCallerId?: string;
  /** Inbound caller's From URI ("preserve-ANI"). */
  callerFrom?: string;
}

/**
 * Resolve the caller-ID to place in the outbound From URI, applying the
 * fallback chain (highest precedence first):
 *
 *  1. TwiML `<Dial callerId="...">` — explicit operator intent, always wins.
 *  2. `DIAL_DEFAULT_CALLER_ID` — operator-wide configuration.
 *  3. `SIP_USER` — the registered SIP auth username, required in From by some
 *     trunks (e.g. sipgate's "Username in From Field required" 403 policy).
 *  4. inbound From URI (`callerFrom`, preserve-ANI) — last-resort fallback for
 *     the rare case where `SIP_USER` is unset; preserves Twilio-style behaviour
 *     on trunks that accept third-party ANI.
 *  5. otherwise throws {@link CallerIdRequiredError} (Twilio code 13214).
 *
 * Each candidate is whitespace-trimmed; an all-whitespace value does not count
 * as present and falls through to the next source. The returned `callerId` is
 * the trimmed value of the first non-empty source.
 *
 * @throws {CallerIdRequiredError} when all sources are empty.
 */
export function resolveCallerID(opts: ResolveCallerIdOptions): { callerId: string } {
  const twiml = (opts.twimlCallerId ?? '').trim();
  if (twiml !== '') {
    return { callerId: twiml };
  }
  const defaultCid = (opts.dialDefaultCallerId ?? '').trim();
  if (defaultCid !== '') {
    return { callerId: defaultCid };
  }
  const sipUser = opts.sipUser.trim();
  if (sipUser !== '') {
    return { callerId: sipUser };
  }
  const callerFrom = (opts.callerFrom ?? '').trim();
  if (callerFrom !== '') {
    return { callerId: callerFrom };
  }
  throw new CallerIdRequiredError();
}

/**
 * Resolve the caller-ID that should be PRESENTED to the callee (the number
 * Phone B sees), independent of the From URI used for trunk auth. Used to build
 * the From display-name and the P-Preferred-Identity header (RFC 3325 §9.2).
 *
 * Precedence (highest first):
 *
 *  1. TwiML `<Dial callerId="...">` — Twilio-standard explicit override.
 *  2. `DIAL_DEFAULT_CALLER_ID` — operator-wide default.
 *  3. inbound From URI (`callerFrom`, preserve-ANI) — Twilio default.
 *  4. `""` — caller did not specify and the inbound From was missing; the
 *     Forwarder treats the empty string as "skip the P-Asserted-Identity
 *     header" and lets the trunk present whatever it presents.
 *
 * Note: unlike {@link resolveCallerID} there is NO `SIP_USER` step here — the
 * SIP username is an auth artifact, never something to show the callee.
 */
export function resolveDisplayCallerID(opts: ResolveDisplayCallerIdOptions): string {
  const twiml = (opts.twimlCallerId ?? '').trim();
  if (twiml !== '') {
    return twiml;
  }
  const defaultCid = (opts.dialDefaultCallerId ?? '').trim();
  if (defaultCid !== '') {
    return defaultCid;
  }
  const callerFrom = (opts.callerFrom ?? '').trim();
  if (callerFrom !== '') {
    return callerFrom;
  }
  return '';
}

/**
 * Convert a phone-number string into the format sipgate's trunking
 * documentation requires for the From display-name and the
 * P-Preferred-Identity user-part: international format without a `+` or `00`
 * prefix and without a leading `0` on the area code.
 *
 * Transformation rules (applied in order):
 *
 *  1. Trim whitespace and strip a single leading `+`.
 *  2. Strip a leading `00` (international-prefix dialling).
 *  3. If `countryCode` is supplied AND the remainder still starts with a single
 *     `0` (national format), replace that `0` with the country code. An empty
 *     `countryCode` disables this step (numbers stay as-is).
 *
 * Strings that are not phone numbers (e.g. a SIP username such as `"2301086t3"`
 * when the operator deliberately set `DIAL_DEFAULT_CALLER_ID` to the
 * `SIP_USER`) pass through unchanged because none of the rules match.
 *
 * Examples:
 *  - `normaliseTrunkCallerID('+4921193674951')` → `'4921193674951'`
 *  - `normaliseTrunkCallerID('021193674951', '49')` → `'4921193674951'`
 *  - `normaliseTrunkCallerID('00451234', '')` → `'451234'`
 *
 * @see https://help.sipgate.de/trunking/wie-setze-ich-bei-sipgate-trunking-die-absenderrufnummer
 */
export function normaliseTrunkCallerID(value: string, countryCode = ''): string {
  let s = value.trim();
  if (s.startsWith('+')) {
    s = s.slice(1);
  }
  if (s.startsWith('00')) {
    s = s.slice(2);
  }
  const cc = countryCode.trim();
  if (cc !== '' && s.length > 1 && s[0] === '0') {
    s = cc + s.slice(1);
  }
  return s;
}

/**
 * Match the SIP reason-phrase patterns trunks use to reject the caller-ID in
 * the From header, so a 403 carrying such a reason maps to the
 * `caller_id_rejected` bucket instead of the generic 4xx fallback. sipgate uses
 * the literal "Username in From Field required"; the synonym set lets other
 * carriers' phrasing land in the same bucket. Matching is case-insensitive.
 */
export function isFromRejectionReason(reason: string): boolean {
  const r = reason.toLowerCase();
  return (
    r.includes('from field') ||
    r.includes('from header') ||
    r.includes('from-username') ||
    r.includes('p-asserted-identity') ||
    r.includes('caller-id') ||
    r.includes('callerid')
  );
}
