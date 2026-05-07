package observability_test

// Log-correlation injection test.
//
// Four tests pin the log-correlation contract:
//
//   1. TestLogCorrelation_CallSidAccountSidPresent
//        Drives a synthetic call lifecycle (Info, Warn, Error) through a
//        zerolog sub-logger that carries call_sid + account_sid; parses
//        every emitted JSON line; asserts both correlation fields are
//        present with the expected values on every line.
//   2. TestLogCorrelation_ForwardLegID_OnDialLines
//        Same as Test 1 but the sub-sub-logger also carries
//        forward_leg_id (the FieldForwardLegID constant); asserts the
//        forward_leg_id field appears on every dial-leg-shaped line.
//   3. TestLogCorrelation_DebugLevel_LeaksAllowedFields
//        Documents the convention that phone-number / URL fields are
//        permitted at Debug level only. Configures a Debug-level logger,
//        emits Str("from", ...) / Str("url", ...) at Debug, asserts the
//        fields appear in the output. The lint walker enforces the
//        "not at Info+" half of the convention statically;
//        this test enforces the "may at Debug" half dynamically.
//   4. TestLogCorrelation_TwilioVsSIPCallID_Distinct
//        Asserts FieldCallSid != FieldSIPCallID (the Twilio CallSid /
//        SIP Call-ID header constants must be distinct strings) AND
//        emits a log line with both fields populated; both appear in
//        the parsed JSON, with their respective values.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sipgate/sipgate-sip-stream-bridge/internal/observability"
)

// setupCorrelationLogger constructs a zerolog logger that writes to a
// bytes.Buffer. Returns the parent logger (callers derive sub-loggers via
// .With().Str(...).Logger()) and the buffer for post-emit JSON parsing.
//
// The harness mirrors what production code does — it does NOT need to BE
// production code. Production wiring happens in
// cmd/sipgate-sip-stream-bridge/main.go via NewSecretMaskWriter; the test
// asserts the field-correlation contract that every per-session sub-logger
// in production is required to honour.
func setupCorrelationLogger(_ *testing.T) (*zerolog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf).With().Timestamp().Logger()
	return &logger, &buf
}

// parseLogLines splits the buffer on '\n', skips empty lines, and parses
// each remaining line as JSON. Returns the parsed maps in emit order.
func parseLogLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, raw := range bytes.Split(buf.Bytes(), []byte("\n")) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("parse %q: %v", string(raw), err)
		}
		lines = append(lines, m)
	}
	return lines
}

// TestLogCorrelation_CallSidAccountSidPresent constructs a sub-logger with
// FieldCallSid + FieldAccountSid; emits Info, Warn, Error events; asserts
// every emitted JSON line carries both fields with the expected values.
func TestLogCorrelation_CallSidAccountSidPresent(t *testing.T) {
	parent, buf := setupCorrelationLogger(t)

	const (
		wantCallSid    = "CA0123456789abcdef0123456789abcdef"
		wantAccountSid = "ACdeadbeefdeadbeefdeadbeefdeadbeef"
	)
	sub := parent.With().
		Str(observability.FieldCallSid, wantCallSid).
		Str(observability.FieldAccountSid, wantAccountSid).
		Logger()

	sub.Info().Msg("call started")
	sub.Warn().Str("reason", "test").Msg("synthetic warn")
	sub.Error().Msg("synthetic error")

	lines := parseLogLines(t, buf)
	if got := len(lines); got != 3 {
		t.Fatalf("expected 3 log lines, got %d:\n%s", got, buf.String())
	}
	for i, line := range lines {
		if got := line["call_sid"]; got != wantCallSid {
			t.Errorf("line %d (%v): call_sid=%v, want %q", i, line["level"], got, wantCallSid)
		}
		if got := line["account_sid"]; got != wantAccountSid {
			t.Errorf("line %d (%v): account_sid=%v, want %q", i, line["level"], got, wantAccountSid)
		}
	}
}

// TestLogCorrelation_ForwardLegID_OnDialLines constructs a sub-sub-logger
// with the additional FieldForwardLegID; asserts dial-leg-shaped log lines
// carry forward_leg_id. Mirrors the production sub-logger derivation in
// sip/forwarder.go where every <Dial>-leg log inherits both call_sid (the
// parent CallSid) and forward_leg_id (the DialCallSid).
func TestLogCorrelation_ForwardLegID_OnDialLines(t *testing.T) {
	parent, buf := setupCorrelationLogger(t)

	const (
		wantCallSid    = "CAabcdef0123456789abcdef0123456789"
		wantAccountSid = "ACdeadbeefcafebabedeadbeefcafebabe"
		wantForwardLeg = "CAdialeg00000000000000000000000001"
	)
	parentLeg := parent.With().
		Str(observability.FieldCallSid, wantCallSid).
		Str(observability.FieldAccountSid, wantAccountSid).
		Logger()
	dialLeg := parentLeg.With().
		Str(observability.FieldForwardLegID, wantForwardLeg).
		Logger()

	dialLeg.Info().Str("event", "initiated").Msg("dial-leg initiated")
	dialLeg.Info().Str("event", "ringing").Msg("dial-leg ringing")
	dialLeg.Warn().Str("event", "no-answer").Msg("dial-leg no-answer")

	lines := parseLogLines(t, buf)
	if got := len(lines); got != 3 {
		t.Fatalf("expected 3 dial-leg lines, got %d:\n%s", got, buf.String())
	}
	for i, line := range lines {
		if got := line["call_sid"]; got != wantCallSid {
			t.Errorf("line %d: call_sid=%v, want %q", i, got, wantCallSid)
		}
		if got := line["account_sid"]; got != wantAccountSid {
			t.Errorf("line %d: account_sid=%v, want %q", i, got, wantAccountSid)
		}
		if got := line["forward_leg_id"]; got != wantForwardLeg {
			t.Errorf("line %d: forward_leg_id=%v, want %q", i, got, wantForwardLeg)
		}
	}
}

// TestLogCorrelation_DebugLevel_LeaksAllowedFields documents the convention
// that phone-number / URL fields (`from`, `to`, `url`) are PERMITTED at
// Debug level. The lint walker statically enforces the
// "not at Info+" half of the rule by flagging Info()...Str("from", ...)
// call sites; this test asserts the Debug path remains open — emitting at
// Debug level produces a JSON line containing the field, which engineers
// can use to trace SIP / WS / REST flows during incident response.
//
// Note: this test does NOT assert that Info+ rejects these fields; the
// lint walker is the production enforcement. The test's purpose is to
// prove the convention is permissive at Debug (the supportive case).
func TestLogCorrelation_DebugLevel_LeaksAllowedFields(t *testing.T) {
	var buf bytes.Buffer
	// Configure logger at Debug level so Debug events pass the level filter.
	logger := zerolog.New(&buf).Level(zerolog.DebugLevel).With().Timestamp().Logger()

	logger.Debug().
		Str("from", "+4915123456789").
		Str("to", "+4930987654321").
		Str("url", "https://customer.example.com/twilio/callback?id=abc").
		Msg("forwarder: outbound INVITE detail")

	lines := parseLogLines(t, &buf)
	if got := len(lines); got != 1 {
		t.Fatalf("expected 1 debug line, got %d:\n%s", got, buf.String())
	}
	debug := lines[0]
	if got := debug["level"]; got != "debug" {
		t.Errorf("expected level=debug, got %v", got)
	}
	for _, field := range []string{"from", "to", "url"} {
		if _, ok := debug[field]; !ok {
			t.Errorf("expected %q field present at debug level, missing in: %v", field, debug)
		}
	}
}

// TestLogCorrelation_TwilioVsSIPCallID_Distinct asserts the Twilio-CallSid
// constant and the SIP Call-ID-header constant are distinct strings AND
// that a sub-logger setting both produces a JSON line with both fields
// populated, each with its expected value. This is the load-bearing
// distinction for cross-system correlation: SIP-layer logs link to packet
// captures via FieldSIPCallID; REST/webhook logs link to Twilio control
// plane via FieldCallSid; both are required, and they must be different
// JSON keys to coexist on the same line.
func TestLogCorrelation_TwilioVsSIPCallID_Distinct(t *testing.T) {
	if observability.FieldCallSid == observability.FieldSIPCallID {
		t.Fatalf("FieldCallSid and FieldSIPCallID must be distinct strings "+
			"(Twilio CallSid vs SIP Call-ID header — different identifiers); "+
			"both equal %q",
			observability.FieldCallSid)
	}

	parent, buf := setupCorrelationLogger(t)
	const (
		wantCallSid    = "CA0123456789abcdef0123456789abcdef"
		wantSIPCallID  = "550e8400-e29b-41d4-a716-446655440000@sipgate.de"
	)
	sub := parent.With().
		Str(observability.FieldCallSid, wantCallSid).
		Str(observability.FieldSIPCallID, wantSIPCallID).
		Logger()
	sub.Info().Msg("synthetic SIP/Twilio dual-correlation line")

	parsed := parseLogLines(t, buf)
	if got := len(parsed); got != 1 {
		t.Fatalf("expected 1 line, got %d:\n%s", got, buf.String())
	}
	line := parsed[0]
	if got := line["call_sid"]; got != wantCallSid {
		t.Errorf("call_sid=%v, want %q", got, wantCallSid)
	}
	if got := line["call_id"]; got != wantSIPCallID {
		t.Errorf("call_id=%v, want %q", got, wantSIPCallID)
	}
	// Belt-and-braces: ensure the two values aren't accidentally collapsed
	// into one JSON key.
	if line["call_sid"] == line["call_id"] {
		t.Errorf("call_sid and call_id collapsed to same value: %v", line["call_sid"])
	}
	// Belt-and-braces: ensure the raw JSON bytes contain both keys distinctly.
	raw := buf.String()
	if !strings.Contains(raw, `"call_sid":"`+wantCallSid+`"`) {
		t.Errorf(`raw JSON missing "call_sid":"...": %s`, raw)
	}
	if !strings.Contains(raw, `"call_id":"`+wantSIPCallID+`"`) {
		t.Errorf(`raw JSON missing "call_id":"...": %s`, raw)
	}
}
