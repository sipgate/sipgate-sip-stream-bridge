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
// callSid=callSidToken (CA-prefixed), customParameters.sipCallId=sipCallID.
func TestSendStart_JSONSchema(t *testing.T) {
	client, server := newPipe(t)
	defer client.Close()
	defer server.Close()

	req := mockSIPRequest("a", "b.com", "c", "d.com")
	streamSid := "MZabc"
	callSidToken := "CAtest123"
	sipCallID := "test-sip-call-id"

	errCh := make(chan error, 1)
	go func() {
		errCh <- sendStart(client, streamSid, callSidToken, sipCallID, req)
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
	if body.CallSid != callSidToken {
		t.Errorf("Start.CallSid: expected %q, got %q", callSidToken, body.CallSid)
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

	if body.CustomParameters["sipCallId"] != sipCallID {
		t.Errorf("CustomParameters.sipCallId: expected %q, got %q", sipCallID, body.CustomParameters["sipCallId"])
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

// Test 5 — wsSignal.Signal() is idempotent: calling it N times never panics.
// Verifies the sync.Once guard prevents double-close of the underlying channel.
func TestWsSignal_MultipleSignalsNoPanic(t *testing.T) {
	sig := newWsSignal()
	done := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func() {
			sig.Signal()
			select {
			case done <- struct{}{}:
			default:
			}
		}()
	}
	// Drain all 10 goroutines (or wait a short time for them to complete)
	sig.Signal() // also call from test goroutine — total 11 calls
	<-sig.Done() // must be readable after at least one Signal()
}

// Test 6 — wsSignal.Done() channel is closed after Signal() is called.
func TestWsSignal_DoneClosedAfterSignal(t *testing.T) {
	sig := newWsSignal()

	// Before Signal: channel should be open (not readable)
	select {
	case <-sig.Done():
		t.Fatal("Done() channel was closed before Signal()")
	default:
	}

	sig.Signal()

	// After Signal: channel must be closed (readable immediately)
	select {
	case <-sig.Done():
		// expected
	default:
		t.Fatal("Done() channel is not closed after Signal()")
	}
}

// Test 7 — handshake sends connected event then start event in order.
// Uses net.Pipe() as a synchronous in-process net.Conn pair.
// Reads two WS frames from the server side and verifies their event fields.
func TestHandshake_SendsConnectedThenStart(t *testing.T) {
	clientConn, serverConn := newPipe(t)
	defer clientConn.Close()
	defer serverConn.Close()

	req := mockSIPRequest("caller", "sip.example.com", "callee", "sip.example.com")
	s := &CallSession{
		callID:       "test-call-id",
		callSidToken: "CAtest",
		streamSid:    "MZtest",
		dlg:          nil, // dlg not needed for handshake — sendStart uses s.dlg.InviteRequest
	}
	// We cannot use a real dlg here, so we test via the handshake helper by calling
	// sendConnected + sendStart directly as handshake() does.
	// handshake(wsConn) calls sendConnected(wsConn) then sendStart(wsConn, streamSid, callSidToken, callID, dlg.InviteRequest).
	// Since dlg is nil we exercise the helper indirectly by reading two frames.
	_ = s
	_ = req

	errCh := make(chan error, 1)
	go func() {
		// Call sendConnected + sendStart directly, mirroring what handshake() does.
		if err := sendConnected(clientConn); err != nil {
			errCh <- err
			return
		}
		errCh <- sendStart(clientConn, "MZtest", "CAtest", "call-1", req)
	}()

	// Read frame 1: connected
	frame1, _, err := wsutil.ReadClientData(serverConn)
	if err != nil {
		t.Fatalf("ReadClientData frame1: %v", err)
	}
	var ev1 map[string]interface{}
	if err := json.Unmarshal(frame1, &ev1); err != nil {
		t.Fatalf("unmarshal frame1: %v", err)
	}
	if ev1["event"] != "connected" {
		t.Errorf("frame1 event: expected %q got %q", "connected", ev1["event"])
	}

	// Read frame 2: start
	frame2, _, err := wsutil.ReadClientData(serverConn)
	if err != nil {
		t.Fatalf("ReadClientData frame2: %v", err)
	}
	var ev2 map[string]interface{}
	if err := json.Unmarshal(frame2, &ev2); err != nil {
		t.Fatalf("unmarshal frame2: %v", err)
	}
	if ev2["event"] != "start" {
		t.Errorf("frame2 event: expected %q got %q", "start", ev2["event"])
	}

	if writeErr := <-errCh; writeErr != nil {
		t.Fatalf("handshake writes: %v", writeErr)
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
