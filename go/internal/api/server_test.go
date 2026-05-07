package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/rs/zerolog"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/bridge"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/config"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/observability"
)

// stubCall implements bridge.BridgeCall for server_test fixtures. The
// embedded fakeCall (defined in json_test.go) already implements every
// method the interface requires, so stubCall is a thin wrapper that exists
// only to make the bridge.BridgeCall typing explicit at the call site.
type stubCall struct {
	fakeCall
}

// stubBridge implements BridgeQuerier with canned data.
type stubBridge struct {
	calls map[string]bridge.BridgeCall
	all   []bridge.BridgeCall
}

func (s *stubBridge) List() []bridge.BridgeCall {
	return s.all
}

func (s *stubBridge) GetByCallSid(callSid string) (bridge.BridgeCall, bool) {
	c, ok := s.calls[callSid]
	return c, ok
}

func newStubBridge(calls ...bridge.BridgeCall) *stubBridge {
	idx := make(map[string]bridge.BridgeCall, len(calls))
	for _, c := range calls {
		idx[c.CallSid()] = c
	}
	return &stubBridge{calls: idx, all: calls}
}

const (
	srvSid       = "ACdeadbeefdeadbeefdeadbeefdeadbeef"
	srvAuthToken = "test-token-32-chars-long-padding"
	srvCallSid   = "CA00000000000000000000000000000001"
)

func srvBasicAuthHeader() string {
	creds := srvSid + ":" + srvAuthToken
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(creds))
}

func newSeededCall() bridge.BridgeCall {
	return stubCall{fakeCall{
		callSid:    srvCallSid,
		accountSid: srvSid,
		from:       "+15551112222",
		to:         "+15553334444",
		direction:  "inbound",
		status:     "in-progress",
		startTime:  time.Date(2026, time.April, 27, 10, 0, 0, 0, time.UTC),
	}}
}

// TestMount_RoutesRegistered: mount on a chi.NewRouter() with stub
// BridgeQuerier; httptest a GET to /2010-04-01/Accounts/{configured-sid}/Calls.json
// with valid Basic Auth → 200 + JSON body.
func TestMount_RoutesRegistered(t *testing.T) {
	t.Parallel()

	q := newStubBridge(newSeededCall())
	r := chi.NewRouter()
	m := observability.NewMetrics()
	Mount(r, q, nil, srvSid, srvAuthToken, m, nilWebhookFetcher{}, testActionPoster(""), zerolog.Nop(), nil, config.Config{})

	req := httptest.NewRequest(http.MethodGet, "/2010-04-01/Accounts/"+srvSid+"/Calls.json", nil)
	req.Header.Set("Authorization", srvBasicAuthHeader())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type: got %q, want application/json", got)
	}
	var env PageJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode body: %v\nbody=%s", err, rec.Body.String())
	}
	if len(env.Calls) != 1 {
		t.Fatalf("calls: got %d, want 1", len(env.Calls))
	}
	if env.Calls[0].Sid != srvCallSid {
		t.Fatalf("call sid: got %q, want %q", env.Calls[0].Sid, srvCallSid)
	}
}

// TestMount_AuthRequired: Calls.json without Authorization header → 401 +
// Twilio JSON error body + WWW-Authenticate header.
func TestMount_AuthRequired(t *testing.T) {
	t.Parallel()

	q := newStubBridge()
	r := chi.NewRouter()
	m := observability.NewMetrics()
	Mount(r, q, nil, srvSid, srvAuthToken, m, nilWebhookFetcher{}, testActionPoster(""), zerolog.Nop(), nil, config.Config{})

	req := httptest.NewRequest(http.MethodGet, "/2010-04-01/Accounts/"+srvSid+"/Calls.json", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != `Basic realm="Twilio API"` {
		t.Fatalf("WWW-Authenticate header: got %q", got)
	}
	var body Error
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Code != 20003 {
		t.Fatalf("error code: got %d, want 20003", body.Code)
	}
	if body.Status != http.StatusUnauthorized {
		t.Fatalf("error status: got %d, want 401", body.Status)
	}
}

// TestMount_AuthRequired_HSTS_Present: on a 401 unauthenticated request, all
// three security headers (HSTS / CSP / X-Content-Type-Options) MUST be
// present. This pins the load-bearing ordering invariant — SecurityHeaders
// middleware fires BEFORE BasicAuth in Mount(), so even auth-failed responses
// carry the headers (anti-DoS hardening, defense in depth).
func TestMount_AuthRequired_HSTS_Present(t *testing.T) {
	t.Parallel()

	q := newStubBridge()
	r := chi.NewRouter()
	m := observability.NewMetrics()
	Mount(r, q, nil, srvSid, srvAuthToken, m, nilWebhookFetcher{}, testActionPoster(""), zerolog.Nop(), nil, config.Config{})

	req := httptest.NewRequest(http.MethodGet, "/2010-04-01/Accounts/"+srvSid+"/Calls.json", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("Strict-Transport-Security"); got != "max-age=63072000; includeSubDomains" {
		t.Fatalf("HSTS on 401: got %q, want %q", got, "max-age=63072000; includeSubDomains")
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != "default-src 'none'; frame-ancestors 'none'" {
		t.Fatalf("CSP on 401: got %q, want %q", got, "default-src 'none'; frame-ancestors 'none'")
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options on 401: got %q, want \"nosniff\"", got)
	}
}

// TestMount_BodyOverflow_Returns413_BeforeAuth POSTs to /Calls/{Sid}.json
// with no Authorization header AND Content-Length > 65536. Asserts status
// is 413 (NOT 401) — proves the security middleware (MaxBytesReader) fires
// BEFORE BasicAuth in Mount() — anti-DoS minimum work to reject an
// unauthenticated oversize-body attack.
func TestMount_BodyOverflow_Returns413_BeforeAuth(t *testing.T) {
	t.Parallel()

	q := newStubBridge(newSeededCall())
	r := chi.NewRouter()
	m := observability.NewMetrics()
	Mount(r, q, nil, srvSid, srvAuthToken, m, nilWebhookFetcher{}, testActionPoster(""), zerolog.Nop(), nil, config.Config{})

	body := bytes.NewReader(make([]byte, 70_000)) // > 64KB
	req := httptest.NewRequest(http.MethodPost,
		"/2010-04-01/Accounts/"+srvSid+"/Calls/"+srvCallSid+".json", body)
	req.ContentLength = 70_000
	// NO Authorization header — proves 413 fires BEFORE BasicAuth.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: got %d, want 413 (must NOT be 401 — security middleware "+
			"must fire before BasicAuth); body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type: got %q, want application/json", got)
	}
	// Security headers must also be present on the 413 response.
	if got := rec.Header().Get("Strict-Transport-Security"); got == "" {
		t.Fatalf("HSTS header missing on 413 — SecurityHeaders must run before MaxBytesReader (or in front of it)")
	}
	var errBody Error
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decode Twilio error body: %v\nraw=%s", err, rec.Body.String())
	}
	if errBody.Code != 21617 {
		t.Fatalf("error code: got %d, want 21617", errBody.Code)
	}
	if errBody.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("error status field: got %d, want 413", errBody.Status)
	}
}

// TestMount_PathSidMismatch: Basic Auth correct but URL has a different
// AccountSid → 401 (NOT 404). 401 is the auth-boundary response; 404 would
// leak which AccountSid values exist on the server.
func TestMount_PathSidMismatch(t *testing.T) {
	t.Parallel()

	q := newStubBridge()
	r := chi.NewRouter()
	m := observability.NewMetrics()
	Mount(r, q, nil, srvSid, srvAuthToken, m, nilWebhookFetcher{}, testActionPoster(""), zerolog.Nop(), nil, config.Config{})

	wrongSid := "ACcafebabecafebabecafebabecafebabe"
	req := httptest.NewRequest(http.MethodGet, "/2010-04-01/Accounts/"+wrongSid+"/Calls.json", nil)
	req.Header.Set("Authorization", srvBasicAuthHeader())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401 (path SID mismatch must NOT 404)", rec.Code)
	}
}

// TestMetricsMiddleware_RecordsCounters: invoke the middleware-wrapped
// handler returning 200 → status="2xx" counter increments; with a 404
// handler → status="4xx" counter increments.
func TestMetricsMiddleware_RecordsCounters(t *testing.T) {
	t.Parallel()

	m := observability.NewMetrics()
	mw := metricsMiddleware(m, "list_calls")

	okHandler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	notFoundHandler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	rec := httptest.NewRecorder()
	okHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := readCounter(t, m.APIRequestsTotal.WithLabelValues("list_calls", "GET", "2xx")); got != 1 {
		t.Fatalf("2xx counter: got %v, want 1", got)
	}

	rec = httptest.NewRecorder()
	notFoundHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := readCounter(t, m.APIRequestsTotal.WithLabelValues("list_calls", "GET", "4xx")); got != 1 {
		t.Fatalf("4xx counter: got %v, want 1", got)
	}
}

// TestMetricsMiddleware_RecordsDuration: wrap a 50ms-sleeping handler;
// histogram observation count is 1 and sum >= 0.05.
func TestMetricsMiddleware_RecordsDuration(t *testing.T) {
	t.Parallel()

	m := observability.NewMetrics()
	mw := metricsMiddleware(m, "list_calls")

	slow := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	slow.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	count, sum := readHistogram(t, m.APIRequestDurationSeconds.WithLabelValues("list_calls"))
	if count != 1 {
		t.Fatalf("histogram count: got %d, want 1", count)
	}
	if sum < 0.05 {
		t.Fatalf("histogram sum: got %v, want >= 0.05", sum)
	}
}

// readCounter pulls the current value out of a prometheus.Counter via the
// gather-style API (Counter exposes no public Get()). Returns the float64
// value; tests compare to small integers.
func readCounter(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var pb dto.Metric
	if err := c.Write(&pb); err != nil {
		t.Fatalf("counter Write: %v", err)
	}
	return pb.GetCounter().GetValue()
}

// readHistogram returns (sample_count, sample_sum) for a Histogram via the
// dto.Metric protobuf, the only public way to inspect histogram state.
func readHistogram(t *testing.T, h prometheus.Observer) (uint64, float64) {
	t.Helper()
	hist, ok := h.(prometheus.Histogram)
	if !ok {
		t.Fatalf("readHistogram: observer is not a Histogram (%T)", h)
	}
	var pb dto.Metric
	if err := hist.Write(&pb); err != nil {
		t.Fatalf("histogram Write: %v", err)
	}
	return pb.GetHistogram().GetSampleCount(), pb.GetHistogram().GetSampleSum()
}
