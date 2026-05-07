// Package clean is a Phase 17-04 lint-metrics walker fixture.
//
// All call sites in this package are LEGITIMATE: literal arguments are members
// of the *Vec's allowlist, no leak patterns appear, and no dynamic-allowed
// opt-out is needed. The walker MUST produce zero diagnostics for this package.
package clean

import (
	"github.com/prometheus/client_golang/prometheus"
)

// metrics:allowlist event=initiated|ringing|answered
var statusCallbackAttempts = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "test_clean_status_callback_attempts_total",
	Help: "Test fixture — clean status-callback-attempts counter.",
}, []string{"event"})

// metrics:allowlist route=list_calls|get_call|modify_call
var apiRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "test_clean_api_requests_total",
	Help: "Test fixture — clean API-requests counter.",
}, []string{"route"})

func recordClean() {
	statusCallbackAttempts.WithLabelValues("initiated").Inc()
	statusCallbackAttempts.WithLabelValues("ringing").Inc()
	statusCallbackAttempts.WithLabelValues("answered").Inc()
	apiRequests.WithLabelValues("list_calls").Inc()
	apiRequests.WithLabelValues("get_call").Inc()
	apiRequests.WithLabelValues("modify_call").Inc()
}
