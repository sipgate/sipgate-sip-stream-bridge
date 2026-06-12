import { describe, expect, it } from 'vitest';
import { isTerminalEvent, resolveEventName, subscriptionMatches } from '../src/webhook/subscription.js';

/** Build a subscription Set from a list of event names. */
function set(...events: string[]): Set<string> {
  return new Set(events);
}

describe('isTerminalEvent', () => {
  const cases: Array<[event: string, want: boolean]> = [
    // 5 terminal events
    ['completed', true],
    ['busy', true],
    ['failed', true],
    ['no-answer', true],
    ['canceled', true],
    // lifecycle / non-terminal
    ['initiated', false],
    ['ringing', false],
    ['answered', false],
    ['in-progress', false],
  ];
  for (const [event, want] of cases) {
    it(`${event} → ${want}`, () => {
      expect(isTerminalEvent(event)).toBe(want);
    });
  }
});

describe('subscriptionMatches', () => {
  const cases: Array<[name: string, events: Set<string> | undefined, event: string, want: boolean]> = [
    // Rule 1: undefined/empty → subscribe-to-all
    ['undefined_events_initiated', undefined, 'initiated', true],
    ['empty_events_initiated', set(), 'initiated', true],
    ['empty_events_completed', set(), 'completed', true],
    ['empty_events_ringing', set(), 'ringing', true],
    ['empty_events_busy', set(), 'busy', true],
    // Rule 2: specific match
    ['initiated_only_initiated', set('initiated'), 'initiated', true],
    ['completed_only_completed', set('completed'), 'completed', true],
    ['ringing_only_ringing', set('ringing'), 'ringing', true],
    ['busy_in_set_busy', set('busy'), 'busy', true],
    ['answered_in_set_answered', set('answered', 'completed'), 'answered', true],
    // Rule 3: terminal-class fallback to "completed"
    ['completed_only_busy_falls_back', set('completed'), 'busy', true],
    ['completed_only_failed_falls_back', set('completed'), 'failed', true],
    ['completed_only_no_answer_falls_back', set('completed'), 'no-answer', true],
    ['completed_only_canceled_falls_back', set('completed'), 'canceled', true],
    ['initiated_and_completed_busy_falls_back', set('initiated', 'completed'), 'busy', true],
    // Rule 4: not subscribed
    ['initiated_only_ringing_drops', set('initiated'), 'ringing', false],
    ['completed_only_initiated_drops (lifecycle no fallback)', set('completed'), 'initiated', false],
    ['completed_only_ringing_drops (lifecycle no fallback)', set('completed'), 'ringing', false],
    ['completed_only_answered_drops (lifecycle no fallback)', set('completed'), 'answered', false],
    ['completed_only_in_progress_drops (in-progress not terminal)', set('completed'), 'in-progress', false],
    ['ringing_only_completed_drops', set('ringing'), 'completed', false],
    ['ringing_only_busy_drops (no completed in set)', set('ringing'), 'busy', false],
    ['initiated_only_busy_drops (no completed in set)', set('initiated'), 'busy', false],
  ];
  for (const [name, events, event, want] of cases) {
    it(`${name} → ${want}`, () => {
      expect(subscriptionMatches(events, event)).toBe(want);
    });
  }
});

describe('resolveEventName', () => {
  const cases: Array<
    [name: string, events: Set<string> | undefined, event: string, wantName: string, wantEmit: boolean]
  > = [
    // Rule 1: passthrough
    ['undefined_events_initiated_passthrough', undefined, 'initiated', 'initiated', true],
    ['empty_events_completed_passthrough', set(), 'completed', 'completed', true],
    ['empty_events_busy_passthrough', set(), 'busy', 'busy', true],
    // Rule 2: specific match (no remap)
    ['initiated_in_set_keeps', set('initiated'), 'initiated', 'initiated', true],
    ['busy_in_set_keeps', set('busy'), 'busy', 'busy', true],
    ['completed_in_set_keeps', set('completed'), 'completed', 'completed', true],
    // Rule 3: REMAP — terminal + only "completed"
    ['completed_only_busy_REMAPS', set('completed'), 'busy', 'completed', true],
    ['completed_only_failed_REMAPS', set('completed'), 'failed', 'completed', true],
    ['completed_only_no_answer_REMAPS', set('completed'), 'no-answer', 'completed', true],
    ['completed_only_canceled_REMAPS', set('completed'), 'canceled', 'completed', true],
    // Rule 4: not subscribed
    ['initiated_only_ringing_drops', set('initiated'), 'ringing', '', false],
    ['completed_only_initiated_drops', set('completed'), 'initiated', '', false],
    ['completed_only_ringing_drops', set('completed'), 'ringing', '', false],
    ['ringing_only_busy_drops', set('ringing'), 'busy', '', false],
  ];
  for (const [name, events, event, wantName, wantEmit] of cases) {
    it(`${name} → (${wantName}, ${wantEmit})`, () => {
      expect(resolveEventName(events, event)).toEqual({ name: wantName, emit: wantEmit });
    });
  }
});
