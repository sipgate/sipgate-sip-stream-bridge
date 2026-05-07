// Package webhook provides outbound HTTP clients for v3.0's three distinct
// outbound surfaces: Url= TwiML fetches, <Dial> action callbacks, and
// status callbacks. Each surface uses its own Client instance (separate
// http.Transport, separate idle pool) so failures on one don't degrade
// the others.
package webhook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// FetchTarget describes a single webhook target.
type FetchTarget struct {
	URL    string
	Method string // "GET" | "POST"; default "POST" if empty
	Body   string // form-encoded body, ignored for GET
}

// FetchResult is the successful outcome of a Fetch / FetchWithFallback call.
type FetchResult struct {
	Body       []byte
	StatusCode int
	URLUsed    string // primary or fallback (whichever returned 2xx)
	Attempts   int    // 1 = primary won, 2 = fallback won
}

// Typed errors. Callers in internal/api translate to Twilio error codes
// (e.g. ErrNonHTTPS → 11200/21218; ErrAllAttemptsFailed → 11200).
var (
	ErrNonHTTPS          = errors.New("webhook: non-https URL not permitted")
	ErrAllAttemptsFailed = errors.New("webhook: all attempts failed")
	ErrEmptyURL          = errors.New("webhook: empty URL")
)

// maxResponseBytes caps a single response body at 1 MB. TwiML responses are
// kilobytes at most; the cap is a defensive guard against a hostile fallback
// URL streaming an unbounded body.
const maxResponseBytes = 1 << 20

// Client wraps an http.Client tuned for webhook fetches. Each instance owns
// its own Transport (separate connection pool) — never share a Client across
// unrelated outbound surfaces (Url= fetch vs status callback vs <Dial> action).
type Client struct {
	http *http.Client
}

// NewClient constructs a Client with the v3.0 default timeout budget:
//
//   - Transport.DialContext             2s   (fail fast on unreachable hosts)
//   - Transport.TLSHandshakeTimeout     3s
//   - Transport.ResponseHeaderTimeout   8s
//   - http.Client.Timeout              15s   (outer cap; safety net)
//   - Transport.MaxIdleConnsPerHost     4   (low-volume use)
//   - Transport.IdleConnTimeout        30s
//
// Per-stage timeouts prevent any single stage (e.g. TLS handshake stuck) from
// monopolizing the entire 15s outer budget.
func NewClient() *Client {
	tr := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   2 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   3 * time.Second,
		ResponseHeaderTimeout: 8 * time.Second,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       30 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &Client{
		http: &http.Client{
			Transport: tr,
			Timeout:   15 * time.Second,
		},
	}
}

// newClientWithTransport is a test-only constructor that lets tests inject
// the TLS configuration of an httptest.NewTLSServer (whose self-signed cert
// would otherwise fail production-grade verification). Not part of the
// public API; called only from client_test.go in the same package.
func newClientWithTransport(tr http.RoundTripper) *Client {
	return &Client{
		http: &http.Client{
			Transport: tr,
			Timeout:   15 * time.Second,
		},
	}
}

// Fetch executes a single attempt against t.URL. Returns a typed error on
// non-2xx, network failure, or timeout. The response body is capped at 1 MB
// (see maxResponseBytes). HTTPS is enforced; ErrNonHTTPS is returned (no
// network call made) if t.URL does not begin with "https://".
func (c *Client) Fetch(ctx context.Context, t FetchTarget) (*FetchResult, error) {
	if t.URL == "" {
		return nil, ErrEmptyURL
	}
	if !strings.HasPrefix(t.URL, "https://") {
		return nil, ErrNonHTTPS
	}
	method := t.Method
	if method == "" {
		method = http.MethodPost
	}

	var body io.Reader
	if method == http.MethodPost && t.Body != "" {
		body = strings.NewReader(t.Body)
	}

	req, err := http.NewRequestWithContext(ctx, method, t.URL, body)
	if err != nil {
		return nil, fmt.Errorf("webhook: build request: %w", err)
	}
	if method == http.MethodPost && t.Body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Accept", "application/xml, text/xml, */*")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("webhook: request: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("webhook: read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("webhook: non-2xx status %d", resp.StatusCode)
	}
	return &FetchResult{
		Body:       bodyBytes,
		StatusCode: resp.StatusCode,
		URLUsed:    t.URL,
		Attempts:   1,
	}, nil
}

// FetchWithFallback attempts primary first; on any error (non-2xx, timeout,
// network), retries against fallback (if non-empty). Returns the successful
// result with Attempts=1 (primary won) or Attempts=2 (fallback won), or
// ErrAllAttemptsFailed wrapping both errors if both fail.
//
// Fallback is SKIPPED on caller errors — ErrEmptyURL and ErrNonHTTPS — since
// retrying would not change the input. If fallback.URL is "", only primary
// is attempted and its raw error is returned (caller never opted in to a
// fallback, so ErrAllAttemptsFailed would be misleading).
func (c *Client) FetchWithFallback(ctx context.Context, primary, fallback FetchTarget) (*FetchResult, error) {
	primaryResult, primaryErr := c.Fetch(ctx, primary)
	if primaryErr == nil {
		return primaryResult, nil
	}
	// Caller errors — don't retry.
	if errors.Is(primaryErr, ErrEmptyURL) || errors.Is(primaryErr, ErrNonHTTPS) {
		return nil, primaryErr
	}
	// No fallback configured — surface the primary error directly.
	if fallback.URL == "" {
		return nil, primaryErr
	}
	fallbackResult, fallbackErr := c.Fetch(ctx, fallback)
	if fallbackErr == nil {
		fallbackResult.Attempts = 2
		return fallbackResult, nil
	}
	return nil, fmt.Errorf("%w: primary=%v fallback=%v", ErrAllAttemptsFailed, primaryErr, fallbackErr)
}
