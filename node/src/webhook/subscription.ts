/**
 * Status-callback subscription resolution (port of go/internal/webhook/subscription.go).
 *
 * Single canonical helper for "should this event be emitted given the
 * customer's subscription?" — collapses previously-divergent local
 * interpretations. An empty/undefined Events set means "subscribe-to-all", the
 * documented Twilio default for `<Dial statusCallback="X">` without an explicit
 * `statusCallbackEvent` attribute.
 *
 * Pure leaf module: no I/O, no dependencies.
 */

/**
 * Terminal-class events: call lifecycle has reached an end state and no further
 * events will follow on this CallSid. Lifecycle events (initiated, ringing,
 * answered, in-progress) are NOT terminal — in-progress is excluded even though
 * it sometimes appears in terminal-shaped POSTs.
 */
const TERMINAL_EVENTS: ReadonlySet<string> = new Set(['completed', 'busy', 'failed', 'no-answer', 'canceled']);

/**
 * Returns true when eventName is a terminal-class event. Used by
 * subscriptionMatches and resolveEventName for the generic-completed fallback.
 */
export function isTerminalEvent(eventName: string): boolean {
  return TERMINAL_EVENTS.has(eventName);
}

/**
 * Returns true when the event should be emitted per the customer's
 * subscription. Resolution rules (per Twilio docs):
 *   1. undefined/empty events → subscribe-to-all (return true)
 *   2. specific event in events → return true
 *   3. fallback "completed" in events for terminal-class events → return true
 *   4. otherwise return false
 * Lifecycle events (initiated, ringing, answered, in-progress) do NOT fall back
 * to "completed" — only terminal-class events do.
 */
export function subscriptionMatches(events: Set<string> | undefined, eventName: string): boolean {
  if (events === undefined || events.size === 0) {
    return true;
  }
  if (events.has(eventName)) {
    return true;
  }
  if (isTerminalEvent(eventName) && events.has('completed')) {
    return true;
  }
  return false;
}

/**
 * Returns the event-vocab label to report for a subscribed event, plus whether
 * it should be emitted at all. The terminal-class fallback REMAPS the label to
 * "completed" (e.g. customer subscribed only to "completed" but the call hit
 * "busy" — the POST goes out with name="completed" while Form.CallStatus="busy").
 *
 * Returns:
 *   - { name: eventName, emit: true }    when the specific event is subscribed OR events is empty
 *   - { name: "completed", emit: true }  when the event is terminal-class AND only "completed" is subscribed
 *   - { name: "", emit: false }          when the event is not subscribed at all
 */
export function resolveEventName(events: Set<string> | undefined, eventName: string): { name: string; emit: boolean } {
  if (events === undefined || events.size === 0) {
    return { name: eventName, emit: true };
  }
  if (events.has(eventName)) {
    return { name: eventName, emit: true };
  }
  if (isTerminalEvent(eventName) && events.has('completed')) {
    return { name: 'completed', emit: true };
  }
  return { name: '', emit: false };
}
