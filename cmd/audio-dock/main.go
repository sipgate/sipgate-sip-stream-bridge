package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog"
	"github.com/sipgate/audio-dock/internal/config"
	"github.com/sipgate/audio-dock/internal/sip"
)

func main() {
	// Config validation first — exits with descriptive JSON error if any required var missing (CFG-05)
	cfg := config.Load()

	// Base logger: JSON to stdout with timestamp on every event (OBS-01)
	// IMPORTANT: Never import "github.com/rs/zerolog/log" (global logger defaults to console format on stderr)
	// Always use this explicit zerolog.New(os.Stdout) pattern throughout the codebase
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

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

	// Signal handling for graceful shutdown
	// Phase 8 (LCY-01) will expand the shutdown loop
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// --- Phase 5: SIP Agent + Registration ---

	// Create sipgo UserAgent, Server, and Client
	// cfg.SIPDomain → UA hostname (From: domain); cfg.SIPRegistrar → REGISTER Request-URI host
	agent, err := sip.NewAgent(cfg, logger)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create SIP agent")
		os.Exit(1)
	}
	defer agent.UA.Close()

	// Start UDP and TCP SIP transport listeners BEFORE registering
	// ListenAndServe blocks until ctx is cancelled — must run in goroutines
	// Server.Close() in sipgo v1.2.0 returns nil only; actual shutdown = cancel ctx or ua.Close()
	go func() {
		if err := agent.Server.ListenAndServe(ctx, "udp", "0.0.0.0:5060"); err != nil {
			logger.Error().Err(err).Msg("SIP UDP listener error")
		}
	}()
	go func() {
		if err := agent.Server.ListenAndServe(ctx, "tcp", "0.0.0.0:5060"); err != nil {
			logger.Error().Err(err).Msg("SIP TCP listener error")
		}
	}()

	// Register with sipgate — blocking; exits if initial registration fails (SIP-01)
	// Starts background re-register goroutine at 75% of server-granted Expires (SIP-02)
	registrar := sip.NewRegistrar(agent.Client, cfg, logger)
	if err := registrar.Register(ctx); err != nil {
		logger.Fatal().Err(err).Msg("SIP registration failed")
		os.Exit(1)
	}

	// Wait for shutdown signal
	// Phase 6 will add INVITE handler goroutines here
	// Phase 8 will add graceful drain before <-ctx.Done()
	logger.Info().Msg("SIP registration active — waiting for calls or shutdown signal")
	<-ctx.Done()

	logger.Info().Str("signal", ctx.Err().Error()).Msg("shutdown signal received")
}
