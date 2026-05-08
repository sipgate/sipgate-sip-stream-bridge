package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	siplib "github.com/emiago/sipgo/sip"
	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/bridge"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/config"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/observability"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/sip"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/webhook"
)

// TestMain forces synchronous dispatch for the api test suite. Production
// modifyCallHandler returns the response immediately and dispatches in a
// goroutine (Twilio behaviour) — but tests assert on post-dispatch state in
// the response body, which only works if dispatch ran before writeCallJSON.
// SetAsyncDispatch(false) flips the runtime path for the duration of the
// test binary.
func TestMain(m *testing.M) {
	SetAsyncDispatch(false)
	os.Exit(m.Run())
}

const (
	callsTestSid       = "ACdeadbeefdeadbeefdeadbeefdeadbeef"
	callsTestAuthToken = "test-token-32-chars-long-padding"
)

func callsBasicAuth() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(callsTestSid+":"+callsTestAuthToken))
}

// makeBridgeCalls constructs n stub bridge.BridgeCall fixtures with deterministic
// sids "CA00000000000000000000000000000XXX" and StartTime offsets so the
// list-handler ordering remains reproducible.
func makeBridgeCalls(n int) []bridge.BridgeCall {
	out := make([]bridge.BridgeCall, n)
	base := time.Date(2026, time.April, 27, 10, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		// Sids are 32 hex chars after "CA" — pad index to 32 zeros.
		sid := fmt.Sprintf("CA%032x", i+1)
		out[i] = stubCall{fakeCall{
			callSid:    sid,
			accountSid: callsTestSid,
			from:       "+15551112222",
			to:         "+15553334444",
			direction:  "inbound",
			status:     "in-progress",
			startTime:  base.Add(time.Duration(i) * time.Second),
		}}
	}
	return out
}

// mountCallsRouter builds a chi router with Mount applied and returns it
// ready for httptest invocations. authPrefix lets tests request URLs with a
// different AccountSid than the configured one (path-mismatch coverage lives
// in server_test.go though; this helper is for the configured-SID happy
// path).
//
// The modify-call route requires a webhookFetcher; we pass nilWebhookFetcher
// here because the GET-only tests in this file never exercise the Url= path.
// Tests that DO exercise the modify handler build their own router with a
// fake fetcher inline.
func mountCallsRouter(q BridgeQuerier) http.Handler {
	r := chi.NewRouter()
	m := observability.NewMetrics()
	// nil manager + nil forwarder: GET-only tests never reach the Dial path.
	Mount(r, q, nil, callsTestSid, callsTestAuthToken, m, nilWebhookFetcher{}, testActionPoster(""), zerolog.Nop(), nil, config.Config{})
	return r
}

// nilWebhookFetcher satisfies webhookFetcher with a non-nil interface value
// that always returns ErrEmptyURL. Tests for read-only handlers (list / get)
// never reach the fetch path; modify-call tests inject a real fake.
type nilWebhookFetcher struct{}

func (nilWebhookFetcher) FetchWithFallback(_ context.Context, _, _ webhook.FetchTarget) (*webhook.FetchResult, error) {
	return nil, webhook.ErrEmptyURL
}

func doGET(t *testing.T, r http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", callsBasicAuth())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func decodePage(t *testing.T, rec *httptest.ResponseRecorder) *PageJSON {
	t.Helper()
	var env PageJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode page: %v\nbody=%s", err, rec.Body.String())
	}
	return &env
}

func decodeCall(t *testing.T, rec *httptest.ResponseRecorder) *CallJSON {
	t.Helper()
	var cj CallJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &cj); err != nil {
		t.Fatalf("decode call: %v\nbody=%s", err, rec.Body.String())
	}
	return &cj
}

// TestListCallsHandler_Empty: empty query result → 200 + envelope with
// calls: [], next/previous null.
func TestListCallsHandler_Empty(t *testing.T) {
	t.Parallel()

	q := newStubBridge()
	r := mountCallsRouter(q)
	rec := doGET(t, r, "/2010-04-01/Accounts/"+callsTestSid+"/Calls.json")

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	env := decodePage(t, rec)
	if len(env.Calls) != 0 {
		t.Fatalf("calls: got %d entries, want 0", len(env.Calls))
	}
	if env.NextPageURI != nil {
		t.Fatalf("next_page_uri: got %v, want nil", *env.NextPageURI)
	}
	if env.PreviousPageURI != nil {
		t.Fatalf("previous_page_uri: got %v, want nil", *env.PreviousPageURI)
	}

	// "calls": [] (NOT null) is a Twilio contract — verify the wire bytes.
	if !strings.Contains(rec.Body.String(), `"calls":[]`) {
		t.Fatalf("expected `\"calls\":[]` in body, got: %s", rec.Body.String())
	}
}

// TestListCallsHandler_OnePage: 3 stub calls, default page size → 200 + 3
// calls, next/prev both null.
func TestListCallsHandler_OnePage(t *testing.T) {
	t.Parallel()

	calls := makeBridgeCalls(3)
	q := newStubBridge(calls...)
	r := mountCallsRouter(q)

	rec := doGET(t, r, "/2010-04-01/Accounts/"+callsTestSid+"/Calls.json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	env := decodePage(t, rec)
	if len(env.Calls) != 3 {
		t.Fatalf("calls: got %d, want 3", len(env.Calls))
	}
	if env.NextPageURI != nil {
		t.Fatalf("next_page_uri: got %v, want nil (only one page)", *env.NextPageURI)
	}
	if env.PreviousPageURI != nil {
		t.Fatalf("previous_page_uri: got %v, want nil (page 0)", *env.PreviousPageURI)
	}
}

// TestListCallsHandler_Pagination: 10 calls, ?Page=1&PageSize=3 → start=3,
// end=6, 3 calls, next + prev both non-null.
func TestListCallsHandler_Pagination(t *testing.T) {
	t.Parallel()

	calls := makeBridgeCalls(10)
	q := newStubBridge(calls...)
	r := mountCallsRouter(q)

	rec := doGET(t, r, "/2010-04-01/Accounts/"+callsTestSid+"/Calls.json?Page=1&PageSize=3")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	env := decodePage(t, rec)
	if env.Page != 1 {
		t.Fatalf("page: got %d, want 1", env.Page)
	}
	if env.PageSize != 3 {
		t.Fatalf("page_size: got %d, want 3", env.PageSize)
	}
	if env.Start != 3 {
		t.Fatalf("start: got %d, want 3", env.Start)
	}
	if env.End != 6 {
		t.Fatalf("end: got %d, want 6", env.End)
	}
	if len(env.Calls) != 3 {
		t.Fatalf("calls: got %d, want 3", len(env.Calls))
	}
	if env.NextPageURI == nil {
		t.Fatal("next_page_uri: got nil, want non-nil (still 4 calls left)")
	}
	if env.PreviousPageURI == nil {
		t.Fatal("previous_page_uri: got nil, want non-nil (page 1 has prev)")
	}
}

// TestListCallsHandler_PageSizeOversize: ?PageSize=99999 → clamps to 1000
// (memory-growth guard from SerializePage).
func TestListCallsHandler_PageSizeOversize(t *testing.T) {
	t.Parallel()

	q := newStubBridge()
	r := mountCallsRouter(q)
	rec := doGET(t, r, "/2010-04-01/Accounts/"+callsTestSid+"/Calls.json?PageSize=99999")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	env := decodePage(t, rec)
	if env.PageSize != 1000 {
		t.Fatalf("page_size: got %d, want 1000 (clamp)", env.PageSize)
	}
}

// TestGetCallHandler_Found: stub returns active call → 200 + Twilio JSON.
func TestGetCallHandler_Found(t *testing.T) {
	t.Parallel()

	calls := makeBridgeCalls(1)
	q := newStubBridge(calls...)
	r := mountCallsRouter(q)

	sid := calls[0].CallSid()
	rec := doGET(t, r, "/2010-04-01/Accounts/"+callsTestSid+"/Calls/"+sid+".json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type: got %q, want application/json", got)
	}
	cj := decodeCall(t, rec)
	if cj.Sid != sid {
		t.Fatalf("sid: got %q, want %q", cj.Sid, sid)
	}
}

// TestGetCallHandler_NotFound: stub returns (nil, false) → 404 + 20404.
func TestGetCallHandler_NotFound(t *testing.T) {
	t.Parallel()

	q := newStubBridge()
	r := mountCallsRouter(q)

	missing := "CAffffffffffffffffffffffffffffffff"
	rec := doGET(t, r, "/2010-04-01/Accounts/"+callsTestSid+"/Calls/"+missing+".json")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
	var body Error
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if body.Code != 20404 {
		t.Fatalf("error code: got %d, want 20404", body.Code)
	}
}

// TestGetCallHandler_MalformedCallSid: /Calls/garbage.json → 404 + 20404
// (NOT 400). Twilio behavior: a syntactically invalid Sid is "not found",
// not "bad request" — SDKs special-case 404+20404.
func TestGetCallHandler_MalformedCallSid(t *testing.T) {
	t.Parallel()

	q := newStubBridge()
	r := mountCallsRouter(q)

	rec := doGET(t, r, "/2010-04-01/Accounts/"+callsTestSid+"/Calls/garbage.json")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404 (NOT 400 for malformed CallSid)", rec.Code)
	}
	var body Error
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if body.Code != 20404 {
		t.Fatalf("error code: got %d, want 20404", body.Code)
	}
}

// TestListCallsHandler_JSONShape: response Content-Type "application/json";
// envelope has all required fields.
func TestListCallsHandler_JSONShape(t *testing.T) {
	t.Parallel()

	calls := makeBridgeCalls(2)
	q := newStubBridge(calls...)
	r := mountCallsRouter(q)

	rec := doGET(t, r, "/2010-04-01/Accounts/"+callsTestSid+"/Calls.json")
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type: got %q, want application/json", got)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	required := []string{
		"page", "page_size", "start", "end", "uri", "next_page_uri",
		"previous_page_uri", "first_page_uri", "calls",
	}
	for _, f := range required {
		if _, ok := raw[f]; !ok {
			t.Fatalf("envelope missing required field %q; body=%s", f, rec.Body.String())
		}
	}
}

// TestGetCallHandler_JSONShape: single Call resource has all 14 Twilio
// fields (sid, account_sid, from, to, status, start_time, end_time,
// duration, direction, answered_by, parent_call_sid, api_version, uri,
// subresource_uris).
func TestGetCallHandler_JSONShape(t *testing.T) {
	t.Parallel()

	calls := makeBridgeCalls(1)
	q := newStubBridge(calls...)
	r := mountCallsRouter(q)

	sid := calls[0].CallSid()
	rec := doGET(t, r, "/2010-04-01/Accounts/"+callsTestSid+"/Calls/"+sid+".json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	required := []string{
		"sid", "account_sid", "from", "to", "status",
		"start_time", "end_time", "duration", "direction",
		"answered_by", "parent_call_sid", "api_version",
		"uri", "subresource_uris",
	}
	for _, f := range required {
		if _, ok := raw[f]; !ok {
			t.Fatalf("call resource missing required field %q; body=%s", f, rec.Body.String())
		}
	}
}

// ===== modify-call POST handler tests =====

// fakeWebhookClient satisfies the package-private webhookFetcher interface
// used by modifyCallHandler. result/err are returned verbatim from the next
// FetchWithFallback call. captured records the (primary, fallback)
// arguments for assertion.
type fakeWebhookClient struct {
	result      *webhook.FetchResult
	err         error
	calls       int
	lastPrimary webhook.FetchTarget
	lastFB      webhook.FetchTarget
}

func (f *fakeWebhookClient) FetchWithFallback(_ context.Context, p, fb webhook.FetchTarget) (*webhook.FetchResult, error) {
	f.calls++
	f.lastPrimary = p
	f.lastFB = fb
	return f.result, f.err
}

// modifyTestActiveSid is the CallSid used for an "active" call fixture across
// the modify tests. The bridge.NewTestSession constructor uses it directly
// so the BridgeQuerier returns the *bridge.CallSession verbatim.
const (
	modifyTestActiveSid     = "CA11111111111111111111111111111111"
	modifyTestTerminatedSid = "CA22222222222222222222222222222222"
)

// modifyBridge implements BridgeQuerier with two slots: one active session
// and one terminated session. Either may be nil so individual tests can
// configure exactly the call(s) they need.
type modifyBridge struct {
	active     *bridge.CallSession // returned for modifyTestActiveSid
	terminated bridge.BridgeCall   // returned for modifyTestTerminatedSid (any BridgeCall — typically a fakeCall to simulate a recentlyTerminated snapshot OR a MarkTestTerminated CallSession)
}

func (b *modifyBridge) List() []bridge.BridgeCall { return nil }

func (b *modifyBridge) GetByCallSid(callSid string) (bridge.BridgeCall, bool) {
	switch callSid {
	case modifyTestActiveSid:
		if b.active == nil {
			return nil, false
		}
		return b.active, true
	case modifyTestTerminatedSid:
		if b.terminated == nil {
			return nil, false
		}
		return b.terminated, true
	default:
		return nil, false
	}
}

// mountModifyRouter builds a chi router with Mount applied for the given
// querier + webhook fake. Returns the router + the active CallSession (if
// any) so tests can inspect post-Terminate state.
func mountModifyRouter(t *testing.T, q BridgeQuerier, wc webhookFetcher) (http.Handler, *observability.Metrics) {
	t.Helper()
	r := chi.NewRouter()
	m := observability.NewMetrics()
	// nil manager + nil forwarder: existing Hangup/Status tests don't use Dial path.
	Mount(r, q, nil, callsTestSid, callsTestAuthToken, m, wc, testActionPoster(""), zerolog.Nop(), nil, config.Config{})
	return r, m
}

// doPOST issues a Basic-Auth POST to path with form-encoded body.
func doPOST(t *testing.T, r http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", callsBasicAuth())
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// modifyURL builds the canonical modify-call POST path for the configured
// test AccountSid + a given CallSid.
func modifyURL(callSid string) string {
	return "/2010-04-01/Accounts/" + callsTestSid + "/Calls/" + callSid + ".json"
}

// newActiveCallFixture builds a *bridge.CallSession ready to be modified.
// byeCount captures the number of BYE invocations through the test stub.
func newActiveCallFixture(t *testing.T, byeCount *atomic.Int32) *bridge.CallSession {
	t.Helper()
	return bridge.NewTestSession(modifyTestActiveSid, callsTestSid, func(_ context.Context) error {
		if byeCount != nil {
			byeCount.Add(1)
		}
		return nil
	})
}

// newTerminatedCallFixture builds a *bridge.CallSession that has already
// been terminated (state == StateTerminated, IsActive() == false). Used to
// exercise the "modify-on-terminated returns 21220" branch where the cast
// to *bridge.CallSession succeeds but IsActive returns false.
func newTerminatedCallFixture(t *testing.T) *bridge.CallSession {
	t.Helper()
	s := bridge.NewTestSession(modifyTestTerminatedSid, callsTestSid, func(_ context.Context) error { return nil })
	bridge.MarkTestTerminated(s, "completed")
	return s
}

// snapshotTerminatedFixture builds a NON-CallSession bridge.BridgeCall that
// simulates a recentlyTerminated snapshot (the unexported *terminatedCall
// type from the bridge package). The cast to *bridge.CallSession FAILS for
// this type, hitting the modify-on-terminated branch via the cast-failure
// path rather than the IsActive=false path. Both paths must produce the
// same observable behavior; this fixture covers the cast-failure side.
func snapshotTerminatedFixture(callSid string) bridge.BridgeCall {
	end := time.Date(2026, time.April, 27, 10, 5, 0, 0, time.UTC)
	return stubCall{fakeCall{
		callSid:    callSid,
		accountSid: callsTestSid,
		from:       "+15551112222",
		to:         "+15553334444",
		direction:  "inbound",
		status:     "completed",
		startTime:  time.Date(2026, time.April, 27, 10, 0, 0, 0, time.UTC),
		endTime:    end,
		duration:   300,
	}}
}

// decodeError pulls the Twilio Error JSON out of a recorder body for assertion.
func decodeError(t *testing.T, rec *httptest.ResponseRecorder) Error {
	t.Helper()
	var body Error
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v\nbody=%s", err, rec.Body.String())
	}
	return body
}

// TestModifyCall_StatusCompleted_TerminatesActive: POST Status=completed
// against an active call → 200 + JSON, session.Terminate("completed") fires
// once, IsActive() flips to false.
func TestModifyCall_StatusCompleted_TerminatesActive(t *testing.T) {
	t.Parallel()

	var byeCount atomic.Int32
	active := newActiveCallFixture(t, &byeCount)
	q := &modifyBridge{active: active}
	r, _ := mountModifyRouter(t, q, &fakeWebhookClient{})

	rec := doPOST(t, r, modifyURL(modifyTestActiveSid), "Status=completed")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	cj := decodeCall(t, rec)
	if cj.Sid != modifyTestActiveSid {
		t.Fatalf("response sid: got %q, want %q", cj.Sid, modifyTestActiveSid)
	}
	if cj.Status != "completed" {
		t.Fatalf("response status: got %q, want \"completed\"", cj.Status)
	}
	if active.IsActive() {
		t.Errorf("post-Terminate: IsActive() = true, want false")
	}
	if got := byeCount.Load(); got != 1 {
		t.Errorf("byeCount: got %d, want 1", got)
	}
}

// TestModifyCall_StatusCompleted_Idempotent: POST Status=completed against
// an already-terminated call → 200 + JSON, NO additional Terminate (BYE)
// fires. Twilio behavior: duplicate Status=completed is idempotent.
func TestModifyCall_StatusCompleted_Idempotent(t *testing.T) {
	t.Parallel()

	// Terminated *bridge.CallSession (cast succeeds but IsActive=false).
	terminated := newTerminatedCallFixture(t)
	q := &modifyBridge{terminated: terminated}
	r, _ := mountModifyRouter(t, q, &fakeWebhookClient{})

	rec := doPOST(t, r, modifyURL(modifyTestTerminatedSid), "Status=completed")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (idempotent); body=%s", rec.Code, rec.Body.String())
	}
	cj := decodeCall(t, rec)
	if cj.Sid != modifyTestTerminatedSid {
		t.Fatalf("response sid: got %q, want %q", cj.Sid, modifyTestTerminatedSid)
	}
	// Status field reflects the terminal state — "completed" since
	// MarkTestTerminated stamped that reason.
	if cj.Status != "completed" {
		t.Fatalf("response status: got %q, want \"completed\"", cj.Status)
	}

	// Also exercise the snapshot-shaped fixture (cast to *bridge.CallSession fails).
	q2 := &modifyBridge{terminated: snapshotTerminatedFixture(modifyTestTerminatedSid)}
	r2, _ := mountModifyRouter(t, q2, &fakeWebhookClient{})
	rec2 := doPOST(t, r2, modifyURL(modifyTestTerminatedSid), "Status=completed")
	if rec2.Code != http.StatusOK {
		t.Fatalf("snapshot fixture: status got %d, want 200; body=%s", rec2.Code, rec2.Body.String())
	}
}

// TestModifyCall_TwimlHangup_Terminates: POST Twiml=<Response><Hangup/></Response>
// against an active call → 200 + status="completed", Terminate called with
// reason="hangup". The internal reason "hangup" projects to the Twilio CallStatus
// enum "completed" on the wire — Twilio's status field is bounded to {queued,
// ringing, in-progress, completed, busy, failed, no-answer, canceled}.
func TestModifyCall_TwimlHangup_Terminates(t *testing.T) {
	t.Parallel()

	var byeCount atomic.Int32
	active := newActiveCallFixture(t, &byeCount)
	q := &modifyBridge{active: active}
	r, _ := mountModifyRouter(t, q, &fakeWebhookClient{})

	form := "Twiml=" + url.QueryEscape("<Response><Hangup/></Response>")
	rec := doPOST(t, r, modifyURL(modifyTestActiveSid), form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	cj := decodeCall(t, rec)
	if cj.Status != "completed" {
		t.Fatalf("response status: got %q, want \"completed\" (Twilio enum mapping for reason=hangup)", cj.Status)
	}
	if active.IsActive() {
		t.Errorf("post-Hangup: IsActive() = true, want false")
	}
	if got := byeCount.Load(); got != 1 {
		t.Errorf("byeCount: got %d, want 1 (Hangup verb must fire BYE once)", got)
	}
}

// TestModifyCall_TwimlOversized_400_12100: POST Twiml=<Response> + 4001
// chars → 400 + 12100. Length cap enforced before parse so a syntactically-
// valid but too-long body still trips the cap.
func TestModifyCall_TwimlOversized_400_12100(t *testing.T) {
	t.Parallel()

	active := newActiveCallFixture(t, nil)
	q := &modifyBridge{active: active}
	r, _ := mountModifyRouter(t, q, &fakeWebhookClient{})

	// Construct a >4000-char Twiml body. Padding is ASCII so URL-encoding
	// doesn't bloat the form key/value boundary into ambiguity.
	pad := strings.Repeat("a", twimlMaxLen+1) // 4001 chars
	form := "Twiml=" + url.QueryEscape(pad)
	rec := doPOST(t, r, modifyURL(modifyTestActiveSid), form)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	body := decodeError(t, rec)
	if body.Code != 12100 {
		t.Fatalf("error code: got %d, want 12100", body.Code)
	}
	if active.IsActive() != true {
		t.Errorf("oversized Twiml must NOT terminate the call")
	}
}

// TestModifyCall_TwimlAndStatus_400_21218: POST with both Twiml=... AND
// Status=completed → 400 + 21218 (at-most-one-of violation).
func TestModifyCall_TwimlAndStatus_400_21218(t *testing.T) {
	t.Parallel()

	active := newActiveCallFixture(t, nil)
	q := &modifyBridge{active: active}
	r, _ := mountModifyRouter(t, q, &fakeWebhookClient{})

	form := "Twiml=" + url.QueryEscape("<Response><Hangup/></Response>") + "&Status=completed"
	rec := doPOST(t, r, modifyURL(modifyTestActiveSid), form)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	body := decodeError(t, rec)
	if body.Code != 21218 {
		t.Fatalf("error code: got %d, want 21218", body.Code)
	}
}

// TestModifyCall_NoParams_400_21218: POST with empty body (no Twiml/Url/
// Status) → 400 + 21218 (at-least-one-of violation).
func TestModifyCall_NoParams_400_21218(t *testing.T) {
	t.Parallel()

	active := newActiveCallFixture(t, nil)
	q := &modifyBridge{active: active}
	r, _ := mountModifyRouter(t, q, &fakeWebhookClient{})

	rec := doPOST(t, r, modifyURL(modifyTestActiveSid), "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	body := decodeError(t, rec)
	if body.Code != 21218 {
		t.Fatalf("error code: got %d, want 21218", body.Code)
	}
}

// TestModifyCall_UnknownCallSid_404: POST against a CallSid that does not
// match any active or terminated call → 404 + 20404. Must NOT leak existence
// via 401-vs-404 path (auth happens first; this test sits AFTER auth via
// valid Basic Auth header).
func TestModifyCall_UnknownCallSid_404(t *testing.T) {
	t.Parallel()

	q := &modifyBridge{}
	r, _ := mountModifyRouter(t, q, &fakeWebhookClient{})

	missing := "CAffffffffffffffffffffffffffffffff"
	rec := doPOST(t, r, modifyURL(missing), "Status=completed")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	body := decodeError(t, rec)
	if body.Code != 20404 {
		t.Fatalf("error code: got %d, want 20404", body.Code)
	}
}

// TestModifyCall_MalformedCallSid_404: POST against /Calls/garbage.json →
// 404 + 20404 (NOT 400). Twilio behavior: malformed Sid is "not found", not
// "bad request" — SDKs special-case 404+20404.
func TestModifyCall_MalformedCallSid_404(t *testing.T) {
	t.Parallel()

	q := &modifyBridge{}
	r, _ := mountModifyRouter(t, q, &fakeWebhookClient{})

	rec := doPOST(t, r, modifyURL("garbage"), "Status=completed")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404 (NOT 400 for malformed CallSid); body=%s", rec.Code, rec.Body.String())
	}
	body := decodeError(t, rec)
	if body.Code != 20404 {
		t.Fatalf("error code: got %d, want 20404", body.Code)
	}
}

// TestModifyCall_TerminatedNonStatus_400_21220: POST Twiml=... against an
// already-terminated call → 400 + 21220 (modify-on-not-active for non-Status
// branches). Idempotence applies ONLY to Status=completed.
func TestModifyCall_TerminatedNonStatus_400_21220(t *testing.T) {
	t.Parallel()

	terminated := newTerminatedCallFixture(t)
	q := &modifyBridge{terminated: terminated}
	r, _ := mountModifyRouter(t, q, &fakeWebhookClient{})

	form := "Twiml=" + url.QueryEscape("<Response><Hangup/></Response>")
	rec := doPOST(t, r, modifyURL(modifyTestTerminatedSid), form)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	body := decodeError(t, rec)
	if body.Code != 21220 {
		t.Fatalf("error code: got %d, want 21220 (call not in-progress)", body.Code)
	}

	// Snapshot fixture (cast to *CallSession fails) — same outcome.
	q2 := &modifyBridge{terminated: snapshotTerminatedFixture(modifyTestTerminatedSid)}
	r2, _ := mountModifyRouter(t, q2, &fakeWebhookClient{})
	rec2 := doPOST(t, r2, modifyURL(modifyTestTerminatedSid), form)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("snapshot fixture: status got %d, want 400", rec2.Code)
	}
	body2 := decodeError(t, rec2)
	if body2.Code != 21220 {
		t.Fatalf("snapshot fixture: error code got %d, want 21220", body2.Code)
	}
}

// TestModifyCall_UnknownStatusValue_400: POST Status=canceled (an
// unsupported value) → 400 + 21218. Only Status=completed is honored.
func TestModifyCall_UnknownStatusValue_400(t *testing.T) {
	t.Parallel()

	active := newActiveCallFixture(t, nil)
	q := &modifyBridge{active: active}
	r, _ := mountModifyRouter(t, q, &fakeWebhookClient{})

	rec := doPOST(t, r, modifyURL(modifyTestActiveSid), "Status=canceled")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	body := decodeError(t, rec)
	if body.Code != 21218 {
		t.Fatalf("error code: got %d, want 21218 (unsupported Status value)", body.Code)
	}
	if !active.IsActive() {
		t.Errorf("rejected Status value must NOT terminate the call")
	}
}

// TestModifyCall_UrlFetchSucceeds_DispatchesHangup: stub webhook.Client
// returns <Response><Hangup/></Response> → 200 + status="completed"
// (Twilio enum), Terminate called once with reason="hangup".
func TestModifyCall_UrlFetchSucceeds_DispatchesHangup(t *testing.T) {
	t.Parallel()

	var byeCount atomic.Int32
	active := newActiveCallFixture(t, &byeCount)
	q := &modifyBridge{active: active}

	wc := &fakeWebhookClient{
		result: &webhook.FetchResult{
			Body:       []byte("<Response><Hangup/></Response>"),
			StatusCode: http.StatusOK,
			URLUsed:    "https://test.example/twiml.xml",
			Attempts:   1,
		},
	}
	r, _ := mountModifyRouter(t, q, wc)

	form := "Url=" + url.QueryEscape("https://test.example/twiml.xml")
	rec := doPOST(t, r, modifyURL(modifyTestActiveSid), form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	cj := decodeCall(t, rec)
	if cj.Status != "completed" {
		t.Fatalf("response status: got %q, want \"completed\" (Twilio enum mapping for reason=hangup)", cj.Status)
	}
	if got := byeCount.Load(); got != 1 {
		t.Errorf("byeCount: got %d, want 1", got)
	}
	if wc.calls != 1 {
		t.Errorf("FetchWithFallback calls: got %d, want 1", wc.calls)
	}
	if wc.lastPrimary.URL != "https://test.example/twiml.xml" {
		t.Errorf("primary URL: got %q, want https://test.example/twiml.xml", wc.lastPrimary.URL)
	}
	if wc.lastPrimary.Method != http.MethodPost {
		t.Errorf("default method: got %q, want POST", wc.lastPrimary.Method)
	}
}

// TestModifyCall_UrlFetchFails_400_11200: stub returns ErrAllAttemptsFailed
// → 400 + 11200 (HTTP retrieval failure).
func TestModifyCall_UrlFetchFails_400_11200(t *testing.T) {
	t.Parallel()

	active := newActiveCallFixture(t, nil)
	q := &modifyBridge{active: active}

	wc := &fakeWebhookClient{
		err: webhook.ErrAllAttemptsFailed,
	}
	r, _ := mountModifyRouter(t, q, wc)

	form := "Url=" + url.QueryEscape("https://unreachable.example/twiml.xml")
	rec := doPOST(t, r, modifyURL(modifyTestActiveSid), form)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	body := decodeError(t, rec)
	if body.Code != 11200 {
		t.Fatalf("error code: got %d, want 11200", body.Code)
	}
	if !active.IsActive() {
		t.Errorf("Url-fetch failure must NOT terminate the call")
	}
}

// TestModifyCall_TwimlMalformed_400_12100: POST Twiml=garbage<no-root> →
// 400 + 12100 (Document parse failure).
func TestModifyCall_TwimlMalformed_400_12100(t *testing.T) {
	t.Parallel()

	active := newActiveCallFixture(t, nil)
	q := &modifyBridge{active: active}
	r, _ := mountModifyRouter(t, q, &fakeWebhookClient{})

	form := "Twiml=" + url.QueryEscape("<NotResponse><Hangup/></NotResponse>")
	rec := doPOST(t, r, modifyURL(modifyTestActiveSid), form)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	body := decodeError(t, rec)
	if body.Code != 12100 {
		t.Fatalf("error code: got %d, want 12100", body.Code)
	}
	if !active.IsActive() {
		t.Errorf("malformed Twiml must NOT terminate the call")
	}
}

// TestModifyCall_TwimlUnknownVerbsWarnAndSkip_200: POST Twiml=<Response>
// <Say>hi</Say><Hangup/></Response> → 200 + status="completed" (Twilio enum
// mapping for reason=hangup). Say is retained as unknownVerb by the parser
// and warned-and-skipped by the dispatcher; Hangup runs and terminates the call.
func TestModifyCall_TwimlUnknownVerbsWarnAndSkip_200(t *testing.T) {
	t.Parallel()

	var byeCount atomic.Int32
	active := newActiveCallFixture(t, &byeCount)
	q := &modifyBridge{active: active}
	r, _ := mountModifyRouter(t, q, &fakeWebhookClient{})

	form := "Twiml=" + url.QueryEscape("<Response><Say>hi</Say><Hangup/></Response>")
	rec := doPOST(t, r, modifyURL(modifyTestActiveSid), form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	cj := decodeCall(t, rec)
	if cj.Status != "completed" {
		t.Fatalf("response status: got %q, want \"completed\" (Twilio enum mapping for reason=hangup)", cj.Status)
	}
	if got := byeCount.Load(); got != 1 {
		t.Errorf("byeCount: got %d, want 1 (Hangup must fire even when preceded by unknown verbs)", got)
	}
	if active.IsActive() {
		t.Errorf("post-dispatch: IsActive() = true, want false")
	}
}

// TestModifyCall_MetricsCounters: cumulative metric assertion across
// success + failure paths to ensure the bounded twiml_modify_total counters
// are wired correctly.
func TestModifyCall_MetricsCounters(t *testing.T) {
	t.Parallel()

	active := newActiveCallFixture(t, nil)
	q := &modifyBridge{active: active}
	r, m := mountModifyRouter(t, q, &fakeWebhookClient{})

	// invalid_params (no body)
	_ = doPOST(t, r, modifyURL(modifyTestActiveSid), "")
	if got := readCounter(t, m.TwimlModifyTotal.WithLabelValues("twiml", "invalid_params")); got != 1 {
		t.Errorf("twiml/invalid_params counter: got %v, want 1", got)
	}

	// status_completed/terminated
	_ = doPOST(t, r, modifyURL(modifyTestActiveSid), "Status=completed")
	if got := readCounter(t, m.TwimlModifyTotal.WithLabelValues("status_completed", "terminated")); got != 1 {
		t.Errorf("status_completed/terminated counter: got %v, want 1", got)
	}
}

// ───────────────────────────────────────────────────────────────────────────
// Twiml=<Dial> end-to-end tests
// ───────────────────────────────────────────────────────────────────────────

// callsTestDialFactory implements sip.DialClientFactory with the correct
// siplib.Uri / *siplib.Response concrete types. Preset returnedErr and
// returnedClient control the Dial outcome.
//
// The factory is in the api test package (not sip) so it can be wired into
// sip.NewForwarderWithFactory without needing the sip package internals.
type callsTestDialFactory struct {
	returnedClient sip.DialClient
	returnedErr    error
	calls          atomic.Int32
}

func (f *callsTestDialFactory) Dial(
	_ context.Context,
	_ siplib.Uri,
	_ siplib.Uri,
	_ string,
	_ *siplib.Uri,
	_ []byte,
	_ sip.DialAuth,
	_ func(*siplib.Response) error,
) (sip.DialClient, error) {
	f.calls.Add(1)
	return f.returnedClient, f.returnedErr
}

func (f *callsTestDialFactory) ReadBye(_ *siplib.Request, _ siplib.ServerTransaction) error {
	return nil
}

// callsTestDialClient simulates an immediately-confirmed + immediately-ended
// confirmed dialog. FinalResponse must return a valid *siplib.Response with a
// valid SDP body for AcceptSDPAnswer to succeed. We return nil here so the
// forwarder takes the "resp == nil" failed path — that's fine for tests that
// only care about the Terminate reason mapping.
type callsTestDialClient struct {
	done chan struct{}
}

func newCallsTestDialClient() *callsTestDialClient {
	ch := make(chan struct{})
	close(ch)
	return &callsTestDialClient{done: ch}
}

func (c *callsTestDialClient) FinalResponse() *siplib.Response { return nil }
func (c *callsTestDialClient) Ack(_ context.Context) error    { return nil }
func (c *callsTestDialClient) Bye(_ context.Context) error    { return nil }
func (c *callsTestDialClient) Done() <-chan struct{}           { return c.done }
func (c *callsTestDialClient) Close() error                   { return nil }

// nonSuccessDialErr wraps a non-2xx status as sipgo.ErrDialogResponse,
// mimicking what sipgo's WaitAnswer surfaces for 4xx/5xx responses.
func nonSuccessDialErr(code int, reason string) error {
	resp := siplib.NewResponse(code, reason)
	return &sipgo.ErrDialogResponse{Res: resp}
}

// dialTestConfig returns a config with +49 allow-list and generous rate limits
// that prevent unintended guardrails rejection in Dial tests.
func dialTestConfig() config.Config {
	return config.Config{
		SDPContactIP:        "127.0.0.1",
		SIPDomain:           "test.example",
		SIPUser:             "testuser",
		DialAllowedPrefixes: []string{"+49"},
		DialMaxPerSession:   10,
		DialMaxPerMinute:    100,
		DialRingTimeoutS:    30,
		DialDefaultCallerID: "+4930000000",
	}
}

// dialTestActiveSid is a dedicated CallSid for Dial path tests; uses a
// separate constant from modifyTestActiveSid to avoid cross-test interference.
const dialTestActiveSid = "CA33333333333333333333333333333333"

// mountDialRouter builds a chi router with a real PortPool + CallManager +
// stub Forwarder backed by factory. Returns the router, the active session,
// and the factory so tests can inspect Dial call counts.
func mountDialRouter(
	t *testing.T,
	factory sip.DialClientFactory,
	cfg config.Config,
	wc webhookFetcher,
	byeCount *atomic.Int32,
) (http.Handler, *bridge.CallSession, *callsTestDialFactory) {
	t.Helper()

	session := bridge.NewTestSession(dialTestActiveSid, callsTestSid, func(_ context.Context) error {
		if byeCount != nil {
			byeCount.Add(1)
		}
		return nil
	})

	pool, err := bridge.NewPortPool(21200, 21210)
	if err != nil {
		t.Fatalf("NewPortPool: %v", err)
	}
	manager := bridge.NewCallManager(pool, callsTestSid, config.Config{SDPContactIP: "127.0.0.1"}, zerolog.Nop(), nil)
	t.Cleanup(manager.Close)

	metrics := observability.NewMetrics()
	guardrails := sip.NewGuardrails(cfg)

	// If no factory was given, use a default that returns nil FinalResponse
	// (causing Forwarder to take the "resp==nil" failed path — acceptable for
	// tests that only care about dispatching, not about the outcome status).
	var tracked *callsTestDialFactory
	if factory == nil {
		tracked = &callsTestDialFactory{returnedClient: newCallsTestDialClient()}
		factory = tracked
	} else {
		// If the caller passed a non-callsTestDialFactory (e.g. privacyGateTestFactory)
		// the type assertion yields a nil tracked, which the call-count
		// helpers treat as "not tracked".
		tracked, _ = factory.(*callsTestDialFactory)
	}

	forwarder := sip.NewForwarderWithFactory(nil, guardrails, cfg, metrics, zerolog.Nop(), factory)

	// Use a single-entry map-backed BridgeQuerier so GetByCallSid returns
	// the session for dialTestActiveSid (not modifyTestActiveSid which
	// modifyBridge uses).
	q := newStubBridge(session)
	r := chi.NewRouter()
	// Dial-router fixtures assert action-callback delivery via the
	// adapterStubWebhookFetcher fake (wc.calls / wc.Body() / wc.URL()).
	// Pass nil actionPoster so fireActionCallback takes the legacy
	// webhookC fallback path — these tests don't exercise X-Twilio-Signature
	// (the dedicated TestFireActionCallback_SignsWithXTwilioSignature does).
	Mount(r, q, manager, callsTestSid, callsTestAuthToken, metrics, wc, nil, zerolog.Nop(), forwarder, cfg)
	return r, session, tracked
}

// ── Test 1: basic Dial dispatch ───────────────────────────────────────────────

// TestModifyCall_TwimlDial_DispatchesForward: POST
// Twiml=<Response><Dial>+4912345</Dial></Response> against an active call
// dispatches through the Dial handler, fires PrepareDial + PerformDial, and
// terminates the call. Response is 200 with a terminal status.
func TestModifyCall_TwimlDial_DispatchesForward(t *testing.T) {
	t.Parallel()

	var byeCount atomic.Int32
	// nil final response → forwarder returns "failed" (resp==nil guard).
	// dial handler receives error → Terminate("failed").
	r, session, factory := mountDialRouter(t, nil, dialTestConfig(), &adapterStubWebhookFetcher{}, &byeCount)

	form := "Twiml=" + url.QueryEscape("<Response><Dial>+4912345</Dial></Response>")
	rec := doPOST(t, r, "/2010-04-01/Accounts/"+callsTestSid+"/Calls/"+dialTestActiveSid+".json", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// factory.Dial must have been called once (after guardrails pass).
	if got := factory.calls.Load(); got != 1 {
		t.Errorf("factory.Dial calls: got %d, want 1", got)
	}

	// Session must be terminated.
	if session.IsActive() {
		t.Errorf("session still active after Dial — Terminate not called")
	}

	cj := decodeCall(t, rec)
	// Status must be a terminal Twilio status.
	terminal := map[string]bool{"completed": true, "failed": true, "busy": true, "no-answer": true, "canceled": true}
	if !terminal[cj.Status] {
		t.Errorf("response status %q is not a terminal Twilio status", cj.Status)
	}
}

// ── Test 2: <Number> child resolves ──────────────────────────────────────────

// TestModifyCall_TwimlDial_NumberChild: POST Twiml with <Dial><Number>
// resolves the same dial target as bare text.
func TestModifyCall_TwimlDial_NumberChild(t *testing.T) {
	t.Parallel()

	r, session, factory := mountDialRouter(t, nil, dialTestConfig(), &adapterStubWebhookFetcher{}, nil)

	form := "Twiml=" + url.QueryEscape("<Response><Dial><Number>+4912345</Number></Dial></Response>")
	rec := doPOST(t, r, "/2010-04-01/Accounts/"+callsTestSid+"/Calls/"+dialTestActiveSid+".json", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := factory.calls.Load(); got != 1 {
		t.Errorf("factory.Dial calls: got %d, want 1 (Number child must resolve dial target)", got)
	}
	if session.IsActive() {
		t.Errorf("session still active after Dial — Terminate not called")
	}
}

// ── Test 3: toll-fraud blocked ────────────────────────────────────────────────

// TestModifyCall_TwimlDial_TollFraudBlocked: dialing a target that does NOT
// match the allow-list triggers ErrTollFraudBlocked in Forwarder.Dial before
// any INVITE. The adapter propagates the error; dialHandler calls
// Terminate("failed"); response status = "failed".
func TestModifyCall_TwimlDial_TollFraudBlocked(t *testing.T) {
	t.Parallel()

	// Allow-list rejects "+49..." (only "+1" is allowed).
	cfg := dialTestConfig()
	cfg.DialAllowedPrefixes = []string{"+1"}

	r, session, factory := mountDialRouter(t, nil, cfg, &adapterStubWebhookFetcher{}, nil)

	form := "Twiml=" + url.QueryEscape("<Response><Dial>+4912345</Dial></Response>")
	rec := doPOST(t, r, "/2010-04-01/Accounts/"+callsTestSid+"/Calls/"+dialTestActiveSid+".json", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// factory.Dial must NOT have been called (guardrails fire before INVITE).
	if got := factory.calls.Load(); got != 0 {
		t.Errorf("factory.Dial calls: got %d, want 0 (guardrails must short-circuit INVITE)", got)
	}

	// Session should be terminated (dialHandler's Terminate path).
	if session.IsActive() {
		t.Errorf("session still active after toll-fraud block — Terminate not called")
	}

	cj := decodeCall(t, rec)
	if cj.Status != "failed" {
		t.Errorf("response status: got %q, want \"failed\"", cj.Status)
	}
}

// ── Test 4: rate limit hit ────────────────────────────────────────────────────

// TestModifyCall_TwimlDial_RateLimitHit: DialMaxPerSession=0 causes
// ErrSessionRateLimit on the first dial attempt. Result: Terminate("failed"),
// response status = "failed".
func TestModifyCall_TwimlDial_RateLimitHit(t *testing.T) {
	t.Parallel()

	cfg := dialTestConfig()
	cfg.DialMaxPerSession = 0 // 0 → any attempt exceeds limit (1 > 0)

	r, session, factory := mountDialRouter(t, nil, cfg, &adapterStubWebhookFetcher{}, nil)

	form := "Twiml=" + url.QueryEscape("<Response><Dial>+4912345</Dial></Response>")
	rec := doPOST(t, r, "/2010-04-01/Accounts/"+callsTestSid+"/Calls/"+dialTestActiveSid+".json", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	if got := factory.calls.Load(); got != 0 {
		t.Errorf("factory.Dial calls: got %d, want 0 (rate limit must short-circuit INVITE)", got)
	}

	if session.IsActive() {
		t.Errorf("session still active after rate limit block")
	}

	cj := decodeCall(t, rec)
	if cj.Status != "failed" {
		t.Errorf("response status: got %q, want \"failed\"", cj.Status)
	}
}

// ── Test 5: busy response ─────────────────────────────────────────────────────

// TestModifyCall_TwimlDial_BusyResponseStatus: factory returns a 486 Busy
// Here response → Forwarder sets result.Status="busy" → dialHandler calls
// Terminate("busy") → response status="busy".
func TestModifyCall_TwimlDial_BusyResponseStatus(t *testing.T) {
	t.Parallel()

	factory := &callsTestDialFactory{
		returnedErr: nonSuccessDialErr(486, "Busy Here"),
	}

	r, session, _ := mountDialRouter(t, factory, dialTestConfig(), &adapterStubWebhookFetcher{}, nil)

	form := "Twiml=" + url.QueryEscape("<Response><Dial>+4912345</Dial></Response>")
	rec := doPOST(t, r, "/2010-04-01/Accounts/"+callsTestSid+"/Calls/"+dialTestActiveSid+".json", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if session.IsActive() {
		t.Errorf("session still active after 486 Busy")
	}
	cj := decodeCall(t, rec)
	if cj.Status != "busy" {
		t.Errorf("response status: got %q, want \"busy\"", cj.Status)
	}
}

// ── Test 6: no-answer response ────────────────────────────────────────────────

// TestModifyCall_TwimlDial_NoAnswerResponseStatus: factory returns
// context.DeadlineExceeded (ring timeout) → result.Status="no-answer" →
// Terminate("no-answer") → response status="no-answer".
func TestModifyCall_TwimlDial_NoAnswerResponseStatus(t *testing.T) {
	t.Parallel()

	factory := &callsTestDialFactory{
		returnedErr: context.DeadlineExceeded,
	}

	r, session, _ := mountDialRouter(t, factory, dialTestConfig(), &adapterStubWebhookFetcher{}, nil)

	form := "Twiml=" + url.QueryEscape("<Response><Dial>+4912345</Dial></Response>")
	rec := doPOST(t, r, "/2010-04-01/Accounts/"+callsTestSid+"/Calls/"+dialTestActiveSid+".json", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if session.IsActive() {
		t.Errorf("session still active after ring timeout")
	}
	cj := decodeCall(t, rec)
	if cj.Status != "no-answer" {
		t.Errorf("response status: got %q, want \"no-answer\"", cj.Status)
	}
}

// ── Test 7: privacy gate fires first ─────────────────────────────────────────

// TestModifyCall_TwimlDial_PrivacyGateFiresFirst: verifies that CloseStream
// is called BEFORE Forwarder.Dial by proving that CalleeLeg() is non-nil
// (set by SetLeg inside PrepareDial) at the time factory.Dial is invoked
// (called inside PerformDial). If PrepareDial runs before PerformDial, the
// ordering is guaranteed.
func TestModifyCall_TwimlDial_PrivacyGateFiresFirst(t *testing.T) {
	t.Parallel()

	var calleeLegAtDialTime *bridge.Leg // captured inside factory.Dial
	var sessionRef *bridge.CallSession   // populated after mountDialRouter

	wrappedFactory := &privacyGateTestFactory{
		inner:       &callsTestDialFactory{returnedErr: nonSuccessDialErr(486, "Busy Here")},
		sessionPtr:  &sessionRef,
		capturedLeg: &calleeLegAtDialTime,
	}

	r, sess, _ := mountDialRouter(t, wrappedFactory, dialTestConfig(), &adapterStubWebhookFetcher{}, nil)
	sessionRef = sess // must be set before doPOST so factory.Dial sees it

	form := "Twiml=" + url.QueryEscape("<Response><Dial>+4912345</Dial></Response>")
	rec := doPOST(t, r, "/2010-04-01/Accounts/"+callsTestSid+"/Calls/"+dialTestActiveSid+".json", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// At the point factory.Dial was invoked, CalleeLeg must already be set
	// (PrepareDial ran before PerformDial → CloseStream before Dial).
	if calleeLegAtDialTime == nil {
		t.Error("privacyGateTestFactory.Dial called but CalleeLeg was nil — PrepareDial must run before PerformDial")
	}
}

// privacyGateTestFactory wraps an inner callsTestDialFactory and captures the
// callee leg state at the time Dial is invoked — proving PrepareDial (which
// calls SetLeg) ran before PerformDial (which calls Dial).
type privacyGateTestFactory struct {
	inner       *callsTestDialFactory
	sessionPtr  **bridge.CallSession
	capturedLeg **bridge.Leg
}

func (f *privacyGateTestFactory) Dial(
	ctx context.Context,
	recipient siplib.Uri,
	from siplib.Uri,
	displayName string,
	ppi *siplib.Uri,
	body []byte,
	auth sip.DialAuth,
	onResponse func(*siplib.Response) error,
) (sip.DialClient, error) {
	// Capture CalleeLeg state at Dial invocation time.
	if *f.sessionPtr != nil {
		*f.capturedLeg = (*f.sessionPtr).CalleeLeg()
	}
	return f.inner.Dial(ctx, recipient, from, displayName, ppi, body, auth, onResponse)
}

func (f *privacyGateTestFactory) ReadBye(req *siplib.Request, tx siplib.ServerTransaction) error {
	return f.inner.ReadBye(req, tx)
}

// ── Test 8: action callback fired ────────────────────────────────────────────

// TestModifyCall_TwimlDial_ActionCallback: when the <Dial> verb has an
// action= attribute, fireActionCallback is dispatched after PerformDial
// completes. Verified via adapterStubWebhookFetcher call count.
func TestModifyCall_TwimlDial_ActionCallback(t *testing.T) {
	t.Parallel()

	wc := &adapterStubWebhookFetcher{}

	// Dial fails (nil final response) → status="failed". Action callback
	// fires regardless of success/failure because PerformDial fires it before
	// returning its error.
	r, _, _ := mountDialRouter(t, nil, dialTestConfig(), wc, nil)

	actionURL := url.QueryEscape("https://example.com/dial-action")
	dialTwiml := `<Response><Dial action="https://example.com/dial-action" method="POST">+4912345</Dial></Response>`
	form := "Twiml=" + url.QueryEscape(dialTwiml)

	rec := doPOST(t, r, "/2010-04-01/Accounts/"+callsTestSid+"/Calls/"+dialTestActiveSid+".json", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	_ = actionURL

	// The action callback goroutine may be asynchronous; poll briefly.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&wc.calls) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if n := atomic.LoadInt32(&wc.calls); n == 0 {
		t.Error("action callback not fired — FetchWithFallback call count = 0")
	}
	if u := wc.URL(); !strings.Contains(u, "example.com/dial-action") {
		t.Errorf("action callback URL = %q, want https://example.com/dial-action", u)
	}
	if b := wc.Body(); !strings.Contains(b, "DialCallStatus=") {
		t.Errorf("action callback body missing DialCallStatus; got: %s", b)
	}
}

// ───────────────────────────────────────────────────────────────────────────
// REST modifyOpts StatusCallback parsing
// ───────────────────────────────────────────────────────────────────────────

// statusCallbackTestRouter wires a Mount router around an active CallSession
// and returns the session so tests can assert against session.StatusCallback().
func statusCallbackTestRouter(t *testing.T) (http.Handler, *bridge.CallSession) {
	t.Helper()
	active := bridge.NewTestSession(modifyTestActiveSid, callsTestSid, func(_ context.Context) error { return nil })
	q := &modifyBridge{active: active}
	r, _ := mountModifyRouter(t, q, &fakeWebhookClient{})
	return r, active
}

// TestModifyCall_StatusCallback_RepeatedKeys — multi-value StatusCallbackEvent
// via repeated keys (Twilio form encoding A): event tokens flatten.
func TestModifyCall_StatusCallback_RepeatedKeys(t *testing.T) {
	t.Parallel()

	r, active := statusCallbackTestRouter(t)

	body := url.Values{}
	body.Set("StatusCallback", "https://customer.example/cb")
	body.Set("StatusCallbackMethod", "POST")
	body["StatusCallbackEvent"] = []string{"initiated", "ringing", "answered", "completed"}

	rec := doPOST(t, r, modifyURL(modifyTestActiveSid), body.Encode())
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	cfg := active.StatusCallback()
	if cfg == nil {
		t.Fatal("session.StatusCallback() = nil, want non-nil after POST")
	}
	if cfg.URL != "https://customer.example/cb" {
		t.Errorf("cfg.URL = %q", cfg.URL)
	}
	if cfg.Method != "POST" {
		t.Errorf("cfg.Method = %q", cfg.Method)
	}
	for _, want := range []string{"initiated", "ringing", "answered", "completed"} {
		if _, ok := cfg.Events[want]; !ok {
			t.Errorf("cfg.Events missing %q; got %v", want, cfg.Events)
		}
	}
}

// TestModifyCall_StatusCallback_SpaceSeparated — single key holding a
// space-separated value tokenizes identically (Twilio form encoding B).
func TestModifyCall_StatusCallback_SpaceSeparated(t *testing.T) {
	t.Parallel()

	r, active := statusCallbackTestRouter(t)

	body := url.Values{}
	body.Set("StatusCallback", "https://customer.example/cb")
	body.Set("StatusCallbackEvent", "initiated ringing answered completed")

	rec := doPOST(t, r, modifyURL(modifyTestActiveSid), body.Encode())
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	cfg := active.StatusCallback()
	if cfg == nil {
		t.Fatal("session.StatusCallback() = nil")
	}
	for _, want := range []string{"initiated", "ringing", "answered", "completed"} {
		if _, ok := cfg.Events[want]; !ok {
			t.Errorf("cfg.Events missing %q", want)
		}
	}
}

// TestModifyCall_StatusCallback_CommaSeparated — I-3 fix: comma form parses
// identically.
func TestModifyCall_StatusCallback_CommaSeparated(t *testing.T) {
	t.Parallel()

	r, active := statusCallbackTestRouter(t)

	body := url.Values{}
	body.Set("StatusCallback", "https://customer.example/cb")
	body.Set("StatusCallbackEvent", "initiated,ringing,answered,completed")

	rec := doPOST(t, r, modifyURL(modifyTestActiveSid), body.Encode())
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	cfg := active.StatusCallback()
	if cfg == nil {
		t.Fatal("session.StatusCallback() = nil")
	}
	if len(cfg.Events) != 4 {
		t.Errorf("len(cfg.Events) = %d, want 4; got %v", len(cfg.Events), cfg.Events)
	}
}

// TestModifyCall_StatusCallback_RejectsUnknownEvent — strict enum gate (I-3).
// Unknown event name → HTTP 400 with code 21218 + message citing "ringinX".
func TestModifyCall_StatusCallback_RejectsUnknownEvent(t *testing.T) {
	t.Parallel()

	r, active := statusCallbackTestRouter(t)

	body := url.Values{}
	body.Set("StatusCallback", "https://customer.example/cb")
	body.Set("StatusCallbackEvent", "initiated,ringinX")

	rec := doPOST(t, r, modifyURL(modifyTestActiveSid), body.Encode())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	errBody := decodeError(t, rec)
	if errBody.Code != 21218 {
		t.Errorf("error code: got %d, want 21218", errBody.Code)
	}
	if !strings.Contains(errBody.Message, "ringinX") {
		t.Errorf("error message %q must cite \"ringinX\"", errBody.Message)
	}
	// Subscription must NOT have been installed.
	if cfg := active.StatusCallback(); cfg != nil {
		t.Errorf("session.StatusCallback() = %v, want nil after parse rejection", cfg)
	}
}

// TestModifyCall_StatusCallback_RejectsBadScheme — http:// URL fails
// ValidateCallbackURL pre-flight → HTTP 400 (Twilio code 11200 mapping
// surfaces via ErrInvalidParams' generic 21218 in this plan; the message
// must mention "StatusCallback").
func TestModifyCall_StatusCallback_RejectsBadScheme(t *testing.T) {
	t.Parallel()

	r, _ := statusCallbackTestRouter(t)

	body := url.Values{}
	body.Set("StatusCallback", "http://customer.example/cb")

	rec := doPOST(t, r, modifyURL(modifyTestActiveSid), body.Encode())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	errBody := decodeError(t, rec)
	if !strings.Contains(errBody.Message, "StatusCallback") {
		t.Errorf("error message %q must mention StatusCallback", errBody.Message)
	}
}

// TestModifyCall_StatusCallback_RejectsLocalhost — literal localhost is
// rejected without DNS by ValidateCallbackURL (16-02).
func TestModifyCall_StatusCallback_RejectsLocalhost(t *testing.T) {
	t.Parallel()

	r, _ := statusCallbackTestRouter(t)

	body := url.Values{}
	body.Set("StatusCallback", "https://localhost/cb")

	rec := doPOST(t, r, modifyURL(modifyTestActiveSid), body.Encode())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	errBody := decodeError(t, rec)
	if !strings.Contains(errBody.Message, "StatusCallback") {
		t.Errorf("error message %q must mention StatusCallback", errBody.Message)
	}
}

// TestModifyCall_StatusCallback_OmittedIsNoOp — without StatusCallback in the
// body, the handler must not install a subscription. The baseline of
// {Twiml, Url, Status} continues to operate.
func TestModifyCall_StatusCallback_OmittedIsNoOp(t *testing.T) {
	t.Parallel()

	r, active := statusCallbackTestRouter(t)

	rec := doPOST(t, r, modifyURL(modifyTestActiveSid), "Status=completed")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if cfg := active.StatusCallback(); cfg != nil {
		t.Errorf("session.StatusCallback() = %v, want nil (omitted is no-op)", cfg)
	}
}

// TestModifyCall_StatusCallbackOnly_NoOtherBodyParams — a body with ONLY
// StatusCallback (no Twiml/Url/Status) must succeed: Twilio treats this as
// "subscribe; do not change call state". This relaxes the
// "at-least-one-of {Twiml, Url, Status}" check to include StatusCallback.
func TestModifyCall_StatusCallbackOnly_NoOtherBodyParams(t *testing.T) {
	t.Parallel()

	r, active := statusCallbackTestRouter(t)

	body := url.Values{}
	body.Set("StatusCallback", "https://customer.example/cb")
	body.Set("StatusCallbackEvent", "completed")

	rec := doPOST(t, r, modifyURL(modifyTestActiveSid), body.Encode())
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (StatusCallback alone is valid); body=%s", rec.Code, rec.Body.String())
	}
	cfg := active.StatusCallback()
	if cfg == nil {
		t.Fatal("session.StatusCallback() = nil, want non-nil")
	}
	if cfg.URL != "https://customer.example/cb" {
		t.Errorf("cfg.URL = %q", cfg.URL)
	}
	if active.IsActive() != true {
		t.Errorf("session terminated unexpectedly — StatusCallback alone must NOT advance call state")
	}
}

// TestModifyCall_StatusCallback_DefaultsMethodToPOST — empty
// StatusCallbackMethod must default to POST per Twilio convention.
func TestModifyCall_StatusCallback_DefaultsMethodToPOST(t *testing.T) {
	t.Parallel()

	r, active := statusCallbackTestRouter(t)

	body := url.Values{}
	body.Set("StatusCallback", "https://customer.example/cb")
	body.Set("StatusCallbackEvent", "completed")

	rec := doPOST(t, r, modifyURL(modifyTestActiveSid), body.Encode())
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	cfg := active.StatusCallback()
	if cfg == nil {
		t.Fatal("session.StatusCallback() = nil")
	}
	if cfg.Method != "POST" {
		t.Errorf("cfg.Method = %q, want POST (Twilio default)", cfg.Method)
	}
}

// TestModifyCall_StatusCallback_RejectsBadMethod — only POST and GET are
// supported.
func TestModifyCall_StatusCallback_RejectsBadMethod(t *testing.T) {
	t.Parallel()

	r, active := statusCallbackTestRouter(t)

	body := url.Values{}
	body.Set("StatusCallback", "https://customer.example/cb")
	body.Set("StatusCallbackMethod", "PUT")

	rec := doPOST(t, r, modifyURL(modifyTestActiveSid), body.Encode())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if cfg := active.StatusCallback(); cfg != nil {
		t.Errorf("session.StatusCallback() = %v, want nil after PUT rejection", cfg)
	}
}

// TestParseModifyOpts_SSRFRejection_LogsAndIncrementsMetric verifies that
// when the REST modify-call body's StatusCallback URL fails the SSRF
// pre-flight at api/calls.go, the handler emits a log.Warn() AND
// increments status_callback_failures_total{reason="ssrf_rejected"}.
// Without this, operator dashboards see only the StatusClient.Enqueue-
// side SSRF rejection and have no signal for the REST-side rejection.
//
// The test wires its own Mount() with a log-capturing zerolog writer (so the
// WARN can be asserted on the JSON byte stream) AND a fresh Metrics registry
// (so testutil.ToFloat64 reads a clean counter). Mirrors the existing
// TestModifyCall_StatusCallback_RejectsLocalhost coverage but additionally
// asserts the observability side-effects.
func TestParseModifyOpts_SSRFRejection_LogsAndIncrementsMetric(t *testing.T) {
	t.Parallel()

	// Capture log output via a buffer-backed zerolog logger.
	var logBuf bytes.Buffer
	logger := zerolog.New(&logBuf)

	// Build a router with our captured logger + a fresh metrics registry so
	// the ssrf_rejected counter starts at zero.
	active := bridge.NewTestSession(modifyTestActiveSid, callsTestSid, func(_ context.Context) error { return nil })
	q := &modifyBridge{active: active}
	r := chi.NewRouter()
	m := observability.NewMetrics()
	Mount(r, q, nil, callsTestSid, callsTestAuthToken, m, &fakeWebhookClient{}, testActionPoster(""), logger, nil, config.Config{})

	body := url.Values{}
	body.Set("StatusCallback", "https://localhost/cb") // SSRF: literal localhost rejected pre-DNS

	before := testutil.ToFloat64(m.StatusCallbackFailuresTotal.WithLabelValues("ssrf_rejected"))
	rec := doPOST(t, r, modifyURL(modifyTestActiveSid), body.Encode())
	after := testutil.ToFloat64(m.StatusCallbackFailuresTotal.WithLabelValues("ssrf_rejected"))

	// Behaviour-preservation assertions (existing semantics must hold).
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	errBody := decodeError(t, rec)
	if !strings.Contains(errBody.Message, "StatusCallback") {
		t.Errorf("error message %q must mention StatusCallback", errBody.Message)
	}

	// Observability fold-in assertions.
	if delta := after - before; delta != 1 {
		t.Errorf("status_callback_failures_total{reason=\"ssrf_rejected\"} delta = %v, want 1", delta)
	}
	logOut := logBuf.String()
	// WARN level must be emitted; presence of `"level":"warn"` in JSON is
	// the load-bearing signal. The Msg may evolve; we anchor on the SSRF
	// keyword so future edits remain backwards-compatible.
	if !strings.Contains(logOut, `"level":"warn"`) {
		t.Errorf("expected WARN log emission; got: %s", logOut)
	}
	if !strings.Contains(logOut, "ssrf") && !strings.Contains(logOut, "SSRF") {
		t.Errorf("expected SSRF mention in log output; got: %s", logOut)
	}
	// Defense-in-depth: the SSRF-rejected URL bytes must NOT appear in the
	// log output — only the host (or a masked indicator). The URL was
	// "https://localhost/cb"; if the full URL appears, the urlHostMasked
	// helper failed.
	if strings.Contains(logOut, "https://localhost/cb") {
		t.Errorf("full callback URL leaked into log output; expected host-only or masked indicator: %s", logOut)
	}
}

// TestModifyCall_StatusCallback_MixedSeparatorsAcrossKeys — both repeated-key
// and within-value separators in one POST: every event flattens into the set.
func TestModifyCall_StatusCallback_MixedSeparatorsAcrossKeys(t *testing.T) {
	t.Parallel()

	r, active := statusCallbackTestRouter(t)

	body := url.Values{}
	body.Set("StatusCallback", "https://customer.example/cb")
	body["StatusCallbackEvent"] = []string{"initiated,ringing", "answered completed"}

	rec := doPOST(t, r, modifyURL(modifyTestActiveSid), body.Encode())
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	cfg := active.StatusCallback()
	if cfg == nil {
		t.Fatal("session.StatusCallback() = nil")
	}
	for _, want := range []string{"initiated", "ringing", "answered", "completed"} {
		if _, ok := cfg.Events[want]; !ok {
			t.Errorf("cfg.Events missing %q; got %v", want, cfg.Events)
		}
	}
}
