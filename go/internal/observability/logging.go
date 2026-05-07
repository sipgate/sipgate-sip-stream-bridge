// Package observability — logging.go provides structured-logging correlation
// constants and a secret-masking io.Writer wrapper for zerolog.
//
// # Architectural rationale: writer-wrap, NOT a zerolog event-hook
//
// zerolog's stable event-hook API only sees the event Level + message string, NOT
// the field bytes. To redact secrets that may appear inside arbitrary field
// values (e.g. a developer accidentally calls Str("auth_token", token) or a
// downstream library embeds the secret in a Stringer's Format output), the
// correct mechanism is to wrap the io.Writer that zerolog ultimately writes
// JSON to. The wrapper scans the JSON byte stream for the literal secret
// bytes and replaces them with "***" before forwarding to the underlying
// writer. This is field-name agnostic by construction — it works regardless
// of how the developer wrote the field, because the redaction happens AFTER
// zerolog has serialised to JSON bytes but BEFORE the bytes reach stdout.
//
// Production wiring (cmd/sipgate-sip-stream-bridge/main.go):
//
//	logger := zerolog.New(observability.NewSecretMaskWriter(os.Stdout, cfg.AuthToken, cfg.SIPPassword)).
//	    With().Timestamp().Logger()
//
// Sub-loggers derived via parent.With().Str(...).Logger() inherit the wrapped
// writer through the zerolog.Context's writer reference, so every per-session
// or per-component sub-logger benefits from the mask without further wiring.
//
// # Field-name constants
//
// The four constants below are the grep-enforceable correlation field names.
// Every per-session sub-logger MUST use these via .Str(FieldCallSid, ...);
// the cardinality lint walker treats raw string literals like
// `"call_sid"` as a violation.
//
//   - FieldCallSid    — Twilio CallSid (CA[0-9a-f]{32}). Cross-system identifier.
//   - FieldAccountSid — Twilio AccountSid (AC[0-9a-f]{32}).
//   - FieldForwardLegID — Twilio DialCallSid for outbound <Dial> legs.
//   - FieldSIPCallID  — SIP Call-ID header. INTENTIONALLY DISTINCT from
//     FieldCallSid; many SIP-layer logs need the on-the-wire Call-ID to
//     correlate with packet captures.
package observability

import (
	"bytes"
	"io"
	"sort"
	"sync"
)

// Field names used by every per-session / per-component sub-logger. See
// package godoc for the Twilio-vs-SIP correlation distinction.
const (
	FieldCallSid      = "call_sid"
	FieldAccountSid   = "account_sid"
	FieldForwardLegID = "forward_leg_id"
	FieldSIPCallID    = "call_id"
)

// secretMaskWriter wraps an io.Writer and redacts every occurrence of a
// configured secret in the byte stream before forwarding to the underlying
// writer.
//
// Concurrency: the byte-replacement work runs on the local p slice (no shared
// state); only the underlying.Write call is serialised via mu. zerolog calls
// Write from a single goroutine per logger, but multiple sub-loggers share
// the same underlying writer — the mutex prevents interleaving across
// concurrent sub-loggers.
//
// Edge case (acceptable, documented): zerolog flushes at event boundaries, so
// each Write() call carries one full JSON line. A secret that is split across
// two Write calls would not be masked. zerolog does not do this in practice;
// if a future zerolog refactor changes that behaviour the contract here would
// need re-validation.
type secretMaskWriter struct {
	underlying io.Writer
	secrets    [][]byte // sorted descending by length; non-empty only
	mu         sync.Mutex
}

// NewSecretMaskWriter wraps the underlying io.Writer (typically os.Stdout)
// and redacts every occurrence of any non-empty secret in the byte stream
// passed through Write(). Empty secrets are filtered.
//
// Zero-cost passthrough: when no non-empty secrets are configured the helper
// returns the underlying writer unchanged (pointer-identity preserved). This
// is observable via reflect-free comparison and is the contract
// TestSecretLeak_NoSecretsConfigured_PassesThrough relies on.
//
// Sort order: secrets are sorted by descending length so the longest secret
// is matched first. Without this, masking "PASSWORD" inside "PASSWORD123"
// would yield "***123" — leaking the suffix.
//
// Replacement bytes: "***" — three ASCII bytes, bounded length, never
// produces a substring that overlaps another configured secret in the same
// write.
func NewSecretMaskWriter(underlying io.Writer, secrets ...string) io.Writer {
	nonempty := make([][]byte, 0, len(secrets))
	for _, s := range secrets {
		if s != "" {
			nonempty = append(nonempty, []byte(s))
		}
	}
	if len(nonempty) == 0 {
		return underlying
	}
	sort.SliceStable(nonempty, func(i, j int) bool { return len(nonempty[i]) > len(nonempty[j]) })
	return &secretMaskWriter{underlying: underlying, secrets: nonempty}
}

// maskedReplacement is allocated once at package level so every Write()
// reuses the same target slice argument to bytes.ReplaceAll.
var maskedReplacement = []byte("***")

// Write redacts every occurrence of every configured secret in p, then
// forwards the masked bytes to the underlying writer.
//
// io.Writer-contract semantics (propagate underlying short-write count +
// io.ErrShortWrite):
//
//   - Underlying short-write (n < len(masked), err == nil): surface
//     (n, io.ErrShortWrite) so the caller can detect partial loss. zerolog
//     buffer flushes are atomic per event in production stdout, but a
//     future redirect to a slow file backend that swallows bytes must not
//     be silently lossy.
//   - Underlying writer error (n, err != nil): propagate (n, err) — return
//     the actual byte count, NOT 0. Returning 0 on an error with partial
//     progress would lie about throughput on the caller's side.
//   - Full write (n == len(masked), err == nil): return (len(p), nil).
//     From the caller's perspective the wrapper "consumed" all of p
//     (masking is a length-shrinking transform), and zerolog expects
//     n == len(p) on success.
func (w *secretMaskWriter) Write(p []byte) (int, error) {
	masked := p
	for _, s := range w.secrets {
		masked = bytes.ReplaceAll(masked, s, maskedReplacement)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.underlying.Write(masked)
	if err != nil {
		// Propagate underlying write error + actual byte count. zerolog
		// logs the error at its writer-error-handler boundary; returning
		// a fabricated 0 would lie about throughput on the caller's side.
		return n, err
	}
	if n < len(masked) {
		// Underlying short-write without an explicit error: surface
		// io.ErrShortWrite so the io.Writer contract is honored. Caller
		// can then retry or escalate. zerolog buffer flushes are atomic
		// per event in production — this branch protects against a
		// future os.Stdout redirect to a slow file backend that swallows
		// bytes.
		return n, io.ErrShortWrite
	}
	// Full underlying write of `masked`. From the caller's perspective
	// we "consumed" all of p (masking is a length-shrinking transform).
	// Returning len(p) preserves the io.Writer contract on the caller's
	// side; the err==nil + n==len(masked) branch is the only path that
	// reaches here.
	return len(p), nil
}
