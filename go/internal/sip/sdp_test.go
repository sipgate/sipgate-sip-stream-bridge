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
	if len(result.AudioPayloadTypes) != 1 || result.AudioPayloadTypes[0] != 0 {
		t.Errorf("expected AudioPayloadTypes=[0], got=%v", result.AudioPayloadTypes)
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
	if len(result.AudioPayloadTypes) != 1 || result.AudioPayloadTypes[0] != 0 {
		t.Errorf("expected AudioPayloadTypes=[0], got=%v", result.AudioPayloadTypes)
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

// Test 5 — ParseCallerSDP extracts multiple audio PTs (sipgate-style wideband offer)
func TestParseCallerSDP_WidebandOffer(t *testing.T) {
	sdpBody := []byte("v=0\r\n" +
		"o=- 1 1 IN IP4 1.2.3.4\r\n" +
		"s=-\r\n" +
		"c=IN IP4 1.2.3.4\r\n" +
		"t=0 0\r\n" +
		"m=audio 20000 RTP/AVP 9 8 0 113\r\n" +
		"a=rtpmap:9 G722/8000\r\n" +
		"a=rtpmap:8 PCMA/8000\r\n" +
		"a=rtpmap:0 PCMU/8000\r\n" +
		"a=rtpmap:113 telephone-event/8000\r\n" +
		"a=fmtp:113 0-16\r\n")

	result, err := ParseCallerSDP(sdpBody)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if result.DTMFPayloadType != 113 {
		t.Errorf("expected DTMFPayloadType=113, got=%d", result.DTMFPayloadType)
	}
	// AudioPayloadTypes must contain 9, 8, 0 (in offer order) but NOT 113
	if len(result.AudioPayloadTypes) != 3 {
		t.Fatalf("expected 3 audio PTs, got=%v", result.AudioPayloadTypes)
	}
	if result.AudioPayloadTypes[0] != 9 || result.AudioPayloadTypes[1] != 8 || result.AudioPayloadTypes[2] != 0 {
		t.Errorf("expected AudioPayloadTypes=[9,8,0], got=%v", result.AudioPayloadTypes)
	}
}

// callerSDPWithPTs is a helper that builds a CallerSDP for BuildSDPAnswer tests.
func callerSDPWithPTs(dtmfPT uint8, audioPTs ...uint8) *CallerSDP {
	return &CallerSDP{
		RTPAddr:           "1.2.3.4",
		RTPPort:           20000,
		DTMFPayloadType:   dtmfPT,
		AudioPayloadTypes: audioPTs,
	}
}

// Test 6 — BuildSDPAnswer (twilio mode) contains PCMU PT 0 and mirrors callerDTMFPT 113
func TestBuildSDPAnswer_TwilioContainsPCMUAndDTMF(t *testing.T) {
	caller := callerSDPWithPTs(113, 0)
	out, negotiatedPT := BuildSDPAnswer("10.0.0.1", 10000, caller, "twilio")
	if len(out) == 0 {
		t.Fatal("BuildSDPAnswer returned empty bytes")
	}
	if negotiatedPT != 0 {
		t.Errorf("expected negotiatedPT=0 (PCMU), got=%d", negotiatedPT)
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

// Test 7 — BuildSDPAnswer (twilio mode) uses sendrecv attribute
func TestBuildSDPAnswer_TwilioSendRecv(t *testing.T) {
	caller := callerSDPWithPTs(113, 0)
	out, _ := BuildSDPAnswer("10.0.0.1", 10000, caller, "twilio")
	if !strings.Contains(string(out), "a=sendrecv") {
		t.Errorf("BuildSDPAnswer output missing 'a=sendrecv':\n%s", string(out))
	}
}

// Test 8 — BuildSDPAnswer (best mode) negotiates G.722 when offered
func TestBuildSDPAnswer_BestModeSelectsG722(t *testing.T) {
	// Simulate sipgate offer: G.722, PCMA, PCMU, DTMF
	caller := callerSDPWithPTs(113, 9, 8, 0)
	out, negotiatedPT := BuildSDPAnswer("10.0.0.1", 10000, caller, "best")
	if len(out) == 0 {
		t.Fatal("BuildSDPAnswer returned empty bytes")
	}
	if negotiatedPT != 9 {
		t.Errorf("expected negotiatedPT=9 (G.722), got=%d", negotiatedPT)
	}

	// SDP answer must contain G.722 rtpmap
	if !strings.Contains(string(out), "G722") {
		t.Errorf("expected G722 in SDP answer:\n%s", string(out))
	}

	// G.722 must be the first format
	result := &sdp.SessionDescription{}
	if err := result.Unmarshal(out); err != nil {
		t.Fatalf("BuildSDPAnswer output is not valid SDP: %v", err)
	}
	var audioMD *sdp.MediaDescription
	for i, md := range result.MediaDescriptions {
		if md.MediaName.Media == "audio" {
			audioMD = result.MediaDescriptions[i]
			break
		}
	}
	if audioMD == nil {
		t.Fatal("no audio media section in best-mode answer")
	}
	if len(audioMD.MediaName.Formats) == 0 || audioMD.MediaName.Formats[0] != "9" {
		t.Errorf("expected G.722 (PT 9) as first format, got=%v", audioMD.MediaName.Formats)
	}
}

// Test 9 — BuildSDPAnswer (best mode) falls back to PCMU when G.722/PCMA not offered
func TestBuildSDPAnswer_BestModeFallbackPCMU(t *testing.T) {
	caller := callerSDPWithPTs(113, 0) // only PCMU offered
	out, negotiatedPT := BuildSDPAnswer("10.0.0.1", 10000, caller, "best")
	if negotiatedPT != 0 {
		t.Errorf("expected negotiatedPT=0 (PCMU fallback), got=%d", negotiatedPT)
	}
	if !strings.Contains(string(out), "PCMU") {
		t.Errorf("expected PCMU in SDP answer:\n%s", string(out))
	}
}
