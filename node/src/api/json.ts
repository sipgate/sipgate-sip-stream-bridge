/**
 * Twilio-strict JSON serialization for the Call resource and its pagination
 * envelope.
 *
 * Pure leaf module. The wire contract is byte-compatible with Twilio's REST
 * API so SDKs validate it against their typed wrappers:
 *   - Nullable fields serialize as explicit `null`, never omitted.
 *   - Empty `calls` is `[]`, never `null`.
 *   - Timestamps are RFC 2822 in UTC with a literal "+0000" offset.
 */

/**
 * CallView is the read-only contract the serializer needs from a bridge call.
 * Defined locally so this module stays decoupled from the bridge layer — the
 * bridge implements it later at the router wire-up site.
 *
 * Times are JS Date | null: a null time means "not set" (e.g. an in-progress
 * call has no endTime), which serializes to JSON null.
 */
export interface CallView {
  callSid: string;
  accountSid: string;
  from: string;
  to: string;
  status: string;
  direction: string;
  startTime: Date | null;
  endTime: Date | null;
  duration: number | null;
  answeredBy: string | null;
  parentCallSid: string | null;
}

const WEEKDAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'] as const;
const MONTHS = [
  'Jan',
  'Feb',
  'Mar',
  'Apr',
  'May',
  'Jun',
  'Jul',
  'Aug',
  'Sep',
  'Oct',
  'Nov',
  'Dec',
] as const;

function pad2(n: number): string {
  return n < 10 ? `0${n}` : `${n}`;
}

/**
 * Format a Date as Twilio's RFC 2822 timestamp, ALWAYS in UTC:
 *
 *   "Mon, 27 Apr 2026 10:00:00 +0000"
 *
 * WARNING: JS Date.toUTCString() yields "...GMT", which is the WRONG suffix for
 * Twilio's wire format. We hand-format every field from the Date's UTC getters
 * so non-UTC inputs are normalized and the offset is always the literal
 * "+0000". Returns null for a null date (callers emit JSON null).
 */
export function rfc2822(date: Date | null): string | null {
  if (date === null) {
    return null;
  }
  const weekday = WEEKDAYS[date.getUTCDay()];
  const day = pad2(date.getUTCDate());
  const month = MONTHS[date.getUTCMonth()];
  const year = date.getUTCFullYear();
  const hh = pad2(date.getUTCHours());
  const mm = pad2(date.getUTCMinutes());
  const ss = pad2(date.getUTCSeconds());
  return `${weekday}, ${day} ${month} ${year} ${hh}:${mm}:${ss} +0000`;
}

/**
 * CallJSON is the Twilio-shaped Call resource. Nullable fields are typed
 * `T | null` and ALWAYS present in the JSON (emitted as `null`, never omitted)
 * to match Twilio's wire contract.
 */
export interface CallJSON {
  sid: string;
  account_sid: string;
  from: string;
  to: string;
  status: string;
  start_time: string | null;
  end_time: string | null;
  duration: number | null;
  direction: string;
  answered_by: string | null;
  parent_call_sid: string | null;
  api_version: string;
  uri: string;
  subresource_uris: Record<string, string>;
}

/**
 * Serialize a CallView into the Twilio-shape Call JSON object.
 *
 * `pathPrefix` is the AccountSid-bearing route prefix used to build URIs, e.g.
 * "/2010-04-01/Accounts/ACxxxx".
 *
 * Mirrors Go's SerializeCall: duration is only set when endTime is present
 * (a call has no meaningful duration until it ends), and answered_by /
 * parent_call_sid stay null when empty.
 */
export function serializeCall(c: CallView, pathPrefix: string): CallJSON {
  const startTime = c.startTime !== null ? rfc2822(c.startTime) : null;
  let endTime: string | null = null;
  let duration: number | null = null;
  if (c.endTime !== null) {
    endTime = rfc2822(c.endTime);
    duration = c.duration;
  }

  return {
    sid: c.callSid,
    account_sid: c.accountSid,
    from: c.from,
    to: c.to,
    status: c.status,
    start_time: startTime,
    end_time: endTime,
    duration,
    direction: c.direction,
    answered_by: c.answeredBy !== null && c.answeredBy !== '' ? c.answeredBy : null,
    parent_call_sid: c.parentCallSid !== null && c.parentCallSid !== '' ? c.parentCallSid : null,
    api_version: '2010-04-01',
    uri: `${pathPrefix}/Calls/${c.callSid}.json`,
    subresource_uris: {
      notifications: `${pathPrefix}/Calls/${c.callSid}/Notifications.json`,
      recordings: `${pathPrefix}/Calls/${c.callSid}/Recordings.json`,
      events: `${pathPrefix}/Calls/${c.callSid}/Events.json`,
      siprec: `${pathPrefix}/Calls/${c.callSid}/Siprec.json`,
    },
  };
}

/**
 * PageJSON is the Twilio page envelope for a list of Call resources.
 *
 * next_page_uri / previous_page_uri are `string | null` and ALWAYS present
 * (PRESENT-but-null on the first/last pages, never omitted). `calls` is always
 * an array (empty `[]`, never null).
 */
export interface PageJSON {
  page: number;
  page_size: number;
  start: number;
  end: number;
  uri: string;
  next_page_uri: string | null;
  previous_page_uri: string | null;
  first_page_uri: string;
  calls: CallJSON[];
}

/**
 * Paginate a list of CallView and produce the Twilio-shape envelope.
 *
 * Conventions (mirroring Go's SerializePage):
 *   - page is 0-indexed; values < 0 clamp to 0.
 *   - pageSize is clamped: values < 1 default to 50, values > 1000 clip to 1000.
 *   - pathPrefix is the AccountSid-bearing route prefix used to build URIs.
 *   - On out-of-range pages the calls slice is empty but start/end reflect the
 *     clamped indices.
 */
export function serializePage(
  items: CallView[],
  pathPrefix: string,
  page: number,
  pageSize: number,
): PageJSON {
  if (pageSize < 1) {
    pageSize = 50;
  }
  if (pageSize > 1000) {
    pageSize = 1000;
  }
  if (page < 0) {
    page = 0;
  }

  const total = items.length;
  let start = page * pageSize;
  let end = start + pageSize;
  if (start > total) {
    start = total;
  }
  if (end > total) {
    end = total;
  }

  const calls = items.slice(start, end).map((c) => serializeCall(c, pathPrefix));

  const currentURI = `${pathPrefix}/Calls.json?Page=${page}&PageSize=${pageSize}`;
  const firstURI = `${pathPrefix}/Calls.json?Page=0&PageSize=${pageSize}`;

  const nextURI =
    end < total ? `${pathPrefix}/Calls.json?Page=${page + 1}&PageSize=${pageSize}` : null;
  const prevURI =
    page > 0 ? `${pathPrefix}/Calls.json?Page=${page - 1}&PageSize=${pageSize}` : null;

  return {
    page,
    page_size: pageSize,
    start,
    end,
    uri: currentURI,
    next_page_uri: nextURI,
    previous_page_uri: prevURI,
    first_page_uri: firstURI,
    calls,
  };
}
