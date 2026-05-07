package observability_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/observability"
)

// TestSecretMaskWriter_NilSecrets_PassesThrough.
//
// Constructor invariant: when no non-empty secrets are configured, the helper
// MUST return the underlying writer unchanged (zero-allocation passthrough).
// TestSecretLeak_NoSecretsConfigured_PassesThrough relies on this
// pointer-identity contract. Verified via reflect-free comparison: write
// "hello world", assert buf bytes equal AND the returned writer is the same
// pointer the caller passed in.
func TestSecretMaskWriter_NilSecrets_PassesThrough(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := observability.NewSecretMaskWriter(&buf)

	// Pointer-identity: the helper must return the underlying io.Writer
	// unchanged when no secrets are configured (zero-cost path).
	if w != io.Writer(&buf) {
		t.Fatalf("NewSecretMaskWriter(no secrets) must return underlying writer unchanged; got distinct wrapper %T", w)
	}

	const payload = "hello world"
	n, err := w.Write([]byte(payload))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("Write n: got %d, want %d", n, len(payload))
	}
	if got := buf.String(); got != payload {
		t.Fatalf("buf: got %q, want %q", got, payload)
	}
}

// TestSecretMaskWriter_RedactsRegardlessOfFieldName.
//
// Field-name agnostic redaction: the same secret bytes must be masked
// regardless of which JSON field name they appear under (or in the message
// body). This is the load-bearing acceptance contract — zerolog's Hook API
// can only see the message string, not field bytes; only a wrapping io.Writer
// achieves field-name-agnostic redaction.
func TestSecretMaskWriter_RedactsRegardlessOfFieldName(t *testing.T) {
	t.Parallel()

	const secret = "ABCDEFGHsupersecret"

	var buf bytes.Buffer
	w := observability.NewSecretMaskWriter(&buf, secret)

	// Three distinct JSON shapes that all carry the literal secret bytes:
	//   1. inside the message body
	//   2. as the value of a "named secret" field (auth_token)
	//   3. as the value of a custom field a developer might forget to redact
	jsonLines := []string{
		`{"level":"info","msg":"token=ABCDEFGHsupersecret embedded"}` + "\n",
		`{"level":"info","auth_token":"ABCDEFGHsupersecret","msg":"oops"}` + "\n",
		`{"level":"info","random_field":"ABCDEFGHsupersecret","msg":"more oops"}` + "\n",
	}
	for _, line := range jsonLines {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("buffer leaked secret bytes %q; full output:\n%s", secret, out)
	}
	if got := strings.Count(out, "***"); got != 3 {
		t.Fatalf("expected 3 occurrences of `***`, got %d; full output:\n%s", got, out)
	}
}

// TestSecretMaskWriter_LongerFirst.
//
// When two secrets are configured and one is a substring of the other, the
// implementation MUST mask the longer one first (as a single unit).
// Sort-by-descending-length is the standard algorithm; without it, the
// shorter substring's redaction would corrupt the longer secret's bytes
// (e.g. "PASSWORD" → "***" inside "PASSWORD123" yields "***123" — leaks
// the suffix).
func TestSecretMaskWriter_LongerFirst(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := observability.NewSecretMaskWriter(&buf, "PASSWORD", "PASSWORD123")

	if _, err := w.Write([]byte(`{"k":"PASSWORD123"}`)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	out := buf.String()
	// The whole "PASSWORD123" must be masked as ONE unit.
	if strings.Contains(out, "PASSWORD") {
		t.Fatalf("buffer leaked partial secret %q; got %q", "PASSWORD", out)
	}
	if strings.Contains(out, "123") {
		t.Fatalf("buffer leaked tail %q (longer-first ordering broken); got %q", "123", out)
	}
	if got := strings.Count(out, "***"); got != 1 {
		t.Fatalf("expected exactly 1 `***`, got %d; out=%q", got, out)
	}
}

// TestSecretMaskWriter_EmptySecretsIgnored.
//
// An empty-string secret MUST be ignored (filtering empty inputs prevents
// bytes.ReplaceAll(p, []byte(""), []byte("***")) from inflating every byte
// boundary in the stream into "***", which would corrupt every log line).
// Real secrets passed alongside empty strings are still redacted.
func TestSecretMaskWriter_EmptySecretsIgnored(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := observability.NewSecretMaskWriter(&buf, "", "real_token", "")

	if _, err := w.Write([]byte(`{"t":"real_token"} hello`)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, "real_token") {
		t.Fatalf("buffer leaked the configured non-empty secret; got %q", out)
	}
	// Sanity: " hello" must remain untouched (proves empty-secret filtering
	// did not insert spurious "***" between every byte).
	if !strings.Contains(out, " hello") {
		t.Fatalf("expected suffix ' hello' to survive untouched; got %q", out)
	}
	if got := strings.Count(out, "***"); got != 1 {
		t.Fatalf("expected exactly 1 `***`, got %d; out=%q", got, out)
	}
}

// TestSecretMaskWriter_ConcurrentWrites.
//
// 100 goroutines, each writing a kilobyte buffer that contains a single
// secret instance. The wrapper MUST be race-free under -race AND every
// instance of the secret bytes MUST be masked in the final aggregated
// buffer. This is the production concurrency model: zerolog sub-loggers
// share the same underlying io.Writer, and zerolog itself does NOT
// serialise writes across sub-loggers — the wrapper's mutex is the
// contract.
func TestSecretMaskWriter_ConcurrentWrites(t *testing.T) {
	t.Parallel()

	const (
		secret  = "WRITER_SECRET_42"
		workers = 100
	)

	var buf bytes.Buffer
	w := observability.NewSecretMaskWriter(&buf, secret)

	// Build a ~1KB payload with the secret embedded once near the middle.
	prefix := strings.Repeat("a", 480)
	suffix := strings.Repeat("z", 480) + "\n"
	payload := []byte(prefix + secret + suffix)

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			if _, err := w.Write(payload); err != nil {
				t.Errorf("Write: %v", err)
			}
		}()
	}
	wg.Wait()

	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("buffer leaked secret under concurrent writes; len(out)=%d", len(out))
	}
	if got := strings.Count(out, "***"); got != workers {
		t.Fatalf("expected %d `***` occurrences (one per write), got %d", workers, got)
	}
}

// TestFieldConstants_NotEmpty_AndDistinct.
//
// Compile-time stable string values + Twilio-vs-SIP correlation distinction:
//   - FieldCallSid    = "call_sid"  (Twilio CA[0-9a-f]{32})
//   - FieldSIPCallID  = "call_id"   (SIP Call-ID header — distinct identifier)
// Mixing these would erase the correlation distinction; tests on each side
// of the migration assert exact string values.
func TestFieldConstants_NotEmpty_AndDistinct(t *testing.T) {
	t.Parallel()

	if observability.FieldCallSid == "" {
		t.Error("FieldCallSid is empty")
	}
	if observability.FieldAccountSid == "" {
		t.Error("FieldAccountSid is empty")
	}
	if observability.FieldForwardLegID == "" {
		t.Error("FieldForwardLegID is empty")
	}
	if observability.FieldSIPCallID == "" {
		t.Error("FieldSIPCallID is empty")
	}

	if observability.FieldCallSid == observability.FieldSIPCallID {
		t.Errorf("FieldCallSid (%q) MUST be distinct from FieldSIPCallID (%q) — Twilio CallSid vs SIP Call-ID header are different identifiers",
			observability.FieldCallSid, observability.FieldSIPCallID)
	}
	if observability.FieldCallSid != "call_sid" {
		t.Errorf("FieldCallSid: got %q, want %q (load-bearing constant for the secret-mask migration grep)", observability.FieldCallSid, "call_sid")
	}
	if observability.FieldSIPCallID != "call_id" {
		t.Errorf("FieldSIPCallID: got %q, want %q (load-bearing constant — preserves SIP-layer correlation field)", observability.FieldSIPCallID, "call_id")
	}
	if observability.FieldAccountSid != "account_sid" {
		t.Errorf("FieldAccountSid: got %q, want %q", observability.FieldAccountSid, "account_sid")
	}
	if observability.FieldForwardLegID != "forward_leg_id" {
		t.Errorf("FieldForwardLegID: got %q, want %q", observability.FieldForwardLegID, "forward_leg_id")
	}
}

// TestSecretMaskWriter_PropagatesShortWrite.
//
// When the underlying io.Writer returns (n, nil) with n < len(masked),
// the wrapper MUST surface (n, io.ErrShortWrite) so the io.Writer
// contract is honored end-to-end. A previous version hid the short-write
// by returning (len(p), nil) — silently lossy on slow backends.
func TestSecretMaskWriter_PropagatesShortWrite(t *testing.T) {
	t.Parallel()
	shortWriter := &shortWritingWriter{halve: true}
	w := observability.NewSecretMaskWriter(shortWriter, "topsecret")

	const payload = "hello topsecret world"
	n, err := w.Write([]byte(payload))
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Write err: got %v, want io.ErrShortWrite", err)
	}
	// n is the count from the underlying writer (after masking shortened
	// the payload to "hello *** world", len 16, halved → 8). It MUST be
	// > 0 and < len(payload) so the caller can distinguish partial-write
	// from full-success.
	if n <= 0 || n >= len(payload) {
		t.Fatalf("Write n: got %d, want a partial count (> 0, < %d)", n, len(payload))
	}
}

// TestSecretMaskWriter_PropagatesUnderlyingError. Underlying-writer error
// propagates with the actual byte count, not 0.
func TestSecretMaskWriter_PropagatesUnderlyingError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("disk full")
	failingWriter := &erroringWriter{n: 5, err: wantErr}
	w := observability.NewSecretMaskWriter(failingWriter, "topsecret")

	n, err := w.Write([]byte("hello topsecret world"))
	if !errors.Is(err, wantErr) {
		t.Fatalf("Write err: got %v, want %v", err, wantErr)
	}
	if n != 5 {
		t.Fatalf("Write n: got %d, want 5 (underlying writer reported count, not fabricated 0)", n)
	}
}

// shortWritingWriter is a deliberately-broken io.Writer that returns
// half of len(p) when halve=true. err==nil to exercise the wrapper
// io.ErrShortWrite branch.
type shortWritingWriter struct {
	halve bool
}

func (s *shortWritingWriter) Write(p []byte) (int, error) {
	if s.halve {
		return len(p) / 2, nil
	}
	return len(p), nil
}

// erroringWriter returns a configured (n, err) tuple unchanged. Used to
// pin the contract that the wrapper propagates the actual byte count
// instead of fabricating 0.
type erroringWriter struct {
	n   int
	err error
}

func (e *erroringWriter) Write(p []byte) (int, error) {
	return e.n, e.err
}

// TestSecretMaskWriter_EndToEndZerolog — Task 2 Test 3 (integration shim).
//
// End-to-end coverage: construct a logger with the wrapper as its underlying
// writer; emit Str("auth_token", secret) AND embed the secret in Msg(); assert
// the secret bytes never appear in the JSON output. Mirrors the real main.go
// construction at cmd/sipgate-sip-stream-bridge/main.go so any regression in
// the wiring surfaces here.
func TestSecretMaskWriter_EndToEndZerolog(t *testing.T) {
	t.Parallel()

	const (
		authTokenLit = "TEST_AUTH_TOKEN_LITERAL"
		sipPwLit     = "TEST_SIP_PASSWORD_LITERAL"
	)

	var buf bytes.Buffer
	w := observability.NewSecretMaskWriter(&buf, authTokenLit, sipPwLit)
	logger := zerolog.New(w).With().Timestamp().Logger()

	// Emit secrets in three distinct shapes that downstream code can produce:
	//   - field value: Str("auth_token", ...)
	//   - field value with arbitrary name: Str("data", sipPwLit)
	//   - inside the message body: Msg("token=" + secret + " ...")
	logger.Info().
		Str("auth_token", authTokenLit).
		Str("data", sipPwLit).
		Msg("emitting token=" + authTokenLit + " and pw=" + sipPwLit + " inline")

	out := buf.String()
	if strings.Contains(out, authTokenLit) {
		t.Fatalf("AuthToken literal leaked into JSON output: %s", out)
	}
	if strings.Contains(out, sipPwLit) {
		t.Fatalf("SIPPassword literal leaked into JSON output: %s", out)
	}
	// Each secret appears at least twice in the construction (once as a field
	// value, once embedded in Msg()) — verify the buffer has at least 4
	// `***` markers (2 per secret × 2 secrets).
	if got := strings.Count(out, "***"); got < 4 {
		t.Fatalf("expected ≥4 `***` occurrences (2 per secret), got %d; out=%s", got, out)
	}
}
