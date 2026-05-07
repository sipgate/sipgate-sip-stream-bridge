package bridge

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/pion/sdp/v3"
)

// Leg represents one SIP dialog + its RTP socket. v3.0 single-leg paths
// use legs[0] only (the inbound caller leg). The dual-leg path adds
// legs[1] (the B2BUA callee leg dialed by the <Dial> verb).
//
// Field ownership:
//   - dlg, rtpPort, remoteRTP, dtmfPT, audioPT, silenceFrame: per-leg SIP/RTP
//     state acquired from the SDP offer/answer for this leg.
//   - mediaEncoding, mediaSampleRate: Twilio mediaFormat values derived from
//     the negotiated audio payload type for this leg.
//   - localSRTPKey/Salt, remoteSRTPKey/Salt: SRTP master keys for this leg's
//     RTP socket. nil when SRTP was not negotiated for this leg.
//   - packetQueue, rtpInboundQueue: the two RTP-side channels for this leg.
//     Single-leg paths use only legs[0]'s channels; the dual-leg path adds
//     legs[1]'s pair.
//   - lastDtmfTS: RFC 4733 dedup timestamp tracked per leg.
//
// Dual-leg additions:
//   - terminateOnce: per-leg sync.Once around bye(). Replaces session-level
//     terminateOnce (which guarded markTerminated, not bye) so that a
//     BYE-from-callee race vs a Terminate-from-API never double-BYEs either
//     side; each leg's bye() is at-most-once.
//   - outboundQueue: RTP-relay queue filled by the peer leg's rtpReader and
//     drained by this leg's rtpPacer (when state == StateForwarding).
//   - sdpOffer / sdpAnswer: outbound-leg SDP buffers populated by
//     BuildSDPOffer / AcceptSDPAnswer; legs[0] (inbound) leaves these nil.
//   - isOutbound: true for legs[1]; lets callers fast-path the relay
//     direction without index introspection.
//   - contactIP: the externally-reachable host IP used when constructing the
//     outbound SDP offer's c= and o= lines. Populated by the Forwarder before
//     calling BuildSDPOffer (wired from cfg.SDPContactIP).
type Leg struct {
	dlg             *sipgo.DialogServerSession // inbound only
	rtpPort         int
	remoteRTP       net.Addr // RENAMED from callerRTP — generalizes to inbound-or-outbound peer
	dtmfPT          uint8
	audioPT         uint8
	silenceFrame    []byte
	mediaEncoding   string
	mediaSampleRate int
	localSRTPKey    []byte
	localSRTPSalt   []byte
	remoteSRTPKey   []byte
	remoteSRTPSalt  []byte
	packetQueue     chan outboundFrame // RTP write side (WS→RTP for legs[0]; relay-target for legs[1])
	rtpInboundQueue chan []byte        // RTP read side (RTP→WS)
	lastDtmfTS      uint32

	// terminateOnce guards bye() so each leg sees at most one BYE in its
	// lifetime. The dual-leg world has multiple BYE entry points —
	// REST Terminate, mid-call <Hangup/>, peer-side BYE detection — and any
	// pair can race; sync.Once collapses concurrent calls to a single
	// dlg.Bye() invocation per leg. See bye() for the dispatch logic.
	terminateOnce sync.Once

	// outboundQueue is the RTP-relay channel for B2BUA forwarding. When the
	// session is in StateForwarding, the peer leg's rtpReader pushes received
	// audio frames here and this leg's rtpPacer drains them (instead of the
	// WS-fed packetQueue used in StateStreaming). nil for legs that never
	// participate in a forwarded call.
	outboundQueue chan outboundFrame

	// sdpOffer / sdpAnswer hold the outbound-leg's SDP bytes for diagnostic
	// inspection and verification. Production code reads them only via the
	// LegConfigurer interface; tests assert codec validation.
	sdpOffer  []byte
	sdpAnswer []byte

	// isOutbound is true for the callee leg created by the <Dial> Forwarder
	// (legs[1]). The inbound caller leg (legs[0]) leaves this false.
	isOutbound bool

	// contactIP is the externally-reachable IP advertised in the outbound SDP
	// offer's c= / o= lines. The Forwarder populates this from
	// cfg.SDPContactIP before calling BuildSDPOffer.
	contactIP string

	// byeFunc, when non-nil, replaces the call to dlg.Bye in the
	// CallSession.Terminate() path. Production code never sets this — dlg.Bye
	// is invoked directly via the bye() helper below. The field exists solely
	// so that bridge package tests (session_terminate_test.go) can construct
	// a *Leg with a stubbed BYE that does not require a real
	// *sipgo.DialogServerSession (which carries a SIP transaction graph that
	// is impractical to fake). See bye() for the precedence rule.
	byeFunc func(ctx context.Context) error

	// session is the parent CallSession backref. Wired by SetLeg so that
	// per-leg hooks (OnAnswered) can reach session-scoped resources: state
	// machine, sessionCtx, wg, log, peer leg's packetQueue. Nil for legs
	// constructed in unit tests that do not exercise OnAnswered. Production
	// code wires this automatically — callers never set it directly.
	session *CallSession
}

// LegConfigurer is the narrow interface the SIP <Dial> Forwarder needs from a
// callee Leg. *Leg satisfies it. The interface is defined inside the bridge
// package so that the sip package can import it without taking a dependency
// on concrete bridge types — a sip → bridge import would create a cycle
// through bridge → sip and the existing CallManagerIface defined in
// internal/sip/handler.go. The Forwarder accepts bridge.LegConfigurer in its
// constructor; production code wires it with a *bridge.Leg.
type LegConfigurer interface {
	// BuildSDPOffer constructs an SDP offer for the outbound INVITE.
	// Returns the SDP bytes, the local RTP port that will receive media, and
	// any error. The implementation captures the offer for inspection via
	// SDPOffer(). Invariant: the offer is PCMU-only (PT 0) plus
	// telephone-event PT 101 — this matches the v3.0 audio-mode "twilio".
	BuildSDPOffer() (sdp []byte, rtpPort int, err error)

	// AcceptSDPAnswer parses the callee's SDP answer, validates the
	// negotiated codec is PCMU (PT 0) — anything else returns
	// ErrCodecMismatch — and stores the answer + remote RTP destination on
	// the leg. The Forwarder calls this exactly once after the outbound 200
	// OK arrives.
	AcceptSDPAnswer(answerSDP []byte) error

	// RTPLocalPort returns the UDP port the RTP socket listens on for this
	// leg. Set by the Forwarder via Leg.rtpPort BEFORE calling BuildSDPOffer
	// (the Forwarder owns the AcquirePort/ReleasePort lifecycle so the bridge
	// package never leaks a port if the outbound INVITE fails).
	RTPLocalPort() int

	// OnAnswered is the dual-leg activation hook. Called by the Forwarder
	// exactly once, immediately after the outbound 200 OK has been ACK'd, to
	// transition the bridge from StateDialingOut to StateForwarding.
	// Implementation responsibilities:
	//  1. Open the UDP socket on RTPLocalPort() — this is the local end of
	//     the callee RTP path, advertised in the SDP offer.
	//  2. Spawn the callee→caller and caller→callee RTP relay goroutines
	//     bridging legs[0].packetQueue ↔ legs[1].outboundQueue.
	//  3. CAS the session state from DialingOut to Forwarding so legs[0]'s
	//     rtpReader switches its enqueue branch from rtpInboundQueue (WS)
	//     to legs[1].outboundQueue (relay).
	// Returns an error if the socket cannot be opened or required leg
	// state (remoteRTP, rtpPort) is missing — in which case the caller
	// should BYE the dialog and surface a "failed" DialResult.
	OnAnswered() error
}

// ErrCodecMismatch is returned by AcceptSDPAnswer when the callee's
// negotiated codec is not PCMU. The current end-to-end codec is PCMU only;
// transcoding is out of scope for this release.
var ErrCodecMismatch = fmt.Errorf("bridge: callee SDP answer negotiated non-PCMU codec (codec_mismatch)")

// BuildSDPOffer constructs an SDP offer for an outbound INVITE on this leg.
// The offer advertises PCMU (PT 0) as the only audio codec plus
// telephone-event PT 101 for DTMF, matching the v3.0 audio-mode "twilio".
//
// Pre-conditions:
//   - l.rtpPort must be set by the caller (the Forwarder acquires it from
//     CallManager.AcquirePort BEFORE calling BuildSDPOffer; we deliberately
//     do not call AcquirePort here so the Forwarder controls the
//     acquire/release lifecycle).
//   - l.contactIP must be set to the externally-reachable host IP
//     (cfg.SDPContactIP). When empty, BuildSDPOffer returns an error rather
//     than emitting an unroutable c= line.
//
// Side effect: l.sdpOffer is captured for downstream inspection.
//
// Implementation note: this builder is inlined here (rather than reusing
// sip.BuildSDPAnswer) because the offer-side has different requirements —
// no caller-driven payload-type negotiation, fixed PCMU, no SRTP a=crypto.
// A future refactor may extract a shared internal/sdpbuilder package; for
// now the duplication is small and the boundary clean.
func (l *Leg) BuildSDPOffer() ([]byte, int, error) {
	if l.contactIP == "" {
		return nil, 0, fmt.Errorf("bridge: BuildSDPOffer requires non-empty contactIP")
	}
	if l.rtpPort <= 0 {
		return nil, 0, fmt.Errorf("bridge: BuildSDPOffer requires positive rtpPort, got %d", l.rtpPort)
	}

	now := uint64(time.Now().UnixNano())
	sd := &sdp.SessionDescription{
		Version: 0,
		Origin: sdp.Origin{
			Username:       "-",
			SessionID:      now,
			SessionVersion: now,
			NetworkType:    "IN",
			AddressType:    "IP4",
			UnicastAddress: l.contactIP,
		},
		SessionName: sdp.SessionName("sipgate-sip-stream-bridge"),
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN",
			AddressType: "IP4",
			Address:     &sdp.Address{Address: l.contactIP},
		},
		TimeDescriptions: []sdp.TimeDescription{{Timing: sdp.Timing{}}},
	}

	audio := &sdp.MediaDescription{
		MediaName: sdp.MediaName{
			Media:   "audio",
			Port:    sdp.RangedPort{Value: l.rtpPort},
			Protos:  []string{"RTP", "AVP"},
			Formats: []string{"0", "101"}, // PCMU, telephone-event
		},
	}
	audio.WithCodec(0, "PCMU", 8000, 1, "")
	audio.WithCodec(101, "telephone-event", 8000, 1, "0-16")
	audio.WithPropertyAttribute("sendrecv")
	sd.MediaDescriptions = append(sd.MediaDescriptions, audio)

	out, err := sd.Marshal()
	if err != nil {
		return nil, 0, fmt.Errorf("bridge: SDP marshal: %w", err)
	}
	l.sdpOffer = out
	return out, l.rtpPort, nil
}

// AcceptSDPAnswer parses the callee's SDP answer, validates that the
// negotiated codec is PCMU (PT 0), and stores the answer + remote RTP
// destination on the leg.
//
// Returns ErrCodecMismatch when the answer's first audio payload type is
// non-zero (anything other than PCMU). Returns a wrapping error on parse
// failure or when no audio media section is present.
//
// Side effects on success:
//   - l.sdpAnswer = a copy of the raw answer bytes (for inspection)
//   - l.remoteRTP = the callee's RTP destination (UDPAddr)
//   - l.audioPT   = 0 (PCMU)
//   - l.dtmfPT    = the negotiated telephone-event PT (defaults to 101)
//   - l.silenceFrame = PCMU silence (160 × 0xFF)
//   - l.mediaEncoding / l.mediaSampleRate = "audio/x-mulaw" / 8000
func (l *Leg) AcceptSDPAnswer(answerSDP []byte) error {
	sd := &sdp.SessionDescription{}
	if err := sd.Unmarshal(answerSDP); err != nil {
		return fmt.Errorf("bridge: SDP answer unmarshal: %w", err)
	}

	for _, md := range sd.MediaDescriptions {
		if md.MediaName.Media != "audio" {
			continue
		}

		ip := ""
		if md.ConnectionInformation != nil {
			ip = md.ConnectionInformation.Address.Address
		} else if sd.ConnectionInformation != nil {
			ip = sd.ConnectionInformation.Address.Address
		}
		if ip == "" {
			return fmt.Errorf("bridge: SDP answer missing connection address")
		}

		port := md.MediaName.Port.Value
		if port <= 0 {
			return fmt.Errorf("bridge: SDP answer has non-positive RTP port %d", port)
		}

		// Walk format list: separate audio PTs from telephone-event.
		var audioPTs []uint8
		var dtmfPT uint8 = 101 // fallback to conventional value
		for _, fmtStr := range md.MediaName.Formats {
			pt, err := strconv.ParseUint(fmtStr, 10, 8)
			if err != nil {
				continue
			}
			codec, err := sd.GetCodecForPayloadType(uint8(pt))
			if err != nil {
				// No rtpmap for this PT — fall back to static-PT semantics.
				// PT 0 is statically defined as PCMU/8000 by RFC 3551.
				if pt == 0 {
					audioPTs = append(audioPTs, 0)
				}
				continue
			}
			if strings.EqualFold(codec.Name, "telephone-event") {
				dtmfPT = uint8(pt)
			} else {
				audioPTs = append(audioPTs, uint8(pt))
			}
		}

		if len(audioPTs) == 0 {
			return fmt.Errorf("bridge: SDP answer has no audio payload types")
		}
		if audioPTs[0] != 0 {
			return fmt.Errorf("%w: got PT %d", ErrCodecMismatch, audioPTs[0])
		}

		// Commit on the leg.
		l.sdpAnswer = make([]byte, len(answerSDP))
		copy(l.sdpAnswer, answerSDP)
		l.remoteRTP = &net.UDPAddr{IP: net.ParseIP(ip), Port: port}
		l.audioPT = 0
		l.dtmfPT = dtmfPT
		l.silenceFrame = pcmuSilenceFrame()
		l.mediaEncoding = "audio/x-mulaw"
		l.mediaSampleRate = 8000
		return nil
	}
	return fmt.Errorf("bridge: SDP answer has no audio media section")
}

// RTPLocalPort returns the UDP port this leg's RTP socket listens on.
// Returns 0 before the Forwarder has populated rtpPort.
func (l *Leg) RTPLocalPort() int { return l.rtpPort }

// OnAnswered is the dual-leg activation hook called by the Forwarder once
// the outbound 200 OK has been ACK'd. It opens the local UDP socket on
// l.rtpPort, spawns the two RTP-relay goroutines (callee→caller via
// legs[0].packetQueue; caller→callee via l.outboundQueue), and CAS-es the
// session state to StateForwarding so legs[0]'s rtpReader switches to the
// relay branch.
//
// Pre-conditions:
//   - l.session must be set (wired by SetLeg).
//   - l.rtpPort must be > 0 (set during NewCalleeLeg / BuildSDPOffer).
//   - l.remoteRTP must be non-nil (set by AcceptSDPAnswer from the 200 OK SDP).
//   - l.audioPT/silenceFrame must be set (also by AcceptSDPAnswer).
//
// Lifecycle: the goroutines exit when the session ctx is cancelled. A
// watchdog goroutine closes the rtpConn on ctx.Done() so the blocking
// ReadFromUDP in calleeRtpReader unblocks and returns. The watchdog itself
// is not tracked by s.wg — it is only responsible for releasing the socket.
//
// Idempotent: a second call after the first succeeds is a no-op (the state
// CAS will miss because state is no longer StateDialingOut). Callers
// (Forwarder.Dial) invoke this exactly once on the success path.
func (l *Leg) OnAnswered() error {
	if l.session == nil {
		return fmt.Errorf("bridge: OnAnswered: leg has no session backref (SetLeg not called?)")
	}
	if l.rtpPort <= 0 {
		return fmt.Errorf("bridge: OnAnswered: rtpPort not set (got %d)", l.rtpPort)
	}
	if l.remoteRTP == nil {
		return fmt.Errorf("bridge: OnAnswered: remoteRTP not set (AcceptSDPAnswer not called?)")
	}

	rtpConn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: l.rtpPort})
	if err != nil {
		return fmt.Errorf("bridge: OnAnswered: ListenUDP on port %d: %w", l.rtpPort, err)
	}

	s := l.session

	// Watchdog: close the socket when the session ends so the relay
	// goroutines unblock from their socket reads/writes. Not tracked by
	// s.wg — its only job is to release the socket; the relay goroutines
	// handle their own ctx.Done() observation.
	go func() {
		<-s.sessionCtx.Done()
		_ = rtpConn.Close()
	}()

	s.wg.Add(2)
	go s.calleeRtpReader(s.sessionCtx, l, rtpConn)
	go s.calleeRtpPacer(s.sessionCtx, l, rtpConn)

	// State transition: DialingOut → Forwarding. Once this lands, legs[0]'s
	// rtpReader switches its enqueue branch from rtpInboundQueue (the
	// WS-bound path) to peer.outboundQueue (the relay path drained by
	// calleeRtpPacer). CAS-miss is non-fatal — Terminate may have already
	// transitioned us out (e.g. caller hung up during ACK).
	s.state.CAS(StateDialingOut, StateForwarding)

	s.log.Info().
		Str("call_id", s.callID).
		Int("local_rtp_port", l.rtpPort).
		Str("remote_rtp", l.remoteRTP.String()).
		Msg("bridge: callee leg active — RTP relay started")

	return nil
}

// SDPOffer returns the most recently built SDP offer bytes (set by
// BuildSDPOffer). Returns nil for legs that never had an offer constructed —
// notably legs[0] (the inbound caller leg, whose SDP is offer-driven by the
// caller).
func (l *Leg) SDPOffer() []byte { return l.sdpOffer }

// SDPAnswer returns the most recently accepted SDP answer bytes (set by
// AcceptSDPAnswer). Returns nil before AcceptSDPAnswer has run.
func (l *Leg) SDPAnswer() []byte { return l.sdpAnswer }

// pcmuSilenceFrame returns a fresh 160-byte PCMU silence frame (μ-law silence
// is 0xFF). Allocated per call so the caller cannot mutate a shared backing
// array. The bridge.manager has its own SilenceFrameForPT path for the
// inbound leg via the sip package; the outbound leg builds its own here to
// keep the leg.go file self-contained for outbound construction.
func pcmuSilenceFrame() []byte {
	b := make([]byte, 160)
	for i := range b {
		b[i] = 0xFF
	}
	return b
}

// bye dispatches a SIP BYE for this leg, guarded by terminateOnce so each
// leg sees at most one BYE in its lifetime. Production callers always end up
// in dlg.Bye(ctx); test fixtures override via byeFunc. nil-dlg AND
// nil-byeFunc returns nil (defensive — keeps Terminate panic-free during the
// pre-StartSession unit-test fixtures that build a Leg without a real
// dialog).
//
// Invariant: per-leg sync.Once. A BYE-from-callee racing with a
// Terminate-from-API on the same leg collapses to one dlg.Bye(ctx). The
// session-level Terminate iterates legs and calls leg.bye(ctx) on each;
// duplicate Terminate calls on the session collapse at the leg level
// because terminateOnce has already fired.
//
// COMPATIBILITY NOTE: per-leg sync.Once intentionally collapses two
// Terminate() calls to byeCount == 1 — a stricter invariant than the
// previous unconditional invocation.
func (l *Leg) bye(ctx context.Context) error {
	var firstErr error
	l.terminateOnce.Do(func() {
		switch {
		case l.byeFunc != nil:
			firstErr = l.byeFunc(ctx)
		case l.dlg != nil:
			firstErr = l.dlg.Bye(ctx)
		}
	})
	return firstErr
}
