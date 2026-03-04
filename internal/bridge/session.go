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
	"github.com/sipgate/audio-dock/internal/observability"
)

// packetQueueSize is the maximum number of 20 ms PCMU frames buffered for the RTP pacer.
// 500 frames = 10 seconds; large enough for any realistic TTS response blob.
// When full, new frames are dropped and a warning is logged.
const packetQueueSize = 500

// rtpInboundQueueSize is the maximum number of 20 ms PCMU frames buffered between
// rtpReader and wsPacer. 50 frames = 1 second — enough to absorb the periodic
// ~400 ms sender-side RTP batching observed from sipgate without dropping packets.
// When full, new frames are dropped and a warning is logged.
const rtpInboundQueueSize = 50

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
	callID       string
	callSidToken string   // CA-prefixed Twilio callSid token (distinct from SIP Call-ID)
	streamSid    string
	dlg          *sipgo.DialogServerSession
	rtpPort      int
	callerRTP    net.Addr // *net.UDPAddr — caller's IP:port from SDP offer
	dtmfPT       uint8    // telephone-event PT from SDP offer (sipgate: 113)
	cfg          config.Config
	log          zerolog.Logger
	cancel          context.CancelFunc
	wg              sync.WaitGroup // tracks ONLY rtpReader + rtpPacer (persistent RTP goroutines)
	packetQueue     chan []byte     // buffered PCMU frames (160 bytes each) for rtpPacer (WS→RTP)
	rtpInboundQueue chan []byte     // buffered PCMU frames (160 bytes each) for wsPacer (RTP→WS)
	lastDtmfTS      uint32         // RTP timestamp of last forwarded DTMF End packet (RFC 4733 dedup)
	dtmfQueue       chan string     // digit strings ("0"-"9","*","#","A"-"D") from rtpReader to wsPacer
	metrics         *observability.Metrics
}

// run is the full call session lifecycle. Called from StartSession (which runs in a goroutine).
// Blocks until the call ends. All cleanup happens via defers.
//
// WRITE-SAFETY INVARIANTS:
//   - wsPacer is the ONLY goroutine that writes to wsConn during audio forwarding.
//   - sendStop is written to wsConn ONLY after all WS goroutines have exited.
//   - rtpPacer is the ONLY goroutine that writes to rtpConn (UDP → caller).
//
// RECONNECT MODEL:
//   - rtpReader and rtpPacer run for the full call lifetime (tracked by s.wg).
//   - wsPacer and wsToRTP are stopped and restarted around each new wsConn (tracked by local wsWg).
//   - A wsSignal fires when either WS goroutine detects a write/read error.
//   - run() reconnects using exponential backoff (1s/2s/4s, 30s budget).
//   - After successful reconnect, connected+start are re-sent before WS goroutines restart.
func (s *CallSession) run(ctx context.Context, initialWsConn net.Conn) {
	// Pitfall 6: if BYE arrived before session started, context is already done — exit immediately.
	if ctx.Err() != nil {
		s.log.Warn().Str("call_id", s.callID).Msg("session context already cancelled at entry — BYE arrived early")
		_ = initialWsConn.Close()
		return
	}

	// Derive session context from dialog context so BYE cancels the session.
	sessionCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	defer cancel()

	// Open RTP socket on the acquired port.
	rtpConn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: s.rtpPort})
	if err != nil {
		s.log.Error().Err(err).Int("rtp_port", s.rtpPort).Str("call_id", s.callID).Msg("ListenUDP failed — sending BYE")
		_ = initialWsConn.Close()
		_ = s.dlg.Bye(context.Background())
		return
	}
	defer rtpConn.Close()

	// Initialize queues before launching goroutines (Pitfall 6 for DTMF in Phase 7-02).
	s.packetQueue = make(chan []byte, packetQueueSize)
	s.rtpInboundQueue = make(chan []byte, rtpInboundQueueSize)
	s.dtmfQueue = make(chan string, 10) // 10 slots; more than enough for any realistic keypress burst

	// Send initial handshake on the pre-dialed connection from StartSession.
	wsConn := initialWsConn
	if err := s.handshake(wsConn); err != nil {
		s.log.Error().Err(err).Str("call_id", s.callID).Msg("initial handshake failed — sending BYE")
		_ = wsConn.Close()
		_ = s.dlg.Bye(context.Background())
		return
	}

	// RTP goroutines: persistent for the full call lifetime, tracked by s.wg.
	// rtpReader enqueues inbound PCMU to rtpInboundQueue; rtpPacer drains packetQueue to caller.
	s.wg.Add(2)
	go s.rtpReader(sessionCtx, rtpConn)
	go s.rtpPacer(sessionCtx, rtpConn)

	// WS reconnect loop: stops/restarts only wsPacer + wsToRTP on each new connection.
	// A fresh sig + wsWg is created for each connection iteration.
	for {
		sig := newWsSignal()
		wsWg := &sync.WaitGroup{}
		wsWg.Add(2)
		go s.wsPacer(sessionCtx, wsConn, wsWg, sig)
		go s.wsToRTP(sessionCtx, wsConn, wsWg, sig)

		select {
		case <-sessionCtx.Done():
			// Normal call end (BYE from either side).
			// Shutdown sequence:
			//   1. SetReadDeadline → unblocks wsToRTP's blocked readWSMessage call
			//   2. wsWg.Wait()    → drains wsToRTP + wsPacer (wsPacer sees ctx.Done or write error)
			//   3. rtpConn.Close() → unblocks rtpReader's blocking ReadFromUDP
			//   4. s.wg.Wait()   → drains rtpReader + rtpPacer
			//   5. sendStop      → sole writer now; best-effort teardown message
			//   6. wsConn.Close() → explicit close of the current connection
			_ = wsConn.SetReadDeadline(time.Now())
			wsWg.Wait()
			rtpConn.Close() // unblock rtpReader
			s.wg.Wait()     // drain rtpReader + rtpPacer
			_ = sendStop(wsConn, s.streamSid, s.callSidToken) // best-effort
			wsConn.Close()
			return

		case <-sig.Done():
			// WS layer failure — attempt reconnect (WSR-01).
			// Drain the old WS goroutines before touching wsConn (Pitfall 2).
			_ = wsConn.SetReadDeadline(time.Now())
			wsConn.Close() // causes wsPacer's next writeJSON to fail → exits
			wsWg.Wait()    // wait for BOTH WS goroutines to exit before reconnecting

			// Reconnect+handshake loop: both must succeed before relaunching audio.
			var newConn net.Conn
			for {
				var ok bool
				newConn, ok = s.reconnect(sessionCtx)
				if !ok {
					s.log.Error().Str("call_id", s.callID).Msg("WS reconnect budget exhausted — sending BYE")
					_ = s.dlg.Bye(context.Background())
					s.cancel()
					rtpConn.Close()
					s.wg.Wait()
					return
				}

				// WSR-02: re-send handshake on every reconnect before relaunching audio.
				if err := s.handshake(newConn); err != nil {
					s.log.Error().Err(err).Str("call_id", s.callID).Msg("reconnect handshake failed — retrying")
					newConn.Close()
					continue // retry reconnect from the top of this inner loop
				}
				break // handshake succeeded
			}

			wsConn = newConn
			// Loop continues — new sig + wsWg created at top of for loop
		}
	}
}

// handshake sends connected + start events on a fresh wsConn.
// Must be called before launching wsPacer/wsToRTP on each new connection.
// Reuses the same streamSid and callSidToken across reconnects (WSR-02:
// the WS consumer treats the reconnected stream as a continuation of the same call).
func (s *CallSession) handshake(wsConn net.Conn) error {
	if err := sendConnected(wsConn); err != nil {
		return fmt.Errorf("sendConnected: %w", err)
	}
	return sendStart(wsConn, s.streamSid, s.callSidToken, s.callID, s.dlg.InviteRequest)
}

// reconnect attempts to re-dial the target WebSocket with exponential backoff.
// Backoff: 1s → 2s → 4s (cap), total budget 30s from WSR-01.
// Returns (conn, true) on success; (nil, false) if budget exhausted or ctx cancelled.
// The backoff sleep happens BEFORE each dial attempt.
func (s *CallSession) reconnect(ctx context.Context) (net.Conn, bool) {
	budget, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	backoff := time.Second
	const maxBackoff = 4 * time.Second

	for attempt := 1; ; attempt++ {
		// Increment reconnect attempt counter at the top of each iteration (OBS-03).
		if s.metrics != nil {
			s.metrics.WSReconnects.Inc()
		}

		// Wait for backoff duration or budget expiry (Pitfall 7: context-aware select).
		select {
		case <-budget.Done():
			s.log.Error().Str("call_id", s.callID).Int("attempt", attempt).Msg("WS reconnect budget exhausted")
			return nil, false
		case <-time.After(backoff):
		}

		dialCtx, dialCancel := context.WithTimeout(budget, 5*time.Second)
		conn, err := dialWS(dialCtx, s.cfg.WSTargetURL)
		dialCancel()
		if err != nil {
			s.log.Warn().Err(err).Str("call_id", s.callID).Int("attempt", attempt).Dur("backoff", backoff).Msg("WS reconnect dial failed")
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		s.log.Info().Str("call_id", s.callID).Int("attempt", attempt).Msg("WebSocket reconnected")
		return conn, true
	}
}

// rtpReader reads RTP packets from the UDP socket and enqueues PCMU payloads to
// rtpInboundQueue for paced forwarding by wsPacer.
// DTMF packets (PT == s.dtmfPT) and non-PCMU packets are dropped silently.
// Enqueuing is non-blocking: if rtpInboundQueue is full the packet is dropped with a warning.
func (s *CallSession) rtpReader(ctx context.Context, rtpConn *net.UDPConn) {
	defer s.wg.Done()

	buf := make([]byte, 1500) // MTU-safe read buffer

	for {
		// Blocking read — no deadline. run() calls rtpConn.Close() after sessionCtx is done,
		// which causes ReadFromUDP to return an error; ctx.Err() != nil check below returns silently.
		n, _, err := rtpConn.ReadFromUDP(buf)

		if err != nil {
			if ctx.Err() != nil {
				return // rtpConn.Close() was called in run() after ctx cancelled — exit silently
			}
			s.log.Error().Err(err).Str("call_id", s.callID).Msg("rtpReader: ReadFromUDP error")
			return
		}

		var pkt rtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			s.log.Warn().Err(err).Str("call_id", s.callID).Msg("rtpReader: RTP unmarshal failed — skipping packet")
			continue
		}

		// RFC 4733 DTMF processing (WSB-07).
		if pkt.PayloadType == s.dtmfPT {
			digit, isEnd, ok := parseTelephoneEvent(pkt.Payload)
			if !ok || !isEnd {
				// Non-End packet (key held) or malformed — drop silently.
				// Only the End=1 packet signals digit completion (RFC 4733 §2.5).
				continue
			}
			// RFC 4733 deduplication by RTP timestamp:
			// Sender retransmits End=1 packet 3x with the SAME timestamp. Drop retransmissions.
			if pkt.Header.Timestamp == s.lastDtmfTS {
				continue
			}
			s.lastDtmfTS = pkt.Header.Timestamp
			select {
			case s.dtmfQueue <- digit:
			default:
				s.log.Warn().Str("call_id", s.callID).Str("digit", digit).Msg("rtpReader: DTMF queue full — dropping digit")
			}
			continue
		}

		// Drop non-PCMU packets.
		if pkt.PayloadType != 0 {
			continue
		}

		// Increment RTP receive counter for each valid PCMU packet (OBS-03).
		if s.metrics != nil {
			s.metrics.RTPRx.Inc()
		}

		// Copy payload — buf is reused on the next ReadFromUDP call.
		payload := make([]byte, len(pkt.Payload))
		copy(payload, pkt.Payload)

		// Non-blocking enqueue: burst packets are queued for paced forwarding by wsPacer.
		// If the queue is full (>1 s backlog) the frame is dropped rather than blocking the read loop.
		select {
		case s.rtpInboundQueue <- payload:
		case <-ctx.Done():
			return
		default:
			s.log.Warn().Str("call_id", s.callID).Msg("rtpReader: inbound queue full — dropping PCMU packet")
		}
	}
}

// wsPacer drains rtpInboundQueue at a constant 20 ms rate and forwards each PCMU frame
// to the WebSocket as a Twilio Media Streams "media" event.
//
// Decoupling receipt (rtpReader) from forwarding (wsPacer) absorbs the periodic sender-side
// RTP bursts observed from sipgate: when the sender holds back N packets for ~100 ms and
// then releases them all at once, rtpReader enqueues the burst instantly while wsPacer
// continues to forward one packet every 20 ms — the WS consumer sees a smooth stream.
//
// wsPacer is the SOLE WS writer during audio forwarding — no other goroutine may write to wsConn.
// wg is the per-connection WaitGroup (not s.wg); sig signals WS-layer failure to run().
func (s *CallSession) wsPacer(ctx context.Context, wsConn net.Conn, wg *sync.WaitGroup, sig *wsSignal) {
	defer wg.Done()

	var seqNo uint32 = 2 // sequence starts at 2 (connected=seq 0, start=seq 1)
	startTimeMs := time.Now().UnixMilli()

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case digit := <-s.dtmfQueue:
			// WSB-07: forward DTMF digit immediately (not paced at 20 ms).
			// dtmfQueue case fires before ticker.C — DTMF is a control event, not audio.
			if err := sendDTMF(wsConn, s.streamSid, digit, seqNo); err != nil {
				s.log.Error().Err(err).Str("call_id", s.callID).Str("digit", digit).Msg("wsPacer: sendDTMF failed")
				sig.Signal()
				return
			}
			seqNo++
		case <-ticker.C:
			// Dequeue one PCMU frame. If none is available, skip this tick silently —
			// the WS consumer is expected to handle gaps (the inbound stream is voice,
			// not continuous audio, and the consumer has its own jitter buffer).
			var payload []byte
			select {
			case payload = <-s.rtpInboundQueue:
			default:
				continue
			}

			encoded := base64.StdEncoding.EncodeToString(payload)
			timestamp := time.Now().UnixMilli() - startTimeMs

			event := MediaEvent{
				Event:          "media",
				SequenceNumber: fmt.Sprintf("%d", seqNo),
				StreamSid:      s.streamSid,
				Media: MediaBody{
					Track:     "inbound",
					Chunk:     fmt.Sprintf("%d", seqNo),
					Timestamp: fmt.Sprintf("%d", timestamp),
					Payload:   encoded,
				},
			}

			if err := writeJSON(wsConn, event); err != nil {
				s.log.Error().Err(err).Str("call_id", s.callID).Msg("wsPacer: writeJSON failed")
				sig.Signal() // notify run() that the WS layer failed
				return
			}
			seqNo++
		}
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
// "stop" events trigger dlg.Bye + cancel session (SIP-05) — this is a SIP-side teardown.
// WS read errors (when ctx is still live) signal run() via sig for reconnect handling.
// wg is the per-connection WaitGroup (not s.wg); sig signals WS-layer failure to run().
func (s *CallSession) wsToRTP(ctx context.Context, wsConn net.Conn, wg *sync.WaitGroup, sig *wsSignal) {
	defer wg.Done()

	for {
		if ctx.Err() != nil {
			return
		}

		msgData, op, err := readWSMessage(wsConn)
		if err != nil {
			if ctx.Err() != nil {
				return // session ended normally — WS read deadline expired or conn closed
			}
			// WS read error while session is still active — signal run() for reconnect.
			// Do NOT send BYE here; run() decides whether to reconnect or give up.
			s.log.Error().Err(err).Str("call_id", s.callID).Msg("wsToRTP: WS read error — signalling reconnect")
			sig.Signal()
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
			// "stop" is a SIP-side teardown signal from the WS consumer (SIP-05).
			// Send BYE and cancel the session — this is not a WS-layer failure.
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
			// Increment RTP send counter on successful packet delivery (OBS-03).
			if s.metrics != nil {
				s.metrics.RTPTx.Inc()
			}
			seqNo++
			timestamp += 160 // 20 ms @ 8 kHz PCMU
		}
	}
}
