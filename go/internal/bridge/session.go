package bridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gobwas/ws"
	"github.com/pion/rtp"
	pionSRTP "github.com/pion/srtp/v2"
	"github.com/rs/zerolog"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/config"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/observability"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/webhook"
)

// packetQueueSize is the maximum number of 20 ms audio frames buffered for the RTP pacer.
// 500 frames = 10 seconds; large enough for any realistic TTS response blob.
// When full, new frames are dropped and a warning is logged.
const packetQueueSize = 500

// terminalState bundles the post-termination snapshot fields so they can be
// published atomically by markTerminated and read race-free by REST API
// getters (EndTime, TermReason, Duration, Status). The pointer is stamped
// exactly once via terminateOnce; a nil pointer means the call is still
// active.
type terminalState struct {
	endTime    time.Time
	termReason string
}

// StatusCallbackConfig is the per-call status-callback subscription consumed
// by the lifecycle and terminal emission paths. Replaceable via
// SetStatusCallback — REST POST /Calls/{Sid}.json with new params overwrites
// cleanly via atomic.Pointer.
//
// Method is "POST" or "GET" (Twilio default POST). Events is a SET (not slice)
// so emission-time membership checks are O(1). Membership domain is the union
// of <Dial>-leg events {initiated, ringing, answered, completed} and parent-
// call events {initiated, ringing, answered, in-progress, completed, busy,
// failed, no-answer, canceled}.
type StatusCallbackConfig struct {
	URL    string
	Method string              // "POST" | "GET"
	Events map[string]struct{} // membership set; nil-or-empty = subscribe-to-all

	// Trusted=true marks an operator-supplied URL (env var
	// STATUS_CALLBACK_DEFAULT_URL) that bypasses the SSRF guard at
	// dial time and the pre-flight ValidateCallbackURL at Enqueue.
	// Customer-supplied URLs (REST POST StatusCallback=) MUST leave
	// this false. Operator-supplied URLs are trusted because the
	// process operator controls deployment and can (e.g.) point at
	// 127.0.0.1 callbacks intentionally for sidecar pipelines.
	Trusted bool
}

// rtpInboundQueueSize is the maximum number of 20 ms audio frames buffered between
// rtpReader and wsPacer. 50 frames = 1 second — enough to absorb the periodic
// ~400 ms sender-side RTP batching observed from sipgate without dropping packets.
// When full, new frames are dropped and a warning is logged.
const rtpInboundQueueSize = 50

// SRTP key/salt lengths for AES_128_CM_HMAC_SHA1_80 (RFC 3711, RFC 4568).
const srtpKeyLength  = 16 // 128-bit AES master key
const srtpSaltLength = 14 // 112-bit master salt

// outboundFrame is a tagged union for packetQueue entries.
// Exactly one field is set per instance:
//   - audio != nil: a 160-byte audio frame to send to the caller
//   - mark != "": a mark sentinel — route to markEchoQueue, send no RTP packet
type outboundFrame struct {
	audio []byte // non-nil for audio frames (160 bytes)
	mark  string // non-empty for mark sentinel frames
}

// CallSession holds all per-call state shared across legs: WS connection,
// goroutine lifecycle, control-plane queues. Per-leg SIP/RTP state lives in
// CallSession.legs (see leg.go). Single-leg paths always have
// len(legs) == 1; the dual-leg path adds legs[1] for the B2BUA <Dial>
// callee.
//
// Ownership: StartSession creates one instance; run() owns it for the call lifetime.
//
// Dual-leg additions:
//   - wsCtx / wsCancel: child of session ctx, scoped to WS goroutine
//     lifetime. CloseStream cancels wsCtx (NOT session ctx) so WS pacer/reader
//     drain while RTP goroutines stay live for the forwarded call.
//   - wsWg: pointer to the active per-connection WaitGroup so CloseStream can
//     wait for WS goroutines to exit before closing the WS conn.
//   - wsConnPtr: pointer to the active wsConn so CloseStream can close it.
//   - streamClosed: latched-once flag; set by CloseStream; read by run() to
//     suppress sendStop on the natural-end path (WS conn already closed).
//   - closeStreamOnce: idempotency guard for CloseStream.
//   - stopReason: free-form string captured by CloseStream; surfaced in logs
//     and metrics. The wire-format StopEvent body stays Twilio-shape (no new
//     fields) — Twilio bit-for-bit compat.
type CallSession struct {
	callID        string
	callSid       string      // CA-prefixed Twilio callSid (distinct from SIP Call-ID)
	streamSid     string
	accountSid    string      // populated by main.go wiring; "" in boot path before wiring
	// REST-API metadata. Captured once at StartSession; from/to/startTime are
	// immutable thereafter. endTime/termReason are stamped exactly once by markTerminated()
	// from the StartSession defer chain on the same goroutine that owns the session — so
	// concurrent reads from the snapshot path are safe-by-construction (single writer, then
	// the active-map entry disappears before any other goroutine can observe stale state).
	from       string    // SIP From URI (e.g. "+4915123…@sipgate.de"), captured at StartSession
	to         string    // SIP To URI
	startTime  time.Time // UTC, captured at StartSession via time.Now().UTC()
	// terminal holds the post-termination snapshot (endTime + termReason)
	// once markTerminated has stamped it. atomic.Pointer makes the
	// single-write / multi-reader access race-free, which matters for the
	// async modify-call dispatch path: writeCallJSON reads from the API
	// goroutine concurrently with markTerminated running in the dispatch
	// goroutine. Nil pointer = call still active (terminal not stamped yet).
	terminal   atomic.Pointer[terminalState]

	// Per-call status-callback subscription + per-call sequence counter +
	// answeredAt timestamp + SIP final code. All four mirror the existing
	// terminal-pointer atomic pattern; emission logic (the actual statusWC
	// POSTer + markTerminated insertion) lives in the terminal-emit path.
	statusCallback atomic.Pointer[StatusCallbackConfig] // current subscription; nil = none
	seqCounter     atomic.Uint64                        // Add(1)-1 yields {0,1,2,...}
	answeredAt     atomic.Pointer[time.Time]            // first 200 OK + ACK; nil if call never answered
	sipFinalCode   atomic.Int32                         // first non-zero SIP final response code; 0 if unknown

	// Tracks whether ANY customer-visible status-callback event has fired
	// on this CallSid. CompareAndSwap-stamped (false→true) by
	// emitStatusEvent (handler-side), emitDialEvent (forwarder-side,
	// dial-leg), and emitTerminalStatusCallback (bridge-side, defense-in-
	// depth) on first successful Enqueue. Read by PreRegisterSession's
	// cleanup closure to suppress ghost terminal-only callbacks for calls
	// that never reached "initiated" emit. The customer is told "the call
	// ended" only if they were first told "the call started".
	everEmitted atomic.Bool

	// The per-process StatusClient that emits terminal callbacks from
	// inside markTerminated's terminateOnce.Do closure. Nil-safe:
	// deployments and tests that don't exercise the emission path leave
	// this nil and emitTerminalStatusCallback short-circuits before
	// touching any network surface. Production wiring threads through
	// CallManager (see SetStatusClient + StartSession).
	statusWC *webhook.StatusClient

	state         AtomicState // zero-value = StateDispatching
	legs          []*Leg      // single-leg paths: len(legs) == 1; dual-leg path grows to len(legs) == 2 via SetLeg(1, ...)
	legsMu        sync.RWMutex // guards growth of the legs slice via SetLeg() vs concurrent reads
	dtmfQueue     chan string // digit strings ("0"-"9","*","#","A"-"D") from rtpReader to wsPacer
	markEchoQueue chan string // mark names from rtpPacer to wsPacer (capacity 10)
	clearSignal   chan struct{} // buffered (1): wsToRTP notifies rtpPacer to drain packetQueue
	cfg           config.Config
	log           zerolog.Logger
	cancel        context.CancelFunc
	wg            sync.WaitGroup // tracks ONLY rtpReader + rtpPacer (persistent RTP goroutines)
	metrics       *observability.Metrics

	// Privacy Gate plumbing.
	// wsCtx is a child of the session context; cancelling it stops only the
	// WS goroutines (wsPacer, wsToRTP). Session-level RTP goroutines stay
	// live so the dual-leg RTP relay can continue once the Forwarder wires
	// legs[1].
	sessionCtx   context.Context    // nil until run() initializes; live for the entire call
	wsCtx        context.Context    // nil until run() initializes
	wsCancel     context.CancelFunc // nil until run() initializes
	wsWgPtr      atomic.Pointer[sync.WaitGroup]
	wsConnPtr    atomic.Pointer[net.Conn]
	streamClosed atomic.Bool // true after CloseStream has run; suppresses sendStop on natural end
	stopReason   string      // captured by CloseStream; logged in WS-stop teardown
	closeStreamOnce sync.Once

	// terminateOnce ensures markTerminated is idempotent. The first call wins:
	// endTime + termReason are stamped exactly once. Subsequent calls (e.g. the
	// run() defer chain after a REST handler already invoked Terminate, or a
	// double Terminate from concurrent goroutines) are no-ops at the field-set
	// level. See Terminate() and markTerminated() for the cooperating sequence.
	terminateOnce sync.Once
}

// primary returns legs[0]. Single-leg paths use s.primary().<field>
// in place of the old s.<field>. The dual-leg path adds legs[1] for the
// outbound leg.
func (s *CallSession) primary() *Leg {
	s.legsMu.RLock()
	defer s.legsMu.RUnlock()
	if len(s.legs) == 0 {
		return nil
	}
	return s.legs[0]
}

// PrimaryLeg returns legs[0] — the inbound caller leg — or nil if no leg is
// installed. Exported for the SIP <Dial> Forwarder which reads
// the caller's RTP destination to wire the relay direction.
func (s *CallSession) PrimaryLeg() *Leg {
	return s.primary()
}

// CalleeLeg returns legs[1] — the outbound B2BUA callee leg installed by the
// Forwarder via SetLeg(1, calleeLeg) — or nil before SetLeg has run.
// Exported for symmetry with PrimaryLeg(). Lock-free safe under SetLeg races
// thanks to the legsMu RWMutex.
func (s *CallSession) CalleeLeg() *Leg {
	s.legsMu.RLock()
	defer s.legsMu.RUnlock()
	if len(s.legs) <= 1 {
		return nil
	}
	return s.legs[1]
}

// SetLeg installs a leg at the given index. Dual-leg use: SetLeg(1, calleeLeg)
// after the outbound 200 OK + ACK. The legs slice is grown if needed; existing
// legs are preserved. Concurrent SetLeg calls and concurrent leg reads are
// guarded by legsMu (writers exclusive; readers shared).
//
// idx must be >= 0. idx == 0 is permitted for tests / forwarder reset paths.
// Out-of-bounds writes (idx > 100 say) silently no-op — defensive guard
// against accidental misuse from buggy Forwarder code.
func (s *CallSession) SetLeg(idx int, leg *Leg) {
	if idx < 0 || idx > 100 {
		return
	}
	s.legsMu.Lock()
	defer s.legsMu.Unlock()
	if idx >= len(s.legs) {
		grown := make([]*Leg, idx+1)
		copy(grown, s.legs)
		s.legs = grown
	}
	if leg != nil {
		// Wire the session backref so leg-side hooks (Leg.OnAnswered) can
		// reach session-scoped resources: state machine, sessionCtx, wg, log,
		// peer leg's packetQueue. Cleared again only when SetLeg(idx, nil)
		// is called, which production code does not do.
		leg.session = s
	}
	s.legs[idx] = leg
}

// NewCalleeLeg constructs a *Leg suitable for outbound use (legs[1] in a
// dual-leg session). The returned leg has:
//   - rtpPort set to the caller-supplied port (acquired from CallManager.AcquirePort)
//   - contactIP set so BuildSDPOffer constructs a routable c= line
//   - outboundQueue allocated at packetQueueSize (callee leg's RTP relay channel)
//   - isOutbound = true (marks this as the callee leg for rtpReader relay logic)
//
// The leg is NOT installed on the session — the caller calls SetLeg(1, leg)
// after the outbound 200 OK + ACK from the Forwarder.
//
// Production callers: midCallAdapter.PrepareDial in internal/api.
// Tests: may call directly to unit-test the outbound SDP flow.
func NewCalleeLeg(rtpPort int, contactIP string) *Leg {
	return &Leg{
		rtpPort:       rtpPort,
		contactIP:     contactIP,
		isOutbound:    true,
		outboundQueue: make(chan outboundFrame, packetQueueSize),
	}
}

// peerLeg returns the leg opposite to currentIdx. Used by rtpReader to
// determine the relay destination during StateForwarding. Returns nil when
// no peer is installed (e.g. mid-call before SetLeg(1, ...) completes).
func (s *CallSession) peerLeg(currentIdx int) *Leg {
	s.legsMu.RLock()
	defer s.legsMu.RUnlock()
	switch currentIdx {
	case 0:
		if len(s.legs) > 1 {
			return s.legs[1]
		}
	case 1:
		if len(s.legs) > 0 {
			return s.legs[0]
		}
	}
	return nil
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
//
// PHASE-15 PRIVACY GATE:
//   - WS goroutines run on s.wsCtx (a child of sessionCtx), not on sessionCtx
//     directly. CloseStream cancels wsCtx so wsPacer/wsToRTP exit cleanly
//     without tearing down the RTP goroutines. After CloseStream, the for-loop
//     observes streamClosed=true on its next iteration and parks waiting for
//     sessionCtx.Done() (the actual call end).
//   - sendStop is suppressed when streamClosed is true (WS conn is already
//     closed by CloseStream — writing would error).
func (s *CallSession) run(ctx context.Context, initialWsConn net.Conn) {
	// Pitfall 6: if BYE arrived before session started, context is already done — exit immediately.
	if ctx.Err() != nil {
		s.log.Warn().Str(observability.FieldSIPCallID, s.callID).Msg("session context already cancelled at entry — BYE arrived early")
		_ = initialWsConn.Close()
		return
	}

	// Derive session context from dialog context so BYE cancels the session.
	sessionCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.sessionCtx = sessionCtx
	defer cancel()

	// Derive a child context for the WS goroutines (Privacy Gate).
	// CloseStream cancels wsCtx independently; sessionCtx stays live so the
	// dual-leg RTP relay can keep running. Ordered before any goroutine spawn
	// so CloseStream's nil-guard is satisfied as soon as run() begins.
	wsCtx, wsCancel := context.WithCancel(sessionCtx)
	s.wsCtx = wsCtx
	s.wsCancel = wsCancel
	defer wsCancel()

	leg := s.primary()

	// Open RTP socket on the acquired port.
	rtpConn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: leg.rtpPort})
	if err != nil {
		s.log.Error().Err(err).Int("rtp_port", leg.rtpPort).Str(observability.FieldSIPCallID, s.callID).Msg("ListenUDP failed — sending BYE")
		_ = initialWsConn.Close()
		_ = leg.dlg.Bye(context.Background())
		return
	}
	defer func() { _ = rtpConn.Close() }()

	// Build SRTP contexts when SRTP was negotiated (localSRTPKey != nil).
	// decCtx uses the remote key to decrypt inbound packets; encCtx uses the local key to encrypt outbound.
	var srtpDecCtx, srtpEncCtx *pionSRTP.Context
	if len(leg.localSRTPKey) == srtpKeyLength && len(leg.localSRTPSalt) == srtpSaltLength &&
		len(leg.remoteSRTPKey) == srtpKeyLength && len(leg.remoteSRTPSalt) == srtpSaltLength {
		decCtx, err := pionSRTP.CreateContext(leg.remoteSRTPKey, leg.remoteSRTPSalt,
			pionSRTP.ProtectionProfileAes128CmHmacSha1_80)
		if err != nil {
			s.log.Error().Err(err).Str(observability.FieldSIPCallID, s.callID).Msg("SRTP decrypt context creation failed — sending BYE")
			_ = initialWsConn.Close()
			_ = leg.dlg.Bye(context.Background())
			return
		}
		encCtx, err := pionSRTP.CreateContext(leg.localSRTPKey, leg.localSRTPSalt,
			pionSRTP.ProtectionProfileAes128CmHmacSha1_80)
		if err != nil {
			s.log.Error().Err(err).Str(observability.FieldSIPCallID, s.callID).Msg("SRTP encrypt context creation failed — sending BYE")
			_ = initialWsConn.Close()
			_ = leg.dlg.Bye(context.Background())
			return
		}
		srtpDecCtx = decCtx
		srtpEncCtx = encCtx
		s.log.Info().Str(observability.FieldSIPCallID, s.callID).Msg("SRTP negotiated — media encrypted with AES-128-CM-HMAC-SHA1-80")
	}

	// Initialize control-plane queues before launching goroutines.
	// Per-leg RTP channels (packetQueue, rtpInboundQueue) are allocated by manager.go
	// when the Leg literal is built. Shared cross-leg queues live on CallSession.
	s.dtmfQueue = make(chan string, 10) // 10 slots; more than enough for any realistic keypress burst
	s.markEchoQueue = make(chan string, 10)
	s.clearSignal = make(chan struct{}, 1)

	// Send initial handshake on the pre-dialed connection from StartSession.
	wsConn := initialWsConn
	if err := s.handshake(wsConn); err != nil {
		s.log.Error().Err(err).Str(observability.FieldSIPCallID, s.callID).Msg("initial handshake failed — sending BYE")
		_ = wsConn.Close()
		_ = leg.dlg.Bye(context.Background())
		return
	}

	// RTP goroutines: persistent for the full call lifetime, tracked by s.wg.
	// rtpReader enqueues inbound PCMU to rtpInboundQueue; rtpPacer drains packetQueue to caller.
	s.wg.Add(2)
	go s.rtpReader(sessionCtx, rtpConn, srtpDecCtx)
	go s.rtpPacer(sessionCtx, rtpConn, srtpEncCtx)

	// WS reconnect loop: stops/restarts only wsPacer + wsToRTP on each new connection.
	// A fresh sig + wsWg is created for each connection iteration.
	for {
		// If CloseStream has already fired (Privacy Gate), do NOT respawn WS
		// goroutines. Park here waiting for the session to end; the RTP
		// goroutines keep relaying for the dual-leg call. The Forwarder
		// will eventually cancel sessionCtx via Terminate().
		if s.streamClosed.Load() {
			<-sessionCtx.Done()
			_ = rtpConn.Close()
			s.wg.Wait()
			// No sendStop — WS conn is already closed by CloseStream.
			s.markTerminated("completed")
			return
		}

		sig := newWsSignal()
		wsWg := &sync.WaitGroup{}
		wsWg.Add(2)
		s.wsWgPtr.Store(wsWg)
		// Capture the current wsConn under an atomic.Pointer so CloseStream
		// can close it without racing on a plain field-write.
		curWsConn := wsConn
		s.wsConnPtr.Store(&curWsConn)
		go s.wsPacer(wsCtx, wsConn, wsWg, sig)
		go s.wsToRTP(wsCtx, wsConn, wsWg, sig)

		select {
		case <-sessionCtx.Done():
			// Normal call end (BYE from either side).
			// Shutdown sequence:
			//   1. SetReadDeadline → unblocks wsToRTP's blocked readWSMessage call
			//   2. wsWg.Wait()    → drains wsToRTP + wsPacer (wsPacer sees ctx.Done or write error)
			//   3. rtpConn.Close() → unblocks rtpReader's blocking ReadFromUDP
			//   4. s.wg.Wait()   → drains rtpReader + rtpPacer
			//   5. sendStop      → sole writer now; best-effort teardown message
			//                       (skipped when CloseStream already ran)
			//   6. wsConn.Close() → explicit close of the current connection
			_ = wsConn.SetReadDeadline(time.Now())
			wsWg.Wait()
			_ = rtpConn.Close() // unblock rtpReader
			s.wg.Wait()         // drain rtpReader + rtpPacer
			if !s.streamClosed.Load() {
				_ = sendStop(wsConn, s.streamSid, s.callSid, s.accountSid) // best-effort
				_ = wsConn.Close()
			}
			// Stamp endTime + termReason on the natural-completion path.
			// StartSession's defer chain reads these into the
			// recentlyTerminated snapshot. Refined reasons (busy / failed /
			// no-answer / canceled) are stamped by other code paths; this
			// branch only handles the graceful-end case.
			s.markTerminated("completed")
			return

		case <-wsCtx.Done():
			// Privacy Gate: CloseStream cancelled wsCtx (which is a
			// child of sessionCtx). When sessionCtx is also done, this case
			// races with sessionCtx.Done(); the natural-end branch above is
			// preferred — fall through to it on the next iteration.
			// When ONLY wsCtx is done (CloseStream alone, sessionCtx still
			// live), the loop top's streamClosed check parks us until
			// sessionCtx ends.
			wsWg.Wait()
			// Loop continues; the next iteration's streamClosed check parks.

		case <-sig.Done():
			// WS layer failure — attempt reconnect (WSR-01).
			// If CloseStream fired concurrently with the WS error, suppress
			// the reconnect and park instead.
			_ = wsConn.SetReadDeadline(time.Now())
			_ = wsConn.Close() // causes wsPacer's next writeJSON to fail → exits
			wsWg.Wait()        // wait for BOTH WS goroutines to exit before reconnecting

			if s.streamClosed.Load() {
				// Privacy Gate took precedence — don't reconnect.
				continue
			}

			// Reconnect+handshake loop: both must succeed before relaunching audio.
			var newConn net.Conn
			for {
				var ok bool
				newConn, ok = s.reconnect(sessionCtx)
				if !ok {
					s.log.Error().Str(observability.FieldSIPCallID, s.callID).Msg("WS reconnect budget exhausted — sending BYE")
					_ = leg.dlg.Bye(context.Background())
					s.cancel()
					_ = rtpConn.Close()
					s.wg.Wait()
					return
				}

				// WSR-02: re-send handshake on every reconnect before relaunching audio.
				if err := s.handshake(newConn); err != nil {
					s.log.Error().Err(err).Str(observability.FieldSIPCallID, s.callID).Msg("reconnect handshake failed — retrying")
					_ = newConn.Close()
					continue // retry reconnect from the top of this inner loop
				}
				break // handshake succeeded
			}

			wsConn = newConn
			// Loop continues — new sig + wsWg created at top of for loop
		}
	}
}

// CloseStream is the Privacy Gate primitive: drain WS goroutines,
// close the WS connection, and prevent any further bot media exposure for the
// remainder of the call. RTP goroutines stay live so the <Dial> Forwarder
// can wire a callee leg and bridge media.
//
// Idempotent via sync.Once. The first call wins on stopReason and on the
// state.CAS to StateDialingOut; subsequent calls are no-ops at every layer.
//
// reason becomes part of the WS-stop log and the metrics tag (a
// forward_streams_closed counter labelled by reason). The wire-format
// StopEvent body remains Twilio-shape — no new fields — preserving bit-for-bit
// Twilio compatibility per project memory.
//
// Ordering invariants:
//  1. streamClosed flag is set FIRST so that any concurrent run() iteration
//     observing wsCtx.Done() loops back into the streamClosed park branch.
//  2. wsCancel triggers wsPacer / wsToRTP exit (they observe ctx.Done()).
//  3. wsWg.Wait() blocks until BOTH WS goroutines have returned — guarantees
//     the WS conn is no longer being read from / written to (Pitfall 1).
//  4. sendStop is best-effort: WS server may already have closed; ignore err.
//  5. wsConn.Close() releases the underlying TCP socket.
//  6. state.CAS(Streaming, DialingOut) for visibility to concurrent IsActive
//     / Status readers.
//
// Returns the first error encountered (currently always nil — sendStop and
// Close are best-effort). The signature reserves the error slot for future
// expansion (e.g. wsTimeout enforcement).
//
// Safe to call from any goroutine; safe to call before run() initializes
// wsCtx — the nil-guards on wsCancel / wsWgPtr / wsConnPtr handle that case.
func (s *CallSession) CloseStream(reason string) error {
	var firstErr error
	s.closeStreamOnce.Do(func() {
		s.stopReason = reason
		s.streamClosed.Store(true)

		// State transition: StateStreaming → StateDialingOut.
		// CAS-misses are non-fatal — Status() readers will observe the
		// post-transition value via state.Load() in any case.
		s.state.CAS(StateStreaming, StateDialingOut)

		// Cancel WS context — wsPacer observes ctx.Done() at the top of its
		// select and exits immediately. wsToRTP is blocked inside
		// wsutil.ReadServerData which does NOT observe ctx — to unblock it
		// we must force the read to fail by setting an immediate read
		// deadline on the wsConn (parallels what run()'s natural-end path
		// does at session.go ~350). Without this, wsToRTP stays parked on
		// the syscall, wsWg.Wait() blocks forever, and the API-handler
		// goroutine that called PrepareDial → CloseStream deadlocks (#2585).
		if s.wsCancel != nil {
			s.wsCancel()
		}
		if connPtr := s.wsConnPtr.Load(); connPtr != nil && *connPtr != nil {
			conn := *connPtr
			// SetReadDeadline of "now" makes the in-flight Read return a
			// timeout error; wsToRTP's loop top then sees ctx.Err() != nil
			// and returns, closing its half of wsWg.
			_ = conn.SetReadDeadline(time.Now())
			// SetWriteDeadline likewise unblocks wsPacer if it happens to
			// be inside a blocking write at the moment we cancel — TCP
			// send-buffer back-pressure from a slow WS consumer must not
			// be able to deadlock the Privacy Gate.
			_ = conn.SetWriteDeadline(time.Now())
		}

		// Wait for WS goroutines to drain. nil-guard covers the
		// pre-run() / unit-test fixture case where wsWgPtr was never set.
		if wg := s.wsWgPtr.Load(); wg != nil {
			wg.Wait()
		}

		// Send WS stop (best-effort) + close the conn. Guarded so that
		// fixtures without a real wsConn don't NPE.
		if connPtr := s.wsConnPtr.Load(); connPtr != nil && *connPtr != nil {
			conn := *connPtr
			// Reset write deadline so sendStop has a fresh chance to write.
			// If the consumer is still misbehaving the write fails fast and
			// we proceed to close — sendStop is best-effort by contract.
			_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			_ = sendStop(conn, s.streamSid, s.callSid, s.accountSid)
			_ = conn.Close()
		}

		s.log.Info().
			Str(observability.FieldSIPCallID, s.callID).
			Str(observability.FieldCallSid, s.callSid).
			Str("reason", reason).
			Msg("CloseStream: WS torn down (Privacy Gate engaged)")
	})
	return firstErr
}

// handshake sends connected + start events on a fresh wsConn.
// Must be called before launching wsPacer/wsToRTP on each new connection.
// Reuses the same streamSid and callSid across reconnects (WSR-02:
// the WS consumer treats the reconnected stream as a continuation of the same call).
func (s *CallSession) handshake(wsConn net.Conn) error {
	if err := sendConnected(wsConn); err != nil {
		return fmt.Errorf("sendConnected: %w", err)
	}
	leg := s.primary()
	return sendStart(wsConn, s.streamSid, s.callSid, s.accountSid, s.callID, leg.dlg.InviteRequest, leg.mediaEncoding, leg.mediaSampleRate)
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
		// Increment reconnect attempt counter at the top of each iteration.
		if s.metrics != nil {
			s.metrics.WSReconnects.Inc()
		}

		// Wait for backoff duration or budget expiry (Pitfall 7: context-aware select).
		select {
		case <-budget.Done():
			s.log.Error().Str(observability.FieldSIPCallID, s.callID).Int("attempt", attempt).Msg("WS reconnect budget exhausted")
			return nil, false
		case <-time.After(backoff):
		}

		dialCtx, dialCancel := context.WithTimeout(budget, 5*time.Second)
		conn, err := dialWS(dialCtx, s.cfg.WSTargetURL)
		dialCancel()
		if err != nil {
			s.log.Warn().Err(err).Str(observability.FieldSIPCallID, s.callID).Int("attempt", attempt).Dur("backoff", backoff).Msg("WS reconnect dial failed")
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		s.log.Info().Str(observability.FieldSIPCallID, s.callID).Int("attempt", attempt).Msg("WebSocket reconnected")
		return conn, true
	}
}

// rtpReader reads RTP (or SRTP) packets from the UDP socket and enqueues audio
// payloads onto either rtpInboundQueue (WS forwarding path, StateStreaming)
// or the peer leg's outboundQueue (B2BUA relay path, StateForwarding).
//
// DTMF packets (PT == s.dtmfPT) and non-audio packets are dropped silently.
// Enqueuing is non-blocking: if the destination queue is full the packet is
// dropped with a warning.
// srtpDecCtx is non-nil when SRTP was negotiated; nil means plain RTP.
//
// RTP relay: when state == StateForwarding, the inbound caller leg's
// rtpReader writes to legs[1].outboundQueue (the callee leg's rtpPacer drains
// it). The reverse direction (callee → caller) is wired when the callee
// leg's own rtpReader is spawned. legs[0] passes currentLegIdx==0;
// legs[1] passes currentLegIdx==1. The single-leg path only spawns
// rtpReader for legs[0] (legs[1].rtpReader belongs to the Forwarder).
func (s *CallSession) rtpReader(ctx context.Context, rtpConn *net.UDPConn, srtpDecCtx *pionSRTP.Context) {
	defer s.wg.Done()

	leg := s.primary()
	const currentLegIdx = 0
	buf := make([]byte, 1500) // MTU-safe read buffer
	decBuf := make([]byte, 1500)

	for {
		// Blocking read — no deadline. run() calls rtpConn.Close() after sessionCtx is done,
		// which causes ReadFromUDP to return an error; ctx.Err() != nil check below returns silently.
		n, _, err := rtpConn.ReadFromUDP(buf)

		if err != nil {
			if ctx.Err() != nil {
				return // rtpConn.Close() was called in run() after ctx cancelled — exit silently
			}
			s.log.Error().Err(err).Str(observability.FieldSIPCallID, s.callID).Msg("rtpReader: ReadFromUDP error")
			return
		}

		// Decrypt SRTP to plain RTP when a decrypt context is available.
		raw := buf[:n]
		if srtpDecCtx != nil {
			decrypted, err := srtpDecCtx.DecryptRTP(decBuf[:0], raw, nil)
			if err != nil {
				s.log.Warn().Err(err).Str(observability.FieldSIPCallID, s.callID).Msg("rtpReader: SRTP decrypt failed — skipping packet")
				continue
			}
			raw = decrypted
		}

		var pkt rtp.Packet
		if err := pkt.Unmarshal(raw); err != nil {
			s.log.Warn().Err(err).Str(observability.FieldSIPCallID, s.callID).Msg("rtpReader: RTP unmarshal failed — skipping packet")
			continue
		}

		// RFC 4733 DTMF processing (WSB-07).
		if pkt.PayloadType == leg.dtmfPT {
			digit, isEnd, ok := parseTelephoneEvent(pkt.Payload)
			if !ok || !isEnd {
				// Non-End packet (key held) or malformed — drop silently.
				// Only the End=1 packet signals digit completion (RFC 4733 §2.5).
				continue
			}
			// RFC 4733 deduplication by RTP timestamp:
			// Sender retransmits End=1 packet 3x with the SAME timestamp. Drop retransmissions.
			if pkt.Timestamp == leg.lastDtmfTS {
				continue
			}
			leg.lastDtmfTS = pkt.Timestamp
			select {
			case s.dtmfQueue <- digit:
			default:
				s.log.Warn().Str(observability.FieldSIPCallID, s.callID).Str("digit", digit).Msg("rtpReader: DTMF queue full — dropping digit")
			}
			continue
		}

		// Drop non-audio packets (pass-through for negotiated PT only).
		if pkt.PayloadType != leg.audioPT {
			continue
		}

		// Increment RTP receive counter for each valid audio packet.
		if s.metrics != nil {
			s.metrics.RTPRx.Inc()
		}

		// Copy payload — buf is reused on the next ReadFromUDP call.
		payload := make([]byte, len(pkt.Payload))
		copy(payload, pkt.Payload)

		// Dual-leg RTP relay: when the session is in StateForwarding,
		// route this audio frame to the peer leg's outboundQueue (drained by
		// the peer's rtpPacer) instead of the WS-bound rtpInboundQueue. Falls
		// through to the WS path when state != StateForwarding (StateStreaming
		// is the normal v3.0 single-leg path).
		if s.state.Load() == StateForwarding {
			peer := s.peerLeg(currentLegIdx)
			if peer != nil && peer.outboundQueue != nil {
				select {
				case peer.outboundQueue <- outboundFrame{audio: payload}:
				case <-ctx.Done():
					return
				default:
					s.log.Warn().Str(observability.FieldSIPCallID, s.callID).
						Msg("rtpReader: peer outboundQueue full — dropping forwarded audio packet")
				}
			}
			continue // do NOT also enqueue to WS path during Forwarding
		}

		// During StateDialingOut (Privacy Gate fired, outbound INVITE in
		// flight) we have no consumer for rtpInboundQueue: wsPacer has
		// exited and the relay path is not yet active. The caller's audio
		// during the ring window is irrelevant (Phone B hasn't answered),
		// so we drop silently without filling the queue. Once OnAnswered
		// runs and the state advances to StateForwarding, the relay branch
		// above takes over before this fallthrough is reached.
		if s.state.Load() == StateDialingOut {
			continue
		}

		// Non-blocking enqueue: burst packets are queued for paced forwarding by wsPacer.
		// If the queue is full (>1 s backlog) the frame is dropped rather than blocking the read loop.
		select {
		case leg.rtpInboundQueue <- payload:
		case <-ctx.Done():
			return
		default:
			s.log.Warn().Str(observability.FieldSIPCallID, s.callID).Msg("rtpReader: inbound queue full — dropping audio packet")
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

	leg := s.primary()
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
				s.log.Error().Err(err).Str(observability.FieldSIPCallID, s.callID).Str("digit", digit).Msg("wsPacer: sendDTMF failed")
				sig.Signal()
				return
			}
			seqNo++
		case markName := <-s.markEchoQueue:
			// MARK-01/MARK-02/MARK-03: echo mark name to WS server.
			// Debug level per locked decision (protocol noise, not error signal).
			s.log.Debug().Str(observability.FieldSIPCallID, s.callID).Str("mark", markName).Msg("wsPacer: echoing mark")
			if err := sendMarkEcho(wsConn, s.streamSid, markName, seqNo); err != nil {
				s.log.Error().Err(err).Str(observability.FieldSIPCallID, s.callID).Str("mark", markName).
					Msg("wsPacer: sendMarkEcho failed")
				sig.Signal()
				return
			}
			seqNo++
			if s.metrics != nil {
				s.metrics.MarkEchoed.Inc()
			}
		case <-ticker.C:
			// Dequeue one PCMU frame. If none is available, skip this tick silently —
			// the WS consumer is expected to handle gaps (the inbound stream is voice,
			// not continuous audio, and the consumer has its own jitter buffer).
			var payload []byte
			select {
			case payload = <-leg.rtpInboundQueue:
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
				s.log.Error().Err(err).Str(observability.FieldSIPCallID, s.callID).Msg("wsPacer: writeJSON failed")
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

	leg := s.primary()
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
			s.log.Error().Err(err).Str(observability.FieldSIPCallID, s.callID).Msg("wsToRTP: WS read error — signalling reconnect")
			sig.Signal()
			return
		}

		// Only process text frames (Twilio Media Streams sends JSON text frames).
		if op != ws.OpText {
			continue
		}

		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(msgData, &envelope); err != nil {
			s.log.Warn().Err(err).Str(observability.FieldSIPCallID, s.callID).Msg("wsToRTP: JSON unmarshal failed — skipping")
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
					s.log.Warn().Err(err).Str(observability.FieldSIPCallID, s.callID).Msg("wsToRTP: media decode failed — skipping")
					continue
				}
			}

			pcmuPayload, err := base64.StdEncoding.DecodeString(mediaObj.Payload)
			if err != nil {
				s.log.Warn().Err(err).Str(observability.FieldSIPCallID, s.callID).Msg("wsToRTP: base64 decode failed — skipping")
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
				case leg.packetQueue <- outboundFrame{audio: chunk}:
				case <-ctx.Done():
					return
				default:
					// Queue full — drop frame rather than block WS reading.
					s.log.Warn().Str(observability.FieldSIPCallID, s.callID).Msg("wsToRTP: packet queue full — dropping PCMU frame")
				}
			}

		case "mark":
			// Decode the mark name from the nested "mark" object.
			var markMsg struct {
				Mark struct {
					Name string `json:"name"`
				} `json:"mark"`
			}
			if raw, ok := envelope["mark"]; ok {
				if err := json.Unmarshal(raw, &markMsg); err != nil {
					s.log.Warn().Err(err).Str(observability.FieldSIPCallID, s.callID).Msg("wsToRTP: mark decode failed — skipping")
					continue
				}
			}
			markName := markMsg.Mark.Name
			s.log.Debug().Str(observability.FieldSIPCallID, s.callID).Str("mark", markName).Msg("wsToRTP: received mark")

			if len(leg.packetQueue) == 0 {
				// MARK-02: queue empty — echo immediately, do not enqueue sentinel.
				select {
				case s.markEchoQueue <- markName:
				default:
					s.log.Warn().Str(observability.FieldSIPCallID, s.callID).Str("mark", markName).
						Msg("wsToRTP: markEchoQueue full — dropping immediate mark echo")
				}
			} else {
				// MARK-01: audio buffered — enqueue sentinel to preserve order.
				select {
				case leg.packetQueue <- outboundFrame{mark: markName}:
				case <-ctx.Done():
					return
				default:
					s.log.Warn().Str(observability.FieldSIPCallID, s.callID).Str("mark", markName).
						Msg("wsToRTP: packetQueue full — dropping mark sentinel")
				}
			}

		case "clear":
			// MARK-03: signal rtpPacer to drain packetQueue.
			// rtpPacer is the sole owner of packetQueue — it performs the drain on its next tick.
			// clearSignal has capacity 1; excess clears coalesce (idempotent).
			s.log.Debug().Str(observability.FieldSIPCallID, s.callID).Msg("wsToRTP: received clear")
			select {
			case s.clearSignal <- struct{}{}:
			default:
				// previous clear not yet processed by rtpPacer — coalesced
			}
			if s.metrics != nil {
				s.metrics.ClearReceived.Inc()
			}

		case "stop":
			// "stop" is a SIP-side teardown signal from the WS consumer (SIP-05).
			// Send BYE and cancel the session — this is not a WS-layer failure.
			s.log.Info().Str(observability.FieldSIPCallID, s.callID).Msg("wsToRTP: received stop event — sending BYE (SIP-05)")
			_ = leg.dlg.Bye(context.Background())
			s.cancel()
			return

		default:
			// Unknown event types (e.g. "connected", "start" echo) — ignore.
			s.log.Debug().Str("event", eventType).Str(observability.FieldSIPCallID, s.callID).Msg("wsToRTP: unknown WS event — skipping")
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
// srtpEncCtx is non-nil when SRTP was negotiated; nil means plain RTP.
func (s *CallSession) rtpPacer(ctx context.Context, rtpConn *net.UDPConn, srtpEncCtx *pionSRTP.Context) {
	defer s.wg.Done()

	leg := s.primary()
	ssrc := rand.Uint32()
	var seqNo uint16
	var timestamp uint32
	encBuf := make([]byte, 1500)

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

			// MARK-03/MARK-04: check for clear signal before normal dequeue.
			// Drain the entire packetQueue: audio frames are discarded, mark sentinels are
			// echoed immediately via markEchoQueue. rtpPacer never stops — silence fills the tick.
			select {
			case <-s.clearSignal:
				s.log.Debug().Str(observability.FieldSIPCallID, s.callID).Msg("rtpPacer: clear signal — draining packetQueue")
			drainLoop:
				for {
					select {
					case f := <-leg.packetQueue:
						if f.mark != "" {
							select {
							case s.markEchoQueue <- f.mark:
							default:
								s.log.Warn().Str(observability.FieldSIPCallID, s.callID).Str("mark", f.mark).
									Msg("rtpPacer: markEchoQueue full during clear — dropping mark echo")
							}
						}
						// audio frames: discard silently
					default:
						break drainLoop
					}
				}
			default:
			}

			var frame outboundFrame
			select {
			case frame = <-leg.packetQueue:
			default:
				// empty queue — silence below
			}
			if frame.mark != "" {
				// MARK-01: mark sentinel dequeued — route to wsPacer and skip RTP for this tick.
				select {
				case s.markEchoQueue <- frame.mark:
				default:
					s.log.Warn().Str(observability.FieldSIPCallID, s.callID).Str("mark", frame.mark).
						Msg("rtpPacer: markEchoQueue full — dropping mark echo")
				}
				// Advance RTP counters normally so timestamp continuity is preserved (MARK-04).
				seqNo++
				timestamp += 160
				continue
			}
			chunk := frame.audio
			if chunk == nil {
				chunk = leg.silenceFrame
			}

			pkt := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    leg.audioPT,
					SequenceNumber: seqNo,
					Timestamp:      timestamp,
					SSRC:           ssrc,
				},
				Payload: chunk,
			}
			encoded, err := pkt.Marshal()
			if err != nil {
				s.log.Warn().Err(err).Str(observability.FieldSIPCallID, s.callID).Msg("rtpPacer: RTP marshal failed — skipping frame")
				continue
			}

			// Encrypt to SRTP when an encryption context is available.
			outPacket := encoded
			if srtpEncCtx != nil {
				encrypted, err := srtpEncCtx.EncryptRTP(encBuf[:0], encoded, &pkt.Header)
				if err != nil {
					s.log.Warn().Err(err).Str(observability.FieldSIPCallID, s.callID).Msg("rtpPacer: SRTP encrypt failed — skipping frame")
					seqNo++
					timestamp += 160
					continue
				}
				outPacket = encrypted
			}

			if _, err := rtpConn.WriteTo(outPacket, leg.remoteRTP); err != nil {
				s.log.Error().Err(err).Str(observability.FieldSIPCallID, s.callID).Msg("rtpPacer: WriteTo caller failed")
				s.cancel()
				return
			}
			// Increment RTP send counter on successful packet delivery.
			if s.metrics != nil {
				s.metrics.RTPTx.Inc()
			}
			seqNo++
			timestamp += 160 // 20 ms @ 8 kHz (identical for all supported codecs)
		}
	}
}

// calleeRtpReader reads RTP packets from the callee leg's UDP socket and
// forwards each audio payload onto the caller leg's packetQueue (legs[0]).
// The caller leg's existing rtpPacer drains packetQueue and re-frames the
// payloads as fresh RTP packets aimed at the caller — completing the
// callee → caller direction of the B2BUA bridge.
//
// Lifecycle: spawned by Leg.OnAnswered() after the outbound 200 OK + ACK
// confirms the callee dialog. Exits when ctx is cancelled (rtpConn.Close
// called by the watchdog goroutine; ReadFromUDP returns an error which we
// recognise as ctx-cancellation and exit silently).
//
// Filtering: only audio payload-type packets are forwarded. Non-audio PTs
// (telephone-event PT 101 or anything else) are dropped silently — DTMF
// is not bridged in the callee → caller direction.
//
// SRTP: the callee leg's SDP offer is plain RTP/AVP (BuildSDPOffer) so the
// callee answers in plain RTP; no decryption needed here.
func (s *CallSession) calleeRtpReader(ctx context.Context, leg *Leg, rtpConn *net.UDPConn) {
	defer s.wg.Done()

	buf := make([]byte, 1500)
	peer := s.primary() // legs[0] is the caller leg; packetQueue feeds its rtpPacer

	for {
		n, _, err := rtpConn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return // session ended; rtpConn was closed by the watchdog
			}
			s.log.Warn().Err(err).Str(observability.FieldSIPCallID, s.callID).Msg("calleeRtpReader: ReadFromUDP error")
			return
		}

		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			// Malformed packet — drop silently.
			continue
		}

		// Drop non-audio packets (DTMF telephone-event etc.). Only PCMU
		// audio is bridged in the callee→caller direction.
		if pkt.PayloadType != leg.audioPT {
			continue
		}

		if s.metrics != nil {
			s.metrics.RTPRx.Inc()
		}

		// Copy payload — buf is reused on the next ReadFromUDP call.
		payload := make([]byte, len(pkt.Payload))
		copy(payload, pkt.Payload)

		if peer == nil || peer.packetQueue == nil {
			// Should not happen post-CloseStream — defensive.
			continue
		}

		select {
		case peer.packetQueue <- outboundFrame{audio: payload}:
		case <-ctx.Done():
			return
		default:
			s.log.Warn().Str(observability.FieldSIPCallID, s.callID).Msg("calleeRtpReader: caller packetQueue full — dropping forwarded audio packet")
		}
	}
}

// calleeRtpPacer drains the callee leg's outboundQueue at 20 ms cadence and
// forwards each audio frame as a fresh RTP packet aimed at the callee. The
// outboundQueue is filled by legs[0].rtpReader's StateForwarding relay branch
// (session.go:631) — completing the caller → callee direction of the B2BUA
// bridge.
//
// Lifecycle: spawned by Leg.OnAnswered() alongside calleeRtpReader. Sends one
// packet per 20 ms tick; falls back to PCMU silence (NAT keepalive + symmetry
// with rtpPacer) when outboundQueue is empty. Exits when ctx is cancelled or
// rtpConn.WriteTo errors after the watchdog closes the socket.
//
// SRTP: callee leg negotiated plain RTP/AVP — no encryption applied here.
func (s *CallSession) calleeRtpPacer(ctx context.Context, leg *Leg, rtpConn *net.UDPConn) {
	defer s.wg.Done()

	if leg.audioPT == 0 && leg.silenceFrame == nil {
		// AcceptSDPAnswer should have populated both. Defensive: synthesize
		// PCMU silence so the pacer can run without panicking.
		leg.silenceFrame = pcmuSilenceFrame()
	}

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
			var frame outboundFrame
			select {
			case frame = <-leg.outboundQueue:
			default:
				// empty queue — silence below
			}

			chunk := frame.audio
			if chunk == nil {
				chunk = leg.silenceFrame
			}

			pkt := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    leg.audioPT,
					SequenceNumber: seqNo,
					Timestamp:      timestamp,
					SSRC:           ssrc,
				},
				Payload: chunk,
			}
			encoded, err := pkt.Marshal()
			if err != nil {
				s.log.Warn().Err(err).Str(observability.FieldSIPCallID, s.callID).Msg("calleeRtpPacer: RTP marshal failed — skipping frame")
				seqNo++
				timestamp += 160
				continue
			}

			if leg.remoteRTP == nil {
				// Should not happen post-AcceptSDPAnswer — defensive.
				seqNo++
				timestamp += 160
				continue
			}

			if _, err := rtpConn.WriteTo(encoded, leg.remoteRTP); err != nil {
				if ctx.Err() != nil {
					return // session ended; rtpConn closed by the watchdog
				}
				s.log.Error().Err(err).Str(observability.FieldSIPCallID, s.callID).Msg("calleeRtpPacer: WriteTo callee failed")
				return
			}
			if s.metrics != nil {
				s.metrics.RTPTx.Inc()
			}
			seqNo++
			timestamp += 160 // 20 ms @ 8 kHz (PCMU)
		}
	}
}

// --- REST-API immutable getters (BridgeCall interface) ---
//
// All getters return value-copies of primitives; they are safe to call from any
// goroutine. They MUST NOT acquire locks or block. The fields they read are
// either set once at StartSession (callID/callSid/from/to/startTime/accountSid)
// or set once by markTerminated() on the StartSession goroutine just before the
// session is removed from CallManager.callSidIdx (endTime/termReason); see
// manager.go's deferred cleanup for the single-writer invariant.

// SetStatusCallback installs (or replaces) the per-call status-callback
// subscription. Idempotent in the sense of "last write wins" — multiple
// REST modify-call POSTs reconfigure cleanly. Storing nil clears the
// subscription so emission hooks (16-05/16-06) no-op.
//
// The terminal emission hook reads the stored config inside markTerminated.
func (s *CallSession) SetStatusCallback(cfg *StatusCallbackConfig) {
	s.statusCallback.Store(cfg)
}

// SetStatusClient injects the per-process *webhook.StatusClient used by
// terminal-event emission. Wired by CallManager at StartSession time so
// each session shares the singleton dispatcher.
//
// Nil-safe: emit helpers (emitTerminalStatusCallback) early-return when
// statusWC is nil, so unit tests that build CallSession by struct literal
// without setting this field continue to work without modification (the
// entire emission path no-ops).
//
// Idempotent: callers may invoke SetStatusClient at any time before
// markTerminated. The StatusClient pointer is single-writer in practice
// (CallManager sets it at session-creation time on the same goroutine
// that subsequently runs the session), so a plain assignment is
// sufficient — no atomic.Pointer is required.
func (s *CallSession) SetStatusClient(sc *webhook.StatusClient) {
	s.statusWC = sc
}

// StatusCallback returns the current subscription or nil. Nil-safe at the
// caller; lifecycle/terminal emit hooks no-op when nil.
func (s *CallSession) StatusCallback() *StatusCallbackConfig {
	return s.statusCallback.Load()
}

// NextSequenceNumber returns the next per-call SequenceNumber (0, 1, 2, ...).
// Race-safe via atomic.Uint64. The standard "Add(1)-1" trick yields a
// monotonic 0-indexed counter without an explicit mutex.
func (s *CallSession) NextSequenceNumber() uint64 {
	return s.seqCounter.Add(1) - 1
}

// SetAnsweredAt records the wall-clock instant of 200 OK + ACK confirm.
// First-write-wins via atomic.Pointer.CompareAndSwap. Used by terminal-event
// emission to compute CallDuration / Duration. Idempotent — second call is
// a no-op so retries on the SIP layer cannot stomp the original timestamp.
func (s *CallSession) SetAnsweredAt(t time.Time) {
	s.answeredAt.CompareAndSwap(nil, &t)
}

// SetSIPFinalCode records the final SIP response code (e.g. 486 / 487 / 408).
// First non-zero write wins via atomic.Int32.CompareAndSwap. Used by terminal-
// event emission to populate SipResponseCode. Calling with code == 0 is a
// no-op (the CAS slot stays open for the first real code).
func (s *CallSession) SetSIPFinalCode(code int) {
	if code == 0 {
		return
	}
	s.sipFinalCode.CompareAndSwap(0, int32(code))
}

// EverEmitted reports whether any customer-visible lifecycle emit has fired
// for this CallSid. Cleanup-on-failure paths (PreRegisterSession's returned
// cleanup closure) read this to gate ghost terminal-only callbacks: a call
// that never reached "initiated" emit must NOT produce a terminal POST when
// its pre-registration is unwound.
func (s *CallSession) EverEmitted() bool { return s.everEmitted.Load() }

// MarkEmitted is called by every emit helper on FIRST successful Enqueue
// (CompareAndSwap from false to true). Idempotent across multiple emits —
// initiated → ringing → answered re-emits all collapse to a single CAS hit.
// Returns true if THIS call was the one that flipped the flag, false if a
// prior call already did so (consistent with sync.atomic.Bool.CompareAndSwap
// semantics; callers are free to ignore the return value).
//
// Production callers:
//   - emitStatusEvent (handler-side, sip package) — first parent-leg lifecycle
//     emit (initiated/ringing/answered) flips this true.
//   - emitTerminalStatusCallback (bridge-side, this package) — defense-in-depth
//     stamping for any code path that reaches terminal emission without a
//     prior lifecycle emission.
func (s *CallSession) MarkEmitted() bool {
	return s.everEmitted.CompareAndSwap(false, true)
}

// CallSid returns the Twilio-style call identifier (CA + 32 hex chars).
func (s *CallSession) CallSid() string { return s.callSid }

// AccountSid returns the synthesized AccountSid (AC + 32 hex chars).
func (s *CallSession) AccountSid() string { return s.accountSid }

// CallID returns the SIP Call-ID header value of the inbound INVITE.
func (s *CallSession) CallID() string { return s.callID }

// From returns the inbound INVITE's From URI string.
func (s *CallSession) From() string { return s.from }

// To returns the inbound INVITE's To URI string.
func (s *CallSession) To() string { return s.to }

// SessionContext returns the session-scoped context. It is cancelled when
// the call terminates (BYE from either side, REST Terminate, etc.). Nil
// before run() initializes — callers must guard. Used by mid-call REST
// async dispatchers (modifyCallHandler) so background goroutines that
// outlive the HTTP request still get cancelled when the call ends.
func (s *CallSession) SessionContext() context.Context { return s.sessionCtx }

// Direction is always "inbound" — only the inbound caller leg is exposed
// here. The B2BUA callee leg is owned by the Forwarder and not surfaced
// through this getter.
func (s *CallSession) Direction() string { return "inbound" }

// StartTime returns the UTC timestamp captured at StartSession.
func (s *CallSession) StartTime() time.Time { return s.startTime }

// EndTime returns the UTC termination timestamp, or the zero value while the
// call is still active (markTerminated has not yet run).
func (s *CallSession) EndTime() time.Time {
	if t := s.terminal.Load(); t != nil {
		return t.endTime
	}
	return time.Time{}
}

// Duration returns the call duration in whole seconds. Returns 0 while the call
// is still active (markTerminated has not yet stamped the terminal snapshot).
func (s *CallSession) Duration() int {
	t := s.terminal.Load()
	if t == nil {
		return 0
	}
	return int(t.endTime.Sub(s.startTime).Seconds())
}

// AnsweredBy is "" today. Future Answering Machine Detection support
// will populate this with "human" / "machine_*".
func (s *CallSession) AnsweredBy() string { return "" }

// ParentCallSid is "" today. The <Dial> outbound callee leg is not
// surfaced here, so there is no paired-leg correlation to expose.
func (s *CallSession) ParentCallSid() string { return "" }

// Status returns the Twilio-shaped call status. Mapping:
//   - termReason set (non-zero endTime): "completed" / "failed" / "busy" / "no-answer" / "canceled"
//   - StateDispatching:                  "queued"
//   - StateStreaming / StateForwardingSetup / StateDialingOut / StateForwarding / StateRedirected: "in-progress"
//   - StateHungUp/StateTerminated:       "completed"
//   - default:                           "queued"
//
// StateDialingOut (transient between WS-down and callee-leg-up) is
// in-progress: the call is still alive from the caller's perspective even
// though the WS path has been closed by CloseStream.
//
// While endTime is zero we always trust the live state.Load() — this avoids
// returning "completed" on a session whose run() has cancelled its context but
// whose StartSession defer chain has not yet stamped endTime.
func (s *CallSession) Status() string {
	if t := s.terminal.Load(); t != nil {
		return twilioStatusFromTermReason(t.termReason)
	}
	switch s.state.Load() {
	case StateDispatching:
		return "queued"
	case StateStreaming, StateForwardingSetup, StateDialingOut, StateForwarding, StateRedirected:
		return "in-progress"
	case StateHungUp, StateTerminated:
		return "completed"
	default:
		return "queued"
	}
}

// twilioStatusFromTermReason maps internal termination reasons to Twilio's
// CallStatus enum values. The internal `termReason` is richer (e.g. "hangup"
// distinguishes API-driven termination from natural caller-initiated hangup),
// but the wire-format `status` field MUST be one of Twilio's documented values:
// queued | ringing | in-progress | completed | busy | failed | no-answer | canceled.
//
// Reasons: "hangup" (Twiml=<Hangup/> mid-call) and "completed" (Status=completed
// REST modify, plus the natural-exit run() path) — both map to Twilio "completed".
// <Dial> outcomes plumb through Terminate(reason) as: "busy" → "busy",
// "no-answer" → "no-answer", "canceled" → "canceled", "failed" → "failed".
func twilioStatusFromTermReason(reason string) string {
	switch reason {
	case "busy":
		return "busy"
	case "no-answer":
		return "no-answer"
	case "canceled":
		return "canceled"
	case "failed":
		return "failed"
	default:
		// "hangup", "completed", "" → Twilio's "completed"
		return "completed"
	}
}

// markTerminated stamps endTime and termReason exactly once over the session
// lifetime. Idempotent via sync.Once: a second call (with any reason) is a
// no-op — the first reason and the first endTime stand.
//
// Concurrency: the public Terminate(reason) method (called from REST
// handler goroutines) calls markTerminated FIRST, so that when run()'s
// defer chain later calls markTerminated("completed") on the natural exit
// path, the sync.Once swallows the second invocation and the REST-driven
// reason wins. The race between two goroutines calling markTerminated is
// also covered — sync.Once guarantees a single execution of the closure
// even under concurrent entry; whichever caller wins the race fixes the
// field values.
//
// Reasons: "completed" (REST Status=completed and natural-exit), "hangup"
// (mid-call <Hangup/> verb), and the <Dial>-outcome reasons "failed",
// "busy", "no-answer", "canceled".
//
// The terminal status-callback emission is invoked from inside the
// terminateOnce.Do closure — single-fire invariant preserved even when
// 3+ Terminate goroutines race (REST + dispatcher + BYE). The emit is
// non-blocking (Enqueue handoff to StatusClient worker goroutine); call
// cleanup (port release, snapshot, map removal) proceeds without waiting
// for HTTP delivery.
func (s *CallSession) markTerminated(reason string) {
	s.terminateOnce.Do(func() {
		endTime := time.Now().UTC()
		s.terminal.Store(&terminalState{
			endTime:    endTime,
			termReason: reason,
		})
		// Defensive default for SipResponseCode. If the SIP layer didn't
		// stamp a final code (e.g. caller-side BYE — natural call end is
		// 200 OK; or CANCEL → 487), stamp the canonical code for the
		// termReason. CompareAndSwap inside SetSIPFinalCode ensures we
		// never overwrite a more-specific code already stamped by
		// handler.go's INVITE-side paths (e.g. 503 on RespondSDP failure).
		// handler.go does NOT stamp SetSIPFinalCode on onCancel or onBye —
		// those paths reach markTerminated via the regular Terminate(reason)
		// flow, and this defensive default closes the gap structurally.
		// Resolution table:
		//   - reason="completed" / "" → 200 (caller hung up cleanly via BYE)
		//   - reason="canceled"       → 487 (caller-side CANCEL)
		//   - reason="busy"           → 486
		//   - reason="no-answer"      → 408
		//   - reason="failed"         → 500
		s.sipFinalCode.CompareAndSwap(0, defaultSIPCodeForReason(reason))
		// Emit terminal status callback exactly once per call.
		s.emitTerminalStatusCallback(reason, endTime)
		// Reclaim the per-CallSid worker goroutine + queue + sync.Map
		// entry. Non-blocking (separate goroutine) so call cleanup is not
		// gated on customer-host responsiveness. The drain budget
		// (webhook.StatusDrainBudget) covers the worst-case retry chain
		// (1+2+4 backoff + 4×4s timeout ≈ 23s) for the just-enqueued
		// terminal event — see webhook/status.go for the canonical
		// definition.
		//
		// Without this drain, DrainAndClose would have zero production
		// callers and per-CallSid resources would leak linearly with call
		// volume. sync.Once collapses concurrent Terminate calls so
		// DrainAndClose only spawns once even if Terminate races with
		// run()'s defer chain.
		if s.statusWC != nil {
			go func(cs string) {
				_ = s.statusWC.DrainAndClose(cs, webhook.StatusDrainBudget)
			}(s.callSid)
		}
	})
}

// defaultSIPCodeForReason returns the canonical SIP final response code for
// a given termination reason. Used by markTerminated as a defensive default
// when the SIP layer didn't stamp a code via SetSIPFinalCode.
//
// The mapping mirrors RFC 3261 + sipgate's observed wire-format:
//   - "canceled"       → 487 (Request Terminated; caller CANCEL before answer)
//   - "busy"           → 486 (Busy Here)
//   - "no-answer"      → 408 (Request Timeout)
//   - "failed"         → 500 (Server Internal Error)
//   - default ("completed" / "hangup" / "") → 200 (OK; natural BYE)
func defaultSIPCodeForReason(reason string) int32 {
	switch reason {
	case "canceled":
		return 487
	case "busy":
		return 486
	case "no-answer":
		return 408
	case "failed":
		return 500
	default:
		// "completed", "hangup", "" → 200 OK
		return 200
	}
}

// emitTerminalStatusCallback builds and Enqueues the Twilio-shape terminal
// status callback. Invoked from inside markTerminated's terminateOnce.Do
// closure — exactly one fire per CallSid regardless of concurrent Terminate
// callers (REST + dispatcher + BYE racing into markTerminated all collapse
// onto a single emission via the existing sync.Once).
//
// No-op (early return) when:
//   - statusWC is nil (deployments and unit tests that don't exercise the
//     emission path)
//   - statusCallback subscription is nil (call has no StatusCallback=
//     subscription installed)
//   - subscription's Events set is non-empty AND does not include the
//     resolved event name (specific match) AND does not include the
//     generic "completed" fallback
//
// Subscription resolution: the Twilio terminal "Event" is usually
// `completed` regardless of the specific CallStatus. Some customers
// subscribe to specific terminal events (`busy`, `no-answer`, `canceled`,
// `failed`); the helper picks the most specific match if present and falls
// back to `completed` if the subscription contains it. An empty Events set
// is treated as "subscribe to all events" (default for REST subscriptions
// where no StatusCallbackEvent is supplied).
//
// Form-field shape: standard Twilio fields PLUS terminal-only additions
// CallDuration / Duration / SipResponseCode (when populated by
// SetAnsweredAt / SetSIPFinalCode at the SIP-handler layer). Both
// terminal-only fields degrade gracefully: a call that never answered
// emits no CallDuration / Duration; an unknown SIP final code emits no
// SipResponseCode.
//
// Lifecycle decoupling: Enqueue is non-blocking; the worker goroutine is
// owned by StatusClient and runs on its own context.Background()-derived
// per-attempt context. Even when the customer's callback host hangs
// forever, markTerminated returns in <100ms — verified by
// TestCallSession_TerminalNotBlockedByCallback.
func (s *CallSession) emitTerminalStatusCallback(reason string, endTime time.Time) {
	if s.statusWC == nil {
		return
	}
	cfg := s.statusCallback.Load()
	if cfg == nil {
		return
	}

	callStatus := twilioStatusFromTermReason(reason)

	// Subscription resolution: subscribe-to-all (empty Events), specific
	// match, or generic-completed REMAP for terminal events. The remap
	// changes the EVENT label only — Form's CallStatus stays as the actual
	// call status (e.g. busy/failed/canceled). handler.go / forwarder.go /
	// session.go share the single canonical helper webhook.ResolveEventName
	// instead of three divergent local interpretations.
	eventName, ok := webhook.ResolveEventName(cfg.Events, callStatus)
	if !ok {
		return // not subscribed to either specific or generic
	}

	seq := s.NextSequenceNumber()

	form := url.Values{}
	form.Set("CallSid", s.callSid)
	form.Set("AccountSid", s.accountSid)
	form.Set("From", s.from)
	form.Set("To", s.to)
	form.Set("Caller", s.from)
	form.Set("Called", s.to)
	form.Set("Direction", s.Direction())
	form.Set("ApiVersion", "2010-04-01")
	form.Set("CallStatus", callStatus)
	// Timestamp MUST equal endTime — the same
	// wall-clock that drives CallDuration below — NOT a fresh time.Now()
	// captured here inside the worker. Two distinct wall-clock reads would
	// drift by ms-to-s if the worker schedules late and would non-determinise
	// tests that assert against a known endTime.
	form.Set("Timestamp", endTime.Format(time.RFC1123Z))
	form.Set("SequenceNumber", strconv.FormatUint(seq, 10))
	form.Set("CallbackSource", "call-progress-events")

	// Terminal-only fields. Populated only when answered.
	if at := s.answeredAt.Load(); at != nil {
		dur := endTime.Sub(*at)
		// Defensive guard against clock-skew: answeredAt > endTime would
		// otherwise leak negative seconds into the form.
		if dur < 0 {
			dur = 0
		}
		secs := int(dur.Seconds())
		mins := int(math.Ceil(dur.Minutes()))
		form.Set("CallDuration", strconv.Itoa(secs))
		form.Set("Duration", strconv.Itoa(mins))
	}
	if code := s.sipFinalCode.Load(); code > 0 {
		form.Set("SipResponseCode", strconv.Itoa(int(code)))
	}

	method := cfg.Method
	if method == "" {
		method = "POST"
	}
	evt := webhook.CallbackEvent{
		URL:    cfg.URL,
		Method: method,
		Form:   form,
		// Event-vocab label for status_callback_attempts_total.
		// eventName is the resolved subscription key — usually equal to
		// callStatus (e.g. "completed"/"busy"/"failed") but may be remapped
		// to "completed" by the generic-completed fallback above.
		Event: eventName,
		// Trusted=true marks operator-supplied default-callback URLs
		// (STATUS_CALLBACK_DEFAULT_URL env) so the SSRF guard at dial
		// time and the pre-flight ValidateCallbackURL at Enqueue are
		// bypassed. Customer URLs (REST POST StatusCallback=) leave
		// cfg.Trusted=false and remain SSRF-guarded.
		Trusted: cfg.Trusted,
	}
	// Defense-in-depth: stamp everEmitted before the terminal Enqueue
	// attempt so any racing cleanup-on-failure path observes a true value
	// (idempotent via CompareAndSwap inside MarkEmitted). In the normal
	// flow this is a no-op because parent-leg lifecycle emits already
	// stamped it via MarkEmitted at the handler-side first-emit gate.
	// Retained here as a safety net for code paths that reach terminal
	// emission without prior lifecycle emission (e.g. early failures
	// stamping SetSIPFinalCode then markTerminated immediately — though
	// those paths are also gated by the cleanup closure's EverEmitted
	// check).
	s.MarkEmitted()
	if err := s.statusWC.Enqueue(s.callSid, evt); err != nil {
		s.log.Warn().
			Err(err).
			Str("event", eventName).
			Str(observability.FieldCallSid, s.callSid).
			Msg("status_callback: terminal Enqueue failed")
	}
}

// IsActive returns true while the session is in a state that the REST modify
// handler (POST /Calls/{Sid}.json) accepts as eligible for in-place
// modification. Returns false once the session has reached StateTerminated
// (mid-call termination) or StateHungUp (caller-initiated BYE). Lock-free:
// relies on AtomicState for safe concurrent reads.
//
// Used by api.modifyCallHandler to distinguish modify-on-active (proceed and
// dispatch the new TwiML chain) from modify-on-terminated (return Twilio
// 21220 — "call is not in-progress").
func (s *CallSession) IsActive() bool {
	st := s.state.Load()
	return st != StateTerminated && st != StateHungUp
}

// Terminate is the public mid-call termination entry point used by the REST
// POST /Calls/{Sid}.json handler (and by the mid-call <Hangup/> verb in plan
// 14-03). Idempotent: a second call on an already-terminated session is a
// no-op at the stamping layer (sync.Once) and at the state layer (CAS misses
// silently). Each leg's BYE is also per-leg sync.Once-guarded inside
// Leg.bye() so concurrent Terminate calls collapse to one BYE per leg.
//
// Order:
//  1. markTerminated stamps endTime + reason FIRST so that any concurrent
//     run()-defer-chain markTerminated("completed") loses the sync.Once race
//     and the REST-driven reason survives into the recentlyTerminated
//     snapshot.
//  2. state.CAS for each possible source state, advancing to StateTerminated.
//     A failed CAS is non-fatal — a different concurrent caller may already
//     have moved the state, but every CAS attempt covers a different source
//     state so at least one of {Streaming, DialingOut, Forwarding} hits.
//  3. Cancel session ctx so run() exits its select loop. run()'s defer chain
//     then runs naturally; its markTerminated call is the no-op.
//  4. Send SIP BYE to BOTH legs (legs[0] always; legs[1] when present from
//     <Dial>). Per-leg sync.Once inside Leg.bye() collapses dup
//     calls. 5s budget per leg is generous enough for sipgate's median
//     BYE-200 RTT (~50ms) but bounded so REST handlers do not stall.
//
// Callable from any goroutine — does NOT touch s.wg, s.rtpConn, s.wsConn
// directly. All cleanup happens in run()'s defer chain (or in StartSession's
// outer defer chain when the session is removed from callSidIdx and the
// terminatedCall snapshot is built).
//
// Returns the FIRST BYE error encountered across all legs (nil if all
// succeeded). Callers may safely ignore it — the session is logically
// terminated regardless of whether the BYE transactions succeeded; sipgate
// observation is that BYE is best-effort in practice.
func (s *CallSession) Terminate(reason string) error {
	// Step 1: stamp termReason / endTime first (before run() defer can race us)
	s.markTerminated(reason)

	// Step 2: state visibility for concurrent IsActive() / Status() readers.
	// CAS may fail when state already advanced — try every plausible source
	// state so the transition lands regardless of where the state machine
	// currently sits. The stamping in step 1 was already performed atomically
	// by sync.Once.
	s.state.CAS(StateStreaming, StateTerminated)
	s.state.CAS(StateDialingOut, StateTerminated)
	s.state.CAS(StateForwarding, StateTerminated)

	// Step 3: signal run() to exit — it will run its defer chain naturally.
	// nil-guard covers the early-Terminate-before-run() edge case (also
	// exercised in the unit test suite). Calling cancel on an already-
	// canceled context is a documented no-op in the stdlib.
	if s.cancel != nil {
		s.cancel()
	}

	// Step 4: BYE every leg (best-effort; sipgo handles the transaction).
	// Per-leg sync.Once inside Leg.bye() means duplicate calls collapse —
	// callable from any goroutine without coordinating with the dispatcher.
	// legs[1] is set after a successful <Dial> 200 OK. Iterate
	// under legsMu (read lock) so SetLeg races are race-detector clean.
	s.legsMu.RLock()
	legsSnapshot := make([]*Leg, len(s.legs))
	copy(legsSnapshot, s.legs)
	s.legsMu.RUnlock()

	var firstErr error
	for _, leg := range legsSnapshot {
		if leg == nil {
			continue
		}
		byeCtx, byeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := leg.bye(byeCtx); err != nil && firstErr == nil {
			firstErr = err
		}
		byeCancel()
	}
	return firstErr
}
