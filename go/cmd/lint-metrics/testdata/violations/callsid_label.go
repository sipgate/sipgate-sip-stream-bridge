package violations

import (
	"github.com/prometheus/client_golang/prometheus"
)

// metrics:allowlist callsid=allowed1|allowed2
var callsidVec = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "test_violation_callsid_label",
	Help: "Test fixture — Twilio CallSid (CA*) passed as a label value.",
}, []string{"callsid"})

func leakCallSid() {
	// VIOLATION: "CA0123456789abcdef0123456789abcdef" is a CallSid — opaque
	// 34-char identifier; one series per call.
	callsidVec.WithLabelValues("CA0123456789abcdef0123456789abcdef").Inc()
}
