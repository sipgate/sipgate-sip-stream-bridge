package observability

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds all Prometheus metrics for audio-dock.
// Passed to CallManager and Registrar at construction; never registered globally.
// Metric names match REQUIREMENTS.md OBS-03 exactly.
// NOTE: active_calls_total is a Gauge (goes up/down with sessions) — the _total suffix
// matches the OBS-03 requirement literally; deviates from Prometheus Counter convention
// but is not enforced by the client library.
type Metrics struct {
	ActiveCalls  prometheus.Gauge   // active_calls_total
	SIPRegStatus prometheus.Gauge   // sip_registration_status (0/1)
	RTPRx        prometheus.Counter // rtp_packets_received_total
	RTPTx        prometheus.Counter // rtp_packets_sent_total
	WSReconnects prometheus.Counter // ws_reconnect_attempts_total
	Registry     *prometheus.Registry
}

// NewMetrics creates a custom registry and registers all five required metrics.
// Uses prometheus.NewRegistry() (NOT prometheus.DefaultRegisterer) to exclude Go runtime metrics
// from the /metrics scrape output — only audio-dock metrics are exposed (Research anti-pattern note).
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()

	activeCalls := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "active_calls_total",
		Help: "Number of currently active SIP call sessions.",
	})
	sipReg := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "sip_registration_status",
		Help: "SIP registration status: 1 = registered, 0 = unregistered or failed.",
	})
	rtpRx := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rtp_packets_received_total",
		Help: "Total RTP packets received from the SIP caller (PCMU PT=0 only).",
	})
	rtpTx := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rtp_packets_sent_total",
		Help: "Total RTP packets sent to the SIP caller.",
	})
	wsReconnects := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ws_reconnect_attempts_total",
		Help: "Total WebSocket reconnect attempts across all calls.",
	})

	// Use reg.MustRegister (NOT prometheus.MustRegister) — custom registry only (Research Pitfall 4)
	reg.MustRegister(activeCalls, sipReg, rtpRx, rtpTx, wsReconnects)

	return &Metrics{
		ActiveCalls:  activeCalls,
		SIPRegStatus: sipReg,
		RTPRx:        rtpRx,
		RTPTx:        rtpTx,
		WSReconnects: wsReconnects,
		Registry:     reg,
	}
}
