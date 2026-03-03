package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog"
	"github.com/sipgate/audio-dock/internal/config"
)

func main() {
	// Config validation first — exits with descriptive JSON error if any required var missing (CFG-05)
	// Uses fmt.Fprintf(os.Stderr, ...) internally — no zerolog dependency in config package
	cfg := config.Load()

	// Base logger: JSON to stdout with timestamp on every event (OBS-01)
	// IMPORTANT: Never import "github.com/rs/zerolog/log" (global logger defaults to console format on stderr)
	// Always use this explicit zerolog.New(os.Stdout) pattern throughout the codebase
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	// Apply log level from config
	level, err := zerolog.ParseLevel(cfg.LogLevel)
	if err != nil {
		level = zerolog.InfoLevel
	}
	logger = logger.Level(level)

	logger.Info().
		Str("sip_user", cfg.SIPUser).
		Str("sip_domain", cfg.SIPDomain).
		Str("sip_registrar", cfg.SIPRegistrar).
		Str("ws_target_url", cfg.WSTargetURL).
		Str("sdp_contact_ip", cfg.SDPContactIP).
		Int("rtp_port_min", cfg.RTPPortMin).
		Int("rtp_port_max", cfg.RTPPortMax).
		Int("sip_expires", cfg.SIPExpires).
		Msg("audio-dock starting")

	// Signal handling for graceful shutdown (full implementation in Phase 8 — LCY-01)
	// Phase 4 scaffold: wait for SIGTERM/SIGINT, then log and exit cleanly
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Phase 4: scaffold only — later phases add SIP UA, bridge goroutines, HTTP server here
	logger.Info().Msg("scaffold ready — waiting for signal")
	<-ctx.Done()

	logger.Info().Str("signal", ctx.Err().Error()).Msg("shutdown signal received")
}
