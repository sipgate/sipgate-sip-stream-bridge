package twiml

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// stubTarget is a deterministic MidCallTarget used by Dispatch + hangupHandler
// tests. terminateCalls captures Terminate reasons in invocation order so each
// test can assert exact call-count and call-order semantics.
type stubTarget struct {
	terminateCalls []string // captures reasons in call order
	terminateErr   error    // optional - returned by Terminate
	logger         zerolog.Logger
}

func (s *stubTarget) Terminate(reason string) error {
	s.terminateCalls = append(s.terminateCalls, reason)
	return s.terminateErr
}

func (s *stubTarget) Log() *zerolog.Logger { return &s.logger }

func newStubTarget() *stubTarget {
	return &stubTarget{logger: zerolog.New(io.Discard)}
}

// parseOrFatal is a tiny helper that parses a TwiML XML string and fails the
// test on any parse error - keeps each test body focused on dispatch logic.
func parseOrFatal(t *testing.T, body string) *Response {
	t.Helper()
	doc, perr := Parse([]byte(body))
	if perr != nil {
		t.Fatalf("unexpected parse error for %q: %+v", body, perr)
	}
	if doc == nil {
		t.Fatalf("expected non-nil doc for %q", body)
	}
	return doc
}

// TestDispatch_NilDoc - defensive: nil document is a valid call (parser may
// have returned nil on a non-fatal path) and must not panic or terminate.
func TestDispatch_NilDoc(t *testing.T) {
	target := newStubTarget()
	if err := Dispatch(context.Background(), nil, target); err != nil {
		t.Fatalf("Dispatch(nil) returned err = %v, want nil", err)
	}
	if len(target.terminateCalls) != 0 {
		t.Fatalf("Terminate called %d times on nil doc, want 0 (calls=%v)",
			len(target.terminateCalls), target.terminateCalls)
	}
}

// TestDispatch_EmptyVerbs - empty doc.Verbs is a no-op walk; returns nil and
// never invokes Terminate.
func TestDispatch_EmptyVerbs(t *testing.T) {
	target := newStubTarget()
	doc := &Response{Verbs: nil}
	if err := Dispatch(context.Background(), doc, target); err != nil {
		t.Fatalf("Dispatch(empty) returned err = %v, want nil", err)
	}
	if len(target.terminateCalls) != 0 {
		t.Fatalf("Terminate called %d times on empty Verbs, want 0", len(target.terminateCalls))
	}
}

// TestDispatch_Hangup_Terminates - the canonical happy path:
// <Response><Hangup/></Response> walks one verb, hangupHandler invokes
// Terminate("hangup") exactly once, Dispatch returns nil.
func TestDispatch_Hangup_Terminates(t *testing.T) {
	target := newStubTarget()
	doc := parseOrFatal(t, `<Response><Hangup/></Response>`)
	if err := Dispatch(context.Background(), doc, target); err != nil {
		t.Fatalf("Dispatch returned err = %v, want nil", err)
	}
	if got, want := target.terminateCalls, []string{"hangup"}; !equalStrings(got, want) {
		t.Fatalf("terminateCalls = %v, want %v", got, want)
	}
}

// TestDispatch_HangupTerminal_VerbAfterUnreachable - terminal-verb semantic:
// any TwiML after <Hangup/> is unreachable (matches Twilio's documented
// behavior). The Dial after Hangup must NOT trigger any side effect on the
// target; Terminate is called exactly once.
func TestDispatch_HangupTerminal_VerbAfterUnreachable(t *testing.T) {
	target := newStubTarget()
	doc := parseOrFatal(t, `<Response><Hangup/><Dial>+4912345</Dial></Response>`)
	if err := Dispatch(context.Background(), doc, target); err != nil {
		t.Fatalf("Dispatch returned err = %v, want nil", err)
	}
	if got, want := target.terminateCalls, []string{"hangup"}; !equalStrings(got, want) {
		t.Fatalf("terminateCalls = %v, want %v (Hangup must be terminal — Dial after it unreachable)", got, want)
	}
}

// TestDispatch_DialWarnAndSkip - the dispatcher's mid-call surface here
// must warn-and-skip <Dial> and never call Terminate.
func TestDispatch_DialWarnAndSkip(t *testing.T) {
	target := newStubTarget()
	doc := parseOrFatal(t, `<Response><Dial>+4912345</Dial></Response>`)
	if err := Dispatch(context.Background(), doc, target); err != nil {
		t.Fatalf("Dispatch returned err = %v, want nil (Dial must warn-and-skip)", err)
	}
	if len(target.terminateCalls) != 0 {
		t.Fatalf("Terminate called %d times on Dial-only doc, want 0", len(target.terminateCalls))
	}
}

// TestDispatch_ConnectWarnAndSkip - mid-call <Connect><Stream> is out of scope
// for v3.0 (streaming is the implicit always-on default; switching streams via
// TwiML is deferred). Must warn-and-skip.
func TestDispatch_ConnectWarnAndSkip(t *testing.T) {
	target := newStubTarget()
	doc := parseOrFatal(t, `<Response><Connect><Stream url="wss://x/y"/></Connect></Response>`)
	if err := Dispatch(context.Background(), doc, target); err != nil {
		t.Fatalf("Dispatch returned err = %v, want nil", err)
	}
	if len(target.terminateCalls) != 0 {
		t.Fatalf("Terminate called %d times on Connect-only doc, want 0", len(target.terminateCalls))
	}
}

// TestDispatch_RejectWarnAndSkip - <Reject> is pre-answer-only in Twilio's
// model; mid-call it has no meaningful semantic. Must warn-and-skip.
func TestDispatch_RejectWarnAndSkip(t *testing.T) {
	target := newStubTarget()
	doc := parseOrFatal(t, `<Response><Reject reason="busy"/></Response>`)
	if err := Dispatch(context.Background(), doc, target); err != nil {
		t.Fatalf("Dispatch returned err = %v, want nil", err)
	}
	if len(target.terminateCalls) != 0 {
		t.Fatalf("Terminate called %d times on Reject-only doc, want 0", len(target.terminateCalls))
	}
}

// TestDispatch_RedirectWarnAndSkip - <Redirect> mid-call (without re-fetch
// machinery) is not implemented. Must warn-and-skip.
func TestDispatch_RedirectWarnAndSkip(t *testing.T) {
	target := newStubTarget()
	doc := parseOrFatal(t, `<Response><Redirect>https://x/twiml</Redirect></Response>`)
	if err := Dispatch(context.Background(), doc, target); err != nil {
		t.Fatalf("Dispatch returned err = %v, want nil", err)
	}
	if len(target.terminateCalls) != 0 {
		t.Fatalf("Terminate called %d times on Redirect-only doc, want 0", len(target.terminateCalls))
	}
}

// TestDispatch_UnknownVerbWarnAndSkip - the parser retains unrecognized verbs
// as unknownVerb wrappers (e.g. <Say>, <Play>). The dispatcher must surface
// them as warn-and-skip per TWIML-05; never propagate as error to the REST
// caller. Construct the doc manually so the test is independent of parser
// quirks.
func TestDispatch_UnknownVerbWarnAndSkip(t *testing.T) {
	target := newStubTarget()
	doc := &Response{Verbs: []Verb{
		unknownVerb{Name: xml.Name{Local: "Say"}},
	}}
	if err := Dispatch(context.Background(), doc, target); err != nil {
		t.Fatalf("Dispatch returned err = %v, want nil (unknown verb must warn-and-skip)", err)
	}
	if len(target.terminateCalls) != 0 {
		t.Fatalf("Terminate called %d times on unknown-only doc, want 0", len(target.terminateCalls))
	}
}

// TestDispatch_MultipleSkippedThenHangup - skipped verbs that precede a
// terminal Hangup must not interfere with the terminal verb running. Walks
// Dial → Connect → Hangup and asserts only Hangup ran.
func TestDispatch_MultipleSkippedThenHangup(t *testing.T) {
	target := newStubTarget()
	doc := parseOrFatal(t, `<Response><Dial>+4912345</Dial><Connect><Stream url="wss://x/y"/></Connect><Hangup/></Response>`)
	if err := Dispatch(context.Background(), doc, target); err != nil {
		t.Fatalf("Dispatch returned err = %v, want nil", err)
	}
	if got, want := target.terminateCalls, []string{"hangup"}; !equalStrings(got, want) {
		t.Fatalf("terminateCalls = %v, want %v (Hangup must run after skipped verbs)", got, want)
	}
}

// TestDispatch_TerminateErrorPropagated - if the target's Terminate fails
// (e.g. BYE could not be sent), the error propagates to Dispatch's caller so
// the REST handler can surface a 5xx + Twilio error code.
func TestDispatch_TerminateErrorPropagated(t *testing.T) {
	wantErr := errors.New("BYE failed")
	target := newStubTarget()
	target.terminateErr = wantErr
	doc := parseOrFatal(t, `<Response><Hangup/></Response>`)
	err := Dispatch(context.Background(), doc, target)
	if err == nil {
		t.Fatal("expected non-nil error when Terminate returns error, got nil")
	}
	if !errors.Is(err, wantErr) && !strings.Contains(err.Error(), "BYE failed") {
		t.Fatalf("Dispatch error = %v, want to wrap or equal %q", err, wantErr)
	}
	if got, want := target.terminateCalls, []string{"hangup"}; !equalStrings(got, want) {
		t.Fatalf("terminateCalls = %v, want %v", got, want)
	}
}

// equalStrings - small helper to avoid pulling in reflect.DeepEqual just for
// []string comparison (clearer test failures, no external dep).
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
