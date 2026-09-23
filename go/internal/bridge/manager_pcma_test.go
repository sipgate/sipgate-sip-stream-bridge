package bridge

import (
	"bytes"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sipgate/sipgate-sip-stream-bridge/internal/config"
	sip "github.com/sipgate/sipgate-sip-stream-bridge/internal/sip"
)

// newCallerLeg wires the negotiated payload type into the caller leg. A PCMA
// leg in twilio mode must be flagged for transcoding and announce
// audio/x-mulaw on the WebSocket; if the flag were lost, the SIP side would
// speak A-law while the WebSocket carries μ-law untranslated, and both parties
// would hear noise. The RTP silence frame stays in the codec of the SIP leg.
func TestNewCallerLeg_MediaFormatWiring(t *testing.T) {
	callerSDP := &sip.CallerSDP{
		RTPAddr:         "192.0.2.10",
		RTPPort:         42828,
		DTMFPayloadType: 97,
	}
	cases := []struct {
		mode          string
		pt            uint8
		wantEncoding  string
		wantRate      int
		wantTranscode bool
		wantSilence   byte
	}{
		{"twilio", 8, "audio/x-mulaw", 8000, true, 0xD5},
		{"twilio", 0, "audio/x-mulaw", 8000, false, 0xFF},
		{"best", 8, "audio/x-alaw", 8000, false, 0xD5},
		{"best", 9, "audio/G722", 16000, false, 0x00},
	}
	for _, c := range cases {
		cm := NewCallManager(nil, "ACdeadbeefdeadbeefdeadbeefdeadbeef", config.Config{AudioMode: c.mode}, zerolog.Nop(), nil)
		leg := cm.newCallerLeg(nil, callerSDP, 20000, c.pt, nil, nil)
		cm.Close()

		if leg.alawTranscode != c.wantTranscode {
			t.Errorf("%s/PT %d: alawTranscode = %v, want %v", c.mode, c.pt, leg.alawTranscode, c.wantTranscode)
		}
		if leg.mediaEncoding != c.wantEncoding || leg.mediaSampleRate != c.wantRate {
			t.Errorf("%s/PT %d: mediaFormat = %s/%d, want %s/%d",
				c.mode, c.pt, leg.mediaEncoding, leg.mediaSampleRate, c.wantEncoding, c.wantRate)
		}
		if leg.audioPT != c.pt {
			t.Errorf("%s/PT %d: audioPT = %d", c.mode, c.pt, leg.audioPT)
		}
		if !bytes.Equal(leg.silenceFrame, bytes.Repeat([]byte{c.wantSilence}, 160)) {
			t.Errorf("%s/PT %d: silenceFrame is not 160 × 0x%02X", c.mode, c.pt, c.wantSilence)
		}
		if leg.dtmfPT != 97 || leg.rtpPort != 20000 {
			t.Errorf("%s/PT %d: dtmfPT = %d, rtpPort = %d, want 97, 20000", c.mode, c.pt, leg.dtmfPT, leg.rtpPort)
		}
		if leg.remoteRTP == nil || leg.remoteRTP.String() != "192.0.2.10:42828" {
			t.Errorf("%s/PT %d: remoteRTP = %v, want 192.0.2.10:42828", c.mode, c.pt, leg.remoteRTP)
		}
		if leg.packetQueue == nil || leg.rtpInboundQueue == nil {
			t.Errorf("%s/PT %d: RTP queues not allocated", c.mode, c.pt)
		}
	}
}
