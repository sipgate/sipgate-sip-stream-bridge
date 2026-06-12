/**
 * TwiML verb model — pure data types produced by the parser (parse.ts) and
 * consumed by the dispatcher (dispatch.ts).
 *
 * Port of go/internal/twiml/verbs.go. The verb set mirrors the v3 Go parser:
 * <Response> root containing <Connect><Stream>, <Dial>+<Number>, <Hangup>,
 * <Redirect>, <Reject>. Unknown verbs are retained as UnknownVerb so the
 * dispatcher can warn-and-skip them (never silently dropped).
 */

/** Discriminant tag for each verb kind. Mirrors the XML element local-name. */
export type VerbKind =
  | 'Connect'
  | 'Dial'
  | 'Hangup'
  | 'Redirect'
  | 'Reject'
  | 'Unknown';

/** Common shape: every verb carries its element-local name for dispatch logs. */
interface VerbBase {
  readonly kind: VerbKind;
  /** Element-local name (e.g. "Dial", "Say"). Used by the dispatcher warn logs. */
  readonly name: string;
}

/**
 * Number is the <Number> child of <Dial>. Per-leg status-callback subscription
 * attributes win over the parent <Dial> values (resolved in verbDial.ts).
 * Backward compat: a bare-text <Number>+49…</Number> parses with text set and
 * the status fields undefined.
 */
export interface Number {
  text: string;
  statusCallback?: string;
  statusCallbackMethod?: string;
  statusCallbackEvents?: string[];
}

/**
 * Dial supports both bare-chardata form (<Dial>+49…</Dial>) AND a <Number>
 * child form (<Dial><Number>+49…</Number></Dial>) — see resolveDialTarget for
 * the disambiguation rules.
 */
export interface Dial extends VerbBase {
  readonly kind: 'Dial';
  readonly name: 'Dial';
  callerId?: string;
  /** Ring timeout in seconds; undefined → default applies (30s). */
  timeout?: number;
  /** Max talk time in seconds; undefined → default applies (14400s). */
  timeLimit?: number;
  hangupOnStar: boolean;
  action?: string;
  method?: string;
  answerOnBridge?: boolean;

  // Status-callback subscription on the parent <Dial>. Per-<Number> overrides
  // take precedence at DialOpts construction time (verbDial.ts).
  statusCallback?: string;
  statusCallbackMethod?: string;
  statusCallbackEvents?: string[];

  // Mutually-preferable target sources — at most one populated in a valid doc.
  numberText?: string;
  number?: Number;

  // Rejected child markers — parser detects these for the dispatcher to warn on.
  hasSip: boolean;
  hasClient: boolean;
  hasConference: boolean;
  hasQueue: boolean;
}

/** Stream models <Stream url=… name=… track=…> with flattened <Parameter> children. */
export interface Stream {
  url?: string;
  name?: string;
  track?: string;
  parameters?: Record<string, string>;
}

/** Connect carries a single nested <Stream>. */
export interface Connect extends VerbBase {
  readonly kind: 'Connect';
  readonly name: 'Connect';
  stream?: Stream;
}

/** Hangup models <Hangup/>. Terminal verb. */
export interface Hangup extends VerbBase {
  readonly kind: 'Hangup';
  readonly name: 'Hangup';
}

/** Redirect models <Redirect method="POST">https://…</Redirect>. */
export interface Redirect extends VerbBase {
  readonly kind: 'Redirect';
  readonly name: 'Redirect';
  method?: string;
  url?: string;
}

/** Reject models <Reject reason="busy|rejected"/>. */
export interface Reject extends VerbBase {
  readonly kind: 'Reject';
  readonly name: 'Reject';
  reason?: string;
}

/**
 * UnknownVerb wraps any element inside <Response> that is not a known verb.
 * Retained so the dispatcher's warn-and-skip path sees it (never dropped).
 */
export interface UnknownVerb extends VerbBase {
  readonly kind: 'Unknown';
}

/** Verb is the discriminated union of all parsed verbs. */
export type Verb = Connect | Dial | Hangup | Redirect | Reject | UnknownVerb;

/**
 * Response is the parsed root document. verbs is in document order — the
 * dispatcher walks the array in order.
 */
export interface Response {
  verbs: Verb[];
}

/**
 * resolveDialTarget returns the dialed number from a Dial verb.
 * Bare-text and <Number> child are equivalent; if BOTH are populated the
 * caller is logged "ambiguous=true" and the <Number> child wins (matches
 * Twilio behavior where structured data trumps inline chardata). Whitespace
 * is trimmed.
 */
export function resolveDialTarget(d: Dial): { target: string; ambiguous: boolean } {
  const text = (d.numberText ?? '').trim();
  if (d.number !== undefined && text !== '') {
    return { target: d.number.text.trim(), ambiguous: true };
  }
  if (d.number !== undefined) {
    return { target: d.number.text.trim(), ambiguous: false };
  }
  if (text !== '') {
    return { target: text, ambiguous: false };
  }
  return { target: '', ambiguous: false };
}
