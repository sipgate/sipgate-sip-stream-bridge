package webhook

// terminalEvents is the set of Twilio events classified as terminal —
// call lifecycle has reached an end state. Lifecycle events
// (initiated, ringing, answered, in-progress) are NOT terminal even though
// in-progress sometimes appears in terminal-shaped POSTs (when a call
// is modified mid-flow); the bookkeeping classification is whether the
// event implies "no further events will follow on this CallSid".
//
// Single canonical helper that collapses three previously-divergent
// local interpretations of "subscription Events" (sip/handler.go,
// sip/forwarder.go, bridge/session.go). Treats an empty/nil Events list
// as "subscribe-to-all" — the documented Twilio default for
// `<Dial statusCallback="X">` without an explicit `statusCallbackEvent`
// attribute.
var terminalEvents = map[string]struct{}{
	"completed": {},
	"busy":      {},
	"failed":    {},
	"no-answer": {},
	"canceled":  {},
}

// IsTerminalEvent returns true when the event name is a terminal-class
// event. Used by SubscriptionMatches and ResolveEventName for the
// generic-completed fallback rule.
func IsTerminalEvent(eventName string) bool {
	_, ok := terminalEvents[eventName]
	return ok
}

// SubscriptionMatches returns true when the event should be emitted per
// the customer's subscription. Resolution rules (per Twilio docs):
//
//  1. nil/empty Events → subscribe-to-all (return true)
//  2. specific event in Events → return true
//  3. fallback "completed" in Events for terminal-class events → return true
//  4. otherwise return false
//
// Lifecycle events (initiated, ringing, answered, in-progress) do NOT
// fall back to "completed" — only terminal-class events do.
//
// Replaces three divergent interpretations of empty Events
// (handler.go / forwarder.go / session.go each behaved differently).
// Previously a `<Dial statusCallback="X">` without an explicit
// statusCallbackEvent produced ZERO callbacks because the forwarder
// short-circuited on the empty events list.
func SubscriptionMatches(events map[string]struct{}, eventName string) bool {
	if len(events) == 0 {
		return true
	}
	if _, ok := events[eventName]; ok {
		return true
	}
	if IsTerminalEvent(eventName) {
		if _, ok := events["completed"]; ok {
			return true
		}
	}
	return false
}

// ResolveEventName returns the event-vocab label that should be reported
// for a subscribed event, plus whether the event should be emitted at all.
//
// Used by emitTerminalStatusCallback in bridge/session.go where the
// generic-completed fallback REMAPS the metric/event label to "completed"
// (e.g. customer subscribed only to "completed" but the call hit "busy" —
// the POST goes out with event="completed" but Form.CallStatus="busy").
//
// For lifecycle emit sites (handler.go / forwarder.go) where no remap is
// wanted, prefer SubscriptionMatches directly.
//
// Returns:
//   - (eventName, true)   when the specific event is subscribed OR Events is empty
//   - ("completed", true) when the event is terminal-class AND only "completed" is subscribed
//   - ("", false)         when the event is not subscribed at all
func ResolveEventName(events map[string]struct{}, eventName string) (string, bool) {
	if len(events) == 0 {
		return eventName, true
	}
	if _, ok := events[eventName]; ok {
		return eventName, true
	}
	if IsTerminalEvent(eventName) {
		if _, ok := events["completed"]; ok {
			return "completed", true
		}
	}
	return "", false
}
