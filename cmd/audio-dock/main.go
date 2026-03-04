package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"github.com/sipgate/audio-dock/internal/bridge"
	"github.com/sipgate/audio-dock/internal/config"
	"github.com/sipgate/audio-dock/internal/observability"
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

	// Create Prometheus metrics registry (OBS-02, OBS-03)
	metrics := observability.NewMetrics()

	// Signal handling for graceful shutdown
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

	// --- Phase 6: Inbound Call + RTP Bridge ---

	// Create RTP port pool from config (CFG-03)
	portPool, err := bridge.NewPortPool(cfg.RTPPortMin, cfg.RTPPortMax)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create RTP port pool")
		os.Exit(1)
	}

	// Create CallManager — tracks active sessions in sync.Map (CON-01)
	callManager := bridge.NewCallManager(portPool, cfg, logger, metrics)

	// Create SIP INVITE handler and register on agent.Server
	// MUST be before registrar.Register() — handlers must be ready when INVITE arrives
	handler := sip.NewHandler(agent, callManager, cfg, logger)

	// Register with sipgate — blocking; exits if initial registration fails (SIP-01)
	// Starts background re-register goroutine at 75% of server-granted Expires (SIP-02)
	registrar := sip.NewRegistrar(agent.Client, cfg, logger, metrics)
	if err := registrar.Register(ctx); err != nil {
		logger.Fatal().Err(err).Msg("SIP registration failed")
		os.Exit(1)
	}

	// HTTP server: /health and /metrics (OBS-02, OBS-03)
	httpMux := http.NewServeMux()

	httpMux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		type healthResp struct {
			Registered  bool `json:"registered"`
			ActiveCalls int  `json:"activeCalls"`
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(healthResp{
			Registered:  registrar.IsRegistered(),
			ActiveCalls: callManager.ActiveCount(),
		})
	})

	httpMux.Handle("/metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}))

	httpServer := &http.Server{
		Addr:    ":" + cfg.HTTPPort,
		Handler: httpMux,
	}
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error().Err(err).Msg("HTTP server error")
		}
	}()
	logger.Info().Str("http_port", cfg.HTTPPort).Msg("HTTP server listening — /health and /metrics ready")

	// Wait for shutdown signal
	logger.Info().
		Int("rtp_port_min", cfg.RTPPortMin).
		Int("rtp_port_max", cfg.RTPPortMax).
		Str("ws_target_url", cfg.WSTargetURL).
		Msg("SIP registration active — ready to accept inbound calls")
	<-ctx.Done()
	logger.Info().Str("signal", ctx.Err().Error()).Msg("shutdown signal received — starting graceful drain")

	// 1. Reject new INVITEs immediately (shutdownFlag set BEFORE drain to prevent race — Research Pitfall 2)
	handler.SetShutdown()

	// 2. BYE all active calls; wait up to 8s for sessions to self-exit (LCY-01)
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer drainCancel()
	callManager.DrainAll(drainCtx)
	logger.Info().Int("remaining_calls", callManager.ActiveCount()).Msg("BYE drain complete")

	// 3. SIP UNREGISTER — send after all calls are drained to avoid sipgate routing confusion
	unregCtx, unregCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer unregCancel()
	if err := registrar.Unregister(unregCtx); err != nil {
		logger.Warn().Err(err).Msg("UNREGISTER failed during shutdown")
	} else {
		logger.Info().Msg("SIP unregistered")
	}

	// 4. Graceful HTTP server drain (allows in-flight scrapes to complete)
	httpShutCtx, httpShutCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer httpShutCancel()
	if err := httpServer.Shutdown(httpShutCtx); err != nil {
		logger.Warn().Err(err).Msg("HTTP server shutdown error")
	}

	logger.Info().Msg("shutdown complete")
	// defer agent.UA.Close() runs here — do NOT add another UA.Close() (Research Pitfall 3)
}
