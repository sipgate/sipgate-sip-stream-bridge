package api

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/sipgate/sipgate-sip-stream-bridge/internal/bridge"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/config"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/sip"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/twiml"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/webhook"
)

// NewSignedActionPoster builds the *http.Client used for <Dial> action-
// callback POSTs. The inner Transport mirrors the voiceWC tuning (Url=
// fetch surface). The signingTransport wrapper injects X-Twilio-Signature.
//
// The signed-poster is carved off voiceWC so the X-Twilio-Signature header
// lands on every <Dial> action callback POST. The verbatim-URL signing seam
// (SignWithContext) is threaded by midCallAdapter.fireActionCallback per
// request.
//
// authToken MUST equal cfg.AuthToken — the same token used as the Basic Auth
// password for inbound REST. Tests may pass any constant.
func NewSignedActionPoster(authToken string) *http.Client {
	tr := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   2 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   3 * time.Second,
		ResponseHeaderTimeout: 8 * time.Second,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       30 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{
		Transport: webhook.SigningTransportFor(tr, authToken),
		Timeout:   15 * time.Second,
	}
}

// midCallAdapter wraps a live *bridge.CallSession as a twiml.MidCallTarget
// and twiml.DialTarget.
//
// The adapter lives in the api package (NOT in bridge or twiml) for cycle-
// avoidance: bridge ← api → twiml is a clean DAG; bridge → twiml would
// require bridge to know about MidCallTarget, which couples the SIP/RTP
// runtime to the TwiML verb model unnecessarily.
//
// Fields:
//   - forwarder: *sip.Forwarder — used by PerformDial to send the outbound INVITE
//   - manager: *bridge.CallManager — AcquirePort / ReleasePort for the callee leg
//   - webhookC: webhookFetcher — action callback fire-and-forget POST
//   - cfg: config.Config — SDPContactIP for SDP offer construction
type midCallAdapter struct {
	session      *bridge.CallSession
	manager      *bridge.CallManager
	forwarder    *sip.Forwarder
	webhookC     webhookFetcher // used for Url= fetch
	actionPoster *http.Client   // signed POSTer for <Dial> action callback
	cfg          config.Config
	logger       zerolog.Logger // per-call enriched logger (call_sid, account_sid)
}

// newMidCallAdapter builds an adapter for the given session, enriching the
// supplied logger with call_sid + account_sid fields so verb-side warn logs
// always carry the call identity.
//
// The supplied logger is captured by value (zerolog.Logger is a small struct
// holding a pointer to the writer); concurrent Log() calls return the same
// enriched logger every time.
//
// actionPoster is the signed *http.Client built once at server startup
// (api.NewSignedActionPoster(cfg.AuthToken)). It is used INSTEAD of
// webhookC for the <Dial> action-callback POST so X-Twilio-Signature lands
// on every request. webhookC is retained for the Url= fetch. May be nil
// in test fixtures that never exercise the action-callback path.
func newMidCallAdapter(
	session *bridge.CallSession,
	manager *bridge.CallManager,
	forwarder *sip.Forwarder,
	wc webhookFetcher,
	actionPoster *http.Client,
	cfg config.Config,
	logger zerolog.Logger,
) *midCallAdapter {
	return &midCallAdapter{
		session:      session,
		manager:      manager,
		forwarder:    forwarder,
		webhookC:     wc,
		actionPoster: actionPoster,
		cfg:          cfg,
		logger: logger.With().
			Str("call_sid", session.CallSid()).
			Str("account_sid", session.AccountSid()).
			Logger(),
	}
}

// Terminate forwards to the wrapped session's Terminate. Idempotent — second
// call is a no-op at the field-set level (sync.Once inside CallSession).
//
// reason flows through to the recentlyTerminated snapshot's status field;
// it is one of {"completed", "hangup", "failed", "busy", "no-answer",
// "canceled"}.
func (a *midCallAdapter) Terminate(reason string) error {
	return a.session.Terminate(reason)
}

// Log returns the per-call enriched logger. Used by the twiml dispatcher's
// warn-and-skip path so each unrecognized / unsupported verb log carries the
// owning call_sid + account_sid for ops correlation.
func (a *midCallAdapter) Log() *zerolog.Logger {
	return &a.logger
}

// ── DialTarget implementation ─────────────────────────────────────────────────

// PrepareDial implements twiml.DialTarget.PrepareDial. It is the Privacy Gate:
//  1. Close WS stream cleanly (so the bot does not hear the forwarded conversation).
//  2. Acquire an outbound RTP port from the port pool.
//  3. Construct the callee Leg + install it on the session at legs[1].
//
// On any error the stream may already be closed (the bot is unaware of the
// call from this point forward) but no INVITE is in flight — the caller is
// terminated by dialHandler with reason="failed".
//
// The returned DialHandle's Release() returns the acquired RTP port to the
// pool. Release is deferred in dialHandler so it fires on all exit paths.
func (a *midCallAdapter) PrepareDial(opts twiml.DialOpts) (twiml.DialHandle, error) {
	_ = opts // CallerID / Timeout etc. consumed by PerformDial; not needed here

	a.logger.Info().Msg("midCallAdapter: PrepareDial — closing WS stream (Privacy Gate)")

	// 1. Privacy Gate: close WS stream BEFORE port allocation. Even if
	//    AcquirePort fails afterward, the bot is already disconnected (which
	//    is the correct privacy behavior — bot must not hear what follows).
	if err := a.session.CloseStream("dial-forward"); err != nil {
		a.logger.Error().Err(err).Msg("midCallAdapter: PrepareDial — CloseStream failed")
		return nil, fmt.Errorf("PrepareDial: CloseStream: %w", err)
	}

	// 2. Acquire outbound RTP port (non-blocking; fails fast on exhaustion).
	port, err := a.manager.AcquirePort()
	if err != nil {
		a.logger.Error().Err(err).Msg("midCallAdapter: PrepareDial — AcquirePort failed")
		return nil, fmt.Errorf("PrepareDial: AcquirePort: %w", err)
	}

	// 3. Construct callee Leg + install at legs[1].
	leg := bridge.NewCalleeLeg(port, a.cfg.SDPContactIP)
	a.session.SetLeg(1, leg)

	a.logger.Info().Int("rtp_port", port).Msg("midCallAdapter: PrepareDial — callee leg ready")

	return &dialHandle{
		adapter: a,
		leg:     leg,
		port:    port,
	}, nil
}

// PerformDial implements twiml.DialTarget.PerformDial.
//
//  1. Translate twiml.DialOpts → sip.DialOpts (including CallerFrom from the
//     inbound session's From URI for ANI preservation).
//  2. Call Forwarder.Dial — blocks until the outbound dialog terminates.
//  3. Fire the action callback in a goroutine (best-effort; no retry).
//  4. Translate sip.DialResult → twiml.DialResult.
func (a *midCallAdapter) PerformDial(ctx context.Context, target string, opts twiml.DialOpts, handle twiml.DialHandle) (*twiml.DialResult, error) {
	a.logger.Info().Str("target", target).Msg("midCallAdapter: PerformDial — invoking Forwarder.Dial")

	sipOpts := sip.DialOpts{
		CallerID:     opts.CallerID,
		Timeout:      opts.Timeout,
		TimeLimit:    opts.TimeLimit,
		HangupOnStar: opts.HangupOnStar,
		Action:       opts.Action,
		Method:       opts.Method,
		CallerFrom:   a.session.From(), // preserve-ANI last-resort fallback
		// Thread the per-<Dial>-leg status callback subscription
		// (resolved by twiml/verb_dial.go from <Dial>/<Number> attrs with
		// Number-overrides-Dial precedence) into the sip layer where the
		// Forwarder consumes it for emitDialInitiated/Ringing/Answered.
		StatusCallback:        opts.StatusCallback,
		StatusCallbackMethod:  opts.StatusCallbackMethod,
		StatusCallbackEvents:  opts.StatusCallbackEvents,
		// DTMFChan is wired elsewhere; left nil here so hangupOnStar in
		// forwarder tests covers the nil-channel guard.
	}

	h := handle.(*dialHandle)
	sipResult, dialErr := a.forwarder.Dial(ctx, a.session.CallSid(), target, sipOpts, h.leg)

	// Translate sip.DialResult → twiml.DialResult unconditionally so that
	// dialHandler can inspect the result.Status even on the error path (e.g.
	// "busy" or "no-answer" come back as errors from Forwarder.Dial but carry
	// a meaningful result.Status that must reach twilioReasonFromDialResult).
	var result *twiml.DialResult
	if sipResult != nil {
		result = &twiml.DialResult{
			Status:       sipResult.Status,
			Reason:       sipResult.Reason,
			DialCallSid:  sipResult.DialCallSid,
			Duration:     sipResult.Duration,
			SIPFinalCode: sipResult.SIPFinalCode,
			DialedTarget: sipResult.DialedTarget,
		}
	}

	// Action callback: fire before returning so dialHandler sees the final
	// state. Best-effort goroutine.
	if opts.Action != "" {
		go a.fireActionCallback(opts.Action, opts.Method, sipResult)
	}

	if dialErr != nil {
		return result, dialErr
	}
	return result, nil
}

// fireActionCallback posts the Twilio DialCallStatus / DialCallSid /
// DialCallDuration form body to opts.Action (best-effort, no retries).
// Fired in a goroutine from PerformDial.
//
// Replaces the unsigned webhookC.FetchWithFallback path with a dedicated
// signed *http.Client (actionPoster). The signingTransport wrapper injects
// X-Twilio-Signature; the SignWithContext seam threads the verbatim URL
// bytes so the signature matches what Twilio's RequestValidator computes
// on the customer side (req.URL.String() can normalize percent encoding,
// IDN punycode, etc.).
//
// Fallback when actionPoster is nil (test fixtures that pass nil): falls
// back to the unsigned webhookC.FetchWithFallback path so existing
// body-capture tests keep working without modification.
func (a *midCallAdapter) fireActionCallback(actionURL, method string, result *sip.DialResult) {
	if actionURL == "" {
		return
	}
	if result == nil {
		result = &sip.DialResult{Status: "failed"}
	}

	// Map DialResult.Status to Twilio DialCallStatus enum.
	dialCallStatus := twilioDialCallStatus(result.Status)

	form := url.Values{}
	form.Set("DialCallStatus", dialCallStatus)
	form.Set("DialCallSid", result.DialCallSid)
	form.Set("DialCallDuration", strconv.Itoa(int(result.Duration.Seconds())))
	form.Set("Direction", "outbound-dial")
	form.Set("CallSid", a.session.CallSid())
	form.Set("AccountSid", a.session.AccountSid())

	if method == "" {
		method = http.MethodPost
	}
	method = strings.ToUpper(method)

	// Fallback path: when actionPoster is nil (test fixtures that don't
	// wire it), fire through the legacy webhookC so body-capture based
	// tests continue to assert correctly. New tests SHOULD pass an
	// actionPoster to exercise the signed path.
	if a.actionPoster == nil {
		ctx := context.Background()
		target := webhook.FetchTarget{
			URL:    actionURL,
			Method: method,
			Body:   form.Encode(),
		}
		if _, err := a.webhookC.FetchWithFallback(ctx, target, webhook.FetchTarget{}); err != nil {
			a.logger.Warn().Err(err).Str("action_url", actionURL).Msg("midCallAdapter: action callback POST failed (best-effort, unsigned legacy path)")
		}
		return
	}

	// Signed path: use actionPoster (signingTransport-wrapped) and stash
	// the verbatim actionURL via SignWithContext so the signing transport
	// reads the unmodified bytes (defeats req.URL.String() normalization,
	// IDN punycode, etc.).
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var bodyReader io.Reader
	encoded := ""
	if method == http.MethodPost {
		encoded = form.Encode()
		bodyReader = strings.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, actionURL, bodyReader)
	if err != nil {
		a.logger.Warn().Err(err).Str("action_url", actionURL).Msg("midCallAdapter: action callback build-request failed")
		return
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.ContentLength = int64(len(encoded))
	}
	// Verbatim URL signing.
	req = req.WithContext(webhook.SignWithContext(req.Context(), actionURL))

	resp, err := a.actionPoster.Do(req)
	if err != nil {
		a.logger.Warn().Err(err).Str("action_url", actionURL).Msg("midCallAdapter: action callback POST failed (best-effort, signed)")
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
}

// twilioDialCallStatus maps a sip.DialResult.Status to the Twilio
// DialCallStatus enum value that appears in action callback POST bodies.
//
//	"answered"     → "completed"   (Twilio's convention: talk-time success)
//	"busy"         → "busy"
//	"no-answer"    → "no-answer"
//	"canceled"     → "canceled"
//	"hangup-star"  → "completed"   (custom hangup; surfaces as completed)
//	default        → "failed"
func twilioDialCallStatus(status string) string {
	switch status {
	case "answered", "hangup-star":
		return "completed"
	case "busy":
		return "busy"
	case "no-answer":
		return "no-answer"
	case "canceled":
		return "canceled"
	default:
		return "failed"
	}
}

// ── dialHandle ────────────────────────────────────────────────────────────────

// dialHandle is the opaque resource handle returned by PrepareDial.
// dialHandler defers handle.Release() so RTP ports are always returned to
// the pool regardless of the PerformDial outcome.
type dialHandle struct {
	adapter *midCallAdapter
	leg     *bridge.Leg
	port    int
}

// Release returns the acquired RTP port to the port pool. The callee Leg
// itself is cleaned up by session.Terminate's BYE-all-legs path; we only
// free the port here (the Leg's resources are already nil at port-free time).
func (h *dialHandle) Release() {
	h.adapter.manager.ReleasePort(h.port)
}
