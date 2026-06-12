/**
 * Hand-rolled, strict TwiML parser. Port of go/internal/twiml/parse.go.
 *
 * No XML dependency: a minimal forward token-walk over the input string that
 * handles exactly the TwiML subset — a <Response> root, child verb elements in
 * document order, element attributes, bare character data, self-closing tags,
 * nested children (<Dial><Number>…</Number></Dial>), XML entity unescaping
 * (&amp; &lt; &gt; &quot; &apos; &#NN; &#xNN;), and skipping comments / PIs /
 * DOCTYPE / whitespace.
 *
 * The walker preserves verb order and retains UNKNOWN verbs as UnknownVerb so
 * the dispatcher can warn-and-skip them (never silently dropped).
 */

import type {
  Connect,
  Dial,
  Number as DialNumber,
  Redirect,
  Reject,
  Response,
  Stream,
  Verb,
} from './verbs.js';

/**
 * errorCodeMalformed is the Twilio error code surfaced for any malformed-XML,
 * wrong-root, or generic-parse-failure case (Twilio 12100 — "Document parse
 * failure"). Every parse failure funnels through this single code so the
 * downstream webhook signer and REST status callbacks have one stable reason.
 */
export const ERROR_CODE_MALFORMED = 12100;

/** TwimlError is the structured failure type thrown on any parse problem. */
export class TwimlError extends Error {
  readonly code: number;

  constructor(code: number, message: string) {
    super(`twiml error ${code}: ${message}`);
    this.name = 'TwimlError';
    this.code = code;
  }
}

// ── Tokenizer ──────────────────────────────────────────────────────────────

type Token =
  | { type: 'start'; name: string; attrs: Record<string, string>; selfClosing: boolean }
  | { type: 'end'; name: string }
  | { type: 'chardata'; text: string };

/**
 * Tokenizer is a minimal forward XML scanner. It emits start/end/chardata
 * tokens and silently skips comments, processing instructions, CDATA markers,
 * and DOCTYPE declarations. Self-closing start tokens carry selfClosing=true
 * (no synthesized end token is emitted — callers must handle it).
 *
 * On structurally malformed input (unterminated tag, etc.) next() throws
 * TwimlError(12100).
 */
class Tokenizer {
  private readonly s: string;
  private pos = 0;

  constructor(s: string) {
    this.s = s;
  }

  /** Returns the next token, or null at end of input. */
  next(): Token | null {
    if (this.pos >= this.s.length) {
      return null;
    }

    if (this.s[this.pos] !== '<') {
      // Character data up to the next '<' (or end of input).
      const start = this.pos;
      const lt = this.s.indexOf('<', this.pos);
      const end = lt === -1 ? this.s.length : lt;
      this.pos = end;
      return { type: 'chardata', text: unescapeEntities(this.s.slice(start, end)) };
    }

    // We are at '<'. Distinguish markup forms.
    if (this.s.startsWith('<!--', this.pos)) {
      const close = this.s.indexOf('-->', this.pos + 4);
      if (close === -1) {
        throw new TwimlError(ERROR_CODE_MALFORMED, 'unterminated comment');
      }
      this.pos = close + 3;
      return this.next();
    }
    if (this.s.startsWith('<![CDATA[', this.pos)) {
      const close = this.s.indexOf(']]>', this.pos + 9);
      if (close === -1) {
        throw new TwimlError(ERROR_CODE_MALFORMED, 'unterminated CDATA');
      }
      const text = this.s.slice(this.pos + 9, close);
      this.pos = close + 3;
      // CDATA is literal text — not entity-decoded.
      return { type: 'chardata', text };
    }
    if (this.s.startsWith('<?', this.pos)) {
      const close = this.s.indexOf('?>', this.pos + 2);
      if (close === -1) {
        throw new TwimlError(ERROR_CODE_MALFORMED, 'unterminated processing instruction');
      }
      this.pos = close + 2;
      return this.next();
    }
    if (this.s.startsWith('<!', this.pos)) {
      // DOCTYPE or other declaration — skip to matching '>'.
      const close = this.s.indexOf('>', this.pos + 2);
      if (close === -1) {
        throw new TwimlError(ERROR_CODE_MALFORMED, 'unterminated declaration');
      }
      this.pos = close + 1;
      return this.next();
    }

    // A start or end element. Find the closing '>'.
    const gt = this.s.indexOf('>', this.pos);
    if (gt === -1) {
      throw new TwimlError(ERROR_CODE_MALFORMED, 'unterminated tag');
    }
    let inner = this.s.slice(this.pos + 1, gt);
    this.pos = gt + 1;

    if (inner.startsWith('/')) {
      const name = inner.slice(1).trim();
      if (name === '') {
        throw new TwimlError(ERROR_CODE_MALFORMED, 'empty end tag');
      }
      return { type: 'end', name: localName(name) };
    }

    let selfClosing = false;
    if (inner.endsWith('/')) {
      selfClosing = true;
      inner = inner.slice(0, -1);
    }

    const parsed = parseStartTag(inner);
    return { type: 'start', name: parsed.name, attrs: parsed.attrs, selfClosing };
  }
}

/** parseStartTag splits "Name attr1=\"v\" attr2='v'" into a name + attr map. */
function parseStartTag(inner: string): { name: string; attrs: Record<string, string> } {
  let i = 0;
  const n = inner.length;
  const skipWs = (): void => {
    while (i < n && isXmlSpace(inner[i])) i++;
  };

  skipWs();
  const nameStart = i;
  while (i < n && !isXmlSpace(inner[i])) i++;
  const rawName = inner.slice(nameStart, i);
  if (rawName === '') {
    throw new TwimlError(ERROR_CODE_MALFORMED, 'empty start tag');
  }

  const attrs: Record<string, string> = {};
  for (;;) {
    skipWs();
    if (i >= n) break;
    const attrStart = i;
    while (i < n && inner[i] !== '=' && !isXmlSpace(inner[i])) i++;
    const attrName = inner.slice(attrStart, i);
    if (attrName === '') break;
    skipWs();
    if (i >= n || inner[i] !== '=') {
      // Valueless attribute — TwiML never uses these; record empty value.
      attrs[localName(attrName)] = '';
      continue;
    }
    i++; // consume '='
    skipWs();
    if (i >= n) {
      throw new TwimlError(ERROR_CODE_MALFORMED, `attribute ${attrName} missing value`);
    }
    const quote = inner[i];
    if (quote !== '"' && quote !== "'") {
      throw new TwimlError(ERROR_CODE_MALFORMED, `attribute ${attrName} value not quoted`);
    }
    i++; // consume opening quote
    const valStart = i;
    while (i < n && inner[i] !== quote) i++;
    if (i >= n) {
      throw new TwimlError(ERROR_CODE_MALFORMED, `attribute ${attrName} value unterminated`);
    }
    const rawVal = inner.slice(valStart, i);
    i++; // consume closing quote
    attrs[localName(attrName)] = unescapeEntities(rawVal);
  }

  return { name: localName(rawName), attrs };
}

/** localName strips an optional XML namespace prefix (ns:Local → Local). */
function localName(qname: string): string {
  const colon = qname.indexOf(':');
  return colon === -1 ? qname : qname.slice(colon + 1);
}

function isXmlSpace(c: string): boolean {
  return c === ' ' || c === '\t' || c === '\n' || c === '\r';
}

/** unescapeEntities decodes the XML entity subset TwiML uses. */
function unescapeEntities(s: string): string {
  if (s.indexOf('&') === -1) return s;
  return s.replace(/&(#x[0-9a-fA-F]+|#[0-9]+|[a-zA-Z]+);/g, (match, body: string) => {
    if (body[0] === '#') {
      const code =
        body[1] === 'x' || body[1] === 'X'
          ? Number.parseInt(body.slice(2), 16)
          : Number.parseInt(body.slice(1), 10);
      if (Number.isNaN(code)) return match;
      return String.fromCodePoint(code);
    }
    switch (body) {
      case 'amp':
        return '&';
      case 'lt':
        return '<';
      case 'gt':
        return '>';
      case 'quot':
        return '"';
      case 'apos':
        return "'";
      default:
        return match; // unknown named entity — leave verbatim
    }
  });
}

// ── StatusCallbackEvent enum gate ────────────────────────────────────────────

const VALID_STATUS_CALLBACK_EVENTS = new Set<string>([
  'initiated',
  'ringing',
  'answered',
  'in-progress',
  'completed',
  'busy',
  'failed',
  'no-answer',
  'canceled',
]);

/**
 * parseStatusCallbackEvents tokenizes a StatusCallbackEvent attribute or
 * form-param value. Twilio accepts BOTH space-separated AND comma-separated
 * forms (and any mix); unknown event names are rejected with TwimlError(12100).
 * Returns undefined on empty input (no sentinel branch needed by callers).
 */
export function parseStatusCallbackEvents(raw: string): string[] | undefined {
  const tokens = raw.split(/[\s,]+/);
  const out: string[] = [];
  for (const t of tokens) {
    const tok = t.trim();
    if (tok === '') continue;
    if (!VALID_STATUS_CALLBACK_EVENTS.has(tok)) {
      throw new TwimlError(ERROR_CODE_MALFORMED, `StatusCallbackEvent: unknown value "${tok}"`);
    }
    out.push(tok);
  }
  return out.length === 0 ? undefined : out;
}

// ── Parser ───────────────────────────────────────────────────────────────────

/**
 * parseResponse consumes a TwiML body and returns the parsed Response, or
 * throws TwimlError(12100) on any parse problem.
 *
 * Strict <Response> root: any other root throws 12100. Empty input, EOF before
 * a start element, and malformed markup all funnel through 12100.
 *
 * Unknown verbs are retained as UnknownVerb so the dispatcher emits a per-verb
 * warn-and-skip log. The parser does NOT silently drop them.
 */
export function parseResponse(xml: string): Response {
  if (xml.length === 0) {
    throw new TwimlError(ERROR_CODE_MALFORMED, 'empty document');
  }
  const tz = new Tokenizer(xml);

  // Scan for the root start element, skipping leading chardata/whitespace.
  for (;;) {
    const tok = tz.next();
    if (tok === null) {
      throw new TwimlError(ERROR_CODE_MALFORMED, 'empty document');
    }
    if (tok.type === 'chardata') {
      continue;
    }
    if (tok.type === 'end') {
      throw new TwimlError(ERROR_CODE_MALFORMED, `unexpected </${tok.name}> before root`);
    }
    // start element
    if (tok.name !== 'Response') {
      throw new TwimlError(ERROR_CODE_MALFORMED, 'root is not <Response>');
    }
    if (tok.selfClosing) {
      return { verbs: [] };
    }
    return parseResponseChildren(tz);
  }
}

/**
 * parseResponseChildren walks the children of <Response> until the matching
 * </Response>. Unknown elements are retained as UnknownVerb.
 */
function parseResponseChildren(tz: Tokenizer): Response {
  const verbs: Verb[] = [];
  for (;;) {
    const tok = tz.next();
    if (tok === null) {
      throw new TwimlError(ERROR_CODE_MALFORMED, 'unexpected EOF inside <Response>');
    }
    if (tok.type === 'chardata') {
      continue; // whitespace / text at Response level is ignored
    }
    if (tok.type === 'end') {
      if (tok.name === 'Response') {
        return { verbs };
      }
      throw new TwimlError(
        ERROR_CODE_MALFORMED,
        `unexpected </${tok.name}> at Response level`,
      );
    }
    // start element
    switch (tok.name) {
      case 'Connect':
        verbs.push(parseConnect(tz, tok.selfClosing));
        break;
      case 'Dial':
        verbs.push(parseDial(tz, tok.attrs, tok.selfClosing));
        break;
      case 'Hangup':
        if (!tok.selfClosing) skipToEnd(tz, 'Hangup');
        verbs.push({ kind: 'Hangup', name: 'Hangup' });
        break;
      case 'Redirect':
        verbs.push(parseRedirect(tz, tok.attrs, tok.selfClosing));
        break;
      case 'Reject':
        if (!tok.selfClosing) skipToEnd(tz, 'Reject');
        verbs.push({ kind: 'Reject', name: 'Reject', reason: tok.attrs.reason });
        break;
      default:
        if (!tok.selfClosing) skipToEnd(tz, tok.name);
        verbs.push({ kind: 'Unknown', name: tok.name });
    }
  }
}

/**
 * parseConnect consumes children of <Connect>. Only a single <Stream> child is
 * honored; any other child is silently consumed.
 */
function parseConnect(tz: Tokenizer, selfClosing: boolean): Connect {
  const connect: Connect = { kind: 'Connect', name: 'Connect' };
  if (selfClosing) return connect;
  for (;;) {
    const tok = tz.next();
    if (tok === null) {
      throw new TwimlError(ERROR_CODE_MALFORMED, 'unexpected EOF inside <Connect>');
    }
    if (tok.type === 'end') {
      if (tok.name === 'Connect') return connect;
      continue;
    }
    if (tok.type === 'start') {
      if (tok.name === 'Stream') {
        connect.stream = parseStream(tz, tok.attrs, tok.selfClosing);
      } else if (!tok.selfClosing) {
        skipToEnd(tz, tok.name);
      }
    }
  }
}

/** parseStream reads <Stream> attributes and flattens <Parameter> children. */
function parseStream(tz: Tokenizer, attrs: Record<string, string>, selfClosing: boolean): Stream {
  const stream: Stream = {};
  if (attrs.url !== undefined) stream.url = attrs.url;
  if (attrs.name !== undefined) stream.name = attrs.name;
  if (attrs.track !== undefined) stream.track = attrs.track;
  if (selfClosing) return stream;

  for (;;) {
    const tok = tz.next();
    if (tok === null) {
      throw new TwimlError(ERROR_CODE_MALFORMED, 'unexpected EOF inside <Stream>');
    }
    if (tok.type === 'end') {
      if (tok.name === 'Stream') return stream;
      continue;
    }
    if (tok.type === 'start') {
      if (tok.name === 'Parameter') {
        const pn = tok.attrs.name;
        const pv = tok.attrs.value ?? '';
        if (pn !== undefined && pn !== '') {
          stream.parameters ??= {};
          stream.parameters[pn] = pv;
        }
      }
      if (!tok.selfClosing) skipToEnd(tz, tok.name);
    }
  }
}

/**
 * parseDial reads <Dial> attributes and walks children. Bare chardata becomes
 * numberText; a <Number> child populates number. <Sip>/<Client>/<Conference>/
 * <Queue> set the corresponding has* flag.
 */
function parseDial(tz: Tokenizer, attrs: Record<string, string>, selfClosing: boolean): Dial {
  const dial: Dial = {
    kind: 'Dial',
    name: 'Dial',
    hangupOnStar: attrs.hangupOnStar === 'true',
    hasSip: false,
    hasClient: false,
    hasConference: false,
    hasQueue: false,
  };
  if (attrs.callerId !== undefined) dial.callerId = attrs.callerId;
  // Non-numeric timeout/timeLimit is silently ignored (field stays undefined).
  const timeout = parseIntStrict(attrs.timeout);
  if (timeout !== undefined) dial.timeout = timeout;
  const timeLimit = parseIntStrict(attrs.timeLimit);
  if (timeLimit !== undefined) dial.timeLimit = timeLimit;
  if (attrs.action !== undefined) dial.action = attrs.action;
  if (attrs.method !== undefined) dial.method = attrs.method;
  if (attrs.answerOnBridge !== undefined) dial.answerOnBridge = attrs.answerOnBridge === 'true';
  if (attrs.statusCallback !== undefined) dial.statusCallback = attrs.statusCallback;
  if (attrs.statusCallbackMethod !== undefined) {
    dial.statusCallbackMethod = attrs.statusCallbackMethod;
  }
  if (attrs.statusCallbackEvent !== undefined) {
    try {
      dial.statusCallbackEvents = parseStatusCallbackEvents(attrs.statusCallbackEvent);
    } catch (e) {
      throw rewrapStatusError(e, '<Dial>');
    }
  }

  if (selfClosing) return dial;

  let numberText = '';
  for (;;) {
    const tok = tz.next();
    if (tok === null) {
      throw new TwimlError(ERROR_CODE_MALFORMED, 'unexpected EOF inside <Dial>');
    }
    if (tok.type === 'chardata') {
      numberText += tok.text;
      continue;
    }
    if (tok.type === 'end') {
      if (tok.name === 'Dial') {
        if (numberText !== '') dial.numberText = numberText;
        return dial;
      }
      continue;
    }
    // start element
    switch (tok.name) {
      case 'Number':
        dial.number = parseNumber(tz, tok.attrs, tok.selfClosing);
        break;
      case 'Sip':
        dial.hasSip = true;
        if (!tok.selfClosing) skipToEnd(tz, 'Sip');
        break;
      case 'Client':
        dial.hasClient = true;
        if (!tok.selfClosing) skipToEnd(tz, 'Client');
        break;
      case 'Conference':
        dial.hasConference = true;
        if (!tok.selfClosing) skipToEnd(tz, 'Conference');
        break;
      case 'Queue':
        dial.hasQueue = true;
        if (!tok.selfClosing) skipToEnd(tz, 'Queue');
        break;
      default:
        if (!tok.selfClosing) skipToEnd(tz, tok.name);
    }
  }
}

/** parseNumber reads per-leg attrs then collects chardata into text. */
function parseNumber(
  tz: Tokenizer,
  attrs: Record<string, string>,
  selfClosing: boolean,
): DialNumber {
  const number: DialNumber = { text: '' };
  if (attrs.statusCallback !== undefined) number.statusCallback = attrs.statusCallback;
  if (attrs.statusCallbackMethod !== undefined) {
    number.statusCallbackMethod = attrs.statusCallbackMethod;
  }
  if (attrs.statusCallbackEvent !== undefined) {
    try {
      number.statusCallbackEvents = parseStatusCallbackEvents(attrs.statusCallbackEvent);
    } catch (e) {
      throw rewrapStatusError(e, '<Number>');
    }
  }
  if (selfClosing) return number;

  let text = '';
  for (;;) {
    const tok = tz.next();
    if (tok === null) {
      throw new TwimlError(ERROR_CODE_MALFORMED, 'unexpected EOF inside <Number>');
    }
    if (tok.type === 'chardata') {
      text += tok.text;
      continue;
    }
    if (tok.type === 'end') {
      if (tok.name === 'Number') {
        number.text = text;
        return number;
      }
      continue;
    }
    // <Number> has no children in v3; skip any defensively.
    if (tok.type === 'start' && !tok.selfClosing) {
      skipToEnd(tz, tok.name);
    }
  }
}

/** parseRedirect reads <Redirect method="…">URL</Redirect>. */
function parseRedirect(
  tz: Tokenizer,
  attrs: Record<string, string>,
  selfClosing: boolean,
): Redirect {
  const redirect: Redirect = { kind: 'Redirect', name: 'Redirect' };
  if (attrs.method !== undefined) redirect.method = attrs.method;
  if (selfClosing) return redirect;

  let url = '';
  for (;;) {
    const tok = tz.next();
    if (tok === null) {
      throw new TwimlError(ERROR_CODE_MALFORMED, 'unexpected EOF inside <Redirect>');
    }
    if (tok.type === 'chardata') {
      url += tok.text;
      continue;
    }
    if (tok.type === 'end') {
      if (tok.name === 'Redirect') {
        if (url !== '') redirect.url = url;
        return redirect;
      }
      continue;
    }
    if (tok.type === 'start' && !tok.selfClosing) {
      skipToEnd(tz, tok.name);
    }
  }
}

/**
 * skipToEnd consumes tokens until the matching end element for `name` at the
 * current depth, handling nested same-name elements and self-closing children.
 */
function skipToEnd(tz: Tokenizer, name: string): void {
  let depth = 0;
  for (;;) {
    const tok = tz.next();
    if (tok === null) {
      throw new TwimlError(ERROR_CODE_MALFORMED, `unexpected EOF skipping <${name}>`);
    }
    if (tok.type === 'start') {
      if (!tok.selfClosing) depth++;
    } else if (tok.type === 'end') {
      if (depth === 0 && tok.name === name) return;
      if (depth > 0) depth--;
    }
  }
}

/** parseIntStrict parses a base-10 integer, returning undefined on any non-integer. */
function parseIntStrict(v: string | undefined): number | undefined {
  if (v === undefined) return undefined;
  const s = v.trim();
  if (!/^[+-]?\d+$/.test(s)) return undefined;
  const n = Number.parseInt(s, 10);
  return Number.isNaN(n) ? undefined : n;
}

/** rewrapStatusError prefixes a TwimlError message with the element context. */
function rewrapStatusError(e: unknown, ctx: string): TwimlError {
  if (e instanceof TwimlError) {
    return new TwimlError(e.code, `${ctx}: ${e.message.replace(/^twiml error \d+: /, '')}`);
  }
  return new TwimlError(ERROR_CODE_MALFORMED, `${ctx}: ${String(e)}`);
}
