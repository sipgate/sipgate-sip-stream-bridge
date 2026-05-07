package clean

import (
	"github.com/prometheus/client_golang/prometheus"
)

// metrics:allowlist outcome=answered|no_answer|busy|error
var bucketerVec = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "test_clean_bucketer_vec",
	Help: "Test fixture — *Vec called with a // metrics:bucketer-annotated function.",
}, []string{"outcome"})

// metrics:bucketer
//
// fixtureBucketOutcome maps an arbitrary input string onto one of the bounded
// outcome labels. The annotation tells the walker that this function's
// return value is bounded by contract — the walker accepts call sites that
// pass the result of fixtureBucketOutcome to *Vec.WithLabelValues.
func fixtureBucketOutcome(s string) string {
	switch s {
	case "answered":
		return "answered"
	case "no_answer":
		return "no_answer"
	case "busy":
		return "busy"
	default:
		return "error"
	}
}

func recordBucketerCall(s string) {
	bucketerVec.WithLabelValues(fixtureBucketOutcome(s)).Inc()
}
