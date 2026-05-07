package violations

import (
	"github.com/prometheus/client_golang/prometheus"
)

// metrics:allowlist target=callee
var localVarFromParamVec = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "test_violation_local_var_from_param",
	Help: "Test fixture — local var assigned from a function parameter; provenance unbounded.",
}, []string{"target"})

// recordLeakFromParam takes an externally-supplied target string and emits
// it directly as a metric label. Without a // metrics:dynamic-allowed
// opt-out the walker MUST flag this — the value is unbounded by contract.
func recordLeakFromParam(target string) {
	// VIOLATION: function-param source — no opt-out. The walker should
	// emit a "function-param source" diagnostic.
	localVarFromParamVec.WithLabelValues(target).Inc()
}
