package bridge

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo"
	siplib "github.com/emiago/sipgo/sip"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/config"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/observability"
	sip "github.com/sipgate/sipgate-sip-stream-bridge/internal/sip"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/webhook"
)

// BridgeCall is the union view of an active CallSession and a recently-terminated
// snapshot, consumed by internal/api for REST serialization. CallSession satisfies
// this directly; *terminatedCall (private value type below) satisfies it for the
// 5-minute grace cache.
//
// All methods MUST return value-copies of primitives — callers iterate without
// locks and the underlying CallSession may be racing with markTerminated on its
// own goroutine. The invariant is: every getter on *CallSession is a
// pure read of an immutable-after-StartSession field, except endTime/termReason
// which are written exactly once by markTerminated() right before the session is
// removed from CallManager.callSidIdx (see the StartSession defer chain).
type BridgeCall interface {
	CallSid() string
	AccountSid() string
	From() string
	To() string
	Direction() string
	Status() string
	StartTime() time.Time
	EndTime() time.Time
	Duration() int
	AnsweredBy() string
	ParentCallSid() string
}

// terminatedCall is an immutable snapshot of a CallSession at termination time,
// retained for the 5-minute grace window so REST GET /Calls/{Sid} can find
// recently-completed calls before the cache sweeper expires them. Built on the
// StartSession goroutine just after markTerminated() stamps endTime, then handed
// off to recentlyTerminated; thereafter the value is read-only.
type terminatedCall struct {
	callSid       string
	accountSid    string
	from          string
	to            string
	direction     string
	status        string
	startTime     time.Time
	endTime       time.Time
	duration      int
	answeredBy    string
	parentCallSid string
}

func (t *terminatedCall) CallSid() string       { return t.callSid }
func (t *terminatedCall) AccountSid() string    { return t.accountSid }
func (t *terminatedCall) From() string          { return t.from }
func (t *terminatedCall) To() string            { return t.to }
func (t *terminatedCall) Direction() string     { return t.direction }
func (t *terminatedCall) Status() string        { return t.status }
func (t *terminatedCall) StartTime() time.Time  { return t.startTime }
func (t *terminatedCall) EndTime() time.Time    { return t.endTime }
func (t *terminatedCall) Duration() int         { return t.duration }
func (t *terminatedCall) AnsweredBy() string    { return t.answeredBy }
func (t *terminatedCall) ParentCallSid() string { return t.parentCallSid }

// recentlyTerminatedTTL is how long a terminated-call snapshot stays in
// CallManager.recentlyTerminated before recentlyTerminatedSweep removes it.
// Twilio's REST API exposes completed calls for hours/days; v3.0 keeps a
// 5-minute in-memory grace only. Persistence is out of scope — bot frameworks
// that need durable history must use statusCallback subscriptions.
const recentlyTerminatedTTL = 5 * time.Minute

// recentlyTerminatedSweepInterval is how often the sweep goroutine wakes up to
// scan recentlyTerminated for expired entries. 30s is well under the 5min TTL.
const recentlyTerminatedSweepInterval = 30 * time.Second

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
//
// Two parallel indices serve the REST API:
//   - callSidIdx        : O(1) lookup by Twilio CallSid for GET /Calls/{Sid}
//   - recentlyTerminated: 5-minute grace cache so just-completed calls remain
//                          visible to REST clients
// Both maps are maintained transactionally inside StartSession's defer chain.
type CallManager struct {
	sessions           sync.Map           // key: callID string → value: *CallSession
	callSidIdx         sync.Map           // key: CallSid string → value: *CallSession (active calls only)
	recentlyTerminated sync.Map           // key: CallSid string → value: *terminatedCall (5min TTL grace)
	sweepCancel        context.CancelFunc // stops the recentlyTerminatedSweep goroutine; called from Close()
	portPool           *PortPool
	accountSid         string // set once at startup via identity.DeriveAccountSid(cfg.SIPUser)
	cfg                config.Config
	log                zerolog.Logger
	metrics            *observability.Metrics

	// Per-process StatusClient threaded onto every CallSession created by
	// StartSession. Nil-safe — when unset, StartSession leaves
	// session.statusWC nil and emission helpers no-op. Wired by main.go via
	// SetStatusClient at boot AFTER the StatusClient has been constructed;
	// tests that don't exercise the emission path leave this nil
	// unconditionally.
	statusWC *webhook.StatusClient
}

// NewCallManager creates a CallManager with the given port pool, accountSid, config, logger, and metrics.
// Starts the recentlyTerminatedSweep goroutine; main.go must defer Close() to stop it cleanly.
//
// The per-process StatusClient is NOT a constructor parameter — it is
// wired post-construction via SetStatusClient. This keeps the constructor
// signature stable across the many bridge.NewCallManager call sites in
// the test suite (most tests don't need a StatusClient). Production main.go
// calls SetStatusClient once at boot.
func NewCallManager(portPool *PortPool, accountSid string, cfg config.Config, log zerolog.Logger, metrics *observability.Metrics) *CallManager {
	sweepCtx, sweepCancel := context.WithCancel(context.Background())
	cm := &CallManager{
		portPool:    portPool,
		accountSid:  accountSid,
		cfg:         cfg,
		log:         log,
		metrics:     metrics,
		sweepCancel: sweepCancel,
	}
	go cm.recentlyTerminatedSweep(sweepCtx)
	return cm
}

// Close stops the recentlyTerminatedSweep goroutine. Call from main.go's
// shutdown path via defer; safe to call multiple times (sweepCancel itself is
// idempotent and the nil-guard handles a manager that was never started).
func (m *CallManager) Close() {
	if m.sweepCancel != nil {
		m.sweepCancel()
	}
}

// SetStatusClient injects the per-process StatusClient that StartSession
// will thread onto every CallSession created hereafter. Wired by main.go
// at boot AFTER webhook.NewStatusClient succeeds.
//
// Nil-safe: when unset, every CallSession.statusWC stays nil and the
// terminal-event emission helper short-circuits before touching any
// network surface. Tests that don't construct a StatusClient continue to
// pass without modification.
//
// Single-writer: invoked once at boot. There is no race against concurrent
// StartSession calls in practice because main.go sets this BEFORE
// callManager is exposed to any SIP/REST surface.
func (m *CallManager) SetStatusClient(sc *webhook.StatusClient) {
	m.statusWC = sc
}

// recentlyTerminatedSweep wakes every 30s and removes any terminatedCall whose
// endTime is older than recentlyTerminatedTTL (5min). Exits on ctx.Done().
// Bounded memory: at peak ~5min × max-call-rate; for sipgate v3.0 volume the
// cache stays well under 1MB even under sustained load.
func (m *CallManager) recentlyTerminatedSweep(ctx context.Context) {
	ticker := time.NewTicker(recentlyTerminatedSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			m.sweepRecentlyTerminatedOnce(now)
		}
	}
}

// sweepRecentlyTerminatedOnce performs a single pass over recentlyTerminated
// and deletes entries whose endTime is older than recentlyTerminatedTTL relative
// to `now`. Extracted from the ticker loop so tests can exercise the eviction
// logic deterministically without waiting for the 30s production tick.
func (m *CallManager) sweepRecentlyTerminatedOnce(now time.Time) {
	m.recentlyTerminated.Range(func(key, value any) bool {
		t := value.(*terminatedCall)
		if now.UTC().Sub(t.endTime) > recentlyTerminatedTTL {
			m.recentlyTerminated.Delete(key)
		}
		return true
	})
}

// GetByCallSid returns an active session if present, otherwise a recently-terminated
// snapshot if within the 5-minute grace window. The second return value is false
// when neither lookup hit. Active wins on duplicate-key races (a stale terminatedCall
// under the same CallSid as a live session is shadowed — see the chaos test).
func (m *CallManager) GetByCallSid(callSid string) (BridgeCall, bool) {
	if v, ok := m.callSidIdx.Load(callSid); ok {
		return v.(*CallSession), true
	}
	if v, ok := m.recentlyTerminated.Load(callSid); ok {
		return v.(*terminatedCall), true
	}
	return nil, false
}

// List returns active calls plus recently-terminated calls (within the 5-min
// grace window) sorted StartTime-descending. The slice is a snapshot — callers
// may iterate without locks. Underlying *CallSession state may change concurrently
// but BridgeCall getters are read-safe (the package guarantees: getters
// return copy-by-value primitives, no internal references).
//
// Sorting on StartTime descending is deterministic even when sync.Map iteration
// order is not, satisfying threat row "List() ordering nondeterminism".
func (m *CallManager) List() []BridgeCall {
	var out []BridgeCall
	m.callSidIdx.Range(func(_, value any) bool {
		out = append(out, value.(*CallSession))
		return true
	})
	m.recentlyTerminated.Range(func(_, value any) bool {
		out = append(out, value.(*terminatedCall))
		return true
	})
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartTime().After(out[j].StartTime())
	})
	return out
}

// AccountSid returns the AccountSid derived at startup. Satisfies sip.CallManagerIface.
func (m *CallManager) AccountSid() string { return m.accountSid }

// ActiveCount returns the number of currently active call sessions.
// Used by the /health endpoint and the DrainAll polling loop.
func (m *CallManager) ActiveCount() int {
	count := 0
	m.sessions.Range(func(_, _ any) bool { count++; return true })
	return count
}

// ActiveForwardCount returns the count of CallSessions currently in any of
// the "active outbound forwarding" states: StateForwardingSetup,
// StateDialingOut, or StateForwarding (these align with the canonical
// Status() helper which classifies these three states + Streaming +
// Redirected as "in-progress" — only the three forwarding states count
// here).
//
// Used by the /health endpoint to populate active_forwards. Bounded
// latency: sync.Map.Range + atomic state.Load — same idiom as ActiveCount(),
// no synchronous registrar reads or pool counts. <5ms p99 on 1000 sessions
// (locked SLO; CI_SLOW_HOST=true is the only documented escape hatch —
// see TestActiveForwardCount_LatencyBound).
//
// sync.Map.Range tolerates concurrent Store/Delete; counts may be slightly
// stale (eventual consistency) which is acceptable for /health semantics.
func (m *CallManager) ActiveForwardCount() int {
	count := 0
	m.sessions.Range(func(_, value any) bool {
		sess, ok := value.(*CallSession)
		if !ok {
			return true
		}
		switch sess.state.Load() {
		case StateForwardingSetup, StateDialingOut, StateForwarding:
			count++
		}
		return true
	})
	return count
}

// DrainAll terminates every active call (BYE every leg) and waits until all
// sessions self-exit. Sessions call m.sessions.Delete(callID) via defer in
// StartSession when their goroutine exits. Uses polling (50ms tick) rather
// than a drain WaitGroup to avoid modifying the session close path.
//
// CRITICAL: call handler.SetShutdown() BEFORE DrainAll to prevent new sessions
// during drain.
//
// Architectural-gap fix: route through s.Terminate("shutdown") so the
// per-session terminateOnce + dual-leg fan-out BYEs BOTH legs (parent +
// dial). The previous primary().dlg.Bye() only BYEd legs[0] and leaked
// dial-leg dialogs in sipgo's dialog cache + their RTP ports.
func (m *CallManager) DrainAll(ctx context.Context) {
	// Terminate every active session — Terminate's leg loop honors dual-leg
	// sessions and per-leg sync.Once collapses any concurrent BYE attempts.
	m.sessions.Range(func(_, value any) bool {
		s := value.(*CallSession)
		m.log.Info().Str("call_id", s.callID).Msg("shutdown: terminating active call (dual-leg drain)")
		// Use Terminate so per-session sync.Once + dual-leg fan-out at
		// session.go:1797-1812 BYE BOTH legs (parent + dial). The previous
		// primary().dlg.Bye() only BYEd legs[0] and leaked dial-leg dialogs
		// in sipgo's cache + their RTP ports.
		_ = s.Terminate("shutdown")
		return true
	})

	// Wait until sessions map is empty (sessions self-delete on goroutine exit)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		count := 0
		m.sessions.Range(func(_, _ any) bool { count++; return true })
		if count == 0 {
			return
		}
		select {
		case <-ctx.Done():
			remaining := 0
			m.sessions.Range(func(_, _ any) bool { remaining++; return true })
			m.log.Warn().Int("remaining", remaining).Msg("shutdown: drain timeout — abandoning active sessions")
			return
		case <-ticker.C:
		}
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

// PreRegisterSession synchronously builds a CallSession shell and registers
// it in callSidIdx + sessions so concurrent REST GETs and the lifecycle hook
// closure in main.go resolve the session immediately at INVITE arrival time —
// BEFORE the StartSessionWithPreRegistered goroutine runs and attaches legs[0].
//
// The returned session has the immutable REST-API metadata fields populated
// (callSid, callID, streamSid (PER BLOCKER 2 OF 16-10 REVISION FEEDBACK —
// minted here, never reassigned downstream), accountSid, from, to, startTime,
// state=StateStreaming), statusWC threaded through, and an empty legs slice
// — legs[0] is attached inside StartSessionWithPreRegistered when the
// goroutine body runs (via SetLeg(0, inboundLeg)).
//
// The returned cleanup func MUST be deferred by the caller; it removes the
// session from both indices + stamps a recentlyTerminated snapshot ONLY IF
// session.EverEmitted() == true (per BLOCKER 3 of revision feedback —
// never produce a ghost terminal-only callback for a call that never reached
// "initiated"). Idempotent — calling it twice (or after a successful
// StartSessionWithPreRegistered, whose own defer chain handles teardown) is a
// no-op.
//
// Without synchronous pre-registration, handler.go's emitInitiated /
// emitRinging / emitAnswered hooks would reach a statusLookup closure that
// returns ok=false, so customer subscribers would receive ZERO lifecycle
// callbacks for inbound calls.
//
// The returned interface type is sip.PreRegisteredSession (single-producer /
// single-consumer convention; see sip.PreRegisteredSession docstring for the
// architectural invariant).
func (m *CallManager) PreRegisterSession(callSid, callID, accountSid, fromURI, toURI string, startTime time.Time) (sip.PreRegisteredSession, func()) {
	// Mint streamSid SYNCHRONOUSLY here, populate the shell — downstream
	// code (StartSessionWithPreRegistered) MUST NOT reassign. The WS START
	// frame at session.go's run() reads s.streamSid AS-IS.
	streamSid := "MZ" + strings.ReplaceAll(uuid.New().String(), "-", "")

	session := &CallSession{
		callID:     callID,
		callSid:    callSid,
		streamSid:  streamSid,
		accountSid: accountSid,
		cfg:        m.cfg,
		log:        m.log.With().Str(observability.FieldCallSid, callSid).Str(observability.FieldAccountSid, accountSid).Logger(),
		metrics:    m.metrics,
		// Thread the per-process StatusClient onto every freshly-created
		// CallSession. Nil when SetStatusClient was never called (tests
		// or operator deployments without status callbacks) — the
		// emission helper no-ops.
		statusWC:  m.statusWC,
		from:      fromURI,
		to:        toURI,
		startTime: startTime,
	}
	session.state.Store(StateStreaming)

	// Operator-supplied default StatusCallback subscription
	// (STATUS_CALLBACK_DEFAULT_URL env). Installed BEFORE the session
	// is registered in the lookup maps so the very first emit
	// (initiated/queued, fired by handler.onInvite right after
	// PreRegisterSession returns) can resolve the subscription.
	// Trusted=true marks the URL as operator-supplied so the SSRF
	// guard at dial time and the pre-flight ValidateCallbackURL at
	// Enqueue are bypassed.
	if m.cfg.StatusCallbackDefaultURL != "" {
		events := map[string]struct{}{}
		for _, ev := range strings.Split(m.cfg.StatusCallbackDefaultEvents, ",") {
			ev = strings.TrimSpace(strings.ToLower(ev))
			if ev != "" {
				events[ev] = struct{}{}
			}
		}
		session.SetStatusCallback(&StatusCallbackConfig{
			URL:     m.cfg.StatusCallbackDefaultURL,
			Method:  m.cfg.StatusCallbackDefaultMethod,
			Events:  events,
			Trusted: true,
		})
	}

	m.sessions.Store(callID, session)
	m.callSidIdx.Store(callSid, session)
	if m.metrics != nil {
		m.metrics.ActiveCalls.Inc()
	}

	var cleaned atomic.Bool
	cleanup := func() {
		if !cleaned.CompareAndSwap(false, true) {
			return // idempotent — already torn down
		}
		// Only stamp a terminal state + snapshot when the call ever
		// reached a customer-visible event. Otherwise the
		// customer would receive a "completed/failed" POST for a CallSid
		// they never knew about. The customer is told "the call ended"
		// only if they were first told "the call started".
		if session.EverEmitted() {
			if session.EndTime().IsZero() {
				session.markTerminated("failed")
			}
			snap := &terminatedCall{
				callSid:    session.callSid,
				accountSid: session.accountSid,
				from:       session.from,
				to:         session.to,
				direction:  session.Direction(),
				status:     session.Status(),
				startTime:  session.startTime,
				endTime:    session.EndTime(),
				duration:   session.Duration(),
			}
			m.recentlyTerminated.Store(session.callSid, snap)
		}
		// Always remove from active maps + dec ActiveCalls (the
		// PreRegisterSession Inc happens unconditionally).
		m.sessions.Delete(callID)
		m.callSidIdx.Delete(session.callSid)
		if m.metrics != nil {
			m.metrics.ActiveCalls.Dec()
		}
	}
	return session, cleanup
}

// StartSessionWithPreRegistered consumes a CallSession built by
// PreRegisterSession and attaches legs[0] + runs the WS dial + RTP bridge.
// Replaces the synchronous registration block of the legacy StartSession
// (sessions.Store / callSidIdx.Store / ActiveCalls.Inc) with a no-op — the
// session is already registered.
//
// INVARIANT: streamSid is owned by PreRegisterSession; this function MUST
// NOT reassign s.streamSid. The WS START frame at session.go's run() reads
// s.streamSid AS-IS.
//
// The sip.PreRegisteredSession interface is satisfied ONLY by *CallSession
// in this codebase — bridge.CallManager.PreRegisterSession is the single
// constructor. The type assertion below is documented as panic-if-misuse:
// no other code path constructs a value of this interface.
//
// Mirrors the existing defer chain — same recentlyTerminated snapshot
// semantics, same callSidIdx.Delete cleanup, same ActiveCalls.Dec. The
// PreRegisterSession-returned cleanup func is a no-op once we reach this
// function (the caller's `success = true` flag suppressed it before
// dispatching the goroutine).
func (m *CallManager) StartSessionWithPreRegistered(
	psession sip.PreRegisteredSession,
	dlg *sipgo.DialogServerSession,
	req *siplib.Request,
	callerSDP *sip.CallerSDP,
	rtpPort int,
	audioPT uint8,
	localSRTPKey []byte,
	localSRTPSalt []byte,
	streamURL string,
	customParams map[string]string,
	log zerolog.Logger,
) {
	// The sip.PreRegisteredSession iface is satisfied ONLY by *CallSession in
	// this codebase — type-assert is documented as panic-if-misuse (no other
	// code path constructs a value of this interface; see sip-package
	// docstring for the architectural invariant).
	session := psession.(*CallSession)
	session.customParams = customParams

	callID := req.CallID().Value()
	// CON-02: always release port — covers WS dial failure and call end.
	defer m.portPool.Release(rtpPort)

	callerRTPAddr := &net.UDPAddr{
		IP:   net.ParseIP(callerSDP.RTPAddr),
		Port: callerSDP.RTPPort,
	}

	// Compute media format fields from negotiated codec.
	encoding, sampleRate := sip.MediaFormatForPT(audioPT)

	// Build the inbound caller leg — single-leg paths always have exactly one leg.
	// Per-leg RTP channels (packetQueue, rtpInboundQueue) are allocated here so
	// that session.run() does not need to know about leg internals.
	inboundLeg := &Leg{
		dlg:             dlg,
		rtpPort:         rtpPort,
		remoteRTP:       callerRTPAddr,
		dtmfPT:          callerSDP.DTMFPayloadType,
		audioPT:         audioPT,
		silenceFrame:    sip.SilenceFrameForPT(audioPT),
		mediaEncoding:   encoding,
		mediaSampleRate: sampleRate,
		localSRTPKey:    localSRTPKey,
		localSRTPSalt:   localSRTPSalt,
		remoteSRTPKey:   callerSDP.RemoteSRTPKey,
		remoteSRTPSalt:  callerSDP.RemoteSRTPSalt,
		packetQueue:     make(chan outboundFrame, packetQueueSize),
		rtpInboundQueue: make(chan []byte, rtpInboundQueueSize),
	}
	// Attach via SetLeg(0, ...) which sets leg.session backref atomically
	// under legsMu.
	session.SetLeg(0, inboundLeg)

	// Existing defer cleanup chain — owns teardown for the goroutine path.
	// PreRegisterSession's cleanup func is a no-op once we reach here (the
	// caller's `success = true` flag suppressed it before dispatch).
	defer func() {
		// Single-writer cleanup ordering:
		//   1. markTerminated stamps endTime+termReason on the session goroutine
		//      that owns this defer chain (no concurrent writer exists).
		//   2. Build the immutable terminatedCall snapshot from the live session;
		//      the snapshot reads the freshly-stamped endTime so Duration() is
		//      already coherent.
		//   3. Store the snapshot into recentlyTerminated BEFORE removing the
		//      session from active maps, so concurrent List() always sees the
		//      call somewhere — never both nowhere nor twice.
		//   4. Delete from sessions + callSidIdx.
		// session.run() may already have stamped endTime via its natural-exit
		// markTerminated("completed") call; we defensively re-stamp on the
		// panic / abnormal-exit path so endTime is never zero in the snapshot.
		if session.EndTime().IsZero() {
			session.markTerminated("failed")
		}
		snap := &terminatedCall{
			callSid:    session.callSid,
			accountSid: session.accountSid,
			from:       session.from,
			to:         session.to,
			direction:  session.Direction(),
			status:     session.Status(),
			startTime:  session.startTime,
			endTime:    session.EndTime(),
			duration:   session.Duration(),
		}
		m.recentlyTerminated.Store(session.callSid, snap)
		m.sessions.Delete(callID)
		m.callSidIdx.Delete(session.callSid)
		if m.metrics != nil {
			m.metrics.ActiveCalls.Dec()
		}
	}()

	// Call is already answered (200 OK sent + ACK received in onInvite). Dial
	// WS now. If WS fails we send BYE (can't reject after 200 OK).
	// streamURL is resolved by the verb dispatcher (BC-shim path uses
	// cfg.WSTargetURL; the <Connect><Stream url=…/> verb overrides).
	wsCtx, wsCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer wsCancel()
	wsConn, err := dialWS(wsCtx, streamURL)
	if err != nil {
		log.Error().Err(err).
			Str("call_id", callID).
			Str("call_sid", session.callSid).
			Str("stream_url", streamURL).
			Msg("dialWS failed — sending BYE")
		_ = dlg.Bye(context.Background())
		return
	}

	session.run(dlg.Context(), wsConn)
}

// StartSession is the legacy single-call entry point retained for backward-
// compatibility with tests + any caller that does not go through onInvite's
// new pre-registration path. Production main.go uses
// PreRegisterSession + StartSessionWithPreRegistered directly via the SIP
// handler. The wrapper goes through the same code path so streamSid mint is
// preserved for ALL callers (BLOCKER 2 acceptance — non-empty streamSid for
// inbound INVITE via the legacy wrapper).
//
// callSid + streamURL are minted/resolved by the onInvite goroutine in plan
// 12-05: callSid via identity.NewCallSid(); streamURL from the verb-driven
// <Connect><Stream> (BC-shim equals cfg.WSTargetURL when VOICE_URL is empty).
func (m *CallManager) StartSession(
	dlg *sipgo.DialogServerSession,
	req *siplib.Request,
	callerSDP *sip.CallerSDP,
	rtpPort int,
	audioPT uint8,
	localSRTPKey []byte,
	localSRTPSalt []byte,
	callSid string,
	streamURL string,
	log zerolog.Logger,
) {
	startTime := time.Now().UTC()
	psession, _ := m.PreRegisterSession(
		callSid,
		req.CallID().Value(),
		m.accountSid,
		req.From().Address.String(),
		req.To().Address.String(),
		startTime,
	)
	m.StartSessionWithPreRegistered(
		psession,
		dlg, req, callerSDP, rtpPort, audioPT,
		localSRTPKey, localSRTPSalt,
		streamURL, nil, log,
	)
}
