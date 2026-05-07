package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/bridge"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/config"
)

// ── /health contract tests ─────────────────────────────────────────────────
//
// The /health endpoint is the K8s readiness probe surface. Its JSON shape is
// LOCKED at exactly four fields:
//   - registered      (bool)
//   - account_sid     (string, COMPAT-02 regex ^AC[0-9a-f]{32}$)
//   - active_calls    (int)
//   - active_forwards (int)
// No mode field (streaming is always-on default), no rtp_port_pool_in_use_ratio
// (operator-monitoring belongs in /metrics).
//
// Latency contract: <5ms p99 even under load. The escape hatch is t.Skip
// when CI_SLOW_HOST=true — explicit, not silent baked-in slack.
//
// These tests exercise the extracted newHealthHandler closure with a fake
// registrationProbe + a real CallManager populated via the bridge package's
// exported test helpers.

// fakeRegistrar satisfies the registrationProbe interface used by
// newHealthHandler. Lets us flip Registered=true/false without spinning up a
// real *sip.Registrar (which requires a sipgo.Client + UDP socket).
type fakeRegistrar struct {
	registered bool
}

func (f fakeRegistrar) IsRegistered() bool { return f.registered }

// newTestHealthHandler constructs a CallManager + fakeRegistrar and returns
// the wired /health handler. The accountSid follows the Twilio
// COMPAT-02 regex ^AC[0-9a-f]{32}$.
func newTestHealthHandler(t *testing.T, registered bool) (http.HandlerFunc, *bridge.CallManager) {
	t.Helper()
	pool, err := bridge.NewPortPool(20000, 20001)
	if err != nil {
		t.Fatalf("NewPortPool: %v", err)
	}
	const accountSid = "ACdeadbeefdeadbeefdeadbeefdeadbeef"
	cm := bridge.NewCallManager(pool, accountSid, config.Config{}, zerolog.Nop(), nil)
	t.Cleanup(cm.Close)
	return newHealthHandler(fakeRegistrar{registered: registered}, cm, accountSid), cm
}

// TestHealth_FourFieldContract_NoExtraneous — the JSON has exactly the four
// locked fields. Adding a 5th field requires updating BOTH the handler AND
// this test (visible in PR diff).
func TestHealth_FourFieldContract_NoExtraneous(t *testing.T) {
	t.Parallel()
	handler, _ := newTestHealthHandler(t, true)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d (body=%q)", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode JSON: %v (body=%q)", err, w.Body.String())
	}

	// Required fields present.
	wantKeys := []string{"registered", "account_sid", "active_calls", "active_forwards"}
	for _, k := range wantKeys {
		if _, ok := resp[k]; !ok {
			t.Errorf("missing required field %q in /health response (body=%q)", k, w.Body.String())
		}
	}

	// Forbidden fields ABSENT — locked discipline.
	for _, forbidden := range []string{"mode", "rtp_port_pool_in_use_ratio"} {
		if _, ok := resp[forbidden]; ok {
			t.Errorf("/health must NOT contain %q (locked four-field contract; ratio belongs in /metrics)", forbidden)
		}
	}

	// Field-shape sanity (json.Unmarshal into any → bool/string/float64 for numbers).
	if _, ok := resp["registered"].(bool); !ok {
		t.Errorf("registered must be bool, got %T", resp["registered"])
	}
	if _, ok := resp["account_sid"].(string); !ok {
		t.Errorf("account_sid must be string, got %T", resp["account_sid"])
	}
	if _, ok := resp["active_calls"].(float64); !ok {
		t.Errorf("active_calls must be number, got %T", resp["active_calls"])
	}
	if _, ok := resp["active_forwards"].(float64); !ok {
		t.Errorf("active_forwards must be number, got %T", resp["active_forwards"])
	}

	// Total field count == 4 (no extraneous keys at all).
	if len(resp) != 4 {
		gotKeys := make([]string, 0, len(resp))
		for k := range resp {
			gotKeys = append(gotKeys, k)
		}
		t.Errorf("/health JSON has %d fields, want exactly 4 (got keys: %v)", len(resp), gotKeys)
	}
}

// TestHealth_AccountSidShape — account_sid matches Twilio's ^AC[0-9a-f]{32}$
// (COMPAT-02 invariant). Operators rely on this for grep-based correlation
// across /health, /metrics, and the REST API.
func TestHealth_AccountSidShape(t *testing.T) {
	t.Parallel()
	handler, _ := newTestHealthHandler(t, true)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got, _ := resp["account_sid"].(string)
	matched, _ := regexp.MatchString(`^AC[0-9a-f]{32}$`, got)
	if !matched {
		t.Errorf("account_sid %q does not match COMPAT-02 regex ^AC[0-9a-f]{32}$", got)
	}
}

// TestHealth_ActiveForwards_ReflectsCallManager — populate a CallManager with
// a known mix of states and assert /health returns the live counts:
//   - 3 sessions in forwarding states → active_forwards == 3
//   - 2 in streaming + 3 in forwarding = 5 active sessions → active_calls == 5
//
// (StateTerminated entries should also appear in m.sessions — but in
// production they are removed before reaching that state. We only inject
// non-terminated states here so the count math is unambiguous.)
func TestHealth_ActiveForwards_ReflectsCallManager(t *testing.T) {
	t.Parallel()
	handler, cm := newTestHealthHandler(t, true)

	mix := []bridge.State{
		bridge.StateForwarding,
		bridge.StateForwarding,
		bridge.StateDialingOut, // counts as forwarding
		bridge.StateStreaming,
		bridge.StateStreaming,
	}
	for i, st := range mix {
		bridge.AddSessionInStateForTest(cm, fmt.Sprintf("callid-%d", i), fmt.Sprintf("CAhealth%026d", i), st)
	}

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got := resp["active_calls"].(float64); got != float64(len(mix)) {
		t.Errorf("active_calls = %v, want %d (every non-terminated session in m.sessions)", got, len(mix))
	}
	const wantForwards = 3 // 2× StateForwarding + 1× StateDialingOut
	if got := resp["active_forwards"].(float64); got != wantForwards {
		t.Errorf("active_forwards = %v, want %d", got, wantForwards)
	}
	if got := resp["registered"].(bool); !got {
		t.Errorf("registered = %v, want true (fakeRegistrar configured registered=true)", got)
	}
}

// TestHealth_LatencyUnder5ms — p99 latency over 100 measurements with 1000
// sessions in mixed states must be under 5ms. The 5ms ceiling is the locked
// SLO — operators rely on it for K8s readiness probe budget.
// CI_SLOW_HOST=true is the documented escape hatch (t.Skip on demonstrably
// slow runners), NOT a silent relaxation to 50ms.
func TestHealth_LatencyUnder5ms(t *testing.T) {
	if os.Getenv("CI_SLOW_HOST") == "true" {
		t.Skip("CI host marked too slow for sub-5ms /health latency assertion (CI_SLOW_HOST=true); the contract still locks at <5ms — fix the host or the handler, not the test")
	}
	t.Parallel()
	handler, cm := newTestHealthHandler(t, true)

	const N = 1000
	for i := 0; i < N; i++ {
		var st bridge.State
		switch i % 5 {
		case 0:
			st = bridge.StateStreaming
		case 1:
			st = bridge.StateForwarding
		case 2:
			st = bridge.StateDialingOut
		case 3:
			st = bridge.StateForwardingSetup
		case 4:
			st = bridge.StateDispatching
		}
		bridge.AddSessionInStateForTest(cm, fmt.Sprintf("callid-lat-%d", i), fmt.Sprintf("CAhealthlat%023d", i), st)
	}

	// Warm up — discard first measurement.
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))

	const iterations = 100
	durations := make([]time.Duration, iterations)
	for i := 0; i < iterations; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/health", nil)
		start := time.Now()
		handler.ServeHTTP(w, r)
		durations[i] = time.Since(start)
		if w.Code != http.StatusOK {
			t.Fatalf("iteration %d: HTTP %d", i, w.Code)
		}
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p99 := durations[98] // 99th percentile of 100 samples
	if p99 > 5*time.Millisecond {
		t.Errorf("/health p99 latency over %d sessions = %v, exceeds 5ms ceiling (locked SLO; set CI_SLOW_HOST=true to skip on demonstrably slow CI runners — never relax the assertion)",
			N, p99)
	}
}

// TestHealth_HTTP200_ContentType — HTTP 200 + Content-Type must contain
// "application/json". K8s kubelet uses the status code for the readiness
// signal; observability scrapers use Content-Type for parser dispatch.
func TestHealth_HTTP200_ContentType(t *testing.T) {
	t.Parallel()
	handler, _ := newTestHealthHandler(t, true)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("Content-Type = %q, want substring \"application/json\"", got)
	}
}

// TestHealth_K8sReadinessSmoke — operator-shaped end-to-end probe: GET /health,
// decode via standard encoding/json, assert no decode error AND every locked
// field is present. Mirrors what a K8s kubelet readiness probe (or curl-based
// observability scraper) would do.
func TestHealth_K8sReadinessSmoke(t *testing.T) {
	t.Parallel()
	handler, cm := newTestHealthHandler(t, true)

	// Inject a realistic mix so the response carries non-zero counts. K8s
	// readiness only cares that /health responds, but a healthy probe with
	// zero traffic should still emit valid integers (not null/missing).
	for i := 0; i < 5; i++ {
		bridge.AddSessionInStateForTest(cm, fmt.Sprintf("callid-k8s-%d", i), fmt.Sprintf("CAk8sready%024d", i), bridge.StateStreaming)
	}
	for i := 0; i < 2; i++ {
		bridge.AddSessionInStateForTest(cm, fmt.Sprintf("callid-k8s-fwd-%d", i), fmt.Sprintf("CAk8sfwd%026d", i), bridge.StateForwarding)
	}

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200 (K8s readiness probe would fail)", resp.StatusCode)
	}

	var parsed struct {
		Registered     bool   `json:"registered"`
		AccountSid     string `json:"account_sid"`
		ActiveCalls    int    `json:"active_calls"`
		ActiveForwards int    `json:"active_forwards"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode: %v (a K8s probe with strict JSON parsing would fail)", err)
	}
	if !parsed.Registered {
		t.Errorf("Registered = false, want true (fakeRegistrar configured registered=true)")
	}
	if parsed.AccountSid == "" {
		t.Errorf("AccountSid empty in K8s probe response")
	}
	if parsed.ActiveCalls != 7 {
		t.Errorf("ActiveCalls = %d, want 7 (5 streaming + 2 forwarding)", parsed.ActiveCalls)
	}
	if parsed.ActiveForwards != 2 {
		t.Errorf("ActiveForwards = %d, want 2", parsed.ActiveForwards)
	}
}
