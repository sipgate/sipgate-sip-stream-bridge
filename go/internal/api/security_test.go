package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/config"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/observability"
)

// TestSecurityHeaders_HSTS_CSP_Nosniff_Present asserts the three locked
// security headers (HSTS, CSP, X-Content-Type-Options) are emitted on every
// response when SecurityHeaders() wraps a handler. Pins the locked-by-CONTEXT
// values byte-exactly so a future tweak surfaces here.
func TestSecurityHeaders_HSTS_CSP_Nosniff_Present(t *testing.T) {
	t.Parallel()

	mw := SecurityHeaders()
	noop := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	noop.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if got, want := rec.Header().Get("Strict-Transport-Security"), "max-age=63072000; includeSubDomains"; got != want {
		t.Fatalf("Strict-Transport-Security: got %q, want %q", got, want)
	}
	if got, want := rec.Header().Get("Content-Security-Policy"), "default-src 'none'; frame-ancestors 'none'"; got != want {
		t.Fatalf("Content-Security-Policy: got %q, want %q", got, want)
	}
	if got, want := rec.Header().Get("X-Content-Type-Options"), "nosniff"; got != want {
		t.Fatalf("X-Content-Type-Options: got %q, want %q", got, want)
	}
}

// TestSecurityHeaders_PresentOn401 mounts the api router with BasicAuth-
// failing creds and asserts HSTS/CSP/nosniff are STILL present on the 401
// response. This proves SecurityHeaders middleware fires BEFORE BasicAuth —
// the load-bearing ordering invariant from the plan's Mount() change.
func TestSecurityHeaders_PresentOn401(t *testing.T) {
	t.Parallel()

	q := newStubBridge()
	r := chi.NewRouter()
	m := observability.NewMetrics()
	Mount(r, q, nil, srvSid, srvAuthToken, m, nilWebhookFetcher{}, testActionPoster(""), zerolog.Nop(), nil, config.Config{})

	// NO Authorization header — must 401.
	req := httptest.NewRequest(http.MethodGet, "/2010-04-01/Accounts/"+srvSid+"/Calls.json", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401 (this test depends on the 401 path)", rec.Code)
	}
	if got := rec.Header().Get("Strict-Transport-Security"); got == "" {
		t.Fatalf("HSTS header missing on 401 — SecurityHeaders must fire before BasicAuth")
	}
	if got := rec.Header().Get("Content-Security-Policy"); got == "" {
		t.Fatalf("CSP header missing on 401 — SecurityHeaders must fire before BasicAuth")
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options on 401: got %q, want \"nosniff\"", got)
	}
}

// TestMaxBytesReader_ContentLength_Over64KB_Returns413_TwilioJSON drives the
// Tier-1 (Content-Length pre-check) path: a request declaring CL > limit is
// rejected with 413 + Twilio-shaped JSON body BEFORE any body bytes are read.
//
// Verified at the middleware layer (no Mount/BasicAuth involvement) so the
// assertion isolates MaxBytesReader's behavior from auth/route concerns.
func TestMaxBytesReader_ContentLength_Over64KB_Returns413_TwilioJSON(t *testing.T) {
	t.Parallel()

	const limit int64 = 64 << 10 // 65536
	mw := MaxBytesReader(limit)
	// Stub handler — only reached if the middleware lets the request through.
	// In the over-limit test it must NOT be reached; if it is, the Reach flag
	// surfaces in the failure message.
	var reachedHandler bool
	stub := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reachedHandler = true
		w.WriteHeader(http.StatusOK)
	}))

	body := bytes.NewReader(make([]byte, 100_000))
	req := httptest.NewRequest(http.MethodPost, "/", body)
	req.ContentLength = 100_000 // pre-declared oversize

	rec := httptest.NewRecorder()
	stub.ServeHTTP(rec, req)

	if reachedHandler {
		t.Fatalf("downstream handler reached — Tier-1 should reject pre-declared oversize without forwarding")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: got %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type: got %q, want application/json", got)
	}

	var errBody Error
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decode Twilio error body: %v\nraw=%s", err, rec.Body.String())
	}
	if errBody.Code != 21617 {
		t.Fatalf("error code: got %d, want 21617", errBody.Code)
	}
	if errBody.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("error status: got %d, want 413", errBody.Status)
	}
	if !strings.Contains(errBody.Message, "64KB") {
		t.Fatalf("error message: got %q, want it to contain \"64KB\"", errBody.Message)
	}
}

// TestMaxBytesReader_ContentLength_AtBoundary_64KB_Allowed pins the boundary:
// a request with body exactly 64KB (and matching Content-Length) is NOT
// rejected — Tier 1 uses strict `>` not `>=`, and Tier 2's MaxBytesReader
// allows exactly `limit` bytes through.
func TestMaxBytesReader_ContentLength_AtBoundary_64KB_Allowed(t *testing.T) {
	t.Parallel()

	const limit int64 = 64 << 10
	mw := MaxBytesReader(limit)

	var bytesRead int64
	stub := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the body so the assertion below proves the 64KB boundary
		// passes Tier 2 (http.MaxBytesReader allows exactly `limit` bytes).
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			t.Errorf("body read at boundary: unexpected error: %v", err)
		}
		bytesRead = n
		w.WriteHeader(http.StatusOK)
	}))

	body := bytes.NewReader(make([]byte, limit))
	req := httptest.NewRequest(http.MethodPost, "/", body)
	req.ContentLength = limit

	rec := httptest.NewRecorder()
	stub.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status at 64KB boundary: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if bytesRead != limit {
		t.Fatalf("bytes read at 64KB boundary: got %d, want %d", bytesRead, limit)
	}
}

// TestMaxBytesReader_ChunkedOversize_BodyTruncatedAndHandlerSees4xx pins the
// load-bearing Tier-2 invariant: a chunked-encoded request with NO (or
// fraudulent) Content-Length AND a body > limit must be bounded by the
// http.MaxBytesReader body wrap. Without Tier 2, an attacker bypasses Tier 1
// trivially via Transfer-Encoding: chunked with no Content-Length.
//
// Deliberate-break verification (manual): removing the
// `r.Body = http.MaxBytesReader(...)` line from MaxBytesReader makes this test
// fail because io.ReadAll(r.Body) consumes the full unbounded body without
// surfacing *http.MaxBytesError.
func TestMaxBytesReader_ChunkedOversize_BodyTruncatedAndHandlerSees4xx(t *testing.T) {
	t.Parallel()

	const limit int64 = 64 << 10

	// Hand-craft a request with NO Content-Length so Tier 1 cannot help.
	// httptest.NewRequest sets ContentLength from a *bytes.Reader; we override
	// it back to -1 to simulate Transfer-Encoding: chunked with unknown length.
	oversize := bytes.NewReader(make([]byte, 100_000))
	req := httptest.NewRequest(http.MethodPost, "/", oversize)
	req.ContentLength = -1
	req.TransferEncoding = []string{"chunked"}

	mw := MaxBytesReader(limit)

	var (
		gotErr    error
		bytesRead int64
	)
	stub := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read until the bound trips. ReadAll returns the *http.MaxBytesError
		// once consumption exceeds limit.
		buf, err := io.ReadAll(r.Body)
		bytesRead = int64(len(buf))
		gotErr = err
		// Emulate the production downstream behavior: surface a 4xx on read
		// failure. The exact 4xx vs 413 split depends on caller code; here we
		// only need to assert the middleware-level bound trips.
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	stub.ServeHTTP(rec, req)

	// Tier-2 invariant: an *http.MaxBytesError surfaces when the bound trips.
	var maxErr *http.MaxBytesError
	if !errors.As(gotErr, &maxErr) {
		t.Fatalf("Tier-2 MaxBytesReader bound did not trip: gotErr=%v (want *http.MaxBytesError); bytesRead=%d limit=%d",
			gotErr, bytesRead, limit)
	}
	if maxErr.Limit != limit {
		t.Fatalf("MaxBytesError.Limit: got %d, want %d", maxErr.Limit, limit)
	}
	// The handler emitted a 4xx because the body read failed — the production
	// asymmetry (chunked oversize → 4xx, NOT 413) is documented in the
	// MaxBytesReader godoc and the threat register.
	if rec.Code < 400 || rec.Code >= 500 {
		t.Fatalf("downstream handler observed status %d on chunked-oversize: want a 4xx", rec.Code)
	}
	// Sanity: the body we managed to read must be <= limit (MaxBytesReader is
	// inclusive: it allows exactly `limit` bytes through and errors on the
	// `limit+1`th).
	if bytesRead > limit {
		t.Fatalf("bytesRead %d > limit %d — Tier 2 did not bound the body", bytesRead, limit)
	}
}

// TestMaxBytesReader_ContentLength_Unknown_TierTwoOnly_413NotEmitted documents
// the asymmetry between Tier-1 and Tier-2 paths so future readers don't expect
// Tier-2 to also emit a 413. Tier 1 emits 413 when ContentLength > limit;
// Tier 2 only WRAPS r.Body so the downstream handler observes a
// *http.MaxBytesError on read. The 413 emit lives at Tier 1; chunked-oversize
// requests get the downstream handler's error path (typically 400
// invalid_params), not 413. The threat register documents this asymmetry
// as accepted.
func TestMaxBytesReader_ContentLength_Unknown_TierTwoOnly_413NotEmitted(t *testing.T) {
	t.Parallel()

	const limit int64 = 64 << 10

	// CL = -1 (unknown). Body exactly at limit so Tier 2 does not trip.
	body := bytes.NewReader(make([]byte, limit))
	req := httptest.NewRequest(http.MethodPost, "/", body)
	req.ContentLength = -1

	mw := MaxBytesReader(limit)
	stub := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	stub.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("CL=-1 + body at limit: got status %d, want 200", rec.Code)
	}
	// Tier 1 must NOT emit 413 when CL is -1 / unknown — it only fires when
	// the pre-declared ContentLength STRICTLY exceeds the limit.
	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("Tier 1 incorrectly emitted 413 on CL=-1 (unknown-length); only Tier 2 should bound this case")
	}
}
