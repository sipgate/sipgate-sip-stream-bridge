package sip

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sipgate/sipgate-sip-stream-bridge/internal/config"
)

// Typed errors. Caller (sip.Forwarder) translates to Twilio
// error codes:
//
//	ErrTollFraudBlocked  → 21215
//	ErrSessionRateLimit  → 21220
//	ErrGlobalRateLimit   → 21220
var (
	ErrTollFraudBlocked = errors.New("guardrails: dial target not in allow-list (toll-fraud defense)")
	ErrSessionRateLimit = errors.New("guardrails: per-session dial limit reached")
	ErrGlobalRateLimit  = errors.New("guardrails: global per-minute dial limit reached")
)

// Guardrails enforces toll-fraud allow-list + rate limits at the SIP layer.
// Checked from sip.Forwarder BEFORE the outbound INVITE is constructed —
// even if the TwiML parser is bypassed, blocking happens here.
//
// Concurrency: CheckDial / OnSessionEnd are safe for concurrent use across
// many goroutines. Per-session counters use atomic.AddInt32; the global
// rolling-minute bucket array is protected by bucketMu.
type Guardrails struct {
	allowedPrefixes []string // normalized lowercase from cfg.DialAllowedPrefixes
	maxPerSession   int32
	maxPerMinute    int32

	// perSession[CallSid] = *int32 counter; cleared via OnSessionEnd
	perSession sync.Map

	// globalBucket: 60 buckets of 1-second resolution; rolling sum = global rate.
	// bucketSecs[i] is the Unix-second timestamp the slot was last written; if it
	// is older than now-59 the slot's count is excluded from the rolling sum.
	bucketMu     sync.Mutex
	bucketCounts [60]int32
	bucketSecs   [60]int64
}

// NewGuardrails constructs from config. Pass-through for the prefix list
// (already normalized in config.Load).
func NewGuardrails(cfg config.Config) *Guardrails {
	return &Guardrails{
		allowedPrefixes: cfg.DialAllowedPrefixes,
		maxPerSession:   int32(cfg.DialMaxPerSession),
		maxPerMinute:    int32(cfg.DialMaxPerMinute),
	}
}

// CheckDial validates a dial attempt. Returns nil if allowed, or one of the
// typed errors above. On success, increments BOTH the per-session counter
// AND the global rolling-minute counter. Idempotent: call this exactly once
// per <Dial> attempt, BEFORE constructing the outbound INVITE.
func (g *Guardrails) CheckDial(callerSid, target string) error {
	normalized := normalizeTarget(target)
	if !g.matchAllowList(normalized) {
		return fmt.Errorf("%w: target=%s", ErrTollFraudBlocked, maskTarget(normalized))
	}

	// Per-session counter (atomic increment; rollback on later failure).
	sessCounter := g.getOrCreateSessionCounter(callerSid)
	if atomic.AddInt32(sessCounter, 1) > g.maxPerSession {
		atomic.AddInt32(sessCounter, -1) // rollback
		return ErrSessionRateLimit
	}

	// Global rolling-minute counter.
	if !g.checkAndIncrementGlobal() {
		atomic.AddInt32(sessCounter, -1) // rollback session count
		return ErrGlobalRateLimit
	}

	return nil
}

// OnSessionEnd clears per-session state for a CallSid. Called from the bridge
// package's CallSession terminate path.
func (g *Guardrails) OnSessionEnd(callerSid string) {
	g.perSession.Delete(callerSid)
}

// normalizeTarget strips whitespace, scheme prefix, and converts "00..." to "+...".
// Lower-cased so allow-list match is case-insensitive against scheme/host fragments.
func normalizeTarget(target string) string {
	t := strings.ToLower(strings.TrimSpace(target))
	t = strings.TrimPrefix(t, "tel:")
	t = strings.TrimPrefix(t, "sip:")
	// Remove any internal whitespace (operators sometimes paste "+49 30 555").
	t = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, t)
	if strings.HasPrefix(t, "00") {
		t = "+" + t[2:]
	}
	return t
}

// matchAllowList returns true if target starts with any configured prefix.
// Empty prefix list = default-deny (returns false unconditionally).
func (g *Guardrails) matchAllowList(target string) bool {
	if len(g.allowedPrefixes) == 0 {
		return false // default-deny
	}
	for _, p := range g.allowedPrefixes {
		if strings.HasPrefix(target, p) {
			return true
		}
	}
	return false
}

func (g *Guardrails) getOrCreateSessionCounter(callerSid string) *int32 {
	if v, ok := g.perSession.Load(callerSid); ok {
		return v.(*int32)
	}
	var zero int32
	actual, _ := g.perSession.LoadOrStore(callerSid, &zero)
	return actual.(*int32)
}

// checkAndIncrementGlobal returns true if under the rolling-minute limit
// and increments the current second's bucket; false if at the limit.
func (g *Guardrails) checkAndIncrementGlobal() bool {
	g.bucketMu.Lock()
	defer g.bucketMu.Unlock()
	now := time.Now().Unix()
	idx := now % 60
	// If this slot is from a previous minute, reset it.
	if g.bucketSecs[idx] != now {
		g.bucketCounts[idx] = 0
		g.bucketSecs[idx] = now
	}
	// Sum all slots whose timestamp is within the last 60 seconds.
	var sum int32
	for i := 0; i < 60; i++ {
		if now-g.bucketSecs[i] < 60 {
			sum += g.bucketCounts[i]
		}
	}
	if sum >= g.maxPerMinute {
		return false
	}
	g.bucketCounts[idx]++
	return true
}

// maskTarget returns a logging-safe form of a phone number (last 4 chars visible).
// Used in error messages so phone numbers are not leaked in logs (Threat Model:
// "Phone number leakage in logs").
func maskTarget(target string) string {
	if len(target) <= 4 {
		return strings.Repeat("*", len(target))
	}
	return strings.Repeat("*", len(target)-4) + target[len(target)-4:]
}
