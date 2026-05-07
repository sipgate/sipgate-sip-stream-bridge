package bridge

import (
	"testing"
	"time"
)

// Smoke tests for CallSession status-callback scaffolding. These cover
// the typed accessors + atomic-pointer / atomic-counter contract; the
// 50-goroutine concurrency race is exercised by the terminal-emit tests.

// TestCallSession_StatusCallbackRoundTrip — SetStatusCallback stores; the
// matching accessor returns the stored pointer (or nil before any Set).
// Replacing with a second cfg overwrites; storing nil clears.
func TestCallSession_StatusCallbackRoundTrip(t *testing.T) {
	t.Parallel()
	s := newTestSession(nil, nil)
	if got := s.StatusCallback(); got != nil {
		t.Fatalf("initial StatusCallback() = %v, want nil", got)
	}
	cfg := &StatusCallbackConfig{
		URL:    "https://x.example/cb",
		Method: "POST",
		Events: map[string]struct{}{"completed": {}},
	}
	s.SetStatusCallback(cfg)
	if got := s.StatusCallback(); got != cfg {
		t.Errorf("StatusCallback() = %v, want %v", got, cfg)
	}
	cfg2 := &StatusCallbackConfig{URL: "https://y.example/cb", Method: "POST"}
	s.SetStatusCallback(cfg2)
	if got := s.StatusCallback(); got != cfg2 {
		t.Errorf("after replacement: StatusCallback() = %v, want %v", got, cfg2)
	}
	s.SetStatusCallback(nil)
	if got := s.StatusCallback(); got != nil {
		t.Errorf("after nil store: %v, want nil", got)
	}
}

// TestCallSession_NextSequenceNumberSequential — atomic.Uint64.Add(1)-1 yields
// {0,1,2,...} per the SequenceNumber semantics.
func TestCallSession_NextSequenceNumberSequential(t *testing.T) {
	t.Parallel()
	s := newTestSession(nil, nil)
	for want := uint64(0); want < 4; want++ {
		if got := s.NextSequenceNumber(); got != want {
			t.Errorf("call #%d: got %d, want %d", want, got, want)
		}
	}
}

// TestCallSession_SetAnsweredAtIsFirstWriteWins — CAS-from-nil semantics:
// the first SetAnsweredAt wins; later SetAnsweredAt calls are dropped.
func TestCallSession_SetAnsweredAtIsFirstWriteWins(t *testing.T) {
	t.Parallel()
	s := newTestSession(nil, nil)
	t1 := time.Now().UTC()
	t2 := t1.Add(5 * time.Second)
	s.SetAnsweredAt(t1)
	s.SetAnsweredAt(t2)
	got := s.answeredAt.Load()
	if got == nil {
		t.Fatal("answeredAt is nil after SetAnsweredAt")
	}
	if !got.Equal(t1) {
		t.Errorf("answeredAt = %v, want %v (first-write-wins broken)", got, t1)
	}
}

// TestCallSession_SetSIPFinalCodeIsFirstWriteWins — first non-zero code wins.
// 486 stamped first, 503 must be dropped.
func TestCallSession_SetSIPFinalCodeIsFirstWriteWins(t *testing.T) {
	t.Parallel()
	s := newTestSession(nil, nil)
	s.SetSIPFinalCode(486)
	s.SetSIPFinalCode(503)
	if got := s.sipFinalCode.Load(); got != 486 {
		t.Errorf("sipFinalCode = %d, want 486 (first-write-wins broken)", got)
	}
}

// TestCallSession_SetSIPFinalCode_ZeroIsNoop — passing 0 must NOT seat the
// CAS slot; a subsequent non-zero call is what stamps the field.
func TestCallSession_SetSIPFinalCode_ZeroIsNoop(t *testing.T) {
	t.Parallel()
	s := newTestSession(nil, nil)
	s.SetSIPFinalCode(0)
	s.SetSIPFinalCode(486)
	if got := s.sipFinalCode.Load(); got != 486 {
		t.Errorf("sipFinalCode = %d, want 486 (a zero call must not consume the CAS slot)", got)
	}
}
