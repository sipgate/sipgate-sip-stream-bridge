package identity

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
)

// NewCallSid mints a unique Twilio-compatible CallSid.
// Format: "CA" + 32 lowercase hex chars (matches ^CA[0-9a-f]{32}$).
// Uses crypto/rand exclusively (never a pseudo-random or counter source) per COMPAT-03.
// 128 bits of entropy = collision-free for any realistic call volume.
func NewCallSid() string {
	var b [16]byte
	_, err := rand.Read(b[:])
	if err != nil {
		// crypto/rand.Read should never error on supported platforms;
		// panic if it does — running without entropy is unsafe.
		panic("identity.NewCallSid: crypto/rand.Read failed: " + err.Error())
	}
	return "CA" + hex.EncodeToString(b[:])
}

// CallSidRE validates Twilio-compatible CallSid strings.
var CallSidRE = regexp.MustCompile(`^CA[0-9a-f]{32}$`)
