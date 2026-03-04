package sip

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/emiago/sipgo"
	siplib "github.com/emiago/sipgo/sip"
	"github.com/rs/zerolog"
	"github.com/sipgate/audio-dock/internal/config"
)

// Registrar manages SIP REGISTER lifecycle for a single AoR (SIP_USER@SIP_DOMAIN).
type Registrar struct {
	client    *sipgo.Client
	registrar string // SIP_REGISTRAR — REGISTER Request-URI host
	domain    string // SIP_DOMAIN — AoR domain in From/To headers
	user      string // SIP_USER
	password  string // SIP_PASSWORD
	expires   int    // SIP_EXPIRES default 120 — requested expiry; server may grant different value
	log       zerolog.Logger
}

// NewRegistrar constructs a Registrar. client comes from Agent.Client.
func NewRegistrar(client *sipgo.Client, cfg config.Config, log zerolog.Logger) *Registrar {
	return &Registrar{
		client:    client,
		registrar: cfg.SIPRegistrar,
		domain:    cfg.SIPDomain,
		user:      cfg.SIPUser,
		password:  cfg.SIPPassword,
		expires:   cfg.SIPExpires,
		log:       log,
	}
}

// aorURI returns sip:user@domain — the Address-of-Record for From/To headers.
// sipgo's ClientRequestRegisterBuild derives From.User from the UA name (not the SIP user),
// so we pre-set From and To on every request to ensure the correct identity.
func (r *Registrar) aorURI() siplib.Uri {
	return siplib.Uri{Scheme: "sip", User: r.user, Host: r.domain}
}

// Register performs the initial REGISTER + Digest Auth, logs the server-granted Expires,
// and starts the background re-register goroutine. Blocks until first registration succeeds
// or returns an error (caller should log.Fatal + os.Exit on error — SIP-01).
func (r *Registrar) Register(ctx context.Context) error {
	expiry, err := r.doRegister(ctx)
	if err != nil {
		return err
	}
	r.log.Info().
		Str("registrar", r.registrar).
		Str("sip_user", r.user).
		Int("server_expires_s", int(expiry.Seconds())).
		Msg("SIP registration successful")
	go r.reregisterLoop(ctx, expiry)
	return nil
}

// doRegister performs a single REGISTER → 401 challenge → DoDigestAuth cycle.
// Returns the server-granted Expires duration from the 200 OK, or error.
// IMPORTANT: doRegister is called directly by reregisterLoop (not Register) to avoid
// goroutine leak (goroutine nesting anti-pattern — see RESEARCH.md Pitfall 6).
func (r *Registrar) doRegister(ctx context.Context) (time.Duration, error) {
	if r.client == nil {
		return 0, fmt.Errorf("REGISTER send: sipgo client is nil")
	}
	registrarURI := siplib.Uri{Host: r.registrar, Port: 5060}
	req := siplib.NewRequest(siplib.REGISTER, registrarURI)
	req.AppendHeader(siplib.NewHeader("Expires", strconv.Itoa(r.expires)))

	// Pre-set From and To with the correct AoR (sip:user@domain).
	// ClientRequestRegisterBuild skips From/To when already present, but if we don't set them
	// it derives From.User from the UA name string (not the SIP user) and To.User from the
	// Request-URI (empty for a registrar URI) — both wrong for sipgate's auth check.
	aor := r.aorURI()
	fromH := &siplib.FromHeader{Address: aor, Params: siplib.NewParams()}
	fromH.Params.Add("tag", siplib.GenerateTagN(16))
	req.AppendHeader(fromH)
	req.AppendHeader(&siplib.ToHeader{Address: aor})

	res, err := r.client.Do(ctx, req, sipgo.ClientRequestRegisterBuild)
	if err != nil {
		return 0, fmt.Errorf("REGISTER send: %w", err)
	}

	if res.StatusCode == 401 || res.StatusCode == 407 {
		res, err = r.client.DoDigestAuth(ctx, req, res, sipgo.DigestAuth{
			Username: r.user,
			Password: r.password,
		})
		if err != nil {
			return 0, fmt.Errorf("REGISTER digest auth: %w", err)
		}
	}

	// 403 = wrong credentials (sipgate behaviour) — log clearly and exit (SIP-01 success criterion 3)
	if res.StatusCode == 403 {
		return 0, fmt.Errorf("REGISTER rejected 403 Forbidden: invalid credentials (SIP_USER=%s)", r.user)
	}
	if res.StatusCode != 200 {
		return 0, fmt.Errorf("REGISTER rejected %d %s", res.StatusCode, res.Reason)
	}

	// Extract server-granted Expires — fallback to configured value if header absent
	serverExpiry := time.Duration(r.expires) * time.Second
	if h := res.GetHeader("Expires"); h != nil {
		if val, err2 := strconv.Atoi(h.Value()); err2 == nil && val > 0 {
			serverExpiry = time.Duration(val) * time.Second
		}
	}
	return serverExpiry, nil
}

// reregisterLoop re-registers at 75% of the server-granted interval (SIP-02).
// 75% matches diago's calcRetry ratio (see RESEARCH.md source: diago register_transaction.go).
// On ctx cancellation, sends UNREGISTER (Expires: 0) before returning.
// Uses doRegister (not Register) to prevent goroutine nesting (Pitfall 6).
func (r *Registrar) reregisterLoop(ctx context.Context, expiry time.Duration) {
	retryIn := time.Duration(float64(expiry) * 0.75)
	ticker := time.NewTicker(retryIn)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			r.log.Info().Msg("SIP re-register loop stopping — sending UNREGISTER")
			unregCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := r.Unregister(unregCtx); err != nil {
				r.log.Warn().Err(err).Msg("UNREGISTER failed during shutdown")
			}
			return
		case <-ticker.C:
			newExpiry, err := r.doRegister(ctx)
			if err != nil {
				r.log.Error().Err(err).Msg("SIP re-registration failed — will retry next tick")
				continue // keep ticker running; transient network error may recover
			}
			r.log.Info().
				Int("server_expires_s", int(newExpiry.Seconds())).
				Msg("SIP re-registration successful")
			// If server grants a different expiry, reset the ticker
			if newRetry := time.Duration(float64(newExpiry) * 0.75); newRetry != retryIn {
				retryIn = newRetry
				ticker.Reset(retryIn)
			}
		}
	}
}

// Unregister sends REGISTER with Expires: 0 (de-registration per RFC 3261 §10.2.2).
// Called on graceful shutdown from reregisterLoop, and will be called from Phase 8 (LCY-01).
func (r *Registrar) Unregister(ctx context.Context) error {
	registrarURI := siplib.Uri{Host: r.registrar, Port: 5060}
	req := siplib.NewRequest(siplib.REGISTER, registrarURI)
	req.AppendHeader(siplib.NewHeader("Expires", "0"))

	aor := r.aorURI()
	fromH := &siplib.FromHeader{Address: aor, Params: siplib.NewParams()}
	fromH.Params.Add("tag", siplib.GenerateTagN(16))
	req.AppendHeader(fromH)
	req.AppendHeader(&siplib.ToHeader{Address: aor})

	res, err := r.client.Do(ctx, req, sipgo.ClientRequestRegisterBuild)
	if err != nil {
		return fmt.Errorf("UNREGISTER send: %w", err)
	}
	if res.StatusCode == 401 || res.StatusCode == 407 {
		res, err = r.client.DoDigestAuth(ctx, req, res, sipgo.DigestAuth{
			Username: r.user,
			Password: r.password,
		})
		if err != nil {
			return fmt.Errorf("UNREGISTER digest auth: %w", err)
		}
	}
	if res.StatusCode != 200 {
		return fmt.Errorf("UNREGISTER rejected %d", res.StatusCode)
	}
	return nil
}
