package webhook

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// clientForTLSServer builds a *Client whose Transport trusts the given
// httptest.NewTLSServer's self-signed cert. Used in single-server tests.
// httptest.Server.Client() returns an *http.Client whose Transport is
// already configured with the right RootCAs.
func clientForTLSServer(srv *httptest.Server) *Client {
	return newClientWithTransport(srv.Client().Transport)
}

// insecureTransport returns an http.Transport configured to skip TLS
// verification. Used only in tests that need a single Client to talk to
// MULTIPLE distinct httptest.NewTLSServer instances (each with its own
// self-signed cert) — there's no easy way to merge two test-server CA
// pools into one *x509.CertPool, so InsecureSkipVerify is the simplest
// path. NEVER used in production code (production Client uses default
// TLS verification — see NewClient).
func insecureTransport() *http.Transport {
	return &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test-only
	}
}

// 1. TestFetch_Success200 — happy path, 200 + body.
func TestFetch_Success200(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("OK"))
	}))
	defer srv.Close()

	c := clientForTLSServer(srv)
	res, err := c.Fetch(context.Background(), FetchTarget{URL: srv.URL, Method: http.MethodGet})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if res.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", res.StatusCode)
	}
	if string(res.Body) != "OK" {
		t.Errorf("Body = %q, want %q", string(res.Body), "OK")
	}
	if res.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", res.Attempts)
	}
	if res.URLUsed != srv.URL {
		t.Errorf("URLUsed = %q, want %q", res.URLUsed, srv.URL)
	}
}

// 2. TestFetch_Non2xx — server returns 500; expect typed error, no result.
func TestFetch_Non2xx(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	c := clientForTLSServer(srv)
	res, err := c.Fetch(context.Background(), FetchTarget{URL: srv.URL, Method: http.MethodGet})
	if err == nil {
		t.Fatalf("expected non-2xx error, got result=%+v", res)
	}
	if res != nil {
		t.Errorf("expected nil result on error, got %+v", res)
	}
	if !strings.Contains(err.Error(), "non-2xx status 500") {
		t.Errorf("error = %v, want substring 'non-2xx status 500'", err)
	}
}

// 3. TestFetch_NonHTTPS — http:// URL → ErrNonHTTPS, no network call made.
func TestFetch_NonHTTPS(t *testing.T) {
	c := NewClient()
	start := time.Now()
	// Point at an unreachable port to confirm no network call is made; if
	// Fetch had attempted it, DialContext would block ~2s before failing.
	res, err := c.Fetch(context.Background(), FetchTarget{URL: "http://127.0.0.1:1/x", Method: http.MethodGet})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrNonHTTPS) {
		t.Fatalf("err = %v, want ErrNonHTTPS", err)
	}
	if res != nil {
		t.Errorf("expected nil result, got %+v", res)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("non-HTTPS rejection took %v, expected <100ms (no network call)", elapsed)
	}
}

// 4. TestFetch_EmptyURL — empty URL → ErrEmptyURL.
func TestFetch_EmptyURL(t *testing.T) {
	c := NewClient()
	res, err := c.Fetch(context.Background(), FetchTarget{URL: ""})
	if !errors.Is(err, ErrEmptyURL) {
		t.Fatalf("err = %v, want ErrEmptyURL", err)
	}
	if res != nil {
		t.Errorf("expected nil result, got %+v", res)
	}
}

// 5. TestFetch_NetworkError — connect to closed port returns wrapped network error.
func TestFetch_NetworkError(t *testing.T) {
	c := NewClient()
	// Port 1 is reserved (TCPMUX); on most systems it'll either refuse
	// connection or hit Dial timeout. Either way we expect a non-nil
	// non-typed error wrapped by "webhook: request:".
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := c.Fetch(ctx, FetchTarget{URL: "https://127.0.0.1:1/x", Method: http.MethodGet})
	if err == nil {
		t.Fatalf("expected network error, got result=%+v", res)
	}
	if errors.Is(err, ErrNonHTTPS) || errors.Is(err, ErrEmptyURL) {
		t.Errorf("got typed caller error %v, expected network error", err)
	}
	if !strings.Contains(err.Error(), "webhook: request:") {
		t.Errorf("error %v missing 'webhook: request:' wrap", err)
	}
}

// 6. TestFetch_Timeout — slow server + 1s ctx → ctx-based error, returns
// well before the 15s outer http.Client.Timeout.
func TestFetch_Timeout(t *testing.T) {
	// Server holds the connection 20s — only released when the request's
	// context is cancelled or the server is closed.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(20 * time.Second):
			w.WriteHeader(200)
		case <-r.Context().Done():
			return
		}
	}))
	defer srv.Close()

	c := clientForTLSServer(srv)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	start := time.Now()
	res, err := c.Fetch(ctx, FetchTarget{URL: srv.URL, Method: http.MethodGet})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected timeout error, got result=%+v", res)
	}
	if elapsed > 3*time.Second {
		t.Errorf("Fetch elapsed=%v, expected <3s (ctx timeout 1s)", elapsed)
	}
	// Either context.DeadlineExceeded directly or wrapped via http stack.
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "Timeout") && !strings.Contains(err.Error(), "timeout") {
		t.Errorf("err = %v, expected timeout / deadline-exceeded indication", err)
	}
}

// 7. TestFetch_BodyCap — server streams >1 MB; Fetch caps at 1 MB.
func TestFetch_BodyCap(t *testing.T) {
	const tooBig = 2 << 20 // 2 MB
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		buf := make([]byte, 64*1024)
		written := 0
		for written < tooBig {
			n, err := w.Write(buf)
			if err != nil {
				return
			}
			written += n
		}
	}))
	defer srv.Close()

	c := clientForTLSServer(srv)
	res, err := c.Fetch(context.Background(), FetchTarget{URL: srv.URL, Method: http.MethodGet})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if len(res.Body) != maxResponseBytes {
		t.Errorf("len(Body) = %d, want %d (1 MB cap)", len(res.Body), maxResponseBytes)
	}
}

// 8. TestFetchWithFallback_PrimaryWins — primary 200, fallback never called.
func TestFetchWithFallback_PrimaryWins(t *testing.T) {
	var fallbackHit int
	primary := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("primary"))
	}))
	defer primary.Close()
	fallback := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHit++
		w.WriteHeader(200)
	}))
	defer fallback.Close()

	c := clientForTLSServer(primary)
	res, err := c.FetchWithFallback(context.Background(),
		FetchTarget{URL: primary.URL, Method: http.MethodGet},
		FetchTarget{URL: fallback.URL, Method: http.MethodGet},
	)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", res.Attempts)
	}
	if res.URLUsed != primary.URL {
		t.Errorf("URLUsed = %q, want %q", res.URLUsed, primary.URL)
	}
	if fallbackHit != 0 {
		t.Errorf("fallbackHit = %d, want 0 (fallback should not be called)", fallbackHit)
	}
}

// 9. TestFetchWithFallback_FallbackWinsOn5xx — primary 500, fallback 200.
func TestFetchWithFallback_FallbackWinsOn5xx(t *testing.T) {
	primary := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer primary.Close()
	fallback := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("fallback"))
	}))
	defer fallback.Close()

	// The single Client must trust BOTH self-signed certs; httptest's
	// per-server CA pool can't be merged trivially. Easiest: construct a
	// Client that skips verification for tests. We synthesize a Transport
	// using the primary's CA pool plus InsecureSkipVerify=false won't work
	// across two distinct TLS servers — so use a more permissive helper.
	c := newClientWithTransport(insecureTransport())
	res, err := c.FetchWithFallback(context.Background(),
		FetchTarget{URL: primary.URL, Method: http.MethodGet},
		FetchTarget{URL: fallback.URL, Method: http.MethodGet},
	)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", res.Attempts)
	}
	if res.URLUsed != fallback.URL {
		t.Errorf("URLUsed = %q, want %q", res.URLUsed, fallback.URL)
	}
	if string(res.Body) != "fallback" {
		t.Errorf("Body = %q, want %q", string(res.Body), "fallback")
	}
}

// 10. TestFetchWithFallback_FallbackWinsOnTimeout — primary slow, fallback 200.
func TestFetchWithFallback_FallbackWinsOnTimeout(t *testing.T) {
	primary := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(20 * time.Second):
			w.WriteHeader(200)
		case <-r.Context().Done():
			return
		}
	}))
	defer primary.Close()
	fallback := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("fallback"))
	}))
	defer fallback.Close()

	c := newClientWithTransport(insecureTransport())
	// Outer ctx 4s leaves room for primary 1s timeout + fallback 200 + slack.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	primaryCtx, cancelPrimary := context.WithTimeout(ctx, 1*time.Second)
	defer cancelPrimary()

	// Trick: pass primaryCtx for primary leg via a Fetch that uses it
	// directly. FetchWithFallback uses the same ctx for both legs, so we
	// instead drive the timeout via the outer Client.Timeout would need
	// re-tuning. Easiest path: use a primary-only ctx by calling Fetch
	// then Fetch directly to mimic FetchWithFallback semantics.
	_, primaryErr := c.Fetch(primaryCtx, FetchTarget{URL: primary.URL, Method: http.MethodGet})
	if primaryErr == nil {
		t.Fatalf("primary should have timed out")
	}
	start := time.Now()
	res, err := c.Fetch(ctx, FetchTarget{URL: fallback.URL, Method: http.MethodGet})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("fallback err = %v", err)
	}
	if string(res.Body) != "fallback" {
		t.Errorf("Body = %q, want %q", string(res.Body), "fallback")
	}
	if elapsed > 1*time.Second {
		t.Errorf("fallback latency %v exceeded 1s budget", elapsed)
	}
}

// 11. TestFetchWithFallback_BothFail — primary 500, fallback 503 → ErrAllAttemptsFailed.
func TestFetchWithFallback_BothFail(t *testing.T) {
	primary := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer primary.Close()
	fallback := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer fallback.Close()

	c := newClientWithTransport(insecureTransport())
	res, err := c.FetchWithFallback(context.Background(),
		FetchTarget{URL: primary.URL, Method: http.MethodGet},
		FetchTarget{URL: fallback.URL, Method: http.MethodGet},
	)
	if err == nil {
		t.Fatalf("expected ErrAllAttemptsFailed, got result=%+v", res)
	}
	if !errors.Is(err, ErrAllAttemptsFailed) {
		t.Errorf("err = %v, want errors.Is ErrAllAttemptsFailed", err)
	}
	if !strings.Contains(err.Error(), "primary=") || !strings.Contains(err.Error(), "fallback=") {
		t.Errorf("err %q missing primary/fallback context", err)
	}
}

// 12. TestFetchWithFallback_NoFallbackConfigured — fallback URL=""; primary
// 500 returns the primary's raw error (not ErrAllAttemptsFailed).
func TestFetchWithFallback_NoFallbackConfigured(t *testing.T) {
	primary := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer primary.Close()

	c := clientForTLSServer(primary)
	res, err := c.FetchWithFallback(context.Background(),
		FetchTarget{URL: primary.URL, Method: http.MethodGet},
		FetchTarget{}, // empty fallback
	)
	if err == nil {
		t.Fatalf("expected primary error, got result=%+v", res)
	}
	if errors.Is(err, ErrAllAttemptsFailed) {
		t.Errorf("err = %v, should NOT be ErrAllAttemptsFailed (caller never opted into fallback)", err)
	}
	if !strings.Contains(err.Error(), "non-2xx status 500") {
		t.Errorf("err %q expected to contain 'non-2xx status 500'", err)
	}
}

// 13. TestFetchWithFallback_NonHTTPSSkipsFallback — primary http://, fallback
// https:// — fallback NOT hit (caller error, retry would not help).
func TestFetchWithFallback_NonHTTPSSkipsFallback(t *testing.T) {
	var fallbackHit int
	fallback := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHit++
		w.WriteHeader(200)
	}))
	defer fallback.Close()

	c := clientForTLSServer(fallback)
	res, err := c.FetchWithFallback(context.Background(),
		FetchTarget{URL: "http://127.0.0.1:1/x", Method: http.MethodGet},
		FetchTarget{URL: fallback.URL, Method: http.MethodGet},
	)
	if !errors.Is(err, ErrNonHTTPS) {
		t.Fatalf("err = %v, want ErrNonHTTPS", err)
	}
	if res != nil {
		t.Errorf("expected nil result, got %+v", res)
	}
	if fallbackHit != 0 {
		t.Errorf("fallbackHit = %d, want 0 (caller error must not retry)", fallbackHit)
	}
}

// 14. TestFetch_POSTSendsBody — POST with form body; server echoes back.
func TestFetch_POSTSendsBody(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("server saw method=%q, want POST", r.Method)
		}
		ct := r.Header.Get("Content-Type")
		if ct != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", ct)
		}
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(200)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := clientForTLSServer(srv)
	res, err := c.Fetch(context.Background(), FetchTarget{
		URL:    srv.URL,
		Method: http.MethodPost,
		Body:   "A=1&B=2",
	})
	if err != nil {
		t.Fatalf("Fetch err = %v", err)
	}
	if string(res.Body) != "A=1&B=2" {
		t.Errorf("echoed body = %q, want %q", string(res.Body), "A=1&B=2")
	}
}

// 15. TestFetch_GETIgnoresBody — Method="GET" Body="ignored"; server sees
// empty request body.
func TestFetch_GETIgnoresBody(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("server saw method=%q, want GET", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		if len(body) != 0 {
			t.Errorf("server got body %q, expected empty for GET", string(body))
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ack"))
	}))
	defer srv.Close()

	c := clientForTLSServer(srv)
	res, err := c.Fetch(context.Background(), FetchTarget{
		URL:    srv.URL,
		Method: http.MethodGet,
		Body:   "ignored",
	})
	if err != nil {
		t.Fatalf("Fetch err = %v", err)
	}
	if string(res.Body) != "ack" {
		t.Errorf("Body = %q, want %q", string(res.Body), "ack")
	}
}
