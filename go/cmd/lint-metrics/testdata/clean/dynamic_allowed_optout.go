package clean

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog/log"
)

// metrics:allowlist tenant=tenant_a|tenant_b
var dynamicAllowedVec = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "test_clean_dynamic_allowed_vec",
	Help: "Test fixture — // metrics:dynamic-allowed opt-out covers a non-literal arg.",
}, []string{"tenant"})

func recordDynamicAllowed(tenantID string) {
	// metrics:dynamic-allowed
	dynamicAllowedVec.WithLabelValues(tenantID).Inc()
}

// recordSecurityEvent emits a Warn-level log with a URL field. The
// // metrics:url-allowed opt-out marks this as an intentional security
// event — the walker's log-field visitor must NOT flag it.
func recordSecurityEvent(host string) {
	// metrics:url-allowed callback_host: SSRF-rejected URL is a security event
	log.Warn().
		Str("callback_host", host).
		Msg("status_callback: SSRF rejected")
}
