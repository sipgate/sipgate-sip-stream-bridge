package violations

import (
	"github.com/prometheus/client_golang/prometheus"
)

// metrics:allowlist event=initiated|ringing
var literalOutsideAllowlistVec = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "test_violation_literal_outside_allowlist",
	Help: "Test fixture — literal value not in the declared allowlist.",
}, []string{"event"})

func leakLiteralOutsideAllowlist() {
	// VIOLATION: "answered" is a literal not in the allowlist (which only
	// permits initiated|ringing). The walker MUST emit a "not in allowlist"
	// diagnostic.
	literalOutsideAllowlistVec.WithLabelValues("answered").Inc()
}
