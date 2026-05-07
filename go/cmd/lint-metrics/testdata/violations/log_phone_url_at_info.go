package violations

import (
	"github.com/rs/zerolog/log"
)

// leakInfoPhoneURL emits phone-number / URL fields at Info+ levels with NO
// // metrics:url-allowed opt-out. The walker's log-field visitor MUST flag
// every Str("from"|"to"|"url"|...) at Info/Warn/Error/Fatal/Panic.
//
// Debug-level emits are LEGITIMATE (debug logs are operator-controlled).
func leakInfoPhoneURL(phone, url string) {
	// 2 violations — Info-level Str("from", _) AND Str("url", _).
	log.Info().
		Str("from", phone).
		Str("url", url).
		Msg("INFO leak")

	// 1 violation — Warn-level Str("to", _).
	log.Warn().
		Str("to", phone).
		Msg("WARN leak")

	// 0 violations — Debug level allowed by convention.
	log.Debug().
		Str("from", phone).
		Str("url", url).
		Msg("DEBUG allowed by convention")
}
