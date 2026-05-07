package webhook

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sipgate/sipgate-sip-stream-bridge/internal/observability"
)

// testStatusClient builds a StatusClient that posts to srv (TLS) with
// automatic cert trust. Mirrors clientForTLSServer from client_test.go for
// the StatusClient surface — production NewStatusClient cannot reach an
// httptest TLS-server because newSSRFGuard().DialContext rejects loopback.
func testStatusClient(t *testing.T, srv *httptest.Server, authToken string) *StatusClient {
	t.Helper()
	m := observability.NewMetrics()
	log := zerolog.Nop()
	return newStatusClientWithTransport(srv.Client().Transport, authToken, m, log)
}

// sampleEvent builds a canonical Twilio-shape form for a "completed"
// event. The actual content is mostly irrelevant for transport-layer
// tests; what matters is that the form is well-formed, CallStatus is
// populated, and the URL is signed verbatim. Event is set to "completed"
// — the event-vocab label that drives
// status_callback_attempts_total{event=...}.
func sampleEvent(callbackURL, callSid string) CallbackEvent {
	form := url.Values{}
	form.Set("CallSid", callSid)
	form.Set("AccountSid", "ACtest0123456789abcdef0123456789ab")
	form.Set("From", "+4915123456789")
	form.Set("To", "+4930111222333")
	form.Set("Caller", "+4915123456789")
	form.Set("Called", "+4930111222333")
	form.Set("Direction", "inbound")
	form.Set("ApiVersion", "2010-04-01")
	form.Set("CallStatus", "completed")
	form.Set("Timestamp", time.Now().UTC().Format(time.RFC1123Z))
	form.Set("SequenceNumber", "0")
	form.Set("CallbackSource", "call-progress-events")
	return CallbackEvent{URL: callbackURL, Method: http.MethodPost, Form: form, Event: "completed"}
}

// TestStatusClient_OwnsPrivateTransport pins the blast-radius rule:
// NewStatusClient MUST own a private *http.Transport that is provably
// distinct from any other webhook surface. Tuning is asserted by direct
// field reads (reflection-free).
func TestStatusClient_OwnsPrivateTransport(t *testing.T) {
	c := NewStatusClient("12345", observability.NewMetrics(), zerolog.Nop())
	st, ok := c.http.Transport.(*signingTransport)
	if !ok {
		t.Fatalf("Transport is not *signingTransport: %T", c.http.Transport)
	}
	inner, ok := st.inner.(*http.Transport)
	if !ok {
		t.Fatalf("inner Transport is not *http.Transport: %T", st.inner)
	}
	if inner.DialContext == nil {
		t.Fatal("DialContext is nil — SSRF guard not wired")
	}
	if inner.MaxIdleConnsPerHost != 2 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 2", inner.MaxIdleConnsPerHost)
	}
	if inner.IdleConnTimeout != 90*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 90s", inner.IdleConnTimeout)
	}
	if !inner.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 = false; want true")
	}
	if inner.MaxConnsPerHost != 8 {
		t.Errorf("MaxConnsPerHost = %d, want 8", inner.MaxConnsPerHost)
	}
	if inner.TLSHandshakeTimeout != 3*time.Second {
		t.Errorf("TLSHandshakeTimeout = %v, want 3s", inner.TLSHandshakeTimeout)
	}
	if inner.ResponseHeaderTimeout != 4*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v, want 4s", inner.ResponseHeaderTimeout)
	}
}

// TestStatusClient_PerAttemptTimeout — http.Client.Timeout is the 4-second
// outer per-attempt cap.
func TestStatusClient_PerAttemptTimeout(t *testing.T) {
	c := NewStatusClient("12345", observability.NewMetrics(), zerolog.Nop())
	if c.http.Timeout != 4*time.Second {
		t.Errorf("http.Client.Timeout = %v, want 4s", c.http.Timeout)
	}
}

// TestStatusClient_PostsSignedHeader — end-to-end signer wiring.
// The captured X-Twilio-Signature MUST equal webhook.Sign(authToken,
// statusCallbackURL, body). Proves the signingTransport is composed
// inside NewStatusClient and runs on the outbound path.
func TestStatusClient_PostsSignedHeader(t *testing.T) {
	var capturedSig atomic.Value
	var capturedBody atomic.Value
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedSig.Store(r.Header.Get("X-Twilio-Signature"))
		b, _ := io.ReadAll(r.Body)
		capturedBody.Store(string(b))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := testStatusClient(t, srv, "12345")
	evt := sampleEvent(srv.URL+"/cb", "CAtest")
	if err := c.Enqueue("CAtest", evt); err != nil {
		t.Fatal(err)
	}
	if err := c.DrainAndClose("CAtest", 5*time.Second); err != nil {
		t.Fatal(err)
	}

	gotSig, _ := capturedSig.Load().(string)
	wantSig := Sign("12345", srv.URL+"/cb", evt.Form)
	if gotSig != wantSig {
		t.Errorf("X-Twilio-Signature: got %q want %q", gotSig, wantSig)
	}
	gotBody, _ := capturedBody.Load().(string)
	if gotBody != evt.Form.Encode() {
		t.Errorf("body: got %q want %q", gotBody, evt.Form.Encode())
	}
}

// TestStatusClient_QueueFullReturnsErr — depth=64 invariant.
// The 65th Enqueue while the worker is blocked MUST return ErrQueueFull
// and increment status_callback_failures_total{reason="queue_full"}.
func TestStatusClient_QueueFullReturnsErr(t *testing.T) {
	hold := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hold:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(hold)

	c := testStatusClient(t, srv, "12345")
	evt := sampleEvent(srv.URL+"/cb", "CAtest")

	// Fill the queue up to 64. The first Enqueue starts the worker, which
	// pulls one job and blocks on the slow handler — leaving 63 buffered
	// + 1 in-flight = 64 outstanding. Some Enqueue in this loop, once the
	// worker has pulled the first job, may succeed in re-filling; others
	// after the queue refills to 64 will fail. We tolerate both.
	for i := 0; i < 64; i++ {
		if err := c.Enqueue("CAtest", evt); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}
	// Now the queue holds 63 buffered + 1 in-flight (worker drained one).
	// Re-fill to drive ErrQueueFull. Best-effort retry loop absorbs any
	// scheduler interleaving where the worker drains another slot.
	var hitFull bool
	for i := 0; i < 200; i++ {
		err := c.Enqueue("CAtest", evt)
		if errors.Is(err, ErrQueueFull) {
			hitFull = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !hitFull {
		t.Fatal("expected ErrQueueFull within 200 retries; queue never reached capacity")
	}
	// Cleanup: best-effort drain. Worker is blocked on the slow handler,
	// so DrainAndClose will hit the timeout. That's expected.
	_ = c.DrainAndClose("CAtest", 50*time.Millisecond)
}

// TestStatusClient_RetriesOn5xx — measure backoff timing 1s/2s/4s ±20% (with
// a +500ms upper buffer for scheduler jitter). RESEARCH §5.1.
//
// The ±20% tolerance is for measurement noise only — production has no jitter
// (phase-brief decision: queue depth 64 + 3-retries-then-abandon caps the
// thundering-herd risk). I-9 reword.
func TestStatusClient_RetriesOn5xx(t *testing.T) {
	var attemptTimes []time.Time
	var mu sync.Mutex
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attemptTimes = append(attemptTimes, time.Now())
		mu.Unlock()
		w.WriteHeader(500)
	}))
	defer srv.Close()

	c := testStatusClient(t, srv, "12345")
	evt := sampleEvent(srv.URL+"/cb", "CAtest")
	if err := c.Enqueue("CAtest", evt); err != nil {
		t.Fatal(err)
	}
	if err := c.DrainAndClose("CAtest", 30*time.Second); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(attemptTimes) != 4 {
		t.Fatalf("attempts = %d, want 4 (initial + 3 retries)", len(attemptTimes))
	}
	wantDelays := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}
	for i := 0; i < len(wantDelays); i++ {
		got := attemptTimes[i+1].Sub(attemptTimes[i])
		minD := wantDelays[i] - wantDelays[i]/5
		maxD := wantDelays[i] + wantDelays[i]/5 + 500*time.Millisecond
		if got < minD || got > maxD {
			t.Errorf("delay attempt %d→%d: got %v, want ~%v (range %v..%v)",
				i+1, i+2, got, wantDelays[i], minD, maxD)
		}
	}
}

// TestStatusClient_RetriesOn429And408 — both codes trigger the full retry
// chain (4 attempts total). 408 (Request Timeout) and 429 (Too Many Requests)
// are explicitly retry-eligible per RESEARCH §5.1.
func TestStatusClient_RetriesOn429And408(t *testing.T) {
	for _, code := range []int{408, 429} {
		code := code
		t.Run(http.StatusText(code), func(t *testing.T) {
			var attempts atomic.Int32
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.WriteHeader(code)
			}))
			defer srv.Close()
			c := testStatusClient(t, srv, "12345")
			if err := c.Enqueue("CAtest", sampleEvent(srv.URL+"/cb", "CAtest")); err != nil {
				t.Fatal(err)
			}
			if err := c.DrainAndClose("CAtest", 30*time.Second); err != nil {
				t.Fatal(err)
			}
			if got := attempts.Load(); got != 4 {
				t.Errorf("attempts on %d = %d, want 4", code, got)
			}
		})
	}
}

// TestStatusClient_DoesNotRetryOn4xx — 400/401/403/404 are all terminal.
// The customer's bot returning a 4xx is misconfiguration we cannot fix by
// retrying; abandon immediately.
func TestStatusClient_DoesNotRetryOn4xx(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404} {
		code := code
		t.Run(http.StatusText(code), func(t *testing.T) {
			var attempts atomic.Int32
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.WriteHeader(code)
			}))
			defer srv.Close()
			c := testStatusClient(t, srv, "12345")
			if err := c.Enqueue("CAtest", sampleEvent(srv.URL+"/cb", "CAtest")); err != nil {
				t.Fatal(err)
			}
			if err := c.DrainAndClose("CAtest", 5*time.Second); err != nil {
				t.Fatal(err)
			}
			if got := attempts.Load(); got != 1 {
				t.Errorf("attempts on %d = %d, want 1 (no retry on 4xx)", code, got)
			}
		})
	}
}

// TestStatusClient_AbandonsAfterMaxRetries — worker continues after
// abandoning a job. Persistent 502 → 4 attempts total → exhausted_retries
// metric increment → worker exits cleanly via DrainAndClose.
func TestStatusClient_AbandonsAfterMaxRetries(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(502)
	}))
	defer srv.Close()
	c := testStatusClient(t, srv, "12345")
	if err := c.Enqueue("CAtest", sampleEvent(srv.URL+"/cb", "CAtest")); err != nil {
		t.Fatal(err)
	}
	if err := c.DrainAndClose("CAtest", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := attempts.Load(); got != 4 {
		t.Errorf("attempts = %d, want 4 (1 + 3 retries)", got)
	}
}

// TestStatusClient_TerminateNotBlockedByCallback — RESEARCH §5.3 / Pitfall 6.
// The customer's flapping callback host MUST NOT block call cleanup. The
// worker uses context.Background() (NOT sessionCtx), so DrainAndClose can
// give up at its own timeout without waiting for retries to finish.
func TestStatusClient_TerminateNotBlockedByCallback(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)

	c := testStatusClient(t, srv, "12345")
	if err := c.Enqueue("CAtest", sampleEvent(srv.URL+"/cb", "CAtest")); err != nil {
		t.Fatal(err)
	}

	// DrainAndClose with a tiny timeout — worker is stuck on attempt 1.
	// Expected: DeadlineExceeded; elapsed ≈ drainTimeout (NOT the 4s per-
	// attempt budget or the 23s full retry chain).
	start := time.Now()
	err := c.DrainAndClose("CAtest", 200*time.Millisecond)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("DrainAndClose: err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("DrainAndClose elapsed = %v, want <500ms (caller must not block)", elapsed)
	}
}

// TestStatusClient_DrainAndClose — happy-path drain. Three jobs against a
// fast 200-OK server all deliver; DrainAndClose returns nil.
func TestStatusClient_DrainAndClose(t *testing.T) {
	var delivered atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delivered.Add(1)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	c := testStatusClient(t, srv, "12345")
	for i := 0; i < 3; i++ {
		if err := c.Enqueue("CAtest", sampleEvent(srv.URL+"/cb", "CAtest")); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.DrainAndClose("CAtest", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := delivered.Load(); got != 3 {
		t.Errorf("delivered = %d, want 3", got)
	}
}

// TestStatusCallback_NoBlastRadiusOnUrlFetch — a flapping status-callback
// host must not raise voiceWC.Fetch p99 latency (proving separate
// Transport pools).
//
// Skipped under -short; carries explicit comment for future readers.
func TestStatusCallback_NoBlastRadiusOnUrlFetch(t *testing.T) {
	if testing.Short() {
		t.Skip("blast-radius test slow")
	}

	// 1. Flapping status server (always sleeps until test cleanup).
	flapBlock := make(chan struct{})
	flapping := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-flapBlock:
		case <-r.Context().Done():
		}
	}))
	defer flapping.Close()
	defer close(flapBlock)

	// 2. Fast Url= server (webhookFetcher target).
	fast := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<Response><Hangup/></Response>`))
	}))
	defer fast.Close()

	// Spam the flapping server via StatusClient (queue all 64 slots).
	sc := testStatusClient(t, flapping, "12345")
	for i := 0; i < 64; i++ {
		_ = sc.Enqueue("CAblast", sampleEvent(flapping.URL+"/cb", "CAblast"))
	}

	// Concurrent voiceWC.Fetch calls against the fast server using a
	// fresh Client (separate Transport pool by construction).
	voiceWC := newClientWithTransport(fast.Client().Transport)
	const N = 100
	latencies := make([]time.Duration, N)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			start := time.Now()
			_, err := voiceWC.Fetch(context.Background(), FetchTarget{URL: fast.URL + "/twiml", Method: "GET"})
			latencies[i] = time.Since(start)
			if err != nil {
				t.Errorf("Fetch %d: %v", i, err)
			}
		}()
	}
	wg.Wait()

	// Compute p99 on a sorted copy.
	sortedDurations := make([]time.Duration, N)
	copy(sortedDurations, latencies)
	sort.Slice(sortedDurations, func(i, j int) bool { return sortedDurations[i] < sortedDurations[j] })
	p99 := sortedDurations[(N*99)/100]
	if p99 > 100*time.Millisecond {
		t.Errorf("p99 = %v, want <100ms — transports may be shared (blast-radius violation)", p99)
	}

	_ = sc.DrainAndClose("CAblast", 100*time.Millisecond)
}

// TestStatusClient_VerbatimURLSigning — Customer URL with literal %20 —
// req.URL.String() in Go normalizes some percent-encoded sequences in
// some code paths, which would break the signature. The SignWithContext
// seam prevents this. End-to-end proof.
func TestStatusClient_VerbatimURLSigning(t *testing.T) {
	var capturedSig atomic.Value
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedSig.Store(r.Header.Get("X-Twilio-Signature"))
		w.WriteHeader(200)
	}))
	defer srv.Close()
	c := testStatusClient(t, srv, "12345")

	cbURL := srv.URL + "/cb%20path"
	evt := sampleEvent(cbURL, "CAtest")

	if err := c.Enqueue("CAtest", evt); err != nil {
		t.Fatal(err)
	}
	if err := c.DrainAndClose("CAtest", 5*time.Second); err != nil {
		t.Fatal(err)
	}

	gotSig, _ := capturedSig.Load().(string)
	wantSig := Sign("12345", cbURL, evt.Form) // sign the literal URL bytes
	if gotSig != wantSig {
		t.Errorf("X-Twilio-Signature: got %q want %q (URL verbatim broken — req.URL.String() regression)", gotSig, wantSig)
	}
}

// TestStatusClient_RejectsSSRFAtEnqueue — pre-flight ValidateCallbackURL
// guard. Literal-localhost is rejected before the URL ever reaches the
// queue.
func TestStatusClient_RejectsSSRFAtEnqueue(t *testing.T) {
	c := NewStatusClient("12345", observability.NewMetrics(), zerolog.Nop())
	evt := sampleEvent("https://localhost/cb", "CAtest")
	err := c.Enqueue("CAtest", evt)
	if !errors.Is(err, ErrSSRFRejected) {
		t.Errorf("Enqueue with localhost URL: err = %v, want ErrSSRFRejected", err)
	}
}

// TestStatusClient_NilMetricsIsSafe — defense-in-depth: the StatusClient
// must not panic when Metrics is nil. Allows tests to run without the
// observability counters wired.
func TestStatusClient_NilMetricsIsSafe(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	c := newStatusClientWithTransport(srv.Client().Transport, "12345", nil, zerolog.Nop())
	if err := c.Enqueue("CAtest", sampleEvent(srv.URL+"/cb", "CAtest")); err != nil {
		t.Fatal(err)
	}
	_ = c.DrainAndClose("CAtest", 5*time.Second)
	// No panic = pass.
}

// TestStatusClient_AuthTokenNeverLogged (I-7) — the AuthToken bytes MUST NEVER
// appear in any zerolog field call. Drives BOTH a successful delivery AND a
// failed (5xx → exhausted-retries) delivery so warning/error log paths are
// exercised. A unique marker token allows substring-search of the captured
// zerolog output buffer with zero false-positive risk.
func TestStatusClient_AuthTokenNeverLogged(t *testing.T) {
	const marker = "UNIQUE-MARKER-XYZ123-AUTH-TOKEN-DO-NOT-LOG"
	var buf bytes.Buffer
	log := zerolog.New(&buf).With().Timestamp().Logger()

	// Step 1: successful delivery against a fast server.
	okSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(200)
	}))
	defer okSrv.Close()
	sc := newStatusClientWithTransport(okSrv.Client().Transport, marker, nil, log)
	if err := sc.Enqueue("CAok", sampleEvent(okSrv.URL+"/cb", "CAok")); err != nil {
		t.Fatal(err)
	}
	_ = sc.DrainAndClose("CAok", 5*time.Second)

	// Step 2: persistent 5xx → exhausted_retries (exercises warn paths).
	failSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer failSrv.Close()
	sc2 := newStatusClientWithTransport(failSrv.Client().Transport, marker, nil, log)
	if err := sc2.Enqueue("CAfail", sampleEvent(failSrv.URL+"/cb", "CAfail")); err != nil {
		t.Fatal(err)
	}
	_ = sc2.DrainAndClose("CAfail", 30*time.Second)

	if got := buf.String(); strings.Contains(got, marker) {
		t.Fatalf("AuthToken marker leaked into zerolog output:\n%s", got)
	}
}
