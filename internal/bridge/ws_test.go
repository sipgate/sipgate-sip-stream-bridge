package bridge

import (
	"encoding/json"
	"net"
	"strings"
	"testing"

	siplib "github.com/emiago/sipgo/sip"
)

// mockSIPRequest creates a minimal *siplib.Request with From and To headers set.
// Used to test sendStart's customParameters extraction.
// URI fields are set directly: scheme "sip", user and host parsed from "user@host" form.
func mockSIPRequest(fromUser, fromHost, toUser, toHost string) *siplib.Request {
	req := siplib.NewRequest(siplib.INVITE, siplib.Uri{})
	fromHdr := &siplib.FromHeader{
		Address: siplib.Uri{Scheme: "sip", User: fromUser, Host: fromHost},
	}
	toHdr := &siplib.ToHeader{
		Address: siplib.Uri{Scheme: "sip", User: toUser, Host: toHost},
	}
	req.AppendHeader(fromHdr)
	req.AppendHeader(toHdr)
	return req
}

// bufConn wraps a strings.Builder as a net.Conn for write-only testing.
// Only Write is used; all other methods return zero values.
type bufConn struct {
	net.Conn
	buf *strings.Builder
}

func (b *bufConn) Write(p []byte) (int, error) {
	return b.buf.Write(p)
}

func (b *bufConn) Close() error { return nil }

// Test 1 — sendConnected marshals correct JSON schema (WSB-01).
// Expected: {"event":"connected","protocol":"Call","version":"1.0.0"}
func TestSendConnected_JSONSchema(t *testing.T) {
	var sb strings.Builder
	conn := &bufConn{buf: &sb}

	if err := sendConnected(conn); err != nil {
		t.Fatalf("sendConnected: unexpected error: %v", err)
	}

	written := sb.String()
	var got ConnectedEvent
	if err := json.Unmarshal([]byte(written), &got); err != nil {
		t.Fatalf("JSON unmarshal failed: %v\nRaw: %s", err, written)
	}

	if got.Event != "connected" {
		t.Errorf("Event: expected %q, got %q", "connected", got.Event)
	}
	if got.Protocol != "Call" {
		t.Errorf("Protocol: expected %q, got %q", "Call", got.Protocol)
	}
	if got.Version != "1.0.0" {
		t.Errorf("Version: expected %q, got %q", "1.0.0", got.Version)
	}
}

// Test 2 — sendStart marshals correct JSON schema (WSB-02 + WSB-06).
// Verifies: event="start", sequenceNumber="1", tracks=["inbound","outbound"],
// mediaFormat.encoding="audio/x-mulaw", mediaFormat.sampleRate=8000,
// customParameters.CallSid=callID, customParameters.From=from URI.
func TestSendStart_JSONSchema(t *testing.T) {
	var sb strings.Builder
	conn := &bufConn{buf: &sb}

	req := mockSIPRequest("a", "b.com", "c", "d.com")
	streamSid := "MZabc"
	callID := "test-call-id"

	if err := sendStart(conn, streamSid, callID, req); err != nil {
		t.Fatalf("sendStart: unexpected error: %v", err)
	}

	written := sb.String()
	var got StartEvent
	if err := json.Unmarshal([]byte(written), &got); err != nil {
		t.Fatalf("JSON unmarshal failed: %v\nRaw: %s", err, written)
	}

	if got.Event != "start" {
		t.Errorf("Event: expected %q, got %q", "start", got.Event)
	}
	if got.SequenceNumber != "1" {
		t.Errorf("SequenceNumber: expected %q, got %q", "1", got.SequenceNumber)
	}
	if got.StreamSid != streamSid {
		t.Errorf("StreamSid: expected %q, got %q", streamSid, got.StreamSid)
	}

	// Start body
	body := got.Start
	if body.StreamSid != streamSid {
		t.Errorf("Start.StreamSid: expected %q, got %q", streamSid, body.StreamSid)
	}
	if body.CallSid != callID {
		t.Errorf("Start.CallSid: expected %q, got %q", callID, body.CallSid)
	}
	if body.AccountSid != "" {
		t.Errorf("Start.AccountSid: expected empty string, got %q", body.AccountSid)
	}

	// Tracks
	if len(body.Tracks) != 2 || body.Tracks[0] != "inbound" || body.Tracks[1] != "outbound" {
		t.Errorf("Tracks: expected [inbound outbound], got %v", body.Tracks)
	}

	// MediaFormat
	if body.MediaFormat.Encoding != "audio/x-mulaw" {
		t.Errorf("MediaFormat.Encoding: expected %q, got %q", "audio/x-mulaw", body.MediaFormat.Encoding)
	}
	if body.MediaFormat.SampleRate != 8000 {
		t.Errorf("MediaFormat.SampleRate: expected 8000, got %d", body.MediaFormat.SampleRate)
	}
	if body.MediaFormat.Channels != 1 {
		t.Errorf("MediaFormat.Channels: expected 1, got %d", body.MediaFormat.Channels)
	}

	// CustomParameters
	if body.CustomParameters["CallSid"] != callID {
		t.Errorf("CustomParameters.CallSid: expected %q, got %q", callID, body.CustomParameters["CallSid"])
	}
	// From address string should contain "a@b.com" (siplib.Uri.String() format is "sip:user@host")
	if from := body.CustomParameters["From"]; !strings.Contains(from, "a@b.com") {
		t.Errorf("CustomParameters.From: expected to contain %q, got %q", "a@b.com", from)
	}
}

// Test 3 — sendStop marshals correct JSON schema (WSB-04).
// sequenceNumber is intentionally empty in Phase 6.
func TestSendStop_JSONSchema(t *testing.T) {
	var sb strings.Builder
	conn := &bufConn{buf: &sb}

	streamSid := "MZabc"
	callID := "test-call-id"

	if err := sendStop(conn, streamSid, callID); err != nil {
		t.Fatalf("sendStop: unexpected error: %v", err)
	}

	written := sb.String()
	var got StopEvent
	if err := json.Unmarshal([]byte(written), &got); err != nil {
		t.Fatalf("JSON unmarshal failed: %v\nRaw: %s", err, written)
	}

	if got.Event != "stop" {
		t.Errorf("Event: expected %q, got %q", "stop", got.Event)
	}
	if got.StreamSid != streamSid {
		t.Errorf("StreamSid: expected %q, got %q", streamSid, got.StreamSid)
	}
	// SequenceNumber is intentionally empty in Phase 6
	if got.SequenceNumber != "" {
		t.Errorf("SequenceNumber: expected empty string (Phase 6 deferred), got %q", got.SequenceNumber)
	}
	if got.Stop.CallSid != callID {
		t.Errorf("Stop.CallSid: expected %q, got %q", callID, got.Stop.CallSid)
	}
	if got.Stop.AccountSid != "" {
		t.Errorf("Stop.AccountSid: expected empty string, got %q", got.Stop.AccountSid)
	}
}

// Test 4 — writeJSON + readWSMessage round-trip using net.Pipe().
// net.Pipe() does not do WebSocket framing — this tests the JSON layer only.
// writeJSON uses plain JSON marshal (the bufConn.Write path); readWSMessage
// wraps wsutil.ReadServerData for use in production. For the pipe round-trip,
// we write and read raw JSON bytes directly to verify struct equality.
func TestWriteJSON_RoundTrip(t *testing.T) {
	// Write side: marshal to JSON and verify unmarshal equality.
	// We use a bufConn (write-only mock) because net.Pipe would require
	// WebSocket framing. The round-trip test verifies the JSON marshal/unmarshal
	// contract — that ConnectedEvent fields survive the encoding cycle.
	var sb strings.Builder
	conn := &bufConn{buf: &sb}

	original := ConnectedEvent{
		Event:    "connected",
		Protocol: "Call",
		Version:  "1.0.0",
	}

	if err := writeJSON(conn, original); err != nil {
		t.Fatalf("writeJSON: unexpected error: %v", err)
	}

	var roundTripped ConnectedEvent
	if err := json.Unmarshal([]byte(sb.String()), &roundTripped); err != nil {
		t.Fatalf("json.Unmarshal: unexpected error: %v\nRaw: %s", err, sb.String())
	}

	if roundTripped.Event != original.Event {
		t.Errorf("Event: expected %q, got %q", original.Event, roundTripped.Event)
	}
	if roundTripped.Protocol != original.Protocol {
		t.Errorf("Protocol: expected %q, got %q", original.Protocol, roundTripped.Protocol)
	}
	if roundTripped.Version != original.Version {
		t.Errorf("Version: expected %q, got %q", original.Version, roundTripped.Version)
	}
}
