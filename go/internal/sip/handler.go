package sip

import (
	"errors"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo"
	siplib "github.com/emiago/sipgo/sip"
	"github.com/rs/zerolog"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/config"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/identity"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/observability"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/webhook"
)

// StatusSubscription is the sip-package projection of a per-call status
// callback subscription. Mirrors bridge.StatusCallbackConfig but lives in
// sip so the Handler can own a typed lookup callback without taking a
// dependency on internal/bridge (which would create an import cycle —
// bridge already depends on sip implicitly via the manager package's
// reference to sip.LegConfigurer).
//
// Production wiring in cmd/main.go constructs a closure
// (StatusSubscriptionLookup) over *bridge.CallManager that converts
// bridge.StatusCallbackConfig → sip.StatusSubscription at lookup time;
// tests inject their own closure.
type StatusSubscription struct {
	URL    string
	Method string
	Events map[string]struct{}

	// Trusted=true marks operator-supplied default-callback URLs
	// (STATUS_CALLBACK_DEFAULT_URL env). Propagated from
	// bridge.StatusCallbackConfig.Trusted by the resolver closure;
	// CallbackEvent.Trusted is set from this field at emit time.
	Trusted bool
}

// StatusSubscriptionLookup resolves a CallSid to its current per-call
// status-callback subscription, allocating the next monotonic
// SequenceNumber for the emit attempt. Returns ok=false when no session
// is registered for callSid OR when no subscription is set on the
// resolved session.
//
// Also returns the live PreRegisteredSession iface so emitStatusEvent can
// call session.MarkEmitted() after a successful Enqueue. This is the
// symmetric "started ⇒ ended" wire — without it, everEmitted stays false
// through the lifecycle path and the cleanup-on-failure gate in
// PreRegisterSession would incorrectly suppress terminal callbacks for
// calls that DID emit `initiated` then later failed mid-handshake. The
// session may be nil for tests that don't exercise the MarkEmitted path;
// emitStatusEvent guards on `if session != nil`.
//
// The returned seq is the SequenceNumber to stamp on the CURRENT emit;
// callers SHOULD only consume it on a path that actually Enqueues, but
// because allocation is cheap (atomic.Add) it is acceptable to allocate
// then drop on the rare nil-Enqueue path.
type StatusSubscriptionLookup func(callSid string) (sub *StatusSubscription, seq uint64, session PreRegisteredSession, ok bool)

// CallManagerIface is satisfied by bridge.CallManager (defined in 06-02).
// Defined here to avoid circular import (sip package would otherwise import bridge).
//
// Extended with PreRegisterSession + StartSessionWithPreRegistered for the
// synchronous-pre-registration path that activates parent-leg lifecycle
// emit hooks. The legacy StartSession remains for backward compatibility
// with tests that don't go through onInvite's new path.
type CallManagerIface interface {
	AcquirePort() (int, error)
	ReleasePort(port int)
	AccountSid() string
	StartSession(
		dlg *sipgo.DialogServerSession,
		req *siplib.Request,
		callerSDP *CallerSDP,
		rtpPort int,
		audioPT uint8,
		localSRTPKey []byte,
		localSRTPSalt []byte,
		callSid string,
		streamURL string,
		log zerolog.Logger,
	)

	// PreRegisterSession synchronously builds a CallSession shell and
	// registers it in callSidIdx + sessions so the lifecycle hooks below
	// (emitInitiated/Ringing/Answered) resolve the session immediately at
	// INVITE-arrival time — BEFORE StartSessionWithPreRegistered's goroutine
	// runs. Returns a PreRegisteredSession iface + idempotent cleanup func
	// that the caller MUST defer until StartSessionWithPreRegistered is
	// dispatched (in which case the cleanup is a no-op) or the inbound flow
	// aborts (in which case cleanup unwinds the registration). See
	// PreRegisteredSession docstring for the architectural invariant.
	PreRegisterSession(callSid, callID, accountSid, fromURI, toURI string, startTime time.Time) (PreRegisteredSession, func())

	// StartSessionWithPreRegistered consumes a session built by
	// PreRegisterSession and attaches legs[0] + runs the bridge. MUST NOT
	// reassign streamSid (BLOCKER 2 contract — owned by PreRegisterSession).
	StartSessionWithPreRegistered(
		session PreRegisteredSession,
		dlg *sipgo.DialogServerSession,
		req *siplib.Request,
		callerSDP *CallerSDP,
		rtpPort int,
		audioPT uint8,
		localSRTPKey []byte,
		localSRTPSalt []byte,
		streamURL string,
		log zerolog.Logger,
	)
}

// Handler manages inbound SIP dialog state using sipgo.DialogServerCache.
// Register it via NewHandler before calling registrar.Register() in main.go.
type Handler struct {
	dialogSrv   *sipgo.DialogServerCache
	callManager CallManagerIface
	cfg         config.Config
	log         zerolog.Logger
	shutdown    atomic.Bool

	// extraByeReaders are tried in order after dialogSrv when a BYE arrives
	// that doesn't match a server-side dialog. The Forwarder registers itself
	// here so BYEs for outbound (UAC) dialogs are routed to its
	// DialogClientCache; without this the callee-hangs-up-first path on a
	// B2BUA <Dial> bridge would never resolve and the API handler that
	// triggered the dial would deadlock on PerformDial.
	extraByeReaders []ByeReader

	// Per-call status-callback emission surface for the parent inbound
	// leg. statusWC is the process-global StatusClient; statusLookup
	// resolves the per-CallSid subscription + allocates the next
	// SequenceNumber. Both nil-safe — the emit helpers
	// (emitInitiated / emitRinging / emitAnswered) early-return when either
	// is nil so existing fixtures that don't construct one continue to
	// pass.
	//
	// Practical note: at INVITE arrival time (line 116) and 180/200 send
	// time (lines below), the *bridge.CallSession does NOT YET exist in
	// callManager.callSidIdx (StartSession is launched as a goroutine on
	// line 197 and only registers the session inside its own body). The
	// statusLookup will therefore return ok=false at all three callsites
	// for the inbound INVITE flow as currently structured. The hooks are
	// wired and ready; a future architectural change that pre-registers
	// the session before the goroutine spawn (or moves StartSession to a
	// synchronous prefix) will activate them. Tests in handler_test.go use
	// a synthetic lookup that returns a pre-built subscription, validating
	// the helper code path independently of the timing constraint.
	statusWC     *webhook.StatusClient
	statusLookup StatusSubscriptionLookup
	accountSid   string // process-global AccountSid; the callManager.AccountSid() at boot
}

// ByeReader is satisfied by anything that can route an incoming BYE to a
// matching dialog and drive it to terminated state. *sipgo.DialogServerCache,
// *sipgo.DialogClientCache, and *sip.Forwarder all satisfy it. Handler.onBye
// tries each ByeReader in registration order and stops at the first one that
// successfully consumes the BYE.
type ByeReader interface {
	ReadBye(req *siplib.Request, tx siplib.ServerTransaction) error
}

// SetShutdown marks the handler as shutting down. onInvite will reject new INVITEs with 503.
func (h *Handler) SetShutdown() {
	h.shutdown.Store(true)
}

// NewHandler creates the Handler, wires it to agent.Server, and returns the ready handler.
// Must be called BEFORE registrar.Register() so INVITE handling is active from first registration.
//
// extraByeReaders, when supplied, are tried in order after the inbound
// DialogServerCache when a BYE doesn't match a server-side dialog. Production
// code passes the Forwarder here so outbound <Dial> dialogs receive their
// BYEs correctly when the callee hangs up first.
//
// Parent-leg status-callback fields are wired post-construction via
// SetStatusEmission so existing test fixtures that construct a Handler
// without a StatusClient continue to compile + pass unchanged. Production
// main.go calls SetStatusEmission once at boot.
func NewHandler(agent *Agent, callManager CallManagerIface, cfg config.Config, log zerolog.Logger, extraByeReaders ...ByeReader) *Handler {
	contact := siplib.ContactHeader{
		Address: siplib.Uri{
			User: cfg.SIPUser,
			Host: cfg.SDPContactIP,
			Port: cfg.ListenPort(),
		},
	}
	dialogSrv := sipgo.NewDialogServerCache(agent.Client, contact)

	h := &Handler{
		dialogSrv:       dialogSrv,
		callManager:     callManager,
		cfg:             cfg,
		log:             log,
		extraByeReaders: extraByeReaders,
	}

	// Register handlers BEFORE Register() is called in main.go
	agent.Server.OnInvite(h.onInvite)
	agent.Server.OnAck(h.onAck)
	agent.Server.OnBye(h.onBye)
	agent.Server.OnCancel(h.onCancel)
	agent.Server.OnOptions(h.onOptions)

	return h
}

// SetStatusEmission wires the per-call status-callback emission surface.
// statusWC is the process-global *webhook.StatusClient;
// lookup is a closure over the live call-state index that resolves the
// per-CallSid subscription + allocates the next SequenceNumber.
// accountSid is the process-global AccountSid stamped on every emitted
// callback's form payload.
//
// Nil-safe: any of the three may be left unset, in which case the emit
// helpers no-op. Single-writer in practice — main.go invokes this once
// at boot before the SIP server starts accepting INVITEs.
func (h *Handler) SetStatusEmission(statusWC *webhook.StatusClient, lookup StatusSubscriptionLookup, accountSid string) {
	h.statusWC = statusWC
	h.statusLookup = lookup
	h.accountSid = accountSid
}

// onInvite handles inbound INVITE requests with the v2.1 inline-answer flow:
//
//	ReadInvite → ParseCallerSDP → AcquirePort → 100 Trying → 180 Ringing →
//	BuildSDPAnswer → RespondSDP(200 OK) → go StartSession
//
// The 200 OK is emitted on the same goroutine as INVITE receipt (sipgo's
// server-transaction requires this — see RESEARCH §1). StartSession is then
// launched as a goroutine to dial WS and run the RTP bridge post-answer.
//
// The default WS target is cfg.WSTargetURL. Future webhook-driven dispatch
// (via the <Connect><Stream url=…/> verb) overrides this; the default
// path always streams to the configured default target.
func (h *Handler) onInvite(req *siplib.Request, tx siplib.ServerTransaction) {
	// Mint CallSid up-front so every log line below carries it (COMPAT-03).
	callSid := identity.NewCallSid()
	accountSid := h.callManager.AccountSid()

	// The per-session sub-logger carries correlation identifiers ONLY
	// (call_id = SIP Call-ID header for SIP-layer correlation; call_sid =
	// Twilio CallSid for cross-system correlation; account_sid =
	// AccountSid). Phone-number / URI fields (from / to) are intentionally
	// NOT baked into the With() chain — embedding them propagates phone
	// numbers into every Info+ log line emitted from this logger, which
	// breaks the "phone-number / URL debug-only" cardinality rule. fromURI
	// and toURI captured below are still available for the status-callback
	// emit hooks where they are part of the wire-format (Twilio form body).
	log := h.log.With().
		Str(observability.FieldSIPCallID, req.CallID().Value()).
		Str(observability.FieldCallSid, callSid).
		Str(observability.FieldAccountSid, accountSid).
		Logger()

	// Capture URI strings once for the status-callback emit hooks (and any
	// future callers that need a String() copy of From/To for logs).
	fromURI := req.From().Address.String()
	toURI := req.To().Address.String()
	startTime := time.Now().UTC()

	// Synchronous pre-registration. The session is now in
	// callManager.callSidIdx + sessions BEFORE the lifecycle hooks
	// (emitInitiated/Ringing/Answered) fire, so the statusLookup closure
	// resolves the live session and the customer's subscription drives
	// actual POSTs.
	//
	// The cleanup closure deferred below is the safety hatch: it runs when
	// onInvite returns WITHOUT having dispatched StartSessionWithPreRegistered
	// (any failure-exit path) and unwinds the registration. It only stamps
	// a recently-terminated snapshot if the call ever reached "initiated"
	// emit (gated via session.EverEmitted()). Set `success = true` before
	// the goroutine spawn to suppress cleanup on the happy path.
	// PreRegisterSession installs the operator-supplied default
	// StatusCallback subscription internally when STATUS_CALLBACK_DEFAULT_URL
	// is set in cfg — kept inside the bridge package to avoid the
	// sip→bridge import cycle.
	session, cleanup := h.callManager.PreRegisterSession(callSid, req.CallID().Value(), accountSid, fromURI, toURI, startTime)
	success := false
	defer func() {
		if !success {
			cleanup()
		}
	}()

	// Hook 1: emit "initiated" (CallStatus=queued) on the parent
	// CallSession's status-callback subscription. The session is registered
	// synchronously so the statusLookup closure now resolves it.
	h.emitInitiated(callSid, fromURI, toURI, log)

	// Reject new INVITEs during graceful shutdown (LCY-01 — RFC 3261 §21.5.4)
	if h.shutdown.Load() {
		session.SetSIPFinalCode(503)
		_ = tx.Respond(siplib.NewResponseFromRequest(req, 503, "Service Unavailable", nil))
		return
	}

	// ReadInvite MUST be called first — creates dialog context and To-tag.
	// NOTE: do NOT defer dlg.Close() here — that removes the dialog from DialogServerCache
	// immediately when onInvite returns, making MatchDialogRequest fail for ACK and BYE.
	// ReadBye already calls dlg.Close() internally via its own defer.
	dlg, err := h.dialogSrv.ReadInvite(req, tx)
	if err != nil {
		session.SetSIPFinalCode(500)
		log.Error().Err(err).Msg("ReadInvite failed")
		_ = tx.Respond(siplib.NewResponseFromRequest(req, 500, "Server Error", nil))
		return
	}

	// Parse caller's SDP offer to extract RTP addr/port and DTMF PT
	callerSDP, err := ParseCallerSDP(req.Body())
	if err != nil {
		session.SetSIPFinalCode(503)
		log.Error().Err(err).Msg("SDP parse failed — rejecting INVITE")
		_ = dlg.Respond(503, "Service Unavailable", nil)
		return
	}

	// Acquire RTP port — reject with 503 if pool exhausted (SIP-04)
	rtpPort, err := h.callManager.AcquirePort()
	if err != nil {
		session.SetSIPFinalCode(503)
		log.Warn().Err(err).Msg("RTP port pool exhausted — rejecting INVITE")
		_ = dlg.Respond(503, "Service Unavailable", nil)
		return
	}

	// 100 Trying — suppresses INVITE retransmissions from the proxy.
	// Built manually and sent via tx.Respond to avoid sipgo copying Record-Route
	// headers from the INVITE into provisional responses. Record-Route in 180/100
	// causes some SIP clients (softphones, mobile) to immediately send CANCEL.
	trying := siplib.NewResponseFromRequest(req, 100, "Trying", nil)
	for trying.RemoveHeader("Record-Route") {
	} // RemoveHeader removes only one at a time; loop until all are gone
	_ = tx.Respond(trying)

	// 180 Ringing — signals caller that we are alerting before answer.
	// Same: strip ALL Record-Route headers and send without Contact (no early dialog).
	ringing := siplib.NewResponseFromRequest(req, 180, "Ringing", nil)
	for ringing.RemoveHeader("Record-Route") {
	}
	_ = tx.Respond(ringing)

	// Hook 2: emit "ringing" (CallStatus=ringing) on the parent
	// CallSession's status-callback subscription.
	h.emitRinging(callSid, fromURI, toURI, log)

	// Build the SDP answer for our side of the media path.
	sdpBytes, negotiatedPT, localSRTPKey, localSRTPSalt := BuildSDPAnswer(h.cfg.SDPContactIP, rtpPort, callerSDP, h.cfg.AudioMode, h.cfg.SRTPEnabled)
	if h.cfg.SRTPEnabled && !callerSDP.IsSRTP {
		log.Warn().Msg("SRTP desired but caller offered plain RTP/AVP — proceeding unencrypted")
	}

	// 200 OK with SDP answer — emitted synchronously on this goroutine so the
	// sipgo server-transaction stays open through the final response (RESEARCH §1).
	if err := dlg.RespondSDP(sdpBytes); err != nil {
		session.SetSIPFinalCode(503)
		log.Info().Err(err).Msg("RespondSDP 200 OK failed — releasing port")
		h.callManager.ReleasePort(rtpPort)
		return
	}

	// Success path — stamp 200 OK final code + answered-at timestamp BEFORE
	// the emitAnswered hook. This populates the atomics that drive
	// emitTerminalStatusCallback's CallDuration / Duration / SipResponseCode
	// form fields. On the BYE-driven termination path (caller hangs up
	// after answer), markTerminated's defensive default
	// (defaultSIPCodeForReason) preserves SipResponseCode=200 if the caller's
	// BYE is the natural exit.
	session.SetSIPFinalCode(200)
	session.SetAnsweredAt(time.Now().UTC())

	// Hook 3: emit "answered" (CallStatus=in-progress — Twilio's
	// event/status divergence) on the parent subscription. Fired AFTER
	// the 200 OK + SDP send succeeds and BEFORE the
	// StartSessionWithPreRegistered goroutine spawn.
	h.emitAnswered(callSid, fromURI, toURI, log)

	// Success — disable the cleanup-on-failure defer; the
	// StartSessionWithPreRegistered goroutine's defer chain owns teardown
	// from here.
	success = true
	go h.callManager.StartSessionWithPreRegistered(
		session, dlg, req, callerSDP, rtpPort, negotiatedPT,
		localSRTPKey, localSRTPSalt, h.cfg.WSTargetURL, log,
	)
}

// onAck routes the ACK to the correct dialog, transitioning it to DialogStateConfirmed.
func (h *Handler) onAck(req *siplib.Request, tx siplib.ServerTransaction) {
	if err := h.dialogSrv.ReadAck(req, tx); err != nil {
		h.log.Error().Err(err).Str(observability.FieldSIPCallID, req.CallID().Value()).Msg("ReadAck failed")
	}
}

// onBye routes the BYE to the correct dialog, sends 200 OK, and cancels
// dlg.Context(). The session goroutine launched in onInvite will exit when
// dlg.Context().Done() fires.
//
// The dialogSrv (DialogServerCache) handles BYEs for inbound dialogs (the
// caller's leg). For B2BUA <Dial> bridges, the callee may hang up first —
// sipgate then sends BYE to our Server destined for the OUTBOUND dialog
// which is tracked in the Forwarder's DialogClientCache. We try the inbound
// cache first; on ErrDialogDoesNotExists we fall through to the
// extraByeReaders (the Forwarder).
func (h *Handler) onBye(req *siplib.Request, tx siplib.ServerTransaction) {
	callID := req.CallID().Value()

	if err := h.dialogSrv.ReadBye(req, tx); err == nil {
		return // matched an inbound dialog — done
	} else if !errors.Is(err, sipgo.ErrDialogDoesNotExists) {
		h.log.Error().Err(err).Str(observability.FieldSIPCallID, callID).Msg("ReadBye failed (server cache)")
		return
	}

	// Server cache didn't know about this dialog; try the registered
	// extraByeReaders (typically the Forwarder for outbound <Dial> dialogs).
	for _, r := range h.extraByeReaders {
		if err := r.ReadBye(req, tx); err == nil {
			return
		} else if !errors.Is(err, sipgo.ErrDialogDoesNotExists) {
			h.log.Error().Err(err).Str(observability.FieldSIPCallID, callID).Msg("ReadBye failed (fallback)")
			return
		}
	}

	h.log.Warn().Str(observability.FieldSIPCallID, callID).Msg("ReadBye: no matching dialog in any cache")
}

// onCancel handles CANCEL requests. sipgo automatically sends 200 OK to CANCEL + 487 to
// the matching INVITE at the transaction layer (before this handler is called). This handler
// is only reached for CANCEL retransmissions that arrive after the original INVITE transaction
// is already gone. We respond 200 OK to stop retransmissions.
func (h *Handler) onCancel(req *siplib.Request, tx siplib.ServerTransaction) {
	h.log.Info().Str(observability.FieldSIPCallID, req.CallID().Value()).Msg("CANCEL retransmission received — responding 200 OK")
	_ = tx.Respond(siplib.NewResponseFromRequest(req, 200, "OK", nil))
}

// onOptions sends a 200 OK for SIP OPTIONS keepalive probes (no dialog needed).
func (h *Handler) onOptions(req *siplib.Request, tx siplib.ServerTransaction) {
	_ = tx.Respond(siplib.NewResponseFromRequest(req, 200, "OK", nil))
}

// ── parent-leg status-callback emission ────────────────────────────────────
//
// Three emit helpers that build the Twilio-shape form payload and Enqueue
// onto the StatusClient. Twilio form-field shape; RFC 1123Z timestamp;
// SequenceNumber allocation; event/CallStatus divergence
// (event="answered" while CallStatus="in-progress").
//
// All three are nil-safe at every reachable point: nil StatusClient ⇒ skip;
// nil lookup ⇒ skip; lookup ok=false (no session OR no subscription) ⇒
// skip; subscribed Events does not include this event ⇒ skip. Each helper
// is fire-and-forget — Enqueue is non-blocking by 16-03's contract.

// emitStatusEvent is the common code path for handler-side state-transition
// emissions. INVITE arrival is treated as "Twilio starts dialing", so the
// inbound semantics map onto Twilio's outbound vocab as
// initiated/ringing/answered with CallStatus queued/ringing/in-progress.
//
// On first successful Enqueue, calls session.MarkEmitted() to stamp the
// CallSession's everEmitted flag. Read by PreRegisterSession's cleanup
// closure to gate ghost terminal-only callbacks — the customer is told
// "the call ended" only if they were first told "the call started".
func (h *Handler) emitStatusEvent(callSid, eventName, callStatus, fromURI, toURI string, log zerolog.Logger) {
	if h.statusWC == nil || h.statusLookup == nil {
		return
	}
	sub, seq, session, ok := h.statusLookup(callSid)
	if !ok || sub == nil {
		return // no session yet OR no subscription set
	}
	// Subscription resolution per Twilio default: empty Events ⇒
	// subscribe-to-all; specific match ⇒ emit; terminal-class events fall
	// back to "completed" if subscribed. Lifecycle events
	// (initiated/ringing/answered) do NOT fall back. Single source of
	// truth for subscription resolution lives in internal/webhook.
	if !webhook.SubscriptionMatches(sub.Events, eventName) {
		return // event not subscribed (per Twilio resolution rules)
	}

	form := url.Values{}
	form.Set("CallSid", callSid)
	form.Set("AccountSid", h.accountSid)
	form.Set("From", fromURI)
	form.Set("To", toURI)
	form.Set("Caller", fromURI)
	form.Set("Called", toURI)
	form.Set("Direction", "inbound")
	form.Set("ApiVersion", "2010-04-01")
	form.Set("CallStatus", callStatus)
	form.Set("Timestamp", time.Now().UTC().Format(time.RFC1123Z))
	form.Set("SequenceNumber", strconv.FormatUint(seq, 10))
	form.Set("CallbackSource", "call-progress-events")

	method := sub.Method
	if method == "" {
		method = "POST"
	}
	evt := webhook.CallbackEvent{
		URL:    sub.URL,
		Method: method,
		Form:   form,
		// Event-vocab label for status_callback_attempts_total. eventName
		// is the helper's second arg (initiated/ringing/answered) —
		// distinct from CallStatus form value (queued/ringing/in-progress)
		// per the event/CallStatus vocabulary split.
		Event: eventName,
		// Operator-trusted URL → bypass SSRF guard at dial time.
		Trusted: sub.Trusted,
	}
	if err := h.statusWC.Enqueue(callSid, evt); err != nil {
		// Log warn (with call_sid; NEVER include URL or authToken at warn
		// level). The accountSid is ALSO safe; URL is intentionally
		// omitted so a misconfigured customer host does not leak via ops
		// logs.
		log.Warn().
			Err(err).
			Str("event", eventName).
			Msg("status_callback: parent-leg Enqueue failed")
		return
	}
	// Stamp everEmitted on first successful Enqueue so PreRegisterSession's
	// cleanup closure knows the customer was told "the call started" and
	// can therefore tell them "the call ended". CompareAndSwap inside
	// MarkEmitted makes this idempotent across initiated → ringing →
	// answered re-emits. The session iface comes from the extended
	// StatusSubscriptionLookup signature; nil-safe for tests that don't
	// exercise the MarkEmitted path.
	if session != nil {
		session.MarkEmitted()
	}
}

// emitInitiated emits the "initiated" event (CallStatus=queued) — fired
// at INVITE arrival.
func (h *Handler) emitInitiated(callSid, fromURI, toURI string, log zerolog.Logger) {
	h.emitStatusEvent(callSid, "initiated", "queued", fromURI, toURI, log)
}

// emitRinging emits the "ringing" event (CallStatus=ringing) — fired
// after the 180 Ringing send.
func (h *Handler) emitRinging(callSid, fromURI, toURI string, log zerolog.Logger) {
	h.emitStatusEvent(callSid, "ringing", "ringing", fromURI, toURI, log)
}

// emitAnswered emits the "answered" event (CallStatus=in-progress) —
// fired after the 200 OK + SDP send. Event/status divergence per §3.1.
func (h *Handler) emitAnswered(callSid, fromURI, toURI string, log zerolog.Logger) {
	h.emitStatusEvent(callSid, "answered", "in-progress", fromURI, toURI, log)
}
