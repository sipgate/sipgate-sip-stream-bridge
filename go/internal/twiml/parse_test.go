package twiml

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// TestParseRootNotResponse — strict <Response> root: any other root returns
// *Error with Twilio code 12100.
func TestParseRootNotResponse(t *testing.T) {
	body := []byte(`<NotResponse><Hangup/></NotResponse>`)
	doc, perr := Parse(body)
	if doc != nil {
		t.Fatalf("expected nil doc on non-Response root, got %+v", doc)
	}
	if perr == nil {
		t.Fatal("expected *Error on non-Response root, got nil")
	}
	if perr.Code != 12100 {
		t.Fatalf("expected Code 12100, got %d (msg=%q)", perr.Code, perr.Message)
	}
}

// TestParseMalformedReturns12100 — truncated/malformed XML returns *Error code 12100.
func TestParseMalformedReturns12100(t *testing.T) {
	body := []byte(`<Response><Hangup`)
	doc, perr := Parse(body)
	if doc != nil {
		t.Fatalf("expected nil doc on malformed input, got %+v", doc)
	}
	if perr == nil {
		t.Fatal("expected *Error on malformed input, got nil")
	}
	if perr.Code != 12100 {
		t.Fatalf("expected Code 12100, got %d (msg=%q)", perr.Code, perr.Message)
	}
}

// TestParseUnknownVerbsRetained — parser MUST retain unknown verbs (e.g. <Say>)
// so the dispatcher can warn-and-skip them per TWIML-05. The known <Hangup>
// must also be present in document order.
func TestParseUnknownVerbsRetained(t *testing.T) {
	body := []byte(`<Response><Say>hi</Say><Hangup/></Response>`)
	doc, perr := Parse(body)
	if perr != nil {
		t.Fatalf("unexpected parse error: %+v", perr)
	}
	if doc == nil {
		t.Fatal("expected non-nil doc")
	}
	if len(doc.Verbs) != 2 {
		t.Fatalf("expected 2 verbs (Say + Hangup), got %d: %+v", len(doc.Verbs), doc.Verbs)
	}
	if got := doc.Verbs[0].XMLName().Local; got != "Say" {
		t.Fatalf("expected first verb Local=Say, got %q", got)
	}
	if got := doc.Verbs[1].XMLName().Local; got != "Hangup" {
		t.Fatalf("expected second verb Local=Hangup, got %q", got)
	}
}

// TestParseTwiMLDialBareTextAndNumberChild — TWIML-06: <Dial> accepts both bare
// chardata (<Dial>+49…</Dial>) and a <Number> child equivalently. Both fixtures
// must yield ResolveDialTarget == ("+4912345", false).
func TestParseTwiMLDialBareTextAndNumberChild(t *testing.T) {
	cases := []struct {
		name    string
		fixture string
	}{
		{"bare-text", "testdata/dial-bare-text.xml"},
		{"number-child", "testdata/dial-number-child.xml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := os.ReadFile(tc.fixture)
			if err != nil {
				t.Fatalf("read fixture %s: %v", tc.fixture, err)
			}
			doc, perr := Parse(body)
			if perr != nil {
				t.Fatalf("unexpected parse error on %s: %+v", tc.fixture, perr)
			}
			if doc == nil || len(doc.Verbs) != 1 {
				t.Fatalf("expected exactly 1 verb in %s, got doc=%+v", tc.fixture, doc)
			}
			d, ok := doc.Verbs[0].(*Dial)
			if !ok {
				t.Fatalf("expected *Dial, got %T", doc.Verbs[0])
			}
			target, ambiguous := d.ResolveDialTarget()
			if target != "+4912345" {
				t.Fatalf("ResolveDialTarget target = %q, want %q", target, "+4912345")
			}
			if ambiguous {
				t.Fatalf("ResolveDialTarget ambiguous = true, want false (only one source populated)")
			}
		})
	}
}

// TestResolveDialTargetAmbiguous — both <Number> child AND bare chardata populated
// returns ambiguous=true and prefers the <Number> child value.
func TestResolveDialTargetAmbiguous(t *testing.T) {
	d := &Dial{
		NumberText: " +4900000 ",
		Number:     &Number{Text: "+4912345"},
	}
	target, ambiguous := d.ResolveDialTarget()
	if !ambiguous {
		t.Fatalf("expected ambiguous=true when both NumberText and Number populated")
	}
	if target != "+4912345" {
		t.Fatalf("expected Number.Text to win when ambiguous, got %q", target)
	}
}

// TestParseConnectStreamURL — <Connect><Stream url="…"/></Connect> populates
// Connect.Stream.URL.
func TestParseConnectStreamURL(t *testing.T) {
	body := []byte(`<Response><Connect><Stream url="wss://x/y"/></Connect></Response>`)
	doc, perr := Parse(body)
	if perr != nil {
		t.Fatalf("unexpected parse error: %+v", perr)
	}
	if doc == nil || len(doc.Verbs) != 1 {
		t.Fatalf("expected exactly 1 verb, got doc=%+v", doc)
	}
	c, ok := doc.Verbs[0].(*Connect)
	if !ok {
		t.Fatalf("expected *Connect, got %T", doc.Verbs[0])
	}
	if c.Stream == nil {
		t.Fatal("expected Connect.Stream non-nil")
	}
	if c.Stream.URL != "wss://x/y" {
		t.Fatalf("Stream.URL = %q, want %q", c.Stream.URL, "wss://x/y")
	}
}

// TestParseHangupSelfClosing — <Hangup/> appended as *Hangup verb.
func TestParseHangupSelfClosing(t *testing.T) {
	body := []byte(`<Response><Hangup/></Response>`)
	doc, perr := Parse(body)
	if perr != nil {
		t.Fatalf("unexpected parse error: %+v", perr)
	}
	if doc == nil || len(doc.Verbs) != 1 {
		t.Fatalf("expected exactly 1 verb, got doc=%+v", doc)
	}
	if _, ok := doc.Verbs[0].(*Hangup); !ok {
		t.Fatalf("expected *Hangup, got %T", doc.Verbs[0])
	}
}

// TestParseEmptyDocument — empty input returns *Error code 12100.
func TestParseEmptyDocument(t *testing.T) {
	doc, perr := Parse(nil)
	if doc != nil || perr == nil {
		t.Fatalf("expected nil doc + non-nil err on empty input, got doc=%+v err=%+v", doc, perr)
	}
	if perr.Code != 12100 {
		t.Fatalf("expected Code 12100 on empty input, got %d", perr.Code)
	}
	// Error.Error() must surface the code.
	if !strings.Contains(perr.Error(), "12100") {
		t.Fatalf("expected Error() string to contain code 12100, got %q", perr.Error())
	}
}

// TestParseRedirectAndRejectAttributes — Redirect and Reject parse with the
// expected attributes/chardata.
func TestParseRedirectAndRejectAttributes(t *testing.T) {
	body := []byte(`<Response><Redirect method="POST">https://x/twiml</Redirect><Reject reason="busy"/></Response>`)
	doc, perr := Parse(body)
	if perr != nil {
		t.Fatalf("unexpected parse error: %+v", perr)
	}
	if doc == nil || len(doc.Verbs) != 2 {
		t.Fatalf("expected 2 verbs, got doc=%+v", doc)
	}
	r, ok := doc.Verbs[0].(*Redirect)
	if !ok {
		t.Fatalf("expected *Redirect, got %T", doc.Verbs[0])
	}
	if r.Method != "POST" {
		t.Fatalf("Redirect.Method = %q, want POST", r.Method)
	}
	if r.URL != "https://x/twiml" {
		t.Fatalf("Redirect.URL = %q, want https://x/twiml", r.URL)
	}
	rj, ok := doc.Verbs[1].(*Reject)
	if !ok {
		t.Fatalf("expected *Reject, got %T", doc.Verbs[1])
	}
	if rj.Reason != "busy" {
		t.Fatalf("Reject.Reason = %q, want busy", rj.Reason)
	}
}

// ── Status-callback parsing on <Dial> + <Number> ──────────────────────────

// firstDial returns doc.Verbs[0].(*Dial) or fails the test.
func firstDial(t *testing.T, body []byte) *Dial {
	t.Helper()
	resp, perr := Parse(body)
	if perr != nil {
		t.Fatalf("Parse: %v", perr)
	}
	if resp == nil || len(resp.Verbs) == 0 {
		t.Fatalf("Parse: no verbs")
	}
	d, ok := resp.Verbs[0].(*Dial)
	if !ok {
		t.Fatalf("first verb = %T, want *Dial", resp.Verbs[0])
	}
	return d
}

// TestParseDial_StatusCallback — <Dial statusCallback statusCallbackMethod
// statusCallbackEvent> populates the three new fields.
func TestParseDial_StatusCallback(t *testing.T) {
	t.Parallel()
	body := []byte(`<Response><Dial statusCallback="https://x.example/cb" statusCallbackMethod="POST" statusCallbackEvent="initiated ringing answered completed">+4915123456789</Dial></Response>`)
	d := firstDial(t, body)
	if d.StatusCallback != "https://x.example/cb" {
		t.Errorf("StatusCallback = %q, want https://x.example/cb", d.StatusCallback)
	}
	if d.StatusCallbackMethod != "POST" {
		t.Errorf("StatusCallbackMethod = %q, want POST", d.StatusCallbackMethod)
	}
	want := []string{"initiated", "ringing", "answered", "completed"}
	if !reflect.DeepEqual(d.StatusCallbackEvents, want) {
		t.Errorf("StatusCallbackEvents = %v, want %v", d.StatusCallbackEvents, want)
	}
}

// TestParseNumber_StatusCallback — per-leg <Number statusCallback…> attrs are
// preserved through parseNumber. Text chardata stays intact.
func TestParseNumber_StatusCallback(t *testing.T) {
	t.Parallel()
	body := []byte(`<Response><Dial><Number statusCallback="https://leg.example/cb" statusCallbackMethod="GET" statusCallbackEvent="answered completed">+4915123456789</Number></Dial></Response>`)
	d := firstDial(t, body)
	n := d.Number
	if n == nil {
		t.Fatal("Number nil")
	}
	if n.Text != "+4915123456789" {
		t.Errorf("Text = %q", n.Text)
	}
	if n.StatusCallback != "https://leg.example/cb" {
		t.Errorf("StatusCallback = %q", n.StatusCallback)
	}
	if n.StatusCallbackMethod != "GET" {
		t.Errorf("StatusCallbackMethod = %q", n.StatusCallbackMethod)
	}
	want := []string{"answered", "completed"}
	if !reflect.DeepEqual(n.StatusCallbackEvents, want) {
		t.Errorf("StatusCallbackEvents = %v, want %v", n.StatusCallbackEvents, want)
	}
}

// TestParseNumber_BackwardCompat — bare-text <Number>+49…</Number> (the
// legacy shape) MUST continue to parse; status-callback fields zero-valued.
func TestParseNumber_BackwardCompat(t *testing.T) {
	t.Parallel()
	body := []byte(`<Response><Dial><Number>+4915123456789</Number></Dial></Response>`)
	d := firstDial(t, body)
	n := d.Number
	if n == nil {
		t.Fatal("Number nil")
	}
	if n.Text != "+4915123456789" {
		t.Errorf("Text = %q (backward compat broken)", n.Text)
	}
	if n.StatusCallback != "" || n.StatusCallbackMethod != "" || len(n.StatusCallbackEvents) != 0 {
		t.Errorf("zero-valued status fields expected; got URL=%q method=%q events=%v",
			n.StatusCallback, n.StatusCallbackMethod, n.StatusCallbackEvents)
	}
}

// TestParseDial_StatusCallbackEvent_CommaSeparated — I-3 fix: comma form
// parses identically to space form.
func TestParseDial_StatusCallbackEvent_CommaSeparated(t *testing.T) {
	t.Parallel()
	body := []byte(`<Response><Dial statusCallback="https://x.example/cb" statusCallbackEvent="initiated,ringing,answered,completed">+49</Dial></Response>`)
	d := firstDial(t, body)
	want := []string{"initiated", "ringing", "answered", "completed"}
	if !reflect.DeepEqual(d.StatusCallbackEvents, want) {
		t.Errorf("StatusCallbackEvents = %v, want %v", d.StatusCallbackEvents, want)
	}
}

// TestParseDial_StatusCallbackEvent_MixedSeparators — mixed comma+space
// tokenization parses cleanly.
func TestParseDial_StatusCallbackEvent_MixedSeparators(t *testing.T) {
	t.Parallel()
	body := []byte(`<Response><Dial statusCallback="https://x.example/cb" statusCallbackEvent="initiated, ringing answered,completed">+49</Dial></Response>`)
	d := firstDial(t, body)
	want := []string{"initiated", "ringing", "answered", "completed"}
	if !reflect.DeepEqual(d.StatusCallbackEvents, want) {
		t.Errorf("StatusCallbackEvents = %v, want %v", d.StatusCallbackEvents, want)
	}
}

// TestParseDial_StatusCallbackEvent_RejectsUnknown — I-3 fix: unknown event
// name surfaces as a TwiML parse error citing the unknown value.
func TestParseDial_StatusCallbackEvent_RejectsUnknown(t *testing.T) {
	t.Parallel()
	body := []byte(`<Response><Dial statusCallback="https://x.example/cb" statusCallbackEvent="initiated foo">+49</Dial></Response>`)
	_, perr := Parse(body)
	if perr == nil {
		t.Fatal("expected parse error for unknown event \"foo\"; got nil")
	}
	if perr.Code != 12100 {
		t.Errorf("err code = %d, want 12100", perr.Code)
	}
	if !strings.Contains(perr.Message, "foo") {
		t.Errorf("err message = %q; expected to cite \"foo\"", perr.Message)
	}
}

// TestParseNumber_StatusCallbackEvent_RejectsUnknown — same enum-gate on the
// per-<Number> path.
func TestParseNumber_StatusCallbackEvent_RejectsUnknown(t *testing.T) {
	t.Parallel()
	body := []byte(`<Response><Dial><Number statusCallback="https://x.example/cb" statusCallbackEvent="answered ringinX">+49</Number></Dial></Response>`)
	_, perr := Parse(body)
	if perr == nil {
		t.Fatal("expected parse error for typo \"ringinX\"; got nil")
	}
	if !strings.Contains(perr.Message, "ringinX") {
		t.Errorf("err message = %q; expected to cite \"ringinX\"", perr.Message)
	}
}

// TestParseStatusCallbackEvents_Empty — empty input returns (nil, nil) so
// callers do not need a sentinel branch for "no events specified".
func TestParseStatusCallbackEvents_Empty(t *testing.T) {
	t.Parallel()
	got, err := ParseStatusCallbackEvents("")
	if err != nil {
		t.Fatalf("err = %v, want nil for empty input", err)
	}
	if got != nil {
		t.Errorf("got = %v, want nil", got)
	}
}

// TestParseStatusCallbackEvents_FullEnum — every Twilio-documented event name
// is accepted (parser is event-vocab-agnostic; emission code does context-
// specific subset filtering).
func TestParseStatusCallbackEvents_FullEnum(t *testing.T) {
	t.Parallel()
	full := "initiated ringing answered in-progress completed busy failed no-answer canceled"
	got, err := ParseStatusCallbackEvents(full)
	if err != nil {
		t.Fatalf("err = %v, want nil for full enum", err)
	}
	want := []string{
		"initiated", "ringing", "answered", "in-progress",
		"completed", "busy", "failed", "no-answer", "canceled",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got = %v, want %v", got, want)
	}
}
