package sip

import (
	"strings"
	"testing"

	"github.com/pion/sdp/v3"
)

// Test 1 — ParseCallerSDP extracts DTMF PT 113 (sipgate-style offer)
func TestParseCallerSDP_DTMFPT113(t *testing.T) {
	sdpBody := []byte("v=0\r\n" +
		"o=- 1 1 IN IP4 1.2.3.4\r\n" +
		"s=-\r\n" +
		"c=IN IP4 1.2.3.4\r\n" +
		"t=0 0\r\n" +
		"m=audio 20000 RTP/AVP 0 113\r\n" +
		"a=rtpmap:0 PCMU/8000\r\n" +
		"a=rtpmap:113 telephone-event/8000\r\n" +
		"a=fmtp:113 0-16\r\n")

	result, err := ParseCallerSDP(sdpBody)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if result.RTPAddr != "1.2.3.4" {
		t.Errorf("expected RTPAddr=1.2.3.4, got=%s", result.RTPAddr)
	}
	if result.RTPPort != 20000 {
		t.Errorf("expected RTPPort=20000, got=%d", result.RTPPort)
	}
	if result.DTMFPayloadType != 113 {
		t.Errorf("expected DTMFPayloadType=113, got=%d", result.DTMFPayloadType)
	}
}

// Test 2 — ParseCallerSDP falls back to PT 101 if no telephone-event in offer
func TestParseCallerSDP_FallbackPT101(t *testing.T) {
	sdpBody := []byte("v=0\r\n" +
		"o=- 1 1 IN IP4 1.2.3.4\r\n" +
		"s=-\r\n" +
		"c=IN IP4 1.2.3.4\r\n" +
		"t=0 0\r\n" +
		"m=audio 20000 RTP/AVP 0\r\n" +
		"a=rtpmap:0 PCMU/8000\r\n")

	result, err := ParseCallerSDP(sdpBody)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if result.RTPAddr != "1.2.3.4" {
		t.Errorf("expected RTPAddr=1.2.3.4, got=%s", result.RTPAddr)
	}
	if result.RTPPort != 20000 {
		t.Errorf("expected RTPPort=20000, got=%d", result.RTPPort)
	}
	if result.DTMFPayloadType != 101 {
		t.Errorf("expected DTMFPayloadType=101 (fallback), got=%d", result.DTMFPayloadType)
	}
}

// Test 3 — ParseCallerSDP reads session-level c= when no per-media c= exists
func TestParseCallerSDP_SessionLevelConnection(t *testing.T) {
	sdpBody := []byte("v=0\r\n" +
		"o=- 1 1 IN IP4 5.6.7.8\r\n" +
		"s=-\r\n" +
		"c=IN IP4 5.6.7.8\r\n" +
		"t=0 0\r\n" +
		"m=audio 20000 RTP/AVP 0\r\n" +
		"a=rtpmap:0 PCMU/8000\r\n")

	result, err := ParseCallerSDP(sdpBody)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if result.RTPAddr != "5.6.7.8" {
		t.Errorf("expected RTPAddr=5.6.7.8 (session-level), got=%s", result.RTPAddr)
	}
	if result.RTPPort != 20000 {
		t.Errorf("expected RTPPort=20000, got=%d", result.RTPPort)
	}
	if result.DTMFPayloadType != 101 {
		t.Errorf("expected DTMFPayloadType=101 (fallback), got=%d", result.DTMFPayloadType)
	}
}

// Test 4 — ParseCallerSDP errors when no audio section
func TestParseCallerSDP_NoAudioSection(t *testing.T) {
	sdpBody := []byte("v=0\r\n" +
		"o=- 1 1 IN IP4 1.2.3.4\r\n" +
		"s=-\r\n" +
		"c=IN IP4 1.2.3.4\r\n" +
		"t=0 0\r\n" +
		"m=video 9000 RTP/AVP 96\r\n" +
		"a=rtpmap:96 H264/90000\r\n")

	_, err := ParseCallerSDP(sdpBody)
	if err == nil {
		t.Fatal("expected error for SDP with no audio section, got nil")
	}
	if !strings.Contains(err.Error(), "no audio media section") {
		t.Errorf("expected error to contain 'no audio media section', got: %v", err)
	}
}

// Test 5 — BuildSDPAnswer contains PCMU PT 0 and mirrors callerDTMFPT 113
func TestBuildSDPAnswer_ContainsPCMUAndDTMF(t *testing.T) {
	out := BuildSDPAnswer("10.0.0.1", 10000, 113)
	if len(out) == 0 {
		t.Fatal("BuildSDPAnswer returned empty bytes")
	}

	// Parse the output to validate structure
	result := &sdp.SessionDescription{}
	if err := result.Unmarshal(out); err != nil {
		t.Fatalf("BuildSDPAnswer output is not valid SDP: %v", err)
	}

	// Find the audio media section
	var audioMD *sdp.MediaDescription
	for i, md := range result.MediaDescriptions {
		if md.MediaName.Media == "audio" {
			audioMD = result.MediaDescriptions[i]
			break
		}
	}
	if audioMD == nil {
		t.Fatal("BuildSDPAnswer output has no audio media section")
	}

	// Check that formats include "0" (PCMU) and "113" (DTMF)
	formats := audioMD.MediaName.Formats
	hasPCMU := false
	hasDTMF := false
	for _, f := range formats {
		if f == "0" {
			hasPCMU = true
		}
		if f == "113" {
			hasDTMF = true
		}
	}
	if !hasPCMU {
		t.Errorf("BuildSDPAnswer formats missing PT 0 (PCMU): %v", formats)
	}
	if !hasDTMF {
		t.Errorf("BuildSDPAnswer formats missing PT 113 (DTMF): %v", formats)
	}

	// Check c= line IP is correct
	if result.ConnectionInformation == nil || result.ConnectionInformation.Address == nil {
		t.Fatal("BuildSDPAnswer has no session-level connection information")
	}
	if result.ConnectionInformation.Address.Address != "10.0.0.1" {
		t.Errorf("expected c= IP=10.0.0.1, got=%s", result.ConnectionInformation.Address.Address)
	}
}

// Test 6 — BuildSDPAnswer uses sendrecv attribute
func TestBuildSDPAnswer_SendRecv(t *testing.T) {
	out := BuildSDPAnswer("10.0.0.1", 10000, 113)
	if !strings.Contains(string(out), "a=sendrecv") {
		t.Errorf("BuildSDPAnswer output missing 'a=sendrecv':\n%s", string(out))
	}
}
