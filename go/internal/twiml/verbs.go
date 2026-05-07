// Package twiml implements a strict, hand-rolled parser and verb dispatcher for
// the Twilio Markup Language (TwiML) subset that v3.0 supports: <Response> root
// containing <Connect><Stream>, <Dial>+<Number>, <Hangup>, <Redirect>, <Reject>.
//
// The parser uses a token-walk over encoding/xml.Decoder.Token() rather than
// struct tags with the wildcard child marker. Reasons (per
// a known XML-pitfall): Go's encoding/xml populates BOTH
// the named-field pointer AND the wildcard slice for the same element,
// double-counting verbs. The token walker gives perfect control over which
// verbs are accepted and the order they appear in the dispatch list.
//
// Unknown verbs are retained as `unknownVerb` wrappers so the dispatcher can
// emit a per-verb warn-and-skip log (TWIML-05).
package twiml

import (
	"encoding/xml"
	"strings"
)

// Verb is a TwiML verb produced by the parser. Each concrete verb type below
// (Connect, Dial, Hangup, Redirect, Reject) implements XMLName() returning a
// stable element-local name used by the dispatcher to look up handlers.
type Verb interface {
	XMLName() xml.Name
}

// Response is the parsed root document. Verbs is in document order — the
// dispatcher walks the slice in order.
type Response struct {
	Verbs []Verb
}

// Connect carries a single nested <Stream>. v3.0 only supports <Stream> as a
// Connect child; other Connect children would be parsed as unknown and
// warned-and-skipped by the dispatcher.
type Connect struct {
	Stream *Stream
}

// XMLName implements Verb. Local must match the dispatcher handler key.
func (Connect) XMLName() xml.Name { return xml.Name{Local: "Connect"} }

// Stream models <Stream url="…" name="…" track="…"> with optional
// <Parameter name="…" value="…"/> children flattened into Parameters.
type Stream struct {
	URL        string
	Name       string
	Track      string
	Parameters map[string]string
}

// Dial supports both bare-chardata form (<Dial>+49…</Dial>) AND a <Number>
// child form (<Dial><Number>+49…</Number></Dial>) — see ResolveDialTarget for
// the disambiguation rules. The dispatcher routes Dial to the B2BUA
// forwarder (or to notImplHandler when forwarding is not wired).
type Dial struct {
	CallerID       string
	Timeout        *int
	TimeLimit      *int
	HangupOnStar   bool
	Action         string
	Method         string
	AnswerOnBridge *bool

	// Mutually-preferable target sources — at most one populated in a valid doc.
	NumberText string  // <Dial>+49…</Dial>
	Number     *Number // <Dial><Number>+49…</Number></Dial>

	// Rejected child markers — parser detects these for the dispatcher to warn on.
	HasSip        bool
	HasClient     bool
	HasConference bool
	HasQueue      bool

	// Parent-<Dial> status-callback subscription. Per-<Number> overrides
	// take precedence at DialOpts construction time (verb_dial.go).
	StatusCallback       string   // <Dial statusCallback="…">
	StatusCallbackMethod string   // "POST" | "GET"; empty defaults to POST at emission
	StatusCallbackEvents []string // tokenized event-name list; nil/empty = subscribe-to-all
}

// XMLName implements Verb.
func (Dial) XMLName() xml.Name { return xml.Name{Local: "Dial"} }

// Number is the <Number> child of <Dial>. Per-leg status-callback
// subscription attributes win over the parent <Dial> values.
// Backward compat: a bare-text <Number>+49…</Number> still parses with
// Text populated and the status fields zero-valued.
type Number struct {
	Text string

	// Per-<Number>-leg status-callback overrides. Resolution rule
	// (verb_dial.go): if StatusCallback != "" use it; otherwise fall
	// back to the parent <Dial> value.
	StatusCallback       string   // <Number statusCallback="…">
	StatusCallbackMethod string   // "POST" | "GET"
	StatusCallbackEvents []string // tokenized event-name list
}

// Hangup models <Hangup/>. The dispatcher routes this to hangupHandler which
// invokes DispatcherSession.Hangup() and returns ActionStop.
type Hangup struct{}

// XMLName implements Verb.
func (Hangup) XMLName() xml.Name { return xml.Name{Local: "Hangup"} }

// Redirect models <Redirect method="POST">https://…</Redirect>. Currently
// routed to the notImplHandler stub.
type Redirect struct {
	Method string
	URL    string
}

// XMLName implements Verb.
func (Redirect) XMLName() xml.Name { return xml.Name{Local: "Redirect"} }

// Reject models <Reject reason="busy|rejected"/>. Currently a stub —
// SIP-final-response mapping is not yet wired.
type Reject struct {
	Reason string
}

// XMLName implements Verb.
func (Reject) XMLName() xml.Name { return xml.Name{Local: "Reject"} }

// unknownVerb is the parser's wrapper for any element name inside <Response>
// that does not match a known verb. Retained in Response.Verbs so the
// dispatcher's warn-and-skip path sees them.
type unknownVerb struct {
	Name xml.Name
}

// XMLName implements Verb.
func (u unknownVerb) XMLName() xml.Name { return u.Name }

// ResolveDialTarget returns the dialed number from a Dial verb.
// Bare-text and <Number> child are equivalent; if BOTH are populated the
// caller is logged "ambiguous=true" and the <Number> child wins (matches
// Twilio behavior where structured data trumps inline chardata).
func (d *Dial) ResolveDialTarget() (target string, ambiguous bool) {
	text := strings.TrimSpace(d.NumberText)
	switch {
	case d.Number != nil && text != "":
		return strings.TrimSpace(d.Number.Text), true
	case d.Number != nil:
		return strings.TrimSpace(d.Number.Text), false
	case text != "":
		return text, false
	default:
		return "", false
	}
}
