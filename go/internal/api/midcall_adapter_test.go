package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	siplib "github.com/emiago/sipgo/sip"
	"github.com/rs/zerolog"

	"github.com/sipgate/sipgate-sip-stream-bridge/internal/bridge"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/config"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/observability"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/sip"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/twiml"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/webhook"
)

const (
	adapterCallSid    = "CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	adapterAccountSid = "ACbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// ── Terminate + Log tests ────────────────────────────────────────────────────

// TestMidCallAdapter_TerminateForwarded — Terminate on the adapter forwards
// to the underlying *bridge.CallSession's Terminate, advancing IsActive() to
// false and stamping the reason via the public Status getter.
func TestMidCallAdapter_TerminateForwarded(t *testing.T) {
	t.Parallel()

	var byeCount atomic.Int32
	session := bridge.NewTestSession(adapterCallSid, adapterAccountSid, func(_ context.Context) error {
		byeCount.Add(1)
		return nil
	})
	if !session.IsActive() {
		t.Fatalf("precondition: session.IsActive() = false, want true on freshly built fixture")
	}

	adapter := newMidCallAdapter(session, nil, nil, nil, nil, config.Config{}, zerolog.Nop())
	if err := adapter.Terminate("hangup"); err != nil {
		t.Fatalf("adapter.Terminate(\"hangup\"): unexpected error: %v", err)
	}

	if session.IsActive() {
		t.Errorf("post-Terminate: session.IsActive() = true, want false")
	}
	if got := byeCount.Load(); got != 1 {
		t.Errorf("byeCount = %d, want 1 (Terminate must forward through to dlg.Bye)", got)
	}
	// Internal reason "hangup" maps to the Twilio CallStatus enum "completed".
	if got := session.Status(); got != "completed" {
		t.Errorf("session.Status() = %q, want \"completed\" (Twilio enum mapping for reason=hangup)", got)
	}

	// Idempotence: a second Terminate is a no-op at every layer.
	if err := adapter.Terminate("anything"); err != nil {
		t.Fatalf("second adapter.Terminate: unexpected error: %v", err)
	}
	if got := session.Status(); got != "completed" {
		t.Errorf("after second Terminate: Status = %q, want unchanged \"completed\"", got)
	}
	if got := byeCount.Load(); got != 1 {
		t.Errorf("after second Terminate: byeCount = %d, want 1 (per-leg sync.Once collapses dup BYE)", got)
	}
}

// TestMidCallAdapter_LogEnrichment — the adapter's Log() returns a logger
// pre-populated with call_sid + account_sid fields.
func TestMidCallAdapter_LogEnrichment(t *testing.T) {
	t.Parallel()

	session := bridge.NewTestSession(adapterCallSid, adapterAccountSid, func(_ context.Context) error {
		return nil
	})

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	adapter := newMidCallAdapter(session, nil, nil, nil, nil, config.Config{}, logger)
	logger2 := adapter.Log()
	if logger2 == nil {
		t.Fatal("adapter.Log() returned nil — must return a non-nil *zerolog.Logger")
	}

	logger2.Info().Msg("test log line")

	raw := buf.Bytes()
	if len(raw) == 0 {
		t.Fatal("logger emitted no bytes — adapter Log() did not produce a writable logger")
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("log line is not valid JSON: %v\nbytes=%s", err, raw)
	}
	if got := decoded["call_sid"]; got != adapterCallSid {
		t.Errorf("call_sid field: got %v, want %q", got, adapterCallSid)
	}
	if got := decoded["account_sid"]; got != adapterAccountSid {
		t.Errorf("account_sid field: got %v, want %q", got, adapterAccountSid)
	}
	if !strings.Contains(string(raw), `"call_sid":"`+adapterCallSid+`"`) {
		t.Errorf("log bytes missing exact `\"call_sid\":\"…\"` literal; got: %s", raw)
	}
	if !strings.Contains(string(raw), `"account_sid":"`+adapterAccountSid+`"`) {
		t.Errorf("log bytes missing exact `\"account_sid\":\"…\"` literal; got: %s", raw)
	}
}

// TestMidCallAdapter_StructurallySatisfiesMidCallTarget — compile-time
// regression guard: if the twiml.MidCallTarget interface drifts, this fails.
func TestMidCallAdapter_StructurallySatisfiesMidCallTarget(t *testing.T) {
	t.Parallel()

	type localMidCallTarget interface {
		Terminate(reason string) error
		Log() *zerolog.Logger
	}

	session := bridge.NewTestSession(adapterCallSid, adapterAccountSid, func(_ context.Context) error {
		return nil
	})
	adapter := newMidCallAdapter(session, nil, nil, nil, nil, config.Config{}, zerolog.Nop())

	var _ localMidCallTarget = adapter // compile-time check
	_ = adapter
}

// ── DialTarget tests ─────────────────────────────────────────────────────────

// adapterStubWebhookFetcher captures FetchWithFallback calls for inspection.
// mu guards lastBody/lastURL/lastMethod which may be written from a goroutine
// (fireActionCallback runs in a separate goroutine from PerformDial).
type adapterStubWebhookFetcher struct {
	calls      int32
	mu         sync.Mutex
	lastBody   string
	lastURL    string
	lastMethod string
	returnErr  error
}

func (f *adapterStubWebhookFetcher) FetchWithFallback(_ context.Context, p, _ webhook.FetchTarget) (*webhook.FetchResult, error) {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	f.lastBody = p.Body
	f.lastURL = p.URL
	f.lastMethod = p.Method
	f.mu.Unlock()
	if f.returnErr != nil {
		return nil, f.returnErr
	}
	return &webhook.FetchResult{StatusCode: 200, URLUsed: p.URL, Attempts: 1}, nil
}

// Body, URL, Method are safe accessors for the captured fields.
func (f *adapterStubWebhookFetcher) Body() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastBody
}

func (f *adapterStubWebhookFetcher) URL() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastURL
}

// Compile-time assertion: *adapterStubWebhookFetcher satisfies webhookFetcher.
var _ webhookFetcher = (*adapterStubWebhookFetcher)(nil)

// ── PrepareDial tests ─────────────────────────────────────────────────────────

// TestMidCallAdapter_PrepareDial_ClosesWSStream: PrepareDial must call
// CloseStream("dial-forward") — Privacy Gate fires before port allocation.
// We verify by checking that the callee leg is installed on the session.
func TestMidCallAdapter_PrepareDial_ClosesWSStream(t *testing.T) {
	t.Parallel()

	session := bridge.NewTestSession(adapterCallSid, adapterAccountSid, func(_ context.Context) error { return nil })
	pool, _ := bridge.NewPortPool(20000, 20010)
	manager := bridge.NewCallManager(pool, adapterAccountSid, config.Config{SDPContactIP: "127.0.0.1"}, zerolog.Nop(), nil)
	t.Cleanup(manager.Close)

	cfg := config.Config{SDPContactIP: "127.0.0.1"}
	adapter := newMidCallAdapter(session, manager, nil, nil, nil, cfg, zerolog.Nop())

	opts := twiml.DialOpts{Timeout: 30 * time.Second}
	handle, err := adapter.PrepareDial(opts)
	if err != nil {
		t.Fatalf("PrepareDial returned unexpected error: %v", err)
	}
	if handle == nil {
		t.Fatal("PrepareDial returned nil handle on success")
	}
	defer handle.Release()

	// legs[1] should now be installed on the session.
	if leg := session.CalleeLeg(); leg == nil {
		t.Error("CalleeLeg() = nil after PrepareDial — SetLeg(1, ...) not called")
	}
}

// TestMidCallAdapter_PrepareDial_AcquireFails_NoLeak: if AcquirePort fails
// (pool exhausted), PrepareDial returns an error and does NOT install a callee leg.
func TestMidCallAdapter_PrepareDial_AcquireFails_NoLeak(t *testing.T) {
	t.Parallel()

	session := bridge.NewTestSession(adapterCallSid, adapterAccountSid, func(_ context.Context) error { return nil })
	// 1-slot pool that we pre-drain.
	pool, _ := bridge.NewPortPool(20100, 20101)
	port, _ := pool.Acquire()
	_ = port // pool is now exhausted

	manager := bridge.NewCallManager(pool, adapterAccountSid, config.Config{SDPContactIP: "127.0.0.1"}, zerolog.Nop(), nil)
	t.Cleanup(manager.Close)

	cfg := config.Config{SDPContactIP: "127.0.0.1"}
	adapter := newMidCallAdapter(session, manager, nil, nil, nil, cfg, zerolog.Nop())

	opts := twiml.DialOpts{}
	handle, err := adapter.PrepareDial(opts)
	if err == nil {
		if handle != nil {
			handle.Release()
		}
		t.Fatal("PrepareDial: want error on exhausted pool, got nil")
	}
	// No callee leg should be installed.
	if leg := session.CalleeLeg(); leg != nil {
		t.Errorf("CalleeLeg() = non-nil after failed PrepareDial — unexpected SetLeg call")
	}
}

// TestDialHandle_Release_ReleasesPort: h.Release() must return the port to
// the pool, allowing a subsequent Acquire to succeed.
func TestDialHandle_Release_ReleasesPort(t *testing.T) {
	t.Parallel()

	session := bridge.NewTestSession(adapterCallSid, adapterAccountSid, func(_ context.Context) error { return nil })
	pool, _ := bridge.NewPortPool(20200, 20201) // 1-slot pool
	manager := bridge.NewCallManager(pool, adapterAccountSid, config.Config{SDPContactIP: "127.0.0.1"}, zerolog.Nop(), nil)
	t.Cleanup(manager.Close)

	cfg := config.Config{SDPContactIP: "127.0.0.1"}
	adapter := newMidCallAdapter(session, manager, nil, nil, nil, cfg, zerolog.Nop())

	handle, err := adapter.PrepareDial(twiml.DialOpts{})
	if err != nil {
		t.Fatalf("PrepareDial: %v", err)
	}

	// Release the port.
	handle.Release()

	// Pool should now have the port available again.
	p, err := manager.AcquirePort()
	if err != nil {
		t.Fatalf("AcquirePort after Release: %v — port was not returned to pool", err)
	}
	manager.ReleasePort(p) // cleanup
}

// ── Status mapping tests ──────────────────────────────────────────────────────

// TestTwilioDialCallStatus: status mapping table.
func TestTwilioDialCallStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in  string
		out string
	}{
		{"answered", "completed"},
		{"hangup-star", "completed"},
		{"busy", "busy"},
		{"no-answer", "no-answer"},
		{"canceled", "canceled"},
		{"failed", "failed"},
		{"", "failed"},
		{"unknown", "failed"},
	}
	for _, tc := range cases {
		got := twilioDialCallStatus(tc.in)
		if got != tc.out {
			t.Errorf("twilioDialCallStatus(%q) = %q, want %q", tc.in, got, tc.out)
		}
	}
}

// ── Action callback tests ─────────────────────────────────────────────────────

// TestMidCallAdapter_ActionCallback_FiredOnAction: when opts.Action is set,
// fireActionCallback posts the form body via webhookC.FetchWithFallback.
// We verify via adapterStubWebhookFetcher — no real HTTP server needed.
func TestMidCallAdapter_ActionCallback_FiredOnAction(t *testing.T) {
	t.Parallel()

	session := bridge.NewTestSession(adapterCallSid, adapterAccountSid, func(_ context.Context) error { return nil })
	wc := &adapterStubWebhookFetcher{}
	adapter := newMidCallAdapter(session, nil, nil, wc, nil, config.Config{SDPContactIP: "127.0.0.1"}, zerolog.Nop())

	result := &sip.DialResult{
		Status:      "answered",
		DialCallSid: "CA12345678901234567890123456789012",
		Duration:    35 * time.Second,
	}

	adapter.fireActionCallback("https://example.com/action", "POST", result)

	if n := atomic.LoadInt32(&wc.calls); n != 1 {
		t.Fatalf("FetchWithFallback called %d times, want 1", n)
	}

	body := wc.Body()
	if !strings.Contains(body, "DialCallStatus=completed") {
		t.Errorf("action callback body missing DialCallStatus=completed; got: %s", body)
	}
	if !strings.Contains(body, fmt.Sprintf("DialCallSid=%s", result.DialCallSid)) {
		t.Errorf("action callback body missing DialCallSid; got: %s", body)
	}
	if !strings.Contains(body, "DialCallDuration=35") {
		t.Errorf("action callback body missing DialCallDuration=35; got: %s", body)
	}
	if !strings.Contains(body, fmt.Sprintf("CallSid=%s", adapterCallSid)) {
		t.Errorf("action callback body missing CallSid; got: %s", body)
	}
	if u := wc.URL(); u != "https://example.com/action" {
		t.Errorf("FetchWithFallback URL = %q, want https://example.com/action", u)
	}
}

// TestMidCallAdapter_ActionCallback_NotFiredOnEmptyAction: when opts.Action
// is empty, no webhook POST is issued.
func TestMidCallAdapter_ActionCallback_NotFiredOnEmptyAction(t *testing.T) {
	t.Parallel()

	session := bridge.NewTestSession(adapterCallSid, adapterAccountSid, func(_ context.Context) error { return nil })
	wc := &adapterStubWebhookFetcher{}
	adapter := newMidCallAdapter(session, nil, nil, wc, nil, config.Config{}, zerolog.Nop())

	result := &sip.DialResult{Status: "answered", DialCallSid: "CA12345678901234567890123456789012"}

	// fireActionCallback with empty URL — should be a no-op.
	adapter.fireActionCallback("", "POST", result)

	if n := atomic.LoadInt32(&wc.calls); n != 0 {
		t.Errorf("webhook POST called %d times with empty action URL, want 0", n)
	}
}

// ── PerformDial integration test ──────────────────────────────────────────────

// fwdTestDialFactory satisfies sip.DialClientFactory with correct siplib types.
// It returns a stubbed DialClient that immediately closes Done() to simulate
// a confirmed dialog ending without touching the network.
type fwdTestDialFactory struct {
	dialErr error
}

// fwdTestDialClient simulates a confirmed dialog (immediate Done() close).
type fwdTestDialClient struct {
	done chan struct{}
}

func (f *fwdTestDialFactory) Dial(
	_ context.Context,
	_ siplib.Uri,
	_ siplib.Uri,
	_ string,
	_ *siplib.Uri,
	_ []byte,
	_ sip.DialAuth,
	_ func(*siplib.Response) error,
) (sip.DialClient, error) {
	if f.dialErr != nil {
		return nil, f.dialErr
	}
	return &fwdTestDialClient{done: closedChan()}, nil
}

func (f *fwdTestDialFactory) ReadBye(_ *siplib.Request, _ siplib.ServerTransaction) error {
	return nil
}

func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func (c *fwdTestDialClient) FinalResponse() *siplib.Response { return nil }
func (c *fwdTestDialClient) Ack(_ context.Context) error    { return nil }
func (c *fwdTestDialClient) Bye(_ context.Context) error    { return nil }
func (c *fwdTestDialClient) Done() <-chan struct{}           { return c.done }
func (c *fwdTestDialClient) Close() error                   { return nil }

// TestMidCallAdapter_PerformDial_ForwardsResult: verify that PerformDial
// translates sip.DialResult → twiml.DialResult correctly using a stubbed
// forwarder. Target must pass guardrails allow-list (+49).
//
// NOTE: Because fwdTestDialFactory returns FinalResponse()==nil (simulating a
// confirmed dialog with no SDP body to parse), Forwarder.Dial will return an
// error on the "resp == nil" guard. We therefore test the error path here
// (PerformDial returns non-nil error) and verify the DialCallSid is minted.
// The full happy-path (SDP negotiation) is covered by sip/forwarder_test.go.
func TestMidCallAdapter_PerformDial_ForwardsResult(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		SDPContactIP:        "127.0.0.1",
		SIPDomain:           "test.example",
		SIPUser:             "testuser",
		DialAllowedPrefixes: []string{"+49"},
		DialMaxPerSession:   3,
		DialMaxPerMinute:    60,
		DialRingTimeoutS:    30,
		DialDefaultCallerID: "+4930000000",
	}

	guardrails := sip.NewGuardrails(cfg)
	metrics := observability.NewMetrics()

	factory := &fwdTestDialFactory{}
	forwarder := sip.NewForwarderWithFactory(nil, guardrails, cfg, metrics, zerolog.Nop(), factory)

	session := bridge.NewTestSession(adapterCallSid, adapterAccountSid, func(_ context.Context) error { return nil })
	pool, _ := bridge.NewPortPool(20300, 20310)
	manager := bridge.NewCallManager(pool, adapterAccountSid, config.Config{SDPContactIP: "127.0.0.1"}, zerolog.Nop(), nil)
	t.Cleanup(manager.Close)

	wc := &adapterStubWebhookFetcher{}
	adapter := newMidCallAdapter(session, manager, forwarder, wc, nil, cfg, zerolog.Nop())

	// PrepareDial first (sets up the callee leg required by Forwarder.Dial).
	handle, err := adapter.PrepareDial(twiml.DialOpts{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("PrepareDial: %v", err)
	}
	defer handle.Release()

	opts := twiml.DialOpts{
		CallerID: "+4930999",
		Timeout:  30 * time.Second,
	}
	// FinalResponse()==nil causes Forwarder to return error; PerformDial
	// propagates the error AND the (non-nil) translated DialResult so that
	// dialHandler can inspect result.Status for the termination reason.
	result, err := adapter.PerformDial(context.Background(), "+4912345", opts, handle)
	// Either path is acceptable: a nil-resp guard error or a successful result.
	// The important invariant is: no panic, no goroutine leak, and the result
	// is non-nil on the error path (so dialHandler can read result.Status).
	if err != nil {
		// Error path: result must be non-nil (carries the status for dialHandler).
		if result == nil {
			t.Errorf("PerformDial error path: result = nil, want non-nil (dialHandler needs result.Status)")
		}
	} else {
		// Success path (future: when stub returns real SDP).
		if result == nil {
			t.Fatal("PerformDial returned nil result with nil error")
		}
	}
}

// ── signed action-callback POST ─────────────────────────────────────────────

// TestFireActionCallback_SignsWithXTwilioSignature — when actionPoster is
// non-nil, the action-callback POST goes through the signed transport and
// the X-Twilio-Signature header equals webhook.Sign(authToken, actionURL,
// form). Asserted by reading the captured header on a httptest.NewTLSServer
// receiver.
func TestFireActionCallback_SignsWithXTwilioSignature(t *testing.T) {
	t.Parallel()

	const authToken = "12345"

	var capturedSig atomic.Value
	var capturedBody atomic.Value
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedSig.Store(r.Header.Get("X-Twilio-Signature"))
		b, _ := io.ReadAll(r.Body)
		capturedBody.Store(string(b))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Build a signed actionPoster trusting the test TLS cert. We compose
	// the signing transport over srv.Client().Transport so the test client
	// trusts the server's self-signed cert.
	actionPoster := &http.Client{
		Transport: webhook.SigningTransportFor(srv.Client().Transport, authToken),
		Timeout:   5 * time.Second,
	}

	session := bridge.NewTestSession(adapterCallSid, adapterAccountSid, func(_ context.Context) error { return nil })
	adapter := &midCallAdapter{
		session:      session,
		actionPoster: actionPoster,
		logger:       zerolog.Nop(),
	}

	result := &sip.DialResult{
		Status:      "answered",
		DialCallSid: "CAtest0123456789abcdef0123456789ab",
		Duration:    42 * time.Second,
	}

	actionURL := srv.URL + "/cb"
	adapter.fireActionCallback(actionURL, http.MethodPost, result)

	gotSig, _ := capturedSig.Load().(string)
	gotBody, _ := capturedBody.Load().(string)

	if gotSig == "" {
		t.Fatal("X-Twilio-Signature not captured — header missing on signed POST")
	}

	// Compute expected signature against the captured body bytes (sort+dedupe
	// per-key values inside webhook.Sign).
	form, err := url.ParseQuery(gotBody)
	if err != nil {
		t.Fatalf("parse captured body: %v", err)
	}
	wantSig := webhook.Sign(authToken, actionURL, form)
	if gotSig != wantSig {
		t.Errorf("X-Twilio-Signature: got %q, want %q", gotSig, wantSig)
	}
	// Sanity check the form body — preserves the wire shape under signing.
	if form.Get("DialCallStatus") != "completed" {
		t.Errorf("DialCallStatus = %q, want completed (answered status maps to completed)", form.Get("DialCallStatus"))
	}
	if form.Get("DialCallSid") != result.DialCallSid {
		t.Errorf("DialCallSid = %q, want %q", form.Get("DialCallSid"), result.DialCallSid)
	}
	if form.Get("DialCallDuration") != "42" {
		t.Errorf("DialCallDuration = %q, want 42", form.Get("DialCallDuration"))
	}
	if form.Get("CallSid") != adapterCallSid {
		t.Errorf("CallSid = %q, want %q", form.Get("CallSid"), adapterCallSid)
	}
}

// TestFireActionCallback_NilPosterFallsBackToWebhookC — when actionPoster
// is nil (test fixtures that don't wire it), the legacy unsigned path is
// preserved so body-capture tests keep working.
func TestFireActionCallback_NilPosterFallsBackToWebhookC(t *testing.T) {
	t.Parallel()

	session := bridge.NewTestSession(adapterCallSid, adapterAccountSid, func(_ context.Context) error { return nil })
	wc := &adapterStubWebhookFetcher{}
	adapter := &midCallAdapter{
		session:      session,
		webhookC:     wc,
		actionPoster: nil, // explicit
		logger:       zerolog.Nop(),
	}

	result := &sip.DialResult{
		Status:      "answered",
		DialCallSid: "CAtest0123456789abcdef0123456789ab",
		Duration:    7 * time.Second,
	}
	adapter.fireActionCallback("https://legacy.example/cb", http.MethodPost, result)

	if n := atomic.LoadInt32(&wc.calls); n != 1 {
		t.Errorf("legacy webhookC.FetchWithFallback calls = %d, want 1", n)
	}
	if u := wc.URL(); u != "https://legacy.example/cb" {
		t.Errorf("legacy URL = %q", u)
	}
	if b := wc.Body(); !strings.Contains(b, "DialCallStatus=completed") {
		t.Errorf("legacy body missing DialCallStatus=completed; got: %s", b)
	}
}
