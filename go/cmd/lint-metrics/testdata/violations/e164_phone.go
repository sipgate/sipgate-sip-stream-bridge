package violations

import (
	"github.com/prometheus/client_golang/prometheus"
)

// metrics:allowlist target=allowed1|allowed2
var dialAttemptsByTarget = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "test_violation_e164_phone",
	Help: "Test fixture — E.164 phone number passed as a label value.",
}, []string{"target"})

func leakE164Phone() {
	// VIOLATION: "+4915123456789" is an E.164 phone number — high cardinality
	// (one series per phone number).
	dialAttemptsByTarget.WithLabelValues("+4915123456789").Inc()
}
