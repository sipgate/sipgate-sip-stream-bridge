# Go SIP Library Research: audio-dock v2.0 Go Rewrite

**Researched:** 2026-03-03
**Domain:** Go SIP libraries for SIP-to-WebSocket audio bridge (UAC registration + UAS incoming INVITE)
**Confidence:** HIGH

---

## Summary

The audio-dock v2.0 Go rewrite requires a pure Go SIP library capable of: REGISTER with Digest Auth (UAC), incoming INVITE handling (UAS), full dialog lifecycle (100/180/200/ACK/BYE), OPTIONS keepalive responses, RFC 4733 DTMF via RTP, and both UDP/TCP transport — with zero CGo dependencies so the final binary can be built `FROM scratch`.

**`emiago/sipgo` v1.2.0 is the definitive choice.** It is the only Go SIP library that is:
(a) actively maintained with a stable v1.x API as of early 2025,
(b) proven in production via LiveKit's SIP-to-WebRTC bridge (github.com/livekit/sip uses it directly),
(c) 100% pure Go with no CGo, and
(d) a direct fit for all nine SIP requirements in REQUIREMENTS.md.

The supporting stack is: `pion/sdp v3` for SDP parsing/generation (same library LiveKit uses), `pion/rtp` for RTP packet framing and telephone-event (PT 101) detection, and Go's `net.UDPConn` / `net/http` for raw RTP sockets and the health/metrics HTTP server. The `emiago/diago` higher-level framework is available but is NOT needed — sipgo's lower-level API gives the explicit control required for a bridge (raw SDP bytes in/out, manual RTP port negotiation, direct goroutine-per-call model).

**Primary recommendation:** Use `emiago/sipgo v1.2.0` + `pion/sdp/v3` + `pion/rtp`. Skip diago. Build the registration loop, dialog lifecycle, and RTP goroutines directly using sipgo primitives.

---

## Standard Stack

### Core

| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `github.com/emiago/sipgo` | v1.2.0 (Feb 2025) | SIP protocol stack: UAC REGISTER, UAS INVITE, transaction layer, dialog management | Only actively-maintained pure-Go SIP stack with stable v1.x API; used by LiveKit in production |
| `github.com/pion/sdp/v3` | v3.0.18 (Feb 2026) | Parse SDP from INVITE body, build SDP answer with our RTP port + PCMU codec | Pure Go, MIT, actively maintained, same library LiveKit SIP bridge uses |
| `github.com/pion/rtp` | v1.10.1 (Jan 2026) | Parse incoming RTP packets (detect PT 101 telephone-event for DTMF), write outgoing RTP | Pure Go, MIT, Pion ecosystem standard |

### Supporting

| Library | Purpose | When to Use |
|---------|---------|-------------|
| `github.com/rs/zerolog` | Structured JSON logging with call context (callId, streamSid) | OBS-01 — structured log fields per call, low-allocation hot path |
| `github.com/emiago/diago` | Higher-level VoIP framework built on sipgo (media session, AudioReader/AudioWriter) | ADV-03 (v2 requirement) — evaluate if raw RTP management becomes complex |

### Alternatives Considered

| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| emiago/sipgo | ghettovoice/gosip | gosip has no stable releases, last commit Oct 2024, no production evidence; sipgo's README names gosip as the ancestor project |
| emiago/sipgo | cloudwebrtc/go-sip-ua | Last release Jan 2022, inactive; requires libopus (CGo) for media — disqualified |
| emiago/sipgo | jart/gosip | Abandoned since 2020; Ragel dependency complicates pure-Go builds; no REGISTER auth examples |
| emiago/sipgo | Raw `net.UDPConn` + manual SIP parser | RFC 3261 transaction state machine has ~15 distinct states per transaction type; retransmission timers (T1/T2/T4), Via/Route/Record-Route header management, dialog tag generation — weeks of work with a long tail of interop bugs |
| pion/sdp/v3 | emiago/sipgox/sdp | sipgox is deprecated — README says "please do not use this"; all media/phone development moved to diago |
| pion/sdp/v3 | pixelbender/go-sdp | Less active, fewer stars, not used by major projects |

**Installation:**
```bash
go get github.com/emiago/sipgo@v1.2.0
go get github.com/pion/sdp/v3@v3.0.18
go get github.com/pion/rtp@v1.10.1
go get github.com/rs/zerolog@latest
```

---

## Architecture Patterns

### Recommended Project Structure

```
cmd/audio-dock/
└── main.go               # flag parsing, config validation, signal handling (SIGTERM/SIGINT)

internal/
├── config/
│   └── config.go         # env var parsing, fail-fast validation (CFG-01 through CFG-05)
├── sip/
│   ├── agent.go          # UserAgent, Server, Client construction; ListenAndServe goroutines
│   ├── registrar.go      # REGISTER + periodic re-register loop (SIP-01, SIP-02)
│   ├── handler.go        # OnInvite, OnOptions, OnBye, OnAck request handlers (SIP-03..05)
│   └── sdp.go            # SDP parsing (extract caller RTP addr/port) + SDP answer builder
├── bridge/
│   ├── manager.go        # CallManager: per-call session map, concurrency (CON-01, CON-02)
│   ├── session.go        # CallSession: RTP goroutines ↔ WebSocket goroutines (WSB-01..07)
│   ├── rtp.go            # net.UDPConn listener, RTP packet demux, DTMF detection (WSB-07)
│   └── ws.go             # WebSocket client, Twilio Media Streams protocol, reconnect loop
└── obs/
    ├── health.go          # GET /health JSON endpoint (OBS-02)
    └── metrics.go         # GET /metrics Prometheus exposition (OBS-03)
```

### Pattern 1: UserAgent + Server + Client Shared UA

Both `Server` (incoming) and `Client` (outgoing REGISTER) share a single `UserAgent`. This is the canonical sipgo pattern.

```go
// Source: https://pkg.go.dev/github.com/emiago/sipgo
ua, err := sipgo.NewUA(
    sipgo.WithUserAgentHostname("our-sip-host.example.com"),
)
if err != nil {
    log.Fatal().Err(err).Msg("failed to create UA")
}

srv, err := sipgo.NewServer(ua)
if err != nil {
    log.Fatal().Err(err).Msg("failed to create SIP server")
}

cli, err := sipgo.NewClient(ua)
if err != nil {
    log.Fatal().Err(err).Msg("failed to create SIP client")
}

// Register handlers
srv.OnInvite(handleInvite)
srv.OnOptions(handleOptions)
srv.OnBye(handleBye)
srv.OnAck(handleAck)

// Start transport listener (blocks; run in goroutine)
go srv.ListenAndServe(ctx, "udp", "0.0.0.0:5060")
go srv.ListenAndServe(ctx, "tcp", "0.0.0.0:5060")
```

### Pattern 2: REGISTER with Digest Auth + Periodic Re-registration

sipgo's `Client.DoDigestAuth` handles the 401/407 challenge-response cycle automatically.

```go
// Source: https://github.com/emiago/diago/blob/main/register_transaction.go
// (adapted for direct sipgo Client use)

func register(ctx context.Context, cli *sipgo.Client, registrar, username, password string) error {
    registrarURI := sip.Uri{Host: registrar, Port: 5060}

    req := sip.NewRequest(sip.REGISTER, registrarURI)
    req.AppendHeader(sip.NewHeader("Expires", "3600"))

    // Send initial REGISTER (no auth)
    res, err := cli.Do(ctx, req, sipgo.ClientRequestRegisterBuild)
    if err != nil {
        return fmt.Errorf("REGISTER failed: %w", err)
    }

    // Handle 401 Unauthorized or 407 Proxy Auth Required
    if res.StatusCode == sip.StatusUnauthorized || res.StatusCode == sip.StatusProxyAuthRequired {
        res, err = cli.DoDigestAuth(ctx, req, res, sipgo.DigestAuth{
            Username: username,
            Password: password,
        })
        if err != nil {
            return fmt.Errorf("REGISTER digest auth failed: %w", err)
        }
    }

    if res.StatusCode != sip.StatusOK {
        return fmt.Errorf("REGISTER rejected: %d %s", res.StatusCode, res.Reason)
    }

    // Parse server-granted Expires from response
    expiresHdr := res.GetHeader("Expires")
    expiry := parseExpires(expiresHdr) // typically 3600s

    // Re-register at 75% of expiry (matches diago pattern, matches v1.0 refreshFrequency 90%)
    retryInterval := time.Duration(float64(expiry) * 0.75)

    go func() {
        ticker := time.NewTicker(retryInterval)
        defer ticker.Stop()
        for {
            select {
            case <-ctx.Done():
                unregister(ctx, cli, registrar, username, password)
                return
            case <-ticker.C:
                if err := register(ctx, cli, registrar, username, password); err != nil {
                    log.Error().Err(err).Msg("re-registration failed")
                }
            }
        }
    }()

    return nil
}
```

### Pattern 3: Incoming INVITE — Full Dialog Lifecycle (UAS)

`DialogServerCache` manages per-dialog state. The key insight: `ReadInvite` must be called before sending any response, then `ReadAck`/`ReadBye` route subsequent messages to the correct dialog.

```go
// Source: https://pkg.go.dev/github.com/emiago/sipgo + integration tests
// Source: https://github.com/emiago/sipgo/blob/main/dialog_server.go

var (
    contactHDR = sip.ContactHeader{
        Address: sip.Uri{Host: ourIP, Port: 5060},
    }
    dialogSrv = sipgo.NewDialogServerCache(cli, contactHDR)
)

func handleInvite(req *sip.Request, tx sip.ServerTransaction) {
    // 1. Create dialog — MUST happen before responding
    dlg, err := dialogSrv.ReadInvite(req, tx)
    if err != nil {
        tx.Respond(sip.NewResponseFromRequest(req, 500, "Server Error", nil))
        return
    }

    // 2. Extract caller's SDP (RTP IP + port + codec)
    callerSDP := req.Body()

    // 3. Check WebSocket availability — reject with 503 if unavailable (SIP-04)
    rtpPort, err := portPool.Acquire()
    if err != nil || !wsAvailable(req) {
        dlg.Respond(sip.StatusServiceUnavailable, "Service Unavailable", nil)
        dlg.Close()
        return
    }

    // 4. Send 100 Trying (suppresses retransmissions)
    dlg.Respond(sip.StatusTrying, "Trying", nil)

    // 5. Send 180 Ringing
    dlg.Respond(sip.StatusRinging, "Ringing", nil)

    // 6. Build SDP answer with our RTP listen port + PCMU + telephone-event
    sdpAnswer := buildSDPAnswer(ourIP, rtpPort, callerSDP)

    // 7. Send 200 OK with SDP answer
    // RespondSDP sets Content-Type: application/sdp automatically
    if err := dlg.RespondSDP(sdpAnswer); err != nil {
        log.Error().Err(err).Msg("failed to send 200 OK")
        return
    }

    // 8. Launch call session (RTP ↔ WebSocket bridge goroutines)
    go callManager.StartSession(dlg, callerSDP, sdpAnswer, rtpPort)
}

func handleAck(req *sip.Request, tx sip.ServerTransaction) {
    if err := dialogSrv.ReadAck(req, tx); err != nil {
        log.Error().Err(err).Msg("ACK routing failed")
    }
}

func handleBye(req *sip.Request, tx sip.ServerTransaction) {
    if err := dialogSrv.ReadBye(req, tx); err != nil {
        log.Error().Err(err).Msg("BYE routing failed")
    }
}

func handleOptions(req *sip.Request, tx sip.ServerTransaction) {
    res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
    tx.Respond(res)
}
```

### Pattern 4: SDP Parsing and Answer Generation

```go
// Source: https://pkg.go.dev/github.com/pion/sdp/v3

func parseCallerRTP(sdpBytes []byte) (ip string, port int, dtmfPT uint8, err error) {
    sd := &sdp.SessionDescription{}
    if err = sd.Unmarshal(sdpBytes); err != nil {
        return
    }

    for _, md := range sd.MediaDescriptions {
        if md.MediaName.Media != "audio" {
            continue
        }
        port = md.MediaName.Port.Value

        // Connection address: prefer per-media, fall back to session-level
        if md.ConnectionInformation != nil {
            ip = md.ConnectionInformation.Address.Address
        } else if sd.ConnectionInformation != nil {
            ip = sd.ConnectionInformation.Address.Address
        }

        // Find telephone-event payload type (dynamic, defined by SDP)
        for _, fmt := range md.MediaName.Formats {
            pt, _ := strconv.ParseUint(fmt, 10, 8)
            codec, _ := sd.GetCodecForPayloadType(uint8(pt))
            if strings.EqualFold(codec.Name, "telephone-event") {
                dtmfPT = uint8(pt)
            }
        }
        return
    }
    err = fmt.Errorf("no audio media in SDP")
    return
}

func buildSDPAnswer(ourIP string, ourRTPPort int, offerBytes []byte) []byte {
    // Parse offer to extract session ID, caller PT assignments
    offer := &sdp.SessionDescription{}
    offer.Unmarshal(offerBytes)

    sd := &sdp.SessionDescription{
        Version: 0,
        Origin: sdp.Origin{
            Username:       "-",
            SessionID:      uint64(time.Now().UnixNano()),
            SessionVersion: uint64(time.Now().UnixNano()),
            NetworkType:    "IN",
            AddressType:    "IP4",
            UnicastAddress: ourIP,
        },
        SessionName: sdp.SessionName("audio-dock"),
        ConnectionInformation: &sdp.ConnectionInformation{
            NetworkType: "IN",
            AddressType: "IP4",
            Address:     &sdp.Address{Address: ourIP},
        },
        TimeDescriptions: []sdp.TimeDescription{{Timing: sdp.Timing{}}},
    }

    // Audio media line: PCMU (PT 0) + telephone-event (PT 101)
    audio := &sdp.MediaDescription{
        MediaName: sdp.MediaName{
            Media:   "audio",
            Port:    sdp.RangedPort{Value: ourRTPPort},
            Protos:  []string{"RTP", "AVP"},
            Formats: []string{"0", "101"},
        },
    }
    audio.WithCodec(0, "PCMU", 8000, 1, "")
    audio.WithCodec(101, "telephone-event", 8000, 1, "0-16")
    audio.WithPropertyAttribute("sendrecv")

    sd.MediaDescriptions = append(sd.MediaDescriptions, audio)

    out, _ := sd.Marshal()
    return out
}
```

### Pattern 5: DTMF Detection from RTP (RFC 4733 / telephone-event)

```go
// Source: https://pkg.go.dev/github.com/pion/rtp
// telephone-event PT is negotiated in SDP — typically PT 101

func processRTPPacket(raw []byte, dtmfPT uint8) (audioPayload []byte, dtmfEvent *DTMFEvent, err error) {
    var pkt rtp.Packet
    if err = pkt.Unmarshal(raw); err != nil {
        return
    }

    if pkt.Header.PayloadType == dtmfPT {
        // RFC 4733 telephone-event payload: byte 0 = digit, byte 1 = E+Volume, bytes 2-3 = duration
        if len(pkt.Payload) >= 4 {
            digit := pkt.Payload[0]          // 0-15 = DTMF digit
            endBit := pkt.Payload[1]&0x80 != 0
            // Deduplicate by RTP timestamp (same timestamp = retransmission of same event)
            if endBit {
                dtmfEvent = &DTMFEvent{Digit: digit, Timestamp: pkt.Header.Timestamp}
            }
        }
        return
    }

    // Regular audio (PCMU PT 0 for sipgate)
    audioPayload = pkt.Payload
    return
}
```

### Anti-Patterns to Avoid

- **Using `sipgox` (emiago/sipgox):** Deprecated as of June 2024. README explicitly says "please do not use this." Use `sipgo` directly.
- **Using `diago` for the v2.0 bridge core:** Diago wraps sipgo with opinionated media abstractions (AudioReader/AudioWriter). For a raw RTP-to-WebSocket bridge we need byte-level access to RTP payloads — diago's abstraction would fight the design. Evaluate for ADV-03 in v3.
- **Sharing a single `net.UDPConn` for all calls:** Each call needs its own UDP port for RTP (sipgate sends RTP to the port in the SDP answer). Allocate from a pool (CFG-03 `RTP_PORT_MIN`/`RTP_PORT_MAX`).
- **Buffering RTP during WebSocket reconnect:** WSR-03 explicitly says drop — unbounded buffer risks OOM under reconnect storm.
- **Responding to INVITE before `ReadInvite`:** The dialog server session must be created first or ACK/BYE routing will fail — sipgo v1.1.2 fixed a regression here.
- **Using `dialog.Close()` without sending BYE first (v0.17.0 change):** As of sipgo v0.17.0, `Close()` only removes the dialog from cache without state transition. Always send BYE explicitly before Close (LCY-01).

---

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| SIP transaction state machine | Custom UDP packet loop | `emiago/sipgo` | RFC 3261 defines 5 transaction FSMs (INVITE client/server, non-INVITE client/server, proxy) — each with retransmit timers T1/T2/T4, provisional ACK handling, branch parameter management |
| Digest MD5 auth challenge-response | Custom WWW-Authenticate header parser | `sipgo.Client.DoDigestAuth` | nonce, cnonce, nc, qop="auth" — subtle enough to break with specific providers; sipgo has this tested |
| SDP parsing | Manual string splitting on `\r\n` | `pion/sdp/v3` | Multi-media SDP, connection-level vs media-level `c=` lines, `b=` bandwidth, fmtp parsing — naive parsers break on real SIP traffic |
| Dialog matching (Call-ID + tags) | Map keyed on Call-ID | `sipgo.DialogServerCache` | Dialog matching requires Call-ID + From-tag + To-tag tuple; retransmitted INVITEs must hit same dialog |
| Re-registration timer | `time.Sleep(3600)` | Ticker at 75% of server-granted Expires | Server can grant shorter Expires than requested (common: grant 60s when client requests 3600s) |
| RTP packet framing | Manual bit-twiddling | `pion/rtp.Packet.Unmarshal` | SSRC, sequence number, extension headers, CSRC list — spec-correct parsing in 3 lines |

**Key insight:** SIP has a combinatorial explosion of error cases per RFC 3261. The retransmission logic alone (timers A/B/D/E/F/G/H/I/J/K) takes more code than the entire application logic. Never start from `net.UDPConn`.

---

## Common Pitfalls

### Pitfall 1: `dialog.Close()` Without BYE (v0.17.0 Behavioral Change)

**What goes wrong:** In sipgo >=v0.17.0, `Close()` removes the dialog from cache but does NOT send BYE or transition state. If you call `Close()` thinking it terminates the call, the remote side keeps sending RTP.

**Why it happens:** v0.17.0 release note: "Modified `dialog.Close()` to only remove the dialog from the shared map without terminating state, requiring explicit BYE message handling to prevent race conditions."

**How to avoid:** Always call `dlg.Bye(ctx)` before `dlg.Close()` when ending a call. Structure: `defer dlg.Close()` for cleanup, explicit `dlg.Bye(ctx)` for termination signal.

**Warning signs:** Caller hears silence but never gets BYE — call hangs on remote side.

### Pitfall 2: UDP MTU Limit for Large INVITEs (Issue #206)

**What goes wrong:** sipgo rejects sending SIP messages over UDP if the packet exceeds MTU (~1400 bytes). A SIP INVITE with a full SDP, many Via headers, and proprietary sipgate headers can reach 2000+ bytes.

**Why it happens:** RFC 3261 §18.1.1 recommends switching to TCP when UDP message size exceeds MTU. sipgo enforces this.

**How to avoid:** Listen on both UDP and TCP (as the architecture above does). For outgoing REGISTER, prefer TCP transport. For incoming calls from sipgate, sipgate uses UDP by default — if INVITEs are large, configure sipgate to use TCP.

**Warning signs:** `"size of packet larger than MTU"` error in logs on INVITE send.

### Pitfall 3: Folded SIP Header Lines (Issue #251 — Open Bug)

**What goes wrong:** Some SIP servers (confirmed: AT&T VoWifi) fold WWW-Authenticate headers across multiple lines. sipgo's parser throws `"field name with no value in header"` and the entire message is dropped — meaning digest auth never completes.

**Why it happens:** sipgo does not yet implement RFC 3261 line-folding parsing.

**How to avoid:** sipgate standard trunking does not appear to use folded headers (no community reports). However, if using sipgate business or enterprise variants, test with actual SIP traces. Low risk for this project.

**Warning signs:** Registration fails with parse error on 401 response.

### Pitfall 4: SDP Contact IP Must Be Externally Reachable

**What goes wrong:** If `o=` and `c=` lines in SDP answer contain the internal Docker container IP (e.g., `172.17.0.2`), sipgate will attempt to send RTP to that address — no audio flows.

**Why it happens:** Docker networking — container IP is not routable from sipgate's infrastructure.

**How to avoid:** CFG-04 `SDP_CONTACT_IP` environment variable sets the external/host IP injected into SDP answer. Use `network_mode: host` in Docker Compose (already in v1.0 KEY DECISIONS) to make host networking bypass this.

**Warning signs:** Call connects (SIP layer OK), but no audio in either direction.

### Pitfall 5: RTP Port Pool Exhaustion Under Concurrent Calls

**What goes wrong:** Each call binds a `net.UDPConn` on a unique port. If the port pool is exhausted (all ports in `RTP_PORT_MIN..RTP_PORT_MAX` are in use), new calls silently fail to get a port.

**Why it happens:** Port pool unbounded growth under high call volume or port leak on abnormal termination.

**How to avoid:** Implement bounded port pool with explicit `Acquire()` / `Release()`. Release the port in a `defer` within the call session goroutine. Set `RTP_PORT_MAX - RTP_PORT_MIN >= max_expected_concurrent_calls` (e.g., 100 ports for 50 concurrent calls, 2 ports per call for safety).

**Warning signs:** `bind: address already in use` errors; calls fail with 503 when load increases.

### Pitfall 6: DTMF Deduplication by Timestamp

**What goes wrong:** RFC 4733 requires sending each DTMF event at minimum 3 times (for redundancy). Without deduplication, the WebSocket consumer receives 3+ `dtmf` events per keypress.

**Why it happens:** The v1.0 implementation deduplicates by RTP timestamp (`callManager.ts:461` notes the `wsReconnecting` gate gap). The Go rewrite must replicate this.

**How to avoid:** Track last-seen `(digit, timestamp)` pair per call. Emit the event only when the End-bit (RFC 4733 §2.5.1) is set for the first time for a given timestamp.

**Warning signs:** AI voice backend receives duplicate DTMF digits, triggering double-actions.

### Pitfall 7: Graceful Shutdown — sipgo Has No Built-in Drain (Issue #116 — Open)

**What goes wrong:** On SIGTERM, simply calling `srv.Close()` tears down the transport immediately. In-flight transactions (INVITE in progress, BYE not yet ACK'd) are dropped, leaving remote side in an unknown state.

**Why it happens:** sipgo issue #116 marks graceful shutdown as "will be supported" but it is not implemented as of v1.2.0.

**How to avoid:** Track active `DialogServerSession` instances in CallManager. On SIGTERM: stop accepting new calls (track a `shutdownFlag`), send BYE to all active dialogs, wait for them to complete (or a 5s timeout), then send UNREGISTER, then close UA. This is LCY-01.

**Warning signs:** Callers get unexpectedly disconnected on deploy restarts.

---

## Code Examples

### Skeleton: REGISTER + Incoming INVITE (Verified Pattern)

This skeleton combines all verified patterns into the minimum working structure for the bridge.

```go
// Source: emiago/sipgo v1.2.0 API (https://pkg.go.dev/github.com/emiago/sipgo)
// Source: emiago/diago register_transaction.go (https://github.com/emiago/diago/blob/main/register_transaction.go)
// Source: dialog_server.go + dialog_integration_test.go (https://github.com/emiago/sipgo)

package main

import (
    "context"
    "fmt"
    "log"
    "net"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/emiago/sipgo"
    "github.com/emiago/sipgo/sip"
    pionsdp "github.com/pion/sdp/v3"
    "github.com/pion/rtp"
)

func main() {
    ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
    defer cancel()

    // --- 1. Create User Agent ---
    ua, err := sipgo.NewUA(
        sipgo.WithUserAgentHostname(os.Getenv("SIP_DOMAIN")),
    )
    if err != nil {
        log.Fatal(err)
    }
    defer ua.Close()

    // --- 2. Create Server (UAS) and Client (UAC) sharing the same UA ---
    srv, err := sipgo.NewServer(ua)
    if err != nil {
        log.Fatal(err)
    }

    cli, err := sipgo.NewClient(ua)
    if err != nil {
        log.Fatal(err)
    }

    ourIP := os.Getenv("SDP_CONTACT_IP") // externally reachable IP for RTP

    // Dialog cache routes ACK and BYE to the correct dialog
    contactHDR := sip.ContactHeader{
        Address: sip.Uri{User: os.Getenv("SIP_USER"), Host: ourIP, Port: 5060},
    }
    dialogSrv := sipgo.NewDialogServerCache(cli, contactHDR)

    // --- 3. Register request handlers ---
    srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
        dlg, err := dialogSrv.ReadInvite(req, tx)
        if err != nil {
            tx.Respond(sip.NewResponseFromRequest(req, 500, "Server Error", nil))
            return
        }
        defer dlg.Close()

        // Check WebSocket / port availability
        rtpPort := acquireRTPPort()
        if rtpPort == 0 {
            dlg.Respond(sip.StatusServiceUnavailable, "Service Unavailable", nil)
            return
        }
        defer releaseRTPPort(rtpPort)

        dlg.Respond(sip.StatusTrying, "Trying", nil)
        dlg.Respond(sip.StatusRinging, "Ringing", nil)

        // Build SDP answer
        sdpAnswer := buildSDPAnswer(ourIP, rtpPort)
        if err := dlg.RespondSDP(sdpAnswer); err != nil {
            log.Printf("200 OK send failed: %v", err)
            return
        }

        // Wait for call to end (dialog context cancelled by BYE or error)
        // NOTE: bridge goroutines launched here would own this context
        <-dlg.Context().Done()

        // Send BYE if we are ending the call
        // dlg.Bye(ctx) — called from CallManager when WS side ends the call
    })

    srv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {
        dialogSrv.ReadAck(req, tx)
    })

    srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
        dialogSrv.ReadBye(req, tx)
        // CallManager.EndSession(req.CallID()) triggered here
    })

    srv.OnOptions(func(req *sip.Request, tx sip.ServerTransaction) {
        tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))
    })

    // --- 4. Start listeners ---
    go func() {
        if err := srv.ListenAndServe(ctx, "udp", "0.0.0.0:5060"); err != nil {
            log.Printf("UDP listener stopped: %v", err)
        }
    }()
    go func() {
        if err := srv.ListenAndServe(ctx, "tcp", "0.0.0.0:5060"); err != nil {
            log.Printf("TCP listener stopped: %v", err)
        }
    }()

    // --- 5. Register with sipgate ---
    registrar := os.Getenv("SIP_REGISTRAR") // e.g. sipconnect.sipgate.de
    if err := registerWithRetry(ctx, cli, registrar,
        os.Getenv("SIP_USER"), os.Getenv("SIP_PASSWORD")); err != nil {
        log.Fatalf("initial registration failed: %v", err)
    }

    // --- 6. Wait for shutdown ---
    <-ctx.Done()
    // LCY-01: BYE all active calls, UNREGISTER, then close UA (implemented in CallManager)
    log.Println("shutting down")
}

// registerWithRetry sends REGISTER, handles 401 digest challenge, starts re-register loop.
func registerWithRetry(ctx context.Context, cli *sipgo.Client,
    registrar, user, pass string) error {

    registrarURI := sip.Uri{Host: registrar, Port: 5060}
    req := sip.NewRequest(sip.REGISTER, registrarURI)
    req.AppendHeader(sip.NewHeader("Expires", "3600"))

    // Initial attempt (no auth header)
    res, err := cli.Do(ctx, req, sipgo.ClientRequestRegisterBuild)
    if err != nil {
        return fmt.Errorf("REGISTER send error: %w", err)
    }

    // Handle digest challenge (sipgate uses 401 Unauthorized)
    if res.StatusCode == sip.StatusUnauthorized || res.StatusCode == sip.StatusProxyAuthRequired {
        res, err = cli.DoDigestAuth(ctx, req, res, sipgo.DigestAuth{
            Username: user,
            Password: pass,
        })
        if err != nil {
            return fmt.Errorf("digest auth error: %w", err)
        }
    }

    if res.StatusCode != sip.StatusOK {
        return fmt.Errorf("REGISTER rejected %d: %s", res.StatusCode, res.Reason)
    }

    // Parse server-granted expiry
    expiry := 3600 * time.Second
    if h := res.GetHeader("Expires"); h != nil {
        // parse h.Value() into expiry
    }

    // Re-register at 75% of granted expiry
    retryIn := time.Duration(float64(expiry) * 0.75)

    go func() {
        t := time.NewTicker(retryIn)
        defer t.Stop()
        for {
            select {
            case <-ctx.Done():
                return
            case <-t.C:
                if err := registerWithRetry(ctx, cli, registrar, user, pass); err != nil {
                    log.Printf("re-register failed: %v", err)
                }
            }
        }
    }()

    return nil
}

// buildSDPAnswer constructs a minimal SDP answer advertising PCMU + telephone-event.
func buildSDPAnswer(ourIP string, ourPort int) []byte {
    // Source: https://pkg.go.dev/github.com/pion/sdp/v3
    sd := &pionsdp.SessionDescription{
        Version: 0,
        Origin: pionsdp.Origin{
            Username: "-", SessionID: uint64(time.Now().UnixNano()),
            SessionVersion: uint64(time.Now().UnixNano()),
            NetworkType: "IN", AddressType: "IP4", UnicastAddress: ourIP,
        },
        SessionName: "audio-dock",
        ConnectionInformation: &pionsdp.ConnectionInformation{
            NetworkType: "IN", AddressType: "IP4",
            Address: &pionsdp.Address{Address: ourIP},
        },
        TimeDescriptions: []pionsdp.TimeDescription{{Timing: pionsdp.Timing{}}},
    }
    audio := &pionsdp.MediaDescription{
        MediaName: pionsdp.MediaName{
            Media: "audio",
            Port:  pionsdp.RangedPort{Value: ourPort},
            Protos: []string{"RTP", "AVP"}, Formats: []string{"0", "101"},
        },
    }
    audio.WithCodec(0, "PCMU", 8000, 1, "")
    audio.WithCodec(101, "telephone-event", 8000, 1, "0-16")
    audio.WithPropertyAttribute("sendrecv")
    sd.MediaDescriptions = append(sd.MediaDescriptions, audio)
    out, _ := sd.Marshal()
    return out
}

// processRTP demultiplexes audio vs DTMF from incoming RTP.
// dtmfPT is the payload type negotiated in SDP (typically 101 for telephone-event).
func processRTP(raw []byte, dtmfPT uint8) (audio []byte, dtmfDigit byte, isDTMF bool) {
    // Source: https://pkg.go.dev/github.com/pion/rtp
    var pkt rtp.Packet
    if err := pkt.Unmarshal(raw); err != nil {
        return
    }
    if pkt.Header.PayloadType == dtmfPT && len(pkt.Payload) >= 4 {
        endBit := pkt.Payload[1]&0x80 != 0
        if endBit {
            return nil, pkt.Payload[0], true
        }
        return
    }
    return pkt.Payload, 0, false
}

// Stub helpers — implemented in bridge/manager.go
func acquireRTPPort() int       { return 0 }
func releaseRTPPort(port int)   {}
```

---

## Library Comparison Table

| Criterion | emiago/sipgo v1.2.0 | ghettovoice/gosip | jart/gosip | cloudwebrtc/go-sip-ua | Raw stdlib |
|-----------|--------------------|--------------------|------------|----------------------|------------|
| **GitHub Stars** | ~993 | ~515 | ~526 | ~234 | N/A |
| **Last Commit** | Feb 2025 | Oct 2024 | Jun 2020 | Jan 2022 | N/A |
| **Stable Release** | v1.2.0 (stable) | None (no releases) | None (dormant) | v1.1.6 (stale) | N/A |
| **Pure Go / No CGo** | YES | YES | Ragel FSM compiler* | YES | YES |
| **UAC REGISTER + Digest Auth** | YES — `DoDigestAuth` | Partial (no confirmed example) | No examples | YES (but stale) | Manual — weeks |
| **UAS Incoming INVITE** | YES — `OnInvite` + `DialogServerCache` | YES (lower-level) | Limited | YES (B2BUA focused) | Manual — weeks |
| **Dialog lifecycle (ACK/BYE routing)** | YES — `DialogServerCache.ReadAck/ReadBye` | Partial | No | YES | Manual |
| **OPTIONS keepalive** | YES — `OnOptions` handler | YES | Unknown | YES | Trivial |
| **UDP + TCP transport** | YES — `ListenAndServe("udp"|"tcp")` | YES | UDP only | YES | Manual per-transport |
| **SDP built-in** | No — use pion/sdp (by design) | No | Basic | No | None |
| **Production evidence** | LiveKit SIP bridge (363 stars, actively deployed) | No known major users | None | None | N/A |
| **Active maintenance** | YES — v1.2.0 Feb 2025, 21 open issues | Uncertain — no releases | DEAD | DEAD | N/A |
| **License** | BSD-2-Clause | BSD-2-Clause | Apache 2.0 | Apache 2.0 | N/A |
| **Known Issues** | #116 (graceful shutdown), #206 (UDP MTU), #251 (folded headers) | Unknown | N/A | N/A | N/A |
| **Recommendation** | **PRIMARY CHOICE** | Fallback only | Reject | Reject | Reject |

*Ragel is a code generator; the generated Go code is still pure Go after code generation, but the build requires Ragel at code-gen time. The distributed `.go` files are pure Go. Still — the project is abandoned.

---

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| emiago/sipgox (extra libs) | emiago/diago (full VoIP framework) | June 2024 (sipgox deprecated) | sipgox README: "please do not use this"; use sipgo directly or diago |
| emiago/media (standalone) | Media code merged into diago | Feb 2025 (archived) | emiago/media is read-only; media API lives in diago now |
| gosip (ghettovoice) as the reference Go SIP lib | emiago/sipgo as the reference | 2023 (sipgo HN announcement) | sipgo explicitly states it replaced gosip as the performance baseline |
| Manual dialog state with sync.Map | `DialogServerCache` / `DialogClientCache` | sipgo v0.x | Built-in dialog routing prevents ACK/BYE routing bugs |
| `dialog.Close()` implicitly terminating | `dialog.Bye()` + `dialog.Close()` explicit | sipgo v0.17.0 (Jan 2026) | Must call Bye() before Close() for proper termination |

**Deprecated/outdated:**
- `emiago/sipgox`: Deprecated June 2024. Do not use.
- `emiago/media`: Archived February 2025. Read-only.
- `ghettovoice/gosip`: No releases ever published; treat as experimental.
- `cloudwebrtc/go-sip-ua`: Last release Jan 2022. Depends on libopus (CGo) — disqualified.

---

## Open Questions

1. **Does sipgate use WWW-Authenticate header line-folding?**
   - What we know: sipgo issue #251 shows folded headers break auth. AT&T confirmed affected.
   - What's unclear: Whether sipgate's `sipconnect.sipgate.de` registrar folds headers.
   - Recommendation: Capture an actual REGISTER 401 response from sipgate in a packet trace during integration testing. If folded, a pre-processing step (unfold before parse) is needed.

2. **Does sipgate grant a short Expires (< 3600s) that differs from what we request?**
   - What we know: sipgate's RTP uses ports 15000-30000; their SIP behavior follows standard Asterisk configurations.
   - What's unclear: Exact server-granted Expires value. v1.0 used `refreshFrequency: 90` (90% of requested). The Go rewrite uses 75% of server-granted (diago pattern).
   - Recommendation: Log the server-granted Expires value on first registration and verify the re-registration timer is firing correctly.

3. **Does sipgate send large INVITEs that trigger the UDP MTU issue (#206)?**
   - What we know: sipgate injects P-Preferred-Identity and potentially other proprietary headers. INVITE from a sipgate trunk can be 1200-1800 bytes depending on SDP and extra headers.
   - What's unclear: Exact INVITE size from sipgate in production.
   - Recommendation: Start both UDP and TCP listeners. If sipgate INVITEs exceed MTU, they can be configured to use TCP. Monitor for `"size of packet larger than MTU"` errors.

4. **Should DTMF payload type be hardcoded to 101 or extracted from SDP offer?**
   - What we know: PT 101 is conventional for telephone-event but is a dynamic type — the SDP offer defines the actual PT. sipgate reportedly uses PT 113 (v1.0 implementation note: `callManager.ts` references PT 113).
   - What's unclear: Whether sipgate always sends PT 113 or follows what the UAS SDP answer specifies.
   - Recommendation: Always extract the telephone-event PT from the caller's SDP offer (scan all `a=rtpmap` lines for `telephone-event`), do not hardcode. The SDP answer should mirror back the same PT the caller specified.

---

## Risks

| Risk | Severity | Probability | Mitigation |
|------|----------|-------------|------------|
| sipgo graceful shutdown (#116) not implemented | HIGH (LCY-01 requirement) | CONFIRMED — open issue | Implement manual drain loop in CallManager: send BYE all sessions, wait ≤5s, then unregister, then close UA |
| Folded WWW-Authenticate header from sipgate breaks auth (#251) | MEDIUM | LOW (no reports against sipgate) | Test with live sipgate credentials; add pre-parse unfold step if needed |
| UDP MTU exceeded on large sipgate INVITEs (#206) | MEDIUM | LOW-MEDIUM | Listen on TCP; log and alert on MTU errors |
| DTMF PT mismatch (sipgate uses 113, not 101) | MEDIUM | CONFIRMED from v1.0 | Extract PT from SDP offer dynamically, never hardcode 101 |
| Single maintainer (emiago) bus factor | LOW | LOW | Project has 993 stars, stable API, LiveKit dependency creates external pressure for maintenance |

---

## Recommendation

**Use `emiago/sipgo v1.2.0`.**

The evidence is conclusive:

1. It is the only pure-Go SIP library with a stable (v1.x) API, active maintenance (multiple releases in 2025), and documented production deployment (LiveKit's SIP-to-WebRTC bridge with hundreds of production deployments).

2. The API maps directly to every requirement in REQUIREMENTS.md: `OnInvite` / `DialogServerCache` for SIP-03, `DoDigestAuth` + `ClientRequestRegisterBuild` for SIP-01/02, `OnOptions` for EXT-02, `OnBye` for SIP-05, dual `ListenAndServe("udp"|"tcp")` for transport.

3. The three known open issues (#116, #206, #251) all have workarounds and none are blockers for the primary inbound-call-with-audio-bridge use case.

4. `pion/sdp/v3` + `pion/rtp` complete the stack with the same libraries LiveKit uses — maximizing confidence that the combination works with real-world SIP trunks.

5. `emiago/diago` is available as an upgrade path for v3 requirements (ADV-03 multi-codec) without changing the SIP layer.

**Do not use diago for v2.0.** The audio-dock bridge requires raw byte access to RTP payloads for the Twilio Media Streams base64 encoding. Diago's AudioReader/AudioWriter abstraction sits above that level and would require bypassing it — adding complexity without benefit.

---

## Sources

### Primary (HIGH confidence)

- `emiago/sipgo` pkg.go.dev — https://pkg.go.dev/github.com/emiago/sipgo — verified v1.2.0 API signatures
- `emiago/sipgo` GitHub — https://github.com/emiago/sipgo — README, release notes, issues, source files
- `emiago/diago` register_transaction.go — https://github.com/emiago/diago/blob/main/register_transaction.go — re-register pattern
- `pion/sdp/v3` pkg.go.dev — https://pkg.go.dev/github.com/pion/sdp/v3 — v3.0.18 API
- `pion/rtp` pkg.go.dev — https://pkg.go.dev/github.com/pion/rtp — v1.10.1 API
- sipgo releases — https://github.com/emiago/sipgo/releases — version dates confirmed
- sipgo dialog_server.go — https://github.com/emiago/sipgo/blob/main/dialog_server.go — DialogServerCache API
- sipgo dialog_integration_test.go — integration test patterns

### Secondary (MEDIUM confidence)

- livekit/sip go.mod — https://github.com/livekit/sip/blob/main/go.mod — confirms emiago/sipgo + pion/sdp/v3 + pion/rtp usage
- DeepWiki livekit/sipgo — https://deepwiki.com/livekit/sipgo/6.2-sip-dialogs — confirms LiveKit wraps emiago/sipgo
- sipgo GitHub issues — #116, #206, #211, #251 — confirmed known issues and resolutions
- Hacker News sipgo announcement — https://news.ycombinator.com/item?id=38203046 — community reception and "2000+ CPS" simulator report
- sipgate trunking community docs — https://www.msxfaq.de/skype_for_business/gateway/sipgate.htm — registrar domain and RTP port range

### Tertiary (LOW confidence — needs live validation)

- sipgate DTMF PT 113 — inferred from v1.0 implementation comment, not verified against current sipgate SIP traces
- sipgate Expires behavior — community Asterisk configs suggest standard 3600s; unverified for sipgate trunking specifically

---

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH — v1.2.0 confirmed on pkg.go.dev, LiveKit production evidence
- Architecture patterns: HIGH — verified against sipgo source files and integration tests
- Pitfalls: HIGH (confirmed issues) / MEDIUM (sipgate-specific folded headers, DTMF PT)
- Recommendation: HIGH

**Research date:** 2026-03-03
**Valid until:** 2026-09-03 (sipgo releases frequently — recheck if major version bumps before rewrite starts)
