package twiml

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestHangupHandler_CallsTerminateWithReasonHangup - direct unit test on the
// handler (bypasses Dispatch) to lock in the exact reason string passed to
// Terminate. The status-callback emission path reads this reason when
// computing the CallStatus mapping, so the constant matters.
func TestHangupHandler_CallsTerminateWithReasonHangup(t *testing.T) {
	target := newStubTarget()
	if err := hangupHandler(context.Background(), target); err != nil {
		t.Fatalf("hangupHandler returned err = %v, want nil", err)
	}
	if got, want := target.terminateCalls, []string{"hangup"}; !equalStrings(got, want) {
		t.Fatalf("terminateCalls = %v, want %v", got, want)
	}
}

// TestHangupHandler_PropagatesTerminateError - if Terminate fails (e.g. BYE
// could not be sent over an already-shut-down dialog), hangupHandler must
// return that error unchanged so Dispatch can propagate it to the REST
// handler.
func TestHangupHandler_PropagatesTerminateError(t *testing.T) {
	wantErr := errors.New("BYE: dialog terminated")
	target := newStubTarget()
	target.terminateErr = wantErr

	err := hangupHandler(context.Background(), target)
	if err == nil {
		t.Fatal("expected non-nil error when Terminate fails, got nil")
	}
	if !errors.Is(err, wantErr) && !strings.Contains(err.Error(), "BYE: dialog terminated") {
		t.Fatalf("hangupHandler error = %v, want to wrap or equal %q", err, wantErr)
	}
	// Even on failure, Terminate must have been invoked exactly once with the
	// canonical reason.
	if got, want := target.terminateCalls, []string{"hangup"}; !equalStrings(got, want) {
		t.Fatalf("terminateCalls = %v, want %v", got, want)
	}
}
