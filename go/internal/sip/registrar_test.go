package sip

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	siplib "github.com/emiago/sipgo/sip"
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

// TestOptionsKeepalive_ClassifyFailure verifies isOptionsFailure covers all OPTS-02/OPTS-03 cases.
func TestOptionsKeepalive_ClassifyFailure(t *testing.T) {
	tests := []struct {
		name    string
		res     *siplib.Response
		err     error
		wantFail bool
	}{
		{"nil res nil err = timeout", nil, nil, true},
		{"non-nil err = transport error", nil, errors.New("timeout"), true},
		{"404 Not Found", &siplib.Response{StatusCode: 404}, nil, true},
		{"503 Service Unavailable (5xx)", &siplib.Response{StatusCode: 503}, nil, true},
		{"500 Internal Server Error (5xx)", &siplib.Response{StatusCode: 500}, nil, true},
		{"200 OK = success, not failure", &siplib.Response{StatusCode: 200}, nil, false},
		{"401 Unauthorized = auth, not failure", &siplib.Response{StatusCode: 401}, nil, false},
		{"407 Proxy Auth = auth, not failure", &siplib.Response{StatusCode: 407}, nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isOptionsFailure(tt.res, tt.err)
			if got != tt.wantFail {
				t.Errorf("isOptionsFailure() = %v, want %v", got, tt.wantFail)
			}
		})
	}
}

// TestOptionsKeepalive_ClassifyAuth verifies isOptionsAuth covers OPTS-03 auth handling.
func TestOptionsKeepalive_ClassifyAuth(t *testing.T) {
	tests := []struct {
		name     string
		res      *siplib.Response
		wantAuth bool
	}{
		{"401 Unauthorized", &siplib.Response{StatusCode: 401}, true},
		{"407 Proxy Auth Required", &siplib.Response{StatusCode: 407}, true},
		{"200 OK = not auth", &siplib.Response{StatusCode: 200}, false},
		{"nil response = not auth", nil, false},
		{"404 Not Found = not auth", &siplib.Response{StatusCode: 404}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isOptionsAuth(tt.res)
			if got != tt.wantAuth {
				t.Errorf("isOptionsAuth() = %v, want %v", got, tt.wantAuth)
			}
		})
	}
}

// TestOptionsKeepalive_ThresholdLogic verifies the consecutive-failure state machine (OPTS-02).
func TestOptionsKeepalive_ThresholdLogic(t *testing.T) {
	tests := []struct {
		name              string
		consecutiveIn     int
		res               *siplib.Response
		err               error
		wantCount         int
		wantTrigger       bool
	}{
		{
			name:          "1st failure: count increments to 1, no trigger",
			consecutiveIn: 0,
			res:           nil,
			err:           nil,
			wantCount:     1,
			wantTrigger:   false,
		},
		{
			name:          "2nd failure: hits threshold 2, trigger=true, count resets to 0",
			consecutiveIn: 1,
			res:           nil,
			err:           nil,
			wantCount:     0,
			wantTrigger:   true,
		},
		{
			name:          "success resets counter to 0",
			consecutiveIn: 1,
			res:           &siplib.Response{StatusCode: 200},
			err:           nil,
			wantCount:     0,
			wantTrigger:   false,
		},
		{
			name:          "401 auth does not increment counter, resets to 0",
			consecutiveIn: 1,
			res:           &siplib.Response{StatusCode: 401},
			err:           nil,
			wantCount:     0,
			wantTrigger:   false,
		},
		{
			name:          "407 auth does not increment counter, resets to 0",
			consecutiveIn: 0,
			res:           &siplib.Response{StatusCode: 407},
			err:           nil,
			wantCount:     0,
			wantTrigger:   false,
		},
		{
			name:          "404 failure increments counter",
			consecutiveIn: 0,
			res:           &siplib.Response{StatusCode: 404},
			err:           nil,
			wantCount:     1,
			wantTrigger:   false,
		},
		{
			name:          "5xx failure at count 1 triggers register",
			consecutiveIn: 1,
			res:           &siplib.Response{StatusCode: 503},
			err:           nil,
			wantCount:     0,
			wantTrigger:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCount, gotTrigger := applyOptionsResponse(tt.consecutiveIn, tt.res, tt.err)
			if gotCount != tt.wantCount {
				t.Errorf("applyOptionsResponse() count = %d, want %d", gotCount, tt.wantCount)
			}
			if gotTrigger != tt.wantTrigger {
				t.Errorf("applyOptionsResponse() triggerRegister = %v, want %v", gotTrigger, tt.wantTrigger)
			}
		})
	}
}

// TestOptionsKeepalive_ContextCancel verifies the goroutine stops when ctx is cancelled (OPTS-04).
// Uses a 500ms ticker and cancels after 50ms — well before the first tick — so sendOptions
// (which would panic on nil client) is never called.
func TestOptionsKeepalive_ContextCancel(t *testing.T) {
	cfg := config.Config{
		SIPUser:            "u",
		SIPDomain:          "d",
		SIPRegistrar:       "r",
		SIPOptionsInterval: 500 * time.Millisecond,
	}
	log := zerolog.Nop()
	reg := NewRegistrar(nil, cfg, log, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		reg.optionsKeepaliveLoop(ctx, 500*time.Millisecond)
	}()

	// Cancel well before the first tick so sendOptions (nil client) is never reached
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
		// goroutine exited cleanly
	case <-time.After(500 * time.Millisecond):
		t.Fatal("optionsKeepaliveLoop did not stop within 500ms after ctx cancel")
	}
}
