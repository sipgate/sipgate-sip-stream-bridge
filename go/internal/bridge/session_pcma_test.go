package bridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"github.com/pion/rtp/v2"
	"github.com/rs/zerolog"

	sip "github.com/sipgate/sipgate-sip-stream-bridge/internal/sip"
)

// --- AUDIO_MODE=twilio PCMA fallback: A-law <-> μ-law transcoding ---
//
// When the caller does not offer PCMU, BuildSDPAnswer negotiates PCMA while
// the WebSocket keeps the Twilio contract of audio/x-mulaw. These tests drive
// the real wsToRTP and wsPacer goroutines over a net.Pipe and assert that the
// caller receives A-law and the WebSocket consumer receives μ-law. PCMU legs
// must pass through byte-identical.

func newPCMATestSession(leg *Leg) *CallSession {
	return &CallSession{
		callID:    "callid-pcma",
		streamSid: "MZpcma",
		legs:      []*Leg{leg},
		log:       zerolog.Nop(),
	}
}

// muLawTestFrame returns one 20 ms PCMU frame carrying a non-silent signal.
func muLawTestFrame() []byte {
	b := make([]byte, 160)
	for i := range b {
		b[i] = byte(0x10 + i%0x60)
	}
	return b
}

// sendMediaEvent writes one Twilio "media" event carrying payload to the
// server side of a net.Pipe WebSocket.
func sendMediaEvent(t *testing.T, peer net.Conn, payload []byte) {
	t.Helper()
	msg, err := json.Marshal(map[string]any{
		"event":     "media",
		"streamSid": "MZpcma",
		"media":     map[string]string{"payload": base64.StdEncoding.EncodeToString(payload)},
	})
	if err != nil {
		t.Fatalf("marshal media event: %v", err)
	}
	go func() { _ = wsutil.WriteServerMessage(peer, ws.OpText, msg) }()
}

// runWsToRTP starts s.wsToRTP on one half of a net.Pipe and returns the other
// half for the test to write WebSocket frames into.
func runWsToRTP(t *testing.T, s *CallSession) net.Conn {
	t.Helper()
	wsConn, peer := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = wsConn.Close()
		_ = peer.Close()
	})
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go s.wsToRTP(ctx, wsConn, wg, newWsSignal())
	return peer
}

func TestWsToRTP_PCMALeg_TranscodesToALaw(t *testing.T) {
	leg := &Leg{audioPT: 8, alawTranscode: true, packetQueue: make(chan outboundFrame, 8)}
	peer := runWsToRTP(t, newPCMATestSession(leg))

	in := muLawTestFrame()
	sendMediaEvent(t, peer, in)

	select {
	case f := <-leg.packetQueue:
		if want := sip.ULawToALaw(in); !bytes.Equal(f.audio, want) {
			t.Fatalf("RTP frame is not the A-law of the WebSocket μ-law payload:\n got  %x\n want %x", f.audio, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no RTP frame queued")
	}
}

func TestWsToRTP_PCMULeg_Untouched(t *testing.T) {
	leg := &Leg{audioPT: 0, packetQueue: make(chan outboundFrame, 8)}
	peer := runWsToRTP(t, newPCMATestSession(leg))

	in := muLawTestFrame()
	sendMediaEvent(t, peer, in)

	select {
	case f := <-leg.packetQueue:
		if !bytes.Equal(f.audio, in) {
			t.Fatalf("PCMU frame was altered:\n got  %x\n want %x", f.audio, in)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no RTP frame queued")
	}
}

func TestWsPacer_PCMALeg_TranscodesToMuLaw(t *testing.T) {
	leg := &Leg{audioPT: 8, alawTranscode: true, rtpInboundQueue: make(chan []byte, 8)}
	s := newPCMATestSession(leg)
	wsConn, peer := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = wsConn.Close()
		_ = peer.Close()
	})

	alaw := sip.ULawToALaw(muLawTestFrame())
	leg.rtpInboundQueue <- alaw
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go s.wsPacer(ctx, wsConn, wg, newWsSignal())

	_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	data, err := wsutil.ReadClientText(peer)
	if err != nil {
		t.Fatalf("no media event on the WebSocket: %v", err)
	}
	var ev struct {
		Event string `json:"event"`
		Media struct {
			Payload string `json:"payload"`
		} `json:"media"`
	}
	if err := json.Unmarshal(data, &ev); err != nil || ev.Event != "media" {
		t.Fatalf("unexpected WebSocket message: %s", data)
	}
	got, err := base64.StdEncoding.DecodeString(ev.Media.Payload)
	if err != nil {
		t.Fatalf("media payload is not base64: %v", err)
	}
	if want := sip.ALawToULaw(alaw); !bytes.Equal(got, want) {
		t.Fatalf("WebSocket payload is not the μ-law of the RTP A-law frame:\n got  %x\n want %x", got, want)
	}
}

// mediaFormatFor keeps the WebSocket on audio/x-mulaw in twilio mode even when
// the SIP leg negotiated PCMA, and flags the leg for transcoding. best mode
// passes the negotiated codec through unchanged.
func TestMediaFormatFor_TwilioPCMAKeepsMuLaw(t *testing.T) {
	cases := []struct {
		mode          string
		pt            uint8
		wantEncoding  string
		wantRate      int
		wantTranscode bool
	}{
		{"twilio", 0, "audio/x-mulaw", 8000, false},
		{"twilio", 8, "audio/x-mulaw", 8000, true},
		{"best", 8, "audio/x-alaw", 8000, false},
		{"best", 9, "audio/G722", 16000, false},
		{"best", 0, "audio/x-mulaw", 8000, false},
	}
	for _, c := range cases {
		enc, rate, transcode := mediaFormatFor(c.mode, c.pt)
		if enc != c.wantEncoding || rate != c.wantRate || transcode != c.wantTranscode {
			t.Errorf("mediaFormatFor(%q, %d) = %s/%d transcode=%v, want %s/%d transcode=%v",
				c.mode, c.pt, enc, rate, transcode, c.wantEncoding, c.wantRate, c.wantTranscode)
		}
	}
}

// --- <Dial> relay with a PCMA caller leg ---
//
// The callee leg is PCMU-only (BuildSDPOffer / AcceptSDPAnswer). When the
// caller leg negotiated PCMA in twilio mode, the relay must convert in both
// directions, or the two parties hear each other as noise.

// sendTestRTP sends one RTP packet with the given payload type and payload to
// addr from a throwaway UDP socket.
func sendTestRTP(t *testing.T, addr net.Addr, pt uint8, payload []byte) {
	t.Helper()
	pkt := rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: pt, SequenceNumber: 1, Timestamp: 160, SSRC: 0x1234},
		Payload: payload,
	}
	raw, err := pkt.Marshal()
	if err != nil {
		t.Fatalf("marshal RTP: %v", err)
	}
	conn, err := net.DialUDP("udp4", nil, addr.(*net.UDPAddr))
	if err != nil {
		t.Fatalf("dial UDP: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write(raw); err != nil {
		t.Fatalf("send RTP: %v", err)
	}
}

// listenTestRTP opens a loopback UDP socket for an RTP reader under test and
// returns it with a stop function that cancels ctx and closes the socket.
func listenTestRTP(t *testing.T) (*net.UDPConn, context.Context, func()) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen UDP: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return conn, ctx, func() {
		cancel()
		_ = conn.Close()
	}
}

func TestRTPRelay_PCMACallerLeg_RelaysMuLawToCallee(t *testing.T) {
	caller := &Leg{audioPT: 8, dtmfPT: 101, alawTranscode: true, rtpInboundQueue: make(chan []byte, 8)}
	callee := &Leg{audioPT: 0, outboundQueue: make(chan outboundFrame, 8)}
	s := &CallSession{callID: "callid-pcma-relay", legs: []*Leg{caller, callee}, log: zerolog.Nop()}
	s.state.Store(StateForwarding)

	conn, ctx, stop := listenTestRTP(t)
	s.wg.Add(1)
	go s.rtpReader(ctx, conn, nil)
	t.Cleanup(func() { stop(); s.wg.Wait() })

	alaw := sip.ULawToALaw(muLawTestFrame())
	sendTestRTP(t, conn.LocalAddr(), 8, alaw)

	select {
	case f := <-callee.outboundQueue:
		if want := sip.ALawToULaw(alaw); !bytes.Equal(f.audio, want) {
			t.Fatalf("relayed frame is not the μ-law of the caller's A-law:\n got  %x\n want %x", f.audio, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no frame relayed to the callee leg")
	}
}

func TestCalleeRTPReader_PCMACallerLeg_RelaysALawToCaller(t *testing.T) {
	caller := &Leg{audioPT: 8, alawTranscode: true, packetQueue: make(chan outboundFrame, 8)}
	callee := &Leg{audioPT: 0}
	s := &CallSession{callID: "callid-pcma-relay", legs: []*Leg{caller, callee}, log: zerolog.Nop()}
	s.state.Store(StateForwarding)

	conn, ctx, stop := listenTestRTP(t)
	s.wg.Add(1)
	go s.calleeRtpReader(ctx, callee, conn)
	t.Cleanup(func() { stop(); s.wg.Wait() })

	ulaw := muLawTestFrame()
	sendTestRTP(t, conn.LocalAddr(), 0, ulaw)

	select {
	case f := <-caller.packetQueue:
		if want := sip.ULawToALaw(ulaw); !bytes.Equal(f.audio, want) {
			t.Fatalf("relayed frame is not the A-law of the callee's μ-law:\n got  %x\n want %x", f.audio, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no frame relayed to the caller leg")
	}
}

// A PCMU caller leg is relayed unchanged (the pre-existing behaviour).
func TestCalleeRTPReader_PCMUCallerLeg_Untouched(t *testing.T) {
	caller := &Leg{audioPT: 0, packetQueue: make(chan outboundFrame, 8)}
	callee := &Leg{audioPT: 0}
	s := &CallSession{callID: "callid-pcmu-relay", legs: []*Leg{caller, callee}, log: zerolog.Nop()}
	s.state.Store(StateForwarding)

	conn, ctx, stop := listenTestRTP(t)
	s.wg.Add(1)
	go s.calleeRtpReader(ctx, callee, conn)
	t.Cleanup(func() { stop(); s.wg.Wait() })

	ulaw := muLawTestFrame()
	sendTestRTP(t, conn.LocalAddr(), 0, ulaw)

	select {
	case f := <-caller.packetQueue:
		if !bytes.Equal(f.audio, ulaw) {
			t.Fatalf("PCMU frame was altered:\n got  %x\n want %x", f.audio, ulaw)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no frame relayed to the caller leg")
	}
}
