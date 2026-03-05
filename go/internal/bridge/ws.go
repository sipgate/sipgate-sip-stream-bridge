package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	siplib "github.com/emiago/sipgo/sip"
)

// ConnectedEvent is the first event sent on a new Twilio Media Streams WebSocket connection (WSB-01).
// Schema: {"event":"connected","protocol":"Call","version":"1.0.0"}
type ConnectedEvent struct {
	Event    string `json:"event"`
	Protocol string `json:"protocol"`
	Version  string `json:"version"`
}

// StartEvent is sent after ConnectedEvent and provides call metadata (WSB-02 + WSB-06).
type StartEvent struct {
	Event          string         `json:"event"`
	SequenceNumber string         `json:"sequenceNumber"`
	StreamSid      string         `json:"streamSid"`
	Start          StartEventBody `json:"start"`
}

// StartEventBody holds the nested metadata within a start event.
type StartEventBody struct {
	StreamSid        string            `json:"streamSid"`
	AccountSid       string            `json:"accountSid"`
	CallSid          string            `json:"callSid"`
	Tracks           []string          `json:"tracks"`
	CustomParameters map[string]string `json:"customParameters"`
	MediaFormat      MediaFormat       `json:"mediaFormat"`
}

// MediaFormat describes the audio encoding parameters within a start event.
type MediaFormat struct {
	Encoding   string `json:"encoding"`
	SampleRate int    `json:"sampleRate"`
	Channels   int    `json:"channels"`
}

// MediaEvent carries a single audio frame (WSB-03 — audio-dock to WS server direction).
// Schema: {"event":"media","sequenceNumber":"N","streamSid":"MZxxx","media":{...}}
type MediaEvent struct {
	Event          string    `json:"event"`
	SequenceNumber string    `json:"sequenceNumber"`
	StreamSid      string    `json:"streamSid"`
	Media          MediaBody `json:"media"`
}

// MediaBody holds the audio payload within a media event.
type MediaBody struct {
	Track     string `json:"track"`
	Chunk     string `json:"chunk"`
	Timestamp string `json:"timestamp"`
	Payload   string `json:"payload"`
}

// StopEvent is sent when the call ends (WSB-04).
// SequenceNumber is intentionally empty in Phase 6 — see sendStop comment.
type StopEvent struct {
	Event          string   `json:"event"`
	SequenceNumber string   `json:"sequenceNumber"`
	StreamSid      string   `json:"streamSid"`
	Stop           StopBody `json:"stop"`
}

// StopBody holds the nested metadata within a stop event.
type StopBody struct {
	AccountSid string `json:"accountSid"`
	CallSid    string `json:"callSid"`
}

// dialWS connects to the target WebSocket URL using gobwas/ws.
// Handles both ws:// and wss:// (TLS via system roots — included in Docker image from Phase 4).
// Returns a net.Conn that is safe to use with wsutil read/write helpers.
//
// TCP_NODELAY is set to disable Nagle's algorithm. ws.WriteFrame makes two separate
// write() syscalls (header then payload). Without TCP_NODELAY, Nagle holds the payload
// write until the header's ACK arrives (up to 40–200 ms on macOS), causing periodic
// ~100 ms stalls every ~25 frames and bursts of 5 back-to-back frames at the listener.
func dialWS(ctx context.Context, url string) (net.Conn, error) {
	conn, _, _, err := ws.Dial(ctx, url)
	if err != nil {
		return nil, err
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	return conn, nil
}

// sendConnected sends the WSB-01 connected event to the WebSocket server.
func sendConnected(conn net.Conn) error {
	return writeJSON(conn, ConnectedEvent{Event: "connected", Protocol: "Call", Version: "1.0.0"})
}

// sendStart sends the WSB-02 + WSB-06 start event with full call metadata.
// callSidToken is a CA-prefixed token (Twilio callSid convention).
// callID is the SIP Call-ID, forwarded in customParameters.sipCallId for consumer use.
// req.From().Address.String() and req.To().Address.String() provide SIP URI strings.
func sendStart(conn net.Conn, streamSid, callSidToken, callID string, req *siplib.Request) error {
	customParams := map[string]string{
		"sipCallId": callID,
		"From":      req.From().Address.String(),
		"To":        req.To().Address.String(),
	}
	return writeJSON(conn, StartEvent{
		Event:          "start",
		SequenceNumber: "1",
		StreamSid:      streamSid,
		Start: StartEventBody{
			StreamSid:        streamSid,
			AccountSid:       "",
			CallSid:          callSidToken,
			Tracks:           []string{"inbound", "outbound"},
			CustomParameters: customParams,
			MediaFormat:      MediaFormat{Encoding: "audio/x-mulaw", SampleRate: 8000, Channels: 1},
		},
	})
}

// sendStop sends the WSB-04 stop event when the call ends.
//
// SequenceNumber is intentionally left empty (zero value "") in Phase 6.
// The Twilio Media Streams spec lists sequenceNumber as a field on stop events,
// but consumers identify the stream via streamSid + callSid. Tracking a global
// per-session sequence counter is deferred to Phase 7 if needed.
// Per-session seqNo lives in rtpToWS; passing it through to sendStop would require
// an out-of-band channel — unnecessary complexity for Phase 6.
func sendStop(conn net.Conn, streamSid, callSidToken string) error {
	return writeJSON(conn, StopEvent{
		Event:     "stop",
		StreamSid: streamSid,
		Stop:      StopBody{AccountSid: "", CallSid: callSidToken},
	})
}

// dtmfEventToDigit maps RFC 4733 event codes 0-15 to DTMF digit strings.
// Source: RFC 4733 Section 3.10 — https://www.rfc-editor.org/rfc/rfc4733.html
var dtmfEventToDigit = [16]string{
	"0", "1", "2", "3", "4", "5", "6", "7", "8", "9",
	"*", "#", "A", "B", "C", "D",
}

// parseTelephoneEvent parses the 4-byte RFC 4733 telephone-event RTP payload.
//
// Wire format (RFC 4733 §2.3):
//
//	Byte 0:   event code (0-15 for standard DTMF digits)
//	Byte 1:   E|R|volume — E bit = MSB (0x80); 1 means this is the End packet
//	Bytes 2-3: duration (big-endian uint16; not used for forwarding)
//
// Returns (digit, isEnd, ok). ok=false if payload < 4 bytes or event code > 15.
func parseTelephoneEvent(payload []byte) (digit string, isEnd bool, ok bool) {
	if len(payload) < 4 {
		return "", false, false
	}
	eventCode := payload[0]
	if eventCode > 15 {
		return "", false, false
	}
	isEnd = (payload[1] & 0x80) != 0
	return dtmfEventToDigit[eventCode], isEnd, true
}

// DtmfEvent is the Twilio Media Streams `dtmf` event (WSB-07).
// Schema verified from https://www.twilio.com/docs/voice/media-streams/websocket-messages
type DtmfEvent struct {
	Event          string   `json:"event"`
	StreamSid      string   `json:"streamSid"`
	SequenceNumber string   `json:"sequenceNumber"`
	Dtmf           DtmfBody `json:"dtmf"`
}

// DtmfBody holds the nested DTMF payload.
type DtmfBody struct {
	Track string `json:"track"` // always "inbound_track" per Twilio spec
	Digit string `json:"digit"` // "0"-"9", "*", "#", "A"-"D"
}

// sendDTMF writes a `dtmf` event to the WebSocket connection.
// Called only from wsPacer (sole WS writer invariant preserved).
func sendDTMF(conn net.Conn, streamSid, digit string, seqNo uint32) error {
	return writeJSON(conn, DtmfEvent{
		Event:          "dtmf",
		StreamSid:      streamSid,
		SequenceNumber: fmt.Sprintf("%d", seqNo),
		Dtmf: DtmfBody{
			Track: "inbound_track",
			Digit: digit,
		},
	})
}

// MarkEvent is the Twilio Media Streams `mark` event (WS server → audio-dock outbound echo).
// Schema: {"event":"mark","sequenceNumber":"N","streamSid":"MZxxx","mark":{"name":"label"}}
// Sent by wsPacer only (sole-writer invariant). Called from markEchoQueue consumer in wsPacer.
// Reference: https://www.twilio.com/docs/voice/media-streams/websocket-messages#mark
type MarkEvent struct {
	Event          string   `json:"event"`
	SequenceNumber string   `json:"sequenceNumber"`
	StreamSid      string   `json:"streamSid"`
	Mark           MarkBody `json:"mark"`
}

// MarkBody holds the nested mark payload.
type MarkBody struct {
	Name string `json:"name"`
}

// sendMarkEcho writes a `mark` echo event to the WebSocket connection.
// Called only from wsPacer (sole WS writer invariant preserved).
// seqNo is the wsPacer-local sequence counter, incremented by the caller after each send.
func sendMarkEcho(conn net.Conn, streamSid, markName string, seqNo uint32) error {
	return writeJSON(conn, MarkEvent{
		Event:          "mark",
		SequenceNumber: fmt.Sprintf("%d", seqNo),
		StreamSid:      streamSid,
		Mark:           MarkBody{Name: markName},
	})
}

// wsSignal is a single-fire channel signal for WS goroutine failure notification.
// Both wsPacer and wsToRTP call Signal() on error; sync.Once ensures the channel
// is closed exactly once regardless of which goroutine fires first (prevents panic
// on double-close — Research Pitfall 1).
type wsSignal struct {
	once sync.Once
	ch   chan struct{}
}

func newWsSignal() *wsSignal {
	return &wsSignal{ch: make(chan struct{})}
}

// Signal closes the done channel exactly once. Safe to call from multiple goroutines.
func (w *wsSignal) Signal() {
	w.once.Do(func() { close(w.ch) })
}

// Done returns the channel that is closed when Signal() is first called.
func (w *wsSignal) Done() <-chan struct{} {
	return w.ch
}

// writeJSON marshals v to JSON and writes it as a masked WebSocket text frame.
// wsutil.WriteClientText handles RFC 6455 client-side masking — never use raw conn.Write.
// CRITICAL: Do not call from multiple goroutines on the same conn — not concurrent-write-safe.
func writeJSON(conn net.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return wsutil.WriteClientText(conn, data)
}

// readWSMessage reads one text frame from the WebSocket server.
// Returns the raw frame payload bytes, the WebSocket opcode, and any error.
// Used in wsToRTP goroutine in session.go — call readWSMessage(wsConn) there,
// not wsutil.ReadServerData directly. This keeps the production code path consistent
// with the tested wrapper.
func readWSMessage(conn net.Conn) ([]byte, ws.OpCode, error) {
	return wsutil.ReadServerData(conn)
}
