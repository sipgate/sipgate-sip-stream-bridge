package violations

import (
	"github.com/prometheus/client_golang/prometheus"
)

// metrics:allowlist account=allowed1|allowed2
var accountSidVec = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "test_violation_accountsid_label",
	Help: "Test fixture — Twilio AccountSid (AC*) passed as a label value.",
}, []string{"account"})

func leakAccountSid() {
	// VIOLATION: "ACdeadbeefdeadbeefdeadbeefdeadbeef" is an AccountSid — opaque
	// 34-char identifier; one series per account (multi-tenant cardinality).
	accountSidVec.WithLabelValues("ACdeadbeefdeadbeefdeadbeefdeadbeef").Inc()
}
