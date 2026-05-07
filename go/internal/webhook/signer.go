package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // SHA-1 is mandated by Twilio's X-Twilio-Signature spec — interop, not crypto strength.
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"sort"
)

// signRawURLKeyType is the unexported type for the rawURL context key.  It
// is unexported so foreign code can't accidentally collide with the key —
// callers MUST use SignWithContext to set it.
type signRawURLKeyType struct{}

// signRawURLKey is the context key the StatusClient uses to thread the
// customer's verbatim StatusCallback URL bytes through to the signing
// transport. req.URL.String() may re-stringify (percent-encoding
// normalisation, IDN punycode, port stripping) producing a signature the
// customer's RequestValidator.validate() will reject. When this key is
// present in req.Context(), signingTransport signs THAT string instead of
// req.URL.String(). API callers MUST stash the original StatusCallback=
// bytes here.
var signRawURLKey = signRawURLKeyType{}

// SignWithContext returns ctx with rawURL stashed for the signing transport
// to read at RoundTrip time. Callers (StatusClient.Enqueue and the
// action-callback poster) MUST use this when the URL string they want
// signed differs from req.URL.String() — i.e. always, on the
// customer-supplied StatusCallback URL.
func SignWithContext(ctx context.Context, rawURL string) context.Context {
	return context.WithValue(ctx, signRawURLKey, rawURL)
}

// Sign computes the X-Twilio-Signature for an outbound webhook POST.  Byte-
// identical to twilio-python.RequestValidator.compute_signature and to
// twilio-node.getExpectedTwilioSignature (RESEARCH §1.4 — verified against
// upstream main 2026-05-01 and golden-vector tested against
// twilio-python==9.5.* and twilio@5.x).
//
// Parameters
//
//   - authToken: customer's Twilio AuthToken (raw string, NOT base64-decoded)
//   - urlStr:    literal URL bytes that will appear on the request line
//                (sign verbatim — caller is responsible for URL fidelity, §1.6)
//   - params:    form params for POSTs; pass nil/empty url.Values for GET
//                (the `if params:` short-circuit is preserved)
//
// Six load-bearing details (RESEARCH §1.2 — verified against both upstream
// libs):
//
//  1. Param names sorted by case-sensitive ASCII byte order
//     (sort.Strings, identical to Python str sort and JS Object.keys().sort()
//     for the ASCII range Twilio uses).
//  2. No delimiter between key and value (s += k + v — no '=' or '&').
//  3. base64 standard alphabet, padding '=' (encoding/base64.StdEncoding).
//  4. URL is signed verbatim — caller is responsible for URL fidelity (§1.6).
//  5. Multi-value keys: SORT+DEDUPE values per key (NOT submission order;
//     phase brief was wrong — fixture B confirms).
//  6. Values concatenated raw, NOT URL-encoded (s += k + rawValue).
func Sign(authToken string, urlStr string, params url.Values) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	s := urlStr
	for _, k := range keys {
		seen := make(map[string]struct{}, len(params[k]))
		unique := make([]string, 0, len(params[k]))
		for _, v := range params[k] {
			if _, ok := seen[v]; ok {
				continue
			}
			seen[v] = struct{}{}
			unique = append(unique, v)
		}
		sort.Strings(unique)
		for _, v := range unique {
			s += k + v
		}
	}

	h := hmac.New(sha1.New, []byte(authToken))
	h.Write([]byte(s))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// signingTransport is an http.RoundTripper middleware that injects the
// X-Twilio-Signature header on outbound requests.  The underlying Transport
// is unchanged — only the header is added.  Body bytes are read+replaced via
// io.NopCloser(bytes.NewReader(...)) so downstream stages (the inner
// RoundTripper, the receiving server) can still read the body.
//
// Production callers (StatusClient and the action-callback poster) own
// a private signingTransport instance — never share one across customer
// accounts because authToken is per-account.
type signingTransport struct {
	inner     http.RoundTripper
	authToken string
}

// SigningTransportFor returns an http.RoundTripper that wraps inner with
// X-Twilio-Signature injection using authToken. Exposed so callers in the
// api/ package (action-callback poster — midcall_adapter.go) can
// construct their own *http.Client outside the webhook package without
// having to expose the unexported signingTransport type.
//
// The returned RoundTripper is a thin wrapper (no goroutine, no state) — it
// is safe to share across all requests for a single (authToken, inner) pair
// but never across accounts because authToken is per-account.
func SigningTransportFor(inner http.RoundTripper, authToken string) http.RoundTripper {
	return &signingTransport{inner: inner, authToken: authToken}
}

// RoundTrip implements http.RoundTripper.  Form-body params from a POST are
// parsed for signing; GET requests sign with nil params.  The URL string is
// preferentially read from the rawURL stashed via SignWithContext, falling
// back to req.URL.String() (acceptable for tests that build their own
// requests, but production code MUST stash the verbatim URL).
func (t *signingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var params url.Values
	if req.Method == http.MethodPost && req.Body != nil {
		// Read+replace body so downstream stages can re-read it.
		bodyBytes, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		// Restore the body for the inner RoundTripper.  ContentLength
		// is preserved (set by NewRequest) — body length is unchanged.
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		parsed, err := url.ParseQuery(string(bodyBytes))
		if err != nil {
			return nil, err
		}
		params = parsed
	}

	// URL fidelity (§1.6 / Pitfall 1): prefer the verbatim URL stashed
	// in ctx, fall back to req.URL.String() (acceptable when caller
	// built the request from a URL they trust to be canonical, e.g.
	// unit tests).
	urlStr := req.URL.String()
	if v := req.Context().Value(signRawURLKey); v != nil {
		if raw, ok := v.(string); ok && raw != "" {
			urlStr = raw
		}
	}

	sig := Sign(t.authToken, urlStr, params)
	req.Header.Set("X-Twilio-Signature", sig)
	return t.inner.RoundTrip(req)
}
