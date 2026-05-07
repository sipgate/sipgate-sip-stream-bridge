// Package identity mints and validates Twilio-compatible SID values used
// throughout the v3.0 control plane. AccountSid is deterministically derived
// from SIP_USER (COMPAT-02); CallSid is cryptographically random (COMPAT-03).
//
// The package is a pure leaf: stdlib only, no internal/project imports.
package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
)

// DeriveAccountSid produces a deterministic Twilio-compatible AccountSid from SIP_USER.
// Format: "AC" + 32 lowercase hex chars (matches ^AC[0-9a-f]{32}$).
// Deterministic across restarts (COMPAT-02: surfaced via /health, sent on every webhook;
// changing it would break customer allow-lists).
func DeriveAccountSid(sipUser string) string {
	sum := sha256.Sum256([]byte(sipUser))
	return "AC" + hex.EncodeToString(sum[:16]) // first 16 bytes = 32 hex chars
}

// AccountSidRE validates Twilio-compatible AccountSid strings.
var AccountSidRE = regexp.MustCompile(`^AC[0-9a-f]{32}$`)
