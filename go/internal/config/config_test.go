package config_test // black-box test package

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/sipgate/sipgate-sip-stream-bridge/internal/config"
)

func TestLoad_AllRequired_ReturnsConfig(t *testing.T) {
	t.Setenv("SIP_USER", "e12345p0")
	t.Setenv("SIP_PASSWORD", "secret")
	t.Setenv("SIP_DOMAIN", "sipconnect.sipgate.de")
	t.Setenv("SIP_REGISTRAR", "sipconnect.sipgate.de")
	t.Setenv("WS_TARGET_URL", "wss://example.com/ws")
	t.Setenv("SDP_CONTACT_IP", "1.2.3.4")

	cfg := config.Load()

	if cfg.SIPUser != "e12345p0" {
		t.Errorf("SIPUser = %q, want e12345p0", cfg.SIPUser)
	}
	if cfg.RTPPortMin != 10000 {
		t.Errorf("RTPPortMin = %d, want 10000 (default)", cfg.RTPPortMin)
	}
	if cfg.RTPPortMax != 10099 {
		t.Errorf("RTPPortMax = %d, want 10099 (default)", cfg.RTPPortMax)
	}
}

// Subprocess helper — called by the test binary itself with SUBPROC_SCENARIO set
func TestMain(m *testing.M) {
	switch os.Getenv("SUBPROC_SCENARIO") {
	case "missing_sip_user":
		// All vars set EXCEPT SIP_USER — Load() should exit(1)
		os.Setenv("SIP_PASSWORD", "secret")
		os.Setenv("SIP_DOMAIN", "sipconnect.sipgate.de")
		os.Setenv("SIP_REGISTRAR", "sipconnect.sipgate.de")
		os.Setenv("WS_TARGET_URL", "wss://example.com/ws")
		os.Setenv("SDP_CONTACT_IP", "1.2.3.4")
		config.Load()
		os.Exit(0) // should never reach — Load exits
	case "inverted_rtp_ports":
		os.Setenv("SIP_USER", "e12345p0")
		os.Setenv("SIP_PASSWORD", "secret")
		os.Setenv("SIP_DOMAIN", "sipconnect.sipgate.de")
		os.Setenv("SIP_REGISTRAR", "sipconnect.sipgate.de")
		os.Setenv("WS_TARGET_URL", "wss://example.com/ws")
		os.Setenv("SDP_CONTACT_IP", "1.2.3.4")
		os.Setenv("RTP_PORT_MIN", "20000")
		os.Setenv("RTP_PORT_MAX", "10000")
		config.Load()
		os.Exit(0) // should never reach
	case "missing_sdp_contact_ip":
		os.Setenv("SIP_USER", "e12345p0")
		os.Setenv("SIP_PASSWORD", "secret")
		os.Setenv("SIP_DOMAIN", "sipconnect.sipgate.de")
		os.Setenv("SIP_REGISTRAR", "sipconnect.sipgate.de")
		os.Setenv("WS_TARGET_URL", "wss://example.com/ws")
		cfg := config.Load()
		if cfg.SDPContactIP == "" {
			fmt.Fprintln(os.Stderr, "SDPContactIP is empty after Load()")
			os.Exit(1)
		}
		os.Exit(0)
	case "dial_ring_timeout_too_low":
		setBaseEnv()
		os.Setenv("DIAL_RING_TIMEOUT_S", "4")
		config.Load()
		os.Exit(0) // should never reach
	case "dial_ring_timeout_too_high":
		setBaseEnv()
		os.Setenv("DIAL_RING_TIMEOUT_S", "601")
		config.Load()
		os.Exit(0) // should never reach
	case "dial_max_per_session_zero":
		setBaseEnv()
		os.Setenv("DIAL_MAX_PER_SESSION", "0")
		config.Load()
		os.Exit(0) // should never reach
	case "dial_max_per_minute_zero":
		setBaseEnv()
		os.Setenv("DIAL_MAX_PER_MINUTE", "0")
		config.Load()
		os.Exit(0) // should never reach
	case "dial_allowed_prefixes_invalid":
		setBaseEnv()
		os.Setenv("DIAL_ALLOWED_PREFIXES", "abc")
		config.Load()
		os.Exit(0) // should never reach
	default:
		os.Exit(m.Run())
	}
}

// setBaseEnv sets all required env vars to plausible values (used by subprocess scenarios
// that need a valid base config plus one bad DIAL_* var).
func setBaseEnv() {
	os.Setenv("SIP_USER", "e12345p0")
	os.Setenv("SIP_PASSWORD", "secret")
	os.Setenv("SIP_DOMAIN", "sipconnect.sipgate.de")
	os.Setenv("SIP_REGISTRAR", "sipconnect.sipgate.de")
	os.Setenv("WS_TARGET_URL", "wss://example.com/ws")
	os.Setenv("SDP_CONTACT_IP", "1.2.3.4")
}

func TestLoad_MissingSIPUser_ExitsNonZero(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestMain")
	cmd.Env = append([]string{"SUBPROC_SCENARIO=missing_sip_user"}, os.Environ()...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit, got nil error")
	}
	output := string(out)
	if !strings.Contains(output, "SIP_USER") {
		t.Errorf("expected output to mention SIP_USER, got: %s", output)
	}
}

func TestLoad_InvertedRTPPorts_ExitsNonZero(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestMain")
	cmd.Env = append([]string{"SUBPROC_SCENARIO=inverted_rtp_ports"}, os.Environ()...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit, got nil error")
	}
	output := string(out)
	if !strings.Contains(output, "RTP_PORT_MIN") {
		t.Errorf("expected output to mention RTP_PORT_MIN, got: %s", output)
	}
}

func TestLoad_MissingSDPContactIP_DefaultsToLocalIP(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestMain")
	cmd.Env = append([]string{"SUBPROC_SCENARIO=missing_sdp_contact_ip"}, os.Environ()...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expected zero exit (auto-detect), got error: %v\noutput: %s", err, out)
	}
}

func TestLoad_SIPOptionsInterval_DefaultIs30s(t *testing.T) {
	t.Setenv("SIP_USER", "e12345p0")
	t.Setenv("SIP_PASSWORD", "secret")
	t.Setenv("SIP_DOMAIN", "sipconnect.sipgate.de")
	t.Setenv("SIP_REGISTRAR", "sipconnect.sipgate.de")
	t.Setenv("WS_TARGET_URL", "wss://example.com/ws")
	t.Setenv("SDP_CONTACT_IP", "1.2.3.4")
	// SIP_OPTIONS_INTERVAL not set — default should be 30s

	cfg := config.Load()

	const want = 30e9 // 30 * time.Second in nanoseconds
	if cfg.SIPOptionsInterval != want {
		t.Errorf("SIPOptionsInterval = %v, want 30s", cfg.SIPOptionsInterval)
	}
}

func TestLoad_SIPOptionsInterval_Override1m(t *testing.T) {
	t.Setenv("SIP_USER", "e12345p0")
	t.Setenv("SIP_PASSWORD", "secret")
	t.Setenv("SIP_DOMAIN", "sipconnect.sipgate.de")
	t.Setenv("SIP_REGISTRAR", "sipconnect.sipgate.de")
	t.Setenv("WS_TARGET_URL", "wss://example.com/ws")
	t.Setenv("SDP_CONTACT_IP", "1.2.3.4")
	t.Setenv("SIP_OPTIONS_INTERVAL", "1m")

	cfg := config.Load()

	const want = 60e9 // 60 * time.Second in nanoseconds
	if cfg.SIPOptionsInterval != want {
		t.Errorf("SIPOptionsInterval = %v, want 1m (60s)", cfg.SIPOptionsInterval)
	}
}

// ── v3.0 Dial / B2BUA config tests ──

func TestLoad_DialDefaults(t *testing.T) {
	t.Setenv("SIP_USER", "e12345p0")
	t.Setenv("SIP_PASSWORD", "secret")
	t.Setenv("SIP_DOMAIN", "sipconnect.sipgate.de")
	t.Setenv("SIP_REGISTRAR", "sipconnect.sipgate.de")
	t.Setenv("WS_TARGET_URL", "wss://example.com/ws")
	t.Setenv("SDP_CONTACT_IP", "1.2.3.4")

	cfg := config.Load()

	if cfg.DialRingTimeoutS != 30 {
		t.Errorf("DialRingTimeoutS = %d, want 30 (default)", cfg.DialRingTimeoutS)
	}
	if cfg.DialMaxPerSession != 3 {
		t.Errorf("DialMaxPerSession = %d, want 3 (default)", cfg.DialMaxPerSession)
	}
	if cfg.DialMaxPerMinute != 60 {
		t.Errorf("DialMaxPerMinute = %d, want 60 (default)", cfg.DialMaxPerMinute)
	}
	if len(cfg.DialAllowedPrefixes) != 0 {
		t.Errorf("DialAllowedPrefixes = %v, want empty (default-deny)", cfg.DialAllowedPrefixes)
	}
	if cfg.DialDefaultCallerID != "" {
		t.Errorf("DialDefaultCallerID = %q, want empty (default)", cfg.DialDefaultCallerID)
	}
}

func TestLoad_DialAllowedPrefixes_CSVParsing(t *testing.T) {
	t.Setenv("SIP_USER", "e12345p0")
	t.Setenv("SIP_PASSWORD", "secret")
	t.Setenv("SIP_DOMAIN", "sipconnect.sipgate.de")
	t.Setenv("SIP_REGISTRAR", "sipconnect.sipgate.de")
	t.Setenv("WS_TARGET_URL", "wss://example.com/ws")
	t.Setenv("SDP_CONTACT_IP", "1.2.3.4")
	t.Setenv("DIAL_ALLOWED_PREFIXES", "+49,+44,+1")

	cfg := config.Load()

	want := []string{"+49", "+44", "+1"}
	if len(cfg.DialAllowedPrefixes) != len(want) {
		t.Fatalf("DialAllowedPrefixes len = %d, want %d (got %v)", len(cfg.DialAllowedPrefixes), len(want), cfg.DialAllowedPrefixes)
	}
	for i, p := range want {
		if cfg.DialAllowedPrefixes[i] != p {
			t.Errorf("DialAllowedPrefixes[%d] = %q, want %q", i, cfg.DialAllowedPrefixes[i], p)
		}
	}
}

func TestLoad_DialAllowedPrefixes_Normalization(t *testing.T) {
	t.Setenv("SIP_USER", "e12345p0")
	t.Setenv("SIP_PASSWORD", "secret")
	t.Setenv("SIP_DOMAIN", "sipconnect.sipgate.de")
	t.Setenv("SIP_REGISTRAR", "sipconnect.sipgate.de")
	t.Setenv("WS_TARGET_URL", "wss://example.com/ws")
	t.Setenv("SDP_CONTACT_IP", "1.2.3.4")
	// Mix whitespace, mixed case (digits aren't case-sensitive but the trim path should run).
	t.Setenv("DIAL_ALLOWED_PREFIXES", "  +49 , +44, +1 ")

	cfg := config.Load()

	want := []string{"+49", "+44", "+1"}
	if len(cfg.DialAllowedPrefixes) != len(want) {
		t.Fatalf("DialAllowedPrefixes len = %d, want %d (got %v)", len(cfg.DialAllowedPrefixes), len(want), cfg.DialAllowedPrefixes)
	}
	for i, p := range want {
		if cfg.DialAllowedPrefixes[i] != p {
			t.Errorf("DialAllowedPrefixes[%d] = %q, want %q", i, cfg.DialAllowedPrefixes[i], p)
		}
	}
}

func TestLoad_DialRingTimeoutTooLow_ExitsNonZero(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestMain")
	cmd.Env = append([]string{"SUBPROC_SCENARIO=dial_ring_timeout_too_low"}, os.Environ()...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit, got nil error")
	}
	output := string(out)
	if !strings.Contains(output, "DIAL_RING_TIMEOUT_S") {
		t.Errorf("expected output to mention DIAL_RING_TIMEOUT_S, got: %s", output)
	}
}

func TestLoad_DialRingTimeoutTooHigh_ExitsNonZero(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestMain")
	cmd.Env = append([]string{"SUBPROC_SCENARIO=dial_ring_timeout_too_high"}, os.Environ()...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit, got nil error")
	}
	output := string(out)
	if !strings.Contains(output, "DIAL_RING_TIMEOUT_S") {
		t.Errorf("expected output to mention DIAL_RING_TIMEOUT_S, got: %s", output)
	}
}

func TestLoad_DialMaxPerSessionZero_ExitsNonZero(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestMain")
	cmd.Env = append([]string{"SUBPROC_SCENARIO=dial_max_per_session_zero"}, os.Environ()...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit, got nil error")
	}
	output := string(out)
	if !strings.Contains(output, "DIAL_MAX_PER_SESSION") {
		t.Errorf("expected output to mention DIAL_MAX_PER_SESSION, got: %s", output)
	}
}

func TestLoad_DialMaxPerMinuteZero_ExitsNonZero(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestMain")
	cmd.Env = append([]string{"SUBPROC_SCENARIO=dial_max_per_minute_zero"}, os.Environ()...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit, got nil error")
	}
	output := string(out)
	if !strings.Contains(output, "DIAL_MAX_PER_MINUTE") {
		t.Errorf("expected output to mention DIAL_MAX_PER_MINUTE, got: %s", output)
	}
}

func TestLoad_DialAllowedPrefixesInvalid_ExitsNonZero(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestMain")
	cmd.Env = append([]string{"SUBPROC_SCENARIO=dial_allowed_prefixes_invalid"}, os.Environ()...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit, got nil error")
	}
	output := string(out)
	if !strings.Contains(output, "DIAL_ALLOWED_PREFIXES") {
		t.Errorf("expected output to mention DIAL_ALLOWED_PREFIXES, got: %s", output)
	}
}
