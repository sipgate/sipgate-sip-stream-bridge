// Package violations contains synthetic fixtures that the walker MUST flag.
// Each fixture file isolates a single violation pattern.
package violations

import (
	"github.com/prometheus/client_golang/prometheus"
)

// metrics:allowlist route=list_calls|get_call|modify_call method=GET|POST status=2xx|4xx|5xx
var apiRequestsRawStatus = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "test_violation_raw_status_code",
	Help: "Test fixture — raw 3-digit status code passed as a label value.",
}, []string{"route", "method", "status"})

func leakRawStatusCode() {
	// VIOLATION: "200" is a raw HTTP status code — should be bucketed via
	// observability.BucketStatus into "2xx"/"4xx"/"5xx".
	apiRequestsRawStatus.WithLabelValues("list_calls", "GET", "200").Inc()
}
