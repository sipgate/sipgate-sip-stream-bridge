package api

import (
	"net/http"
	"time"

	"github.com/sipgate/sipgate-sip-stream-bridge/internal/webhook"
)

// testActionPoster returns the signed *http.Client every internal/api test
// passes to Mount() (and to newMidCallAdapter directly) for the <Dial>
// action-callback path. A single helper keeps the per-test boilerplate
// flat now that Mount's signature accepts actionPoster.
//
// authToken defaults to a stable test value so signature assertions are
// deterministic across tests. Tests that need to capture a specific
// X-Twilio-Signature against a httptest.NewTLSServer MUST pass their own
// *http.Client built around srv.Client().Transport (see
// TestFireActionCallback_SignsWithXTwilioSignature for the pattern).
//
// File suffix is plain `_helpers.go` (not `_test.go`) so the helper is
// reachable from any file in the api package — including production code
// paths that need a sane default in fixtures.
//
// Build constraint: this file is intentionally NOT under a `// +build test`
// build tag because Go's standard test fixtures live alongside production
// source. The helper itself is harmless (just builds an *http.Client) and
// adds no dependencies beyond what midcall_adapter.go already imports.
func testActionPoster(authToken string) *http.Client {
	if authToken == "" {
		authToken = "test-auth-token-12345"
	}
	return &http.Client{
		Transport: webhook.SigningTransportFor(http.DefaultTransport, authToken),
		Timeout:   5 * time.Second,
	}
}
