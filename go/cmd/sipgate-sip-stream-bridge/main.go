package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/api"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/bridge"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/config"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/identity"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/observability"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/sip"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/webhook"
)

func main() {
	// Config validation first — exits with descriptive JSON error if any required var missing (CFG-05)
	cfg := config.Load()

	// Base logger: JSON to stdout with timestamp on every event +
	// secret-mask writer wrapping os.Stdout.
	//
	// IMPORTANT: Never import "github.com/rs/zerolog/log" (global logger
	// defaults to console format on stderr). Always use this explicit
	// zerolog.New(os.Stdout) pattern throughout the codebase.
	//
	// IMPORTANT: NewSecretMaskWriter wraps os.Stdout (NOT a zerolog event-hook).
	// zerolog's event-hook API only sees the message string — not field bytes —
	// so a Hook cannot redact secrets accidentally embedded in
	// Str("auth_token", ...) field values. Wrapping the io.Writer is
	// field-name agnostic by construction: the redaction operates on serialised
	// JSON bytes before they reach the OS pipe.
	//
	// Empty secrets are filtered by NewSecretMaskWriter; an empty AuthToken
	// (legacy v2.1 deployments without REST consumers) means no AuthToken
	// substring is masked, but cfg.SIPPassword is still redacted.
	logger := zerolog.New(observability.NewSecretMaskWriter(os.Stdout, cfg.AuthToken, cfg.SIPPassword)).With().Timestamp().Logger()

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
		Bool("srtp_enabled", cfg.SRTPEnabled).
		Msg("sipgate-sip-stream-bridge starting")

	// Create Prometheus metrics registry
	metrics := observability.NewMetrics()

	// Signal handling for graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// --- SIP Agent + Registration ---

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
	// SIP_LISTEN_ADDR (default 0.0.0.0:5060) — overrideable for e2e harness co-existence
	// or non-privileged-port deploys.
	go func() {
		if err := agent.Server.ListenAndServe(ctx, "udp", cfg.SIPListenAddr); err != nil {
			logger.Error().Err(err).Msg("SIP UDP listener error")
		}
	}()
	go func() {
		if err := agent.Server.ListenAndServe(ctx, "tcp", cfg.SIPListenAddr); err != nil {
			logger.Error().Err(err).Msg("SIP TCP listener error")
		}
	}()

	// --- Inbound Call + RTP Bridge ---

	// Create RTP port pool from config
	portPool, err := bridge.NewPortPool(cfg.RTPPortMin, cfg.RTPPortMax)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create RTP port pool")
		os.Exit(1)
	}

	// Derive AccountSid once at startup (COMPAT-02). Deterministic from cfg.SIPUser.
	accountSid := identity.DeriveAccountSid(cfg.SIPUser)
	if !identity.AccountSidRE.MatchString(accountSid) {
		logger.Fatal().Str("account_sid", accountSid).Msg("derived AccountSid does not match ^AC[0-9a-f]{32}$ — invalid SIP_USER?")
		os.Exit(1)
	}

	// AUTH_TOKEN guards: warn-only, non-fatal so v2.1 deployments
	// without REST consumers still boot. The REST API is mounted unconditionally;
	// requests without valid Basic Auth are rejected by api.BasicAuth (401).
	if cfg.AuthToken == "" {
		logger.Warn().Msg("AUTH_TOKEN unset — REST API will reject all requests with 401 (set AUTH_TOKEN to enable)")
	} else if len(cfg.AuthToken) < 32 {
		logger.Warn().Int("length", len(cfg.AuthToken)).Msg("AUTH_TOKEN length below Twilio's 32-char standard — confirm this is not a placeholder")
	}

	// Create CallManager — tracks active sessions in sync.Map.
	// AccountSid threaded so every CallSession can stamp WS events.
	// NewCallManager spawns the recentlyTerminatedSweep goroutine;
	// callManager.Close() cancels its context on shutdown.
	callManager := bridge.NewCallManager(portPool, accountSid, cfg, logger, metrics)
	defer callManager.Close() // stop recentlyTerminatedSweep cleanly

	// Per-process StatusClient — owns its own *http.Transport distinct
	// from voiceWC (the Url= fetcher) so a flapping customer-supplied
	// callback host can't degrade Url= fetch latency. Threaded onto:
	//   - every CallSession created by callManager.StartSession (via
	//     SetStatusClient → terminal-event emission inside markTerminated)
	//   - sip.Forwarder for per-<Dial>-leg lifecycle emission
	statusWC := webhook.NewStatusClient(cfg.AuthToken, metrics, logger.With().Str("component", "status-client").Logger())
	callManager.SetStatusClient(statusWC)

	// Guardrails enforce toll-fraud prefix allow-list and per-session/global
	// dial rate limits for the B2BUA <Dial> verb.
	guardrails := sip.NewGuardrails(cfg)

	// Forwarder is the B2BUA SIP UAC — constructs outbound INVITEs for <Dial>.
	// Shares agent.Client so the UDP source port that sipgate registered stays
	// live (never call sipgo.NewClient inside the forwarder).
	//
	// statusWC + accountSid threaded for per-<Dial>-leg status callback
	// emission (initiated/ringing/answered events on the callee leg).
	forwarder := sip.NewForwarder(agent, guardrails, cfg, metrics, logger, statusWC, accountSid)

	// Create SIP INVITE handler and register on agent.Server.
	// MUST be before registrar.Register() — handlers must be ready when INVITE arrives.
	//
	// Pass the Forwarder as a fallback ByeReader so BYEs for outbound <Dial>
	// dialogs are routed to its DialogClientCache. Without this, when the
	// callee hangs up first on a B2BUA bridge, the outbound dialog never
	// observes the BYE and Forwarder.Dial deadlocks the API handler.
	handler := sip.NewHandler(agent, callManager, cfg, logger, forwarder)

	// Wire the parent-leg status-callback emission surface on the Handler.
	// The lookup closure resolves the per-CallSid subscription from the
	// live callManager.callSidIdx (typing it through a sip-local
	// StatusSubscription value to keep the bridge → sip cycle closed).
	//
	// The closure also returns the live sip.PreRegisteredSession so
	// emitStatusEvent can call session.MarkEmitted() after a successful
	// Enqueue (gates ghost terminal-only callbacks). *bridge.CallSession
	// satisfies sip.PreRegisteredSession structurally (SetAnsweredAt +
	// SetSIPFinalCode + CallSid + MarkEmitted) — return the live pointer
	// directly.
	handler.SetStatusEmission(statusWC, func(callSid string) (*sip.StatusSubscription, uint64, sip.PreRegisteredSession, bool) {
		call, ok := callManager.GetByCallSid(callSid)
		if !ok {
			return nil, 0, nil, false
		}
		live, isLive := call.(*bridge.CallSession)
		if !isLive {
			return nil, 0, nil, false // recently-terminated snapshot — no subscription surface
		}
		cfg := live.StatusCallback()
		if cfg == nil {
			return nil, 0, nil, false
		}
		// Allocate the next monotonic SequenceNumber on the live session.
		seq := live.NextSequenceNumber()
		// Project bridge.StatusCallbackConfig onto sip.StatusSubscription.
		// Events is a map[string]struct{} on both sides — share the
		// reference (read-only at the emit-helper layer).
		return &sip.StatusSubscription{
			URL:     cfg.URL,
			Method:  cfg.Method,
			Events:  cfg.Events,
			Trusted: cfg.Trusted,
		}, seq, live, true
	}, accountSid)

	// Register with sipgate — blocking; exits if initial registration fails.
	// Starts background re-register goroutine at 75% of server-granted Expires.
	registrar := sip.NewRegistrar(agent.Client, cfg, logger, metrics)
	if err := registrar.Register(ctx); err != nil {
		logger.Fatal().Err(err).Msg("SIP registration failed")
		os.Exit(1)
	}

	// Startup banner emitted AFTER registration succeeds.
	logger.Info().
		Str("account_sid", accountSid).
		Msg("ready to accept inbound calls")

	// HTTP server: /health and /metrics.
	// chi.Router hosts the /health, /metrics, and Twilio-compatible REST routes.
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)

	r.Get("/health", newHealthHandler(registrar, callManager, accountSid))

	r.Method("GET", "/metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}))

	// Mount Twilio-compatible REST API:
	//   GET  /2010-04-01/Accounts/{AccountSid}/Calls.json
	//   GET  /2010-04-01/Accounts/{AccountSid}/Calls/{CallSid}.json
	//   POST /2010-04-01/Accounts/{AccountSid}/Calls/{CallSid}.json
	// All routes require HTTP Basic Auth (accountSid:cfg.AuthToken) and emit
	// api_requests_total + api_request_duration_seconds metrics.
	//
	// voiceWC is the dedicated outbound HTTP client for the modify-call Url=
	// fetch path. Each outbound surface (Url= fetch, status callback, <Dial>
	// action callback) owns its own client to keep failures on one from
	// degrading the others — see internal/webhook package docstring.
	voiceWC := webhook.NewClient()
	// Separate signed *http.Client for the <Dial> action-callback POST so
	// X-Twilio-Signature is injected via the signing transport. voiceWC
	// stays in place for the Url= fetch surface.
	actionPoster := api.NewSignedActionPoster(cfg.AuthToken)
	api.Mount(r, callManager, callManager, accountSid, cfg.AuthToken, metrics, voiceWC, actionPoster, logger, forwarder, cfg)

	httpServer := &http.Server{
		Addr:    ":" + cfg.HTTPPort,
		Handler: r,
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
		Bool("rest_api_enabled", cfg.AuthToken != "").
		Msg("SIP registration active — ready to accept inbound calls")
	<-ctx.Done()
	runShutdown(ctx, handler, httpServer, callManager, registrar, logger)
	// defer agent.UA.Close() runs here — do NOT add another UA.Close().
}

// drainBudget is the wall-clock ceiling for callManager.DrainAll during
// graceful shutdown. Hard-coded const (NOT an env var) — operators have
// no good reason to tune it; the surrounding K8s 30s
// terminationGracePeriodSeconds is fixed. Sized at 15s — up from v2.1's
// 10s — to accommodate dual-leg <Dial> sessions (parent + dial leg)
// under sipgate's median BYE RTT.
//
// Total shutdown budget worst-case: 5s (HTTP stop-accept + finish in-flight)
// + 15s (drainBudget) + 5s (UNREGISTER) = 25s, comfortably under K8s 30s.
const drainBudget = 15 * time.Second

// shutdownHandler is the minimum surface runShutdown needs from the SIP
// INVITE handler: SetShutdown rejects new INVITEs (returns 503) for the
// rest of the process lifetime. *sip.Handler satisfies this directly; the
// fakeShutdownHandler in main_shutdown_test.go satisfies it for the
// order-of-operations test.
type shutdownHandler interface {
	SetShutdown()
}

// shutdownHTTPServer is the minimum surface runShutdown needs from the
// REST/observability HTTP server. *http.Server satisfies this directly.
type shutdownHTTPServer interface {
	Shutdown(ctx context.Context) error
}

// shutdownCallManager is the minimum surface runShutdown needs from the
// bridge: DrainAll terminates active calls (BYE every leg, dual-leg
// honored); ActiveCount reports remaining calls after drain so a
// non-zero count surfaces in shutdown logs. *bridge.CallManager
// satisfies this directly.
type shutdownCallManager interface {
	DrainAll(ctx context.Context)
	ActiveCount() int
}

// shutdownRegistrar is the minimum surface runShutdown needs from the SIP
// registrar: Unregister sends REGISTER with Expires:0 to gracefully
// de-register from sipgate. *sip.Registrar satisfies this directly.
type shutdownRegistrar interface {
	Unregister(ctx context.Context) error
}

// runShutdown executes the locked shutdown sequence:
//
//  1. SetShutdown — reject new INVITEs immediately (shutdownFlag set
//     BEFORE drain to prevent races between drain and a racing
//     accepted-then-orphaned INVITE).
//  2. httpServer.Shutdown(httpShutCtx, 5s) — stop accepting new HTTP
//     connections and finish in-flight requests. The 5s httpShutCtx is
//     intentionally shorter than urlFetchTimeout=15s in api/calls.go;
//     in-flight Url= TwiML fetches are aborted with context.Canceled
//     after 5s — accepted because the slowness is customer-side (the
//     customer's TwiML server), and the K8s 30s grace cannot
//     accommodate the full 15s worst-case (5s HTTP + 15s drain + 5s
//     unreg = 25s ≤ 30s).
//  3. callManager.DrainAll(drainCtx, drainBudget=15s) — BYE every leg
//     of every active call (dual-leg discipline). The polling loop
//     self-exits when all sessions self-delete OR when the budget
//     expires (warn-and-abandon).
//  4. registrar.Unregister(unregCtx, 5s) — send REGISTER Expires:0 to
//     sipgate. Sequenced AFTER drain so sipgate doesn't see a
//     "registration gone" + "active call still open" inconsistency
//     during the BYE flight time.
//
// Errors at steps 2 and 4 are logged-and-continued: the K8s 30s grace
// must not be wasted on a single failed step. Step 3 always runs through
// to its budget — DrainAll is not error-returning by design.
//
// Extracted from main()'s shutdown block so the order-of-operations test
// in main_shutdown_test.go can exercise it directly with fakes.
func runShutdown(ctx context.Context, handler shutdownHandler, httpServer shutdownHTTPServer, callManager shutdownCallManager, registrar shutdownRegistrar, logger zerolog.Logger) {
	logger.Info().Str("signal", ctx.Err().Error()).Msg("shutdown signal received — starting graceful drain")

	// 1. Reject new INVITEs immediately. shutdownFlag set BEFORE drain so a
	// racing INVITE that arrives mid-drain is rejected with 503 instead of
	// being accepted into a CallManager that's about to terminate.
	handler.SetShutdown()

	// 2. Graceful HTTP server: stop accepting new connections + finish
	// in-flight requests within budget. The 5s httpShutCtx is shorter than
	// urlFetchTimeout=15s in api/calls.go; in-flight Url= TwiML fetches are
	// aborted with context.Canceled after 5s. Accepted because the
	// customer's TwiML server is the cause of slow response, not us, and
	// the K8s 30s grace cannot accommodate the full 15s worst-case (5s
	// HTTP + 15s drain + 5s unreg = 25s ≤ 30s).
	httpShutCtx, httpShutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer httpShutCancel()
	if err := httpServer.Shutdown(httpShutCtx); err != nil {
		logger.Warn().Err(err).Msg("HTTP server shutdown error")
	} else {
		logger.Info().Msg("HTTP server stopped accepting new connections; in-flight drained")
	}

	// 3. BYE every leg of every active call; wait up to drainBudget (15s)
	// for sessions to self-exit. DrainAll routes through
	// CallSession.Terminate so dual-leg sessions BYE BOTH legs.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), drainBudget)
	defer drainCancel()
	callManager.DrainAll(drainCtx)
	logger.Info().Int("remaining_calls", callManager.ActiveCount()).Msg("BYE drain complete")

	// 4. SIP UNREGISTER — send after all calls are drained so sipgate
	// doesn't observe a "registration gone" + "active call still open"
	// inconsistency.
	unregCtx, unregCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer unregCancel()
	if err := registrar.Unregister(unregCtx); err != nil {
		logger.Warn().Err(err).Msg("UNREGISTER failed during shutdown")
	} else {
		logger.Info().Msg("SIP unregistered")
	}

	logger.Info().Msg("shutdown complete")
}

// ── /health handler ───────────────────────────────────────────────────────
//
// Extracted from the main() router setup so a contract test in main_test.go
// can wire fakes without spinning up the full bridge (which needs a UDP socket
// for the real *sip.Registrar). The locked four-field JSON contract is
// asserted by TestHealth_FourFieldContract_NoExtraneous.

// registrationProbe is the minimum surface newHealthHandler needs from the
// SIP registrar. *sip.Registrar.IsRegistered() satisfies this directly; a
// fakeRegistrar in main_test.go satisfies it for the contract test.
type registrationProbe interface {
	IsRegistered() bool
}

// forwardCounter is the minimum surface newHealthHandler needs from the
// CallManager: live counts of active calls and active forwarding legs.
// *bridge.CallManager satisfies this directly.
type forwardCounter interface {
	ActiveCount() int
	ActiveForwardCount() int
}

// newHealthHandler returns the /health http.HandlerFunc that emits the locked
// four-field JSON contract: { registered, account_sid, active_calls,
// active_forwards }. K8s-readiness scrapable; <5ms p99 even under load (the
// counters are sync.Map.Range + atomic.Load — no synchronous registrar reads
// or pool counts; see TestHealth_LatencyUnder5ms).
//
// Locked field set: NO `mode` field (streaming is the always-on default), NO
// `rtp_port_pool_in_use_ratio` field (operator-monitoring belongs in /metrics,
// not /health). Adding a 5th field requires updating both the handler AND
// TestHealth_FourFieldContract_NoExtraneous (visible in the PR diff).
func newHealthHandler(reg registrationProbe, fc forwardCounter, accountSid string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		type healthResp struct {
			Registered     bool   `json:"registered"`
			AccountSid     string `json:"account_sid"`
			ActiveCalls    int    `json:"active_calls"`
			ActiveForwards int    `json:"active_forwards"`
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(healthResp{
			Registered:     reg.IsRegistered(),
			AccountSid:     accountSid,
			ActiveCalls:    fc.ActiveCount(),
			ActiveForwards: fc.ActiveForwardCount(),
		})
	}
}
