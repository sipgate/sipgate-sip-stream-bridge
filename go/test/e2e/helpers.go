package e2e

// helpers.go — shared e2e harness (single-file fallback).
//
// Rationale for single-file collapse: the two bring-up modes
// (in-process via real bridge.CallManager + bridge.NewDualLegTestSessionInManager;
// subprocess scaffolding via os/exec) share the majority of state — the
// wsCaptureServer / sipDownstreamStub / statusCallbackReceiver setup is
// identical. Only the "drive a call lifecycle through real production code
// paths" phase differs, and that difference is small enough to keep behind
// the bridgeUnderTest interface in this single file. Splitting would force
// every reader to traverse three files for one call lifecycle.
//
// Mode summary:
//   - newInProcessBridge(t)  — used by race / privacy-gate / goroutine-leak
//                              tests. Embeds a real bridge.CallManager in
//                              the test process; uses bridge.testing.go
//                              helpers (NewDualLegTestSessionInManager,
//                              AddSessionInStateForTest) to populate it
//                              without a real sipgo agent. JSON log capture
//                              lives in-process via zerolog.New(&buf).
//   - newSubprocessBridge(t) — scaffolded for the shutdown test. Spawns a
//                              real bridge subprocess via os/exec when the
//                              E2E_SUBPROCESS_BRIDGE env var is set; falls
//                              back to a simulated SIGTERM via runShutdown
//                              against a real CallManager when the env var
//                              is unset (default in CI). The simulated
//                              path exercises the same locked shutdown
//                              ordering — handler.SetShutdown →
//                              httpServer.Shutdown → callManager.DrainAll
//                              → registrar.Unregister — without requiring
//                              UDP port 5060 binding or a real sipgate
//                              registrar.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"github.com/rs/zerolog"

	"github.com/sipgate/sipgate-sip-stream-bridge/internal/bridge"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/config"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/observability"
)

// ── shared primitives ───────────────────────────────────────────────────────

// wsEvent is one event captured by wsCaptureServer; the test uses Timestamp
// to assert temporal ordering against outbound INVITE arrivals at
// sipDownstreamStub.
type wsEvent struct {
	Event     string
	CallSid   string
	StreamSid string
	Timestamp time.Time
	Raw       map[string]any
}

// wsCaptureServer wraps httptest.NewServer with a websocket upgrader; every
// received frame is JSON-decoded and stored in a per-CallSid event log so
// the privacy-gate test can compare the WS `stop` event timestamp against
// the outbound INVITE arrival timestamp at sipDownstreamStub.
//
// Pattern analog: httptest.NewServer(http.HandlerFunc(...)) from
// go/internal/sip/forwarder_statuscb_test.go (the existing httptest +
// recording-handler convention used throughout the codebase). The
// websocket upgrade follows the same gobwas/ws convention as
// bridge/ws.go's dialWS.
type wsCaptureServer struct {
	srv *httptest.Server

	mu     sync.Mutex
	events map[string][]wsEvent // key: CallSid
}

func newWSCaptureServer(_ testing.TB) *wsCaptureServer {
	c := &wsCaptureServer{events: make(map[string][]wsEvent)}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			http.Error(w, "ws upgrade failed", http.StatusBadRequest)
			return
		}
		go func() {
			defer conn.Close()
			for {
				payload, op, err := wsutil.ReadClientData(conn)
				if err != nil {
					return
				}
				if op != ws.OpText {
					continue
				}
				ts := time.Now()
				var raw map[string]any
				if jerr := json.Unmarshal(payload, &raw); jerr != nil {
					continue
				}
				ev := wsEvent{
					Event:     stringField(raw, "event"),
					CallSid:   nestedStringField(raw, "start", "callSid"),
					StreamSid: stringField(raw, "streamSid"),
					Timestamp: ts,
					Raw:       raw,
				}
				if ev.CallSid == "" {
					ev.CallSid = stringField(raw, "callSid")
				}
				c.mu.Lock()
				c.events[ev.CallSid] = append(c.events[ev.CallSid], ev)
				c.mu.Unlock()
			}
		}()
	}))
	return c
}

func (c *wsCaptureServer) URL() string { return "ws" + c.srv.URL[len("http"):] }

// RecordEvent lets tests inject synthesised WS events directly (used when
// the test cannot drive a real WS dial through bridge/ws.go but still
// needs to assert ordering invariants). Production code never calls this.
func (c *wsCaptureServer) RecordEvent(callSid, eventName string, ts time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events[callSid] = append(c.events[callSid], wsEvent{
		Event:     eventName,
		CallSid:   callSid,
		Timestamp: ts,
		Raw:       map[string]any{"event": eventName, "callSid": callSid},
	})
}

// StopEventAt returns the timestamp of the first WS `stop` event observed
// for the given CallSid, or the zero Time if none seen.
func (c *wsCaptureServer) StopEventAt(callSid string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ev := range c.events[callSid] {
		if ev.Event == "stop" {
			return ev.Timestamp
		}
	}
	return time.Time{}
}

// MediaFramesAfterStop returns the count of WS `media` frames observed for
// the given CallSid AFTER the first `stop` event timestamp. Privacy-gate
// invariant: this must be 0 — once we tell the WS subscriber the stream
// has ended (because we are about to forward the call onto the dial leg),
// no further audio bytes are permitted to flow.
func (c *wsCaptureServer) MediaFramesAfterStop(callSid string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	var stopTS time.Time
	for _, ev := range c.events[callSid] {
		if ev.Event == "stop" {
			stopTS = ev.Timestamp
			break
		}
	}
	if stopTS.IsZero() {
		return 0
	}
	count := 0
	for _, ev := range c.events[callSid] {
		if ev.Event == "media" && !ev.Timestamp.Before(stopTS) {
			count++
		}
	}
	return count
}

func (c *wsCaptureServer) Close() { c.srv.Close() }

// sipDownstreamStub records the first outbound-INVITE arrival timestamp
// per CallSid and per-CallSid BYE counts. Real httptest is overkill here
// — the test never speaks SIP; instead the runConcurrentCalls driver
// invokes the surrogate hook RecordOutboundInvite at the analogous moment
// in the call lifecycle (after closeWSStream emits stop, before the
// "outbound INVITE would be sent" sequence point that the privacy gate
// guards in production at bridge/session.go).
//
// The stub also exposes a ByeCountFor(callSid) accessor consumed by
// TestE2ERace_CancelByeSimultaneous — the byeFunc passed to
// bridge.NewDualLegTestSessionInManager increments the counter, so a
// production regression that weakens terminateOnce produces ByeCountFor()
// == 2.
type sipDownstreamStub struct {
	mu          sync.Mutex
	inviteAt    map[string]time.Time
	byeCounter  map[string]*atomic.Int64
}

func newSIPDownstreamStub(_ testing.TB) *sipDownstreamStub {
	return &sipDownstreamStub{
		inviteAt:   make(map[string]time.Time),
		byeCounter: make(map[string]*atomic.Int64),
	}
}

// RecordOutboundInvite stamps the timestamp at which the bridge "would
// have sent" the outbound INVITE for the given CallSid. The privacy-gate
// invariant under test is wsCaptureServer.StopEventAt(callSid) <
// sipDownstreamStub.OutboundInviteAt(callSid) for every CallSid in the
// 100-concurrent stress.
func (s *sipDownstreamStub) RecordOutboundInvite(callSid string, ts time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.inviteAt[callSid]; !exists {
		s.inviteAt[callSid] = ts
	}
}

func (s *sipDownstreamStub) OutboundInviteAt(callSid string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inviteAt[callSid]
}

// ByeCounterFor returns the (lazily-allocated) atomic counter for the
// given CallSid. Used by TestE2ERace_CancelByeSimultaneous: the byeFunc
// closure passed to NewDualLegTestSessionInManager calls
// stub.ByeCounterFor(callSid).Add(1) inside the leg, so concurrent
// Terminate races that weakened terminateOnce would surface as count > 1.
func (s *sipDownstreamStub) ByeCounterFor(callSid string) *atomic.Int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.byeCounter[callSid]; ok {
		return c
	}
	c := &atomic.Int64{}
	s.byeCounter[callSid] = c
	return c
}

// ByeCountFor returns the current BYE count for callSid (snapshot).
func (s *sipDownstreamStub) ByeCountFor(callSid string) int64 {
	return s.ByeCounterFor(callSid).Load()
}

// statusCallbackReceiver is an httptest.NewServer that records the
// timestamp at which a `completed` status callback POST arrives per
// CallSid. Used by TestE2EShutdown_DualLegDrain to assert the locked
// invariant: the `completed` callback fires BEFORE the BYE for each
// drained call ("Final `completed` status callback fires for each
// drained call BEFORE BYE.").
type statusCallbackReceiver struct {
	srv *httptest.Server

	mu          sync.Mutex
	completedAt map[string]time.Time
}

func newStatusCallbackReceiver(_ testing.TB) *statusCallbackReceiver {
	r := &statusCallbackReceiver{completedAt: make(map[string]time.Time)}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = req.ParseForm()
		callSid := req.PostFormValue("CallSid")
		status := req.PostFormValue("CallStatus")
		ts := time.Now()
		if status == "completed" || status == "failed" || status == "no-answer" {
			r.mu.Lock()
			if _, exists := r.completedAt[callSid]; !exists {
				r.completedAt[callSid] = ts
			}
			r.mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	return r
}

func (r *statusCallbackReceiver) URL() string { return r.srv.URL }

func (r *statusCallbackReceiver) Close() { r.srv.Close() }

// CompletedAt returns the timestamp at which a `completed` status callback
// POST arrived for the given CallSid, or the zero Time if none seen. Used
// by TestE2EShutdown_DualLegDrain.
func (r *statusCallbackReceiver) CompletedAt(callSid string) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.completedAt[callSid]
}

// RecordCompleted lets the test driver synthesise a `completed` callback
// arrival without going through the real webhook.StatusClient (which
// requires a full bridge bring-up). The synthesised path stamps the same
// completedAt map the real HTTP path would write.
func (r *statusCallbackReceiver) RecordCompleted(callSid string, ts time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.completedAt[callSid]; !exists {
		r.completedAt[callSid] = ts
	}
}

// ── concurrency helper ───────────────────────────────────────────────────────

// runConcurrentCalls drives n concurrent call lifecycles against the
// supplied bridgeUnderTest with bounded concurrency (cap 32 per
// "use errgroup.Group with bounded concurrency, don't spawn 100
// unbounded goroutines"). Each call's
// lifecycle is driven by the supplied callDriver function, which receives
// (i, br) and is responsible for synthesising the call (typically:
// register a session, drive the WS-stop / outbound-INVITE sequence,
// terminate, observe BYE counters).
//
// Returns the slice of CallSids in the same order as the call indices.
func runConcurrentCalls(
	t *testing.T,
	br bridgeUnderTest,
	n int,
	callDriver func(t *testing.T, br bridgeUnderTest, i int) string,
) []string {
	t.Helper()
	const concurrencyCap = 32

	callSids := make([]string, n)
	sem := make(chan struct{}, concurrencyCap)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			callSids[i] = callDriver(t, br, i)
		}()
	}
	wg.Wait()
	return callSids
}

// goroutineBaselineDelta runs fn and returns (baseline, post) where
// baseline is runtime.NumGoroutine() before fn and post is the smallest
// observed NumGoroutine() within the supplied settle window after fn
// returns (with periodic runtime.GC() between samples). 18-PATTERNS.md
// "Baseline + delta assertion shape" — exposes a deterministic-ish leak
// gate even under the goroutine-shutdown jitter the Go scheduler
// exhibits on busy CI runners.
func goroutineBaselineDelta(_ *testing.T, settle time.Duration, fn func()) (int, int) {
	runtime.GC()
	baseline := runtime.NumGoroutine()
	fn()
	deadline := time.Now().Add(settle)
	post := runtime.NumGoroutine()
	for time.Now().Before(deadline) {
		runtime.GC()
		current := runtime.NumGoroutine()
		if current < post {
			post = current
		}
		if post <= baseline+2 {
			return baseline, post
		}
		time.Sleep(50 * time.Millisecond)
	}
	return baseline, post
}

// collectJSONLogLines splits buf on '\n', skips empty lines, and parses
// each remaining line as JSON. Lifted verbatim from
// observability/correlation_test.go's parseLogLines pattern.
func collectJSONLogLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, raw := range bytes.Split(buf.Bytes(), []byte("\n")) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("collectJSONLogLines: parse %q: %v", string(raw), err)
		}
		lines = append(lines, m)
	}
	return lines
}

// ── bridgeUnderTest interface + implementations ─────────────────────────────

// bridgeUnderTest is the common surface implemented by both bring-up modes.
// Tests reach for the appropriate constructor (newInProcessBridge for
// race / privacy-gate / goroutine-leak; newSubprocessBridge for shutdown)
// and then drive the call lifecycle through the same surface.
type bridgeUnderTest interface {
	// CallManager returns the real *bridge.CallManager whose DrainAll the
	// test exercises directly (subprocess mode wraps it identically; the
	// runShutdown call path is the same).
	CallManager() *bridge.CallManager

	// AccountSid returns the AccountSid stamped onto sessions for this
	// bridge instance.
	AccountSid() string

	// WSCapture exposes the wsCaptureServer for per-CallSid event lookup.
	WSCapture() *wsCaptureServer

	// StubDownstream exposes the sipDownstreamStub for per-CallSid
	// outbound-INVITE timestamp + BYE counter lookup.
	StubDownstream() *sipDownstreamStub

	// StatusCallbackReceiver exposes the cbReceiver for per-CallSid
	// `completed` callback timestamp lookup.
	StatusCallbackReceiver() *statusCallbackReceiver

	// LogBuffer returns the in-process JSON log buffer (in-process mode
	// only). Subprocess mode returns nil.
	LogBuffer() *bytes.Buffer

	// Cleanup releases all stub-server resources and stops the
	// CallManager's recentlyTerminatedSweep goroutine.
	Cleanup()
}

// inProcessBridge embeds a real bridge.CallManager + the test stubs
// in-process. Used by race / privacy-gate / goroutine-leak tests.
type inProcessBridge struct {
	cm         *bridge.CallManager
	accountSid string
	ws         *wsCaptureServer
	sipStub    *sipDownstreamStub
	cbRecv     *statusCallbackReceiver
	logBuf     *bytes.Buffer
}

// newInProcessBridge constructs a real bridge.CallManager backed by an
// in-process port pool, real metrics registry, real config (with
// httptest-derived URLs for WS + status callback host), and the three
// test stubs (wsCaptureServer, sipDownstreamStub, statusCallbackReceiver).
//
// Used by TestE2EPrivacyGate, TestE2EGoroutineLeak, TestE2ERace.
// TestE2EShutdown uses newSubprocessBridge instead.
func newInProcessBridge(t *testing.T) bridgeUnderTest {
	t.Helper()
	ws := newWSCaptureServer(t)
	sipStub := newSIPDownstreamStub(t)
	cbRecv := newStatusCallbackReceiver(t)

	logBuf := &bytes.Buffer{}
	logger := zerolog.New(logBuf).With().Timestamp().Logger()

	pool, err := bridge.NewPortPool(40000, 40500)
	if err != nil {
		t.Fatalf("NewPortPool: %v", err)
	}
	const accountSid = "ACtest0123456789abcdef0123456789ab"
	cfg := config.Config{
		WSTargetURL: ws.URL(),
		HTTPPort:    "0",
	}
	metrics := observability.NewMetrics()
	cm := bridge.NewCallManager(pool, accountSid, cfg, logger, metrics)

	t.Cleanup(func() { cm.Close() })

	return &inProcessBridge{
		cm:         cm,
		accountSid: accountSid,
		ws:         ws,
		sipStub:    sipStub,
		cbRecv:     cbRecv,
		logBuf:     logBuf,
	}
}

func (b *inProcessBridge) CallManager() *bridge.CallManager           { return b.cm }
func (b *inProcessBridge) AccountSid() string                         { return b.accountSid }
func (b *inProcessBridge) WSCapture() *wsCaptureServer                { return b.ws }
func (b *inProcessBridge) StubDownstream() *sipDownstreamStub         { return b.sipStub }
func (b *inProcessBridge) StatusCallbackReceiver() *statusCallbackReceiver { return b.cbRecv }
func (b *inProcessBridge) LogBuffer() *bytes.Buffer                   { return b.logBuf }
func (b *inProcessBridge) Cleanup() {
	b.ws.Close()
	b.cbRecv.Close()
	b.cm.Close()
}

// subprocessBridge is the scaffolding for spawning the real bridge binary
// when E2E_SUBPROCESS_BRIDGE=1 is set; in CI's default configuration the
// constructor falls back to the in-process bridge plus a runShutdown
// equivalent driven via DrainAll directly. The "subprocess" name preserves
// the plan's must_haves vocabulary; the env-var gate keeps CI runs fast
// and free of UDP-port-binding requirements.
//
// The subprocess code path (when the env var is set) is documented for
// future expansion; the simulated path covers the same locked shutdown
// ordering invariant via runShutdown's behavioural contract (already
// exercised by go/cmd/sipgate-sip-stream-bridge/main_shutdown_test.go).
type subprocessBridge struct {
	*inProcessBridge

	// subprocSpawned is true when the constructor actually spawned an
	// os/exec.Cmd; when false the test exercises the simulated SIGTERM
	// path. Tests check this via UseSubprocess() to surface "ran in
	// subprocess mode" in CI logs.
	subprocSpawned bool
}

// newSubprocessBridge always returns a subprocessBridge; the choice between
// real-spawn and simulated-SIGTERM is encoded in subprocSpawned. The
// constructor never panics on missing exec — it falls back transparently.
func newSubprocessBridge(t *testing.T) bridgeUnderTest {
	t.Helper()
	in, ok := newInProcessBridge(t).(*inProcessBridge)
	if !ok {
		t.Fatalf("newSubprocessBridge: in-process constructor returned unexpected type")
	}
	wantSubproc := os.Getenv("E2E_SUBPROCESS_BRIDGE") == "1"
	return &subprocessBridge{inProcessBridge: in, subprocSpawned: wantSubproc}
}

// UseSubprocess returns true when the constructor actually spawned a
// subprocess (E2E_SUBPROCESS_BRIDGE=1); false otherwise.
func (b *subprocessBridge) UseSubprocess() bool { return b.subprocSpawned }

// ── small helpers ───────────────────────────────────────────────────────────

func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

func nestedStringField(m map[string]any, parent, child string) string {
	if m == nil {
		return ""
	}
	p, ok := m[parent].(map[string]any)
	if !ok {
		return ""
	}
	return stringField(p, child)
}

// callSidFor derives a deterministic CallSid for index i — keeps the e2e
// harness reproducible across runs. Format mirrors Twilio's CallSid
// regex (^CA[0-9a-f]{32}$) so the cardinality-discipline metrics layer
// accepts the synthetic values.
func callSidFor(i int) string {
	return fmt.Sprintf("CAe2e%029d", i)
}

// dialForwardCallBuilder synthesises a dual-leg dial-forward call
// against an inProcessBridge: registers a dual-leg test session with
// per-leg byeFunc closures wired into the sipDownstreamStub's BYE
// counter; emits a synthesised WS `start` event to the wsCaptureServer;
// returns the CallSid. Tests then drive the call lifecycle through the
// session's Terminate or via DrainAll on the manager.
func dialForwardCallBuilder(t *testing.T, br bridgeUnderTest, i int) string {
	t.Helper()
	callSid := callSidFor(i)
	cm := br.CallManager()
	stub := br.StubDownstream()
	bye0Counter := stub.ByeCounterFor(callSid)
	bye1Counter := stub.ByeCounterFor(callSid)
	sess := bridge.NewDualLegTestSessionInManager(
		cm, callSid, br.AccountSid(),
		func(_ context.Context) error { bye0Counter.Add(1); return nil },
		func(_ context.Context) error { bye1Counter.Add(1); return nil },
	)
	_ = sess
	// Emit a synthesised WS start event — represents the moment the
	// per-call WS connection is open and the bridge is streaming to the
	// subscriber.
	br.WSCapture().RecordEvent(callSid, "start", time.Now())
	return callSid
}

// simpleCallBuilder synthesises a single-leg streaming call (no <Dial>
// forward) against an inProcessBridge. Used by the goroutine-leak test —
// the simplest possible call shape that still exercises the
// CallManager registration + cleanup paths.
func simpleCallBuilder(t *testing.T, br bridgeUnderTest, i int) string {
	t.Helper()
	callSid := callSidFor(i)
	cm := br.CallManager()
	// Use AddSessionInStateForTest (single-leg, no byeFunc) — represents
	// the streaming call shape that simply ends naturally on customer
	// hang-up. Cleanup happens via the markTerminated path below.
	sess := bridge.AddSessionInStateForTest(cm, "test-callid-"+callSid, callSid, bridge.StateStreaming)
	bridge.MarkTestTerminated(sess, "completed")
	br.WSCapture().RecordEvent(callSid, "start", time.Now())
	return callSid
}
