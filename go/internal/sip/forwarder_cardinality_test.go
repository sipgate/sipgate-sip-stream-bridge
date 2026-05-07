package sip

import (
	"errors"
	"testing"
	"time"
)

// TestForwarder_Cardinality_OutcomeAlwaysBucketed is the cardinality
// regression for the recordFailure outcome label. Every raw result.Status
// value that production callsites in forwarder.go can set MUST be bucketed
// by recordFailure into the bounded enum
// {answered, no_answer, busy, error} — never reach
// forward_duration_seconds{outcome} as raw bytes.
//
// Pre-fix evidence: Registry.Gather emitted 7 outcome series including the
// non-allowlist values [canceled, failed, no-answer]. With unconditional
// bucketOutcome inside recordFailure, this test guarantees that scenario
// can no longer occur regardless of which caller fired.
//
// DELIBERATE-BREAK contract: temporarily reverting the unconditional
// bucketing edit (re-introducing the `if outcome == ""` conditional guard)
// MUST make this test fail with the offending raw labels surfaced in the
// error message. This is the regression-detection mechanism.
func TestForwarder_Cardinality_OutcomeAlwaysBucketed(t *testing.T) {
	rawStatuses := []string{
		// The 4 raw values that production sites historically passed
		// directly to the histogram instead of bucketing.
		"no-answer", "canceled", "failed", "busy",
		// Empty-string input — pre-emit failure callers that historically
		// hit the fallback branch. bucketOutcome("") must default to
		// "error".
		"",
		// Future-proofing: any non-{"answered","no-answer","busy"} input
		// must default to "error" via bucketOutcome's default branch.
		// "completed" is the existing dialTerminalEventName mapping
		// target — make sure it does NOT leak as an outcome label.
		"completed",
		// Hypothetical future producers — every one of these must bucket
		// into "error", not into the histogram as raw bytes.
		"rejected",
		"unreachable",
		"timeout",
	}

	// newTestForwarder is the shared helper at forwarder_test.go:200; returns
	// (*Forwarder, *observability.Metrics, *Guardrails). The Forwarder is
	// constructed without a real Agent — the recordFailure path does not
	// touch the factory or the agent so a stub is sufficient.
	f, m, _ := newTestForwarder(&stubDialFactory{}, []string{"+49"})

	for _, rawStatus := range rawStatuses {
		result := &DialResult{
			DialCallSid:  "CAregress" + rawStatus,
			DialedTarget: "+490000000",
			Status:       rawStatus,
			Reason:       "", // force recordFailure's reason fallback
		}
		// Direct invocation of the unexported chokepoint. opts/callerSid
		// zero-value: emitDialEvent is nil-safe and short-circuits on
		// opts.StatusCallback=="" — this exercises ONLY the metric-emit
		// path the regression covers. The `outcome` arg deliberately
		// passes the raw status (mirrors the 9-of-11 production callers).
		f.recordFailure(errors.New("regression: "+rawStatus), time.Now(), DialOpts{}, "", result, rawStatus)
	}

	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("Registry.Gather: %v", err)
	}
	allowlist := map[string]bool{
		"answered":  true,
		"no_answer": true,
		"busy":      true,
		"error":     true,
	}
	var violations []string
	var seen []string
	for _, fam := range families {
		if fam.GetName() != "sipgate_bridge_forward_duration_seconds" {
			continue
		}
		for _, metric := range fam.GetMetric() {
			for _, lbl := range metric.GetLabel() {
				if lbl.GetName() != "outcome" {
					continue
				}
				seen = append(seen, lbl.GetValue())
				if !allowlist[lbl.GetValue()] {
					violations = append(violations, lbl.GetValue())
				}
			}
		}
	}
	if len(violations) > 0 {
		t.Fatalf("forward_duration_seconds emitted non-allowlist outcome values %v; "+
			"allowlist={answered,no_answer,busy,error}; full set seen=%v", violations, seen)
	}
	if len(seen) == 0 {
		t.Fatalf("forward_duration_seconds emitted zero outcome series — recordFailure did not run; check fixture")
	}
}
