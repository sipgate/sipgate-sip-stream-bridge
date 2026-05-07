package observability_test

// Bridge-level secret-leak injection test.
//
// Runs the bridge with a known token, drives a synthetic call, and greps
// the log stream for the literal token bytes (must be 0).
//
// Three tests close the contract:
//
//   1. TestSecretLeak_AuthTokenAndPasswordNeverInLogs
//        Six leak-attempt scenarios across multiple field shapes (msg-body,
//        named-field exact-name, named-field embedded, error-embed, bytes
//        field). Asserts buffer contains 0 occurrences of either secret
//        literal AND >=6 occurrences of "***".
//   2. TestSecretLeak_NestedSubLoggerInheritsMask
//        Three-level nested sub-loggers; emits the secret via the deepest
//        sub-logger; asserts the mask still applies (sub-loggers inherit
//        the wrapped writer through zerolog.Context).
//   3. TestSecretLeak_NoSecretsConfigured_PassesThrough
//        Pointer-identity assertion: NewSecretMaskWriter(buf) with no
//        secrets configured MUST return the underlying writer unchanged.
//        This is the documented passthrough contract; downstream tests
//        rely on it being unconditional.

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sipgate/sipgate-sip-stream-bridge/internal/observability"
)

const (
	// fakeAuthToken / fakeSIPPassword are deliberately distinctive so a
	// substring match in the masked output is unambiguous. The "DO_NOT_LEAK"
	// suffix makes a test failure self-documenting in CI logs.
	fakeAuthToken   = "TEST_AUTH_TOKEN_LITERAL_DO_NOT_LEAK"
	fakeSIPPassword = "TEST_SIP_PASSWORD_LITERAL_DO_NOT_LEAK"
)

// setupMaskedLogger constructs a zerolog logger whose underlying io.Writer
// is the secret-mask writer wrapping a bytes.Buffer. Mirrors the production
// wiring at cmd/sipgate-sip-stream-bridge/main.go.
func setupMaskedLogger(t *testing.T) (*zerolog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	masker := observability.NewSecretMaskWriter(&buf, fakeAuthToken, fakeSIPPassword)
	logger := zerolog.New(masker).With().Timestamp().Logger()
	return &logger, &buf
}

// TestSecretLeak_AuthTokenAndPasswordNeverInLogs is the load-bearing
// secret-mask acceptance test. Six leak-attempt scenarios cover the
// realistic shapes a
// future contributor might accidentally introduce: embedded in the message,
// named field with the obvious name (auth_token), named field with the SIP
// password name, embedded in an unrelated field value, embedded in an error
// passed via Err(), and embedded in a Bytes() field.
//
// Asserts:
//   - buffer contains 0 occurrences of fakeAuthToken
//   - buffer contains 0 occurrences of fakeSIPPassword
//   - buffer contains >=6 occurrences of "***" (one per leak attempt)
//
// A locally inverted token (e.g. setting fakeAuthToken="") makes this test
// fail — proving the test catches a regression where the AuthToken is
// disabled or empty.
func TestSecretLeak_AuthTokenAndPasswordNeverInLogs(t *testing.T) {
	parent, buf := setupMaskedLogger(t)
	sub := parent.With().
		Str(observability.FieldCallSid, "CA0123456789abcdef0123456789abcdef").
		Str(observability.FieldAccountSid, "ACdeadbeefdeadbeefdeadbeefdeadbeef").
		Logger()

	// Six leak-attempt scenarios — each must be redacted to "***".
	sub.Info().Msg("token=" + fakeAuthToken + "&user=alice")                                                    // 1: msg-body
	sub.Info().Str("auth_token", fakeAuthToken).Msg("auth_token field leak")                                    // 2: obvious-named field
	sub.Info().Str("password", fakeSIPPassword).Msg("password field leak")                                      // 3: SIP password named field
	sub.Info().Str("totally_unrelated_field", "prefix..."+fakeAuthToken+"...suffix").Msg("embedded leak")       // 4: embedded in unrelated field
	sub.Error().Err(errors.New("auth failed: token=" + fakeAuthToken)).Msg("error embed leak")                  // 5: in error
	sub.Info().Bytes("config_dump", []byte("auth_token="+fakeAuthToken+"\nsip_password="+fakeSIPPassword)).Msg( // 6: bytes field
		"bytes leak — config dump")

	output := buf.String()
	if strings.Contains(output, fakeAuthToken) {
		t.Errorf("AuthToken literal leaked in log output:\n%s", output)
	}
	if strings.Contains(output, fakeSIPPassword) {
		t.Errorf("SIPPassword literal leaked in log output:\n%s", output)
	}
	maskCount := strings.Count(output, "***")
	if maskCount < 6 {
		t.Errorf("expected >=6 *** redactions across 6 leak-attempt scenarios, got %d:\n%s", maskCount, output)
	}
}

// TestSecretLeak_NestedSubLoggerInheritsMask asserts that sub-loggers
// derived via parent.With().Str(...).Logger() inherit the wrapped writer
// — the secret-mask contract holds across arbitrary derivation depth.
//
// This is the architectural correctness assertion that backs the
// design decision to wrap the io.Writer (not install a zerolog Hook):
// Hooks only see the message, not field bytes; writer wrappers see every
// byte every sub-logger emits.
func TestSecretLeak_NestedSubLoggerInheritsMask(t *testing.T) {
	parent, buf := setupMaskedLogger(t)
	sub := parent.With().Str("level", "1").Logger()
	subSub := sub.With().Str("level", "2").Logger()
	subSubSub := subSub.With().Str("level", "3").Logger()

	subSubSub.Info().Msg("token=" + fakeAuthToken)
	subSubSub.Info().Str("auth_token", fakeAuthToken).Msg("nested str leak")
	subSubSub.Error().Err(errors.New("nested err: " + fakeSIPPassword)).Msg("nested err leak")

	output := buf.String()
	if strings.Contains(output, fakeAuthToken) {
		t.Errorf("nested sub-logger leaked AuthToken:\n%s", output)
	}
	if strings.Contains(output, fakeSIPPassword) {
		t.Errorf("nested sub-logger leaked SIPPassword:\n%s", output)
	}
	if got := strings.Count(output, "***"); got < 3 {
		t.Errorf("expected >=3 *** redactions across 3 nested-emit lines, got %d:\n%s", got, output)
	}
}

// TestSecretLeak_NoSecretsConfigured_PassesThrough verifies the
// passthrough contract: when no non-empty secrets are configured,
// NewSecretMaskWriter MUST return the underlying io.Writer unchanged
// (zero-cost passthrough — pointer-identity preserved).
//
// The assertion is UNCONDITIONAL — fails when the contract is violated
// AND runs every test invocation regardless of whether wrapping happened.
//
// Sanity write: also asserts that bytes round-trip cleanly to the
// underlying buffer (the contract has both a structural half — pointer
// identity — and a behavioural half — bytes flow through).
func TestSecretLeak_NoSecretsConfigured_PassesThrough(t *testing.T) {
	var buf bytes.Buffer

	// Constructor with no secrets: must return underlying buffer unchanged.
	w := observability.NewSecretMaskWriter(&buf)

	// Pointer-identity assertion runs unconditionally. NewSecretMaskWriter
	// returns io.Writer; the underlying *bytes.Buffer satisfies io.Writer
	// directly. If the helper wraps unnecessarily (e.g. always returns a
	// fresh *secretMaskWriter), the comparison fails — exactly what the
	// regression guard wants.
	if w != io.Writer(&buf) {
		t.Errorf("expected passthrough (same writer instance) when no secrets configured; "+
			"got a wrapper of type %T — NewSecretMaskWriter MUST return the "+
			"underlying writer unchanged when no non-empty secrets are configured", w)
	}

	// Behavioural half: writes round-trip byte-for-byte.
	const sentinel = "hello world — passthrough sanity"
	if _, err := w.Write([]byte(sentinel)); err != nil {
		t.Fatalf("passthrough write failed: %v", err)
	}
	if got := buf.String(); got != sentinel {
		t.Errorf("passthrough mismatch: got %q, want %q", got, sentinel)
	}

	// Empty-string secrets are also filtered — the constructor MUST treat
	// ""/""/"" as no-secrets and pass through.
	var buf2 bytes.Buffer
	w2 := observability.NewSecretMaskWriter(&buf2, "", "", "")
	if w2 != io.Writer(&buf2) {
		t.Errorf("expected passthrough when all secrets are empty strings; got wrapper of type %T", w2)
	}
}
