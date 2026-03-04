package bridge

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	siplib "github.com/emiago/sipgo/sip"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/sipgate/audio-dock/internal/config"
	sip "github.com/sipgate/audio-dock/internal/sip"
)

// PortPool is a bounded, channel-based RTP port pool.
// Acquire/Release are O(1) and lock-free (select on buffered channel).
// Acquire is non-blocking — returns error immediately when pool is exhausted (SIP-04).
type PortPool struct {
	ports chan int
}

// NewPortPool creates a PortPool pre-loaded with ports [min, max).
// Returns an error if min >= max (invalid range).
func NewPortPool(min, max int) (*PortPool, error) {
	if min >= max {
		return nil, fmt.Errorf("RTP_PORT_MIN (%d) must be less than RTP_PORT_MAX (%d)", min, max)
	}
	ch := make(chan int, max-min)
	for p := min; p < max; p++ {
		ch <- p
	}
	return &PortPool{ports: ch}, nil
}

// Acquire is non-blocking — returns error if pool is exhausted (SIP-04).
func (p *PortPool) Acquire() (int, error) {
	select {
	case port := <-p.ports:
		return port, nil
	default:
		return 0, fmt.Errorf("RTP port pool exhausted")
	}
}

// Release returns a port to the pool. Call via defer in StartSession.
func (p *PortPool) Release(port int) {
	select {
	case p.ports <- port:
	default:
		// programming error: port was never acquired or released twice — drop silently
	}
}

// CallManager stores active call sessions and delegates port management to PortPool.
// Satisfies the sip.CallManagerIface defined in internal/sip/handler.go.
type CallManager struct {
	sessions sync.Map   // key: callID string → value: *CallSession
	portPool *PortPool
	cfg      config.Config
	log      zerolog.Logger
}

// NewCallManager creates a CallManager with the given port pool, config, and logger.
func NewCallManager(portPool *PortPool, cfg config.Config, log zerolog.Logger) *CallManager {
	return &CallManager{
		portPool: portPool,
		cfg:      cfg,
		log:      log,
	}
}

// AcquirePort delegates to portPool.Acquire().
func (m *CallManager) AcquirePort() (int, error) {
	return m.portPool.Acquire()
}

// ReleasePort delegates to portPool.Release().
func (m *CallManager) ReleasePort(port int) {
	m.portPool.Release(port)
}

// StartSession dials WS, negotiates the SIP dialog (180 Ringing → 200 OK+SDP), creates a
// CallSession, and runs it synchronously. Must be called in a goroutine (launched by onInvite).
// Blocks until the call ends. Port is released on all exit paths via defer.
func (m *CallManager) StartSession(
	dlg *sipgo.DialogServerSession,
	req *siplib.Request,
	callerSDP *sip.CallerSDP,
	rtpPort int,
	log zerolog.Logger,
) {
	callID := req.CallID().Value()
	// CON-02: always release port — covers WS dial failure, RespondSDP failure, and call end.
	defer m.portPool.Release(rtpPort)

	// streamSid: MZ + 32 hex chars (Twilio Media Streams convention)
	// callSidToken: CA + 32 hex chars (Twilio callSid convention, distinct from SIP Call-ID)
	streamSid := "MZ" + strings.ReplaceAll(uuid.New().String(), "-", "")
	callSidToken := "CA" + strings.ReplaceAll(uuid.New().String(), "-", "")

	callerRTPAddr := &net.UDPAddr{
		IP:   net.ParseIP(callerSDP.RTPAddr),
		Port: callerSDP.RTPPort,
	}

	// Dial WS before answering — reject with 503 if WS is unavailable (WSR-01).
	// Use context.Background() (not dlg.Context()) so a CANCEL from the caller before
	// 200 OK doesn't immediately abort the WS dial. dlg.Context() cancels on
	// DialogStateEnded which can happen if the caller sends CANCEL during this window.
	wsCtx, wsCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer wsCancel()
	wsConn, err := dialWS(wsCtx, m.cfg.WSTargetURL)
	if err != nil {
		log.Error().Err(err).Str("call_id", callID).Msg("dialWS failed — sending 503 (WSR-01)")
		_ = dlg.Respond(503, "Service Unavailable", nil)
		return
	}

	// Build SDP answer: our contact IP + acquired RTP port + mirrored DTMF PT (never hardcoded).
	sdpAnswer := sip.BuildSDPAnswer(m.cfg.SDPContactIP, rtpPort, callerSDP.DTMFPayloadType)

	// 200 OK with SDP — answers the call (caller will send ACK).
	if err := dlg.RespondSDP(sdpAnswer); err != nil {
		log.Error().Err(err).Str("call_id", callID).Msg("RespondSDP 200 OK failed")
		_ = wsConn.Close()
		return
	}

	session := &CallSession{
		callID:       callID,
		callSidToken: callSidToken,
		streamSid:    streamSid,
		dlg:          dlg,
		rtpPort:      rtpPort,
		callerRTP:    callerRTPAddr,
		dtmfPT:       callerSDP.DTMFPayloadType,
		cfg:          m.cfg,
		log:          log,
	}

	m.sessions.Store(callID, session)
	defer m.sessions.Delete(callID)

	session.run(dlg.Context(), wsConn)
}
