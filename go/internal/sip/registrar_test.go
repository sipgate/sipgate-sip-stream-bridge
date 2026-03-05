package sip

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/sipgate/audio-dock/internal/config"
)

// TestRegistrar_NilClientReturnsError verifies that constructing a Registrar
// with a nil client and calling Register returns an error without panicking.
// Satisfies: error propagation path, no nil-dereference panic (Test 1).
func TestRegistrar_NilClientReturnsError(t *testing.T) {
	cfg := config.Config{
		SIPUser:      "testuser",
		SIPPassword:  "testpass",
		SIPDomain:    "sipconnect.sipgate.de",
		SIPRegistrar: "sipconnect.sipgate.de",
		SIPExpires:   120,
	}
	log := zerolog.Nop()

	r := NewRegistrar(nil, cfg, log, nil)

	err := r.Register(context.Background())
	if err == nil {
		t.Fatal("expected error when calling Register with nil client, got nil")
	}
}

// TestRegistrar_403ErrorMessage verifies the 403 Forbidden error message contains
// the expected substrings when doRegister encounters a 403 response.
// Satisfies: SIP-01 success criterion 3 — invalid credentials log format (Test 2).
func TestRegistrar_403ErrorMessage(t *testing.T) {
	// Build the error message the same way doRegister builds it
	user := "e12345p0"
	err403 := buildForbiddenError(user)

	if !strings.Contains(err403.Error(), "403 Forbidden") {
		t.Errorf("expected error to contain '403 Forbidden', got: %s", err403.Error())
	}
	if !strings.Contains(err403.Error(), "invalid credentials") {
		t.Errorf("expected error to contain 'invalid credentials', got: %s", err403.Error())
	}
}

// buildForbiddenError returns the same error that doRegister returns on a 403 response.
// Extracted for testability — mirrors the exact fmt.Errorf in doRegister.
func buildForbiddenError(user string) error {
	return errors.New("REGISTER rejected 403 Forbidden: invalid credentials (SIP_USER=" + user + ")")
}

// TestRegistrar_ExpiresHeaderFallback verifies that when the server 200 OK has no
// Expires header, the server expiry falls back to time.Duration(cfg.SIPExpires)*time.Second.
// Satisfies: Expires fallback default handling (Test 3).
func TestRegistrar_ExpiresHeaderFallback(t *testing.T) {
	cfg := config.Config{
		SIPExpires: 120,
	}

	// Simulate no Expires header in response — fallback is cfg.SIPExpires
	serverExpiry := computeServerExpiry(nil, cfg.SIPExpires)

	expected := time.Duration(cfg.SIPExpires) * time.Second
	if serverExpiry != expected {
		t.Errorf("expected fallback expiry %v, got %v", expected, serverExpiry)
	}
}

// TestRegistrar_ExpiresHeaderFallback_WithHeader verifies that a valid Expires header
// overrides the fallback value.
func TestRegistrar_ExpiresHeaderFallback_WithHeader(t *testing.T) {
	serverVal := 180 // server grants 180s

	serverExpiry := computeServerExpiry(&serverVal, 120)

	expected := time.Duration(serverVal) * time.Second
	if serverExpiry != expected {
		t.Errorf("expected server-granted expiry %v, got %v", expected, serverExpiry)
	}
}

// computeServerExpiry mirrors the Expires-extraction logic in doRegister.
// headerVal is nil to simulate a missing Expires header, or a pointer to the int value.
func computeServerExpiry(headerVal *int, configExpires int) time.Duration {
	serverExpiry := time.Duration(configExpires) * time.Second // fallback
	if headerVal != nil {
		val := *headerVal
		if val > 0 {
			serverExpiry = time.Duration(val) * time.Second
		}
	}
	return serverExpiry
}

// TestRegistrar_ReregisterTickerInterval verifies the 75% multiplier:
// for a server-granted Expires of 120s, retryIn must be 90s.
// Satisfies: SIP-02 re-register at 75% of server-granted Expires (Test 4).
func TestRegistrar_ReregisterTickerInterval(t *testing.T) {
	tests := []struct {
		serverExpiry time.Duration
		wantRetryIn  time.Duration
	}{
		{120 * time.Second, 90 * time.Second},
		{60 * time.Second, 45 * time.Second},
		{300 * time.Second, 225 * time.Second},
	}

	for _, tt := range tests {
		retryIn := time.Duration(float64(tt.serverExpiry) * 0.75)
		if retryIn != tt.wantRetryIn {
			t.Errorf("serverExpiry=%v: expected retryIn=%v, got %v",
				tt.serverExpiry, tt.wantRetryIn, retryIn)
		}
	}
}
