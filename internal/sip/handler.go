package sip

import (
	"github.com/emiago/sipgo"
	siplib "github.com/emiago/sipgo/sip"
	"github.com/rs/zerolog"
	"github.com/sipgate/audio-dock/internal/config"
)

// CallManagerIface is satisfied by bridge.CallManager (defined in 06-02).
// Defined here to avoid circular import (sip package would otherwise import bridge).
type CallManagerIface interface {
	AcquirePort() (int, error)
	ReleasePort(port int)
	StartSession(dlg *sipgo.DialogServerSession, req *siplib.Request, callerSDP *CallerSDP, rtpPort int, log zerolog.Logger)
}

// Handler manages inbound SIP dialog state using sipgo.DialogServerCache.
// Register it via NewHandler before calling registrar.Register() in main.go.
type Handler struct {
	dialogSrv   *sipgo.DialogServerCache
	callManager CallManagerIface
	cfg         config.Config
	log         zerolog.Logger
}

// NewHandler creates the Handler, wires it to agent.Server, and returns the ready handler.
// Must be called BEFORE registrar.Register() so INVITE handling is active from first registration.
func NewHandler(agent *Agent, callManager CallManagerIface, cfg config.Config, log zerolog.Logger) *Handler {
	contact := siplib.ContactHeader{
		Address: siplib.Uri{
			User: cfg.SIPUser,
			Host: cfg.SDPContactIP,
			Port: 5060,
		},
	}
	dialogSrv := sipgo.NewDialogServerCache(agent.Client, contact)

	h := &Handler{
		dialogSrv:   dialogSrv,
		callManager: callManager,
		cfg:         cfg,
		log:         log,
	}

	// Register handlers BEFORE Register() is called in main.go
	agent.Server.OnInvite(h.onInvite)
	agent.Server.OnAck(h.onAck)
	agent.Server.OnBye(h.onBye)
	agent.Server.OnOptions(h.onOptions)

	return h
}

// onInvite handles inbound INVITE requests following the UAS dialog flow:
// ReadInvite → ParseCallerSDP → AcquirePort → 100 Trying → 200 OK+SDP → StartSession goroutine.
func (h *Handler) onInvite(req *siplib.Request, tx siplib.ServerTransaction) {
	log := h.log.With().
		Str("call_id", req.CallID().Value()).
		Str("from", req.From().Address.String()).
		Str("to", req.To().Address.String()).
		Logger()

	// ReadInvite MUST be called first — creates dialog context and To-tag
	dlg, err := h.dialogSrv.ReadInvite(req, tx)
	if err != nil {
		log.Error().Err(err).Msg("ReadInvite failed")
		_ = tx.Respond(siplib.NewResponseFromRequest(req, 500, "Server Error", nil))
		return
	}
	defer dlg.Close() // always cleanup from DialogServerCache

	// Parse caller's SDP offer to extract RTP addr/port and DTMF PT
	callerSDP, err := ParseCallerSDP(req.Body())
	if err != nil {
		log.Error().Err(err).Msg("SDP parse failed — rejecting INVITE")
		_ = dlg.Respond(503, "Service Unavailable", nil)
		return
	}

	// Acquire RTP port — reject with 503 if pool exhausted (SIP-04)
	rtpPort, err := h.callManager.AcquirePort()
	if err != nil {
		log.Warn().Err(err).Msg("RTP port pool exhausted — rejecting INVITE")
		_ = dlg.Respond(503, "Service Unavailable", nil)
		return
	}

	// 100 Trying — suppresses INVITE retransmissions from the proxy
	_ = dlg.Respond(100, "Trying", nil)

	// Build SDP answer: our contact IP + acquired RTP port + mirrored DTMF PT (never hardcoded)
	sdpAnswer := BuildSDPAnswer(h.cfg.SDPContactIP, rtpPort, callerSDP.DTMFPayloadType)

	// 200 OK with SDP body — establishes the dialog (caller will send ACK)
	if err := dlg.RespondSDP(sdpAnswer); err != nil {
		log.Error().Err(err).Msg("RespondSDP 200 OK failed")
		h.callManager.ReleasePort(rtpPort) // return port to pool on error path (Pitfall 4)
		return
	}

	// Launch call session in goroutine so onInvite returns and does not block the next INVITE.
	// dlg.Context() is cancelled when: (a) caller sends BYE → onBye/ReadBye, (b) we call dlg.Bye().
	// Port release and all other cleanup happen inside StartSession (via bridge.CallManager).
	go h.callManager.StartSession(dlg, req, callerSDP, rtpPort, log)
}

// onAck routes the ACK to the correct dialog, transitioning it to DialogStateConfirmed.
func (h *Handler) onAck(req *siplib.Request, tx siplib.ServerTransaction) {
	if err := h.dialogSrv.ReadAck(req, tx); err != nil {
		h.log.Error().Err(err).Str("call_id", req.CallID().Value()).Msg("ReadAck failed")
	}
}

// onBye routes the BYE to the correct dialog, sends 200 OK, and cancels dlg.Context().
// The session goroutine launched in onInvite will exit when dlg.Context().Done() fires.
func (h *Handler) onBye(req *siplib.Request, tx siplib.ServerTransaction) {
	if err := h.dialogSrv.ReadBye(req, tx); err != nil {
		h.log.Error().Err(err).Str("call_id", req.CallID().Value()).Msg("ReadBye failed")
	}
}

// onOptions sends a 200 OK for SIP OPTIONS keepalive probes (no dialog needed).
func (h *Handler) onOptions(req *siplib.Request, tx siplib.ServerTransaction) {
	_ = tx.Respond(siplib.NewResponseFromRequest(req, 200, "OK", nil))
}
