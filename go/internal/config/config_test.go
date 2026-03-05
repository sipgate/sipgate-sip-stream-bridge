package config_test // black-box test package

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/sipgate/audio-dock/internal/config"
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
	default:
		os.Exit(m.Run())
	}
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
