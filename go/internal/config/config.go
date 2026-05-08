package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"go-simpler.org/env"
)

// Config holds all environment-variable configuration for sipgate-sip-stream-bridge.
// Field names match v1.0 env var names exactly for drop-in compatibility (CFG-01 through CFG-04).
type Config struct {
	// SIP credentials (CFG-01)
	SIPUser      string `env:"SIP_USER,required"      usage:"SIP username / SIP-ID (e.g. e12345p0)"`
	SIPPassword  string `env:"SIP_PASSWORD,required"  usage:"SIP account password"`
	SIPDomain    string `env:"SIP_DOMAIN,required"    usage:"SIP registrar domain (e.g. sipconnect.sipgate.de)"`
	SIPRegistrar string `env:"SIP_REGISTRAR,required" usage:"SIP registrar address (e.g. sipconnect.sipgate.de)"`

	// SIP listen address. Both UDP and TCP listeners bind here. The port portion
	// also drives the Contact: header (where sipgate delivers inbound INVITEs
	// and where outbound dialog UAs reach us). Default 0.0.0.0:5060 preserves
	// long-standing behaviour. Override for: e2e harness co-existing with a stub
	// registrar on the same host, IPv6-only deploys, or non-privileged-port
	// environments.
	SIPListenAddr string `env:"SIP_LISTEN_ADDR" default:"0.0.0.0:5060" usage:"SIP listener bind address (host:port). Drives both the UDP/TCP listeners and the Contact: header port"`

	// Outbound INVITE target port — when set (>0), the outbound INVITE
	// Request-URI carries this explicit port and sipgo routes the dialog
	// directly to host:port instead of resolving via DNS / using the default
	// SIP port. Default 0 leaves sipgo's standard routing in place
	// (DNS / 5060). Override when the trunk listens on a non-standard port,
	// when running behind a local SBC, or in e2e harness setups where
	// outbound INVITEs must reach a UAS stub on a specific port.
	SIPOutboundTargetPort int `env:"SIP_OUTBOUND_TARGET_PORT" default:"0" usage:"Explicit port for outbound INVITE Request-URI (0 = sipgo default routing)"`

	// Operator-supplied default StatusCallback subscription installed on
	// every inbound call. URL = empty disables the feature. URL must
	// validate as http:// or https:// at startup. Operator-supplied URLs
	// bypass the SSRF guard at dial time (CallbackEvent.Trusted=true);
	// REST-supplied StatusCallback= via POST /Calls/{Sid}.json is still
	// SSRF-guarded — operators control deployment, customers do not.
	StatusCallbackDefaultURL    string `env:"STATUS_CALLBACK_DEFAULT_URL" usage:"Operator-supplied default StatusCallback URL installed on every inbound call (http:// or https://; empty = disabled)"`
	StatusCallbackDefaultMethod string `env:"STATUS_CALLBACK_DEFAULT_METHOD" default:"POST" usage:"HTTP method for the default StatusCallback (POST or GET)"`
	StatusCallbackDefaultEvents string `env:"STATUS_CALLBACK_DEFAULT_EVENTS" default:"initiated,ringing,answered,completed" usage:"CSV of subscribed events (subset of initiated|ringing|answered|in-progress|completed|busy|failed|no-answer|canceled)"`

	// WebSocket target (CFG-02) — env var name WS_TARGET_URL matches v1.0 exactly
	WSTargetURL string `env:"WS_TARGET_URL,required" usage:"Target WebSocket URL (e.g. wss://my-bot.example.com/ws)"`

	// SDP contact IP (CFG-04) — optional: defaults to outbound local IP if not set
	SDPContactIP string `env:"SDP_CONTACT_IP" usage:"Externally-reachable IP address for SDP contact line (default: auto-detected local IP)"`

	// RTP port range (CFG-03)
	RTPPortMin int `env:"RTP_PORT_MIN" default:"10000" usage:"Minimum UDP port for RTP (inclusive)"`
	RTPPortMax int `env:"RTP_PORT_MAX" default:"10099" usage:"Maximum UDP port for RTP (inclusive)"`

	// SIP registration expiry (optional)
	SIPExpires int `env:"SIP_EXPIRES" default:"120" usage:"SIP registration expiry in seconds"`

	// SIP OPTIONS keepalive interval
	SIPOptionsInterval time.Duration `env:"SIP_OPTIONS_INTERVAL" default:"30s" usage:"Interval between SIP OPTIONS keepalive pings (e.g. 30s, 1m)"`

	// Audio codec mode (AUDIO_MODE)
	AudioMode string `env:"AUDIO_MODE" default:"twilio" usage:"Audio codec mode: twilio (PCMU only) or best (G.722 preferred)"`

	// SRTP media encryption (SRTP_ENABLED)
	SRTPEnabled bool `env:"SRTP_ENABLED" default:"false" usage:"Enable SRTP media encryption (RTP/SAVP with SDES key exchange)"`

	// Log level (optional)
	LogLevel string `env:"LOG_LEVEL" default:"info" usage:"Log level: trace, debug, info, warn, error"`

	// HTTP server port for /health and /metrics
	HTTPPort string `env:"HTTP_PORT" default:"9090" usage:"HTTP server port for /health and /metrics endpoints"`

	// ── v3.0 control plane ──

	// Twilio AuthToken — used as Basic Auth password (REST) and HMAC-SHA1 signing key.
	AuthToken string `env:"AUTH_TOKEN" usage:"Twilio-compatible auth token (REST Basic Auth password + HMAC-SHA1 signing key)"`

	// Public base URL. Used for HMAC URL reconstruction behind reverse proxies.
	PublicBaseURL string `env:"PUBLIC_BASE_URL" usage:"External base URL when running behind a reverse proxy (e.g. https://bridge.example.com)"`

	// ── v3.0 Dial / B2BUA forwarding ──

	// DialAllowedPrefixes — toll-fraud allow-list (default-deny). CSV of E.164
	// prefixes; empty = block ALL outbound dials. Operator MUST opt in.
	DialAllowedPrefixes []string `env:"DIAL_ALLOWED_PREFIXES" usage:"CSV of allowed E.164 prefixes for <Dial> targets (default empty = deny-all)"`

	// DialDefaultCallerID — explicit operator-configured caller-ID for <Dial>
	// when the TwiML callerId attribute is empty. Higher priority than the
	// implicit auto-fallbacks (inbound-To URI, then preserve-ANI). Set this
	// to a verified DID on your trunk if you want a fixed outbound caller-ID
	// regardless of which DID the inbound call hit.
	DialDefaultCallerID string `env:"DIAL_DEFAULT_CALLER_ID" usage:"Explicit fallback caller-ID for <Dial> (overrides inbound-To auto-fallback)"`

	// DialCallerIDCountryCode — E.164 country code (without +) used to
	// normalise display caller-IDs into the format sipgate's trunking
	// documentation requires for both the From display-name and the
	// P-Preferred-Identity user-part: international format without leading
	// "+" or "00", and without leading "0" on the area code. Examples:
	//   "49" → "021193674951" becomes "4921193674951";
	//          "+4921193674951" becomes "4921193674951";
	//          "004921193674951" becomes "4921193674951".
	// Empty → only strip leading "+" / "00", leave national numbers as-is.
	// Set to your country code (e.g. "49" for Germany / sipgate) so
	// nationally-formatted ANIs from inbound INVITEs reach the trunk in the
	// shape sipgate documents.
	// Reference: https://help.sipgate.de/trunking/wie-setze-ich-bei-sipgate-trunking-die-absenderrufnummer
	DialCallerIDCountryCode string `env:"DIAL_CALLER_ID_COUNTRY_CODE" usage:"E.164 country code without + (e.g. 49) used to normalise display caller-IDs to sipgate trunk format"`

	// DialRingTimeoutS — default ring timeout in seconds (overridable per-Dial via TwiML timeout=).
	// Twilio range: 5–600 seconds.
	DialRingTimeoutS int `env:"DIAL_RING_TIMEOUT_S" default:"30" usage:"Default ring timeout in seconds (5–600)"`

	// DialMaxPerSession — max <Dial> calls per inbound session lifetime (rate limit).
	DialMaxPerSession int `env:"DIAL_MAX_PER_SESSION" default:"3" usage:"Max <Dial> calls per inbound session"`

	// DialMaxPerMinute — global rolling-minute outbound dial cap (toll-fraud / overload defense).
	DialMaxPerMinute int `env:"DIAL_MAX_PER_MINUTE" default:"60" usage:"Max outbound <Dial> calls per rolling minute (global)"`
}

// Load reads environment variables into Config and fails fast on misconfiguration (CFG-05).
// Errors are printed as minimal JSON to stderr so they are parseable before zerolog is initialized.
// Never returns on error — always exits non-zero.
func Load() Config {
	// Load .env file if present — silently ignored in production where vars are set directly.
	// Does not override variables already set in the process environment.
	_ = godotenv.Load("../.env", ".env")

	var cfg Config
	// SliceSep="," parses CSV slice values (used by DIAL_ALLOWED_PREFIXES).
	if err := env.Load(&cfg, &env.Options{SliceSep: ","}); err != nil {
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
			_ = conn.Close()
		}
	}

	// Post-load cross-field validation — go-simpler/env does not support cross-field checks (CFG-05)
	if cfg.RTPPortMin >= cfg.RTPPortMax {
		fmt.Fprintf(os.Stderr,
			`{"level":"fatal","msg":"RTP_PORT_MIN must be less than RTP_PORT_MAX","RTP_PORT_MIN":%d,"RTP_PORT_MAX":%d}`+"\n",
			cfg.RTPPortMin, cfg.RTPPortMax)
		os.Exit(1)
	}
	if cfg.AudioMode != "twilio" && cfg.AudioMode != "best" {
		fmt.Fprintf(os.Stderr,
			`{"level":"fatal","msg":"AUDIO_MODE must be 'twilio' or 'best'","AUDIO_MODE":%q}`+"\n",
			cfg.AudioMode)
		os.Exit(1)
	}

	// ── v3.0 Dial / B2BUA cross-field validation ──
	if cfg.DialRingTimeoutS < 5 || cfg.DialRingTimeoutS > 600 {
		fmt.Fprintf(os.Stderr,
			`{"level":"fatal","msg":"DIAL_RING_TIMEOUT_S must be in [5, 600]","value":%d}`+"\n",
			cfg.DialRingTimeoutS)
		os.Exit(1)
	}
	if cfg.DialMaxPerSession < 1 {
		fmt.Fprintf(os.Stderr,
			`{"level":"fatal","msg":"DIAL_MAX_PER_SESSION must be >= 1","value":%d}`+"\n",
			cfg.DialMaxPerSession)
		os.Exit(1)
	}
	if cfg.DialMaxPerMinute < 1 {
		fmt.Fprintf(os.Stderr,
			`{"level":"fatal","msg":"DIAL_MAX_PER_MINUTE must be >= 1","value":%d}`+"\n",
			cfg.DialMaxPerMinute)
		os.Exit(1)
	}
	// SIP_LISTEN_ADDR validation — must parse as host:port with port in [1,65535].
	if _, _, err := splitListenAddr(cfg.SIPListenAddr); err != nil {
		fmt.Fprintf(os.Stderr,
			`{"level":"fatal","msg":"SIP_LISTEN_ADDR must be host:port (e.g. 0.0.0.0:5060, 127.0.0.1:5070, [::]:5060)","value":%q,"error":%q}`+"\n",
			cfg.SIPListenAddr, err.Error())
		os.Exit(1)
	}

	// SIP_OUTBOUND_TARGET_PORT validation — 0 (default routing) or [1,65535].
	if cfg.SIPOutboundTargetPort < 0 || cfg.SIPOutboundTargetPort > 65535 {
		fmt.Fprintf(os.Stderr,
			`{"level":"fatal","msg":"SIP_OUTBOUND_TARGET_PORT must be 0 (default) or in [1,65535]","value":%d}`+"\n",
			cfg.SIPOutboundTargetPort)
		os.Exit(1)
	}

	// STATUS_CALLBACK_DEFAULT_* validation — empty URL disables; otherwise
	// must be http:// or https://, method POST or GET, events CSV must be
	// non-empty and members of the documented Twilio vocabulary.
	if cfg.StatusCallbackDefaultURL != "" {
		if !strings.HasPrefix(cfg.StatusCallbackDefaultURL, "http://") &&
			!strings.HasPrefix(cfg.StatusCallbackDefaultURL, "https://") {
			fmt.Fprintf(os.Stderr,
				`{"level":"fatal","msg":"STATUS_CALLBACK_DEFAULT_URL must start with http:// or https://","value":%q}`+"\n",
				cfg.StatusCallbackDefaultURL)
			os.Exit(1)
		}
		switch strings.ToUpper(cfg.StatusCallbackDefaultMethod) {
		case "POST", "GET":
			cfg.StatusCallbackDefaultMethod = strings.ToUpper(cfg.StatusCallbackDefaultMethod)
		default:
			fmt.Fprintf(os.Stderr,
				`{"level":"fatal","msg":"STATUS_CALLBACK_DEFAULT_METHOD must be POST or GET","value":%q}`+"\n",
				cfg.StatusCallbackDefaultMethod)
			os.Exit(1)
		}
		validEvents := map[string]struct{}{
			"initiated": {}, "ringing": {}, "answered": {},
			"in-progress": {}, "completed": {}, "busy": {},
			"failed": {}, "no-answer": {}, "canceled": {},
		}
		anyEvent := false
		for _, ev := range strings.Split(cfg.StatusCallbackDefaultEvents, ",") {
			ev = strings.TrimSpace(strings.ToLower(ev))
			if ev == "" {
				continue
			}
			anyEvent = true
			if _, ok := validEvents[ev]; !ok {
				fmt.Fprintf(os.Stderr,
					`{"level":"fatal","msg":"STATUS_CALLBACK_DEFAULT_EVENTS contains unknown event","value":%q,"valid":"initiated,ringing,answered,in-progress,completed,busy,failed,no-answer,canceled"}`+"\n",
					ev)
				os.Exit(1)
			}
		}
		if !anyEvent {
			fmt.Fprintf(os.Stderr,
				`{"level":"fatal","msg":"STATUS_CALLBACK_DEFAULT_EVENTS must list at least one event when STATUS_CALLBACK_DEFAULT_URL is set"}`+"\n")
			os.Exit(1)
		}
	}

	// Normalize + validate allow-list prefixes (no allocations on common path: empty list).
	if len(cfg.DialAllowedPrefixes) > 0 {
		norm := make([]string, 0, len(cfg.DialAllowedPrefixes))
		for _, p := range cfg.DialAllowedPrefixes {
			p = strings.TrimSpace(strings.ToLower(p))
			if p == "" {
				continue
			}
			// Allow leading '+' once; rest must be ASCII digits.
			for i, ch := range p {
				if i == 0 && ch == '+' {
					continue
				}
				if ch < '0' || ch > '9' {
					fmt.Fprintf(os.Stderr,
						`{"level":"fatal","msg":"DIAL_ALLOWED_PREFIXES entry must match ^\\+?[0-9]+$","value":%q}`+"\n",
						p)
					os.Exit(1)
				}
			}
			norm = append(norm, p)
		}
		cfg.DialAllowedPrefixes = norm
	}

	return cfg
}

// ListenPort returns the numeric port portion of SIP_LISTEN_ADDR. Drives the
// Contact: header port (where sipgate delivers inbound INVITEs) so a non-default
// listen port produces a coherent Contact. Validated by Load().
func (c Config) ListenPort() int {
	_, port, err := splitListenAddr(c.SIPListenAddr)
	if err != nil {
		// Validated at Load() — should be unreachable. Defensive default 5060.
		return 5060
	}
	return port
}

// splitListenAddr parses host:port. Accepts IPv4 ("0.0.0.0:5060"), IPv6 with
// brackets ("[::]:5060"), and bare-port forms (":5060" → host=""). Returns
// host string and numeric port. Used by Load() validation and ListenPort().
func splitListenAddr(addr string) (string, int, error) {
	if addr == "" {
		return "", 0, fmt.Errorf("empty address")
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("port not numeric: %w", err)
	}
	if port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("port out of range: %d", port)
	}
	return host, port, nil
}
