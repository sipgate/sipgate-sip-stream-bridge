package violations

import (
	"github.com/prometheus/client_golang/prometheus"
)

// metrics:allowlist callback=allowed1|allowed2
var callbackUrlVec = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "test_violation_url_label",
	Help: "Test fixture — full URL passed as a label value.",
}, []string{"callback"})

func leakURL() {
	// VIOLATION: "https://attacker.example/cb" is a full URL — high cardinality
	// (one series per customer-supplied callback host) AND leaks PII via labels.
	callbackUrlVec.WithLabelValues("https://attacker.example/cb").Inc()
}
