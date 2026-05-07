package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture is the on-disk JSON shape produced by
// tools/gen_status_callback_fixtures/{gen.py,gen.js}. Each value is a JSON
// array (even single-value keys) so it maps cleanly to url.Values.
type fixture struct {
	Name              string              `json:"name"`
	SourceLib         string              `json:"source_lib"`
	SourceVersion     string              `json:"source_version"`
	AuthToken         string              `json:"auth_token"`
	URL               string              `json:"url"`
	Params            map[string][]string `json:"params"`
	ExpectedSignature string              `json:"expected_signature"`
}

// loadFixtures reads testdata/<name> and returns the parsed fixture list.
// Fails the test if the file is missing, malformed, or thinner than the
// 12-fixture floor for cross-language byte-fidelity coverage.
func loadFixtures(t *testing.T, name string) []fixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var out []fixture
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if len(out) < 12 {
		t.Fatalf("expected >=12 fixtures in %s, got %d", name, len(out))
	}
	return out
}

// TestSign_PythonGoldenVectors — cross-language byte-fidelity gate
// against twilio-python.RequestValidator.compute_signature. Hard CI failure
// on any single mismatch.
func TestSign_PythonGoldenVectors(t *testing.T) {
	for _, f := range loadFixtures(t, "python_fixtures.json") {
		f := f
		t.Run(f.Name, func(t *testing.T) {
			got := Sign(f.AuthToken, f.URL, url.Values(f.Params))
			if got != f.ExpectedSignature {
				t.Errorf("Sign mismatch:\n  url:    %q\n  params: %v\n  got:    %q\n  want:   %q",
					f.URL, f.Params, got, f.ExpectedSignature)
			}
		})
	}
}

// TestSign_NodeGoldenVectors — same gate against twilio-node.getExpectedTwilioSignature.
func TestSign_NodeGoldenVectors(t *testing.T) {
	for _, f := range loadFixtures(t, "node_fixtures.json") {
		f := f
		t.Run(f.Name, func(t *testing.T) {
			got := Sign(f.AuthToken, f.URL, url.Values(f.Params))
			if got != f.ExpectedSignature {
				t.Errorf("Sign mismatch:\n  url:    %q\n  params: %v\n  got:    %q\n  want:   %q",
					f.URL, f.Params, got, f.ExpectedSignature)
			}
		})
	}
}

// TestSign_CrossLibraryParity — for every fixture name shared between
// python_fixtures.json and node_fixtures.json, the upstream-emitted
// expected_signature MUST be identical. This is the cross-library Twilio
// invariant — divergence here is a Twilio-side bug we'd need to hold both
// sides of (RESEARCH §2.4).
func TestSign_CrossLibraryParity(t *testing.T) {
	py := loadFixtures(t, "python_fixtures.json")
	nd := loadFixtures(t, "node_fixtures.json")
	pyMap := make(map[string]string, len(py))
	for _, f := range py {
		pyMap[f.Name] = f.ExpectedSignature
	}
	for _, n := range nd {
		if want, ok := pyMap[n.Name]; ok && want != n.ExpectedSignature {
			t.Errorf("cross-lib divergence on %q: python=%q node=%q",
				n.Name, want, n.ExpectedSignature)
		}
	}
}

// TestSign_SixLoadBearingDetails — RESEARCH §1.2.  Each load-bearing detail
// gets a named subtest so a failure points at the specific invariant rather
// than only the golden-vector roll-up.  Covers the corrections vs the phase
// brief (Detail 5 — SORT+DEDUPE, NOT submission order).
func TestSign_SixLoadBearingDetails(t *testing.T) {
	t.Run("detail-1-deterministic-key-sort", func(t *testing.T) {
		// Detail 1: case-sensitive ASCII byte sort of param names —
		// uppercase 'Z' < lowercase 'a'.  The signature MUST be
		// independent of insertion order in the url.Values map (a Go
		// map is intentionally unordered, so any order-sensitive
		// implementation would already produce flaky output; this
		// check makes the invariant explicit).
		a := Sign("t", "u", url.Values{"Z": {"1"}, "a": {"2"}})
		b := Sign("t", "u", url.Values{"a": {"2"}, "Z": {"1"}})
		if a != b {
			t.Fatalf("Detail 1 (deterministic key sort) FAILED: order-dependent: %q vs %q", a, b)
		}
	})

	t.Run("detail-2-no-delimiter", func(t *testing.T) {
		// Detail 2: no delimiter between key and value (s += k + v —
		// no '=' or '&').  Manual reconstruction of the Twilio
		// canonical concat: HMAC-SHA1 over url + "a" + "v" (the "av"
		// reflects key="a", value="v" with NO '=' between them).
		urlStr := "https://customer.example/cb"
		authToken := "12345"
		params := url.Values{"a": {"v"}}
		wantHmac := hmac.New(sha1.New, []byte(authToken))
		wantHmac.Write([]byte(urlStr + "a" + "v"))
		want := base64.StdEncoding.EncodeToString(wantHmac.Sum(nil))
		got := Sign(authToken, urlStr, params)
		if got != want {
			t.Errorf("Detail 2 (no key/value delimiter) FAILED: got %q want %q", got, want)
		}
		// Negative: if the implementation accidentally inserted '='
		// it would yield a different signature.  Verify by computing
		// the wrong-form HMAC and asserting we did NOT produce that
		// value.
		wrongHmac := hmac.New(sha1.New, []byte(authToken))
		wrongHmac.Write([]byte(urlStr + "a=v"))
		wrong := base64.StdEncoding.EncodeToString(wrongHmac.Sum(nil))
		if got == wrong {
			t.Errorf("Detail 2 (no delimiter) regressed: signature matches the WRONG 'a=v' form")
		}
	})

	t.Run("detail-3-base64-standard-padding", func(t *testing.T) {
		// Detail 3: base64 standard alphabet WITH padding ('=').
		// HMAC-SHA1 = 20 bytes -> ceil(20/3)*4 = 28 chars,
		// with one '=' pad char (since 20 % 3 == 2).
		sig := Sign("12345", "https://customer.example/cb", nil)
		if len(sig) != 28 {
			t.Errorf("Detail 3 (base64 length) FAILED: signature len = %d, want 28 (HMAC-SHA1 base64)", len(sig))
		}
		if len(sig)%4 != 0 {
			t.Errorf("Detail 3 (base64 padding) FAILED: signature %q has len %d, must be multiple of 4 with '=' padding", sig, len(sig))
		}
		if len(sig) >= 28 && sig[27] != '=' {
			t.Errorf("Detail 3 (base64 padding) FAILED: trailing char = %q, want '='", sig[27])
		}
	})

	t.Run("detail-5-sort-and-dedupe-values", func(t *testing.T) {
		// Detail 5: SORT + DEDUPE values per key (NOT submission
		// order).  Phase brief was wrong; fixture B is the golden
		// proof — produces IK+Dwps556ElfBT0I3Rgjkr1wJU= ONLY when the
		// signer sorts and dedupes.
		p := url.Values{
			"Digits":     {"5678", "1234", "1234"},
			"Sid":        {"CA123"},
			"SidAccount": {"AC123"},
		}
		sig := Sign("12345", "https://mycompany.com/myapp.php?foo=1&bar=2", p)
		if sig != "IK+Dwps556ElfBT0I3Rgjkr1wJU=" {
			t.Fatalf("Detail 5 (sort+dedupe values) FAILED: got %q want IK+Dwps556ElfBT0I3Rgjkr1wJU=", sig)
		}
	})

	t.Run("detail-6-raw-values-not-url-encoded", func(t *testing.T) {
		// Detail 6: values are concatenated RAW, not URL-encoded.  A
		// '+' in a value MUST be signed verbatim — NOT as '%2B'.
		urlStr := "https://customer.example/cb"
		authToken := "12345"
		// Manual raw-concat (NOT url-encoded).
		wantHmac := hmac.New(sha1.New, []byte(authToken))
		wantHmac.Write([]byte(urlStr + "From" + "+4915123456789"))
		want := base64.StdEncoding.EncodeToString(wantHmac.Sum(nil))
		got := Sign(authToken, urlStr, url.Values{"From": {"+4915123456789"}})
		if got != want {
			t.Errorf("Detail 6 (raw values) FAILED: got %q want %q", got, want)
		}
		// Negative: if the implementation url-encoded the value, we'd
		// see the signature for "From%2B4915123456789" instead.
		wrongHmac := hmac.New(sha1.New, []byte(authToken))
		wrongHmac.Write([]byte(urlStr + "From" + "%2B4915123456789"))
		wrong := base64.StdEncoding.EncodeToString(wrongHmac.Sum(nil))
		if got == wrong {
			t.Errorf("Detail 6 (raw values) regressed: matches the wrong url-encoded form")
		}
	})
}

// TestSign_URLVerbatim — RESEARCH §1.6 / Pitfall 1.  Five URL variants that
// differ only in normalization-style bytes (trailing slash, explicit port,
// %20 vs literal space) MUST produce five distinct signatures.  This is the
// invariant that justifies SignWithContext (the verbatim-URL seam in the
// signing transport).
func TestSign_URLVerbatim(t *testing.T) {
	params := url.Values{"x": {"1"}}
	sigA := Sign("t", "https://customer.example/cb", params)
	sigB := Sign("t", "https://customer.example/cb/", params)             // trailing slash
	sigC := Sign("t", "https://customer.example:8443/cb", params)         // explicit port
	sigD := Sign("t", "https://customer.example/cb%20path", params)       // %20
	sigE := Sign("t", "https://customer.example/cb path", params)         // literal space
	seen := map[string]bool{sigA: true, sigB: true, sigC: true, sigD: true, sigE: true}
	if len(seen) < 5 {
		t.Fatalf("URL verbatim FAILED: expected 5 distinct signatures, got %d (sigs: %q %q %q %q %q)",
			len(seen), sigA, sigB, sigC, sigD, sigE)
	}
}

// TestSign_EmptyParams — Sign with nil / empty params signs the URL bytes
// only.  Twilio's `if params:` short-circuit is preserved.
func TestSign_EmptyParams(t *testing.T) {
	sig := Sign("12345", "https://customer.example/cb", nil)
	sigEmpty := Sign("12345", "https://customer.example/cb", url.Values{})
	if sig != sigEmpty {
		t.Errorf("nil and empty url.Values should produce same signature: %q vs %q", sig, sigEmpty)
	}
	if sig == "" {
		t.Errorf("Sign returned empty string for URL-only input")
	}
}

// TestSigningTransport_SetsHeader — proves the RoundTripper injects the
// X-Twilio-Signature header AND preserves the request body for downstream
// readers (the inner RoundTripper / the receiving server).  Uses
// SignWithContext to thread the customer's verbatim StatusCallback URL,
// so the signature compares against the canonical Fixture A vector.
func TestSigningTransport_SetsHeader(t *testing.T) {
	var captured struct {
		sig  string
		body string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.sig = r.Header.Get("X-Twilio-Signature")
		b, _ := io.ReadAll(r.Body)
		captured.body = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr := &signingTransport{inner: srv.Client().Transport, authToken: "12345"}
	client := &http.Client{Transport: tr}

	body := url.Values{
		"CallSid": {"CA1234567890ABCDE"},
		"Digits":  {"1234"},
		"From":    {"+14158675309"},
		"To":      {"+18005551212"},
		"Caller":  {"+14158675309"},
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(body.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Stash the URL we want signed (the customer's StatusCallback= bytes —
	// canonical Fixture A URL, which has a known expected signature).
	req = req.WithContext(SignWithContext(req.Context(), "https://mycompany.com/myapp.php?foo=1&bar=2"))

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	want := "RSOYDt4T1cUTdK1PDd93/VVr8B8="
	if captured.sig != want {
		t.Errorf("X-Twilio-Signature: got %q want %q", captured.sig, want)
	}
	if captured.body != body.Encode() {
		t.Errorf("body lost in transport: got %q want %q", captured.body, body.Encode())
	}
}

// TestSigningTransport_ContextFallback — when no rawURL is stashed in the
// request context, the transport falls back to req.URL.String().  This is
// the test-only path; production callers (StatusClient) MUST use
// SignWithContext to defeat req.URL.String() normalization.
func TestSigningTransport_ContextFallback(t *testing.T) {
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Get("X-Twilio-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	tr := &signingTransport{inner: srv.Client().Transport, authToken: "t"}
	client := &http.Client{Transport: tr}
	body := bytes.NewReader([]byte(""))
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/cb", body)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// Empty POST body parses to url.Values{}; signing is over URL-only bytes.
	want := Sign("t", srv.URL+"/cb", url.Values{})
	if captured != want {
		t.Errorf("got %q want %q", captured, want)
	}
}

// TestSigningTransport_GetSignsURLOnly — GET requests have no form body, so
// the transport signs the URL only (params=nil path).  This guards against
// a future regression where someone tries to read req.Body unconditionally.
func TestSigningTransport_GetSignsURLOnly(t *testing.T) {
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Get("X-Twilio-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	tr := &signingTransport{inner: srv.Client().Transport, authToken: "t"}
	client := &http.Client{Transport: tr}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/cb?x=1", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	want := Sign("t", srv.URL+"/cb?x=1", nil)
	if captured != want {
		t.Errorf("got %q want %q", captured, want)
	}
}

// TestSigningTransportFor_Exported — SigningTransportFor is an exported
// factory so callers in internal/api/ can compose the signing
// middleware over their own *http.Transport without touching the unexported
// signingTransport type. This is a structural smoke test: the returned value
// MUST be a non-nil http.RoundTripper that injects X-Twilio-Signature.
func TestSigningTransportFor_Exported(t *testing.T) {
	t.Parallel()
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Get("X-Twilio-Signature")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	rt := SigningTransportFor(http.DefaultTransport, "tok")
	if rt == nil {
		t.Fatal("SigningTransportFor returned nil")
	}
	client := &http.Client{Transport: rt}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/cb", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if captured == "" {
		t.Error("X-Twilio-Signature header not injected — SigningTransportFor wrapper inert")
	}
	if want := Sign("tok", srv.URL+"/cb", nil); captured != want {
		t.Errorf("captured signature = %q, want %q", captured, want)
	}
}
