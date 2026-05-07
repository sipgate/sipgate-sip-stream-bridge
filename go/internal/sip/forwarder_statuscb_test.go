package sip

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	siplib "github.com/emiago/sipgo/sip"
	"github.com/rs/zerolog"

	"github.com/sipgate/sipgate-sip-stream-bridge/internal/config"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/observability"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/webhook"
)

// ── <Dial>-leg status-callback emission tests ──────────────────────────────
//
// These tests exercise emitDialInitiated / emitDialRinging / emitDialAnswered
// end-to-end via a httptest.NewTLSServer that captures the form bodies of the
// Twilio-shape POSTs, the per-leg SequenceNumber generator, the no-subscription
// silent-drop semantic, and the I-6 ringing-on-180 hook.
//
// Test scaffolding extends stubDialFactory with a `provisional` slice — when
// set, each entry is fed through the onResponse callback before the final
// response is returned. This lets us drive the 180 → 200 sequence the I-6
// fix relies on.

// stubDialFactory180 wraps stubDialFactory with a provisional-response queue
// so we can simulate 180 / 183 / etc. before the final response. The embedded
// pointer is shared so existing accessor patterns (calls, returnedErr, etc.)
// keep working.
type stubDialFactory180 struct {
	*stubDialFactory
	provisional []*siplib.Response
}

func (f *stubDialFactory180) Dial(
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

	for _, p := range f.provisional {
		if err := onResponse(p); err != nil {
			return nil, err
		}
	}
	if f.hangForever {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.returnedClient, f.returnedErr
}

// capturedCallback records a single received status-callback POST.
type capturedCallback struct {
	URL    string
	Method string
	Form   url.Values
	Header http.Header
}

// newCaptureServer builds an httptest.NewTLSServer that captures every form
// body it receives. Returns the server, a slice pointer, and the mutex
// guarding it.
func newCaptureServer(t *testing.T) (*httptest.Server, *[]capturedCallback, *sync.Mutex) {
	t.Helper()
	captured := []capturedCallback{}
	var mu sync.Mutex
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		captured = append(captured, capturedCallback{
			URL:    r.URL.String(),
			Method: r.Method,
			Form:   r.PostForm,
			Header: r.Header.Clone(),
		})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	return srv, &captured, &mu
}

// statusCbTestCfg builds a minimal config.Config that lets resolveCallerID
// succeed with TwiML callerId set explicitly in DialOpts. Mirrors the
// newTestForwarder helper but specialised for status-callback tests.
func statusCbTestCfg() config.Config {
	return config.Config{
		SIPUser:               "testuser",
		SIPPassword:           "testpass",
		SIPDomain:             "sipconnect.sipgate.de",
		SIPRegistrar:          "sipconnect.sipgate.de",
		SDPContactIP:          "10.0.0.1",
		DialAllowedPrefixes:   []string{"+49"},
		DialDefaultCallerID:   "+49default",
		DialRingTimeoutS:      30,
		DialMaxPerSession:     10,
		DialMaxPerMinute:      60,
	}
}

// newStatusCbForwarder builds a Forwarder wired with a captureServer-backed
// StatusClient and the given factory. accountSid is the derived AccountSid
// emitted in every status-callback form payload.
func newStatusCbForwarder(t *testing.T, factory DialClientFactory, srv *httptest.Server, accountSid string) *Forwarder {
	t.Helper()
	cfg := statusCbTestCfg()
	metrics := observability.NewMetrics()
	wc := webhook.NewStatusClientForTest(srv.Client().Transport, "12345abcdef", metrics, zerolog.Nop())
	return &Forwarder{
		agent:      nil,
		guardrails: NewGuardrails(cfg),
		cfg:        cfg,
		metrics:    metrics,
		log:        zerolog.Nop(),
		factory:    factory,
		statusWC:   wc,
		accountSid: accountSid,
	}
}

// drainServer waits up to 2s for the captureServer to receive `expected`
// POSTs. The per-CallSid worker is goroutine-driven, so the receiver must
// poll with a short deadline.
func drainServer(t *testing.T, mu *sync.Mutex, captured *[]capturedCallback, expected int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(*captured)
		mu.Unlock()
		if n >= expected {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("captureServer received %d POSTs, expected %d (got: %v)", len(*captured), expected, *captured)
}

// settleNoExtra waits 200ms and asserts no MORE POSTs arrive than `expected`.
// Used to detect spurious duplicates (e.g. 180 retransmissions firing twice).
func settleNoExtra(t *testing.T, mu *sync.Mutex, captured *[]capturedCallback, expected int) {
	t.Helper()
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(*captured) > expected {
		t.Fatalf("captureServer received %d POSTs, expected exactly %d (extras: %v)", len(*captured), expected, (*captured)[expected:])
	}
}

// sortBySeq sorts captured POSTs by their SequenceNumber form field so the
// assertion order is stable even if the worker delivers out of arrival order
// (it doesn't — the per-CallSid worker is serial — but be defensive).
func sortBySeq(captured []capturedCallback) []capturedCallback {
	out := append([]capturedCallback(nil), captured...)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Form.Get("SequenceNumber") < out[j].Form.Get("SequenceNumber")
	})
	return out
}

// ── Tests ────────────────────────────────────────────────────────────────────

// TestForwarder_EmitsInitiatedAtDialEntry — Forwarder.Dial entry hook fires
// the "initiated" event AFTER guardrails pass and BEFORE the SIP INVITE
// hits the wire.
func TestForwarder_EmitsInitiatedAtDialEntry(t *testing.T) {
	t.Parallel()
	srv, captured, mu := newCaptureServer(t)
	defer srv.Close()

	stubClient := newStubDialClient(successResp([]byte("v=0\r\nm=audio 5000 RTP/AVP 0\r\n")))
	factory := &stubDialFactory{returnedClient: stubClient}
	f := newStatusCbForwarder(t, factory, srv, "ACtest0123456789abcdef0123456789ab")

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{
		CallerID:              "+4930",
		CallerFrom:            "+4915123ani",
		StatusCallback:        srv.URL + "/cb",
		StatusCallbackMethod:  "POST",
		StatusCallbackEvents:  []string{"initiated", "answered"},
	}
	if _, err := f.Dial(context.Background(), "CAcaller000000000000000000000000000", "+4915123", opts, leg); err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Expect TWO POSTs in order: initiated (seq=0) then answered (seq=1).
	drainServer(t, mu, captured, 2)

	mu.Lock()
	got := sortBySeq(*captured)
	mu.Unlock()

	if got[0].Form.Get("CallStatus") != "queued" {
		t.Errorf("[0] CallStatus = %q, want queued (initiated event)", got[0].Form.Get("CallStatus"))
	}
	if got[0].Form.Get("SequenceNumber") != "0" {
		t.Errorf("[0] SequenceNumber = %q, want 0", got[0].Form.Get("SequenceNumber"))
	}
	if got[1].Form.Get("CallStatus") != "in-progress" {
		t.Errorf("[1] CallStatus = %q, want in-progress (answered event)", got[1].Form.Get("CallStatus"))
	}
	if got[1].Form.Get("SequenceNumber") != "1" {
		t.Errorf("[1] SequenceNumber = %q, want 1", got[1].Form.Get("SequenceNumber"))
	}
	if got[0].Form.Get("Direction") != "outbound-dial" {
		t.Errorf("Direction = %q, want outbound-dial", got[0].Form.Get("Direction"))
	}
	if got[0].Form.Get("ParentCallSid") != "CAcaller000000000000000000000000000" {
		t.Errorf("ParentCallSid = %q, want CAcaller…", got[0].Form.Get("ParentCallSid"))
	}
	// CallSid in the form is the callee's DialCallSid (NOT the caller's).
	if got[0].Form.Get("CallSid") == "CAcaller000000000000000000000000000" {
		t.Errorf("CallSid form value should be the callee DialCallSid, not the caller's CallSid")
	}
	if !callSidRE.MatchString(got[0].Form.Get("CallSid")) {
		t.Errorf("CallSid = %q does not match CA[0-9a-f]{32}", got[0].Form.Get("CallSid"))
	}
	if got[0].Form.Get("AccountSid") != "ACtest0123456789abcdef0123456789ab" {
		t.Errorf("AccountSid = %q", got[0].Form.Get("AccountSid"))
	}
	if got[0].Form.Get("From") != "+4915123ani" {
		t.Errorf("From = %q", got[0].Form.Get("From"))
	}
	if got[0].Form.Get("CallbackSource") != "call-progress-events" {
		t.Errorf("CallbackSource = %q", got[0].Form.Get("CallbackSource"))
	}
}

// TestForwarder_NoSubscriptionNoEmit — empty StatusCallback URL ⇒ ZERO POSTs.
func TestForwarder_NoSubscriptionNoEmit(t *testing.T) {
	t.Parallel()
	srv, captured, mu := newCaptureServer(t)
	defer srv.Close()

	stubClient := newStubDialClient(successResp([]byte("v=0\r\nm=audio 5000 RTP/AVP 0\r\n")))
	factory := &stubDialFactory{returnedClient: stubClient}
	f := newStatusCbForwarder(t, factory, srv, "ACtest0123456789abcdef0123456789ab")

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{
		CallerID:   "+4930",
		CallerFrom: "+4915123ani",
		// StatusCallback intentionally empty
	}
	if _, err := f.Dial(context.Background(), "CAcaller000000000000000000000000000", "+4915123", opts, leg); err != nil {
		t.Fatalf("dial: %v", err)
	}
	settleNoExtra(t, mu, captured, 0)
}

// TestForwarder_EmitsOnlySubscribedEvents — DialOpts subscribes to
// {"initiated"} only ⇒ exactly ONE POST (no answered event).
func TestForwarder_EmitsOnlySubscribedEvents(t *testing.T) {
	t.Parallel()
	srv, captured, mu := newCaptureServer(t)
	defer srv.Close()

	stubClient := newStubDialClient(successResp([]byte("v=0\r\nm=audio 5000 RTP/AVP 0\r\n")))
	factory := &stubDialFactory{returnedClient: stubClient}
	f := newStatusCbForwarder(t, factory, srv, "ACtest0123456789abcdef0123456789ab")

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{
		CallerID:              "+4930",
		CallerFrom:            "+4915123ani",
		StatusCallback:        srv.URL + "/cb",
		StatusCallbackEvents:  []string{"initiated"},
	}
	if _, err := f.Dial(context.Background(), "CAcaller000000000000000000000000000", "+4915123", opts, leg); err != nil {
		t.Fatalf("dial: %v", err)
	}
	drainServer(t, mu, captured, 1)
	settleNoExtra(t, mu, captured, 1)

	mu.Lock()
	got := *captured
	mu.Unlock()
	if got[0].Form.Get("CallStatus") != "queued" {
		t.Errorf("CallStatus = %q, want queued", got[0].Form.Get("CallStatus"))
	}
}

// TestForwarder_EmitsRingingOn180Callee — I-6 fix: a callee 180 fires exactly
// ONE ringing event. Subsequent 180 retransmissions DO NOT fire duplicate
// ringing events (sync.Once dedupes).
func TestForwarder_EmitsRingingOn180Callee(t *testing.T) {
	t.Parallel()
	srv, captured, mu := newCaptureServer(t)
	defer srv.Close()

	stubClient := newStubDialClient(successResp([]byte("v=0\r\nm=audio 5000 RTP/AVP 0\r\n")))
	// Two 180 provisional responses (simulates a retransmission), then the
	// stub returns the 200 OK final response. The hook MUST fire ringing
	// exactly once.
	provisional := []*siplib.Response{
		siplib.NewResponse(180, "Ringing"),
		siplib.NewResponse(180, "Ringing"), // retransmission
	}
	base := &stubDialFactory{returnedClient: stubClient}
	factory := &stubDialFactory180{stubDialFactory: base, provisional: provisional}
	f := newStatusCbForwarder(t, factory, srv, "ACtest0123456789abcdef0123456789ab")

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{
		CallerID:              "+4930",
		CallerFrom:            "+4915123ani",
		StatusCallback:        srv.URL + "/cb",
		StatusCallbackEvents:  []string{"initiated", "ringing", "answered"},
	}
	if _, err := f.Dial(context.Background(), "CAcaller000000000000000000000000000", "+4915123", opts, leg); err != nil {
		t.Fatalf("dial: %v", err)
	}

	drainServer(t, mu, captured, 3)
	settleNoExtra(t, mu, captured, 3)

	mu.Lock()
	got := sortBySeq(*captured)
	mu.Unlock()

	if got[0].Form.Get("CallStatus") != "queued" {
		t.Errorf("[0] CallStatus = %q, want queued (initiated)", got[0].Form.Get("CallStatus"))
	}
	if got[1].Form.Get("CallStatus") != "ringing" {
		t.Errorf("[1] CallStatus = %q, want ringing", got[1].Form.Get("CallStatus"))
	}
	if got[1].Form.Get("SequenceNumber") != "1" {
		t.Errorf("[1] SequenceNumber = %q, want 1", got[1].Form.Get("SequenceNumber"))
	}
	if got[2].Form.Get("CallStatus") != "in-progress" {
		t.Errorf("[2] CallStatus = %q, want in-progress (answered)", got[2].Form.Get("CallStatus"))
	}
	if got[2].Form.Get("SequenceNumber") != "2" {
		t.Errorf("[2] SequenceNumber = %q, want 2", got[2].Form.Get("SequenceNumber"))
	}
}

// TestForwarder_PerLegSequenceCounterIndependent — two concurrent dials each
// produce their own SequenceNumber={0,1,…} space; no cross-leg interleaving.
func TestForwarder_PerLegSequenceCounterIndependent(t *testing.T) {
	t.Parallel()
	srv, captured, mu := newCaptureServer(t)
	defer srv.Close()

	stubClient := newStubDialClient(successResp([]byte("v=0\r\nm=audio 5000 RTP/AVP 0\r\n")))
	factory := &stubDialFactory{returnedClient: stubClient}
	f := newStatusCbForwarder(t, factory, srv, "ACtest0123456789abcdef0123456789ab")

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{
		CallerID:              "+4930",
		CallerFrom:            "+4915123ani",
		StatusCallback:        srv.URL + "/cb",
		StatusCallbackEvents:  []string{"initiated", "answered"},
	}
	// Two SEQUENTIAL dials — each gets its own DialCallSid and thus its own
	// per-leg seq counter starting at 0.
	for i := 0; i < 2; i++ {
		if _, err := f.Dial(context.Background(), "CAcaller000000000000000000000000000", "+4915123", opts, leg); err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
	}
	drainServer(t, mu, captured, 4)

	// Group POSTs by CallSid (each leg has its own CallSid). Each group MUST
	// contain exactly two events with seq {0,1}.
	mu.Lock()
	cap := append([]capturedCallback(nil), *captured...)
	mu.Unlock()
	byCallSid := map[string][]capturedCallback{}
	for _, c := range cap {
		sid := c.Form.Get("CallSid")
		byCallSid[sid] = append(byCallSid[sid], c)
	}
	if len(byCallSid) != 2 {
		t.Fatalf("expected 2 distinct CallSids (one per leg), got %d: %v", len(byCallSid), byCallSid)
	}
	for sid, evts := range byCallSid {
		evts = sortBySeq(evts)
		if len(evts) != 2 {
			t.Errorf("leg %s: %d events, want 2", sid, len(evts))
			continue
		}
		if evts[0].Form.Get("SequenceNumber") != "0" || evts[1].Form.Get("SequenceNumber") != "1" {
			t.Errorf("leg %s: sequence numbers = (%s, %s), want (0, 1)", sid,
				evts[0].Form.Get("SequenceNumber"), evts[1].Form.Get("SequenceNumber"))
		}
	}
}

// TestForwarder_ReleaseLegSequence — calling ReleaseLegSequence then a fresh
// nextLegSequence on the same DialCallSid resets the counter to 0 (the
// counter map entry is gone; the next call recreates it).
func TestForwarder_ReleaseLegSequence(t *testing.T) {
	t.Parallel()
	cfg := statusCbTestCfg()
	f := &Forwarder{
		cfg:        cfg,
		guardrails: NewGuardrails(cfg),
		metrics:    observability.NewMetrics(),
		log:        zerolog.Nop(),
		factory:    &stubDialFactory{},
	}
	dialSid := "CAdial0000000000000000000000000000"
	if got := f.nextLegSequence(dialSid); got != 0 {
		t.Errorf("first nextLegSequence = %d, want 0", got)
	}
	if got := f.nextLegSequence(dialSid); got != 1 {
		t.Errorf("second nextLegSequence = %d, want 1", got)
	}
	f.ReleaseLegSequence(dialSid)
	if got := f.nextLegSequence(dialSid); got != 0 {
		t.Errorf("nextLegSequence after Release = %d, want 0 (counter recreated)", got)
	}
	// Idempotent
	f.ReleaseLegSequence("CAnonexistent00000000000000000000000")
}

// TestForwarder_NilStatusClient_NoOp — Forwarder constructed without a
// StatusClient (test-fixture pattern) does NOT crash on dial-leg events.
func TestForwarder_NilStatusClient_NoOp(t *testing.T) {
	t.Parallel()
	cfg := statusCbTestCfg()
	stubClient := newStubDialClient(successResp([]byte("v=0\r\nm=audio 5000 RTP/AVP 0\r\n")))
	factory := &stubDialFactory{returnedClient: stubClient}
	f := &Forwarder{
		agent:      nil,
		guardrails: NewGuardrails(cfg),
		cfg:        cfg,
		metrics:    observability.NewMetrics(),
		log:        zerolog.Nop(),
		factory:    factory,
		// statusWC intentionally nil
	}

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{
		CallerID:              "+4930",
		CallerFrom:            "+4915123ani",
		StatusCallback:        "https://customer.example/cb",
		StatusCallbackEvents:  []string{"initiated", "answered"},
	}
	res, err := f.Dial(context.Background(), "CAcaller000000000000000000000000000", "+4915123", opts, leg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if res.Status != "answered" {
		t.Errorf("expected Status=answered, got %q", res.Status)
	}
}

// TestForwarder_DialStatusCallback_NoExplicitEvents_SubscribeToAll
// is a regression test for the empty-events subscribe-to-all default. A
// `<Dial statusCallback="https://X">+4912345</Dial>` (URL set, no
// statusCallbackEvent attribute) is the canonical Twilio "subscribe to
// all events" form. A previous version short-circuited on empty
// StatusCallbackEvents and emitted ZERO callbacks — customer
// integrations that relied on Twilio's documented default produced no
// callbacks. With the canonical webhook.SubscriptionMatches, nil/empty
// events are treated as subscribe-to-all, so this dial produces all
// FOUR lifecycle/terminal callbacks (initiated, ringing, answered,
// completed).
//
// Drives the live Forwarder against an httptest.NewTLSServer with
// stubDialFactory180 wrapping (one 180 provisional then 200 OK final)
// to fire the full initiated → ringing → answered → completed chain.
func TestForwarder_DialStatusCallback_NoExplicitEvents_SubscribeToAll(t *testing.T) {
	t.Parallel()
	srv, captured, mu := newCaptureServer(t)
	defer srv.Close()

	stubClient := newStubDialClient(successResp([]byte("v=0\r\nm=audio 5000 RTP/AVP 0\r\n")))
	provisional := []*siplib.Response{
		siplib.NewResponse(180, "Ringing"),
	}
	base := &stubDialFactory{returnedClient: stubClient}
	factory := &stubDialFactory180{stubDialFactory: base, provisional: provisional}
	f := newStatusCbForwarder(t, factory, srv, "ACtest0123456789abcdef0123456789ab")

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{
		CallerID:       "+4930",
		CallerFrom:     "+4915123ani",
		StatusCallback: srv.URL + "/cb",
		// StatusCallbackEvents intentionally NOT set — Twilio default
		// "subscribe to all events" default.
	}
	if _, err := f.Dial(context.Background(), "CAcaller000000000000000000000000000", "+4915123", opts, leg); err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Expect FOUR POSTs: initiated, ringing, answered, completed.
	// A previous version asserted zero (the short-circuit dropped them
	// all); an intermediate version asserted three (the dial-leg terminal
	// "completed" event was not implemented yet).
	drainServer(t, mu, captured, 4)
	settleNoExtra(t, mu, captured, 4)

	mu.Lock()
	got := sortBySeq(*captured)
	mu.Unlock()

	if got[0].Form.Get("CallStatus") != "queued" {
		t.Errorf("[0] CallStatus = %q, want queued (initiated event)", got[0].Form.Get("CallStatus"))
	}
	if got[1].Form.Get("CallStatus") != "ringing" {
		t.Errorf("[1] CallStatus = %q, want ringing", got[1].Form.Get("CallStatus"))
	}
	if got[2].Form.Get("CallStatus") != "in-progress" {
		t.Errorf("[2] CallStatus = %q, want in-progress (answered event)", got[2].Form.Get("CallStatus"))
	}
	if got[3].Form.Get("CallStatus") != "completed" {
		t.Errorf("[3] CallStatus = %q, want completed (terminal event)", got[3].Form.Get("CallStatus"))
	}
	// SequenceNumber must be 0/1/2/3 (per-leg counter) — proves single
	// CallSid receives all four lifecycle/terminal events.
	for i, c := range got {
		if c.Form.Get("SequenceNumber") != strconv.Itoa(i) {
			t.Errorf("[%d] SequenceNumber = %q, want %d", i, c.Form.Get("SequenceNumber"), i)
		}
	}
}

// TestForwarder_DialStatusCallback_ExplicitInitiatedOnly — companion
// regression test proving that the specific-match path still works
// after the empty-events short-circuit removal. With
// StatusCallbackEvents=["initiated"] only ONE POST arrives (the
// "answered" event is not subscribed and must NOT fall back to
// "completed" because answered is a lifecycle event, not terminal).
//
// The existing TestForwarder_EmitsOnlySubscribedEvents covers this
// shape too; the explicit pairing with the empty-events subscribe-to-
// all test above makes the boundary case visible side-by-side.
func TestForwarder_DialStatusCallback_ExplicitInitiatedOnly(t *testing.T) {
	t.Parallel()
	srv, captured, mu := newCaptureServer(t)
	defer srv.Close()

	stubClient := newStubDialClient(successResp([]byte("v=0\r\nm=audio 5000 RTP/AVP 0\r\n")))
	factory := &stubDialFactory{returnedClient: stubClient}
	f := newStatusCbForwarder(t, factory, srv, "ACtest0123456789abcdef0123456789ab")

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{
		CallerID:             "+4930",
		CallerFrom:           "+4915123ani",
		StatusCallback:       srv.URL + "/cb",
		StatusCallbackEvents: []string{"initiated"},
	}
	if _, err := f.Dial(context.Background(), "CAcaller000000000000000000000000000", "+4915123", opts, leg); err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Expect EXACTLY ONE POST — the initiated event.
	drainServer(t, mu, captured, 1)
	settleNoExtra(t, mu, captured, 1)

	mu.Lock()
	got := *captured
	mu.Unlock()
	if got[0].Form.Get("CallStatus") != "queued" {
		t.Errorf("CallStatus = %q, want queued (initiated)", got[0].Form.Get("CallStatus"))
	}
}

// ── Leak regression suite for ReleaseLegSequence wiring. ─────────────────────

// legSeqsCount returns the number of entries in f.legSeqs sync.Map. Used by
// the leak tests below to prove ReleaseLegSequence is wired correctly. Lives
// in this _test.go file (excluded from production binary; mirrors the
// webhook/export_test.go pattern).
func legSeqsCount(f *Forwarder) int {
	n := 0
	f.legSeqs.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// TestForwarder_Dial_ReleasesLegSequence proves a single Dial() call that
// allocates a per-leg sequence counter (via emitDialInitiated →
// nextLegSequence) returns with the counter freed (legSeqs map is empty).
// Exercises the deferred ReleaseLegSequence wiring inside Forwarder.Dial.
func TestForwarder_Dial_ReleasesLegSequence(t *testing.T) {
	t.Parallel()
	srv, captured, mu := newCaptureServer(t)
	defer srv.Close()

	stubClient := newStubDialClient(successResp([]byte("v=0\r\nm=audio 5000 RTP/AVP 0\r\n")))
	factory := &stubDialFactory{returnedClient: stubClient}
	f := newStatusCbForwarder(t, factory, srv, "ACtest0123456789abcdef0123456789ab")

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{
		CallerID:             "+4930",
		CallerFrom:           "+4915123ani",
		StatusCallback:       srv.URL + "/cb",
		StatusCallbackEvents: []string{"initiated", "answered"},
	}
	if _, err := f.Dial(context.Background(), "CAcaller000000000000000000000000000", "+4915123", opts, leg); err != nil {
		t.Fatalf("dial: %v", err)
	}
	drainServer(t, mu, captured, 2)

	// After Dial returns, the per-leg counter must have been released by
	// the deferred ReleaseLegSequence call inside Forwarder.Dial.
	if got := legSeqsCount(f); got != 0 {
		t.Fatalf("legSeqs entries after single Dial() = %d, want 0 (defer f.ReleaseLegSequence not wired or not firing)", got)
	}
}

// TestForwarder_Dial_HighChurn_LegSeqsReturnsToZero proves at scale that
// 100 sequential Dial() lifecycles drain back to 0 entries in f.legSeqs.
// The idle-state acceptance criterion is "1000 sequential dials →
// 0 entries"; we use 100 to keep test runtime bounded under 5s.
func TestForwarder_Dial_HighChurn_LegSeqsReturnsToZero(t *testing.T) {
	t.Parallel()
	srv, captured, mu := newCaptureServer(t)
	defer srv.Close()

	// Custom config: bump per-session + per-minute caps high enough
	// that 100 sequential dials don't trip the rate limits. The
	// production-default statusCbTestCfg has DialMaxPerSession=10
	// (per-CallSid) + DialMaxPerMinute=60, both of which would short-
	// circuit this test mid-loop. The leak invariant is independent
	// of the rate-limit semantics; the high-churn dimension is what
	// matters.
	cfg := statusCbTestCfg()
	cfg.DialMaxPerSession = 10000
	cfg.DialMaxPerMinute = 10000
	stubClient := newStubDialClient(successResp([]byte("v=0\r\nm=audio 5000 RTP/AVP 0\r\n")))
	factory := &stubDialFactory{returnedClient: stubClient}
	metrics := observability.NewMetrics()
	wc := webhook.NewStatusClientForTest(srv.Client().Transport, "12345abcdef", metrics, zerolog.Nop())
	f := &Forwarder{
		agent:      nil,
		guardrails: NewGuardrails(cfg),
		cfg:        cfg,
		metrics:    metrics,
		log:        zerolog.Nop(),
		factory:    factory,
		statusWC:   wc,
		accountSid: "ACtest0123456789abcdef0123456789ab",
	}

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{
		CallerID:             "+4930",
		CallerFrom:           "+4915123ani",
		StatusCallback:       srv.URL + "/cb",
		StatusCallbackEvents: []string{"initiated", "answered"},
	}

	const N = 100
	for i := 0; i < N; i++ {
		// Use a fresh caller SID per iteration so the guardrails
		// per-session dial limit (DialMaxPerSession=10 in
		// statusCbTestCfg) does not short-circuit at iteration 10. The
		// global per-minute cap (DialMaxPerMinute=60) is also avoided
		// because OnSessionEnd is not invoked here — instead we keep
		// DialMaxPerMinute high enough by relying on the test's
		// short wall-clock window. Mirror real-world usage where each
		// call has a distinct CallSid.
		callerSid := fmt.Sprintf("CAcaller%026d", i)
		if _, err := f.Dial(context.Background(), callerSid, "+4915123", opts, leg); err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		// Per-iteration assertion: the deferred Release fires when Dial
		// returns, so legSeqs is empty between iterations even on the
		// happy path.
		if got := legSeqsCount(f); got != 0 {
			t.Fatalf("legSeqs entries after dial #%d = %d, want 0 (per-iteration defer must fire on Dial return)", i, got)
		}
	}
	drainServer(t, mu, captured, 2*N)

	if got := legSeqsCount(f); got != 0 {
		t.Fatalf("legSeqs entries after %d Dial() lifecycles = %d, want 0 (release fix not effective at scale)", N, got)
	}
}

// TestForwarder_Dial_GuardrailsBlock_LegSeqsClean_AndReusable — strengthened
// per WARNING 11 of 16-11 revision feedback. The original "no leak on
// guardrails-block" invariant is structurally trivial since the counter
// was never created in the first place (emitDialInitiated runs AFTER
// guardrails). Strengthening: verify that a guardrails-rejected dial does
// NOT pollute the counter namespace observed by SUBSEQUENT dials — a
// later successful dial must still allocate sequence 0 (proving no orphan
// counter from the rejected dial).
//
// Setup: a Forwarder with deny-all guardrails to drive the rejection,
// then we mutate the guardrails to allow-all (sharing the SAME legSeqs
// map) and run a successful dial. Final assertion: the successful dial's
// captured POSTs report SequenceNumber=0 (no orphan from the rejected
// dial), and legSeqs returns to 0 after the successful dial completes
// (defer fires correctly).
func TestForwarder_Dial_GuardrailsBlock_LegSeqsClean_AndReusable(t *testing.T) {
	t.Parallel()
	srv, captured, mu := newCaptureServer(t)
	defer srv.Close()

	// Step 1: build a Forwarder with deny-all guardrails. The
	// statusCbTestCfg helper sets DialAllowedPrefixes=["+49"], so a
	// number outside that prefix is rejected.
	stubClient := newStubDialClient(successResp([]byte("v=0\r\nm=audio 5000 RTP/AVP 0\r\n")))
	factory := &stubDialFactory{returnedClient: stubClient}
	f := newStatusCbForwarder(t, factory, srv, "ACtest0123456789abcdef0123456789ab")

	// Step 2: dial a NON-allowlisted number → guardrails CheckDial
	// returns ErrTollFraudBlocked → Forwarder.Dial early-returns BEFORE
	// emitDialInitiated. legSeqs must be empty (trivially) and the
	// returned error must wrap the toll-fraud sentinel.
	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{
		CallerID:             "+4930",
		CallerFrom:           "+4915123ani",
		StatusCallback:       srv.URL + "/cb",
		StatusCallbackEvents: []string{"initiated", "answered"},
	}
	_, err := f.Dial(context.Background(), "CAcaller000000000000000000000000000", "+1555guardblock", opts, leg)
	if err == nil {
		t.Fatal("expected guardrails block error on non-allowlisted +1 dial; got nil")
	}
	if !errors.Is(err, ErrTollFraudBlocked) {
		t.Fatalf("expected ErrTollFraudBlocked in chain, got %v", err)
	}

	// Step 3: assert legSeqs is empty (trivially — the rejected dial
	// short-circuited before nextLegSequence allocated anything).
	if got := legSeqsCount(f); got != 0 {
		t.Fatalf("legSeqs entries after guardrails-block = %d, want 0 (guardrails-rejected dial must not allocate counter)", got)
	}
	// And the captured POSTs must contain ZERO entries (guardrails
	// rejection short-circuits before emitDialInitiated).
	settleNoExtra(t, mu, captured, 0)

	// Step 4 — STRENGTHENING (WARNING 11): a SUBSEQUENT dial with an
	// allowed prefix must still allocate sequence 0 (no orphan counter
	// from the rejected dial). Reuse the SAME Forwarder f — its legSeqs
	// map is shared between the two dials.
	if _, err := f.Dial(context.Background(), "CAcaller000000000000000000000000000", "+4915123", opts, leg); err != nil {
		t.Fatalf("subsequent allowed dial: %v", err)
	}
	drainServer(t, mu, captured, 2)

	mu.Lock()
	got := sortBySeq(*captured)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("captured POST count = %d, want 2 (initiated + answered for the allowed dial)", len(got))
	}
	// The strengthening assertion: the FIRST event (initiated) must
	// report SequenceNumber=0. If a previous orphan counter from the
	// guardrails-rejected dial leaked into legSeqs, this dial would
	// observe a non-zero starting sequence (the orphan counter would
	// have been incremented by nextLegSequence).
	if got[0].Form.Get("SequenceNumber") != "0" {
		t.Errorf("subsequent dial's initiated event SequenceNumber = %q, want \"0\" (orphan counter from guardrails-rejected dial polluted namespace — WARNING 11 strengthening failed)",
			got[0].Form.Get("SequenceNumber"))
	}
	if got[1].Form.Get("SequenceNumber") != "1" {
		t.Errorf("subsequent dial's answered event SequenceNumber = %q, want \"1\"",
			got[1].Form.Get("SequenceNumber"))
	}

	// Step 5: after the successful dial completes, legSeqs returns to 0
	// (proves the defer fires on the successful path too).
	if got := legSeqsCount(f); got != 0 {
		t.Fatalf("legSeqs entries after successful dial completed = %d, want 0 (defer Release must fire on success path)", got)
	}
}

// ── <Dial>-leg terminal status-callback event tests ────────────────────────
//
// These tests exercise the 5 emitDialCompleted/Busy/Failed/NoAnswer/
// Canceled wrappers + recordFailure chokepoint emission + ReleaseLegSequence
// + non-blocking DrainAndClose at terminal-dispatch. Each test:
//   1. Drives Forwarder.Dial through the matching outcome (success / 486 /
//      408 / 487 / 503).
//   2. Asserts the terminal callback arrived with correct CallStatus,
//      ParentCallSid, Direction=outbound-dial, and per-leg SequenceNumber.
//   3. Asserts legSeqs returns to 0 (the explicit Release fires at
//      terminal-dispatch — the earlier defer is now a safety-net).

// findCallback returns the first captured callback whose CallStatus form
// value matches the supplied status. nil if not found. mu is the captured
// slice's mutex (the caller must NOT hold it).
func findCallback(captured *[]capturedCallback, mu *sync.Mutex, callStatus string) *capturedCallback {
	mu.Lock()
	defer mu.Unlock()
	for i := range *captured {
		if (*captured)[i].Form.Get("CallStatus") == callStatus {
			c := (*captured)[i]
			return &c
		}
	}
	return nil
}

// assertTerminalCallback checks the common invariants every dial-leg
// terminal callback must satisfy: ParentCallSid, Direction, per-leg
// SequenceNumber populated, CallSid is the callee DialCallSid (not the
// parent's), AccountSid + From propagated.
func assertTerminalCallback(t *testing.T, c *capturedCallback, wantStatus, parentCallSid string) {
	t.Helper()
	if c == nil {
		t.Fatalf("no terminal callback captured for CallStatus=%q", wantStatus)
	}
	if got := c.Form.Get("CallStatus"); got != wantStatus {
		t.Errorf("CallStatus = %q, want %q", got, wantStatus)
	}
	if got := c.Form.Get("ParentCallSid"); got != parentCallSid {
		t.Errorf("ParentCallSid = %q, want %q", got, parentCallSid)
	}
	if got := c.Form.Get("Direction"); got != "outbound-dial" {
		t.Errorf("Direction = %q, want outbound-dial", got)
	}
	if got := c.Form.Get("CallbackSource"); got != "call-progress-events" {
		t.Errorf("CallbackSource = %q, want call-progress-events", got)
	}
	if !callSidRE.MatchString(c.Form.Get("CallSid")) {
		t.Errorf("CallSid = %q does not match CA[0-9a-f]{32}", c.Form.Get("CallSid"))
	}
	if c.Form.Get("CallSid") == parentCallSid {
		t.Errorf("CallSid form value should be callee DialCallSid, not parent CallSid")
	}
	if got := c.Form.Get("SequenceNumber"); got == "" {
		t.Errorf("SequenceNumber missing")
	}
}

// TestForwarder_DialTerminalEvent_Completed — success path emits "completed"
// terminal event after the dialog naturally ends (newStubDialClient pre-
// closes the done channel). Asserts seq=3 (after initiated/ringing/answered)
// and that legSeqs is empty after Dial returns.
func TestForwarder_DialTerminalEvent_Completed(t *testing.T) {
	t.Parallel()
	srv, captured, mu := newCaptureServer(t)
	defer srv.Close()

	stubClient := newStubDialClient(successResp([]byte("v=0\r\nm=audio 5000 RTP/AVP 0\r\n")))
	provisional := []*siplib.Response{siplib.NewResponse(180, "Ringing")}
	base := &stubDialFactory{returnedClient: stubClient}
	factory := &stubDialFactory180{stubDialFactory: base, provisional: provisional}
	f := newStatusCbForwarder(t, factory, srv, "ACtest0123456789abcdef0123456789ab")

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	parentSid := "CAcaller000000000000000000000000000"
	opts := DialOpts{
		CallerID:             "+4930",
		CallerFrom:           "+4915123ani",
		StatusCallback:       srv.URL + "/cb",
		StatusCallbackEvents: []string{"initiated", "ringing", "answered", "completed"},
	}
	res, err := f.Dial(context.Background(), parentSid, "+4915123", opts, leg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if res.Status != "answered" {
		t.Fatalf("res.Status = %q, want answered", res.Status)
	}

	// Expect FOUR POSTs: initiated/ringing/answered/completed.
	drainServer(t, mu, captured, 4)
	settleNoExtra(t, mu, captured, 4)

	terminal := findCallback(captured, mu, "completed")
	assertTerminalCallback(t, terminal, "completed", parentSid)
	if got := terminal.Form.Get("SequenceNumber"); got != "3" {
		t.Errorf("SequenceNumber = %q, want 3 (after initiated=0/ringing=1/answered=2)", got)
	}

	// legSeqs MUST be empty — the explicit Release at the success-path
	// terminal-dispatch site fires; the earlier defer is a safety-net
	// (idempotent).
	if got := legSeqsCount(f); got != 0 {
		t.Errorf("legSeqs entries after completed terminal = %d, want 0", got)
	}
}

// TestForwarder_DialTerminalEvent_Busy — callee returns 486 → terminal
// "busy" event. dispatchFinalResponse maps 486 → Status=busy/Reason=busy;
// recordFailure chokepoint emits the matching terminal event.
func TestForwarder_DialTerminalEvent_Busy(t *testing.T) {
	t.Parallel()
	srv, captured, mu := newCaptureServer(t)
	defer srv.Close()

	// 486 Busy Here as a final non-2xx (wrapped in sipgo.ErrDialogResponse).
	factory := &stubDialFactory{returnedErr: nonSuccessErr(486, "Busy Here")}
	f := newStatusCbForwarder(t, factory, srv, "ACtest0123456789abcdef0123456789ab")

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	parentSid := "CAcaller000000000000000000000000000"
	opts := DialOpts{
		CallerID:             "+4930",
		CallerFrom:           "+4915123ani",
		StatusCallback:       srv.URL + "/cb",
		StatusCallbackEvents: []string{"initiated", "busy"},
	}
	res, _ := f.Dial(context.Background(), parentSid, "+4915123", opts, leg)
	if res.Status != "busy" {
		t.Fatalf("res.Status = %q, want busy", res.Status)
	}

	// Expect TWO POSTs: initiated + busy. (No ringing — provisional
	// responses were not emitted; no answered — the call never connected.)
	drainServer(t, mu, captured, 2)
	settleNoExtra(t, mu, captured, 2)

	terminal := findCallback(captured, mu, "busy")
	assertTerminalCallback(t, terminal, "busy", parentSid)

	if got := legSeqsCount(f); got != 0 {
		t.Errorf("legSeqs entries after busy terminal = %d, want 0", got)
	}
}

// TestForwarder_DialTerminalEvent_NoAnswer — callee returns 408 →
// dispatchFinalResponse maps 408 → Status=no-answer/Reason=no_answer;
// recordFailure emits "no-answer" terminal event.
func TestForwarder_DialTerminalEvent_NoAnswer(t *testing.T) {
	t.Parallel()
	srv, captured, mu := newCaptureServer(t)
	defer srv.Close()

	factory := &stubDialFactory{returnedErr: nonSuccessErr(408, "Request Timeout")}
	f := newStatusCbForwarder(t, factory, srv, "ACtest0123456789abcdef0123456789ab")

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	parentSid := "CAcaller000000000000000000000000000"
	opts := DialOpts{
		CallerID:             "+4930",
		CallerFrom:           "+4915123ani",
		StatusCallback:       srv.URL + "/cb",
		StatusCallbackEvents: []string{"initiated", "no-answer"},
	}
	res, _ := f.Dial(context.Background(), parentSid, "+4915123", opts, leg)
	if res.Status != "no-answer" {
		t.Fatalf("res.Status = %q, want no-answer", res.Status)
	}

	drainServer(t, mu, captured, 2)
	settleNoExtra(t, mu, captured, 2)

	terminal := findCallback(captured, mu, "no-answer")
	assertTerminalCallback(t, terminal, "no-answer", parentSid)

	if got := legSeqsCount(f); got != 0 {
		t.Errorf("legSeqs entries after no-answer terminal = %d, want 0", got)
	}
}

// TestForwarder_DialTerminalEvent_Failed — callee returns 503 →
// dispatchFinalResponse maps 5xx → Status=failed/Reason=trunk_5xx;
// recordFailure emits "failed" terminal event.
func TestForwarder_DialTerminalEvent_Failed(t *testing.T) {
	t.Parallel()
	srv, captured, mu := newCaptureServer(t)
	defer srv.Close()

	factory := &stubDialFactory{returnedErr: nonSuccessErr(503, "Service Unavailable")}
	f := newStatusCbForwarder(t, factory, srv, "ACtest0123456789abcdef0123456789ab")

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	parentSid := "CAcaller000000000000000000000000000"
	opts := DialOpts{
		CallerID:             "+4930",
		CallerFrom:           "+4915123ani",
		StatusCallback:       srv.URL + "/cb",
		StatusCallbackEvents: []string{"initiated", "failed"},
	}
	res, _ := f.Dial(context.Background(), parentSid, "+4915123", opts, leg)
	if res.Status != "failed" {
		t.Fatalf("res.Status = %q, want failed", res.Status)
	}
	if res.Reason != "trunk_5xx" {
		t.Fatalf("res.Reason = %q, want trunk_5xx", res.Reason)
	}

	drainServer(t, mu, captured, 2)
	settleNoExtra(t, mu, captured, 2)

	terminal := findCallback(captured, mu, "failed")
	assertTerminalCallback(t, terminal, "failed", parentSid)

	if got := legSeqsCount(f); got != 0 {
		t.Errorf("legSeqs entries after failed terminal = %d, want 0", got)
	}
}

// TestForwarder_DialTerminalEvent_Canceled — caller cancels ctx before the
// dial returns → context.Canceled → Status=canceled/Reason=canceled;
// recordFailure emits "canceled" terminal event.
func TestForwarder_DialTerminalEvent_Canceled(t *testing.T) {
	t.Parallel()
	srv, captured, mu := newCaptureServer(t)
	defer srv.Close()

	// hangForever: the stub blocks until ctx.Done(); we cancel right after
	// scheduling Dial — the inner branch returns ctx.Err() == context.Canceled
	// (NOT DeadlineExceeded — that's the no-answer ring-timeout path).
	factory := &stubDialFactory{hangForever: true}
	f := newStatusCbForwarder(t, factory, srv, "ACtest0123456789abcdef0123456789ab")

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	parentSid := "CAcaller000000000000000000000000000"
	opts := DialOpts{
		CallerID:             "+4930",
		CallerFrom:           "+4915123ani",
		StatusCallback:       srv.URL + "/cb",
		StatusCallbackEvents: []string{"initiated", "canceled"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Schedule the cancel so the Dial goroutine's ctx-blocked stub picks
	// up context.Canceled rather than DeadlineExceeded.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	res, _ := f.Dial(ctx, parentSid, "+4915123", opts, leg)
	if res.Status != "canceled" {
		t.Fatalf("res.Status = %q, want canceled", res.Status)
	}

	drainServer(t, mu, captured, 2)
	settleNoExtra(t, mu, captured, 2)

	terminal := findCallback(captured, mu, "canceled")
	assertTerminalCallback(t, terminal, "canceled", parentSid)

	if got := legSeqsCount(f); got != 0 {
		t.Errorf("legSeqs entries after canceled terminal = %d, want 0", got)
	}
}

// TestForwarder_DialTerminalEvent_NoSubscription — customer subscribed to
// initiated/ringing/answered ONLY (no terminal events). The terminal-emit
// chokepoint MUST honour the subscription filter — zero terminal POSTs
// for unsubscribed customers (just like the lifecycle events). Defends the
// per-customer cardinality contract.
func TestForwarder_DialTerminalEvent_NoSubscription(t *testing.T) {
	t.Parallel()
	srv, captured, mu := newCaptureServer(t)
	defer srv.Close()

	stubClient := newStubDialClient(successResp([]byte("v=0\r\nm=audio 5000 RTP/AVP 0\r\n")))
	factory := &stubDialFactory{returnedClient: stubClient}
	f := newStatusCbForwarder(t, factory, srv, "ACtest0123456789abcdef0123456789ab")

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	parentSid := "CAcaller000000000000000000000000000"
	opts := DialOpts{
		CallerID:             "+4930",
		CallerFrom:           "+4915123ani",
		StatusCallback:       srv.URL + "/cb",
		StatusCallbackEvents: []string{"initiated", "answered"}, // no terminals
	}
	if _, err := f.Dial(context.Background(), parentSid, "+4915123", opts, leg); err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Expect EXACTLY TWO POSTs (initiated + answered) — no completed
	// because the customer didn't subscribe.
	drainServer(t, mu, captured, 2)
	settleNoExtra(t, mu, captured, 2)

	if got := findCallback(captured, mu, "completed"); got != nil {
		t.Errorf("found completed callback despite no subscription: %v", got)
	}

	// legSeqs cleanup MUST still happen at terminal-dispatch even when no
	// terminal POST is emitted (the SubscriptionMatches=false branch in
	// emitDialEvent short-circuits before allocating a counter for the
	// terminal event, but Release runs unconditionally afterwards).
	if got := legSeqsCount(f); got != 0 {
		t.Errorf("legSeqs entries after no-subscription dial = %d, want 0", got)
	}
}

// TestForwarder_DialTerminalEvent_SequenceMonotonic — verifies the per-leg
// SequenceNumber counter is monotonic across the four event lifecycle
// (initiated=0, ringing=1, answered=2, completed=3). Defends the
// "independent counter space" invariant from the must_haves block.
func TestForwarder_DialTerminalEvent_SequenceMonotonic(t *testing.T) {
	t.Parallel()
	srv, captured, mu := newCaptureServer(t)
	defer srv.Close()

	stubClient := newStubDialClient(successResp([]byte("v=0\r\nm=audio 5000 RTP/AVP 0\r\n")))
	provisional := []*siplib.Response{siplib.NewResponse(180, "Ringing")}
	base := &stubDialFactory{returnedClient: stubClient}
	factory := &stubDialFactory180{stubDialFactory: base, provisional: provisional}
	f := newStatusCbForwarder(t, factory, srv, "ACtest0123456789abcdef0123456789ab")

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	parentSid := "CAcaller000000000000000000000000000"
	opts := DialOpts{
		CallerID:             "+4930",
		CallerFrom:           "+4915123ani",
		StatusCallback:       srv.URL + "/cb",
		StatusCallbackEvents: []string{"initiated", "ringing", "answered", "completed"},
	}
	if _, err := f.Dial(context.Background(), parentSid, "+4915123", opts, leg); err != nil {
		t.Fatalf("dial: %v", err)
	}
	drainServer(t, mu, captured, 4)
	settleNoExtra(t, mu, captured, 4)

	mu.Lock()
	got := sortBySeq(*captured)
	mu.Unlock()

	wantPairs := []struct {
		callStatus string
		seq        string
	}{
		{"queued", "0"},      // initiated
		{"ringing", "1"},     // ringing
		{"in-progress", "2"}, // answered
		{"completed", "3"},   // terminal completed
	}
	for i, w := range wantPairs {
		if got[i].Form.Get("CallStatus") != w.callStatus {
			t.Errorf("[%d] CallStatus = %q, want %q", i, got[i].Form.Get("CallStatus"), w.callStatus)
		}
		if got[i].Form.Get("SequenceNumber") != w.seq {
			t.Errorf("[%d] SequenceNumber = %q, want %q", i, got[i].Form.Get("SequenceNumber"), w.seq)
		}
	}
}

// TestForwarder_DialTerminalEvent_HighChurn_DialLegsReturnToZero exercises
// the high-churn invariant with terminal-emit + cleanup wired at
// recordFailure / success-path tail. Drives 100 sequential dials through
// complete lifecycles (initiated → ringing → answered → completed terminal)
// and asserts legSeqs returns to 0 between every iteration AND after the
// loop. Mirrors TestForwarder_Dial_HighChurn_LegSeqsReturnsToZero with the
// new completed-terminal expectation.
func TestForwarder_DialTerminalEvent_HighChurn_DialLegsReturnToZero(t *testing.T) {
	t.Parallel()
	srv, captured, mu := newCaptureServer(t)
	defer srv.Close()

	cfg := statusCbTestCfg()
	cfg.DialMaxPerSession = 10000
	cfg.DialMaxPerMinute = 10000
	stubClient := newStubDialClient(successResp([]byte("v=0\r\nm=audio 5000 RTP/AVP 0\r\n")))
	factory := &stubDialFactory{returnedClient: stubClient}
	metrics := observability.NewMetrics()
	wc := webhook.NewStatusClientForTest(srv.Client().Transport, "12345abcdef", metrics, zerolog.Nop())
	f := &Forwarder{
		agent:      nil,
		guardrails: NewGuardrails(cfg),
		cfg:        cfg,
		metrics:    metrics,
		log:        zerolog.Nop(),
		factory:    factory,
		statusWC:   wc,
		accountSid: "ACtest0123456789abcdef0123456789ab",
	}

	leg := &stubLegConfigurer{offerSDP: []byte("v=0"), offerPort: 10000}
	opts := DialOpts{
		CallerID:             "+4930",
		CallerFrom:           "+4915123ani",
		StatusCallback:       srv.URL + "/cb",
		StatusCallbackEvents: []string{"initiated", "answered", "completed"},
	}

	const N = 100
	for i := 0; i < N; i++ {
		callerSid := fmt.Sprintf("CAcaller%026d", i)
		if _, err := f.Dial(context.Background(), callerSid, "+4915123", opts, leg); err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		// Per-iteration assertion: explicit Release at success-path
		// terminal-dispatch + defer (idempotent) → legSeqs is empty.
		if got := legSeqsCount(f); got != 0 {
			t.Fatalf("legSeqs entries after dial #%d = %d, want 0", i, got)
		}
	}
	// Each iteration produces 3 callbacks (initiated/answered/completed) — N*3 total.
	drainServer(t, mu, captured, 3*N)

	if got := legSeqsCount(f); got != 0 {
		t.Fatalf("legSeqs entries after %d Dial() lifecycles = %d, want 0", N, got)
	}
}
