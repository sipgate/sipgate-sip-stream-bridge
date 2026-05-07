package webhook

import "testing"

// TestSubscriptionMatches_TableDriven covers the four resolution rules
// from SubscriptionMatches' docstring with ≥10 sub-cases. Each case has
// an explicit "rule" comment referencing the rule in the function
// docstring, so a future change that breaks one case can be diagnosed
// at the rule level. Defends the canonical helper against divergent
// interpretations of empty Events lists.
func TestSubscriptionMatches_TableDriven(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		events map[string]struct{}
		event  string
		want   bool
		rule   string
	}{
		// Rule 1: nil/empty Events ⇒ subscribe-to-all
		{"nil_events_initiated", nil, "initiated", true, "rule 1 (nil → subscribe-to-all)"},
		{"empty_events_initiated", map[string]struct{}{}, "initiated", true, "rule 1 (empty → subscribe-to-all)"},
		{"empty_events_completed", map[string]struct{}{}, "completed", true, "rule 1 (empty → subscribe-to-all)"},
		{"empty_events_ringing", map[string]struct{}{}, "ringing", true, "rule 1 (empty → subscribe-to-all)"},
		{"empty_events_busy", map[string]struct{}{}, "busy", true, "rule 1 (empty → subscribe-to-all)"},
		// Rule 2: specific event in Events
		{"initiated_only_initiated", map[string]struct{}{"initiated": {}}, "initiated", true, "rule 2 (specific match)"},
		{"completed_only_completed", map[string]struct{}{"completed": {}}, "completed", true, "rule 2 (specific match)"},
		{"ringing_only_ringing", map[string]struct{}{"ringing": {}}, "ringing", true, "rule 2 (specific match)"},
		{"busy_in_set_busy", map[string]struct{}{"busy": {}}, "busy", true, "rule 2 (specific match)"},
		{"answered_in_set_answered", map[string]struct{}{"answered": {}, "completed": {}}, "answered", true, "rule 2 (specific match)"},
		// Rule 3: terminal-class fallback to "completed"
		{"completed_only_busy_falls_back", map[string]struct{}{"completed": {}}, "busy", true, "rule 3 (terminal fallback)"},
		{"completed_only_failed_falls_back", map[string]struct{}{"completed": {}}, "failed", true, "rule 3 (terminal fallback)"},
		{"completed_only_no_answer_falls_back", map[string]struct{}{"completed": {}}, "no-answer", true, "rule 3 (terminal fallback)"},
		{"completed_only_canceled_falls_back", map[string]struct{}{"completed": {}}, "canceled", true, "rule 3 (terminal fallback)"},
		{"initiated_and_completed_busy_falls_back", map[string]struct{}{"initiated": {}, "completed": {}}, "busy", true, "rule 3 (terminal fallback)"},
		// Rule 4: not subscribed at all
		{"initiated_only_ringing_drops", map[string]struct{}{"initiated": {}}, "ringing", false, "rule 4 (no match)"},
		{"completed_only_initiated_drops", map[string]struct{}{"completed": {}}, "initiated", false, "rule 4 (lifecycle does NOT fall back to completed)"},
		{"completed_only_ringing_drops", map[string]struct{}{"completed": {}}, "ringing", false, "rule 4 (lifecycle does NOT fall back to completed)"},
		{"completed_only_answered_drops", map[string]struct{}{"completed": {}}, "answered", false, "rule 4 (lifecycle does NOT fall back to completed)"},
		{"completed_only_in_progress_drops", map[string]struct{}{"completed": {}}, "in-progress", false, "rule 4 (in-progress is NOT terminal here)"},
		{"ringing_only_completed_drops", map[string]struct{}{"ringing": {}}, "completed", false, "rule 4 (no completed in set, no specific match)"},
		{"ringing_only_busy_drops", map[string]struct{}{"ringing": {}}, "busy", false, "rule 4 (no completed in set, terminal still drops)"},
		{"initiated_only_busy_drops", map[string]struct{}{"initiated": {}}, "busy", false, "rule 4 (no completed in set, terminal still drops)"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := SubscriptionMatches(tc.events, tc.event); got != tc.want {
				t.Errorf("SubscriptionMatches(%v, %q) = %v, want %v (%s)",
					tc.events, tc.event, got, tc.want, tc.rule)
			}
		})
	}
}

// TestResolveEventName_TableDriven asserts the REMAP behavior for the
// generic-completed fallback. The terminal-class fallback returns
// ("completed", true) — a remap from the input eventName to the
// canonical "completed" label — so callers (notably
// emitTerminalStatusCallback in bridge/session.go) can use the returned
// label as the metric/event tag while keeping Form.CallStatus = the
// actual call status (e.g. "busy" / "failed" / "canceled").
//
// At least one test case proves: ResolveEventName({completed}, "busy")
// → ("completed", true). This is the structural acceptance criterion
// from 16-09-PLAN Task 1.
func TestResolveEventName_TableDriven(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		events    map[string]struct{}
		event     string
		wantLabel string
		wantOk    bool
		rule      string
	}{
		// Rule 1: nil/empty ⇒ pass through (eventName, true)
		{"nil_events_initiated_passthrough", nil, "initiated", "initiated", true, "rule 1 (nil → passthrough)"},
		{"empty_events_completed_passthrough", map[string]struct{}{}, "completed", "completed", true, "rule 1 (empty → passthrough)"},
		{"empty_events_busy_passthrough", map[string]struct{}{}, "busy", "busy", true, "rule 1 (empty → passthrough)"},
		// Rule 2: specific match ⇒ (eventName, true)
		{"initiated_in_set_initiated_keeps", map[string]struct{}{"initiated": {}}, "initiated", "initiated", true, "rule 2 (specific match)"},
		{"busy_in_set_busy_keeps", map[string]struct{}{"busy": {}}, "busy", "busy", true, "rule 2 (specific match)"},
		{"completed_in_set_completed_keeps", map[string]struct{}{"completed": {}}, "completed", "completed", true, "rule 2 (specific match — no remap)"},
		// Rule 3: REMAP — terminal eventName + only "completed" in set
		// ⇒ ("completed", true). This is the load-bearing case.
		{"completed_only_busy_REMAPS_to_completed", map[string]struct{}{"completed": {}}, "busy", "completed", true, "rule 3 (REMAP — terminal fallback)"},
		{"completed_only_failed_REMAPS_to_completed", map[string]struct{}{"completed": {}}, "failed", "completed", true, "rule 3 (REMAP — terminal fallback)"},
		{"completed_only_no_answer_REMAPS_to_completed", map[string]struct{}{"completed": {}}, "no-answer", "completed", true, "rule 3 (REMAP — terminal fallback)"},
		{"completed_only_canceled_REMAPS_to_completed", map[string]struct{}{"completed": {}}, "canceled", "completed", true, "rule 3 (REMAP — terminal fallback)"},
		// Rule 4: not subscribed ⇒ ("", false)
		{"initiated_only_ringing_drops", map[string]struct{}{"initiated": {}}, "ringing", "", false, "rule 4 (no match)"},
		{"completed_only_initiated_drops", map[string]struct{}{"completed": {}}, "initiated", "", false, "rule 4 (lifecycle does NOT fall back)"},
		{"completed_only_ringing_drops", map[string]struct{}{"completed": {}}, "ringing", "", false, "rule 4 (lifecycle does NOT fall back)"},
		{"ringing_only_busy_drops", map[string]struct{}{"ringing": {}}, "busy", "", false, "rule 4 (no completed in set)"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotLabel, gotOk := ResolveEventName(tc.events, tc.event)
			if gotLabel != tc.wantLabel || gotOk != tc.wantOk {
				t.Errorf("ResolveEventName(%v, %q) = (%q, %v), want (%q, %v) (%s)",
					tc.events, tc.event, gotLabel, gotOk, tc.wantLabel, tc.wantOk, tc.rule)
			}
		})
	}
}

// TestIsTerminalEvent_TableDriven covers all five terminal events plus
// representative lifecycle / non-terminal events to guard against
// accidental classification drift (e.g. moving "in-progress" into the
// terminal set, which would change SubscriptionMatches' fallback
// behavior in a customer-visible way).
func TestIsTerminalEvent_TableDriven(t *testing.T) {
	t.Parallel()
	tests := []struct {
		event string
		want  bool
	}{
		// 5 terminal events (the documented enum)
		{"completed", true},
		{"busy", true},
		{"failed", true},
		{"no-answer", true},
		{"canceled", true},
		// 4 lifecycle / non-terminal events
		{"initiated", false},
		{"ringing", false},
		{"answered", false},
		{"in-progress", false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.event, func(t *testing.T) {
			t.Parallel()
			if got := IsTerminalEvent(tc.event); got != tc.want {
				t.Errorf("IsTerminalEvent(%q) = %v, want %v", tc.event, got, tc.want)
			}
		})
	}
}
