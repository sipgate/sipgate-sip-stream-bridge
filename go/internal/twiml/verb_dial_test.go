package twiml

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"
)

// ── Stubs ────────────────────────────────────────────────────────────────────

// stubDialHandle tracks how many times Release() has been called.
type stubDialHandle struct {
	releaseCount int
}

func (h *stubDialHandle) Release() {
	h.releaseCount++
}

// stubDialTarget is a test double for DialTarget that records every call
// made by dialHandler with configurable return values.
type stubDialTarget struct {
	// Configurable return values
	prepareErr  error // returned by PrepareDial
	performErr  error // returned by PerformDial
	performRes  *DialResult

	// Captured call arguments
	prepareOpts   []DialOpts // one entry per PrepareDial call
	performCalls  int        // number of PerformDial calls
	performTarget string     // last target passed to PerformDial
	terminateCalls []string  // reasons passed to Terminate (in order)

	handle *stubDialHandle // handle returned by PrepareDial (on success)

	logger zerolog.Logger
}

func newStubDialTarget() *stubDialTarget {
	return &stubDialTarget{
		handle:     &stubDialHandle{},
		logger:     zerolog.Nop(),
		performRes: &DialResult{Status: "answered"},
	}
}

func (s *stubDialTarget) Log() *zerolog.Logger { return &s.logger }

func (s *stubDialTarget) Terminate(reason string) error {
	s.terminateCalls = append(s.terminateCalls, reason)
	return nil
}

func (s *stubDialTarget) PrepareDial(opts DialOpts) (DialHandle, error) {
	s.prepareOpts = append(s.prepareOpts, opts)
	if s.prepareErr != nil {
		return nil, s.prepareErr
	}
	return s.handle, nil
}

func (s *stubDialTarget) PerformDial(_ context.Context, target string, opts DialOpts, _ DialHandle) (*DialResult, error) {
	s.performCalls++
	s.performTarget = target
	_ = opts
	return s.performRes, s.performErr
}

// lastTerminateReason returns the last reason passed to Terminate, or "".
func (s *stubDialTarget) lastTerminateReason() string {
	if len(s.terminateCalls) == 0 {
		return ""
	}
	return s.terminateCalls[len(s.terminateCalls)-1]
}

// plainMCTAdapter satisfies MidCallTarget but NOT DialTarget — used to test
// the fallback warn-and-skip path in Dispatch when the adapter does not
// implement PrepareDial/PerformDial.
type plainMCTAdapter struct {
	log   zerolog.Logger
	terms []string
}

func (p *plainMCTAdapter) Log() *zerolog.Logger { return &p.log }
func (p *plainMCTAdapter) Terminate(reason string) error {
	p.terms = append(p.terms, reason)
	return nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// extractFirstDialVerb parses a full XML document and returns the first *Dial verb.
func extractFirstDialVerb(t *testing.T, xmlDoc string) *Dial {
	t.Helper()
	doc, err := Parse([]byte(xmlDoc))
	if err != nil {
		t.Fatalf("Parse(%q): %v", xmlDoc, err)
	}
	if len(doc.Verbs) == 0 {
		t.Fatalf("Parse(%q): no verbs", xmlDoc)
	}
	d, ok := doc.Verbs[0].(*Dial)
	if !ok {
		t.Fatalf("Parse(%q): first verb is %T, want *Dial", xmlDoc, doc.Verbs[0])
	}
	return d
}

// callDialHandler parses <Response>+dialXML and calls dialHandler.
func callDialHandler(t *testing.T, dialXML string, tgt *stubDialTarget) error {
	t.Helper()
	dial := extractFirstDialVerb(t, "<Response>"+dialXML+"</Response>")
	return dialHandler(context.Background(), dial, tgt)
}

// ── Tests ────────────────────────────────────────────────────────────────────

// 1. Bare-text target: <Dial>+49...</Dial> → PerformDial called; Terminate("completed")
func TestDialHandler_BareTextTarget(t *testing.T) {
	t.Parallel()

	tgt := newStubDialTarget()
	tgt.performRes = &DialResult{Status: "answered"}

	err := callDialHandler(t, "<Dial>+4912345</Dial>", tgt)
	if err != nil {
		t.Fatalf("dialHandler returned unexpected error: %v", err)
	}
	if tgt.performCalls != 1 {
		t.Errorf("performCalls = %d, want 1", tgt.performCalls)
	}
	if tgt.performTarget != "+4912345" {
		t.Errorf("performTarget = %q, want +4912345", tgt.performTarget)
	}
	if got := tgt.lastTerminateReason(); got != "completed" {
		t.Errorf("terminateReason = %q, want \"completed\"", got)
	}
}

// 2. <Number> child: <Dial><Number>+4912345</Number></Dial> → same outcome as bare-text
func TestDialHandler_NumberChild(t *testing.T) {
	t.Parallel()

	tgt := newStubDialTarget()
	tgt.performRes = &DialResult{Status: "answered"}

	err := callDialHandler(t, "<Dial><Number>+4912345</Number></Dial>", tgt)
	if err != nil {
		t.Fatalf("dialHandler returned unexpected error: %v", err)
	}
	if tgt.performCalls != 1 {
		t.Errorf("performCalls = %d, want 1", tgt.performCalls)
	}
	if tgt.performTarget != "+4912345" {
		t.Errorf("performTarget = %q, want +4912345", tgt.performTarget)
	}
	if got := tgt.lastTerminateReason(); got != "completed" {
		t.Errorf("terminateReason = %q, want \"completed\"", got)
	}
}

// 3. Ambiguous: both NumberText and Number child → Number wins; ambiguous=true logged
func TestDialHandler_AmbiguousLogged(t *testing.T) {
	t.Parallel()

	// Build a Dial struct manually to force both NumberText and Number to be set
	// simultaneously (the parser normally emits only one).
	num := "+4987654"
	dial := &Dial{
		NumberText: "+4911111",
		Number:     &Number{Text: num},
	}
	tgt := newStubDialTarget()
	tgt.performRes = &DialResult{Status: "answered"}

	err := dialHandler(context.Background(), dial, tgt)
	if err != nil {
		t.Fatalf("dialHandler returned unexpected error: %v", err)
	}
	if tgt.performCalls != 1 {
		t.Errorf("performCalls = %d, want 1", tgt.performCalls)
	}
	// <Number> wins over bare-text (Twilio precedence: structured child > chardata).
	if tgt.performTarget != num {
		t.Errorf("performTarget = %q, want %q (<Number> must win over bare-text)", tgt.performTarget, num)
	}
}

// 4. Empty target: <Dial></Dial> → no PrepareDial; no Terminate; nil error
func TestDialHandler_EmptyTarget(t *testing.T) {
	t.Parallel()

	tgt := newStubDialTarget()

	err := callDialHandler(t, "<Dial></Dial>", tgt)
	if err != nil {
		t.Fatalf("dialHandler returned unexpected error: %v", err)
	}
	if len(tgt.prepareOpts) != 0 {
		t.Errorf("PrepareDial was called %d times, want 0", len(tgt.prepareOpts))
	}
	if tgt.performCalls != 0 {
		t.Errorf("PerformDial was called %d times, want 0", tgt.performCalls)
	}
	if len(tgt.terminateCalls) != 0 {
		t.Errorf("Terminate was called %d times, want 0", len(tgt.terminateCalls))
	}
}

// 5. HasSip=true with no diallable target → warn log; PerformDial NOT called
func TestDialHandler_HasSipWarn(t *testing.T) {
	t.Parallel()

	// Dial with HasSip=true and no NumberText/Number — the Sip child is
	// recognized by the parser but has no E.164 target to dial.
	dial := &Dial{HasSip: true}
	tgt := newStubDialTarget()

	err := dialHandler(context.Background(), dial, tgt)
	if err != nil {
		t.Fatalf("dialHandler returned unexpected error: %v", err)
	}
	if tgt.performCalls != 0 {
		t.Errorf("PerformDial called %d times, want 0 (no target after Sip warn)", tgt.performCalls)
	}
}

// 6. PrepareDial fails → Terminate("failed") called; no PerformDial
func TestDialHandler_PrepareDialFails(t *testing.T) {
	t.Parallel()

	tgt := newStubDialTarget()
	tgt.prepareErr = errors.New("port pool exhausted")

	err := callDialHandler(t, "<Dial>+4912345</Dial>", tgt)
	if err != nil {
		t.Fatalf("dialHandler returned unexpected error: %v", err)
	}
	if tgt.performCalls != 0 {
		t.Errorf("PerformDial called %d times, want 0 (PrepareDial failed)", tgt.performCalls)
	}
	if got := tgt.lastTerminateReason(); got != "failed" {
		t.Errorf("terminateReason = %q, want \"failed\"", got)
	}
}

// 7. PerformDial returns Status="answered" → Terminate("completed")
func TestDialHandler_PerformDialAnswered(t *testing.T) {
	t.Parallel()

	tgt := newStubDialTarget()
	tgt.performRes = &DialResult{Status: "answered"}

	err := callDialHandler(t, "<Dial>+4912345</Dial>", tgt)
	if err != nil {
		t.Fatalf("dialHandler returned unexpected error: %v", err)
	}
	if got := tgt.lastTerminateReason(); got != "completed" {
		t.Errorf("terminateReason = %q, want \"completed\"", got)
	}
}

// 8. PerformDial returns Status="busy" → Terminate("busy")
func TestDialHandler_PerformDialBusy(t *testing.T) {
	t.Parallel()

	tgt := newStubDialTarget()
	tgt.performRes = &DialResult{Status: "busy"}

	err := callDialHandler(t, "<Dial>+4912345</Dial>", tgt)
	if err != nil {
		t.Fatalf("dialHandler returned unexpected error: %v", err)
	}
	if got := tgt.lastTerminateReason(); got != "busy" {
		t.Errorf("terminateReason = %q, want \"busy\"", got)
	}
}

// 9. PerformDial returns Status="no-answer" → Terminate("no-answer")
func TestDialHandler_PerformDialNoAnswer(t *testing.T) {
	t.Parallel()

	tgt := newStubDialTarget()
	tgt.performRes = &DialResult{Status: "no-answer"}

	err := callDialHandler(t, "<Dial>+4912345</Dial>", tgt)
	if err != nil {
		t.Fatalf("dialHandler returned unexpected error: %v", err)
	}
	if got := tgt.lastTerminateReason(); got != "no-answer" {
		t.Errorf("terminateReason = %q, want \"no-answer\"", got)
	}
}

// 10. PerformDial returns error (nil result) → Terminate("failed")
func TestDialHandler_PerformDialFailed(t *testing.T) {
	t.Parallel()

	tgt := newStubDialTarget()
	tgt.performRes = nil
	tgt.performErr = errors.New("forwarder: guardrails rejected dial")

	err := callDialHandler(t, "<Dial>+4912345</Dial>", tgt)
	if err != nil {
		t.Fatalf("dialHandler returned unexpected error: %v", err)
	}
	if got := tgt.lastTerminateReason(); got != "failed" {
		t.Errorf("terminateReason = %q, want \"failed\"", got)
	}
}

// 11. Handle released on success (PerformDial returns no error).
func TestDialHandler_HandleReleasedOnSuccess(t *testing.T) {
	t.Parallel()

	tgt := newStubDialTarget()
	tgt.performRes = &DialResult{Status: "answered"}

	err := callDialHandler(t, "<Dial>+4912345</Dial>", tgt)
	if err != nil {
		t.Fatalf("dialHandler returned unexpected error: %v", err)
	}
	if tgt.handle.releaseCount != 1 {
		t.Errorf("handle.releaseCount = %d, want 1", tgt.handle.releaseCount)
	}
}

// 12. Handle released on failure (PerformDial returns error).
func TestDialHandler_HandleReleasedOnFailure(t *testing.T) {
	t.Parallel()

	tgt := newStubDialTarget()
	tgt.performRes = nil
	tgt.performErr = errors.New("dial failed")

	err := callDialHandler(t, "<Dial>+4912345</Dial>", tgt)
	if err != nil {
		t.Fatalf("dialHandler returned unexpected error: %v", err)
	}
	if tgt.handle.releaseCount != 1 {
		t.Errorf("handle.releaseCount = %d, want 1 (must be released even on failure)", tgt.handle.releaseCount)
	}
}

// ── DialTarget via Dispatch ───────────────────────────────────────────────────

// TestDispatch_DialRouted: Dispatch routes *Dial to dialHandler when target
// implements DialTarget.
func TestDispatch_DialRouted(t *testing.T) {
	t.Parallel()

	tgt := newStubDialTarget()
	tgt.performRes = &DialResult{Status: "answered"}

	doc, err := Parse([]byte("<Response><Dial>+4912345</Dial></Response>"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := Dispatch(context.Background(), doc, tgt); err != nil {
		t.Fatalf("Dispatch returned unexpected error: %v", err)
	}
	if tgt.performCalls != 1 {
		t.Errorf("PerformDial not called; performCalls = %d, want 1", tgt.performCalls)
	}
}

// ── Number overrides Dial precedence ────────────────────────────────────────

// TestDial_NumberOverridesParentStatusCallback — when both <Dial
// statusCallback> AND <Number statusCallback> are present, the <Number>
// per-leg attribute wins (Twilio's documented per-leg
// precedence). Asserted via the captured DialOpts inside stubDialTarget.
func TestDial_NumberOverridesParentStatusCallback(t *testing.T) {
	t.Parallel()

	dial := &Dial{
		StatusCallback:       "https://parent.example/cb",
		StatusCallbackMethod: "POST",
		StatusCallbackEvents: []string{"initiated", "completed"},
		Number: &Number{
			Text:                 "+4912345",
			StatusCallback:       "https://leg.example/cb",
			StatusCallbackMethod: "GET",
			StatusCallbackEvents: []string{"answered", "completed"},
		},
	}

	tgt := newStubDialTarget()
	tgt.performRes = &DialResult{Status: "answered"}

	if err := dialHandler(context.Background(), dial, tgt); err != nil {
		t.Fatalf("dialHandler returned unexpected error: %v", err)
	}
	if len(tgt.prepareOpts) != 1 {
		t.Fatalf("PrepareDial called %d times, want 1", len(tgt.prepareOpts))
	}
	opts := tgt.prepareOpts[0]
	if opts.StatusCallback != "https://leg.example/cb" {
		t.Errorf("StatusCallback = %q, want \"https://leg.example/cb\" (Number overrides Dial)", opts.StatusCallback)
	}
	if opts.StatusCallbackMethod != "GET" {
		t.Errorf("StatusCallbackMethod = %q, want GET", opts.StatusCallbackMethod)
	}
	wantEvents := []string{"answered", "completed"}
	if len(opts.StatusCallbackEvents) != len(wantEvents) {
		t.Fatalf("StatusCallbackEvents = %v, want %v", opts.StatusCallbackEvents, wantEvents)
	}
	for i, e := range wantEvents {
		if opts.StatusCallbackEvents[i] != e {
			t.Errorf("StatusCallbackEvents[%d] = %q, want %q", i, opts.StatusCallbackEvents[i], e)
		}
	}
}

// TestDial_StatusCallback_DialOnlyPropagates — when <Number> has no
// statusCallback, the parent <Dial> values are used.
func TestDial_StatusCallback_DialOnlyPropagates(t *testing.T) {
	t.Parallel()

	dial := &Dial{
		StatusCallback:       "https://parent.example/cb",
		StatusCallbackMethod: "POST",
		StatusCallbackEvents: []string{"initiated", "completed"},
		Number: &Number{Text: "+4912345"},
	}

	tgt := newStubDialTarget()
	tgt.performRes = &DialResult{Status: "answered"}

	if err := dialHandler(context.Background(), dial, tgt); err != nil {
		t.Fatalf("dialHandler returned unexpected error: %v", err)
	}
	opts := tgt.prepareOpts[0]
	if opts.StatusCallback != "https://parent.example/cb" {
		t.Errorf("StatusCallback = %q, want parent URL (Number had no override)", opts.StatusCallback)
	}
	if opts.StatusCallbackMethod != "POST" {
		t.Errorf("StatusCallbackMethod = %q, want POST", opts.StatusCallbackMethod)
	}
}

// TestDispatch_DialFallbackWarnAndSkip: when target does NOT implement
// DialTarget, *Dial is warned-and-skipped (no panic, nil error, no Terminate).
func TestDispatch_DialFallbackWarnAndSkip(t *testing.T) {
	t.Parallel()

	pm := &plainMCTAdapter{log: zerolog.Nop()}

	doc, err := Parse([]byte("<Response><Dial>+4912345</Dial></Response>"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := Dispatch(context.Background(), doc, pm); err != nil {
		t.Fatalf("Dispatch returned unexpected error: %v", err)
	}
	// Dial was warn-and-skipped: Terminate should NOT have been called.
	if len(pm.terms) != 0 {
		t.Errorf("Terminate called %d times, want 0 (warn-and-skip)", len(pm.terms))
	}
}
