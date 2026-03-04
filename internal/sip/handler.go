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
	agent.Server.OnCancel(h.onCancel)
	agent.Server.OnOptions(h.onOptions)

	return h
}

// onInvite handles inbound INVITE requests following the UAS dialog flow:
// ReadInvite → ParseCallerSDP → AcquirePort → 100 Trying → 180 Ringing → StartSession goroutine.
// StartSession dials WS, builds SDP answer, sends 200 OK, then runs the bridge.
func (h *Handler) onInvite(req *siplib.Request, tx siplib.ServerTransaction) {
	log := h.log.With().
		Str("call_id", req.CallID().Value()).
		Str("from", req.From().Address.String()).
		Str("to", req.To().Address.String()).
		Logger()

	// ReadInvite MUST be called first — creates dialog context and To-tag.
	// NOTE: do NOT defer dlg.Close() here — that removes the dialog from DialogServerCache
	// immediately when onInvite returns, making MatchDialogRequest fail for ACK and BYE.
	// ReadBye already calls dlg.Close() internally via its own defer.
	dlg, err := h.dialogSrv.ReadInvite(req, tx)
	if err != nil {
		log.Error().Err(err).Msg("ReadInvite failed")
		_ = tx.Respond(siplib.NewResponseFromRequest(req, 500, "Server Error", nil))
		return
	}

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

	// 100 Trying — suppresses INVITE retransmissions from the proxy.
	// Built manually and sent via tx.Respond to avoid sipgo copying Record-Route
	// headers from the INVITE into provisional responses. Record-Route in 180/100
	// causes some SIP clients (softphones, mobile) to immediately send CANCEL.
	trying := siplib.NewResponseFromRequest(req, 100, "Trying", nil)
	trying.RemoveHeader("Record-Route")
	_ = tx.Respond(trying)

	// 180 Ringing — signals caller that we are alerting before answer.
	// Same: strip Record-Route and send without Contact (no early dialog).
	ringing := siplib.NewResponseFromRequest(req, 180, "Ringing", nil)
	ringing.RemoveHeader("Record-Route")
	_ = tx.Respond(ringing)

	// Launch StartSession in goroutine — dials WS, builds SDP, sends 200 OK, then bridges.
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

// onCancel handles CANCEL requests. sipgo automatically sends 200 OK to CANCEL + 487 to
// the matching INVITE at the transaction layer (before this handler is called). This handler
// is only reached for CANCEL retransmissions that arrive after the original INVITE transaction
// is already gone. We respond 200 OK to stop retransmissions.
func (h *Handler) onCancel(req *siplib.Request, tx siplib.ServerTransaction) {
	h.log.Info().Str("call_id", req.CallID().Value()).Msg("CANCEL retransmission received — responding 200 OK")
	_ = tx.Respond(siplib.NewResponseFromRequest(req, 200, "OK", nil))
}

// onOptions sends a 200 OK for SIP OPTIONS keepalive probes (no dialog needed).
func (h *Handler) onOptions(req *siplib.Request, tx siplib.ServerTransaction) {
	_ = tx.Respond(siplib.NewResponseFromRequest(req, 200, "OK", nil))
}
