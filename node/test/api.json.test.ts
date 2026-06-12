import { describe, expect, it } from 'vitest';
import {
  rfc2822,
  serializeCall,
  serializePage,
  type CallView,
} from '../src/api/json.js';

const testAccountSid = 'ACdeadbeefdeadbeefdeadbeefdeadbeef';
const testCallSid = 'CAfedcba9876543210fedcba9876543210';
const testPathPrefix = `/2010-04-01/Accounts/${testAccountSid}`;

function baseCall(overrides: Partial<CallView> = {}): CallView {
  return {
    callSid: testCallSid,
    accountSid: testAccountSid,
    from: '+4915123456789',
    to: '+4930555555',
    status: 'in-progress',
    direction: 'inbound',
    startTime: null,
    endTime: null,
    duration: null,
    answeredBy: null,
    parentCallSid: null,
    ...overrides,
  };
}

describe('rfc2822', () => {
  it('formats UTC fixtures byte-for-byte against the Go test vectors', () => {
    expect(rfc2822(new Date(Date.UTC(1970, 0, 1, 0, 0, 0)))).toBe(
      'Thu, 01 Jan 1970 00:00:00 +0000',
    );
    expect(rfc2822(new Date(Date.UTC(2026, 3, 27, 10, 0, 0)))).toBe(
      'Mon, 27 Apr 2026 10:00:00 +0000',
    );
    expect(rfc2822(new Date(Date.UTC(2026, 11, 31, 23, 59, 59)))).toBe(
      'Thu, 31 Dec 2026 23:59:59 +0000',
    );
    expect(rfc2822(new Date(Date.UTC(2024, 1, 29, 12, 0, 0)))).toBe(
      'Thu, 29 Feb 2024 12:00:00 +0000',
    );
    expect(rfc2822(new Date(Date.UTC(2026, 0, 5, 8, 30, 0)))).toBe(
      'Mon, 05 Jan 2026 08:30:00 +0000',
    );
  });

  it('normalizes a non-UTC input to a literal +0000 offset', () => {
    // 12:00 in Europe/Berlin DST (UTC+2) == 10:00 UTC. We construct the Date
    // from an explicit UTC epoch so the test is timezone-independent.
    const berlinNoonAsUtc = new Date(Date.UTC(2026, 3, 27, 10, 0, 0));
    const got = rfc2822(berlinNoonAsUtc);
    expect(got).toBe('Mon, 27 Apr 2026 10:00:00 +0000');
    expect(got?.endsWith('+0000')).toBe(true);
  });

  it('never emits a GMT suffix (the wrong Twilio format)', () => {
    expect(rfc2822(new Date(Date.UTC(2026, 3, 27, 10, 0, 0)))).not.toContain('GMT');
  });

  it('returns null for a null date', () => {
    expect(rfc2822(null)).toBeNull();
  });
});

describe('serializeCall', () => {
  it('active call: start_time set; end_time/duration/answered_by/parent_call_sid null', () => {
    const cj = serializeCall(
      baseCall({ startTime: new Date(Date.UTC(2026, 3, 27, 10, 0, 0)) }),
      testPathPrefix,
    );
    expect(cj.sid).toBe(testCallSid);
    expect(cj.account_sid).toBe(testAccountSid);
    expect(cj.from).toBe('+4915123456789');
    expect(cj.to).toBe('+4930555555');
    expect(cj.status).toBe('in-progress');
    expect(cj.direction).toBe('inbound');
    expect(cj.api_version).toBe('2010-04-01');
    expect(cj.start_time).toBe('Mon, 27 Apr 2026 10:00:00 +0000');
    expect(cj.end_time).toBeNull();
    expect(cj.duration).toBeNull();
    expect(cj.answered_by).toBeNull();
    expect(cj.parent_call_sid).toBeNull();
  });

  it('completed call: end_time + duration both set; answered_by present', () => {
    const start = new Date(Date.UTC(2026, 3, 27, 10, 0, 0));
    const end = new Date(start.getTime() + 42_000);
    const cj = serializeCall(
      baseCall({ status: 'completed', startTime: start, endTime: end, duration: 42, answeredBy: 'human' }),
      testPathPrefix,
    );
    expect(cj.end_time).toBe('Mon, 27 Apr 2026 10:00:42 +0000');
    expect(cj.duration).toBe(42);
    expect(cj.answered_by).toBe('human');
    expect(cj.status).toBe('completed');
  });

  it('builds the canonical URI and all four subresource URIs', () => {
    const cj = serializeCall(
      baseCall({ startTime: new Date(Date.UTC(2026, 3, 27, 10, 0, 0)) }),
      testPathPrefix,
    );
    expect(cj.uri).toMatch(
      /^\/2010-04-01\/Accounts\/AC[0-9a-f]{32}\/Calls\/CA[0-9a-f]{32}\.json$/,
    );
    expect(cj.subresource_uris).toEqual({
      notifications: `${testPathPrefix}/Calls/${testCallSid}/Notifications.json`,
      recordings: `${testPathPrefix}/Calls/${testCallSid}/Recordings.json`,
      events: `${testPathPrefix}/Calls/${testCallSid}/Events.json`,
      siprec: `${testPathPrefix}/Calls/${testCallSid}/Siprec.json`,
    });
  });

  it('serializes nullable fields as explicit null (not omitted) in JSON', () => {
    const cj = serializeCall(baseCall(), testPathPrefix);
    const raw = JSON.stringify(cj);
    expect(raw).toContain('"start_time":null');
    expect(raw).toContain('"end_time":null');
    expect(raw).toContain('"duration":null');
    expect(raw).toContain('"answered_by":null');
    expect(raw).toContain('"parent_call_sid":null');
  });
});

function makeCalls(n: number): CallView[] {
  const start = new Date(Date.UTC(2026, 3, 27, 10, 0, 0));
  return Array.from({ length: n }, (_, i) =>
    baseCall({ from: '+49000', to: '+49111', startTime: new Date(start.getTime() + i * 1000) }),
  );
}

describe('serializePage', () => {
  it('first page of many: start=0 end=3, next set, previous null', () => {
    const pj = serializePage(makeCalls(10), testPathPrefix, 0, 3);
    expect(pj.page).toBe(0);
    expect(pj.page_size).toBe(3);
    expect(pj.start).toBe(0);
    expect(pj.end).toBe(3);
    expect(pj.calls).toHaveLength(3);
    expect(pj.next_page_uri).not.toBeNull();
    expect(pj.next_page_uri).toContain('Page=1');
    expect(pj.previous_page_uri).toBeNull();
    expect(pj.uri).toBe(`${testPathPrefix}/Calls.json?Page=0&PageSize=3`);
    expect(pj.first_page_uri).toBe(`${testPathPrefix}/Calls.json?Page=0&PageSize=3`);
  });

  it('last page: start=9 end=10, next null, previous set', () => {
    const pj = serializePage(makeCalls(10), testPathPrefix, 3, 3);
    expect(pj.start).toBe(9);
    expect(pj.end).toBe(10);
    expect(pj.calls).toHaveLength(1);
    expect(pj.next_page_uri).toBeNull();
    expect(pj.previous_page_uri).not.toBeNull();
    expect(pj.previous_page_uri).toContain('Page=2');
  });

  it('empty list: calls is [] (never null), both boundary URIs null', () => {
    const pj = serializePage([], testPathPrefix, 0, 50);
    expect(pj.start).toBe(0);
    expect(pj.end).toBe(0);
    expect(Array.isArray(pj.calls)).toBe(true);
    expect(pj.calls).toHaveLength(0);
    expect(pj.next_page_uri).toBeNull();
    expect(pj.previous_page_uri).toBeNull();
    expect(JSON.stringify(pj)).toContain('"calls":[]');
  });

  it('clamps pageSize and page bounds like Go', () => {
    expect(serializePage(makeCalls(1), testPathPrefix, 0, 0).page_size).toBe(50);
    expect(serializePage(makeCalls(1), testPathPrefix, 0, 10000).page_size).toBe(1000);
    const neg = serializePage(makeCalls(1), testPathPrefix, -1, -7);
    expect(neg.page).toBe(0);
    expect(neg.page_size).toBe(50);
  });

  it('next/previous URIs are present-but-null in JSON on a single page (not omitted)', () => {
    const raw = JSON.stringify(serializePage(makeCalls(3), testPathPrefix, 0, 50));
    expect(raw).toContain('"next_page_uri":null');
    expect(raw).toContain('"previous_page_uri":null');
  });
});
