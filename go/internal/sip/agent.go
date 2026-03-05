package sip

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/emiago/sipgo"
	"github.com/rs/zerolog"
	slogzerolog "github.com/samber/slog-zerolog"
	"github.com/sipgate/audio-dock/internal/config"
)

// Agent bundles the sipgo UserAgent, Server, and Client for the entire service lifetime.
// A single UA is shared — both UAS (inbound) and UAC (outbound REGISTER) use the same transport.
type Agent struct {
	UA     *sipgo.UserAgent
	Server *sipgo.Server
	Client *sipgo.Client
}

// NewAgent creates the sipgo Agent from config. cfg.SIPDomain is the UA hostname (From: domain).
// sipgo's internal slog output is bridged to zerolog via samber/slog-zerolog so all logs are
// unified JSON on stdout.
func NewAgent(cfg config.Config, log zerolog.Logger) (*Agent, error) {
	ua, err := sipgo.NewUA(
		sipgo.WithUserAgentHostname(cfg.SIPDomain),
		sipgo.WithUserAgent("audio-dock/2.0"),
	)
	if err != nil {
		return nil, fmt.Errorf("create sipgo UserAgent: %w", err)
	}

	// Bridge sipgo's slog output into zerolog stream
	zerologBase := zerolog.New(os.Stdout).With().Timestamp().Logger()
	handler := slogzerolog.Option{
		Level:  slog.LevelDebug,
		Logger: &zerologBase,
	}.NewZerologHandler()
	sipSlogLogger := slog.New(handler)

	srv, err := sipgo.NewServer(ua, sipgo.WithServerLogger(sipSlogLogger))
	if err != nil {
		ua.Close()
		return nil, fmt.Errorf("create sipgo Server: %w", err)
	}

	cli, err := sipgo.NewClient(ua, sipgo.WithClientLogger(sipSlogLogger))
	if err != nil {
		ua.Close()
		return nil, fmt.Errorf("create sipgo Client: %w", err)
	}

	return &Agent{UA: ua, Server: srv, Client: cli}, nil
}
