package sip

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
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

// LegConfigurer is the narrow interface the SIP <Dial> Forwarder needs from
// a callee Leg. *bridge.Leg satisfies it structurally — we duplicate the
// interface declaration here (mirroring bridge.LegConfigurer in
// internal/bridge/leg.go) to avoid a sip → bridge import cycle. Production
// code passes a *bridge.Leg; tests pass a stubLegConfigurer.
type LegConfigurer interface {
	BuildSDPOffer() (sdp []byte, rtpPort int, err error)
	AcceptSDPAnswer(answerSDP []byte) error
	RTPLocalPort() int
	// OnAnswered is the dual-leg activation hook. Called by the Forwarder
	// exactly once after a successful 200 OK + ACK. The bridge.Leg
	// implementation opens the callee RTP socket, spawns the relay
	// goroutines, and CAS-es the session state to StateForwarding so the
	// caller-side rtpReader switches into B2BUA relay mode. Returns an
	// error if the socket cannot be opened or required leg state
	// (rtpPort/remoteRTP) is missing — in which case the dialog is BYE'd
	// and DialResult.Status becomes "failed".
	OnAnswered() error
}

// statusDrainBudget is a local alias for webhook.StatusDrainBudget — kept
// unexported here to preserve the call-site shape `statusDrainBudget`
// already used at forwarder.go:1066 + 1315. See webhook/status.go for the
// canonical justification (1+2+4 backoff + 4×4s timeout ≈ 23s + 7s
// headroom). Single source of truth.
//
// Used by the terminal-dispatch hook in recordFailure (failure path) and
// by the success-path terminal emit in Forwarder.Dial.
const statusDrainBudget = webhook.StatusDrainBudget

// ── Sentinel errors ───────────────────────────────────────────────────────────
//
// Translation to Twilio error codes happens at the wire-up layer; the
// Forwarder is responsible only for identifying the *kind* of failure.
// The error messages embed substrings that observability.BucketForwardReason
// recognises so the failure metric gets the right `reason` label without
// having to import internal/sip into observability.
var (
	// ErrCallerIDRequired is returned by Dial when the caller-ID fallback
	// chain (TwiML callerId → preserve ANI → cfg.DialDefaultCallerID) yields
	// the empty string. Twilio code 13214 is embedded so BucketForwardReason
	// classifies this as "caller_id_rejected".
	ErrCallerIDRequired = errors.New("forwarder: caller-id required (13214)")

	// ErrCodecMismatch is returned when the callee's 200 OK SDP answer
	// negotiates a non-PCMU codec. Twilio code 13224 is embedded for the
	// reason mapping.
	ErrCodecMismatch = errors.New("forwarder: codec_mismatch — only PCMU supported (13224)")
)

// ── DialOpts / DialResult value types ─────────────────────────────────────────

// DialOpts captures the per-Dial knobs that come from the TwiML <Dial>
// attributes (callerId, timeout, timeLimit, hangupOnStar, action, method).
// All fields are optional — Forwarder applies defaults from cfg/Twilio
// when values are zero.
type DialOpts struct {
	// CallerID is the value from TwiML <Dial callerId="...">. When empty the
	// Forwarder falls back to the inbound caller's From URI (preserve ANI),
	// then to cfg.DialDefaultCallerID, then errors with ErrCallerIDRequired.
	CallerID string

	// Timeout is the ring timeout (Twilio default 30s, range 5–600s). Zero
	// means "use cfg.DialRingTimeoutS".
	Timeout time.Duration

	// TimeLimit is the maximum talk-time after the dialog confirms (Twilio
	// default 14400s = 4h). Zero means "no limit" — the watchdog goroutine
	// is not spawned.
	TimeLimit time.Duration

	// HangupOnStar: when true and DTMFChan delivers a '*' digit, the
	// Forwarder sends BYE to the callee leg. Wires DTMFChan to
	// bridge.session.dtmfQueue.
	HangupOnStar bool

	// DTMFChan is the inbound DTMF stream from the caller leg. Wired
	// upstream; the forwarder consumes only when HangupOnStar is true.
	DTMFChan <-chan rune

	// Action / Method are the post-dial action callback URL and HTTP method.
	// Recorded on DialResult; the wire-up layer dispatches the callback.
	Action string
	Method string

	// CallerFrom is the inbound caller's From URI string used for ANI
	// preservation. Extracted from the inbound INVITE by the wire-up layer
	// and forwarded here.
	CallerFrom string

	// Per-<Dial>-leg status-callback subscription. Values originate from
	// twiml.DialOpts (TwiML <Dial statusCallback…> / <Number
	// statusCallback…> attrs resolved by verb_dial.go) and are threaded
	// here by midCallAdapter.PerformDial. Independent from the parent
	// CallSession's StatusCallback — the callee leg has its own per-leg
	// subscription.
	StatusCallback        string
	StatusCallbackMethod  string
	StatusCallbackEvents  []string
}

// DialResult is the terminal-state report from a single Dial invocation.
// Status drives the Twilio DialCallStatus mapping at the wire-up layer:
//
//	"answered"     → Twilio "completed" or "answered" (depending on TwiML attr)
//	"no-answer"    → "no-answer"
//	"busy"         → "busy"
//	"failed"       → "failed"
//	"canceled"     → "canceled"
//	"hangup-star"  → custom; surfaces as "completed" with action callback
type DialResult struct {
	Status        string        // see comment above
	Reason        string        // "" on success; e.g. "codec_mismatch", "auth_failed", "trunk_5xx"
	DialCallSid   string        // CA[0-9a-f]{32} — minted in Dial regardless of outcome
	Duration      time.Duration // 200 OK + ACK → BYE; zero on failure paths
	SIPFinalCode  int           // 200, 486, 408, 503, etc.; 0 if no final response
	DialedTarget  string        // normalized E.164 (for logging/audit)
	AuthChallenge string        // "401" | "407" | "" — from sipgo's WaitAnswer OnResponse hook
}

// ── DialClientFactory: dependency-injection seam for sipgo testability ────────
//
// Real sipgo dialogs require a UDP listener — that breaks unit testability
// (no fixed port; race-prone; goroutine leaks on cleanup). DialClientFactory
// abstracts the surface the Forwarder needs from a sipgo DialogUA so tests can
// inject a stub that returns canned responses (final status code, SDP body,
// auth-challenge kind) without touching the network.
//
// Production constructor: NewSipgoDialFactory(agent.Client, contactHDR).
// Test constructor: stubDialFactory in forwarder_test.go.
type DialClientFactory interface {
	// Dial sends an INVITE to the recipient with the given SDP body and waits
	// for a final response. The returned DialClient lets the caller inspect
	// the final response status, SDP answer, ACK on 2xx, and BYE on
	// terminate. The caller is responsible for invoking Close exactly once.
	//
	// `from` is the caller-ID URI to put in the From header (callerID
	// fallback already resolved upstream in Forwarder.Dial).
	// `displayName`, when non-empty, is rendered as the From header's
	// display-name part: `"display" <sip:user@host>`. Most carriers honour
	// the From display-name as the Caller-ID shown to the callee, even when
	// the addr-spec carries the SIP auth username — the cleanest sipgate-
	// compat path.
	// `ppi`, when non-nil, is added as a P-Preferred-Identity header
	// (RFC 3325 §9.2) — the identity the UA would like the trust-domain
	// proxy to assert on its behalf. PPI is the correct UA-side header
	// (P-Asserted-Identity is what the trust domain emits, not what the
	// UA sends). Used as defense-in-depth for carriers that resolve
	// Caller-ID from PPI rather than From display-name.
	// `auth` carries the digest credentials sipgo's WaitAnswer auto-applies
	// on 401/407 challenge responses.
	// `onResponse` is invoked for every (provisional + final) response — the
	// Forwarder uses it to capture the auth-challenge kind metric.
	Dial(ctx context.Context, recipient siplib.Uri, from siplib.Uri, displayName string, ppi *siplib.Uri, body []byte, auth DialAuth, onResponse func(*siplib.Response) error) (DialClient, error)

	// ReadBye routes an incoming BYE to the matching outbound dialog
	// managed by this factory. Returns sipgo's ErrDialogDoesNotExists when
	// the BYE belongs to no dialog tracked here — caller should fall back
	// to the server-side dialog cache. Used by Handler.onBye to terminate
	// outbound dialogs cleanly when the callee hangs up first; without it
	// awaitDialogEnd never observes dlg.Done() and Forwarder.Dial deadlocks
	// the API handler that triggered the dial.
	ReadBye(req *siplib.Request, tx siplib.ServerTransaction) error
}

// DialAuth carries digest credentials passed to sipgo's WaitAnswer.
type DialAuth struct {
	Username string
	Password string
}

// DialClient is the post-INVITE handle returned by a DialClientFactory. The
// real implementation wraps a *sipgo.DialogClientSession; the test stub
// implements canned responses.
type DialClient interface {
	// FinalResponse returns the captured final response from WaitAnswer.
	// Provisional responses are NOT exposed here — they go through the
	// onResponse callback passed to Dial.
	FinalResponse() *siplib.Response

	// Ack sends ACK in response to a 2xx final response (RFC 3261 §13.2.2.4).
	Ack(ctx context.Context) error

	// Bye sends BYE to terminate a confirmed dialog.
	Bye(ctx context.Context) error

	// Done returns a channel closed when the dialog terminates (BYE from
	// either side, transaction timeout, or context cancellation). Used by
	// the timeLimit watchdog goroutine to exit early when the dialog ends
	// naturally.
	Done() <-chan struct{}

	// Close releases sipgo internal resources. Idempotent.
	Close() error
}

// ── Forwarder ─────────────────────────────────────────────────────────────────

// Forwarder is the B2BUA SIP UAC. One instance is constructed in main.go and
// shared across all <Dial> invocations — its dependencies are agent.Client
// (shared listener so the UDP source-port pinhole survives),
// guardrails (toll-fraud / rate limit enforcement), config (caller-ID
// defaults + ring timeout), metrics (forward_* + auth_challenge_kind), and a
// DialClientFactory (defaults to the sipgo wrapper; tests inject a stub).
type Forwarder struct {
	agent      *Agent
	guardrails *Guardrails
	cfg        config.Config
	metrics    *observability.Metrics
	log        zerolog.Logger
	factory    DialClientFactory

	// Status-callback emission surface for the callee <Dial> leg.
	// statusWC is nil-safe — deployments and fixtures that don't
	// construct one continue to work via the nil-guard inside
	// emitDialEvent.
	statusWC   *webhook.StatusClient
	accountSid string

	// Per-DialCallSid SequenceNumber generators. Created lazily on first
	// emit; removed when the leg's terminal event fires (the terminal-
	// dispatch path calls ReleaseLegSequence at the dispatch point).
	// Each callee leg has its own monotonic 0-indexed counter,
	// independent from the parent CallSession's per-call counter.
	legSeqs sync.Map // map[string]*atomic.Uint64
}

// NewForwarder wires a Forwarder with the production sipgo-backed
// DialClientFactory. The shared agent.Client is used for outbound INVITEs so
// the UDP source port that sipgate registered for SIP_USER stays live (per
// — never call sipgo.NewClient inside the forwarder).
//
// statusWC is the process-global *webhook.StatusClient used for
// per-<Dial>-leg lifecycle status callbacks. May be nil — emit helpers
// no-op when nil so existing fixtures that don't construct one keep working.
// accountSid is the AccountSid stamped on every emitted callback's form
// payload (per CallSession's AccountSid, but Forwarder doesn't have a
// session — main.go passes the process-global value derived from SIP_USER).
func NewForwarder(agent *Agent, guardrails *Guardrails, cfg config.Config, metrics *observability.Metrics, log zerolog.Logger, statusWC *webhook.StatusClient, accountSid string) *Forwarder {
	contactHDR := siplib.ContactHeader{
		Address: siplib.Uri{
			User: cfg.SIPUser,
			Host: cfg.SDPContactIP,
			Port: cfg.ListenPort(),
		},
	}
	return &Forwarder{
		agent:      agent,
		guardrails: guardrails,
		cfg:        cfg,
		metrics:    metrics,
		log:        log,
		factory:    newSipgoDialFactory(agent.Client, contactHDR),
		statusWC:   statusWC,
		accountSid: accountSid,
	}
}

// NewForwarderWithFactory is the test-only constructor — accepts a stub
// DialClientFactory so unit tests don't need a UDP listener. Production code
// always uses NewForwarder.
//
// statusWC and accountSid are accepted via NewForwarderWithStatusClient (see
// below); this constructor leaves them nil/empty so existing tests that
// build a stub-factory Forwarder continue to compile unchanged.
func NewForwarderWithFactory(agent *Agent, guardrails *Guardrails, cfg config.Config, metrics *observability.Metrics, log zerolog.Logger, factory DialClientFactory) *Forwarder {
	return &Forwarder{
		agent:      agent,
		guardrails: guardrails,
		cfg:        cfg,
		metrics:    metrics,
		log:        log,
		factory:    factory,
	}
}

// NewForwarderWithStatusClient is the test-only constructor for
// status-callback emission tests. Mirrors NewForwarderWithFactory but also
// wires statusWC + accountSid so emitDialInitiated / emitDialRinging /
// emitDialAnswered can be exercised against an httptest target. Used only
// by tests; production code calls NewForwarder.
func NewForwarderWithStatusClient(agent *Agent, guardrails *Guardrails, cfg config.Config, metrics *observability.Metrics, log zerolog.Logger, factory DialClientFactory, statusWC *webhook.StatusClient, accountSid string) *Forwarder {
	return &Forwarder{
		agent:      agent,
		guardrails: guardrails,
		cfg:        cfg,
		metrics:    metrics,
		log:        log,
		factory:    factory,
		statusWC:   statusWC,
		accountSid: accountSid,
	}
}

// nextLegSequence returns the next per-DialCallSid SequenceNumber (0, 1, 2,
// ...). Each callee leg gets its own atomic counter; the counter is removed
// by ReleaseLegSequence at terminal-event emission time.
func (f *Forwarder) nextLegSequence(dialCallSid string) uint64 {
	v, _ := f.legSeqs.LoadOrStore(dialCallSid, new(atomic.Uint64))
	return v.(*atomic.Uint64).Add(1) - 1
}

// ReleaseLegSequence frees the per-leg counter so the legSeqs sync.Map does
// not grow without bound. Called at terminal-event dispatch time.
// Idempotent — calling for an unknown DialCallSid is a no-op.
func (f *Forwarder) ReleaseLegSequence(dialCallSid string) {
	f.legSeqs.Delete(dialCallSid)
}

// emitDialEvent is the common code path for the three <Dial>-leg lifecycle
// events (initiated, ringing, answered). Returns immediately if no
// StatusClient is wired or the event is not subscribed for this leg.
//
// CallSid in the form payload is the callee leg's own DialCallSid, NOT the
// parent caller's CallSid — each leg has independent identity.
// ParentCallSid carries the caller's CallSid for correlation.
//
// Direction = "outbound-dial" — Twilio's documented value for <Dial> legs.
//
// DialResult.SIPFinalCode carries the dial-leg final response code; the
// parent CallSession's SipResponseCode is stamped separately by the
// inbound SIP handler (handler.go onInvite).
func (f *Forwarder) emitDialEvent(opts DialOpts, eventName, callStatus, callerSid, dialCallSid, target string, log zerolog.Logger) {
	if f.statusWC == nil {
		return
	}
	if opts.StatusCallback == "" {
		return
	}
	// Subscription resolution per Twilio default. Convert []string →
	// map[string]struct{} once for the canonical helper. nil/empty list ⇒
	// subscribe-to-all per Twilio's documented default for
	// `<Dial statusCallback="X">` without an explicit `statusCallbackEvent`
	// attr. Single source of truth in internal/webhook.
	eventSet := make(map[string]struct{}, len(opts.StatusCallbackEvents))
	for _, e := range opts.StatusCallbackEvents {
		eventSet[e] = struct{}{}
	}
	if !webhook.SubscriptionMatches(eventSet, eventName) {
		return
	}

	seq := f.nextLegSequence(dialCallSid)

	form := url.Values{}
	form.Set("CallSid", dialCallSid)
	form.Set("AccountSid", f.accountSid)
	form.Set("From", opts.CallerFrom)
	form.Set("To", target)
	form.Set("Caller", opts.CallerFrom)
	form.Set("Called", target)
	form.Set("Direction", "outbound-dial")
	form.Set("ApiVersion", "2010-04-01")
	form.Set("CallStatus", callStatus)
	form.Set("Timestamp", time.Now().UTC().Format(time.RFC1123Z))
	form.Set("SequenceNumber", strconv.FormatUint(seq, 10))
	form.Set("CallbackSource", "call-progress-events")
	if callerSid != "" {
		form.Set("ParentCallSid", callerSid)
	}

	method := opts.StatusCallbackMethod
	if method == "" {
		method = "POST"
	}
	evt := webhook.CallbackEvent{
		URL:    opts.StatusCallback,
		Method: method,
		Form:   form,
		// Event-vocab label for status_callback_attempts_total. eventName
		// is the helper's second arg (initiated/ringing/answered) —
		// distinct from CallStatus form value (queued/ringing/in-progress)
		// per Twilio's event/CallStatus divergence.
		Event: eventName,
	}
	if err := f.statusWC.Enqueue(dialCallSid, evt); err != nil {
		log.Warn().
			Err(err).
			Str("event", eventName).
			Str(observability.FieldForwardLegID, dialCallSid).
			Msg("status_callback: dial-leg Enqueue failed")
	}
}

// emitDialInitiated emits the <Dial>-leg "initiated" event (CallStatus=queued).
// Fired from Forwarder.Dial after guardrails pass, before the SIP INVITE goes
// on the wire — semantically: "we are about to start dialing the callee".
func (f *Forwarder) emitDialInitiated(opts DialOpts, callerSid, dialCallSid, target string, log zerolog.Logger) {
	f.emitDialEvent(opts, "initiated", "queued", callerSid, dialCallSid, target, log)
}

// emitDialRinging emits the <Dial>-leg "ringing" event (CallStatus=ringing) on
// receipt of the first 180 Ringing from the callee. I-6 fix: without this
// hook customers subscribed to {initiated, ringing, answered, completed} on
// the callee leg would silently miss `ringing`. Twilio's <Dial> event vocab
// includes `ringing`.
func (f *Forwarder) emitDialRinging(opts DialOpts, callerSid, dialCallSid, target string, log zerolog.Logger) {
	f.emitDialEvent(opts, "ringing", "ringing", callerSid, dialCallSid, target, log)
}

// emitDialAnswered emits the <Dial>-leg "answered" event (CallStatus=
// in-progress) after the callee 200 OK + ACK + OnAnswered hook completes.
// Twilio's event/status divergence: event name is "answered" while
// CallStatus is "in-progress".
func (f *Forwarder) emitDialAnswered(opts DialOpts, callerSid, dialCallSid, target string, log zerolog.Logger) {
	f.emitDialEvent(opts, "answered", "in-progress", callerSid, dialCallSid, target, log)
}

// ── <Dial>-leg terminal status-callback events ─────────────────────────────
//
// Each wrapper mirrors the existing emitDialInitiated/Ringing/Answered
// shape. eventName / callStatus pairs (Twilio vocab):
//
//	completed   / completed   → natural-end after answer (BYE / dlg.Done())
//	busy        / busy        → 486 Busy Here
//	failed      / failed      → 5xx, codec-mismatch, OnAnswered failure, etc.
//	no-answer   / no-answer   → 408, 480, ring timeout
//	canceled    / canceled    → 487 Request Terminated, ctx.Canceled

// emitDialCompleted emits the <Dial>-leg "completed" terminal event
// (CallStatus=completed). Fired from the success-path tail of Forwarder.Dial
// after the dialog terminates (BYE from either side, ctx cancellation,
// dlg.Done(), timeLimit, hangup-star).
func (f *Forwarder) emitDialCompleted(opts DialOpts, callerSid, dialCallSid, target string, log zerolog.Logger) {
	f.emitDialEvent(opts, "completed", "completed", callerSid, dialCallSid, target, log)
}

// emitDialBusy emits the <Dial>-leg "busy" terminal event (CallStatus=busy)
// when the callee returns 486 Busy Here.
func (f *Forwarder) emitDialBusy(opts DialOpts, callerSid, dialCallSid, target string, log zerolog.Logger) {
	f.emitDialEvent(opts, "busy", "busy", callerSid, dialCallSid, target, log)
}

// emitDialFailed emits the <Dial>-leg "failed" terminal event
// (CallStatus=failed) on 5xx, codec-mismatch, OnAnswered failure, generic
// dial errors, and any other failure not classified as busy/no-answer/
// canceled.
func (f *Forwarder) emitDialFailed(opts DialOpts, callerSid, dialCallSid, target string, log zerolog.Logger) {
	f.emitDialEvent(opts, "failed", "failed", callerSid, dialCallSid, target, log)
}

// emitDialNoAnswer emits the <Dial>-leg "no-answer" terminal event
// (CallStatus=no-answer) on 408 Request Timeout, 480 Temporarily
// Unavailable, or ring-timeout (ctx.DeadlineExceeded).
func (f *Forwarder) emitDialNoAnswer(opts DialOpts, callerSid, dialCallSid, target string, log zerolog.Logger) {
	f.emitDialEvent(opts, "no-answer", "no-answer", callerSid, dialCallSid, target, log)
}

// emitDialCanceled emits the <Dial>-leg "canceled" terminal event
// (CallStatus=canceled) on 487 Request Terminated or context.Canceled
// (caller hung up before the callee answered).
func (f *Forwarder) emitDialCanceled(opts DialOpts, callerSid, dialCallSid, target string, log zerolog.Logger) {
	f.emitDialEvent(opts, "canceled", "canceled", callerSid, dialCallSid, target, log)
}

// metrics:bucketer
//
// dialTerminalEventName maps a Forwarder DialResult.Status value onto the
// Twilio <Dial>-leg terminal event vocabulary. The output is a bounded enum
// of {"completed", "busy", "failed", "no-answer", "canceled"} — exactly the
// 5 terminal labels declared in metrics.go's status_callback_attempts_total
// allowlist.
//
// The // metrics:bucketer annotation registers this helper with the
// cardinality-discipline lint walker as a bounded-output source — call
// sites like emitDialEvent(opts, dialTerminalEventName(result.Status), ...)
// need no per-call // metrics:dynamic-allowed opt-out.
//
// Mapping (per Twilio docs + DialResult.Status producer):
//
//	"answered"     → "completed"  (success path; caller picks this directly
//	                                via emitDialCompleted, but we map here
//	                                too so the chokepoint stays uniform)
//	"hangup-star"  → "completed"  (caller pressed *; dialog answered then
//	                                terminated — Twilio surface this as a
//	                                completed call)
//	"busy"         → "busy"
//	"no-answer"    → "no-answer"
//	"canceled"     → "canceled"
//	"failed" / ""  → "failed"     (5xx, codec_mismatch, auth_failed,
//	                                trunk_5xx, caller_id_rejected, error)
func dialTerminalEventName(status string) string {
	switch status {
	case "answered", "hangup-star":
		return "completed"
	// NOTE: "completed" is deliberately NOT a case here —
	// no producer in Forwarder.Dial ever sets result.Status="completed".
	// Natural-end maps "answered"→"completed" via the case above.
	// "hangup-star" (caller pressed *) maps the same way. Future natural-
	// completion status values (if any) fall through to the "failed"
	// default until a dedicated producer is added with a dedicated test.
	case "busy":
		return "busy"
	case "no-answer":
		return "no-answer"
	case "canceled":
		return "canceled"
	default:
		// "failed", "" (unset), and any future status string fall through
		// to "failed" so we always emit a terminal event for every non-
		// guardrails-rejected dial.
		return "failed"
	}
}

// ReadBye routes an incoming BYE to the matching outbound dialog managed by
// this forwarder's factory. The Server-side OnBye handler (handler.onBye)
// delegates here as a fallback when the inbound DialogServerCache has no
// matching dialog — this is how the bridge handles the case where the
// callee hangs up first on a B2BUA <Dial> bridge.
//
// Returns sipgo's ErrDialogDoesNotExists when the BYE doesn't match any
// outbound dialog tracked by this Forwarder. Returns nil on successful
// routing (dialog state advances to Terminated, dlg.Done() closes,
// awaitDialogEnd unblocks, Forwarder.Dial returns).
func (f *Forwarder) ReadBye(req *siplib.Request, tx siplib.ServerTransaction) error {
	return f.factory.ReadBye(req, tx)
}

// resolveCallerID applies the caller-ID fallback chain:
//
//	opts.CallerID  →  callerFrom (preserve ANI)  →  defaultCID  →  ErrCallerIDRequired
//
// The result is whatever URI string the caller provided — the Forwarder
// wraps it into a siplib.Uri later. Returns ErrCallerIDRequired (Twilio
// error 13214) when all three sources are empty.
// resolveCallerID applies the caller-ID fallback chain.
//
// Precedence (highest first):
//  1. TwiML <Dial callerId="..."> attribute — explicit operator intent
//     (Twilio standard; always wins when present).
//  2. cfg.DialDefaultCallerID — explicit operator-wide configuration.
//  3. cfg.SIPUser — the registered SIP authentication username. Required
//     by some carrier trunks in the From URI (sipgate's policy is to
//     reject any other From with "Username in From Field required" 403 —
//     for instance). For trunks that accept any verified DID, this
//     fallback is effectively a no-op when steps 1/2 are configured. For
//     SIP trunks where it isn't a phone number (e.g. sipgate's
//     "2301086t3"), Phone B sees the username string as Caller-ID —
//     functional but ugly; operator can configure step 2 for cleaner
//     display.
//  4. opts.CallerFrom — the inbound caller's From URI ("preserve-ANI").
//     Last-resort fallback for the (rare) case where the SIP_USER is
//     unset; preserves Twilio-style behavior on trunks that accept
//     third-party ANI.
//  5. ErrCallerIDRequired (Twilio code 13214).
func resolveCallerID(opts DialOpts, callerFrom, defaultCID, sipUser string) (string, error) {
	if v := strings.TrimSpace(opts.CallerID); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(defaultCID); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(sipUser); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(callerFrom); v != "" {
		return v, nil
	}
	return "", ErrCallerIDRequired
}

// resolveDisplayCallerID returns the caller-ID that should be PRESENTED to
// the callee (the number Phone B sees), independent of what we put in the
// From URI for auth purposes.
//
// Used to build the From display-name AND the P-Preferred-Identity
// header (RFC 3325 §9.2). On SIP trunks that demand SIP_USER in From for
// auth (sipgate), this lets us still surface a meaningful number to the
// callee — restoring Twilio-style preserve-ANI behaviour at the display
// layer while staying compatible with the trunk's auth-policy at the
// From layer.
//
// Precedence (highest first):
//  1. TwiML <Dial callerId="..."> — Twilio-standard explicit override.
//  2. cfg.DialDefaultCallerID env — explicit operator-wide default.
//  3. opts.CallerFrom (preserve-ANI) — Twilio default.
//  4. "" — caller did not specify and the inbound From was missing; the
//     callee falls back to whatever the trunk presents (typically the From
//     URI). The Forwarder treats "" as "skip the P-Asserted-Identity header".
func resolveDisplayCallerID(opts DialOpts, callerFrom, defaultCID string) string {
	if v := strings.TrimSpace(opts.CallerID); v != "" {
		return v
	}
	if v := strings.TrimSpace(defaultCID); v != "" {
		return v
	}
	if v := strings.TrimSpace(callerFrom); v != "" {
		return v
	}
	return ""
}

// normaliseTrunkCallerID converts a phone-number string into the format
// sipgate's trunking documentation requires for the From display-name and
// the P-Preferred-Identity user-part: international format without "+" or
// "00" prefix, and without a leading "0" on the area code.
//
// Transformation rules (applied in order):
//  1. Trim whitespace and strip a leading "+" if present.
//  2. Strip a leading "00" (international-prefix dialling).
//  3. If a country code is supplied AND the remainder still starts with a
//     single "0" (national format), replace that "0" with the country
//     code. cc="" disables this step (numbers stay as-is).
//
// Inputs that are NOT phone numbers (e.g. SIP usernames like "2301086t3"
// when the operator deliberately set DIAL_DEFAULT_CALLER_ID to the SIP_USER)
// pass through unchanged because none of the rules match.
//
// Reference: https://help.sipgate.de/trunking/wie-setze-ich-bei-sipgate-trunking-die-absenderrufnummer
func normaliseTrunkCallerID(s, countryCode string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "+")
	if strings.HasPrefix(s, "00") {
		s = s[2:]
	}
	cc := strings.TrimSpace(countryCode)
	if cc != "" && len(s) > 1 && s[0] == '0' {
		s = cc + s[1:]
	}
	return s
}

// isFromRejectionReason matches the SIP reason text patterns trunks use to
// reject the caller-ID in the From header. sipgate uses the literal
// "Username in From Field required". We match a small set of synonyms so
// other carriers' phrasing also lands in the caller_id_rejected bucket
// instead of the generic 4xx error fallback.
func isFromRejectionReason(reason string) bool {
	r := strings.ToLower(reason)
	return strings.Contains(r, "from field") ||
		strings.Contains(r, "from header") ||
		strings.Contains(r, "from-username") ||
		strings.Contains(r, "p-asserted-identity") ||
		strings.Contains(r, "caller-id") ||
		strings.Contains(r, "callerid")
}

// targetURI converts an E.164 target string ("+4915123456") into a SIP URI
// pointing at the sipgate trunk: sip:+4915123456@<sipgate-domain>.
//
// `domain` is cfg.SIPDomain (the registrar domain). The leading "+" is
// preserved because sipgate accepts E.164 in user-part. Whitespace is
// stripped.
func targetURI(target, domain string, port int) siplib.Uri {
	user := strings.TrimSpace(target)
	user = strings.TrimPrefix(user, "sip:")
	user = strings.TrimPrefix(user, "tel:")
	uri := siplib.Uri{
		Scheme: "sip",
		User:   user,
		Host:   domain,
	}
	// Port>0 forces sipgo to route directly to host:port (skips DNS / default
	// 5060 resolution). Used for non-standard trunks, local SBCs, or e2e
	// harness setups. Default 0 preserves sipgo's standard routing.
	if port > 0 {
		uri.Port = port
	}
	return uri
}

// fromURI converts a caller-ID string into a SIP URI suitable for the From
// header. Accepts either a bare E.164 ("+4930555") or a full SIP URI
// ("sip:+4930555@example.de"); strips "tel:" / "sip:" prefixes and uses
// cfg.SIPDomain as the host when the input is a bare number.
func fromURI(callerID, defaultDomain string) siplib.Uri {
	t := strings.TrimSpace(callerID)
	t = strings.TrimPrefix(t, "tel:")
	t = strings.TrimPrefix(t, "sip:")
	if at := strings.Index(t, "@"); at >= 0 {
		// caller-ID already includes a host
		return siplib.Uri{
			Scheme: "sip",
			User:   t[:at],
			Host:   t[at+1:],
		}
	}
	return siplib.Uri{
		Scheme: "sip",
		User:   t,
		Host:   defaultDomain,
	}
}

// ── Dial: the main entry point ────────────────────────────────────────────────
//
// Flow:
//   1. Mint DialCallSid — every Dial returns a CallSid even on failure paths
//      so the action-callback dispatch always has an identifier.
//   2. Guardrails.CheckDial — toll-fraud and rate-limit short-circuit BEFORE
//      any SDP/INVITE work. Failures bypass ForwardAttemptsTotal (which
//      counts only attempts that reach the SIP layer) and go directly to
//      ForwardFailedTotal{reason=...}.
//   3. resolveCallerID — caller-ID fallback chain.
//   4. callee.BuildSDPOffer — allocates the local RTP port + builds PCMU SDP.
//      AcquirePort is wired upstream so the bridge package owns the port
//      lifecycle; the Forwarder never leaks ports.
//   5. factory.Dial — sends INVITE, waits for final response (sipgo handles
//      401/407 digest challenges automatically).
//   6. Inspect resp.StatusCode and dispatch:
//        - 200 → AcceptSDPAnswer; on codec_mismatch ACK + Bye + failed
//        - 200 → success: ACK; return DialResult{Status:"answered"}
//        - 486 → busy
//        - 408 / 480 / ctx.Deadline → no-answer (sipgo auto-CANCEL on ctx)
//        - 603 → rejected
//        - 5xx → trunk_5xx
//        - other → failed
//   7. Metrics: ForwardAttemptsTotal++ on entry, ForwardSuccessTotal++ on
//      success, ForwardFailedTotal{reason}++ on failure,
//      ForwardDurationSeconds{outcome}.Observe on every exit path.
//
// Implementation: the happy path (1)–(7) plus TimeLimit watchdog and
// HangupOnStar DTMF watch.
func (f *Forwarder) Dial(ctx context.Context, callerSid, target string, opts DialOpts, callee LegConfigurer) (*DialResult, error) {
	startedAt := time.Now()
	dialCallSid := identity.NewCallSid()

	result := &DialResult{
		DialCallSid:  dialCallSid,
		DialedTarget: normalizeTarget(target),
	}

	f.log.Info().
		Str("caller_sid", callerSid).
		Str(observability.FieldForwardLegID, dialCallSid).
		Str("target", result.DialedTarget).
		Msg("forwarder: <Dial> start")

	// (2) Guardrails — first action. Toll-fraud / rate-limit failures bypass
	//     ForwardAttemptsTotal (we count only attempts that reach the SIP
	//     layer); they go straight to ForwardFailedTotal{reason}.
	//
	// Rule-1 fix: 15-01's typed errors (ErrSessionRateLimit/ErrGlobalRateLimit/
	// ErrTollFraudBlocked) carry messages that do NOT contain the substrings
	// observability.BucketForwardReason expects ("rate limit", "toll-fraud").
	// Dispatch the typed sentinel directly to the canonical reason bucket
	// here; do NOT rely on string matching of the .Error() output.
	if err := f.guardrails.CheckDial(callerSid, target); err != nil {
		reason := guardrailsReason(err)
		result.Reason = reason
		result.Status = "failed"
		f.metrics.ForwardFailedTotal.WithLabelValues(reason).Inc()
		f.metrics.ForwardDurationSeconds.WithLabelValues(bucketOutcome(result.Status)).Observe(time.Since(startedAt).Seconds())
		return result, fmt.Errorf("forwarder: guardrails rejected dial: %w", err)
	}

	// All subsequent failures count as forward "attempts" in metrics.
	f.metrics.ForwardAttemptsTotal.Inc()

	// Emit "initiated" event on the <Dial> callee leg subscription,
	// AFTER guardrails pass (so blocked dials don't
	// surface as "initiated" to customers) and BEFORE we actually send
	// the INVITE on the wire. Fire-and-forget; statusWC is nil-safe.
	f.emitDialInitiated(opts, callerSid, dialCallSid, result.DialedTarget, f.log)

	// Free the per-DialCallSid SequenceNumber counter on every exit path.
	// ReleaseLegSequence is idempotent (no-op on unknown SID), so the
	// defer is safe even when nextLegSequence was never called for this
	// dial (the SubscriptionMatches=false branch where emitDialEvent
	// short-circuits before allocating a counter). Without this,
	// Forwarder.legSeqs sync.Map would grow by one *atomic.Uint64 per
	// <Dial> callee leg over process lifetime.
	//
	// Placement: AFTER emitDialInitiated so the defer covers EVERY exit
	// path that could have allocated a counter (success / busy / no-answer
	// / failed / canceled / codec-mismatch / RespondSDP-failure / etc.).
	// The guardrails-block early return happens BEFORE this defer is
	// registered → no counter to free (nextLegSequence wasn't called).
	defer f.ReleaseLegSequence(dialCallSid)

	// (3) Caller-ID fallback chain.
	// callerID drives the From URI (auth identity).
	// displayCID drives P-Asserted-Identity (what the callee sees).
	// They are decoupled so SIP trunks that demand SIP_USER in From
	// (sipgate) can still present a meaningful Caller-ID to Phone B.
	callerID, err := resolveCallerID(opts, opts.CallerFrom, f.cfg.DialDefaultCallerID, f.cfg.SIPUser)
	if err != nil {
		f.recordFailure(err, startedAt, opts, callerSid, result, "")
		return result, err
	}

	// (4) Build SDP offer + acquire local RTP port (the bridge package
	//     owns the port acquire/release lifecycle).
	offerSDP, _, err := callee.BuildSDPOffer()
	if err != nil {
		wrapped := fmt.Errorf("forwarder: build SDP offer: %w", err)
		f.recordFailure(wrapped, startedAt, opts, callerSid, result, "")
		return result, wrapped
	}

	// (5) Construct outbound INVITE + send via shared agent.Client.
	to := targetURI(target, f.cfg.SIPDomain, f.cfg.SIPOutboundTargetPort)
	from := fromURI(callerID, f.cfg.SIPDomain)

	// Display caller-ID — what Phone B should see. Decoupled from `from`
	// (the auth identity) so trunks that demand the SIP_USER in From's
	// addr-spec (sipgate) can still surface the desired number to the
	// callee. We send this on TWO axes for carrier-portability:
	//   - From header display-name: "display" <sip:user@host> form.
	//     Most carriers honour this as the Caller-ID shown to the callee.
	//   - P-Preferred-Identity (RFC 3325 §9.2): the UA-side header that
	//     tells the trust-domain proxy "this is the identity I would like
	//     you to assert on my behalf". The proxy may then issue PAI
	//     downstream after authenticating us.
	// Both are omitted when the display CID matches From's user (would be
	// redundant). When they differ we set both — the wire bytes are tiny
	// and one of the two is much more likely to win at the carrier.
	displayCID := strings.TrimSpace(resolveDisplayCallerID(opts, opts.CallerFrom, f.cfg.DialDefaultCallerID))
	var (
		ppiURI      *siplib.Uri
		fromDisplay string
	)
	if displayCID != "" {
		u := fromURI(displayCID, f.cfg.SIPDomain)
		// Normalise the user-part to the format sipgate trunking documents
		// for both PPI and From display-name: E.164 without "+" / "00",
		// no leading "0" on the area code. Examples:
		//   "+4921193674951"  → "4921193674951"
		//   "004921193674951" → "4921193674951"
		//   "021193674951"    → "4921193674951" (with cc=49)
		// Reference: https://help.sipgate.de/trunking/wie-setze-ich-bei-sipgate-trunking-die-absenderrufnummer
		u.User = normaliseTrunkCallerID(u.User, f.cfg.DialCallerIDCountryCode)
		// Only emit when the display CID would actually surface a different
		// number than From's addr-spec. We normalise From's user-part for
		// the comparison only — From itself stays as-is so the trunk's
		// auth check still sees the literal SIP_USER (sipgate policy).
		if u.User != normaliseTrunkCallerID(from.User, f.cfg.DialCallerIDCountryCode) {
			ppiURI = &u
			fromDisplay = u.User // just the user-part — never the full "sip:user@host" URI
		}
	}

	// Split the outbound-INVITE log into a redacted Info entry (correlation
	// + timeout only — operator-monitoring) and a Debug entry (phone
	// numbers + display name + PPI — engineering trace). from / to /
	// from_display_name / p_preferred_identity carry full SIP URIs that
	// include the callee phone number. The "phone-number / URL
	// debug-only" rule bars these from Info+ output.
	f.log.Info().
		Str(observability.FieldForwardLegID, dialCallSid).
		Dur("ring_timeout", time.Duration(f.cfg.DialRingTimeoutS)*time.Second).
		Msg("forwarder: sending outbound INVITE")
	logEvt := f.log.Debug().
		Str(observability.FieldForwardLegID, dialCallSid).
		Str("from", from.String()).
		Str("from_display_name", fromDisplay).
		Str("to", to.String()).
		Dur("ring_timeout", time.Duration(f.cfg.DialRingTimeoutS)*time.Second)
	if ppiURI != nil {
		logEvt = logEvt.Str("p_preferred_identity", ppiURI.String())
	}
	logEvt.Msg("forwarder: sending outbound INVITE (detail)")

	// Apply ring timeout. opts.Timeout overrides the config default. Zero
	// means "use cfg.DialRingTimeoutS".
	ringTimeout := opts.Timeout
	if ringTimeout <= 0 {
		ringTimeout = time.Duration(f.cfg.DialRingTimeoutS) * time.Second
	}
	dialCtx, cancelDial := context.WithTimeout(ctx, ringTimeout)
	defer cancelDial()

	// onResponse hook: record auth-challenge kind metric for 401/407, and
	// emit the "ringing" status callback on the FIRST 180 received
	// (sync.Once de-dupes 180 retransmissions). sipgo handles the digest
	// re-INVITE automatically when AnswerOptions carries Username+Password;
	// the 401/407 branches are purely observational.
	var ringingOnce sync.Once
	onResponse := func(res *siplib.Response) error {
		switch res.StatusCode {
		case 180, 183:
			// Emit "ringing" exactly once on the <Dial> callee leg
			// subscription. sync.Once dedupes 180 retransmissions and
			// 180-followed-by-183 sequences.
			ringingOnce.Do(func() {
				f.emitDialRinging(opts, callerSid, dialCallSid, result.DialedTarget, f.log)
			})
		case siplib.StatusUnauthorized:
			result.AuthChallenge = "401"
			f.metrics.AuthChallengeKind.WithLabelValues("401").Inc()
		case siplib.StatusProxyAuthRequired:
			result.AuthChallenge = "407"
			f.metrics.AuthChallengeKind.WithLabelValues("407").Inc()
		}
		return nil
	}

	// sipgo's WaitAnswer is meant to honour ctx.Done() and surface
	// context.DeadlineExceeded when the ring window expires before a
	// final response. Empirically (sipgo v1.3.x) it can miss this
	// signal for INVITE flows that received 100 Trying but no further
	// provisional response, blocking until sipgo's internal Timer B
	// (~32s) fires. The watchdog below races the underlying Dial call
	// against an explicit ringTimeout deadline and surfaces no-answer
	// at the wire-level deadline, regardless of sipgo's behaviour.
	type dialOutcome struct {
		client DialClient
		err    error
	}
	dialDone := make(chan dialOutcome, 1)
	go func() {
		c, e := f.factory.Dial(dialCtx, to, from, fromDisplay, ppiURI, offerSDP, DialAuth{
			Username: f.cfg.SIPUser,
			Password: f.cfg.SIPPassword,
		}, onResponse)
		dialDone <- dialOutcome{c, e}
	}()

	var (
		dlg     DialClient
		dialErr error
	)
	// 200 ms grace past the ring timeout so a sipgo dial that genuinely
	// completes at the deadline (legit no-answer with proper CANCEL/487
	// roundtrip) wins the race over the watchdog.
	select {
	case r := <-dialDone:
		dlg = r.client
		dialErr = r.err
	case <-time.After(ringTimeout + 200*time.Millisecond):
		// sipgo missed the ctx.Done() signal. Force-cancel and surface
		// no-answer; reap the goroutine asynchronously so the caller
		// is not blocked by sipgo's Timer B.
		cancelDial()
		dialErr = fmt.Errorf("forwarder: ring timeout: %w", context.DeadlineExceeded)
		go func() {
			r := <-dialDone
			if r.client != nil {
				_ = r.client.Close()
			}
		}()
	}
	if dlg != nil {
		defer dlg.Close()
	}

	// (6) Map sipgo error / final response → DialResult.
	if dialErr != nil {
		// Distinguish: ctx-deadline → no-answer (CANCEL), context.Canceled →
		// canceled, ErrDialogResponse → final non-2xx.
		var dlgErr *sipgo.ErrDialogResponse
		switch {
		case errors.As(dialErr, &dlgErr):
			f.dispatchFinalResponse(dlgErr.Res, result)
			f.recordFailure(fmt.Errorf("forwarder: dial: %w", dialErr), startedAt, opts, callerSid, result, result.Status)
			return result, fmt.Errorf("forwarder: dial: %w", dialErr)
		case errors.Is(dialErr, context.DeadlineExceeded):
			result.Status = "no-answer"
			result.Reason = "no_answer"
			f.recordFailure(dialErr, startedAt, opts, callerSid, result, result.Status)
			return result, fmt.Errorf("forwarder: ring timeout: %w", dialErr)
		case errors.Is(dialErr, context.Canceled):
			result.Status = "canceled"
			result.Reason = "canceled"
			f.recordFailure(dialErr, startedAt, opts, callerSid, result, result.Status)
			return result, fmt.Errorf("forwarder: dial canceled: %w", dialErr)
		default:
			result.Status = "failed"
			result.Reason = "error"
			f.recordFailure(dialErr, startedAt, opts, callerSid, result, result.Status)
			return result, fmt.Errorf("forwarder: dial: %w", dialErr)
		}
	}

	// dialErr == nil → 2xx final response. Inspect it.
	resp := dlg.FinalResponse()
	if resp == nil {
		// Defensive — should not happen with sipgo's WaitAnswer on success.
		err := errors.New("forwarder: dial succeeded but final response missing")
		result.Status = "failed"
		result.Reason = "error"
		f.recordFailure(err, startedAt, opts, callerSid, result, result.Status)
		return result, err
	}
	result.SIPFinalCode = int(resp.StatusCode)

	if resp.StatusCode != siplib.StatusOK {
		// Non-2xx success-path — 1xx unhandled, 3xx redirect — treat as failed.
		result.Status = "failed"
		result.Reason = "error"
		err := fmt.Errorf("forwarder: unexpected non-2xx final %d %s", resp.StatusCode, resp.Reason)
		f.recordFailure(err, startedAt, opts, callerSid, result, result.Status)
		return result, err
	}

	// 200 OK — accept the SDP answer. Codec mismatch → ACK + immediate Bye.
	if err := callee.AcceptSDPAnswer(resp.Body()); err != nil {
		// ACK first per RFC 3261 §17.1.1.3 — even when we intend to BYE.
		if ackErr := dlg.Ack(ctx); ackErr != nil {
			f.log.Warn().Err(ackErr).Msg("forwarder: ACK failed before codec-mismatch BYE")
		}
		byeCtx, cancelBye := context.WithTimeout(context.Background(), 5*time.Second)
		_ = dlg.Bye(byeCtx)
		cancelBye()

		result.Status = "failed"
		result.Reason = "codec_mismatch"
		wrapped := fmt.Errorf("forwarder: %w", err)
		f.recordFailure(wrapped, startedAt, opts, callerSid, result, result.Status)
		return result, wrapped
	}

	// Successful answer — send ACK to confirm dialog.
	if err := dlg.Ack(ctx); err != nil {
		result.Status = "failed"
		result.Reason = "error"
		wrapped := fmt.Errorf("forwarder: ACK after 200 OK: %w", err)
		f.recordFailure(wrapped, startedAt, opts, callerSid, result, result.Status)
		return result, wrapped
	}

	// Dual-leg activation hook: open the callee RTP socket, spawn the relay
	// goroutines, and transition the session state to StateForwarding so the
	// caller-leg rtpReader stops dropping audio into the WS-bound queue and
	// starts pushing it onto the callee leg's outboundQueue. If the hook
	// fails the dialog is established but no audio can bridge — BYE the
	// callee and surface a "failed" DialResult.
	if err := callee.OnAnswered(); err != nil {
		byeCtx, cancelBye := context.WithTimeout(context.Background(), 5*time.Second)
		_ = dlg.Bye(byeCtx)
		cancelBye()

		result.Status = "failed"
		result.Reason = "error"
		wrapped := fmt.Errorf("forwarder: OnAnswered: %w", err)
		f.recordFailure(wrapped, startedAt, opts, callerSid, result, result.Status)
		return result, wrapped
	}

	// (7) Dialog confirmed — enter the dual-leg lifecycle phase.
	// answeredAt is the wall-clock instant we transitioned to "answered".
	// Duration in DialResult is the talk time from this instant onwards;
	// the ring/INVITE setup time is intentionally excluded so the metric
	// matches the Twilio Duration semantics (talk time only).
	answeredAt := time.Now()
	f.metrics.ForwardSuccessTotal.Inc()

	// Emit "answered" event on the <Dial> callee leg subscription.
	// CallStatus="in-progress" — Twilio's event/status divergence.
	// Fire-and-forget; statusWC nil-safe.
	f.emitDialAnswered(opts, callerSid, dialCallSid, result.DialedTarget, f.log)

	f.log.Info().
		Str(observability.FieldForwardLegID, dialCallSid).
		Int("sip_final_code", result.SIPFinalCode).
		Dur("ring_setup", answeredAt.Sub(startedAt)).
		Msg("forwarder: <Dial> answered — dual-leg bridge active")

	// Wait for one of:
	//   (a) dialog terminates naturally (callee BYE → dlg.Done() fires)
	//   (b) timeLimit watchdog fires (we BYE the dialog)
	//   (c) hangupOnStar receives '*' (we BYE the dialog)
	//   (d) outer ctx canceled (caller leg hung up first → propagate BYE)
	exitStatus := f.awaitDialogEnd(ctx, dlg, opts)

	result.Status = exitStatus
	result.Duration = time.Since(answeredAt)
	f.metrics.ForwardDurationSeconds.WithLabelValues(bucketOutcome("answered")).Observe(result.Duration.Seconds())

	f.log.Info().
		Str(observability.FieldForwardLegID, dialCallSid).
		Str("status", result.Status).
		Dur("talk_time", result.Duration).
		Msg("forwarder: <Dial> end")

	// Success path: emit the <Dial>-leg "completed" terminal event. The
	// dialog answered (200 OK + ACK + OnAnswered) and has now naturally
	// terminated via one of the four pathways in awaitDialogEnd — natural
	// BYE, timeLimit, hangup-star, or outer-ctx cancel. All four surface
	// as a Twilio "completed" terminal callback. The recordFailure
	// chokepoint covers ALL non-success exits; this site covers the
	// single success exit.
	f.emitDialCompleted(opts, callerSid, dialCallSid, result.DialedTarget, f.log)

	// Success-path explicit per-leg cleanup at the terminal-dispatch site.
	// ReleaseLegSequence is idempotent — the existing earlier defer is a
	// safety net. DrainAndClose runs in a goroutine so a hung customer
	// host does not block Forwarder.Dial's return.
	f.ReleaseLegSequence(dialCallSid)
	if f.statusWC != nil {
		go func(d string) {
			_ = f.statusWC.DrainAndClose(d, statusDrainBudget)
		}(dialCallSid)
	}

	return result, nil
}

// awaitDialogEnd blocks until the confirmed dialog terminates by one of the
// four pathways documented in Dial:
//
//	natural-end  →  callee sent BYE; dlg.Done() fires; return "answered"
//	timeLimit    →  opts.TimeLimit elapsed; we BYE the dialog; return "answered"
//	hangup-star  →  opts.HangupOnStar && DTMFChan delivered '*'; we BYE; return "hangup-star"
//	ctx-cancel   →  outer ctx canceled (caller leg hung up); we BYE; return "answered"
//
// timeLimit and hangupOnStar goroutines are managed
// inline here. All goroutines exit cleanly via the dlg.Done() channel
// regardless of which terminator wins — no leaks under -race -count=3.
func (f *Forwarder) awaitDialogEnd(ctx context.Context, dlg DialClient, opts DialOpts) string {
	// timeLimit watchdog. Only spawn when opts.TimeLimit > 0.
	// The goroutine exits when either the timer fires (we BYE) or the dialog
	// terminates by another route (dlg.Done() closes).
	timeLimitFired := make(chan struct{})
	if opts.TimeLimit > 0 {
		go func() {
			timer := time.NewTimer(opts.TimeLimit)
			defer timer.Stop()
			select {
			case <-timer.C:
				byeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = dlg.Bye(byeCtx)
				close(timeLimitFired)
			case <-dlg.Done():
				// Dialog ended by another route — exit silently.
			}
		}()
	}

	// hangupOnStar DTMF watch. Only spawn when both the flag and the
	// channel are set. The wire-up plumbs DTMFChan from
	// bridge.session.dtmfQueue; the channel is left nil in production
	// when no DTMF is wired and is supplied by tests directly.
	starReceived := make(chan struct{})
	if opts.HangupOnStar && opts.DTMFChan != nil {
		go func() {
			for {
				select {
				case digit, ok := <-opts.DTMFChan:
					if !ok {
						// channel closed — caller leg gone; let dlg.Done() handle
						return
					}
					if digit == '*' {
						// Race-fix: close(starReceived) BEFORE Bye. If we Bye'd
						// first, dlg.Done() can close immediately; the outer
						// select might then pick the dlg.Done() branch and the
						// inner non-blocking starReceived check would falsely
						// fall through to "answered". Closing first guarantees
						// any dlg.Done() observer also sees starReceived closed.
						close(starReceived)
						byeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						_ = dlg.Bye(byeCtx)
						cancel()
						return
					}
				case <-dlg.Done():
					return
				}
			}
		}()
	}

	// Block until one of the terminators wins.
	select {
	case <-dlg.Done():
		// Dialog terminated naturally — could be callee BYE OR our own Bye()
		// from the timeLimit / star goroutines. Distinguish by checking the
		// non-blocking state of those signals.
		select {
		case <-starReceived:
			return "hangup-star"
		default:
		}
		select {
		case <-timeLimitFired:
			return "answered" // timeLimit-driven end is still a successful call
		default:
		}
		return "answered"

	case <-ctx.Done():
		// Outer ctx canceled (e.g. caller leg hung up). BYE the callee.
		byeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = dlg.Bye(byeCtx)
		cancel()
		return "answered"

	case <-starReceived:
		// hangup-star fired before we even noticed dlg.Done(). Wait for the
		// dialog to actually end (the goroutine already issued Bye), then
		// report.
		select {
		case <-dlg.Done():
		case <-time.After(5 * time.Second):
			// Defensive: if Bye doesn't terminate the dialog within 5s,
			// proceed anyway — the dialog will be cleaned up by Close.
		}
		return "hangup-star"

	case <-timeLimitFired:
		select {
		case <-dlg.Done():
		case <-time.After(5 * time.Second):
		}
		return "answered"
	}
}

// dispatchFinalResponse maps a non-2xx final SIP response onto a DialResult.
// SIP-status → Twilio-status mapping per FORWARD-* requirements:
//
//	486 / 600          → busy
//	408 / 480 / 487    → no-answer  (487 = CANCEL acknowledged, treat as no-answer)
//	603                → failed/rejected
//	5xx                → failed/trunk_5xx
//	default            → failed/error
func (f *Forwarder) dispatchFinalResponse(resp *siplib.Response, result *DialResult) {
	if resp == nil {
		result.Status = "failed"
		result.Reason = "error"
		return
	}
	result.SIPFinalCode = int(resp.StatusCode)
	switch {
	case resp.StatusCode == siplib.StatusBusyHere || resp.StatusCode == 600:
		result.Status = "busy"
		result.Reason = "busy"
	case resp.StatusCode == siplib.StatusRequestTimeout ||
		resp.StatusCode == siplib.StatusTemporarilyUnavailable ||
		resp.StatusCode == siplib.StatusRequestTerminated:
		result.Status = "no-answer"
		result.Reason = "no_answer"
	case resp.StatusCode == 403 && isFromRejectionReason(resp.Reason):
		// sipgate-style "Username in From Field required" — the trunk
		// rejected our caller-ID. Bucket as caller_id_rejected (Twilio code
		// 13214) so operators see the right metric and can fix via either
		// TwiML callerId or DIAL_DEFAULT_CALLER_ID + DIAL_PREFER_DEFAULT_CALLER_ID.
		result.Status = "failed"
		result.Reason = "caller_id_rejected"
	case resp.StatusCode == 603:
		result.Status = "failed"
		result.Reason = "rejected"
	case resp.StatusCode >= 500 && resp.StatusCode < 600:
		result.Status = "failed"
		result.Reason = "trunk_5xx"
	case resp.StatusCode == siplib.StatusUnauthorized || resp.StatusCode == siplib.StatusProxyAuthRequired:
		// sipgo only surfaces a 401/407 as final when its own re-INVITE with
		// digest creds also failed — i.e. the credentials are wrong.
		result.Status = "failed"
		result.Reason = "auth_failed"
	default:
		result.Status = "failed"
		result.Reason = "error"
	}
}

// recordFailure is the chokepoint for SIP-layer dial failures (post-
// guardrails, post-emitDialInitiated). Eleven Forwarder.Dial exit paths
// route through this helper. The TWELFTH exit — guardrails block at
// guardrails-rejected dials DELIBERATELY BYPASS recordFailure: rejection
// fires BEFORE emitDialInitiated, so no per-leg counter has been
// allocated, no "initiated" event has been emitted, and a blocked dial
// must NOT surface as a terminal status callback to the customer
// (rejected dials are a security posture, not a customer-visible call
// lifecycle event). The bypass increments forward_failed_total{reason} +
// forward_duration_seconds{outcome} directly at the bypass site so the
// metrics still reflect the dial count.
//
// recordFailure increments ForwardFailedTotal with the reason bucket and
// records ForwardDurationSeconds with the outcome bucket. The outcome is
// resolved from result.Status via bucketOutcome unconditionally (see body
// comment). ForwardAttemptsTotal is NOT incremented here — callers do
// that separately when the failure is past the guardrails check.
//
// The signature accepts opts + callerSid so this chokepoint emits the
// matching <Dial>-leg terminal status callback (per dialTerminalEventName
// mapping) and triggers per-leg state cleanup (ReleaseLegSequence +
// non-blocking DrainAndClose). All callers thread these through from
// Forwarder.Dial's local scope.
//
// bucketOutcome runs unconditionally inside this body — the outcome param
// is preserved for callsite-stability but is re-bucketed before the
// histogram emit. The allowlist {answered|no_answer|busy|error} is
// enforced here, not at the callsites.
func (f *Forwarder) recordFailure(err error, startedAt time.Time, opts DialOpts, callerSid string, result *DialResult, outcome string) {
	// Prefer the reason already set on result (dispatchFinalResponse / explicit
	// callers populate it with SIP-final-code-aware buckets like
	// caller_id_rejected, busy, no_answer, codec_mismatch). Only fall back to
	// the err-based bucketing when result has no reason yet.
	reason := result.Reason
	if reason == "" {
		reason = observability.BucketForwardReason(err)
	}
	if reason == "" {
		reason = "error"
	}
	if result.Reason == "" {
		result.Reason = reason
	}
	if result.Status == "" {
		// Best-effort fallback — guardrails errors don't set Status.
		switch reason {
		case "toll_fraud", "rate_limit", "caller_id_rejected":
			result.Status = "failed"
		default:
			result.Status = "failed"
		}
	}
	// metrics:dynamic-allowed reason is sourced from result.Reason (set
	// by dispatchFinalResponse with bucketed labels) or
	// observability.BucketForwardReason(err) — both bounded enums; the
	// walker can't trace the multi-branch local-var assignment shape
	// back to a single bucketer, so an explicit opt-out is the cleanest
	// gate.
	f.metrics.ForwardFailedTotal.WithLabelValues(reason).Inc()
	// Callers historically passed raw result.Status (bypassing
	// bucketOutcome and emitting outcome={no-answer,canceled,failed} into
	// the histogram). Bucket unconditionally at this single chokepoint so
	// callers cannot regress the allowlist contract. The `outcome`
	// parameter is preserved for callsite-stability but is re-bucketed
	// here regardless of caller input. The allowlist
	// {answered|no_answer|busy|error} is enforced here, not at the
	// callsites.
	outcome = bucketOutcome(result.Status)
	// bucketOutcome (annotated with // metrics:bucketer below) is the
	// single source for the outcome label — the assignment immediately
	// above guarantees cardinality discipline regardless of caller input.
	// The allowlist {answered|no_answer|busy|error} is enforced here, not
	// at the callsites.
	// metrics:dynamic-allowed outcome: bucketed unconditionally above via bucketOutcome(result.Status)
	f.metrics.ForwardDurationSeconds.WithLabelValues(outcome).Observe(time.Since(startedAt).Seconds())

	f.log.Warn().
		Err(err).
		Str(observability.FieldForwardLegID, result.DialCallSid).
		Str("target", result.DialedTarget).
		Str("reason", reason).
		Str("status", result.Status).
		Int("sip_final_code", result.SIPFinalCode).
		Dur("elapsed", time.Since(startedAt)).
		Msg("forwarder: <Dial> failed")

	// Emit the matching <Dial>-leg terminal status callback at the
	// centralized chokepoint. dialTerminalEventName maps result.Status
	// onto the bounded 5-event vocab {completed, busy, failed, no-answer,
	// canceled}. emitDialEvent is nil-safe on f.statusWC and short-
	// circuits on opts.StatusCallback=="" or SubscriptionMatches=false —
	// so guardrails-rejected dials (which call recordFailure with a
	// zero-value DialOpts) silently no-op as expected.
	eventName := dialTerminalEventName(result.Status)
	f.emitDialEvent(opts, eventName, eventName, callerSid, result.DialCallSid, result.DialedTarget, f.log)

	// Explicit per-leg cleanup at terminal-dispatch time.
	// ReleaseLegSequence is idempotent — the existing earlier defer is a
	// safety net for any future exit path that bypasses recordFailure.
	// DrainAndClose runs in a goroutine so recordFailure does not block on
	// a hung customer host (mirrors the parent-leg pattern in
	// bridge/session.go).
	f.ReleaseLegSequence(result.DialCallSid)
	if f.statusWC != nil {
		go func(d string) {
			_ = f.statusWC.DrainAndClose(d, statusDrainBudget)
		}(result.DialCallSid)
	}
}

// metrics:bucketer
//
// guardrailsReason dispatches a guardrails sentinel error onto the canonical
// reason label used by forward_failed_total. Bounded enum:
//   "toll_fraud" | "rate_limit" | "error".
// Bypasses observability.BucketForwardReason because 15-01's typed sentinels
// carry messages whose substrings ("per-session dial limit", "global per-
// minute dial limit", "dial target not in allow-list") do not match what
// BucketForwardReason expects. Prefer typed errors.Is over substring match.
//
// The // metrics:bucketer annotation registers this helper with the
// cardinality-discipline lint walker as a bounded-output source — the
// walker accepts WithLabelValues(guardrailsReason(err)) without a
// per-call-site // metrics:dynamic-allowed opt-out.
func guardrailsReason(err error) string {
	switch {
	case errors.Is(err, ErrTollFraudBlocked):
		return "toll_fraud"
	case errors.Is(err, ErrSessionRateLimit), errors.Is(err, ErrGlobalRateLimit):
		return "rate_limit"
	default:
		return "error"
	}
}

// metrics:bucketer
//
// bucketOutcome is the sip-package wrapper around observability.BucketOutcome.
// Kept unexported here so the existing in-package call sites (recordFailure,
// dispatchFinalResponse) stay unchanged; the canonical 4-case enum lives in
// internal/observability/metrics.go alongside the other Bucket* helpers
// (single source of truth for the outcome bucket so the sip-package
// chokepoint AND observability-boundary tests exercise the same mapping).
func bucketOutcome(status string) string {
	return observability.BucketOutcome(status)
}
