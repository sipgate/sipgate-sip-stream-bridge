package clean

import (
	"github.com/prometheus/client_golang/prometheus"
)

// EventInitiated is a package-level enum constant — bounded by definition.
// The walker must resolve the *ast.Ident "EventInitiated" to this constant
// and accept the WithLabelValues argument as a known string value.
const (
	EventInitiated = "initiated"
	EventRinging   = "ringing"
	EventAnswered  = "answered"
)

// metrics:allowlist event=initiated|ringing|answered
var enumConstVec = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "test_clean_enum_const_vec",
	Help: "Test fixture — enum-const ident as WithLabelValues arg.",
}, []string{"event"})

func recordEnumConst() {
	enumConstVec.WithLabelValues(EventInitiated).Inc()
	enumConstVec.WithLabelValues(EventRinging).Inc()
	enumConstVec.WithLabelValues(EventAnswered).Inc()
}
