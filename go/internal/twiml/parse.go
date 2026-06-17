package twiml

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
)

// ExtractStreamURL returns the url and Parameters from the first
// <Connect><Stream url=...> verb in resp. Returns an error if no such verb
// exists or if the <Stream> url attribute is empty.
func ExtractStreamURL(resp *Response) (string, map[string]string, error) {
	for _, v := range resp.Verbs {
		c, ok := v.(*Connect)
		if !ok || c.Stream == nil || c.Stream.URL == "" {
			continue
		}
		return c.Stream.URL, c.Stream.Parameters, nil
	}
	return "", nil, fmt.Errorf("twiml: no <Connect><Stream url=...> found in response")
}

// ParseStatusCallbackEvents tokenizes a StatusCallbackEvent attribute or
// form-param value. Twilio accepts BOTH space-separated AND comma-separated
// forms; this helper accepts both (and any mix) and rejects unknown event
// names. Returns (nil, nil) on empty input.
//
// Single source of truth — used by:
//   - parseDial / parseNumber (TwiML attribute path, this package)
//   - internal/api/calls.go parseModifyOpts (REST body-param path)
//
// Per RESEARCH §3.1, the documented enum is the union of <Dial>-leg events
// {initiated, ringing, answered, completed} and parent-call events
// {initiated, ringing, answered, in-progress, completed, busy, failed,
// no-answer, canceled}. The parser is event-vocab-agnostic: subscription
// validity-by-context is enforced at emission time (lifecycle and
// terminal emit paths drop events not in the customer's specific
// subscription).
//
// A strict enum gate ensures customer typos surface as a TwiML parse error
// rather than invisibly disabling subscriptions.
func ParseStatusCallbackEvents(raw string) ([]string, error) {
	tokens := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	})
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if !validStatusCallbackEvent(t) {
			return nil, fmt.Errorf("StatusCallbackEvent: unknown value %q", t)
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// validStatusCallbackEvent gates the Twilio-documented enum. Allow the full
// 9-value enum at parse time; emission code drops events not in the
// customer's subscription context.
func validStatusCallbackEvent(v string) bool {
	switch v {
	case "initiated", "ringing", "answered", "in-progress",
		"completed", "busy", "failed", "no-answer", "canceled":
		return true
	}
	return false
}

// errorCodeMalformed is the Twilio error code surfaced for any malformed-XML,
// wrong-root, or generic-parse-failure case (Twilio 12100 — "Document parse
// failure"). All Parse() failures funnel through this single code so the
// downstream webhook signer and REST status callbacks have one stable
// reason field.
const errorCodeMalformed = 12100

// Error is the structured failure type returned by Parse on any parse problem.
// Code is the Twilio error code; Message is the human-readable detail (the
// underlying xml package error string in most cases).
type Error struct {
	Code    int
	Message string
}

// Error implements the error interface so *Error can flow through standard
// error-returning Go APIs (the dispatcher wraps perr returns from Parse).
func (e *Error) Error() string { return fmt.Sprintf("twiml error %d: %s", e.Code, e.Message) }

// Parse consumes a TwiML body and returns either a *Response or a *Error with
// Code: 12100. The parser is a single forward token-walk — no struct tags, no
// reflective unmarshal — encoding/xml double-counts verbs when both a
// named-field pointer AND a wildcard child slice are populated for the
// same struct.
//
// Strict <Response> root: any other root element returns *Error{Code: 12100}.
// Empty input, EOF before a StartElement, malformed XML, and any other
// xml.Decoder error all funnel through code 12100 — Twilio surfaces a single
// "Document parse failure" code for the entire class, and downstream webhooks/
// REST callbacks need one stable reason field.
//
// Unknown verbs (anything other than Connect/Dial/Hangup/Redirect/Reject) are
// retained as unknownVerb{Name: …} so the dispatcher can emit a per-verb
// warn-and-skip log. The parser does NOT silently drop them.
func Parse(body []byte) (*Response, *Error) {
	if len(body) == 0 {
		return nil, &Error{Code: errorCodeMalformed, Message: "empty document"}
	}
	dec := xml.NewDecoder(bytes.NewReader(body))
	for {
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				return nil, &Error{Code: errorCodeMalformed, Message: "empty document"}
			}
			return nil, &Error{Code: errorCodeMalformed, Message: err.Error()}
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			// CharData, ProcInst, Directive, Comment — skip and continue scanning for root.
			continue
		}
		if se.Name.Local != "Response" {
			return nil, &Error{Code: errorCodeMalformed, Message: "root is not <Response>"}
		}
		return parseResponseChildren(dec)
	}
}

// parseResponseChildren walks the children of <Response> until the matching
// </Response> EndElement. Unknown elements are retained as unknownVerb so the
// dispatcher's warn-and-skip path (TWIML-05) sees them.
func parseResponseChildren(dec *xml.Decoder) (*Response, *Error) {
	resp := &Response{}
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, &Error{Code: errorCodeMalformed, Message: err.Error()}
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "Connect":
				c, perr := parseConnect(dec)
				if perr != nil {
					return nil, perr
				}
				resp.Verbs = append(resp.Verbs, c)
			case "Dial":
				d, perr := parseDial(dec, t)
				if perr != nil {
					return nil, perr
				}
				resp.Verbs = append(resp.Verbs, d)
			case "Hangup":
				if perr := skipToEnd(dec, t.Name); perr != nil {
					return nil, perr
				}
				resp.Verbs = append(resp.Verbs, &Hangup{})
			case "Redirect":
				r, perr := parseRedirect(dec, t)
				if perr != nil {
					return nil, perr
				}
				resp.Verbs = append(resp.Verbs, r)
			case "Reject":
				rj := parseReject(t)
				if perr := skipToEnd(dec, t.Name); perr != nil {
					return nil, perr
				}
				resp.Verbs = append(resp.Verbs, rj)
			default:
				// Unknown verb — consume its subtree, retain as unknownVerb for the dispatcher.
				if err := dec.Skip(); err != nil {
					return nil, &Error{Code: errorCodeMalformed, Message: err.Error()}
				}
				resp.Verbs = append(resp.Verbs, unknownVerb{Name: t.Name})
			}
		case xml.EndElement:
			if t.Name.Local == "Response" {
				return resp, nil
			}
			// Unmatched EndElement at Response level — treat as malformed.
			return nil, &Error{Code: errorCodeMalformed, Message: "unexpected </" + t.Name.Local + "> at Response level"}
		case xml.CharData, xml.Comment, xml.ProcInst, xml.Directive:
			// Whitespace / comments / processing-instructions inside <Response> are ignored.
			continue
		}
	}
}

// parseConnect consumes children of <Connect>. v3.0 only honors a single
// <Stream> child; any other child is silently consumed (the dispatcher logs a
// "<Connect> without <Stream>" warning if Stream is nil).
func parseConnect(dec *xml.Decoder) (*Connect, *Error) {
	c := &Connect{}
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, &Error{Code: errorCodeMalformed, Message: err.Error()}
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "Stream" {
				s, perr := parseStream(dec, t)
				if perr != nil {
					return nil, perr
				}
				c.Stream = s
				continue
			}
			// Unknown Connect child — consume subtree.
			if err := dec.Skip(); err != nil {
				return nil, &Error{Code: errorCodeMalformed, Message: err.Error()}
			}
		case xml.EndElement:
			if t.Name.Local == "Connect" {
				return c, nil
			}
		}
	}
}

// parseStream reads <Stream url=… name=… track=…> attributes and any nested
// <Parameter name=… value=…/> children into Stream.Parameters.
func parseStream(dec *xml.Decoder, start xml.StartElement) (*Stream, *Error) {
	s := &Stream{}
	for _, a := range start.Attr {
		switch a.Name.Local {
		case "url":
			s.URL = a.Value
		case "name":
			s.Name = a.Value
		case "track":
			s.Track = a.Value
		}
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, &Error{Code: errorCodeMalformed, Message: err.Error()}
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "Parameter" {
				var pn, pv string
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "name":
						pn = a.Value
					case "value":
						pv = a.Value
					}
				}
				if pn != "" {
					if s.Parameters == nil {
						s.Parameters = make(map[string]string)
					}
					s.Parameters[pn] = pv
				}
				if err := dec.Skip(); err != nil {
					return nil, &Error{Code: errorCodeMalformed, Message: err.Error()}
				}
				continue
			}
			// Unknown Stream child — skip subtree.
			if err := dec.Skip(); err != nil {
				return nil, &Error{Code: errorCodeMalformed, Message: err.Error()}
			}
		case xml.EndElement:
			if t.Name.Local == "Stream" {
				return s, nil
			}
		}
	}
}

// parseDial reads <Dial> attributes and walks children. Bare CharData becomes
// NumberText; a <Number> child populates Number.Text. Other Twilio child
// element names (<Sip>, <Client>, <Conference>, <Queue>) set the corresponding
// Has* flag for the dispatcher to warn on.
func parseDial(dec *xml.Decoder, start xml.StartElement) (*Dial, *Error) {
	d := &Dial{}
	for _, a := range start.Attr {
		switch a.Name.Local {
		case "callerId":
			d.CallerID = a.Value
		case "timeout":
			if v, err := strconv.Atoi(a.Value); err == nil {
				d.Timeout = &v
			}
		case "timeLimit":
			if v, err := strconv.Atoi(a.Value); err == nil {
				d.TimeLimit = &v
			}
		case "hangupOnStar":
			d.HangupOnStar = a.Value == "true"
		case "action":
			d.Action = a.Value
		case "method":
			d.Method = a.Value
		case "answerOnBridge":
			b := a.Value == "true"
			d.AnswerOnBridge = &b
		// Status-callback subscription attrs.
		case "statusCallback":
			d.StatusCallback = a.Value
		case "statusCallbackMethod":
			d.StatusCallbackMethod = a.Value
		case "statusCallbackEvent":
			events, err := ParseStatusCallbackEvents(a.Value)
			if err != nil {
				return nil, &Error{Code: errorCodeMalformed, Message: "<Dial>: " + err.Error()}
			}
			d.StatusCallbackEvents = events
		}
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, &Error{Code: errorCodeMalformed, Message: err.Error()}
		}
		switch t := tok.(type) {
		case xml.CharData:
			d.NumberText += string(t)
		case xml.StartElement:
			switch t.Name.Local {
			case "Number":
				n, perr := parseNumber(dec, t)
				if perr != nil {
					return nil, perr
				}
				d.Number = n
			case "Sip":
				d.HasSip = true
				if err := dec.Skip(); err != nil {
					return nil, &Error{Code: errorCodeMalformed, Message: err.Error()}
				}
			case "Client":
				d.HasClient = true
				if err := dec.Skip(); err != nil {
					return nil, &Error{Code: errorCodeMalformed, Message: err.Error()}
				}
			case "Conference":
				d.HasConference = true
				if err := dec.Skip(); err != nil {
					return nil, &Error{Code: errorCodeMalformed, Message: err.Error()}
				}
			case "Queue":
				d.HasQueue = true
				if err := dec.Skip(); err != nil {
					return nil, &Error{Code: errorCodeMalformed, Message: err.Error()}
				}
			default:
				if err := dec.Skip(); err != nil {
					return nil, &Error{Code: errorCodeMalformed, Message: err.Error()}
				}
			}
		case xml.EndElement:
			if t.Name.Local == "Dial" {
				return d, nil
			}
		}
	}
}

// parseNumber consumes <Number>+49…</Number> chardata. The signature
// accepts the StartElement so per-leg attrs (statusCallback,
// statusCallbackMethod, statusCallbackEvent) can be read before the
// chardata loop.
func parseNumber(dec *xml.Decoder, start xml.StartElement) (*Number, *Error) {
	n := &Number{}
	// Parse attrs BEFORE the chardata loop.
	for _, a := range start.Attr {
		switch a.Name.Local {
		case "statusCallback":
			n.StatusCallback = a.Value
		case "statusCallbackMethod":
			n.StatusCallbackMethod = a.Value
		case "statusCallbackEvent":
			events, err := ParseStatusCallbackEvents(a.Value)
			if err != nil {
				return nil, &Error{Code: errorCodeMalformed, Message: "<Number>: " + err.Error()}
			}
			n.StatusCallbackEvents = events
		}
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, &Error{Code: errorCodeMalformed, Message: err.Error()}
		}
		switch t := tok.(type) {
		case xml.CharData:
			n.Text += string(t)
		case xml.StartElement:
			// <Number> has no children in v3.0; skip any to be defensive.
			if err := dec.Skip(); err != nil {
				return nil, &Error{Code: errorCodeMalformed, Message: err.Error()}
			}
		case xml.EndElement:
			if t.Name.Local == "Number" {
				return n, nil
			}
		}
	}
}

// parseRedirect reads <Redirect method="…">URL</Redirect>.
func parseRedirect(dec *xml.Decoder, start xml.StartElement) (*Redirect, *Error) {
	r := &Redirect{}
	for _, a := range start.Attr {
		if a.Name.Local == "method" {
			r.Method = a.Value
		}
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, &Error{Code: errorCodeMalformed, Message: err.Error()}
		}
		switch t := tok.(type) {
		case xml.CharData:
			r.URL += string(t)
		case xml.StartElement:
			if err := dec.Skip(); err != nil {
				return nil, &Error{Code: errorCodeMalformed, Message: err.Error()}
			}
		case xml.EndElement:
			if t.Name.Local == "Redirect" {
				return r, nil
			}
		}
	}
}

// parseReject reads only the reason attribute. It does NOT consume to the
// EndElement — caller (parseResponseChildren) handles that via skipToEnd, so
// we keep the helper signature uniform for self-closing-tolerant elements.
func parseReject(start xml.StartElement) *Reject {
	r := &Reject{}
	for _, a := range start.Attr {
		if a.Name.Local == "reason" {
			r.Reason = a.Value
		}
	}
	return r
}

// skipToEnd consumes tokens until the matching EndElement for the given Name
// is seen. Tolerates self-closing tags (their EndElement is synthesized by the
// xml.Decoder immediately after the StartElement).
func skipToEnd(dec *xml.Decoder, name xml.Name) *Error {
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return &Error{Code: errorCodeMalformed, Message: err.Error()}
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			_ = t
		case xml.EndElement:
			if depth == 0 && t.Name.Local == name.Local {
				return nil
			}
			depth--
		}
	}
}
