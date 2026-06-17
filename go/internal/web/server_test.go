package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// fixtureBundle stands in for the real vite output so tests are decoupled from
// whether `make ui-go` has run.
func fixtureBundle() fstest.MapFS {
	return fstest.MapFS{
		"index.html":          {Data: []byte("<!doctype html><title>audio-dock test bundle</title>")},
		"assets/index-abc.js": {Data: []byte("export const ok = 1;\n")},
		"assets/index-abc.css": {Data: []byte(".x{color:#333}\n")},
	}
}

func get(t *testing.T, h http.Handler, path string) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Result()
}

func TestHandler_ServesIndexAtRoot(t *testing.T) {
	resp := get(t, handlerFor(fixtureBundle()), "/ui/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/ status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "audio-dock test bundle") {
		t.Errorf("index body did not contain the fixture marker: %q", body)
	}
}

func TestHandler_ServesHashedAsset(t *testing.T) {
	resp := get(t, handlerFor(fixtureBundle()), "/ui/assets/index-abc.js")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET asset status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("asset Content-Type = %q, want a javascript type", ct)
	}
}

func TestHandler_BareUIRedirectsOrServes(t *testing.T) {
	// "/ui" (no trailing slash) must resolve to the index, not 404.
	resp := get(t, handlerFor(fixtureBundle()), "/ui")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui status = %d, want 200", resp.StatusCode)
	}
}

func TestHandler_UICSP_PresentAndDistinctFromAPI(t *testing.T) {
	resp := get(t, handlerFor(fixtureBundle()), "/ui/")
	csp := resp.Header.Get("Content-Security-Policy")
	if csp != UIContentSecurityPolicy {
		t.Fatalf("CSP = %q, want %q", csp, UIContentSecurityPolicy)
	}
	// The UI CSP must open 'self' for scripts/styles…
	for _, want := range []string{"script-src 'self'", "style-src 'self'", "connect-src 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q missing %q", csp, want)
		}
	}
	// …and must NOT be the API's lock-everything-down policy.
	if csp == "default-src 'none'; frame-ancestors 'none'" {
		t.Error("UI CSP must not equal the REST API CSP")
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing X-Content-Type-Options: nosniff")
	}
}

func TestHandler_CSPOnMissingAsset(t *testing.T) {
	// CSP headers must land even on a 404 for a missing asset.
	resp := get(t, handlerFor(fixtureBundle()), "/ui/assets/does-not-exist.js")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing asset status = %d, want 404", resp.StatusCode)
	}
	if resp.Header.Get("Content-Security-Policy") != UIContentSecurityPolicy {
		t.Error("CSP header missing on 404 response")
	}
}

// TestHandler_EmbeddedBundleCompilesAndServes exercises the real //go:embed FS
// (placeholder or built bundle) so a broken embed directive fails the suite.
func TestHandler_EmbeddedBundleCompilesAndServes(t *testing.T) {
	resp := get(t, Handler(), "/ui/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("embedded GET /ui/ status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("embedded index Content-Type = %q, want text/html", ct)
	}
}
