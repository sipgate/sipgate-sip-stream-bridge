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
