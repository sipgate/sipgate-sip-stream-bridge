# Stack Research

**Domain:** SIP/RTP/WebSocket audio bridge — Go rewrite (v2.0)
**Researched:** 2026-03-03
**Confidence:** HIGH — all major libraries verified via pkg.go.dev and GitHub releases. Version numbers sourced from authoritative locations.

---

## The Critical Decision You Must Read First

**sipgo (github.com/emiago/sipgo) is the correct Go SIP library.** It is the only actively maintained pure-Go SIP library with digest auth, dialog management, and UDP/TCP transport that does not require a C++ sidecar. Alternatives (pjsip bindings, drachtio Go client) all have C dependencies or are abandoned.

**For WebSocket, use gorilla/websocket v1.5.3.** The alternative (coder/websocket) is more idiomatic but gorilla is the industry default with 42K importers. For a WS-client-only role (this service dials out, never serves), either works — gorilla is the safe, battle-tested choice.

**For config, use godotenv + kelseyhightower/envconfig together.** envconfig handles struct-tag-based env parsing (equivalent to Zod validation in v1). godotenv loads `.env` files in dev. In production Docker, pass env vars directly — godotenv is a dev convenience only.

**For Docker, use `FROM scratch` with a statically linked binary.** This project connects to sipgate via plain UDP SIP (no outbound TLS required). No CA certs needed. `FROM scratch` gives the smallest possible image.

---

## Recommended Stack

### Core Technologies

| Technology | Version | Purpose | Why Recommended |
|------------|---------|---------|-----------------|
| Go | 1.23+ | Language / runtime | Minimum required by prometheus/client_golang v1.23.x; goroutines replace Node.js event loop; `net.UDPConn` replaces dgram; no GC-induced drain-queue workarounds needed |
| github.com/emiago/sipgo | v1.2.0 | SIP signaling (REGISTER, INVITE, BYE, OPTIONS) | Only actively maintained pure-Go SIP library; RFC 3261/3581/6026; built-in digest auth via `DoDigestAuth`/`TransactionDigestAuth`; Dialog abstractions (DialogServerSession, DialogServerCache); UDP/TCP transport both supported natively |
| net.UDPConn (stdlib) | — | RTP audio transport (per-call UDP socket) | Zero dependencies; direct syscall; goroutine-per-socket gives deterministic latency; replaces Node.js `dgram.Socket` 1:1 |
| github.com/gorilla/websocket | v1.5.3 | WebSocket client (outbound to AI backend) | 42K importers; battle-tested; straightforward Dial/ReadMessage/WriteMessage API; sufficient for Twilio Media Streams JSON-over-WS protocol |
| net/http (stdlib) | — | HTTP server for /health and /metrics | Go 1.22+ ServeMux supports method-specific routing (`GET /health`); no framework needed for two endpoints |

### Supporting Libraries

| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| github.com/rs/zerolog | v1.34.0 | Structured JSON logging | Always. Zerolog is the fastest Go JSON logger (zero allocation); chainable API mirrors pino DX; replaces pino 1:1. Use `zerolog.New(os.Stdout).With().Timestamp().Logger()` for container stdout logging |
| github.com/prometheus/client_golang | v1.23.2 | Prometheus metrics (/metrics endpoint) | Always (OBS-03 requirement). Import `prometheus` + `promhttp` sub-packages. Use `prometheus.NewRegistry()` (not default registry) to avoid leaking default Go runtime metrics unless wanted |
| github.com/joho/godotenv | v1.5.1 | Load .env file in development | Dev only. Call `godotenv.Load()` at startup before config parsing. In Docker, skip — env vars come from the container environment. Feature-complete, maintenance mode — stable |
| github.com/kelseyhightower/envconfig | v1.4.0 | Parse env vars into typed Go struct | Always. Struct tags with `envconfig:"SIP_USER"` provide the same fail-fast validation as v1's Zod schema. Supports default values, required fields, custom parsers. Last release 2019 but stable — 8.5K importers |

### Development Tools

| Tool | Purpose | Notes |
|------|---------|-------|
| `go build` | Compile static binary | `CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o audio-dock ./cmd/audio-dock` |
| `go test` | Run unit tests | Table-driven tests for SDP parsing, RTP header parsing, config validation; no external test framework needed |
| `go vet` + `staticcheck` | Static analysis | `go vet ./...` catches common errors; `staticcheck` (honnef.co/go/tools) catches more; replace ESLint equivalent |
| `golangci-lint` | Aggregate linter | Runs vet + staticcheck + errcheck + others in one pass; configure `.golangci.yml` to match project style |
| Air (`github.com/air-verse/air`) | Hot reload in development | Watch-and-rebuild for dev loop; replaces `tsx watch`; optional but improves dev experience |

---

## Installation (go.mod dependencies)

```bash
# Initialize module
go mod init github.com/sipgate/audio-dock

# Core SIP library
go get github.com/emiago/sipgo@v1.2.0

# WebSocket client
go get github.com/gorilla/websocket@v1.5.3

# Structured logging
go get github.com/rs/zerolog@v1.34.0

# Prometheus metrics
go get github.com/prometheus/client_golang@v1.23.2

# Config / env parsing
go get github.com/kelseyhightower/envconfig@v1.4.0
go get github.com/joho/godotenv@v1.5.1

# Tidy and verify
go mod tidy
go mod verify
```

---

## Alternatives Considered

| Recommended | Alternative | When to Use Alternative |
|-------------|-------------|-------------------------|
| github.com/emiago/sipgo | github.com/ghettovoice/gosip | gosip is unmaintained (last commit 2022); sipgo is actively maintained with v1.2.0 released Feb 2025. Never use gosip for new projects. |
| github.com/emiago/sipgo | pjsip Go bindings (sipster equivalent) | If you need extreme SIP compliance edge-case handling for carrier interop. Requires CGO, complicates Docker. Not applicable here. |
| github.com/emiago/sipgo | ghettovoice/gosip | See above — abandoned. |
| net.UDPConn (stdlib) | github.com/wernerd/GoRTP | GoRTP is a full RTP stack but last updated 2014. For PCMU-only with custom DTMF handling (PT 113), raw UDPConn + manual header parsing is simpler and faster than a framework. |
| gorilla/websocket | github.com/coder/websocket | coder/websocket has better context.Context support and is more idiomatic Go. Recommended for greenfield projects. Gorilla chosen here for stability and match to the existing community patterns. Switch to coder if gorilla maintenance concerns arise. |
| gorilla/websocket | gobwas/ws | Maximum performance for WS server roles. Overkill for a WS client sending 20 ms audio packets; complex API. |
| github.com/rs/zerolog | log/slog (stdlib, Go 1.21+) | slog is zero-dependency and performs well. Valid alternative for new Go projects. zerolog is faster in raw benchmarks and its chainable API is cleaner for structured event logging. Either is acceptable — zerolog is the recommendation for this audio-latency-sensitive service. |
| github.com/kelseyhightower/envconfig | github.com/caarlos0/env | caarlos0/env is actively maintained (unlike envconfig) and supports Go generics. If envconfig's lack of maintenance becomes a concern, migrate to `github.com/caarlos0/env/v11`. API is nearly identical. |
| FROM scratch | gcr.io/distroless/static-debian12 | Use distroless if: (1) you need CA certificates for TLS outbound (not needed here — SIP is plain UDP, WS target may be ws:// or wss://). If WS_TARGET_URL is wss://, you MUST add CA certs — either switch to distroless or COPY from builder. (2) Google rebuilds distroless on CVE fixes automatically, giving security updates without rebuilding. |

---

## What NOT to Use

| Avoid | Why | Use Instead |
|-------|-----|-------------|
| github.com/ghettovoice/gosip | Last commit 2022, unmaintained, open issues unresolved, no releases after the project was forked from jart/gosip | github.com/emiago/sipgo |
| github.com/wernerd/GoRTP | Last commit 2014, pre-Go modules, no maintenance. The RTP packet format is 12 bytes — parse it inline with binary.BigEndian, no library needed | net.UDPConn + manual header parse |
| github.com/gorilla/mux | Router overkill for two endpoints (/health, /metrics). Go 1.22+ stdlib ServeMux supports method routing natively. | net/http ServeMux with Go 1.22+ patterns |
| github.com/gin-gonic/gin or echo | HTTP framework adds ~5 MB binary size for two static endpoints. stdlib ServeMux is sufficient. | net/http |
| CGO-based anything | CGO breaks `FROM scratch` and complicates cross-compilation. All chosen libraries are pure Go. Never enable CGO for this project. | CGO_ENABLED=0 always |
| global prometheus.DefaultRegisterer | DefaultRegisterer includes Go runtime metrics (GC pauses, goroutine counts) by default. These are useful but mix with application metrics. Use `prometheus.NewRegistry()` explicitly and add `collectors.NewGoCollector()` only if you want runtime metrics. | prometheus.NewRegistry() |

---

## Stack Patterns by Variant

**If WS_TARGET_URL uses wss:// (TLS WebSocket):**
- Switch Dockerfile base from `FROM scratch` to `gcr.io/distroless/static-debian12:nonroot`
- Distroless includes CA certificates; scratch does not
- Add `COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/` if keeping scratch
- gorilla/websocket TLS dial works transparently via `tls.Dial` when URL scheme is `wss://`

**If concurrent call count is expected to exceed 50:**
- goroutine-per-call model scales well in Go; each call costs ~2 goroutines (RTP reader + WS writer) + ~10 KB stack
- 100 calls = ~200 goroutines = ~2 MB goroutine stacks; well within limits
- No concurrency architecture changes needed up to ~1000 concurrent calls

**If sipgate sends SIP over TCP instead of UDP:**
- sipgo supports TCP transport; change `ListenAndServe` from `"udp"` to `"tcp"`
- sipgate trunking is documented as UDP port 5060; TCP is not required but sipgo handles it if needed

**If config validation needs cross-field rules (e.g., RTP_PORT_MIN < RTP_PORT_MAX):**
- envconfig does not support cross-field validation
- Add a `Validate() error` method on the Config struct and call it after `envconfig.Process`
- This replicates v1's Zod `.refine()` pattern

---

## RTP Implementation Pattern (stdlib only, no library)

The v1 RTP handler is 253 lines. In Go, replace it with `net.UDPConn` directly:

```go
// Per-call RTP socket — goroutine-per-socket pattern
conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: localPort})

// Receive goroutine
go func() {
    buf := make([]byte, 1500) // MTU-sized buffer
    for {
        n, _, err := conn.ReadFrom(buf)
        if err != nil { return }
        parseRTPAndForward(buf[:n])
    }
}()

// Send: build 12-byte RTP header manually
func buildRTPPacket(payload []byte, seq uint16, ts uint32, ssrc uint32) []byte {
    pkt := make([]byte, 12+len(payload))
    pkt[0] = 0x80             // V=2, P=0, X=0, CC=0
    pkt[1] = 0x00             // M=0, PT=0 (PCMU)
    binary.BigEndian.PutUint16(pkt[2:], seq)
    binary.BigEndian.PutUint32(pkt[4:], ts)
    binary.BigEndian.PutUint32(pkt[8:], ssrc)
    copy(pkt[12:], payload)
    return pkt
}
```

Key considerations from v1:
- DTMF: sipgate uses PT 113 (telephone-event/8000), not PT 101. Parse `buf[1] & 0x7f` for payload type.
- RFC 4733 dedup: sender transmits 3 redundant End packets with same RTP timestamp. Deduplicate by storing `lastDtmfTimestamp`.
- Port allocation: use `sync/atomic` for thread-safe port counter (replaces v1's module-level `nextPort` — Go is not single-threaded like Node.js).

---

## Config Struct Pattern (envconfig replacement for Zod)

```go
type Config struct {
    SIPUser      string `envconfig:"SIP_USER"      required:"true"`
    SIPPassword  string `envconfig:"SIP_PASSWORD"   required:"true"`
    SIPDomain    string `envconfig:"SIP_DOMAIN"     required:"true"`
    SIPRegistrar string `envconfig:"SIP_REGISTRAR"  required:"true"`
    WSTargetURL  string `envconfig:"WS_TARGET_URL"  required:"true"`
    RTPPortMin   int    `envconfig:"RTP_PORT_MIN"   default:"10000"`
    RTPPortMax   int    `envconfig:"RTP_PORT_MAX"   default:"10099"`
    SDPContactIP string `envconfig:"SDP_CONTACT_IP" default:""`
    SIPExpires   int    `envconfig:"SIP_EXPIRES"    default:"120"`
    LogLevel     string `envconfig:"LOG_LEVEL"      default:"info"`
}

func LoadConfig() (*Config, error) {
    _ = godotenv.Load() // No-op if .env absent — safe in Docker
    var cfg Config
    if err := envconfig.Process("", &cfg); err != nil {
        return nil, fmt.Errorf("config: %w", err)
    }
    if cfg.RTPPortMin >= cfg.RTPPortMax {
        return nil, fmt.Errorf("RTP_PORT_MIN (%d) must be less than RTP_PORT_MAX (%d)", cfg.RTPPortMin, cfg.RTPPortMax)
    }
    return &cfg, nil
}
```

---

## Dockerfile Pattern (FROM scratch)

```dockerfile
# ── Build stage ──────────────────────────────────────────────────────────────
FROM golang:1.23-alpine AS builder
WORKDIR /app

# Download dependencies first (layer cache)
COPY go.mod go.sum ./
RUN go mod download

# Build static binary
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w" \
    -trimpath \
    -o audio-dock \
    ./cmd/audio-dock

# ── Final stage: scratch (zero OS) ───────────────────────────────────────────
FROM scratch
COPY --from=builder /app/audio-dock /audio-dock

# If WS_TARGET_URL is wss:// (TLS), add CA certs:
# COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

EXPOSE 8080
ENTRYPOINT ["/audio-dock"]
```

**Size comparison:**
- v1 Node.js image: ~180 MB (node:22-alpine)
- v2 Go scratch image: ~8-15 MB (binary only)

**network_mode: host is still required** — RTP requires no NAT; Docker port-proxy adds ~10ms UDP jitter. This was a validated v1 decision and carries forward to v2.

---

## sipgo Integration: Key Types for Audio-Dock

```
sipgo.NewUA()              → UserAgent (manages transport)
sipgo.NewServer(ua)        → UAS: handles INVITE/BYE/OPTIONS callbacks
sipgo.NewClient(ua)        → UAC: sends REGISTER with digest auth

// REGISTER flow
client.Do(ctx, registerReq)          // sends REGISTER
client.DoDigestAuth(ctx, req, res401, DigestAuth{Username, Password})

// Inbound INVITE flow
srv.OnInvite(func(req, tx) {
    dlg := dialogSrv.ReadInvite(req, tx)
    dlg.Respond(200, "OK", sdpBody)
    dlg.ReadAck(ctx)
    // start RTP + WS bridge goroutines
    dlg.ReadBye(ctx)  // blocks until BYE
})
```

**sipgate-specific notes (confirmed from v1 validation):**
- Registrar: `sipconnect.sipgate.de:5060` (UDP)
- Auth: digest (401 challenge-response) — sipgo `DoDigestAuth` handles this
- Re-register: send REGISTER before `SIP_EXPIRES/2` seconds elapse (v1 used `refreshFrequency: 90`); implement with `time.NewTicker(time.Duration(cfg.SIPExpires/2) * time.Second)`
- DTMF: PT 113 (telephone-event/8000) — NOT PT 101; sipgate-specific payload type

---

## Version Compatibility

| Package | Version | Go Minimum | Notes |
|---------|---------|------------|-------|
| github.com/emiago/sipgo | v1.2.0 | go 1.21 | Feb 2025 release; fixes UDP transport memory leak and ACK address handling |
| github.com/gorilla/websocket | v1.5.3 | go 1.16 | Jun 2024; stable API, no breaking changes planned |
| github.com/rs/zerolog | v1.34.0 | go 1.19 | Mar 2025; no breaking changes from v1.x |
| github.com/prometheus/client_golang | v1.23.2 | go 1.23 | Sep 2025; requires Go 1.23+; only latest 2 Go versions supported |
| github.com/kelseyhightower/envconfig | v1.4.0 | go 1.12 | 2019; feature-complete; no API changes |
| github.com/joho/godotenv | v1.5.1 | go 1.17 | Feb 2023; maintenance-only; stable |

**Minimum Go version: 1.23** (driven by prometheus/client_golang v1.23.2 requirement).

---

## Sources

- [github.com/emiago/sipgo releases](https://github.com/emiago/sipgo/releases) — v1.2.0 confirmed Feb 7, 2025 (HIGH confidence)
- [pkg.go.dev/github.com/emiago/sipgo](https://pkg.go.dev/github.com/emiago/sipgo) — API documentation: UserAgent, Server, Client, DialogServerSession, DoDigestAuth (HIGH confidence)
- [github.com/gorilla/websocket releases](https://github.com/gorilla/websocket/releases) — v1.5.3 confirmed Jun 14, 2024; 42K importers; stable (HIGH confidence)
- [pkg.go.dev/github.com/rs/zerolog](https://pkg.go.dev/github.com/rs/zerolog) — v1.34.0 Mar 21, 2025; 28K importers (HIGH confidence)
- [github.com/prometheus/client_golang releases](https://github.com/prometheus/client_golang/releases) — v1.23.2 Sep 5, 2025; minimum Go 1.23 (HIGH confidence)
- [pkg.go.dev/github.com/kelseyhightower/envconfig](https://pkg.go.dev/github.com/kelseyhightower/envconfig) — v1.4.0; 8.5K importers; feature-complete (HIGH confidence)
- [pkg.go.dev/github.com/joho/godotenv](https://pkg.go.dev/github.com/joho/godotenv) — v1.5.1; 49K importers; maintenance mode (HIGH confidence)
- [Go 1.22 routing enhancements](https://go.dev/blog/routing-enhancements) — method routing in stdlib ServeMux (HIGH confidence)
- [GoogleContainerTools/distroless](https://github.com/GoogleContainerTools/distroless) — static-debian12 for Go static binaries; nonroot variant available (HIGH confidence)
- [Go Forum: WebSocket in 2025](https://forum.golangbridge.org/t/websocket-in-2025/38671) — gorilla vs coder consensus (MEDIUM confidence)
- [sipgate trunking: registrar sipconnect.sipgate.de:5060 UDP](https://techdocs.audiocodes.com/livehub/Content/LiveHub/interop_configuration_sipgate_sip_trunk.htm) — confirmed domain and port (MEDIUM confidence — third-party interop doc, not sipgate official)
- v1.0 validated implementation (src/config/index.ts, src/rtp/rtpHandler.ts, src/ws/wsClient.ts, src/sip/sdp.ts) — authoritative source for exact protocol details: PT 113 DTMF, PCMU-only, Twilio Media Streams format (HIGH confidence)

---

*Stack research for: audio-dock v2.0 Go rewrite*
*Researched: 2026-03-03*
