package sip

import (
	"context"

	"github.com/emiago/sipgo"
	siplib "github.com/emiago/sipgo/sip"
)

// ── Production sipgo-backed DialClientFactory ─────────────────────────────────
//
// sipgoDialFactory is the production implementation of DialClientFactory.
// It wraps a *sipgo.DialogClientCache (which itself wraps the shared
// agent.Client) and translates the Forwarder-facing surface
// (DialClient interface) into sipgo's *DialogClientSession surface.
//
// Why DialogClientCache instead of the raw DialogUA: when the callee hangs
// up first, sipgate sends BYE to our SIP Server (port 5060). The Server's
// OnBye handler must be able to look up the matching client-side dialog
// and call ReadBye on it to drive the dialog to terminated state. The
// raw DialogUA has no dialog cache, so BYE arrives at the server with
// no route, sipgo's transaction layer answers "Call/Transaction Does Not
// Exist", and our awaitDialogEnd never observes dlg.Done() — leaving the
// API handler that triggered the dial deadlocked on PerformDial.
// DialogClientCache stores established dialogs by ID and exposes a
// ReadBye method for exactly this routing concern.
//
// The factory is constructed once per Forwarder lifetime so the underlying
// DialogClientCache's ContactHeader is consistent across all <Dial>
// invocations.

// sipgoDialFactory is the production DialClientFactory. It is unexported —
// callers always go through NewForwarder, which wires it up.
type sipgoDialFactory struct {
	cache *sipgo.DialogClientCache
}

// newSipgoDialFactory constructs a DialClientFactory backed by a sipgo
// DialogClientCache. The cache shares the provided *sipgo.Client
// (invariant: same socket as Registrar so the UDP source-port pinhole
// survives). contactHDR is the SIP Contact header advertised on outbound
// INVITEs — points at our externally-reachable IP:port.
func newSipgoDialFactory(client *sipgo.Client, contactHDR siplib.ContactHeader) *sipgoDialFactory {
	return &sipgoDialFactory{
		cache: sipgo.NewDialogClientCache(client, contactHDR),
	}
}

// Dial is the DialClientFactory.Dial implementation. It builds an INVITE via
// sipgo.DialogClientCache.Invite (which generates a fresh Call-ID, From-tag,
// and Via-branch, plus auto-registers the dialog in the cache
// once Established) then drives WaitAnswer with the digest credentials.
// The OnResponse callback fires for every (provisional + final) response —
// the Forwarder uses it to record the auth_challenge_kind metric.
func (f *sipgoDialFactory) Dial(
	ctx context.Context,
	recipient siplib.Uri,
	from siplib.Uri,
	displayName string,
	ppi *siplib.Uri,
	body []byte,
	auth DialAuth,
	onResponse func(*siplib.Response) error,
) (DialClient, error) {
	// Build From header. sipgo's DialogClientCache.Invite generates Call-ID
	// and Via; we override From so the caller-ID fallback chain is
	// honoured at the wire level.
	//
	// displayName, when non-empty, is rendered as `"display" <sip:user@host>`
	// per RFC 3261 §20.20. Most carriers honour the From display-name as the
	// Caller-ID shown to the callee, even when the addr-spec carries the SIP
	// auth username. This is the cleanest sipgate-compat path: From's
	// addr-spec satisfies "Username in From Field required" while the
	// display-name carries the desired Caller-ID number.
	fromHeader := &siplib.FromHeader{
		DisplayName: displayName,
		Address:     from,
		Params:      siplib.NewParams(),
	}
	fromHeader.Params.Add("tag", siplib.GenerateTagN(16))

	// To header: target URI (the trunk DID we're calling).
	toHeader := &siplib.ToHeader{Address: recipient}

	// Content-Type explicitly — sipgo doesn't auto-add it for INVITE bodies.
	contentType := siplib.NewHeader("Content-Type", "application/sdp")

	// Optional P-Preferred-Identity header (RFC 3325 §9.2) — the identity
	// the UA WOULD LIKE the trust-domain proxy to assert on its behalf.
	// PPI is the correct header for a UA to send: P-Asserted-Identity is
	// what the network puts on the wire AFTER authenticating, whereas PPI
	// is what we as the originating client REQUEST. The trust domain
	// (sipgate's SBC) decides whether to honour the PPI and may issue PAI
	// downstream based on it.
	headers := []siplib.Header{fromHeader, toHeader, contentType}
	if ppi != nil {
		ppiValue := "<" + ppi.String() + ">"
		headers = append(headers, siplib.NewHeader("P-Preferred-Identity", ppiValue))
	}

	dlg, err := f.cache.Invite(ctx, recipient, body, headers...)
	if err != nil {
		return nil, err
	}

	// WaitAnswer drives the dialog until a final response arrives. sipgo
	// applies digest authentication automatically when AnswerOptions carries
	// Username+Password — first-attempt 401/407 → re-INVITE with digest, no
	// manual plumbing needed in the Forwarder.
	waitErr := dlg.WaitAnswer(ctx, sipgo.AnswerOptions{
		OnResponse: onResponse,
		Username:   auth.Username,
		Password:   auth.Password,
	})

	wrapper := &sipgoDialClient{
		dlg:           dlg,
		finalResponse: dlg.InviteResponse, // sipgo populates this on final
	}

	return wrapper, waitErr
}

// ReadBye routes an incoming BYE to the matching outbound dialog managed by
// this factory's cache. Returns sipgo's ErrDialogDoesNotExists when the BYE
// belongs to no dialog in this cache — caller should fall back to the
// server-side dialog cache (see Handler.onBye in handler.go).
func (f *sipgoDialFactory) ReadBye(req *siplib.Request, tx siplib.ServerTransaction) error {
	return f.cache.ReadBye(req, tx)
}

// ── sipgoDialClient: wraps *DialogClientSession to satisfy DialClient ─────────

type sipgoDialClient struct {
	dlg           *sipgo.DialogClientSession
	finalResponse *siplib.Response
}

func (c *sipgoDialClient) FinalResponse() *siplib.Response { return c.finalResponse }

func (c *sipgoDialClient) Ack(ctx context.Context) error { return c.dlg.Ack(ctx) }

func (c *sipgoDialClient) Bye(ctx context.Context) error { return c.dlg.Bye(ctx) }

// Done returns a channel closed when the dialog terminates. sipgo exposes
// this via Dialog.Context() — the underlying ctx is canceled on dialog end
// (BYE from either side, transaction timeout, or explicit Close).
func (c *sipgoDialClient) Done() <-chan struct{} { return c.dlg.Context().Done() }

func (c *sipgoDialClient) Close() error { return c.dlg.Close() }
