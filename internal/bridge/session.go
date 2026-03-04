package bridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/gobwas/ws"
	"github.com/pion/rtp"
	"github.com/rs/zerolog"
	"github.com/sipgate/audio-dock/internal/config"
)

// packetQueueSize is the maximum number of 20 ms PCMU frames buffered for the RTP pacer.
// 500 frames = 10 seconds; large enough for any realistic TTS response blob.
// When full, new frames are dropped and a warning is logged.
const packetQueueSize = 500

// pcmuSilenceFrame is a single 20 ms PCMU silence frame (160 bytes of μ-law zero = 0xFF).
// rtpPacer sends this when no audio is queued so the RTP stream is continuous.
// Continuous RTP is required for NAT traversal: the first outbound UDP packet punches the
// NAT hole so the caller's media server can reach our private address.
var pcmuSilenceFrame = func() []byte {
	b := make([]byte, 160)
	for i := range b {
		b[i] = 0xFF // 0xFF = μ-law encoding of linear PCM zero (silence)
	}
	return b
}()

// CallSession holds all per-call state: RTP socket, WS connection, goroutine lifecycle.
// Ownership: StartSession creates one instance; run() owns it for the call lifetime.
type CallSession struct {
	callID      string
	streamSid   string
	dlg         *sipgo.DialogServerSession
	rtpPort     int
	callerRTP   net.Addr      // *net.UDPAddr — caller's IP:port from SDP offer
	dtmfPT      uint8         // telephone-event PT from SDP offer (sipgate: 113)
	cfg         config.Config
	log         zerolog.Logger
	cancel      context.CancelFunc
	wg          sync.WaitGroup // tracks rtpToWS + wsToRTP + rtpPacer goroutines
	packetQueue chan []byte     // buffered PCMU frames (160 bytes each) for rtpPacer
}

// run is the full call session lifecycle. Called from StartSession (which runs in a goroutine).
// Blocks until the call ends. All cleanup happens via defers.
//
// WRITE-SAFETY INVARIANTS:
//   - rtpToWS is the ONLY goroutine that writes to wsConn during audio forwarding.
//   - sendStop is written to wsConn ONLY after wg.Wait() confirms all goroutines have exited.
//   - rtpPacer is the ONLY goroutine that writes to rtpConn (UDP → caller).
func (s *CallSession) run(ctx context.Context) {
	// Pitfall 6: if BYE arrived before session started, context is already done — exit immediately.
	if ctx.Err() != nil {
		s.log.Warn().Str("call_id", s.callID).Msg("session context already cancelled at entry — BYE arrived early")
		return
	}

	// Derive session context from dialog context so BYE cancels the session.
	sessionCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	defer cancel()

	// Open RTP socket on the acquired port (CON-02: defer Close ensures FD cleanup on all paths).
	rtpConn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: s.rtpPort})
	if err != nil {
		s.log.Error().Err(err).Int("rtp_port", s.rtpPort).Str("call_id", s.callID).Msg("ListenUDP failed — sending BYE")
		_ = s.dlg.Bye(context.Background())
		return
	}
	defer rtpConn.Close() // CON-02: FD cleanup

	// Dial WebSocket to the media stream target.
	wsConn, err := dialWS(sessionCtx, s.cfg.WSTargetURL)
	if err != nil {
		s.log.Error().Err(err).Str("call_id", s.callID).Msg("dialWS failed — sending BYE (WSR-01)")
		_ = s.dlg.Bye(context.Background())
		return
	}
	defer wsConn.Close()

	// sendConnected: Twilio Media Streams handshake step 1.
	if err := sendConnected(wsConn); err != nil {
		s.log.Error().Err(err).Str("call_id", s.callID).Msg("sendConnected failed")
		return
	}

	// sendStart: Twilio Media Streams handshake step 2.
	if err := sendStart(wsConn, s.streamSid, s.callID, s.dlg.InviteRequest); err != nil {
		s.log.Error().Err(err).Str("call_id", s.callID).Msg("sendStart failed")
		return
	}

	// packetQueue carries decoded PCMU frames (160 bytes each) from wsToRTP to rtpPacer.
	// wsToRTP decodes and chunks WS audio blobs; rtpPacer sends them at the RTP ptime rate.
	s.packetQueue = make(chan []byte, packetQueueSize)

	// Launch goroutines:
	//   rtpToWS  — reads inbound RTP from caller, writes media events to WS (sole WS writer)
	//   wsToRTP  — reads media events from WS, queues PCMU frames (sole WS reader)
	//   rtpPacer — drains packetQueue at 20 ms intervals, writes RTP to caller (sole UDP writer)
	s.wg.Add(3)
	go s.rtpToWS(sessionCtx, rtpConn, wsConn)
	go s.wsToRTP(sessionCtx, wsConn)
	go s.rtpPacer(sessionCtx, rtpConn)

	// Block until the dialog context is cancelled (BYE from either side).
	<-sessionCtx.Done()

	// Expire the WS read deadline so wsToRTP (which may be blocked in readWSMessage)
	// wakes up and can check ctx.Err(). Without this, wsToRTP blocks until a TCP idle
	// timeout fires (30+ s), producing a spurious "WS read error — sending BYE" log.
	// Using SetReadDeadline (not Close) preserves the write path so sendStop can be sent.
	_ = wsConn.SetReadDeadline(time.Now())

	// Drain all three goroutines before sending stop.
	// rtpToWS exits within 100 ms (UDP read deadline → ctx check).
	// wsToRTP exits promptly (WS read deadline just expired → ctx check).
	// rtpPacer exits on its next ticker tick via ctx.Done().
	s.wg.Wait()

	// sendStop: Twilio Media Streams teardown — safe to write now (all goroutines done).
	if err := sendStop(wsConn, s.streamSid, s.callID); err != nil {
		s.log.Warn().Err(err).Str("call_id", s.callID).Msg("sendStop failed (best effort)")
	}
}

// rtpToWS reads RTP packets from the UDP socket and forwards PCMU audio to the WebSocket.
// PCMU (PT 0) packets are base64-encoded and sent as Twilio "media" events.
// DTMF packets (PT == s.dtmfPT) are dropped — handled in Phase 7.
// Non-PCMU/non-DTMF packets are also dropped.
// rtpToWS is the SOLE WS writer during audio forwarding — no other goroutine may write to wsConn.
func (s *CallSession) rtpToWS(ctx context.Context, rtpConn *net.UDPConn, wsConn net.Conn) {
	defer s.wg.Done()

	buf := make([]byte, 1500) // MTU-safe read buffer
	var seqNo uint32 = 2      // sequence starts at 2 (connected=implicit seq 0, start=seq 1)
	startTimeMs := time.Now().UnixMilli()

	for {
		// Check for cancellation before blocking on read.
		if ctx.Err() != nil {
			return
		}

		// 100ms read deadline — allows ctx cancellation to be detected promptly.
		_ = rtpConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))

		n, _, err := rtpConn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue // deadline expired — re-check ctx, then re-read
			}
			s.log.Error().Err(err).Str("call_id", s.callID).Msg("rtpToWS: ReadFromUDP error")
			return
		}

		// Parse RTP packet header to check payload type.
		var pkt rtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			s.log.Warn().Err(err).Str("call_id", s.callID).Msg("rtpToWS: RTP unmarshal failed — skipping packet")
			continue
		}

		// Drop DTMF packets — handled in Phase 7.
		if pkt.PayloadType == s.dtmfPT {
			continue
		}

		// Drop non-PCMU packets.
		if pkt.PayloadType != 0 {
			continue
		}

		// Base64-encode PCMU payload for Twilio Media Streams "media" event.
		payload := base64.StdEncoding.EncodeToString(pkt.Payload)
		timestamp := time.Now().UnixMilli() - startTimeMs

		// Use MediaEvent struct to ensure correct JSON schema (string fields, not integers).
		// Twilio Media Streams spec requires sequenceNumber, chunk, and timestamp as strings.
		event := MediaEvent{
			Event:          "media",
			SequenceNumber: fmt.Sprintf("%d", seqNo),
			StreamSid:      s.streamSid,
			Media: MediaBody{
				Track:     "inbound",
				Chunk:     fmt.Sprintf("%d", seqNo),
				Timestamp: fmt.Sprintf("%d", timestamp),
				Payload:   payload,
			},
		}

		if err := writeJSON(wsConn, event); err != nil {
			s.log.Error().Err(err).Str("call_id", s.callID).Msg("rtpToWS: writeJSON failed")
			return
		}
		seqNo++
	}
}

// wsToRTP reads Twilio Media Streams events from the WebSocket, decodes PCMU audio, and
// queues 160-byte frames into packetQueue for rtpPacer to transmit at the correct RTP rate.
//
// Separating decode/queue (wsToRTP) from pacing/send (rtpPacer) means:
//   - WS messages of any size (up to ~1 MB per Twilio spec) are handled correctly.
//   - Large blobs are chunked into 160-byte frames immediately on receipt.
//   - rtpPacer drains at exactly 20 ms/frame, matching PCMU ptime — no jitter buffer overflow.
//   - New WS messages (including "stop") are processed promptly even while frames are queued.
//
// "stop" events trigger dlg.Bye + cancel session (SIP-05).
// wsToRTP is the SOLE WS reader — only this goroutine reads from wsConn.
func (s *CallSession) wsToRTP(ctx context.Context, wsConn net.Conn) {
	defer s.wg.Done()

	for {
		if ctx.Err() != nil {
			return
		}

		msgData, op, err := readWSMessage(wsConn)
		if err != nil {
			if ctx.Err() != nil {
				return // session ended normally — WS read deadline expired or conn closed
			}
			s.log.Error().Err(err).Str("call_id", s.callID).Msg("wsToRTP: WS read error — sending BYE")
			_ = s.dlg.Bye(context.Background())
			s.cancel()
			return
		}

		// Only process text frames (Twilio Media Streams sends JSON text frames).
		if op != ws.OpText {
			continue
		}

		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(msgData, &envelope); err != nil {
			s.log.Warn().Err(err).Str("call_id", s.callID).Msg("wsToRTP: JSON unmarshal failed — skipping")
			continue
		}

		var eventType string
		if raw, ok := envelope["event"]; ok {
			_ = json.Unmarshal(raw, &eventType)
		}

		switch eventType {
		case "media":
			var mediaObj struct {
				Payload string `json:"payload"`
			}
			if raw, ok := envelope["media"]; ok {
				if err := json.Unmarshal(raw, &mediaObj); err != nil {
					s.log.Warn().Err(err).Str("call_id", s.callID).Msg("wsToRTP: media decode failed — skipping")
					continue
				}
			}

			pcmuPayload, err := base64.StdEncoding.DecodeString(mediaObj.Payload)
			if err != nil {
				s.log.Warn().Err(err).Str("call_id", s.callID).Msg("wsToRTP: base64 decode failed — skipping")
				continue
			}

			// Chunk into 160-byte (20 ms @ 8 kHz PCMU) frames and enqueue for rtpPacer.
			// WS servers may send arbitrarily large blobs (Twilio allows up to ~1 MB).
			// rtpPacer sends one frame every 20 ms, matching the RTP ptime.
			const rtpFrameSize = 160
			for len(pcmuPayload) > 0 {
				chunk := pcmuPayload
				if len(chunk) > rtpFrameSize {
					chunk = pcmuPayload[:rtpFrameSize]
				}
				pcmuPayload = pcmuPayload[len(chunk):]

				select {
				case s.packetQueue <- chunk:
				case <-ctx.Done():
					return
				default:
					// Queue full — drop frame rather than block WS reading.
					s.log.Warn().Str("call_id", s.callID).Msg("wsToRTP: packet queue full — dropping PCMU frame")
				}
			}

		case "stop":
			s.log.Info().Str("call_id", s.callID).Msg("wsToRTP: received stop event — sending BYE (SIP-05)")
			_ = s.dlg.Bye(context.Background())
			s.cancel()
			return

		default:
			// Unknown event types (e.g. "connected", "start" echo) — ignore.
			s.log.Debug().Str("event", eventType).Str("call_id", s.callID).Msg("wsToRTP: unknown WS event — skipping")
		}
	}
}

// rtpPacer drains packetQueue at the RTP ptime rate (one 160-byte frame every 20 ms)
// and sends each frame as a PCMU RTP packet to the caller.
//
// Pacing at 20 ms/frame ensures the phone's jitter buffer (typically 40–200 ms) is not
// flooded. Without pacing, a single large WS blob would arrive as a burst of hundreds
// of back-to-back UDP datagrams, causing most to be dropped by the jitter buffer.
//
// rtpPacer is the SOLE UDP writer during a session — no other goroutine writes to rtpConn.
func (s *CallSession) rtpPacer(ctx context.Context, rtpConn *net.UDPConn) {
	defer s.wg.Done()

	ssrc := rand.Uint32()
	var seqNo uint16
	var timestamp uint32

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Dequeue next audio frame, or fall back to silence.
			// Sending on every tick (even silence) is required for NAT traversal:
			// the first outbound UDP packet establishes the NAT mapping so the caller's
			// media server can reach our private address. It also keeps the mapping
			// alive for the duration of the call.
			var chunk []byte
			select {
			case chunk = <-s.packetQueue:
			default:
				chunk = pcmuSilenceFrame
			}

			pkt := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    0, // PCMU
					SequenceNumber: seqNo,
					Timestamp:      timestamp,
					SSRC:           ssrc,
				},
				Payload: chunk,
			}
			encoded, err := pkt.Marshal()
			if err != nil {
				s.log.Warn().Err(err).Str("call_id", s.callID).Msg("rtpPacer: RTP marshal failed — skipping frame")
				continue
			}
			if _, err := rtpConn.WriteTo(encoded, s.callerRTP); err != nil {
				s.log.Error().Err(err).Str("call_id", s.callID).Msg("rtpPacer: WriteTo caller failed")
				s.cancel()
				return
			}
			seqNo++
			timestamp += 160 // 20 ms @ 8 kHz PCMU
		}
	}
}
