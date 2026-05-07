package sip

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sipgate/sipgate-sip-stream-bridge/internal/config"
)

// helper: build a Guardrails directly from inline config values (avoids
// re-parsing env in every test).
func newTestGuardrails(prefixes []string, maxSession, maxMinute int) *Guardrails {
	return NewGuardrails(config.Config{
		DialAllowedPrefixes: prefixes,
		DialMaxPerSession:   maxSession,
		DialMaxPerMinute:    maxMinute,
	})
}

// 1. NewGuardrails passes through the configured prefix list verbatim.
func TestNewGuardrails_PassesThroughPrefixes(t *testing.T) {
	prefixes := []string{"+49", "+44"}
	g := newTestGuardrails(prefixes, 3, 60)

	if len(g.allowedPrefixes) != len(prefixes) {
		t.Fatalf("len(allowedPrefixes) = %d, want %d", len(g.allowedPrefixes), len(prefixes))
	}
	for i, p := range prefixes {
		if g.allowedPrefixes[i] != p {
			t.Errorf("allowedPrefixes[%d] = %q, want %q", i, g.allowedPrefixes[i], p)
		}
	}
	if g.maxPerSession != 3 {
		t.Errorf("maxPerSession = %d, want 3", g.maxPerSession)
	}
	if g.maxPerMinute != 60 {
		t.Errorf("maxPerMinute = %d, want 60", g.maxPerMinute)
	}
}

// 2. normalizeTarget table-driven test — all the documented transformations.
func TestNormalizeTarget(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"+49 30 555", "+4930555"},
		{"0049 30", "+4930"},
		{"tel:+49", "+49"},
		{"sip:user@host", "user@host"},
		{"  +49301234  ", "+49301234"},
		{"TEL:+4930", "+4930"},   // case-insensitive scheme strip
		{"+4930", "+4930"},        // already normalized
		{"00441234", "+441234"},   // 00→+ international prefix
	}
	for _, c := range cases {
		got := normalizeTarget(c.in)
		if got != c.want {
			t.Errorf("normalizeTarget(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// 3. Empty prefix list → default-deny.
func TestMatchAllowList_DefaultDeny(t *testing.T) {
	g := newTestGuardrails(nil, 3, 60)
	if g.matchAllowList("+4930555") {
		t.Error("matchAllowList returned true for empty prefix list (expected default-deny)")
	}
	if g.matchAllowList("") {
		t.Error("matchAllowList returned true for empty target with empty prefix list")
	}
}

// 4. Multiple prefixes — match if any prefix matches.
func TestMatchAllowList_LongestPrefixWins(t *testing.T) {
	g := newTestGuardrails([]string{"+49", "+4930"}, 3, 60)
	if !g.matchAllowList("+493012345") {
		t.Error("matchAllowList(+493012345) = false, want true (matches both prefixes)")
	}
	if !g.matchAllowList("+4940123") {
		t.Error("matchAllowList(+4940123) = false, want true (matches +49)")
	}
	if g.matchAllowList("+13012345") {
		t.Error("matchAllowList(+13012345) = true, want false (matches neither prefix)")
	}
}

// 5. Allow-list miss → ErrTollFraudBlocked, no counters touched.
func TestCheckDial_AllowListMiss_BlocksToFraud(t *testing.T) {
	g := newTestGuardrails([]string{"+49"}, 3, 60)

	err := g.CheckDial("CA-test-1", "+1234567890")
	if !errors.Is(err, ErrTollFraudBlocked) {
		t.Fatalf("CheckDial err = %v, want ErrTollFraudBlocked", err)
	}

	// Per-session counter must not have been created.
	if _, ok := g.perSession.Load("CA-test-1"); ok {
		t.Error("per-session counter created for blocked dial (expected unchanged)")
	}

	// Global counter sum must be 0.
	g.bucketMu.Lock()
	var sum int32
	for _, c := range g.bucketCounts {
		sum += c
	}
	g.bucketMu.Unlock()
	if sum != 0 {
		t.Errorf("global bucket sum = %d, want 0 (blocked dial must not increment)", sum)
	}
}

// 6. Allow-list hit → no error, counter increments.
func TestCheckDial_AllowListHit_FirstAttemptOK(t *testing.T) {
	g := newTestGuardrails([]string{"+49"}, 3, 60)

	if err := g.CheckDial("CA-ok-1", "+4912345"); err != nil {
		t.Fatalf("CheckDial returned %v, want nil", err)
	}

	v, ok := g.perSession.Load("CA-ok-1")
	if !ok {
		t.Fatal("per-session counter not created after successful CheckDial")
	}
	if got := atomic.LoadInt32(v.(*int32)); got != 1 {
		t.Errorf("per-session counter = %d, want 1", got)
	}

	// Global bucket should have one increment in some slot.
	g.bucketMu.Lock()
	var sum int32
	for _, c := range g.bucketCounts {
		sum += c
	}
	g.bucketMu.Unlock()
	if sum != 1 {
		t.Errorf("global bucket sum = %d, want 1", sum)
	}
}

// 7. Per-session limit — 4th call returns ErrSessionRateLimit, counter stays at 3 (rollback).
func TestCheckDial_PerSession_HitsLimit(t *testing.T) {
	g := newTestGuardrails([]string{"+49"}, 3, 1000)

	for i := 0; i < 3; i++ {
		if err := g.CheckDial("CA-limit-1", "+49123"); err != nil {
			t.Fatalf("CheckDial #%d returned %v, want nil", i+1, err)
		}
	}
	err := g.CheckDial("CA-limit-1", "+49123")
	if !errors.Is(err, ErrSessionRateLimit) {
		t.Fatalf("CheckDial #4 err = %v, want ErrSessionRateLimit", err)
	}

	// Counter must be exactly 3 (rollback worked).
	v, _ := g.perSession.Load("CA-limit-1")
	if got := atomic.LoadInt32(v.(*int32)); got != 3 {
		t.Errorf("per-session counter after rate-limit hit = %d, want 3 (rollback failed)", got)
	}
}

// 8. OnSessionEnd clears the per-session counter.
func TestCheckDial_OnSessionEnd_ResetsCounter(t *testing.T) {
	g := newTestGuardrails([]string{"+49"}, 2, 1000)

	for i := 0; i < 2; i++ {
		if err := g.CheckDial("CA-end-1", "+49123"); err != nil {
			t.Fatalf("CheckDial #%d returned %v, want nil", i+1, err)
		}
	}
	if err := g.CheckDial("CA-end-1", "+49123"); !errors.Is(err, ErrSessionRateLimit) {
		t.Fatalf("expected ErrSessionRateLimit before OnSessionEnd, got %v", err)
	}

	g.OnSessionEnd("CA-end-1")
	if _, ok := g.perSession.Load("CA-end-1"); ok {
		t.Error("per-session entry still present after OnSessionEnd")
	}

	// New CheckDial after reset should succeed.
	if err := g.CheckDial("CA-end-1", "+49123"); err != nil {
		t.Errorf("CheckDial after OnSessionEnd returned %v, want nil", err)
	}
}

// 9. Global rolling-minute limit — 3rd dial within 1s returns ErrGlobalRateLimit.
func TestCheckDial_GlobalRateLimit_HitsLimit(t *testing.T) {
	g := newTestGuardrails([]string{"+49"}, 100, 2)

	if err := g.CheckDial("CA-glob-1", "+49123"); err != nil {
		t.Fatalf("CheckDial #1 = %v, want nil", err)
	}
	if err := g.CheckDial("CA-glob-2", "+49123"); err != nil {
		t.Fatalf("CheckDial #2 = %v, want nil", err)
	}
	err := g.CheckDial("CA-glob-3", "+49123")
	if !errors.Is(err, ErrGlobalRateLimit) {
		t.Fatalf("CheckDial #3 err = %v, want ErrGlobalRateLimit", err)
	}

	// Per-session counter for the rejected sid must be rolled back to 0
	// (it was incremented to 1 then decremented when global gate failed).
	if v, ok := g.perSession.Load("CA-glob-3"); ok {
		if got := atomic.LoadInt32(v.(*int32)); got != 0 {
			t.Errorf("per-session counter for rejected sid = %d, want 0 (rollback)", got)
		}
	}
}

// 10. Global rolling-minute resets after 60s. White-box: rewrite bucketSecs to
//     simulate elapsed time without an actual sleep.
func TestCheckDial_GlobalRateLimit_ResetsAfter60s(t *testing.T) {
	g := newTestGuardrails([]string{"+49"}, 100, 2)

	for i := 0; i < 2; i++ {
		if err := g.CheckDial("CA-glob-reset", "+49123"); err != nil {
			t.Fatalf("CheckDial #%d = %v, want nil", i+1, err)
		}
	}
	if err := g.CheckDial("CA-glob-reset", "+49123"); !errors.Is(err, ErrGlobalRateLimit) {
		t.Fatalf("CheckDial #3 = %v, want ErrGlobalRateLimit", err)
	}

	// Simulate 61 seconds passing by rewriting all bucketSecs to 61s in the past.
	g.bucketMu.Lock()
	for i := range g.bucketSecs {
		if g.bucketSecs[i] != 0 {
			g.bucketSecs[i] -= 61
		}
	}
	g.bucketMu.Unlock()

	// Now a new dial should be admitted (rolling sum == 0 because all slots are stale).
	if err := g.CheckDial("CA-glob-reset", "+49123"); err != nil {
		t.Fatalf("CheckDial after simulated 60s elapsed = %v, want nil", err)
	}
}

// 11. Chaos / race test: 50 goroutines × 100 dials each. DialMaxPerMinute is set
//     high enough that the global limit never fires; per-session counter is per-sid
//     so concurrent goroutines have isolated counters. Asserts no panic, no race
//     hits (run with -race), and the total per-session counter sum equals 50*100.
func TestCheckDial_ChaosRace(t *testing.T) {
	const goroutines = 50
	const dialsPerGoroutine = 100
	const total = goroutines * dialsPerGoroutine

	g := newTestGuardrails(
		[]string{"+49"},
		dialsPerGoroutine, // per-session ceiling fits exactly one goroutine's burst
		total*2,           // global limit far above worst case so it never fires
	)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	var accepted int64

	for gid := 0; gid < goroutines; gid++ {
		gid := gid
		go func() {
			defer wg.Done()
			sid := fmt.Sprintf("CA-chaos-%d", gid)
			for i := 0; i < dialsPerGoroutine; i++ {
				if err := g.CheckDial(sid, "+4930555"); err == nil {
					atomic.AddInt64(&accepted, 1)
				} else {
					t.Errorf("goroutine %d dial %d unexpectedly rejected: %v", gid, i, err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&accepted); got != total {
		t.Errorf("accepted = %d, want %d", got, total)
	}

	// Sum all per-session counters — should equal total.
	var sum int32
	g.perSession.Range(func(_, v any) bool {
		sum += atomic.LoadInt32(v.(*int32))
		return true
	})
	if int(sum) != total {
		t.Errorf("sum of per-session counters = %d, want %d", sum, total)
	}

	// Sum global rolling bucket — should also equal total (all dials in the same minute).
	g.bucketMu.Lock()
	var bucketSum int32
	now := time.Now().Unix()
	for i := 0; i < 60; i++ {
		if now-g.bucketSecs[i] < 60 {
			bucketSum += g.bucketCounts[i]
		}
	}
	g.bucketMu.Unlock()
	if int(bucketSum) != total {
		t.Errorf("global bucket rolling sum = %d, want %d", bucketSum, total)
	}
}

// 12. maskTarget — short strings fully masked; long strings keep last 4 chars.
func TestMaskTarget(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"+49", "***"},
		{"1234", "****"},
		{"+4930555", "****0555"},
		{"+12345678901", "********8901"},
	}
	for _, c := range cases {
		got := maskTarget(c.in)
		if got != c.want {
			t.Errorf("maskTarget(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
