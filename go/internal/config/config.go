package config

import (
	"fmt"
	"net"
	"os"
	"time"

	"github.com/joho/godotenv"
	"go-simpler.org/env"
)

// Config holds all environment-variable configuration for audio-dock.
// Field names match v1.0 env var names exactly for drop-in compatibility (CFG-01 through CFG-04).
type Config struct {
	// SIP credentials (CFG-01)
	SIPUser      string `env:"SIP_USER,required"      usage:"SIP username / SIP-ID (e.g. e12345p0)"`
	SIPPassword  string `env:"SIP_PASSWORD,required"  usage:"SIP account password"`
	SIPDomain    string `env:"SIP_DOMAIN,required"    usage:"SIP registrar domain (e.g. sipconnect.sipgate.de)"`
	SIPRegistrar string `env:"SIP_REGISTRAR,required" usage:"SIP registrar address (e.g. sipconnect.sipgate.de)"`

	// WebSocket target (CFG-02) — env var name WS_TARGET_URL matches v1.0 exactly
	WSTargetURL string `env:"WS_TARGET_URL,required" usage:"Target WebSocket URL (e.g. wss://my-bot.example.com/ws)"`

	// SDP contact IP (CFG-04) — optional: defaults to outbound local IP if not set
	SDPContactIP string `env:"SDP_CONTACT_IP" usage:"Externally-reachable IP address for SDP contact line (default: auto-detected local IP)"`

	// RTP port range (CFG-03)
	RTPPortMin int `env:"RTP_PORT_MIN" default:"10000" usage:"Minimum UDP port for RTP (inclusive)"`
	RTPPortMax int `env:"RTP_PORT_MAX" default:"10099" usage:"Maximum UDP port for RTP (inclusive)"`

	// SIP registration expiry (optional — used in Phase 5)
	SIPExpires int `env:"SIP_EXPIRES" default:"120" usage:"SIP registration expiry in seconds"`

	// SIP OPTIONS keepalive interval (Phase 10)
	SIPOptionsInterval time.Duration `env:"SIP_OPTIONS_INTERVAL" default:"30s" usage:"Interval between SIP OPTIONS keepalive pings (e.g. 30s, 1m)"`

	// Log level (optional)
	LogLevel string `env:"LOG_LEVEL" default:"info" usage:"Log level: trace, debug, info, warn, error"`

	// HTTP server port for /health and /metrics (OBS-02, OBS-03)
	HTTPPort string `env:"HTTP_PORT" default:"9090" usage:"HTTP server port for /health and /metrics endpoints"`
}

// Load reads environment variables into Config and fails fast on misconfiguration (CFG-05).
// Errors are printed as minimal JSON to stderr so they are parseable before zerolog is initialized.
// Never returns on error — always exits non-zero.
func Load() Config {
	// Load .env file if present — silently ignored in production where vars are set directly.
	// Does not override variables already set in the process environment.
	_ = godotenv.Load("../.env", ".env")

	var cfg Config
	if err := env.Load(&cfg, nil); err != nil {
		// env error format: "env: VAR_NAME is required but not set"
		fmt.Fprintf(os.Stderr,
			`{"level":"fatal","msg":"configuration error","error":%q}`+"\n",
			err.Error())
		os.Exit(1)
	}

	// Auto-detect outbound local IP if SDP_CONTACT_IP not set (CFG-04)
	if cfg.SDPContactIP == "" {
		if conn, err := net.Dial("udp", "8.8.8.8:80"); err == nil {
			cfg.SDPContactIP = conn.LocalAddr().(*net.UDPAddr).IP.String()
			conn.Close()
		}
	}

	// Post-load cross-field validation — go-simpler/env does not support cross-field checks (CFG-05)
	if cfg.RTPPortMin >= cfg.RTPPortMax {
		fmt.Fprintf(os.Stderr,
			`{"level":"fatal","msg":"RTP_PORT_MIN must be less than RTP_PORT_MAX","RTP_PORT_MIN":%d,"RTP_PORT_MAX":%d}`+"\n",
			cfg.RTPPortMin, cfg.RTPPortMax)
		os.Exit(1)
	}

	return cfg
}
