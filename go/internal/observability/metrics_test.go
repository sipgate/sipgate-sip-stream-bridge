package observability_test

import (
	"testing"

	"github.com/sipgate/audio-dock/internal/observability"
)

func TestNewMetrics_SIPOptionsFailures_NotNil(t *testing.T) {
	m := observability.NewMetrics()
	if m.SIPOptionsFailures == nil {
		t.Fatal("expected SIPOptionsFailures to be non-nil, got nil")
	}
}

func TestNewMetrics_SIPOptionsFailures_IncDoesNotPanic(t *testing.T) {
	m := observability.NewMetrics()
	// Inc() must not panic — counter is properly registered on the custom registry
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("SIPOptionsFailures.Inc() panicked: %v", r)
		}
	}()
	m.SIPOptionsFailures.Inc()
}
