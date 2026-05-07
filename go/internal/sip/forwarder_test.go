package sip

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	siplib "github.com/emiago/sipgo/sip"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/config"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/observability"
)

// ── Test fixtures ────────────────────────────────────────────────────────────

// stubLegConfigurer satisfies the sip.LegConfigurer interface for unit
// tests. It captures the offer/accept call sequence and returns canned
// SDP/error values.
type stubLegConfigurer struct {
	offerSDP            []byte
	offerPort           int
	offerErr            error
	acceptErr           error
	onAnsweredErr       error
	offerCallCount      int32
	acceptCallCount     int32
	onAnsweredCallCount int32
	acceptedAnswer      []byte
}

func (s *stubLegConfigurer) BuildSDPOffer() ([]byte, int, error) {
	atomic.AddInt32(&s.offerCallCount, 1)
	return s.offerSDP, s.offerPort, s.offerErr
}

func (s *stubLegConfigurer) AcceptSDPAnswer(b []byte) error {
	atomic.AddInt32(&s.acceptCallCount, 1)
	s.acceptedAnswer = append([]byte(nil), b...)
	return s.acceptErr
}

func (s *stubLegConfigurer) RTPLocalPort() int { return s.offerPort }

// OnAnswered is the dual-leg activation hook. The stub records it was
// called and returns onAnsweredErr (default nil — happy path).
func (s *stubLegConfigurer) OnAnswered() error {
	atomic.AddInt32(&s.onAnsweredCallCount, 1)
	return s.onAnsweredErr
}

// stubDialClient satisfies the sip.DialClient interface. The done channel is
// pre-closed by default (so the natural-end path is immediate) but tests can
// supply a manually-controlled channel for timer/star/cancel scenarios.
type stubDialClient struct {
	finalResp *siplib.Response
	done      chan struct{}
	ackCalls  int32
	byeCalls  int32
	closeCnt  int32
	ackErr    error
	byeErr    error
	mu        sync.Mutex
}

func newStubDialClient(finalResp *siplib.Response) *stubDialClient {
	done := make(chan struct{})
	close(done) // pre-closed → natural-end fires immediately
	return &stubDialClient{finalResp: finalResp, done: done}
}

func newStubDialClientOpenDone(finalResp *siplib.Response) *stubDialClient {
	return &stubDialClient{finalResp: finalResp, done: make(chan struct{})}
}

func (s *stubDialClient) FinalResponse() *siplib.Response { return s.finalResp }

func (s *stubDialClient) Ack(_ context.Context) error {
	atomic.AddInt32(&s.ackCalls, 1)
	return s.ackErr
}

func (s *stubDialClient) Bye(_ context.Context) error {
	atomic.AddInt32(&s.byeCalls, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
		// already closed — idempotent close
	default:
		close(s.done)
	}
	return s.byeErr
}

func (s *stubDialClient) Done() <-chan struct{} { return s.done }

func (s *stubDialClient) Close() error {
	atomic.AddInt32(&s.closeCnt, 1)
	return nil
}

// closeDone unblocks the awaitDialogEnd select for tests that drive the
// dialog open then close it manually.
func (s *stubDialClient) closeDone() {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}

// stubDialFactory satisfies the sip.DialClientFactory interface. Tests
// preset behaviour:
//   - returnedClient: the stub DialClient that Dial returns
//   - returnedErr: the error Dial returns (nil = success)
//   - simulateAuthChallenge: when set to "401" / "407", the factory invokes
//     onResponse with a synthetic challenge response BEFORE returning success
//   - hangForever: when true, Dial blocks until ctx is canceled (tests
//     ring-timeout and ctx-cancel paths)
type stubDialFactory struct {
	returnedClient        DialClient
	returnedErr           error
	simulateAuthChallenge string
	hangForever           bool

	calls            int32
	lastRecipient    siplib.Uri
	lastFrom         siplib.Uri
	lastDisplayName  string
	lastPPI          *siplib.Uri
	lastBody         []byte
	lastAuth         DialAuth
	readByeCalls     int32
	mu               sync.Mutex
	receivedHeader   []*siplib.Response
}

// ReadBye is a no-op for the stub — production code routes BYEs via this
// method but tests don't exercise the BYE path through the factory.
func (f *stubDialFactory) ReadBye(_ *siplib.Request, _ siplib.ServerTransaction) error {
	atomic.AddInt32(&f.readByeCalls, 1)
	return nil
}

func (f *stubDialFactory) Dial(
	ctx context.Context,
	recipient siplib.Uri,
	from siplib.Uri,
	displayName string,
	ppi *siplib.Uri,
	body []byte,
	auth DialAuth,
	onResponse func(*siplib.Response) error,
) (DialClient, error) {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	f.lastRecipient = recipient
	f.lastFrom = from
	f.lastDisplayName = displayName
	f.lastPPI = ppi
	f.lastBody = body
	f.lastAuth = auth
	f.mu.Unlock()

	if f.simulateAuthChallenge != "" {
		var code int
		switch f.simulateAuthChallenge {
		case "401":
			code = siplib.StatusUnauthorized
		case "407":
			code = siplib.StatusProxyAuthRequired
		}
		challengeResp := siplib.NewResponse(code, "Auth Required")
		if err := onResponse(challengeResp); err != nil {
			return nil, err
		}
	}

	if f.hangForever {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	return f.returnedClient, f.returnedErr
}

// ── Test helpers ─────────────────────────────────────────────────────────────

func newTestForwarder(factory DialClientFactory, allowedPrefixes []string) (*Forwarder, *observability.Metrics, *Guardrails) {
	cfg := config.Config{
		SIPUser:               "testuser",
		SIPPassword:           "testpass",
		SIPDomain:             "sipconnect.sipgate.de",
		SIPRegistrar:          "sipconnect.sipgate.de",
		SDPContactIP:          "10.0.0.1",
		DialAllowedPrefixes:   allowedPrefixes,
		DialDefaultCallerID:   "",
		DialRingTimeoutS:      30,
		DialMaxPerSession:     3,
		DialMaxPerMinute:      60,
	}
	metrics := observability.NewMetrics()
	guardrails := NewGuardrails(cfg)
	log := zerolog.Nop()
	// Construct without a real Agent — production code path uses NewForwarder
	// which builds the sipgo factory; tests inject a stub via this helper so
	// agent.Client is never touched.
	f := &Forwarder{
		agent:      nil, // never accessed when factory is stubbed
		guardrails: guardrails,
		cfg:        cfg,
		metrics:    metrics,
		log:        log,
		factory:    factory,
	}
	return f, metrics, guardrails
}

// successResp builds a 200 OK response with a PCMU-only SDP body for the
// happy-path tests. The exact SDP content doesn't matter — the stub
// LegConfigurer's AcceptSDPAnswer ignores the bytes by default.
func successResp(sdpBody []byte) *siplib.Response {
	r := siplib.NewResponse(siplib.StatusOK, "OK")
	r.SetBody(sdpBody)
	return r
}

// nonSuccessResp builds a final non-2xx response — wrapped in
// sipgo.ErrDialogResponse to mimic what sipgo's WaitAnswer surfaces.
func nonSuccessErr(code int, reason string) error {
	resp := siplib.NewResponse(code, reason)
	return &sipgo.ErrDialogResponse{Res: resp}
}

// callSidRE is the same pattern identity.CallSidRE enforces — duplicated
// here to keep test failures self-explanatory in error messages.
var callSidRE = regexp.MustCompile(`^CA[0-9a-f]{32}$`)

// counterValue extracts the current value of a Counter via the Prometheus
// testutil; returns 0 when the metric is unregistered or empty.
func counterValue(c prometheus.Counter) float64 {
	return testutil.ToFloat64(c)
}

// counterVecValue extracts the current value of a CounterVec at the given
// label values.
func counterVecValue(v *prometheus.CounterVec, labels ...string) float64 {
	return testutil.ToFloat64(v.WithLabelValues(labels...))
}

// ── Tests ────────────────────────────────────────────────────────────────────

// 1. Guardrails toll-fraud rejection short-circuits before any INVITE work.
func TestForwarder_Dial_GuardrailsToLLFraud(t *testing.T) {
	t.Parallel()
	factory := &stubDialFactory{}
	f, metrics, _ := newTestForwarder(factory, nil) // empty prefixes = default-deny

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	res, err := f.Dial(context.Background(), "CAcaller", "+4915123456", DialOpts{}, leg)

	if err == nil {
		t.Fatal("expected error from guardrails reject, got nil")
	}
	if !errors.Is(err, ErrTollFraudBlocked) {
		t.Errorf("expected ErrTollFraudBlocked in chain, got %v", err)
	}
	if atomic.LoadInt32(&factory.calls) != 0 {
		t.Errorf("expected factory.Dial NOT called, got calls=%d", factory.calls)
	}
	if atomic.LoadInt32(&leg.offerCallCount) != 0 {
		t.Errorf("expected BuildSDPOffer NOT called on guardrails reject, got %d", leg.offerCallCount)
	}
	if got := counterVecValue(metrics.ForwardFailedTotal, "toll_fraud"); got != 1 {
		t.Errorf("expected forward_failed_total{reason=toll_fraud}=1, got %v", got)
	}
	if got := counterValue(metrics.ForwardAttemptsTotal); got != 0 {
		t.Errorf("expected forward_attempts_total=0 (guardrails fired before attempt), got %v", got)
	}
	if !callSidRE.MatchString(res.DialCallSid) {
		t.Errorf("DialCallSid not minted on guardrails reject: %q", res.DialCallSid)
	}
}

// 2. Per-session rate limit returns ErrSessionRateLimit → reason="rate_limit".
func TestForwarder_Dial_GuardrailsRateLimit(t *testing.T) {
	t.Parallel()
	factory := &stubDialFactory{
		returnedClient: newStubDialClient(successResp([]byte("v=0\r\nm=audio 5000 RTP/AVP 0\r\n"))),
	}
	f, metrics, _ := newTestForwarder(factory, []string{"+49"})
	// Cap at 1 per session, then trigger second dial to hit the limit.
	f.cfg.DialMaxPerSession = 1
	f.guardrails = NewGuardrails(f.cfg)

	leg1 := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	if _, err := f.Dial(context.Background(), "CAsession", "+4915111", DialOpts{CallerID: "+4930555"}, leg1); err != nil {
		t.Fatalf("first dial: unexpected err: %v", err)
	}

	leg2 := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10001}
	_, err := f.Dial(context.Background(), "CAsession", "+4915222", DialOpts{CallerID: "+4930555"}, leg2)
	if err == nil {
		t.Fatal("second dial: expected rate-limit error, got nil")
	}
	if !errors.Is(err, ErrSessionRateLimit) {
		t.Errorf("expected ErrSessionRateLimit, got %v", err)
	}
	if got := counterVecValue(metrics.ForwardFailedTotal, "rate_limit"); got != 1 {
		t.Errorf("expected forward_failed_total{reason=rate_limit}=1, got %v", got)
	}
}

// 3. Caller-ID fallback: TwiML attribute wins.
func TestForwarder_Dial_CallerIDFallback_TwiMLAttribute(t *testing.T) {
	t.Parallel()
	factory := &stubDialFactory{
		returnedClient: newStubDialClient(successResp([]byte("v=0"))),
	}
	f, _, _ := newTestForwarder(factory, []string{"+49"})

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{CallerID: "+49special", CallerFrom: "+49ani"}
	if _, err := f.Dial(context.Background(), "CA1", "+4915123", opts, leg); err != nil {
		t.Fatalf("dial: %v", err)
	}
	if got := factory.lastFrom.User; got != "+49special" {
		t.Errorf("expected From URI user=+49special, got %q", got)
	}
}

// 4. Caller-ID fallback: when SIP_USER is unset and no explicit caller-ID,
// preserve-ANI from CallerFrom is the last-resort fallback. This is the
// Twilio-default behaviour for trunks that accept third-party ANI in From.
func TestForwarder_Dial_CallerIDFallback_PreserveANI(t *testing.T) {
	t.Parallel()
	factory := &stubDialFactory{
		returnedClient: newStubDialClient(successResp([]byte("v=0"))),
	}
	f, _, _ := newTestForwarder(factory, []string{"+49"})
	f.cfg.SIPUser = "" // disable SIP_USER fallback so preserve-ANI is reached

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{CallerID: "", CallerFrom: "+49ani"}
	if _, err := f.Dial(context.Background(), "CA1", "+4915123", opts, leg); err != nil {
		t.Fatalf("dial: %v", err)
	}
	if got := factory.lastFrom.User; got != "+49ani" {
		t.Errorf("expected From URI user=+49ani, got %q", got)
	}
}

// 5. Caller-ID fallback: all empty → cfg.DialDefaultCallerID wins.
func TestForwarder_Dial_CallerIDFallback_DefaultCID(t *testing.T) {
	t.Parallel()
	factory := &stubDialFactory{
		returnedClient: newStubDialClient(successResp([]byte("v=0"))),
	}
	f, _, _ := newTestForwarder(factory, []string{"+49"})
	f.cfg.DialDefaultCallerID = "+49default"

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{CallerID: "", CallerFrom: ""}
	if _, err := f.Dial(context.Background(), "CA1", "+4915123", opts, leg); err != nil {
		t.Fatalf("dial: %v", err)
	}
	if got := factory.lastFrom.User; got != "+49default" {
		t.Errorf("expected From URI user=+49default, got %q", got)
	}
}

// 5b. Auto-fallback to cfg.SIPUser (the registered SIP authentication
// username) when no explicit caller-ID is configured. Solves sipgate's
// "Username in From Field required" 403 without operator configuration:
// sipgate-style trunks accept exactly the SIP_USER as the From user-part.
// Phone B will see the SIP_USER string as Caller-ID — functional, ugly;
// operator overrides for cleaner display via DIAL_DEFAULT_CALLER_ID.
func TestForwarder_Dial_CallerIDFallback_SIPUserBeatsPreserveANI(t *testing.T) {
	t.Parallel()
	factory := &stubDialFactory{
		returnedClient: newStubDialClient(successResp([]byte("v=0"))),
	}
	f, _, _ := newTestForwarder(factory, []string{"+49"})
	f.cfg.SIPUser = "trunkuser123"
	// No DialDefaultCallerID — pure auto-fallback path.

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{
		CallerID:   "",
		CallerFrom: "+4921193674951ani", // would-be preserve-ANI value
	}
	if _, err := f.Dial(context.Background(), "CA1", "+4915123", opts, leg); err != nil {
		t.Fatalf("dial: %v", err)
	}
	if got := factory.lastFrom.User; got != "trunkuser123" {
		t.Errorf("expected From URI user=trunkuser123 (SIP_USER auto-fallback wins over preserve-ANI), got %q", got)
	}
}

// 5c. DIAL_DEFAULT_CALLER_ID env beats SIP_USER — explicit operator intent
// overrides the auto-fallback.
func TestForwarder_Dial_CallerIDFallback_DefaultBeatsSIPUser(t *testing.T) {
	t.Parallel()
	factory := &stubDialFactory{
		returnedClient: newStubDialClient(successResp([]byte("v=0"))),
	}
	f, _, _ := newTestForwarder(factory, []string{"+49"})
	f.cfg.DialDefaultCallerID = "+49operatordid"
	f.cfg.SIPUser = "trunkuser123"

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{CallerID: "", CallerFrom: "+4921193674951ani"}
	if _, err := f.Dial(context.Background(), "CA1", "+4915123", opts, leg); err != nil {
		t.Fatalf("dial: %v", err)
	}
	if got := factory.lastFrom.User; got != "+49operatordid" {
		t.Errorf("expected From URI user=+49operatordid (env wins over SIP_USER), got %q", got)
	}
}

// 5d. TwiML callerId still wins over everything — Twilio standard.
func TestForwarder_Dial_CallerIDFallback_TwiMLAlwaysWins(t *testing.T) {
	t.Parallel()
	factory := &stubDialFactory{
		returnedClient: newStubDialClient(successResp([]byte("v=0"))),
	}
	f, _, _ := newTestForwarder(factory, []string{"+49"})
	f.cfg.DialDefaultCallerID = "+49operatordid"
	f.cfg.SIPUser = "trunkuser123"

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{
		CallerID:   "+49twiml",
		CallerFrom: "+4921193674951ani",
	}
	if _, err := f.Dial(context.Background(), "CA1", "+4915123", opts, leg); err != nil {
		t.Fatalf("dial: %v", err)
	}
	if got := factory.lastFrom.User; got != "+49twiml" {
		t.Errorf("expected From URI user=+49twiml (TwiML always wins), got %q", got)
	}
}

// 5f-pre. From display-name carries the display caller-ID when From URI's
// addr-spec is the SIP_USER (auth identity). Most carriers honour the
// display-name as Caller-ID, even when the addr-spec carries the SIP auth
// username — the cleanest sipgate-compat path. PPI is sent in parallel as
// defense-in-depth.
func TestForwarder_Dial_FromDisplayName_CarriesPreserveANIWhenFromIsSIPUser(t *testing.T) {
	t.Parallel()
	factory := &stubDialFactory{
		returnedClient: newStubDialClient(successResp([]byte("v=0"))),
	}
	f, _, _ := newTestForwarder(factory, []string{"+49"})
	f.cfg.SIPUser = "trunkuser123"

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{CallerFrom: "+4915123preservedani"}
	if _, err := f.Dial(context.Background(), "CA1", "+4915123", opts, leg); err != nil {
		t.Fatalf("dial: %v", err)
	}
	if got := factory.lastFrom.User; got != "trunkuser123" {
		t.Errorf("From URI user = %q, want trunkuser123", got)
	}
	if got := factory.lastDisplayName; got != "4915123preservedani" {
		t.Errorf("From display-name = %q, want 4915123preservedani", got)
	}
}

// 5f-pre2. Display-name is omitted when display CID equals From URI's user
// (e.g. operator set DIAL_DEFAULT_CALLER_ID and From's addr-spec is the
// same number — no display-name needed, would be redundant).
func TestForwarder_Dial_FromDisplayName_OmittedWhenRedundant(t *testing.T) {
	t.Parallel()
	factory := &stubDialFactory{
		returnedClient: newStubDialClient(successResp([]byte("v=0"))),
	}
	f, _, _ := newTestForwarder(factory, []string{"+49"})
	f.cfg.SIPUser = "trunkuser123"
	f.cfg.DialDefaultCallerID = "+49operatordid"

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{CallerFrom: "+4915123preservedani"}
	if _, err := f.Dial(context.Background(), "CA1", "+4915123", opts, leg); err != nil {
		t.Fatalf("dial: %v", err)
	}
	if got := factory.lastFrom.User; got != "+49operatordid" {
		t.Errorf("From URI user = %q, want +49operatordid", got)
	}
	if got := factory.lastDisplayName; got != "" {
		t.Errorf("From display-name = %q, want empty (redundant)", got)
	}
}

// 5f. P-Preferred-Identity carries the display caller-ID when From is the
// SIP_USER (auth identity). This is the sipgate-compat path: trunks that
// require SIP_USER in From for auth still see a meaningful Caller-ID
// for the callee via PAI. Display CID precedence: TwiML callerId →
// DialDefaultCallerID → preserve-ANI.
func TestForwarder_Dial_PPI_CarriesPreserveANIWhenFromIsSIPUser(t *testing.T) {
	t.Parallel()
	factory := &stubDialFactory{
		returnedClient: newStubDialClient(successResp([]byte("v=0"))),
	}
	f, _, _ := newTestForwarder(factory, []string{"+49"})
	f.cfg.SIPUser = "trunkuser123"

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{
		CallerID:   "",
		CallerFrom: "+4915123preservedani",
	}
	if _, err := f.Dial(context.Background(), "CA1", "+4915123", opts, leg); err != nil {
		t.Fatalf("dial: %v", err)
	}
	// From URI carries the SIP_USER (sipgate auth-policy compatible).
	if got := factory.lastFrom.User; got != "trunkuser123" {
		t.Errorf("From URI user = %q, want trunkuser123", got)
	}
	// PPI carries the original ANI (what Phone B will see).
	if factory.lastPPI == nil {
		t.Fatalf("expected non-nil P-Preferred-Identity, got nil")
	}
	if got := factory.lastPPI.User; got != "4915123preservedani" {
		t.Errorf("PPI URI user = %q, want 4915123preservedani (preserve-ANI, "+" stripped by trunk normaliser)", got)
	}
}

// 5g. PPI carries DIAL_DEFAULT_CALLER_ID when set — operator-configured
// display takes precedence over preserve-ANI.
func TestForwarder_Dial_PPI_DefaultBeatsPreserveANI(t *testing.T) {
	t.Parallel()
	factory := &stubDialFactory{
		returnedClient: newStubDialClient(successResp([]byte("v=0"))),
	}
	f, _, _ := newTestForwarder(factory, []string{"+49"})
	f.cfg.SIPUser = "trunkuser123"
	f.cfg.DialDefaultCallerID = "+49operatordid"

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{CallerFrom: "+4915123preservedani"}
	if _, err := f.Dial(context.Background(), "CA1", "+4915123", opts, leg); err != nil {
		t.Fatalf("dial: %v", err)
	}
	// From and PPI both equal DIAL_DEFAULT_CALLER_ID → no PPI emitted (redundant).
	if got := factory.lastFrom.User; got != "+49operatordid" {
		t.Errorf("From URI user = %q, want +49operatordid", got)
	}
	// When From == display CID, PPI is omitted because it would be redundant.
	if factory.lastPPI != nil {
		t.Errorf("expected nil PPI when From == display CID, got %v", factory.lastPPI)
	}
}

// 5h. PPI carries TwiML callerId — explicit operator intent for display.
func TestForwarder_Dial_PPI_TwiMLCallerIDDrivesDisplay(t *testing.T) {
	t.Parallel()
	factory := &stubDialFactory{
		returnedClient: newStubDialClient(successResp([]byte("v=0"))),
	}
	f, _, _ := newTestForwarder(factory, []string{"+49"})
	f.cfg.SIPUser = "trunkuser123"

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{
		CallerID:   "+49twimldid",
		CallerFrom: "+4915123preservedani",
	}
	if _, err := f.Dial(context.Background(), "CA1", "+4915123", opts, leg); err != nil {
		t.Fatalf("dial: %v", err)
	}
	// TwiML callerId drives BOTH From and PPI — operator override is total.
	// (TwiML wins resolveCallerID step 1, and resolveDisplayCallerID step 1.)
	if got := factory.lastFrom.User; got != "+49twimldid" {
		t.Errorf("From URI user = %q, want +49twimldid", got)
	}
	if factory.lastPPI != nil {
		t.Errorf("expected nil PPI when From == display CID (both TwiML), got %v", factory.lastPPI)
	}
}

// 5i. No PPI emitted when there's nothing meaningful to display (no TwiML,
// no env, no preserve-ANI). Defensive: header is optional.
func TestForwarder_Dial_PPI_OmittedWhenNoDisplayCID(t *testing.T) {
	t.Parallel()
	factory := &stubDialFactory{
		returnedClient: newStubDialClient(successResp([]byte("v=0"))),
	}
	f, _, _ := newTestForwarder(factory, []string{"+49"})
	f.cfg.SIPUser = "trunkuser123"

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{} // no CallerID, no CallerFrom, no DefaultCID
	if _, err := f.Dial(context.Background(), "CA1", "+4915123", opts, leg); err != nil {
		t.Fatalf("dial: %v", err)
	}
	if factory.lastPPI != nil {
		t.Errorf("expected nil PPI when no display CID resolvable, got %v", factory.lastPPI)
	}
}

// 5e. Last-resort preserve-ANI when SIP_USER is absent and no explicit
// config — preserves Twilio behavior on trunks that accept third-party ANI
// (e.g. Twilio itself when number-verification is satisfied).
func TestForwarder_Dial_CallerIDFallback_PreserveANIAsLastResort(t *testing.T) {
	t.Parallel()
	factory := &stubDialFactory{
		returnedClient: newStubDialClient(successResp([]byte("v=0"))),
	}
	f, _, _ := newTestForwarder(factory, []string{"+49"})
	f.cfg.SIPUser = "" // simulate missing SIP_USER (test-fixture-only case)

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{
		CallerID:   "",
		CallerFrom: "+4915123preservedani",
	}
	if _, err := f.Dial(context.Background(), "CA1", "+4915123", opts, leg); err != nil {
		t.Fatalf("dial: %v", err)
	}
	if got := factory.lastFrom.User; got != "+4915123preservedani" {
		t.Errorf("expected From URI user=+4915123preservedani (preserve-ANI last resort, From itself NOT normalised), got %q", got)
	}
}

// 6. Caller-ID fallback: all empty + no default → ErrCallerIDRequired.
func TestForwarder_Dial_CallerIDFallback_Fail13214(t *testing.T) {
	t.Parallel()
	factory := &stubDialFactory{
		returnedClient: newStubDialClient(successResp([]byte("v=0"))),
	}
	f, metrics, _ := newTestForwarder(factory, []string{"+49"})
	f.cfg.DialDefaultCallerID = ""
	f.cfg.SIPUser = "" // disable SIP_USER auto-fallback so the chain reaches the error case

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{CallerID: "", CallerFrom: ""}
	res, err := f.Dial(context.Background(), "CA1", "+4915123", opts, leg)
	if err == nil {
		t.Fatal("expected ErrCallerIDRequired, got nil")
	}
	if !errors.Is(err, ErrCallerIDRequired) {
		t.Errorf("expected ErrCallerIDRequired, got %v", err)
	}
	if !strings.Contains(err.Error(), "13214") {
		t.Errorf("expected error to contain 13214, got %q", err.Error())
	}
	if atomic.LoadInt32(&factory.calls) != 0 {
		t.Errorf("expected factory.Dial NOT called when caller-ID resolution fails, got %d", factory.calls)
	}
	if got := counterVecValue(metrics.ForwardFailedTotal, "caller_id_rejected"); got != 1 {
		t.Errorf("expected forward_failed_total{reason=caller_id_rejected}=1, got %v", got)
	}
	if !callSidRE.MatchString(res.DialCallSid) {
		t.Errorf("DialCallSid not minted on caller-id failure: %q", res.DialCallSid)
	}
}

// 7. BuildSDPOffer error short-circuits before any INVITE work.
func TestForwarder_Dial_BuildSDPOfferFails(t *testing.T) {
	t.Parallel()
	factory := &stubDialFactory{}
	f, _, _ := newTestForwarder(factory, []string{"+49"})

	leg := &stubLegConfigurer{offerErr: fmt.Errorf("port pool exhausted")}
	_, err := f.Dial(context.Background(), "CA1", "+4915123", DialOpts{CallerID: "+4930"}, leg)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "port pool exhausted") {
		t.Errorf("expected wrapped offerErr, got %v", err)
	}
	if atomic.LoadInt32(&factory.calls) != 0 {
		t.Errorf("expected factory.Dial NOT called when offer fails, got %d", factory.calls)
	}
}

// 8. 200 OK answer → DialResult{Status:"answered"} + ForwardSuccessTotal++.
func TestForwarder_Dial_Answered200(t *testing.T) {
	t.Parallel()
	sdpBody := []byte("v=0\r\nm=audio 5000 RTP/AVP 0\r\n")
	stubClient := newStubDialClient(successResp(sdpBody))
	factory := &stubDialFactory{returnedClient: stubClient}
	f, metrics, _ := newTestForwarder(factory, []string{"+49"})

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	res, err := f.Dial(context.Background(), "CA1", "+4915123", DialOpts{CallerID: "+4930"}, leg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if res.Status != "answered" {
		t.Errorf("expected Status=answered, got %q", res.Status)
	}
	if res.SIPFinalCode != 200 {
		t.Errorf("expected SIPFinalCode=200, got %d", res.SIPFinalCode)
	}
	if got := counterValue(metrics.ForwardSuccessTotal); got != 1 {
		t.Errorf("expected forward_success_total=1, got %v", got)
	}
	if got := counterValue(metrics.ForwardAttemptsTotal); got != 1 {
		t.Errorf("expected forward_attempts_total=1, got %v", got)
	}
	if atomic.LoadInt32(&stubClient.ackCalls) != 1 {
		t.Errorf("expected exactly 1 ACK call, got %d", stubClient.ackCalls)
	}
	// Dual-leg activation hook MUST fire on the success path, otherwise the
	// caller-side rtpReader stays in StateDialingOut and audio never bridges.
	if got := atomic.LoadInt32(&leg.onAnsweredCallCount); got != 1 {
		t.Errorf("expected exactly 1 OnAnswered call, got %d", got)
	}
}

// 8b. OnAnswered error after 200 OK → BYE the dialog, surface "failed".
// Regression guard for the bug found during 15-06 live verify: without the
// dual-leg activation hook, audio could not bridge even when the SIP
// signaling succeeded. If the hook ever fails (socket bind error, missing
// remoteRTP, etc.), the Forwarder must tear down rather than silently
// leaving the call connected with no audio path.
func TestForwarder_Dial_OnAnsweredError_BYEs(t *testing.T) {
	t.Parallel()
	sdpBody := []byte("v=0\r\nm=audio 5000 RTP/AVP 0\r\n")
	stubClient := newStubDialClient(successResp(sdpBody))
	factory := &stubDialFactory{returnedClient: stubClient}
	f, _, _ := newTestForwarder(factory, []string{"+49"})

	leg := &stubLegConfigurer{
		offerSDP:      []byte("v=0"),
		offerPort:     10000,
		onAnsweredErr: errors.New("ListenUDP: address already in use"),
	}
	res, err := f.Dial(context.Background(), "CA1", "+4915123", DialOpts{CallerID: "+4930"}, leg)
	if err == nil {
		t.Fatal("expected dial error from OnAnswered failure, got nil")
	}
	if res == nil || res.Status != "failed" {
		t.Errorf("expected Status=failed, got result=%+v", res)
	}
	if !strings.Contains(err.Error(), "OnAnswered") {
		t.Errorf("expected error to mention OnAnswered, got %v", err)
	}
	if atomic.LoadInt32(&stubClient.byeCalls) != 1 {
		t.Errorf("expected exactly 1 BYE call (tear-down on OnAnswered error), got %d", stubClient.byeCalls)
	}
	if atomic.LoadInt32(&leg.onAnsweredCallCount) != 1 {
		t.Errorf("expected exactly 1 OnAnswered call, got %d", leg.onAnsweredCallCount)
	}
}

// 9. 486 Busy → DialResult{Status:"busy", Reason:"busy"}.
func TestForwarder_Dial_Busy486(t *testing.T) {
	t.Parallel()
	factory := &stubDialFactory{
		returnedClient: newStubDialClient(nil),
		returnedErr:    nonSuccessErr(siplib.StatusBusyHere, "Busy Here"),
	}
	f, metrics, _ := newTestForwarder(factory, []string{"+49"})

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	res, _ := f.Dial(context.Background(), "CA1", "+4915123", DialOpts{CallerID: "+4930"}, leg)

	if res.Status != "busy" {
		t.Errorf("expected Status=busy, got %q", res.Status)
	}
	if res.Reason != "busy" {
		t.Errorf("expected Reason=busy, got %q", res.Reason)
	}
	if res.SIPFinalCode != siplib.StatusBusyHere {
		t.Errorf("expected SIPFinalCode=486, got %d", res.SIPFinalCode)
	}
	if got := counterVecValue(metrics.ForwardFailedTotal, "busy"); got != 1 {
		t.Errorf("expected forward_failed_total{reason=busy}=1, got %v", got)
	}
}

// 10. 408 Request Timeout → no-answer.
func TestForwarder_Dial_NoAnswer408(t *testing.T) {
	t.Parallel()
	factory := &stubDialFactory{
		returnedClient: newStubDialClient(nil),
		returnedErr:    nonSuccessErr(siplib.StatusRequestTimeout, "Request Timeout"),
	}
	f, _, _ := newTestForwarder(factory, []string{"+49"})

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	res, _ := f.Dial(context.Background(), "CA1", "+4915123", DialOpts{CallerID: "+4930"}, leg)

	if res.Status != "no-answer" {
		t.Errorf("expected Status=no-answer, got %q", res.Status)
	}
	if res.SIPFinalCode != siplib.StatusRequestTimeout {
		t.Errorf("expected SIPFinalCode=408, got %d", res.SIPFinalCode)
	}
}

// 11. Ring timeout via context.DeadlineExceeded → CANCEL + Status=no-answer.
func TestForwarder_Dial_NoAnswerTimeout(t *testing.T) {
	t.Parallel()
	factory := &stubDialFactory{hangForever: true}
	f, _, _ := newTestForwarder(factory, []string{"+49"})

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{CallerID: "+4930", Timeout: 100 * time.Millisecond}

	start := time.Now()
	res, err := f.Dial(context.Background(), "CA1", "+4915123", opts, leg)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected ring-timeout error, got nil")
	}
	if res.Status != "no-answer" {
		t.Errorf("expected Status=no-answer, got %q", res.Status)
	}
	if elapsed > 1*time.Second {
		t.Errorf("ring timeout took too long: %v", elapsed)
	}
}

// 12. 603 Decline → Status=failed, Reason=rejected.
func TestForwarder_Dial_Reject603(t *testing.T) {
	t.Parallel()
	factory := &stubDialFactory{
		returnedClient: newStubDialClient(nil),
		returnedErr:    nonSuccessErr(603, "Decline"),
	}
	f, _, _ := newTestForwarder(factory, []string{"+49"})

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	res, _ := f.Dial(context.Background(), "CA1", "+4915123", DialOpts{CallerID: "+4930"}, leg)

	if res.Status != "failed" {
		t.Errorf("expected Status=failed, got %q", res.Status)
	}
	if res.Reason != "rejected" {
		t.Errorf("expected Reason=rejected, got %q", res.Reason)
	}
}

// 12b. 403 with sipgate "Username in From Field required" reason → Status=failed,
// Reason=caller_id_rejected. Regression guard: trunks that reject
// third-party ANI must surface as caller_id_rejected so operators can
// fix via DIAL_DEFAULT_CALLER_ID rather than chasing a generic 4xx.
func TestForwarder_Dial_403_CallerIDRejected(t *testing.T) {
	t.Parallel()
	factory := &stubDialFactory{
		returnedClient: newStubDialClient(nil),
		returnedErr:    nonSuccessErr(403, "Username in From Field required"),
	}
	f, metrics, _ := newTestForwarder(factory, []string{"+49"})

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	res, _ := f.Dial(context.Background(), "CA1", "+4915123", DialOpts{CallerID: "+4930"}, leg)

	if res.Status != "failed" {
		t.Errorf("expected Status=failed, got %q", res.Status)
	}
	if res.Reason != "caller_id_rejected" {
		t.Errorf("expected Reason=caller_id_rejected, got %q", res.Reason)
	}
	if res.SIPFinalCode != 403 {
		t.Errorf("expected SIPFinalCode=403, got %d", res.SIPFinalCode)
	}
	if got := counterVecValue(metrics.ForwardFailedTotal, "caller_id_rejected"); got != 1 {
		t.Errorf("expected forward_failed_total{reason=caller_id_rejected}=1, got %v", got)
	}
}

// 12c. 403 with non-caller-ID reason → falls through to generic error bucket.
// Confirms isFromRejectionReason doesn't over-match on unrelated 403s.
func TestForwarder_Dial_403_UnrelatedReason_GenericError(t *testing.T) {
	t.Parallel()
	factory := &stubDialFactory{
		returnedClient: newStubDialClient(nil),
		returnedErr:    nonSuccessErr(403, "Forbidden"),
	}
	f, _, _ := newTestForwarder(factory, []string{"+49"})

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	res, _ := f.Dial(context.Background(), "CA1", "+4915123", DialOpts{CallerID: "+4930"}, leg)

	if res.Status != "failed" {
		t.Errorf("expected Status=failed, got %q", res.Status)
	}
	if res.Reason == "caller_id_rejected" {
		t.Errorf("expected Reason != caller_id_rejected for unrelated 403, got %q", res.Reason)
	}
}

// 13. 503 Service Unavailable → Reason=trunk_5xx.
func TestForwarder_Dial_Trunk5xx(t *testing.T) {
	t.Parallel()
	factory := &stubDialFactory{
		returnedClient: newStubDialClient(nil),
		returnedErr:    nonSuccessErr(503, "Service Unavailable"),
	}
	f, _, _ := newTestForwarder(factory, []string{"+49"})

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	res, _ := f.Dial(context.Background(), "CA1", "+4915123", DialOpts{CallerID: "+4930"}, leg)

	if res.Status != "failed" {
		t.Errorf("expected Status=failed, got %q", res.Status)
	}
	if res.Reason != "trunk_5xx" {
		t.Errorf("expected Reason=trunk_5xx, got %q", res.Reason)
	}
	if res.SIPFinalCode != 503 {
		t.Errorf("expected SIPFinalCode=503, got %d", res.SIPFinalCode)
	}
}

// 14. Codec mismatch on 200 OK → ACK + immediate Bye + Status=failed/codec_mismatch.
func TestForwarder_Dial_CodecMismatch(t *testing.T) {
	t.Parallel()
	stubClient := newStubDialClient(successResp([]byte("v=0\r\nm=audio 5000 RTP/AVP 9\r\n")))
	factory := &stubDialFactory{returnedClient: stubClient}
	f, metrics, _ := newTestForwarder(factory, []string{"+49"})

	leg := &stubLegConfigurer{
		offerSDP:  []byte("v=0"),
		offerPort: 10000,
		acceptErr: fmt.Errorf("forwarder: codec mismatch detected by leg"),
	}
	res, err := f.Dial(context.Background(), "CA1", "+4915123", DialOpts{CallerID: "+4930"}, leg)

	if err == nil {
		t.Fatal("expected codec_mismatch error, got nil")
	}
	if res.Status != "failed" {
		t.Errorf("expected Status=failed, got %q", res.Status)
	}
	if res.Reason != "codec_mismatch" {
		t.Errorf("expected Reason=codec_mismatch, got %q", res.Reason)
	}
	if atomic.LoadInt32(&stubClient.ackCalls) != 1 {
		t.Errorf("expected ACK to be sent (per RFC 3261 §17.1.1.3) before BYE, got %d", stubClient.ackCalls)
	}
	if atomic.LoadInt32(&stubClient.byeCalls) != 1 {
		t.Errorf("expected BYE after codec_mismatch ACK, got %d", stubClient.byeCalls)
	}
	if got := counterVecValue(metrics.ForwardFailedTotal, "codec_mismatch"); got != 1 {
		t.Errorf("expected forward_failed_total{reason=codec_mismatch}=1, got %v", got)
	}
}

// 15. timeLimit watchdog fires → BYE the dialog → Status=answered.
func TestForwarder_Dial_TimeLimitExpiry(t *testing.T) {
	t.Parallel()
	stubClient := newStubDialClientOpenDone(successResp([]byte("v=0\r\nm=audio 5000 RTP/AVP 0\r\n")))
	factory := &stubDialFactory{returnedClient: stubClient}
	f, _, _ := newTestForwarder(factory, []string{"+49"})

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{CallerID: "+4930", TimeLimit: 100 * time.Millisecond}

	start := time.Now()
	res, err := f.Dial(context.Background(), "CA1", "+4915123", opts, leg)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if res.Status != "answered" {
		t.Errorf("expected Status=answered (timeLimit-driven natural end), got %q", res.Status)
	}
	if elapsed < 100*time.Millisecond {
		t.Errorf("expected at least 100ms elapsed (timeLimit window), got %v", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("timeLimit watchdog stalled — elapsed=%v", elapsed)
	}
	if atomic.LoadInt32(&stubClient.byeCalls) < 1 {
		t.Errorf("expected at least 1 BYE from timeLimit watchdog, got %d", stubClient.byeCalls)
	}
}

// 16. Hangup-on-star: '*' DTMF triggers BYE → Status=hangup-star.
func TestForwarder_Dial_HangupOnStar(t *testing.T) {
	t.Parallel()
	stubClient := newStubDialClientOpenDone(successResp([]byte("v=0\r\nm=audio 5000 RTP/AVP 0\r\n")))
	factory := &stubDialFactory{returnedClient: stubClient}
	f, _, _ := newTestForwarder(factory, []string{"+49"})

	dtmfChan := make(chan rune, 4)
	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{
		CallerID:     "+4930",
		HangupOnStar: true,
		DTMFChan:     dtmfChan,
	}

	// Send '*' shortly after the dial begins so awaitDialogEnd is in select.
	go func() {
		time.Sleep(50 * time.Millisecond)
		dtmfChan <- '*'
	}()

	start := time.Now()
	res, err := f.Dial(context.Background(), "CA1", "+4915123", opts, leg)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if res.Status != "hangup-star" {
		t.Errorf("expected Status=hangup-star, got %q", res.Status)
	}
	if elapsed > 2*time.Second {
		t.Errorf("hangup-on-star stalled — elapsed=%v", elapsed)
	}
	if atomic.LoadInt32(&stubClient.byeCalls) < 1 {
		t.Errorf("expected at least 1 BYE from star watcher, got %d", stubClient.byeCalls)
	}
}

// 17. DialCallSid is minted on every Dial — even guardrails-rejected paths.
func TestForwarder_Dial_DialCallSidMinted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		factory *stubDialFactory
		prefix  []string
	}{
		{"happy-path", &stubDialFactory{
			returnedClient: newStubDialClient(successResp([]byte("v=0"))),
		}, []string{"+49"}},
		{"toll-fraud", &stubDialFactory{}, nil}, // empty allow-list = deny-all
		{"busy-486", &stubDialFactory{
			returnedClient: newStubDialClient(nil),
			returnedErr:    nonSuccessErr(siplib.StatusBusyHere, "Busy Here"),
		}, []string{"+49"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, _, _ := newTestForwarder(tc.factory, tc.prefix)
			leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
			res, _ := f.Dial(context.Background(), "CA1", "+4915123", DialOpts{CallerID: "+4930"}, leg)
			if !callSidRE.MatchString(res.DialCallSid) {
				t.Errorf("[%s] DialCallSid pattern mismatch: %q", tc.name, res.DialCallSid)
			}
		})
	}
}

// 18. AuthChallenge metric: 401 challenge response → AuthChallengeKind{kind=401}=1.
func TestForwarder_Dial_AuthChallengeMetric_401(t *testing.T) {
	t.Parallel()
	stubClient := newStubDialClient(successResp([]byte("v=0\r\nm=audio 5000 RTP/AVP 0\r\n")))
	factory := &stubDialFactory{
		returnedClient:        stubClient,
		simulateAuthChallenge: "401",
	}
	f, metrics, _ := newTestForwarder(factory, []string{"+49"})

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	res, err := f.Dial(context.Background(), "CA1", "+4915123", DialOpts{CallerID: "+4930"}, leg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	if got := counterVecValue(metrics.AuthChallengeKind, "401"); got != 1 {
		t.Errorf("expected auth_challenge_kind{kind=401}=1, got %v", got)
	}
	if got := counterVecValue(metrics.AuthChallengeKind, "407"); got != 0 {
		t.Errorf("expected auth_challenge_kind{kind=407}=0, got %v", got)
	}
	if res.AuthChallenge != "401" {
		t.Errorf("expected DialResult.AuthChallenge=401, got %q", res.AuthChallenge)
	}
}
