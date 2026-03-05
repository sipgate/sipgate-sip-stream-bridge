# Phase 5: SIP Registration - Research

**Researched:** 2026-03-03
**Domain:** SIP UAC registration with Digest Auth using emiago/sipgo v1.2.0
**Confidence:** HIGH

---

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| SIP-01 | Service registers with sipgate SIP trunking on startup using configured credentials (Digest Auth via sipgo v1.2.0) | `Client.Do` + `ClientRequestRegisterBuild` + `Client.DoDigestAuth` — fully documented pattern; confirmed from pkg.go.dev API |
| SIP-02 | Service automatically re-registers at 90% of server-granted Expires timer (re-register goroutine with `ClientRequestRegisterBuild`) | `res.GetHeader("Expires").Value()` + `strconv.Atoi` extracts server-granted value; ticker at 75–90% confirmed pattern from diago source |
</phase_requirements>

---

## Summary

Phase 5 implements SIP registration: the service starts, sends REGISTER to sipgate's registrar, handles the 401 Digest Auth challenge, and then maintains registration via a periodic re-register goroutine. The sipgo v1.2.0 API is well-suited to this — `Client.Do` with `ClientRequestRegisterBuild` handles the initial send, `Client.DoDigestAuth` handles the 401 round-trip automatically, and a `time.NewTicker` at 75–90% of the server-granted Expires drives re-registration.

The key architectural decision for this phase is: **registration lives in a single file (`internal/sip/registrar.go`) with a blocking `Register()` call at startup and a self-managing goroutine for re-registration**. The main.go signal context drives shutdown — when cancelled, the goroutine sends UNREGISTER (Expires: 0) and exits. The UserAgent, Server, and Client are created in this phase and passed to later phases.

The three known sipgo issues (#116 graceful shutdown, #206 UDP MTU, #251 folded headers) are all documented with mitigations. Issue #206 and #251 are LOW risk for this phase: REGISTER messages are small (well under MTU) and sipgate does not appear to use folded headers. Issue #116 is tracked for Phase 8 (LCY-01), not Phase 5.

**Primary recommendation:** Implement registration in `internal/sip/registrar.go` as a `Registrar` struct with `Register(ctx)` and `Unregister(ctx)` methods, using sipgo's `Client.Do` + `ClientRequestRegisterBuild` + `DoDigestAuth` pattern. Log server-granted Expires on first registration. Re-register with a ticker at 75% of server-granted interval (matches diago's `calcRetry` pattern).

---

## Standard Stack

### Core

| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `github.com/emiago/sipgo` | v1.2.0 | SIP UA, REGISTER request, Digest Auth, Server handler registration | Already decided; pure Go, stable v1.x API, LiveKit production-proven |
| `github.com/emiago/sipgo/sip` | v1.2.0 (sub-package) | `sip.Uri`, `sip.NewRequest`, `sip.Header`, `sip.ContactHeader` types | Same module — no additional install |

### Supporting

| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `github.com/samber/slog-zerolog` | v1.x | slog.Handler backed by zerolog — lets sipgo's `WithClientLogger`/`WithServerLogger` route to zerolog | Use to avoid two logging systems; this is optional but clean |
| `go-simpler.org/env` | v0.12.0 | Already installed in Phase 4 | Already provides `cfg.SIPExpires` (default 120) |

### Alternatives Considered

| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| `DoDigestAuth` (two-step: `Do` → check 401 → `DoDigestAuth`) | `TransactionDigestAuth` (lower-level) | `DoDigestAuth` returns the final response directly and is the idiomatic pattern; use `TransactionDigestAuth` only if you need raw transaction control |
| Ticker at 75% of Expires | `time.AfterFunc` | Ticker is cleaner for a periodic loop; AfterFunc is fine for one-shot re-arm but adds complexity — use Ticker |
| Custom goroutine shutdown | `sipgo.Server.Close()` | `Server.Close()` just returns nil in v1.2.0 — must cancel context and close UA explicitly |

**Installation:**

```bash
go get github.com/emiago/sipgo@v1.2.0
go get github.com/pion/sdp/v3@v3.0.18
go get github.com/pion/rtp@v1.10.1
# Optional — only if sipgo internal log routing to zerolog is desired:
go get github.com/samber/slog-zerolog@latest
```

Note: `pion/sdp` and `pion/rtp` are not used in Phase 5 but should be added to go.mod now so Phase 6 does not require a dependency update during a coding sprint.

---

## Architecture Patterns

### Recommended Project Structure for Phase 5

```
internal/
└── sip/
    ├── agent.go        # NewUA, NewServer, NewClient — wires sipgo UA/Server/Client from cfg
    └── registrar.go    # Registrar struct: Register(), Unregister(), re-register loop
```

Phase 5 creates `internal/sip/` as the SIP subsystem. Phase 6 adds `handler.go` and `sdp.go` to the same package.

### Pattern 1: UA + Server + Client Creation (agent.go)

Both Server (UAS) and Client (UAC REGISTER) share a single UserAgent. This is the canonical sipgo pattern.

```go
// Source: https://pkg.go.dev/github.com/emiago/sipgo@v1.2.0
// Source: https://github.com/emiago/sipgo — README and integration tests

package sip

import (
    "github.com/emiago/sipgo"
    siplib "github.com/emiago/sipgo/sip"
    "github.com/rs/zerolog"
)

type Agent struct {
    UA     *sipgo.UserAgent
    Server *sipgo.Server
    Client *sipgo.Client
}

func NewAgent(cfg Config, log zerolog.Logger) (*Agent, error) {
    // WithUserAgentHostname: "represents FQDN of user that can be presented in From header"
    // Set to SIP_DOMAIN — this is the From domain (e.g. sipconnect.sipgate.de)
    ua, err := sipgo.NewUA(
        sipgo.WithUserAgentHostname(cfg.SIPDomain),
    )
    if err != nil {
        return nil, fmt.Errorf("create UA: %w", err)
    }

    srv, err := sipgo.NewServer(ua)
    if err != nil {
        ua.Close()
        return nil, fmt.Errorf("create server: %w", err)
    }

    cli, err := sipgo.NewClient(ua)
    if err != nil {
        ua.Close()
        return nil, fmt.Errorf("create client: %w", err)
    }

    return &Agent{UA: ua, Server: srv, Client: cli}, nil
}
```

**Optional: wire zerolog into sipgo's slog output**

sipgo v1.2.0 uses `log/slog` internally (introduced in v0.30.0). Both `NewServer` and `NewClient` accept `WithServerLogger(*slog.Logger)` / `WithClientLogger(*slog.Logger)`. Use `samber/slog-zerolog` to route sipgo's internal logs into the zerolog stream:

```go
// Source: https://pkg.go.dev/github.com/samber/slog-zerolog
import (
    "log/slog"
    slogzerolog "github.com/samber/slog-zerolog"
)

zerologBase := zerolog.New(os.Stdout).With().Timestamp().Logger()
slogHandler := slogzerolog.Option{
    Level:  slog.LevelDebug,
    Logger: &zerologBase,
}.NewZerologHandler()
sipSlogLogger := slog.New(slogHandler)

srv, _ := sipgo.NewServer(ua, sipgo.WithServerLogger(sipSlogLogger))
cli, _ := sipgo.NewClient(ua, sipgo.WithClientLogger(sipSlogLogger))
```

This is optional — if not done, sipgo logs to the default `slog` global (stdout JSON if not configured). For a clean production binary where all logs are zerolog JSON, wire this up.

### Pattern 2: REGISTER with Digest Auth — the two-step flow (registrar.go)

sipgate uses 401 Unauthorized to trigger Digest Auth. The flow is:

1. `Client.Do(ctx, req, ClientRequestRegisterBuild)` — sends REGISTER without auth header; returns 401 response
2. `Client.DoDigestAuth(ctx, req, res401, DigestAuth{...})` — builds new REGISTER with `Authorization` header computed from the 401's `WWW-Authenticate`; sends it; returns 200 OK or error

```go
// Source: https://pkg.go.dev/github.com/emiago/sipgo@v1.2.0
// Source: diago register_transaction.go (Expires extraction pattern confirmed)
// Source: sipgate community configs (registrar = SIP_REGISTRAR = sipconnect.sipgate.de)

package sip

import (
    "context"
    "fmt"
    "strconv"
    "time"

    "github.com/emiago/sipgo"
    siplib "github.com/emiago/sipgo/sip"
    "github.com/rs/zerolog"
)

type Registrar struct {
    client     *sipgo.Client
    registrar  string   // SIP_REGISTRAR e.g. "sipconnect.sipgate.de"
    user       string   // SIP_USER      e.g. "e12345p0"
    password   string   // SIP_PASSWORD
    expires    int      // SIP_EXPIRES   e.g. 120 (default)
    log        zerolog.Logger
}

// Register sends the initial REGISTER, handles the 401 challenge, logs the result,
// and starts the background re-register goroutine. Blocks until registration succeeds
// or ctx is cancelled. Exits non-zero (via caller) if registration fails on startup.
func (r *Registrar) Register(ctx context.Context) error {
    expiry, err := r.doRegister(ctx)
    if err != nil {
        return err // caller: log.Fatal().Err(err).Msg("registration failed"); os.Exit(1)
    }

    r.log.Info().
        Str("registrar", r.registrar).
        Str("sip_user", r.user).
        Int("server_expires_s", int(expiry.Seconds())).
        Msg("SIP registration successful")

    go r.reregisterLoop(ctx, expiry)
    return nil
}

// doRegister performs a single REGISTER + Digest Auth cycle.
// Returns the server-granted Expires duration, or error.
func (r *Registrar) doRegister(ctx context.Context) (time.Duration, error) {
    registrarURI := siplib.Uri{Host: r.registrar, Port: 5060}
    req := siplib.NewRequest(siplib.REGISTER, registrarURI)
    req.AppendHeader(siplib.NewHeader("Expires", strconv.Itoa(r.expires)))

    // Step 1: send without auth (expect 401)
    res, err := r.client.Do(ctx, req, sipgo.ClientRequestRegisterBuild)
    if err != nil {
        return 0, fmt.Errorf("REGISTER send: %w", err)
    }

    // Step 2: handle 401 Digest Auth challenge
    if res.StatusCode == 401 || res.StatusCode == 407 {
        res, err = r.client.DoDigestAuth(ctx, req, res, sipgo.DigestAuth{
            Username: r.user,
            Password: r.password,
        })
        if err != nil {
            return 0, fmt.Errorf("REGISTER digest auth: %w", err)
        }
    }

    // SIP-01 success criterion: 403 Forbidden = wrong credentials (not a transient error)
    if res.StatusCode == 403 {
        return 0, fmt.Errorf("REGISTER rejected 403 Forbidden: invalid credentials (SIP_USER=%s)", r.user)
    }
    if res.StatusCode != 200 {
        return 0, fmt.Errorf("REGISTER rejected %d %s", res.StatusCode, res.Reason)
    }

    // Extract server-granted Expires — use strconv.Atoi on header Value()
    // Source: diago register_transaction.go (confirmed pattern)
    serverExpiry := time.Duration(r.expires) * time.Second // fallback
    if h := res.GetHeader("Expires"); h != nil {
        if val, err := strconv.Atoi(h.Value()); err == nil && val > 0 {
            serverExpiry = time.Duration(val) * time.Second
        }
    }
    return serverExpiry, nil
}

// reregisterLoop re-registers at 75% of the server-granted interval.
// On context cancellation, sends UNREGISTER (Expires: 0) before exiting.
// Source: diago calcRetry uses 75% — matches SIP-02 requirement (75–90%)
func (r *Registrar) reregisterLoop(ctx context.Context, expiry time.Duration) {
    retryIn := time.Duration(float64(expiry) * 0.75)
    ticker := time.NewTicker(retryIn)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            r.log.Info().Msg("SIP registration context cancelled — sending UNREGISTER")
            unregCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
            defer cancel()
            _ = r.Unregister(unregCtx)
            return
        case <-ticker.C:
            newExpiry, err := r.doRegister(ctx)
            if err != nil {
                r.log.Error().Err(err).Msg("SIP re-registration failed")
                // Do not exit — keep ticker running; network may recover
                continue
            }
            r.log.Info().
                Int("server_expires_s", int(newExpiry.Seconds())).
                Msg("SIP re-registration successful")
            // If server grants a different expiry, reset timer
            newRetry := time.Duration(float64(newExpiry) * 0.75)
            if newRetry != retryIn {
                retryIn = newRetry
                ticker.Reset(retryIn)
            }
        }
    }
}

// Unregister sends REGISTER with Expires: 0 to deregister from the server.
// Called on graceful shutdown (Phase 8) and from reregisterLoop on ctx cancel.
func (r *Registrar) Unregister(ctx context.Context) error {
    registrarURI := siplib.Uri{Host: r.registrar, Port: 5060}
    req := siplib.NewRequest(siplib.REGISTER, registrarURI)
    req.AppendHeader(siplib.NewHeader("Expires", "0"))

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
```

### Pattern 3: Server Listener Start

The Server must be started before registration so it can receive responses to the REGISTER (though REGISTER uses the Client, the transport must be listening):

```go
// Source: https://pkg.go.dev/github.com/emiago/sipgo@v1.2.0
// ListenAndServe blocks — must run in goroutine
// Returns error only on listen failure; returns nil when ctx is cancelled

go func() {
    if err := agent.Server.ListenAndServe(ctx, "udp", "0.0.0.0:5060"); err != nil {
        log.Error().Err(err).Msg("SIP UDP listener error")
    }
}()
go func() {
    if err := agent.Server.ListenAndServe(ctx, "tcp", "0.0.0.0:5060"); err != nil {
        log.Error().Err(err).Msg("SIP TCP listener error")
    }
}()
```

Server.Close() in v1.2.0 returns nil only — actual shutdown requires closing the UserAgent (ua.Close()) or cancelling the context passed to ListenAndServe.

### Pattern 4: Wiring into main.go

```go
// main.go additions for Phase 5 (after Phase 4 scaffold)

// 1. Create SIP agent (UA + Server + Client)
agent, err := sip.NewAgent(cfg, logger)
if err != nil {
    logger.Fatal().Err(err).Msg("failed to create SIP agent")
}
defer agent.UA.Close()

// 2. Start transport listeners (before registering)
go agent.Server.ListenAndServe(ctx, "udp", "0.0.0.0:5060")
go agent.Server.ListenAndServe(ctx, "tcp", "0.0.0.0:5060")

// 3. Register (blocking; exits if fails)
registrar := sip.NewRegistrar(agent.Client, cfg, logger)
if err := registrar.Register(ctx); err != nil {
    logger.Fatal().Err(err).Msg("SIP registration failed")
    os.Exit(1) // SIP-01 success criterion: invalid credentials → non-zero exit
}
```

### Pattern 5: SIP_REGISTRAR vs SIP_DOMAIN — how they are different

This is a common confusion. In the sipgate context:

- **SIP_DOMAIN** (`cfg.SIPDomain`): Used in the `From:` header URI — the domain part of the SIP AoR (Address of Record). For sipgate: `sipconnect.sipgate.de`. Also passed to `WithUserAgentHostname`.
- **SIP_REGISTRAR** (`cfg.SIPRegistrar`): The host the REGISTER request is sent to (the `Request-URI` host). For sipgate: also `sipconnect.sipgate.de`.

In the v1.0 implementation, the same value was used for both. The v2.0 Config maintains both fields (SIPDomain and SIPRegistrar) separately to be explicit — they happen to be equal for sipgate but differ in some multi-domain SIP setups. Use `cfg.SIPRegistrar` for the REGISTER `Request-URI` host and `cfg.SIPDomain` for the UA hostname.

### Anti-Patterns to Avoid

- **Hardcoding Expires: 3600 in the re-register timer:** The config default is `SIP_EXPIRES=120`. sipgate may grant a different value. Always use the server-granted Expires from the 200 OK response, not the requested value.
- **Re-registering at 90% without accounting for server-granted value:** Use `res.GetHeader("Expires").Value()` from the 200 OK, not `cfg.SIPExpires`. The server may grant less than requested.
- **Running `Client.Do` without `ClientRequestRegisterBuild`:** Without this option, CSeq and the Request-URI userinfo are not properly set per RFC 3261 §10.2. Always pass `sipgo.ClientRequestRegisterBuild` as the option.
- **Starting listeners after registration:** The Client uses the UA's transport layer. ListenAndServe must be started first so the transport is ready.
- **Calling `srv.Close()` to shut down:** In v1.2.0 this returns nil and does nothing useful. Cancel the context or call `ua.Close()`.

---

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Digest Auth (MD5 challenge-response) | Custom WWW-Authenticate parser | `sipgo.Client.DoDigestAuth` | nonce/cnonce/nc/qop="auth" — one wrong byte causes silent auth failure on real SIP servers |
| SIP transaction retransmission | Custom UDP retry loop | `sipgo.Client.Do` | RFC 3261 Timer F (32s non-INVITE transaction timeout), E retransmit doubling — 5 distinct timers |
| REGISTER request construction | Manual `sip.Request` header construction | `sipgo.ClientRequestRegisterBuild` | RFC 3261 §10.2: Request-URI MUST NOT have userinfo, CSeq MUST increment — easy to get wrong |
| Re-register timer based on requested Expires | `time.Sleep(cfg.SIPExpires)` | Ticker at 75% of server-granted Expires | Server can grant 60s when you request 3600s; sleeping 3600s = call drop after 60s |

**Key insight:** Digest Auth seems simple (MD5 hash of a few fields) but the spec has subtle rules: the `qop` field changes the hash algorithm, `nc` must be a zero-padded 8-char hex string, HA2 calculation differs for AUTH vs AUTH-INT. Do not re-implement.

---

## Common Pitfalls

### Pitfall 1: Config `SIPExpires` Default Is 120, Not 3600

**What goes wrong:** Code examples (including some in the prior SIP library research) assume a 3600s default. The actual Config struct has `default:"120"`.

**Why it happens:** SIP specs often cite 3600s as the "standard" Expires. sipgate may grant 120s, which is why the Config default matches. The re-register timer at 75% of 120s = 90s between re-registrations.

**How to avoid:** Always read from server-granted Expires in the 200 OK response. Log it on first registration: `log.Info().Int("server_expires_s", val).Msg("SIP registration successful")`. This is explicitly noted in STATE.md as a pending validation item.

**Warning signs:** Re-registration fires at wrong interval; registration lapses silently.

### Pitfall 2: DoDigestAuth Creates a New Request (CSeq Increments)

**What goes wrong:** After `DoDigestAuth`, some developers assume `req` is mutated. It is not — `DoDigestAuth` internally creates a new request with the Authorization header and incremented CSeq. The returned `res` is from the new request.

**Why it happens:** API looks like mutation but follows the "send new request with auth" pattern from RFC 3261 §22.

**How to avoid:** Always use the response returned by `DoDigestAuth`, not the original `res`. Pattern:
```go
res, err := cli.Do(ctx, req, sipgo.ClientRequestRegisterBuild)
if res.StatusCode == 401 {
    res, err = cli.DoDigestAuth(ctx, req, res, auth) // reassign res
}
if res.StatusCode != 200 { ... }
```

**Warning signs:** Treating `req` as the authenticated request; responding to wrong dialog.

### Pitfall 3: 403 vs 401 — Credential Failure vs Transient Auth Failure

**What goes wrong:** On wrong credentials, sipgate sends 403 Forbidden (not 401 again). If the code only checks for `res.StatusCode != 200` without distinguishing 403, the log message says "REGISTER rejected 403" but doesn't clearly indicate it's a credentials problem.

**Why it happens:** RFC 3261 allows 403 for "forbidden" which covers bad credentials. 401 means "authenticate required" (challenge). 403 means "rejected even with auth."

**How to avoid:** Check for 403 explicitly and log "invalid credentials" — this satisfies SIP-01 success criterion 3 ("service logs a structured error and exits non-zero"). See Pattern 2 above.

**Warning signs:** Operator doesn't know if 403 is a credentials error or a server policy block.

### Pitfall 4: Folded WWW-Authenticate Header (Issue #251 — Open)

**What goes wrong:** If sipgate sends a `WWW-Authenticate` header folded across multiple lines (RFC 3261 line folding), sipgo throws `"field name with no value in header"` and the entire 401 response is dropped — Digest Auth never completes.

**Why it happens:** sipgo does not implement RFC 3261 line-folding as of v1.2.0 (issue #251, open December 2025).

**How to avoid:** sipgate standard trunking is not known to use folded headers (no community reports; only AT&T VoWifi confirmed affected). LOW risk. Mitigation: capture a SIP trace from sipgate's 401 response during integration testing. If folded, apply a pre-parse unfold step.

**Warning signs:** Registration fails with a parse error on the 401 response, not an auth error.

### Pitfall 5: UDP MTU Issue for REGISTER (Issue #206)

**What goes wrong:** REGISTER messages over UDP > ~1400 bytes trigger `"size of packet larger than MTU"` in sipgo.

**Why it happens:** RFC 3261 §18.1.1 recommends switching to TCP for large messages. sipgo enforces this limit.

**REGISTER-specific assessment:** REGISTER messages are small (< 600 bytes typically) — they contain the AoR, Expires, Via, Contact, and auth headers but no SDP body. This issue is primarily a risk for INVITE messages in Phase 6, not REGISTER. **REGISTER is safe on UDP.**

**How to avoid:** For Phase 5, no action needed. Start both UDP and TCP listeners anyway (see Pattern 3) so Phase 6 INVITE path can use TCP if needed.

**Warning signs:** Error log containing `"size of packet larger than MTU"` on REGISTER — extremely unlikely but would indicate an unusual proxy inserting many headers.

### Pitfall 6: re-register Loop Must Not Recurse

**What goes wrong:** A naive implementation calls `Register()` from `reregisterLoop` — if `Register()` itself starts a new goroutine, each re-registration spawns additional goroutines, leaking memory over time.

**Why it happens:** The `Register()` function in Pattern 2 above calls `go r.reregisterLoop(ctx, expiry)`. If `reregisterLoop` calls `Register()` again, the loop spawns nested goroutines.

**How to avoid:** `reregisterLoop` calls `r.doRegister(ctx)` (the internal step), not `r.Register()`. `Register()` is only called once at startup. This is explicit in Pattern 2 above.

**Warning signs:** Memory growth over time; goroutine count increasing with each re-registration.

---

## Code Examples

Verified patterns from official sources:

### Exact sipgo v1.2.0 API for REGISTER + Digest Auth

```go
// Source: https://pkg.go.dev/github.com/emiago/sipgo@v1.2.0
// Source: https://github.com/emiago/sipgo/blob/main/client.go (DoDigestAuth confirmed)
// Source: diago register_transaction.go (Expires extraction with strconv.Atoi confirmed)

// Step 1: Create Request
registrarURI := sip.Uri{Host: "sipconnect.sipgate.de", Port: 5060}
req := sip.NewRequest(sip.REGISTER, registrarURI)
req.AppendHeader(sip.NewHeader("Expires", "120"))  // cfg.SIPExpires default

// Step 2: Initial REGISTER (no auth header) — ClientRequestRegisterBuild sets CSeq, clears userinfo
res, err := cli.Do(ctx, req, sipgo.ClientRequestRegisterBuild)

// Step 3: 401 → Digest Auth
if res.StatusCode == 401 || res.StatusCode == 407 {
    res, err = cli.DoDigestAuth(ctx, req, res, sipgo.DigestAuth{
        Username: "e12345p0",   // cfg.SIPUser
        Password: "secret",     // cfg.SIPPassword
    })
}

// Step 4: Extract server-granted Expires
// res.GetHeader("Expires") returns sip.Header interface; Value() returns string
if h := res.GetHeader("Expires"); h != nil {
    val, _ := strconv.Atoi(h.Value())   // e.g. 120 (or whatever sipgate grants)
    serverExpiry = time.Duration(val) * time.Second
}
```

### Expires Header Access — Two Styles

```go
// Style A: Generic header lookup (recommended — simpler)
// Source: https://pkg.go.dev/github.com/emiago/sipgo@v1.2.0/sip#MessageData.GetHeader
if h := res.GetHeader("Expires"); h != nil {  // use lowercase if concerned about allocs
    val, err := strconv.Atoi(h.Value())
    // h.Value() returns the numeric string, e.g. "120"
}

// Style B: Type assertion to *sip.ExpiresHeader
// sip.ExpiresHeader is type ExpiresHeader uint32 — Value() returns string of the uint32
headers := res.GetHeaders("Expires")
if len(headers) > 0 {
    if eh, ok := headers[0].(*sip.ExpiresHeader); ok {
        // eh.Value() is still a string; cast to uint32 via pointer dereference: uint32(*eh)
    }
}
// Style A is preferred — simpler, avoids type assertion complexity
```

### UA + Server + Client Creation (confirmed API)

```go
// Source: https://pkg.go.dev/github.com/emiago/sipgo@v1.2.0

ua, err := sipgo.NewUA(
    sipgo.WithUserAgentHostname("sipconnect.sipgate.de"),  // cfg.SIPDomain
)
srv, err := sipgo.NewServer(ua)
cli, err := sipgo.NewClient(ua)

// Optional: route sipgo's internal slog to zerolog
// Source: https://pkg.go.dev/github.com/emiago/sipgo@v1.2.0 (WithClientLogger confirmed)
// Source: https://pkg.go.dev/github.com/samber/slog-zerolog
import slogzerolog "github.com/samber/slog-zerolog"
handler := slogzerolog.Option{Logger: &zerologBase}.NewZerologHandler()
sipLogger := slog.New(handler)
srv, _ = sipgo.NewServer(ua, sipgo.WithServerLogger(sipLogger))
cli, _ = sipgo.NewClient(ua, sipgo.WithClientLogger(sipLogger))
```

### Re-register Loop (ticker pattern)

```go
// Source: diago register_transaction.go — 75% confirmed, ticker + Reset on expiry change
func (r *Registrar) reregisterLoop(ctx context.Context, initialExpiry time.Duration) {
    retryIn := time.Duration(float64(initialExpiry) * 0.75)
    ticker := time.NewTicker(retryIn)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            _ = r.Unregister(context.Background())
            return
        case <-ticker.C:
            newExpiry, err := r.doRegister(ctx)
            if err != nil {
                r.log.Error().Err(err).Msg("re-registration failed")
                continue
            }
            // Adjust ticker if server changes granted Expires
            if newRetry := time.Duration(float64(newExpiry)*0.75); newRetry != retryIn {
                retryIn = newRetry
                ticker.Reset(retryIn)
            }
        }
    }
}
```

---

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| emiago/sipgox (extra utils) | emiago/diago or sipgo directly | June 2024 (sipgox deprecated) | sipgox README: "please do not use this" — use sipgo primitives |
| emiago/media (standalone RTP) | Media code in diago | Feb 2025 (archived) | emiago/media read-only; irrelevant for this project (using pion/rtp) |
| `dialog.Close()` implicitly ending call | `Bye(ctx)` + `Close()` explicit | sipgo v0.17.0 (Jan 2026) | Must call Bye() before Close() for proper termination — Phase 5 creates dialog types; Phase 6 implements |
| sipgo using zerolog internally | sipgo using `log/slog` | v0.30.0 (2024) | Bridge via samber/slog-zerolog if unified logs desired |
| Registrar = Domain (same field) | Separate SIPRegistrar + SIPDomain | v2.0 Config design | Both are `sipconnect.sipgate.de` for sipgate but kept explicit for correctness |

**Deprecated/outdated:**
- `emiago/sipgox`: Deprecated June 2024. Do not use.
- `emiago/media`: Archived February 2025. Read-only archive.
- Any pattern using `dialog.Close()` alone to terminate: broken since sipgo v0.17.0.

---

## Open Questions

1. **What Expires value does sipgate actually grant in the 200 OK?**
   - What we know: Config default is 120s. Asterisk community config shows `Expires: 120`. One forum post shows `registertimeout=600` suggesting some installs use 600s.
   - What's unclear: Exact value for sipgate trunking (`sipconnect.sipgate.de`) in the 200 OK to our REGISTER.
   - Recommendation: Log `server_expires_s` on first registration (already in Pattern 2). Check the log after first integration test. STATE.md already flags this as a pending validation item.

2. **Does sipgate send folded WWW-Authenticate headers (Issue #251)?**
   - What we know: Issue #251 is open (Dec 2025); only AT&T VoWifi confirmed affected; no community reports against sipgate trunking.
   - What's unclear: sipgate's exact 401 response format.
   - Recommendation: Capture a SIP trace of the 401 response in integration testing. If folded: add a pre-parse string replacement (unfold continuation lines before passing to sipgo parser). Probability: LOW.

3. **Do we need slog-zerolog bridge for sipgo internal logs?**
   - What we know: sipgo uses `log/slog` internally; `WithClientLogger`/`WithServerLogger` accept `*slog.Logger`. Without wiring, sipgo logs via default slog (to stdout as JSON if no global handler configured — which is the zerolog JSON output goal).
   - What's unclear: Does the default slog in a binary with zerolog produce unified JSON or fragmented output?
   - Recommendation: Add `samber/slog-zerolog` bridge. It is a lightweight dependency (no transitive deps); ensures all sipgo internal logs flow through zerolog with consistent field naming. Decision can be delegated to the planner — mark as Claude's discretion.

---

## Validation Architecture

> `workflow.nyquist_validation` is not set in `.planning/config.json` — this section is skipped per research agent instructions.

---

## Sources

### Primary (HIGH confidence)

- `emiago/sipgo` pkg.go.dev v1.2.0 — https://pkg.go.dev/github.com/emiago/sipgo@v1.2.0 — `NewUA`, `NewServer`, `NewClient`, `Do`, `DoDigestAuth`, `ClientRequestRegisterBuild`, `DigestAuth`, `WithClientLogger`, `WithServerLogger` API signatures verified
- `emiago/sipgo/sip` pkg.go.dev v1.2.0 — https://pkg.go.dev/github.com/emiago/sipgo@v1.2.0/sip — `Uri` struct fields (Scheme, User, Host, Port int), `NewRequest`, `GetHeader`, `ExpiresHeader` type verified
- `emiago/sipgo` dialog_server.go — https://github.com/emiago/sipgo/blob/v1.2.0/dialog_server.go — `DialogServerSession.Bye()`, `Close()` behavior confirmed
- `emiago/sipgo` server.go — `ListenAndServe` returns error only on listen failure; `Close()` returns nil in v1.2.0
- `emiago/diago` register_transaction.go — https://github.com/emiago/diago/blob/main/register_transaction.go — Expires extraction with `strconv.Atoi(h.Value())` confirmed; 75% retry ratio confirmed; ticker.Reset on expiry change confirmed
- `.planning/research/sip-library-research.md` — prior research; confirmed sipgo issue #116, #206, #251 status
- `internal/config/config.go` — `SIPExpires` default 120; `SIPUser`, `SIPPassword`, `SIPDomain`, `SIPRegistrar` field names confirmed
- `internal/config/config_test.go` — SIP_USER format `e12345p0`; registrar `sipconnect.sipgate.de` confirmed

### Secondary (MEDIUM confidence)

- `samber/slog-zerolog` pkg.go.dev — https://pkg.go.dev/github.com/samber/slog-zerolog — slog.Handler backed by zerolog; v1.x; MIT license confirmed
- sipgo issue #116 — https://github.com/emiago/sipgo/issues/116 — graceful shutdown NOT implemented in v1.2.0; labeled "critical, will be supported"; latest comment Dec 2025
- sipgo issue #251 — https://github.com/emiago/sipgo/issues/251 — folded headers NOT fixed in v1.2.0; open Dec 2025; AT&T VoWifi affected
- sipgo issue #206 — https://github.com/emiago/sipgo/issues/206 — UDP MTU limit NOT fixed in v1.2.0; open May 2025; labeled "will be supported"; REGISTER is safe (small messages)
- Asterisk community sipgate config — `register => SIP-ID:PASSWORD@sipconnect.sipgate.de/SIP-ID`; `Expires: 120` observed in one thread
- sipgo v0.30.0 release notes — slog migration confirmed; `WithClientLogger`/`WithServerLogger` introduced

### Tertiary (LOW confidence — validate with live integration test)

- sipgate server-granted Expires value — community evidence suggests 120s; unconfirmed for current sipgate trunking API
- sipgate 401 response format — no folded headers observed but no packet trace available
- sipgate From domain vs Registrar — both `sipconnect.sipgate.de` in all observed configs; LOW risk of divergence

---

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH — pkg.go.dev v1.2.0 API verified; library already chosen in prior research
- Architecture patterns: HIGH — verified against sipgo source files, diago source, integration tests, official pkg.go.dev docs
- Pitfalls: HIGH (sipgo issues confirmed open) / MEDIUM (sipgate-specific behavior unverified without live test)
- Code examples: HIGH — all method signatures verified against pkg.go.dev v1.2.0

**Research date:** 2026-03-03
**Valid until:** 2026-06-03 (sipgo is stable v1.x; recheck if v1.3.0 released before Phase 5 starts)
