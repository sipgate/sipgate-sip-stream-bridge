package clean

import (
	"github.com/prometheus/client_golang/prometheus"
)

// metrics:allowlist reason=timeout|connect_error|exhausted_retries
var localVarBucketerVec = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "test_clean_local_var_bucketer_vec",
	Help: "Test fixture — local var assigned from bucketer call, then passed to WithLabelValues.",
}, []string{"reason"})

// metrics:bucketer
//
// fixtureBucketReason maps an error sentinel string to one of the bounded
// reason labels.
func fixtureBucketReason(s string) string {
	switch s {
	case "deadline":
		return "timeout"
	case "refused":
		return "connect_error"
	default:
		return "exhausted_retries"
	}
}

func recordLocalVarBucketer(s string) {
	// The walker must trace `outcome` back to the bucketer call site and
	// accept the WithLabelValues argument as bounded.
	outcome := fixtureBucketReason(s)
	localVarBucketerVec.WithLabelValues(outcome).Inc()
}
