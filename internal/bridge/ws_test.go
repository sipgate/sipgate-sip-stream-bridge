package bridge

import (
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/gobwas/ws/wsutil"
	siplib "github.com/emiago/sipgo/sip"
)

// mockSIPRequest creates a minimal *siplib.Request with From and To headers set.
// Used to test sendStart's customParameters extraction.
// URI fields are set directly using siplib.Uri struct literals.
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

// newPipe returns a synchronous in-process net.Conn pair using net.Pipe().
// The caller is responsible for closing both ends.
func newPipe(t *testing.T) (client, server net.Conn) {
	t.Helper()
	c, s := net.Pipe()
	return c, s
}

// Test 1 — sendConnected marshals correct JSON schema (WSB-01).
// Expected JSON: {"event":"connected","protocol":"Call","version":"1.0.0"}
//
// writeJSON uses wsutil.WriteClientText (adds RFC 6455 client-side masking).
// wsutil.ReadClientData strips the WS frame on the server end, returning raw JSON.
func TestSendConnected_JSONSchema(t *testing.T) {
	client, server := newPipe(t)
	defer client.Close()
	defer server.Close()

	errCh := make(chan error, 1)
	go func() {
		errCh <- sendConnected(client)
	}()

	data, _, err := wsutil.ReadClientData(server)
	if err != nil {
		t.Fatalf("ReadClientData: unexpected error: %v", err)
	}
	if writeErr := <-errCh; writeErr != nil {
		t.Fatalf("sendConnected: unexpected error: %v", writeErr)
	}

	var got ConnectedEvent
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("JSON unmarshal failed: %v\nRaw: %s", err, string(data))
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
// customParameters.CallSid=callID, customParameters.From contains "a@b.com".
func TestSendStart_JSONSchema(t *testing.T) {
	client, server := newPipe(t)
	defer client.Close()
	defer server.Close()

	req := mockSIPRequest("a", "b.com", "c", "d.com")
	streamSid := "MZabc"
	callID := "test-call-id"

	errCh := make(chan error, 1)
	go func() {
		errCh <- sendStart(client, streamSid, callID, req)
	}()

	data, _, err := wsutil.ReadClientData(server)
	if err != nil {
		t.Fatalf("ReadClientData: unexpected error: %v", err)
	}
	if writeErr := <-errCh; writeErr != nil {
		t.Fatalf("sendStart: unexpected error: %v", writeErr)
	}

	var got StartEvent
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("JSON unmarshal failed: %v\nRaw: %s", err, string(data))
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

	if len(body.Tracks) != 2 || body.Tracks[0] != "inbound" || body.Tracks[1] != "outbound" {
		t.Errorf("Tracks: expected [inbound outbound], got %v", body.Tracks)
	}

	if body.MediaFormat.Encoding != "audio/x-mulaw" {
		t.Errorf("MediaFormat.Encoding: expected %q, got %q", "audio/x-mulaw", body.MediaFormat.Encoding)
	}
	if body.MediaFormat.SampleRate != 8000 {
		t.Errorf("MediaFormat.SampleRate: expected 8000, got %d", body.MediaFormat.SampleRate)
	}
	if body.MediaFormat.Channels != 1 {
		t.Errorf("MediaFormat.Channels: expected 1, got %d", body.MediaFormat.Channels)
	}

	if body.CustomParameters["CallSid"] != callID {
		t.Errorf("CustomParameters.CallSid: expected %q, got %q", callID, body.CustomParameters["CallSid"])
	}
	// siplib.Uri.String() produces "sip:user@host"
	if from := body.CustomParameters["From"]; !strings.Contains(from, "a@b.com") {
		t.Errorf("CustomParameters.From: expected to contain %q, got %q", "a@b.com", from)
	}
}

// Test 3 — sendStop marshals correct JSON schema (WSB-04).
// sequenceNumber is intentionally empty ("") in Phase 6 — see sendStop comment in ws.go.
func TestSendStop_JSONSchema(t *testing.T) {
	client, server := newPipe(t)
	defer client.Close()
	defer server.Close()

	streamSid := "MZabc"
	callID := "test-call-id"

	errCh := make(chan error, 1)
	go func() {
		errCh <- sendStop(client, streamSid, callID)
	}()

	data, _, err := wsutil.ReadClientData(server)
	if err != nil {
		t.Fatalf("ReadClientData: unexpected error: %v", err)
	}
	if writeErr := <-errCh; writeErr != nil {
		t.Fatalf("sendStop: unexpected error: %v", writeErr)
	}

	var got StopEvent
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("JSON unmarshal failed: %v\nRaw: %s", err, string(data))
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
// The server end writes a ConnectedEvent as a server text frame (wsutil.WriteServerText).
// The client end reads it via readWSMessage (wraps wsutil.ReadServerData).
// This verifies the JSON layer and the readWSMessage wrapper exercise the same path used in production.
func TestWriteJSON_RoundTrip(t *testing.T) {
	server, client := newPipe(t)
	defer server.Close()
	defer client.Close()

	original := ConnectedEvent{
		Event:    "connected",
		Protocol: "Call",
		Version:  "1.0.0",
	}

	errCh := make(chan error, 1)
	go func() {
		data, err := json.Marshal(original)
		if err != nil {
			errCh <- err
			return
		}
		errCh <- wsutil.WriteServerText(server, data)
	}()

	// readWSMessage wraps wsutil.ReadServerData — reads a server-sent frame.
	data, _, err := readWSMessage(client)
	if err != nil {
		t.Fatalf("readWSMessage: unexpected error: %v", err)
	}
	if writeErr := <-errCh; writeErr != nil {
		t.Fatalf("WriteServerText: unexpected error: %v", writeErr)
	}

	var roundTripped ConnectedEvent
	if err := json.Unmarshal(data, &roundTripped); err != nil {
		t.Fatalf("json.Unmarshal: unexpected error: %v\nRaw: %s", err, string(data))
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
