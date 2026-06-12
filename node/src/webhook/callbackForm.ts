/**
 * Twilio status-callback form construction.
 *
 * Produces the application/x-www-form-urlencoded field set Twilio POSTs for each
 * call-progress event. Field names + RFC-2822 Timestamp + 0-indexed monotonic
 * SequenceNumber are byte-compatible with Twilio's webhook payloads.
 */

const WEEKDAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

/** Format a Date as RFC 2822 / RFC 1123Z in UTC: "Mon, 27 Apr 2026 10:00:00 +0000". */
export function rfc2822(d: Date): string {
  const p2 = (n: number): string => String(n).padStart(2, '0');
  return (
    `${WEEKDAYS[d.getUTCDay()]}, ${p2(d.getUTCDate())} ${MONTHS[d.getUTCMonth()]} ${d.getUTCFullYear()} ` +
    `${p2(d.getUTCHours())}:${p2(d.getUTCMinutes())}:${p2(d.getUTCSeconds())} +0000`
  );
}

export interface StatusFormInput {
  callSid: string;
  accountSid: string;
  from: string;
  to: string;
  direction: string;
  /** Twilio CallStatus vocab: queued|ringing|in-progress|completed|busy|failed|no-answer|canceled */
  callStatus: string;
  sequenceNumber: number;
  timestamp: Date;
  /** Terminal-only: whole seconds since answer. */
  callDurationSec?: number;
  /** Terminal-only: SIP final response code, if captured. */
  sipResponseCode?: number;
}

/** Build the Twilio status-callback form fields for one event. */
export function buildStatusForm(input: StatusFormInput): Record<string, string> {
  const form: Record<string, string> = {
    CallSid: input.callSid,
    AccountSid: input.accountSid,
    From: input.from,
    To: input.to,
    Caller: input.from,
    Called: input.to,
    Direction: input.direction,
    ApiVersion: '2010-04-01',
    CallStatus: input.callStatus,
    Timestamp: rfc2822(input.timestamp),
    SequenceNumber: String(input.sequenceNumber),
    CallbackSource: 'call-progress-events',
  };
  if (input.callDurationSec !== undefined) {
    form.CallDuration = String(input.callDurationSec);
    form.Duration = String(Math.max(1, Math.ceil(input.callDurationSec / 60)));
  }
  if (input.sipResponseCode !== undefined && input.sipResponseCode > 0) {
    form.SipResponseCode = String(input.sipResponseCode);
  }
  return form;
}
