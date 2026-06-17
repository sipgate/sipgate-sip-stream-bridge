package webhook

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/observability"
)

// VoiceParams holds the Twilio-compatible form fields posted to VOICE_URL on
// each inbound INVITE. Matches the subset of Twilio's voice webhook parameters
// that are knowable before the call is answered.
type VoiceParams struct {
	CallSid    string
	AccountSid string
	From       string
	To         string
	PublicBaseURL string // used for X-Twilio-Signature URL reconstruction
}

// VoiceClient fetches the Voice-URL webhook and returns the raw TwiML response
// body. Separate http.Transport from other webhook surfaces so a slow customer
// voice-URL endpoint cannot degrade status-callback or Url= fetch latency.
//
// HTTPS is required for non-localhost URLs. http://localhost and
// http://127.0.0.1 are allowed for local development — the same bypass used
// for STATUS_CALLBACK_DEFAULT_URL.
type VoiceClient struct {
	primaryURL   string
	primaryMethod string
	fallbackURL   string
	fallbackMethod string
	authToken    string
	http         *http.Client // SSRF-guarded; used for non-localhost https:// URLs
	httpDev      *http.Client // plain dialer; used for localhost http:// URLs
	metrics      *observability.Metrics
	log          zerolog.Logger
}

// NewVoiceClient constructs a VoiceClient for the given primary and optional
// fallback VOICE_URL configuration. timeoutS caps the total time per attempt
// (not counting fallback).
func NewVoiceClient(
	primaryURL, primaryMethod, fallbackURL, fallbackMethod string,
	authToken string,
	timeoutS int,
	m *observability.Metrics,
	log zerolog.Logger,
) *VoiceClient {
	timeout := time.Duration(timeoutS) * time.Second

	// SSRF-guarded transport for public https:// URLs.
	guarded := &http.Transport{
		DialContext:           newSSRFGuard().DialContext,
		TLSHandshakeTimeout:   3 * time.Second,
		ResponseHeaderTimeout: timeout,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
		ForceAttemptHTTP2:     true,
	}

	// Plain transport for http://localhost (dev/test environments).
	plain := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   2 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: timeout,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
	}

	return &VoiceClient{
		primaryURL:     primaryURL,
		primaryMethod:  primaryMethod,
		fallbackURL:    fallbackURL,
		fallbackMethod: fallbackMethod,
		authToken:      authToken,
		http: &http.Client{
			Transport: guarded,
			Timeout:   timeout,
		},
		httpDev: &http.Client{
			Transport: plain,
			Timeout:   timeout,
		},
		metrics: m,
		log:     log,
	}
}

// Fetch POSTs call metadata to the configured VOICE_URL and returns the raw
// TwiML response body. On HTTP 5xx or network error, tries VOICE_FALLBACK_URL
// if configured. Returns (body, "primary"|"fallback", nil) on success.
func (c *VoiceClient) Fetch(ctx context.Context, p VoiceParams) ([]byte, string, error) {
	formValues := c.buildForm(p)

	body, err := c.fetchOne(ctx, c.primaryURL, c.primaryMethod, formValues)
	if err == nil {
		if c.metrics != nil {
			c.metrics.VoiceFetchTotal.WithLabelValues("ok").Inc()
		}
		return body, "primary", nil
	}

	c.log.Debug().Err(err).Str("url", c.primaryURL).Msg("voice-url primary fetch failed")

	if c.fallbackURL == "" {
		outcome := httpErrOutcome(err)
		if c.metrics != nil {
			c.metrics.VoiceFetchTotal.WithLabelValues(outcome).Inc()
		}
		return nil, "", err
	}

	body, fallbackErr := c.fetchOne(ctx, c.fallbackURL, c.fallbackMethod, formValues)
	if fallbackErr == nil {
		if c.metrics != nil {
			c.metrics.VoiceFetchTotal.WithLabelValues("fallback_ok").Inc()
		}
		return body, "fallback", nil
	}

	c.log.Debug().Err(fallbackErr).Str("url", c.fallbackURL).Msg("voice-url fallback fetch failed")
	if c.metrics != nil {
		c.metrics.VoiceFetchTotal.WithLabelValues("http_error").Inc()
	}
	return nil, "", fmt.Errorf("%w: primary=%v fallback=%v", ErrAllAttemptsFailed, err, fallbackErr)
}

// fetchOne performs a single HTTP request to targetURL with the given form
// values. Signs the request with X-Twilio-Signature.
func (c *VoiceClient) fetchOne(ctx context.Context, targetURL, method string, form url.Values) ([]byte, error) {
	if targetURL == "" {
		return nil, ErrEmptyURL
	}
	if !isAllowedVoiceURL(targetURL) {
		return nil, ErrNonHTTPS
	}

	var bodyReader io.Reader
	var bodyStr string
	if strings.ToUpper(method) == http.MethodPost {
		bodyStr = form.Encode()
		bodyReader = strings.NewReader(bodyStr)
	}

	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(method), targetURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("voice: build request: %w", err)
	}

	if strings.ToUpper(method) == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Accept", "application/xml, text/xml, */*")

	// Sign with X-Twilio-Signature so the recipient can verify authenticity.
	if c.authToken != "" {
		var sigParams url.Values
		if strings.ToUpper(method) == http.MethodPost {
			sigParams = form
		}
		sig := Sign(c.authToken, targetURL, sigParams)
		req.Header.Set("X-Twilio-Signature", sig)
	}

	client := c.http
	if isLocalhostHTTP(targetURL) {
		client = c.httpDev
	}

	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("voice: timeout: %w", ctx.Err())
		}
		return nil, fmt.Errorf("voice: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("voice: read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("voice: non-2xx status %d", resp.StatusCode)
	}
	return respBody, nil
}

// buildForm constructs the Twilio-compatible POST body for the voice webhook.
func (c *VoiceClient) buildForm(p VoiceParams) url.Values {
	v := url.Values{}
	v.Set("CallSid", p.CallSid)
	v.Set("AccountSid", p.AccountSid)
	v.Set("From", p.From)
	v.Set("To", p.To)
	v.Set("CallStatus", "ringing")
	v.Set("Direction", "inbound")
	v.Set("ApiVersion", "2010-04-01")
	v.Set("ForwardedFrom", "")
	return v
}

// isAllowedVoiceURL returns true if the URL scheme is https:// or if it is
// an http:// URL targeting localhost / 127.0.0.1 (dev bypass).
func isAllowedVoiceURL(u string) bool {
	if strings.HasPrefix(u, "https://") {
		return true
	}
	return isLocalhostHTTP(u)
}

// isLocalhostHTTP returns true for http://localhost... and http://127.0.0.1...
// URLs. These bypass the SSRF guard and HTTPS requirement for local development.
func isLocalhostHTTP(u string) bool {
	if !strings.HasPrefix(u, "http://") {
		return false
	}
	rest := strings.TrimPrefix(u, "http://")
	return strings.HasPrefix(rest, "localhost") || strings.HasPrefix(rest, "127.0.0.1")
}

// metrics:bucketer
//
// httpErrOutcome maps a fetch error to a bounded Prometheus outcome label for
// VoiceFetchTotal. Return values are a strict subset of the allowlist enum
// {ok, timeout, http_error, twiml_error, fallback_ok}.
func httpErrOutcome(err error) string {
	if err == nil {
		return "ok"
	}
	msg := err.Error()
	if strings.Contains(msg, "timeout") || strings.Contains(msg, "context deadline") || strings.Contains(msg, "context canceled") {
		return "timeout"
	}
	return "http_error"
}
