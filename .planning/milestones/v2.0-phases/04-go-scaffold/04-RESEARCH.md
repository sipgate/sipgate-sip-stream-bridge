# Phase 4: Go Scaffold - Research

**Researched:** 2026-03-03
**Domain:** Go module initialization, env-var validation, structured JSON logging (zerolog), static Docker binary
**Confidence:** HIGH

---

## Summary

Phase 4 establishes the skeleton of the Go rewrite: a runnable binary that validates all required environment variables at startup, logs structured JSON with zerolog, exits with a descriptive error on misconfiguration, and ships as a `FROM scratch` static binary image under 20 MB. This phase has no SIP, RTP, or WebSocket logic — it is purely the scaffold that later phases build on.

The standard Go project layout for a self-contained service places all implementation packages under `internal/` and the `main.go` entry point under `cmd/audio-dock/`. The v1.0 TypeScript reference uses Zod for env-var validation with a fail-fast schema; the Go equivalent is `go-simpler.org/env` (struct tags, required fields, typed integer parsing, clear error messages naming the missing variable). Logging uses `rs/zerolog` v1.34.0 (Mar 2025), which writes zero-allocation JSON to stdout out of the box and supports child loggers carrying per-call fields like `callId` and `streamSid` — critical for later phases.

The Docker pattern is a two-stage build: `golang:1.26-alpine` builder stage compiles with `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -trimpath`, producing a statically-linked binary. The scratch stage copies only the binary (plus CA certs if HTTPS calls are ever needed). Typical output image is 5–15 MB, well under the 20 MB requirement. Docker Compose must carry over the v1.0 `network_mode: host` setting with an env_file reference.

**Primary recommendation:** Use `go-simpler.org/env` for config validation, `rs/zerolog` for structured logging, and a two-stage `golang:1.26-alpine` + `FROM scratch` Dockerfile. The Go module path should be `github.com/sipgate/audio-dock` (or a private equivalent) with `go 1.25` declared in `go.mod` (the default when using Go 1.26 toolchain).

---

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| CFG-01 | SIP credentials (user, password, domain/registrar) configured via env vars — same names as v1.0 | `go-simpler.org/env` struct tags bind `SIP_USER`, `SIP_PASSWORD`, `SIP_DOMAIN` directly; `required` tag enforces presence |
| CFG-02 | Target WebSocket URL configured via env var | `WS_URL` field with `required` tag; can validate URL format with a custom `TextUnmarshaler` or post-load check |
| CFG-03 | RTP port range (min/max) configured via env vars | `RTP_PORT_MIN` / `RTP_PORT_MAX` as `int` fields; `default` tags for fallback values; cross-field validation in a post-load function |
| CFG-04 | External/reachable IP for SDP contact line configured via env var | `SDP_CONTACT_IP` field; validated non-empty; used in SDP answer in later phases |
| CFG-05 | Service fails to start with a descriptive error if any required config is missing | `env.Load()` returns `*NotSetError` naming the missing variable; `log.Fatal()` prints it as structured JSON then exits non-zero |
| OBS-01 | Structured JSON log with callId/streamSid context on each relevant line (zerolog) | `zerolog.New(os.Stdout).With().Timestamp().Logger()` outputs JSON; child loggers via `.With().Str("callId", id).Logger()` carry per-call context; this phase establishes the base logger only |
| DCK-01 | Static Go binary with `CGO_ENABLED=0` — no Node.js runtime | `CGO_ENABLED=0 go build` in Dockerfile builder stage; verified by `file ./audio-dock` showing "statically linked" |
| DCK-02 | Docker Compose file with `network_mode: host` for Linux production | Update existing `docker-compose.yml`: change image/command to Go binary, keep `network_mode: host` and `env_file: .env` |
| DCK-03 | Dockerfile enforces `CGO_ENABLED=0`; final image `FROM scratch` or distroless (~8–15 MB) | Two-stage Dockerfile: `golang:1.26-alpine` builder + `FROM scratch` runtime; `-ldflags="-s -w" -trimpath` reduces binary size |
</phase_requirements>

---

## Standard Stack

### Core

| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `go-simpler.org/env` | v0.12.0 (May 2024) | Env var parsing into typed struct; `required` tag for fail-fast; `default` tag for optional with fallback | Mirrors v1.0 Zod schema pattern; descriptive error messages; pure stdlib, no reflection overhead; Go 1.20+ |
| `github.com/rs/zerolog` | v1.34.0 (Mar 2025) | Zero-allocation structured JSON logging to stdout; child loggers with per-call context fields | Locked decision from STATE.md; low-allocation hot path critical for audio goroutines; widely adopted |

### Supporting

| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| Go stdlib `os`, `fmt`, `context`, `signal` | Go 1.26 (installed) | Signal handling, process exit, env fallback | Entry-point wiring in `main.go`; no extra deps needed |
| `golang:1.26-alpine` builder image | Go 1.26.0 | Compile stage for multi-stage Docker build | Builder stage only — not in final image |
| `FROM scratch` final image | — | Minimal runtime with only the static binary | Final Docker image stage; satisfies DCK-03 |

### Alternatives Considered

| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| `go-simpler.org/env` | `github.com/sethvargo/go-envconfig` | go-envconfig is more feature-rich (mutations, pre-setters) but heavier; go-simpler/env mirrors the v1.0 Zod pattern more closely (struct tags, required, default) |
| `go-simpler.org/env` | `github.com/kelseyhightower/envconfig` | envconfig is battle-tested but older API; no active development since 2021 |
| `go-simpler.org/env` | Manual `os.LookupEnv` per variable | Scales poorly; no struct tag composability; requires hand-written error messages for each variable |
| `rs/zerolog` | `go.uber.org/zap` | zap is also zero-allocation but requires a `Build()` call and separate `SugaredLogger`; zerolog is locked decision (STATE.md) |
| `rs/zerolog` | `log/slog` (stdlib Go 1.21+) | slog is now part of stdlib and performant, but zerolog is already decided; slog would be valid for a new project |
| `FROM scratch` | `gcr.io/distroless/static:nonroot` | distroless adds a nobody user (good for non-root security) but adds ~1 MB; both satisfy DCK-03; scratch is simpler for this phase |

**Installation:**
```bash
go get go-simpler.org/env@v0.12.0
go get github.com/rs/zerolog@v1.34.0
```

---

## Architecture Patterns

### Recommended Project Structure

```
audio-dock/                       # repo root (existing — contains Node.js v1.0)
├── go.mod                        # new — Go module definition
├── go.sum                        # new — dependency checksums
├── Dockerfile                    # replace Node.js Dockerfile with Go version
├── docker-compose.yml            # update to use Go binary
├── cmd/
│   └── audio-dock/
│       └── main.go               # entry point: load config, init logger, wire components, wait for signal
└── internal/
    └── config/
        └── config.go             # Config struct, env.Load(), fail-fast validation
```

Later phases (5–8) will add `internal/sip/`, `internal/bridge/`, `internal/obs/`. This phase creates only `cmd/audio-dock/main.go` and `internal/config/config.go`.

### Pattern 1: go.mod Initialization

**What:** Go 1.26 toolchain defaults to `go 1.25` in the `go.mod` line. Module path should use the repository's canonical path.

**Example:**
```bash
# Run from repo root
go mod init github.com/sipgate/audio-dock
# Creates go.mod with:
#   module github.com/sipgate/audio-dock
#   go 1.25
go get go-simpler.org/env@v0.12.0
go get github.com/rs/zerolog@v1.34.0
```

### Pattern 2: Config Struct with go-simpler/env

**What:** A typed struct with struct tags defines all env vars. `env.Load()` parses, validates required fields, and returns a descriptive error naming the missing variable.

**Key behaviors:**
- `required` tag: returns `*env.NotSetError` with the variable name if absent
- `default` tag: provides fallback values for optional vars
- `int` fields: parsed via `strconv.Atoi`; no range validation (requires post-load check)
- `env.Load()` collects ALL missing required vars into a single error (not just the first)

```go
// Source: https://pkg.go.dev/go-simpler.org/env v0.12.0
// internal/config/config.go

package config

import (
    "fmt"
    "os"

    "go-simpler.org/env"
)

// Config holds all environment-variable configuration for audio-dock.
// Field names match v1.0 env var names exactly (CFG-01 through CFG-04).
type Config struct {
    // SIP credentials (CFG-01)
    SIPUser     string `env:"SIP_USER,required"     usage:"SIP username / SIP-ID (e.g. e12345p0)"`
    SIPPassword string `env:"SIP_PASSWORD,required" usage:"SIP account password"`
    SIPDomain   string `env:"SIP_DOMAIN,required"   usage:"SIP registrar domain (e.g. sipconnect.sipgate.de)"`

    // WebSocket target (CFG-02)
    WSURL string `env:"WS_URL,required" usage:"Target WebSocket URL (e.g. wss://my-bot.example.com/ws)"`

    // RTP port range (CFG-03)
    RTPPortMin int `env:"RTP_PORT_MIN" default:"10000" usage:"Minimum UDP port for RTP (inclusive)"`
    RTPPortMax int `env:"RTP_PORT_MAX" default:"10099" usage:"Maximum UDP port for RTP (inclusive)"`

    // SDP contact IP (CFG-04)
    SDPContactIP string `env:"SDP_CONTACT_IP,required" usage:"Externally-reachable IP address for SDP contact line"`
}

// Load reads environment variables into Config and fails fast on misconfiguration (CFG-05).
// Exits the process with a structured error log if any required variable is missing or invalid.
func Load() Config {
    var cfg Config
    if err := env.Load(&cfg, nil); err != nil {
        // env.Load error message already names the missing variable:
        // "env: SIP_USER is required but not set"
        fmt.Fprintf(os.Stderr, `{"level":"fatal","msg":"Configuration error","error":%q}`+"\n", err.Error())
        os.Exit(1)
    }

    // Post-load cross-field validation (env package does not support this natively)
    if cfg.RTPPortMin >= cfg.RTPPortMax {
        fmt.Fprintf(os.Stderr, `{"level":"fatal","msg":"RTP_PORT_MIN must be less than RTP_PORT_MAX","RTP_PORT_MIN":%d,"RTP_PORT_MAX":%d}`+"\n",
            cfg.RTPPortMin, cfg.RTPPortMax)
        os.Exit(1)
    }

    return cfg
}
```

**Note:** The `fmt.Fprintf(os.Stderr, ...)` pre-logger output ensures a JSON-looking error even before the zerolog logger is initialized. Alternatively, initialize zerolog first (it has no required config) then use `log.Fatal().Err(err).Msg(...)`.

### Pattern 3: Zerolog Base Logger Initialization (OBS-01)

**What:** Create the base logger once in `main.go`, then derive per-component child loggers. Child loggers created with `.With().Str("component", "sip").Logger()` carry the field on every subsequent log call — no boilerplate per line.

```go
// Source: https://pkg.go.dev/github.com/rs/zerolog v1.34.0
// cmd/audio-dock/main.go

package main

import (
    "os"

    "github.com/rs/zerolog"
    "github.com/sipgate/audio-dock/internal/config"
)

func main() {
    // 1. Load and validate config first (exits on error — CFG-05)
    cfg := config.Load()

    // 2. Initialize base logger (OBS-01)
    // zerolog.New(os.Stdout) writes JSON to stdout
    // .With().Timestamp() adds "time" field to every event
    logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

    // Startup log — emitted only after config is valid
    logger.Info().
        Str("sip_user", cfg.SIPUser).
        Str("sip_domain", cfg.SIPDomain).
        Int("rtp_port_min", cfg.RTPPortMin).
        Int("rtp_port_max", cfg.RTPPortMax).
        Msg("audio-dock starting")

    // 3. Per-component loggers (used in later phases)
    // sipLog := logger.With().Str("component", "sip").Logger()
    // bridgeLog := logger.With().Str("component", "bridge").Logger()

    // 4. Per-call loggers (used in Phase 6+)
    // callLog := logger.With().Str("callId", callID).Str("streamSid", streamSid).Logger()

    // 5. Wait for SIGTERM/SIGINT (LCY-01 — full implementation in Phase 8)
    // For Phase 4 scaffold: just keep the process alive
    select {}
}
```

**Child logger for per-call context (Phase 6+ preview):**
```go
// Source: https://pkg.go.dev/github.com/rs/zerolog v1.34.0
callLog := logger.With().
    Str("callId", req.CallID().Value()).
    Str("streamSid", streamSid).
    Logger()

callLog.Info().Msg("call started")
// {"level":"info","time":"...","callId":"abc123","streamSid":"MX...","message":"call started"}
```

### Pattern 4: Two-Stage Static Docker Build (DCK-01, DCK-03)

**What:** Builder stage compiles a statically-linked binary; scratch stage contains only the binary.

```dockerfile
# Source: https://dasroot.net/posts/2026/02/building-minimal-go-containers-high-throughput-apis/
# Source: go.dev/doc/install

# ── Stage 1: Build ───────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS builder

WORKDIR /app

# Download dependencies first — this layer caches unless go.mod/go.sum changes
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build static binary
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
    -ldflags="-s -w" \
    -trimpath \
    -o /audio-dock \
    ./cmd/audio-dock

# ── Stage 2: Runtime ─────────────────────────────────────────────────────────
# FROM scratch = empty filesystem; only our binary runs
FROM scratch

# Copy CA certificates for any HTTPS calls (not needed in Phase 4, included for later phases)
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

COPY --from=builder /audio-dock /audio-dock

# SIP port (used in later phases — documented here for reference)
# No EXPOSE needed with network_mode: host; included for dev/documentation
EXPOSE 5060/udp
EXPOSE 5060/tcp

ENTRYPOINT ["/audio-dock"]
```

**Build flags explained:**
- `CGO_ENABLED=0`: disables C bindings — produces a truly static binary (DCK-01)
- `GOOS=linux GOARCH=amd64`: explicit cross-compile target; safe even on macOS M-series ARM hosts
- `-ldflags="-s -w"`: strips symbol table (`-s`) and DWARF debug info (`-w`); reduces binary 25–35%
- `-trimpath`: removes local filesystem paths from binary (reproducible builds, smaller size, no path leakage)

**Verify static linkage:**
```bash
docker run --rm audio-dock:latest /audio-dock --version || true
# Or on the build host:
file ./audio-dock  # should show "statically linked"
```

### Pattern 5: Docker Compose Update (DCK-02)

The existing `docker-compose.yml` is largely correct; it just needs the image tag updated. No `ports:` needed with `network_mode: host`.

```yaml
# docker-compose.yml — replaces Node.js version
# network_mode: host is REQUIRED for RTP (decision from v1.0 PROJECT.md)
services:
  audio-dock:
    build: .
    image: audio-dock:latest
    network_mode: host
    env_file:
      - .env
    restart: unless-stopped
```

### Anti-Patterns to Avoid

- **Reading env vars with `os.Getenv` and not checking for empty string:** Silent misconfiguration — the service starts but SIP_USER is "" and registration fails with a cryptic error. Always use `go-simpler/env` with `required` or check explicitly.
- **Initializing zerolog after config.Load():** If config.Load() calls `log.Fatal()` before zerolog is configured, the fatal output won't be JSON. Either: (a) initialize zerolog before config load (zerolog has no required config), or (b) use `fmt.Fprintf(os.Stderr, ...)` in the config package for pre-logger errors.
- **Using `log.Fatal()` from the global zerolog logger without configuring it first:** The package-level `log.Logger` defaults to writing to `os.Stderr` in console format (not JSON). Always create an explicit logger with `zerolog.New(os.Stdout)` rather than relying on the global.
- **Hardcoding `GOARCH=amd64` when building locally on ARM:** The Dockerfile builder stage should use `GOARCH=amd64` only when targeting x86 production hosts. If hosts are ARM64, use `GOARCH=arm64`. For Phase 4, AMD64 is the safe default for Linux production.
- **Using a toolchain line in go.mod:** Go 1.25 changed behavior — the go command no longer adds a `toolchain` line automatically. Don't add it manually unless pinning a specific toolchain is required.
- **Large port ranges in `EXPOSE` on Docker Desktop (macOS):** The v1.0 Dockerfile includes `EXPOSE 10000-10099/udp` with a warning that port ranges >100 stall Docker Desktop's port proxy. Phase 4 has no RTP yet — omit large EXPOSE ranges until Phase 6.

---

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Env var validation with typed fields | `os.LookupEnv` per variable + manual int parsing + per-field error messages | `go-simpler.org/env` | Requires handling missing, wrong type, empty string, and cross-field errors — all boilerplate the library provides |
| Structured JSON log line format | `fmt.Fprintf(os.Stdout, `{"level":"info",...}`)` | `rs/zerolog` | Timestamp formatting, level encoding, field escaping, zero-allocation — wrong in subtle ways when hand-written |
| Static binary Docker image | Copying binary from a non-Alpine builder | Multi-stage with `golang:1.26-alpine` builder | Alpine builder already has `musl` libc and CA certs; scratch stage copy is trivial |
| Per-call logger context | Pass logger + map of fields to every function | `zerolog` child logger via `.With().Str().Logger()` | Child loggers are value types (zero heap allocation); thread-safe; fields auto-included on every event |

**Key insight:** The env var validation library does exactly one thing: remove the boilerplate of `os.LookupEnv` + type assertion + error message per variable. The value is the descriptive error message that names the missing variable — matching CFG-05 exactly. Without it, every required field needs 4+ lines of code and teams inevitably forget one.

---

## Common Pitfalls

### Pitfall 1: Config Package Logging Chicken-and-Egg

**What goes wrong:** `config.Load()` needs to emit an error, but the zerolog logger hasn't been initialized yet. If `config.Load()` uses the zerolog global `log.Fatal()`, the output is in console format (human-readable, not JSON) — violating OBS-01.

**Why it happens:** The logger depends on no config, but config.Load() is typically the first thing called in main.go.

**How to avoid:** Two options:
1. Initialize zerolog first (`logger := zerolog.New(os.Stdout).With().Timestamp().Logger()`), then pass it to `config.Load(logger)` for error reporting.
2. In `config.Load()`, use `fmt.Fprintf(os.Stderr, ...)` with manually-written JSON for the error-only case, then exit. This avoids the zerolog import in the config package.

Option 2 keeps the config package dependency-free (no zerolog import), which is cleaner.

**Warning signs:** Config error messages appear in console format or without `"level"` field.

### Pitfall 2: `RTP_PORT_MIN` and `RTP_PORT_MAX` Integer Validation

**What goes wrong:** `go-simpler/env` parses `int` fields with `strconv.Atoi` but does NOT validate min/max ranges or cross-field constraints. Setting `RTP_PORT_MIN=99999` silently passes. Setting `RTP_PORT_MIN=20000` and `RTP_PORT_MAX=10000` (inverted) also passes.

**Why it happens:** The library provides type parsing, not domain validation.

**How to avoid:** After `env.Load()` succeeds, add explicit validation:
```go
if cfg.RTPPortMin < 1024 || cfg.RTPPortMax > 65535 {
    // fatal error
}
if cfg.RTPPortMin >= cfg.RTPPortMax {
    // fatal error
}
```

**Warning signs:** Service starts but silently fails to bind RTP sockets in Phase 6.

### Pitfall 3: `FROM scratch` Missing CA Certificates Breaks WebSocket TLS

**What goes wrong:** Phase 4 only starts the binary; no TLS calls are made. But in Phase 6, the WebSocket client connects to `WS_URL` (which may be `wss://`). A `FROM scratch` image has no `/etc/ssl/certs/ca-certificates.crt`, causing TLS handshake failures.

**Why it happens:** `scratch` is a truly empty filesystem — no libc, no certs, no timezone data.

**How to avoid:** Copy CA certs from the builder stage (Alpine has them at `/etc/ssl/certs/ca-certificates.crt`). Include this in the Phase 4 Dockerfile so later phases don't need to change the Docker layer.

**Warning signs:** `wss://` WebSocket connections fail with `x509: certificate signed by unknown authority` in Phase 6.

### Pitfall 4: GOARCH Mismatch Between Dev Host and Production

**What goes wrong:** Building on an Apple Silicon Mac without specifying `GOARCH` produces an ARM64 binary. If the production host is AMD64 Linux, the container fails to start with `exec format error`.

**Why it happens:** Docker Desktop on macOS ARM uses QEMU for AMD64 emulation, but `go build` without `GOARCH` produces the host architecture.

**How to avoid:** Always set `GOARCH=amd64` (or `GOARCH=arm64` for ARM hosts) explicitly in the Dockerfile's RUN command. The builder stage runs in Docker (which handles the architecture), so cross-compilation is handled by the Go toolchain.

**Warning signs:** Container starts fine in `docker compose up` on Mac but crashes on Linux CI or production with `exec format error`.

### Pitfall 5: `go mod init` Module Path Matters for Internal Imports

**What goes wrong:** If the module path doesn't match expectations (e.g., `go mod init audio-dock` instead of `github.com/sipgate/audio-dock`), internal import paths diverge from the canonical form and later refactoring is more painful.

**Why it happens:** `go mod init` accepts any path; there's no enforcement beyond consistency.

**How to avoid:** Use the repository's GitHub path (`github.com/sipgate/audio-dock`) as the module path even for private repos. This makes the code portable and consistent with Go conventions.

**Warning signs:** Import paths look like `audio-dock/internal/config` instead of `github.com/sipgate/audio-dock/internal/config`.

### Pitfall 6: zerolog Global Logger vs Explicit Logger

**What goes wrong:** Code in `internal/` packages imports `"github.com/rs/zerolog/log"` and calls `log.Info().Msg(...)` — this uses the package-level global logger which defaults to console format (not JSON) and writes to `os.Stderr`.

**Why it happens:** The zerolog package ships a pre-configured global `log.Logger` for convenience, but its defaults differ from what this project needs (JSON to stdout).

**How to avoid:** Never import `"github.com/rs/zerolog/log"` (the global logger). Always pass an explicit `zerolog.Logger` instance as a parameter or use `zerolog.Ctx(ctx)` for context-carried loggers. The base logger created in `main.go` is the single source.

**Warning signs:** Some log lines are JSON, others are console format; logs appear on stderr instead of stdout.

---

## Code Examples

Verified patterns from official sources:

### Complete main.go Scaffold

```go
// Source: cmd/audio-dock/main.go
// Pattern follows https://go.dev/doc/modules/layout (service layout)
// Zerolog API: https://pkg.go.dev/github.com/rs/zerolog v1.34.0

package main

import (
    "context"
    "os"
    "os/signal"
    "syscall"

    "github.com/rs/zerolog"
    "github.com/sipgate/audio-dock/internal/config"
)

func main() {
    // Config validation first — exits with descriptive error if any required var missing (CFG-05)
    cfg := config.Load()

    // Base logger: JSON to stdout with timestamp (OBS-01)
    logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

    logger.Info().
        Str("sip_user", cfg.SIPUser).
        Str("sip_domain", cfg.SIPDomain).
        Str("ws_url", cfg.WSURL).
        Str("sdp_contact_ip", cfg.SDPContactIP).
        Int("rtp_port_min", cfg.RTPPortMin).
        Int("rtp_port_max", cfg.RTPPortMax).
        Msg("audio-dock starting")

    // Signal handling for graceful shutdown (full implementation in Phase 8 — LCY-01)
    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
    defer stop()

    // Phase 4: scaffold only — later phases add SIP, bridge, HTTP server here
    logger.Info().Msg("scaffold ready — waiting for signal")
    <-ctx.Done()

    logger.Info().Str("signal", ctx.Err().Error()).Msg("shutdown signal received")
}
```

### Config Load with Post-Validation

```go
// Source: internal/config/config.go
// go-simpler/env API: https://pkg.go.dev/go-simpler.org/env v0.12.0

package config

import (
    "fmt"
    "os"

    "go-simpler.org/env"
)

type Config struct {
    SIPUser      string `env:"SIP_USER,required"      usage:"SIP username (e.g. e12345p0)"`
    SIPPassword  string `env:"SIP_PASSWORD,required"  usage:"SIP account password"`
    SIPDomain    string `env:"SIP_DOMAIN,required"    usage:"SIP registrar domain"`
    WSURL        string `env:"WS_URL,required"        usage:"Target WebSocket URL"`
    SDPContactIP string `env:"SDP_CONTACT_IP,required" usage:"External IP for SDP contact line"`
    RTPPortMin   int    `env:"RTP_PORT_MIN"           default:"10000" usage:"Min RTP port"`
    RTPPortMax   int    `env:"RTP_PORT_MAX"           default:"10099" usage:"Max RTP port"`
}

func Load() Config {
    var cfg Config
    if err := env.Load(&cfg, nil); err != nil {
        // env error format: "env: VAR_NAME is required but not set"
        // Print as minimal JSON to stderr so it's parseable even before zerolog init
        fmt.Fprintf(os.Stderr,
            `{"level":"fatal","msg":"configuration error","error":%q}`+"\n",
            err.Error())
        os.Exit(1)
    }

    // Cross-field validation not supported by go-simpler/env — do it manually
    if cfg.RTPPortMin >= cfg.RTPPortMax {
        fmt.Fprintf(os.Stderr,
            `{"level":"fatal","msg":"RTP_PORT_MIN must be less than RTP_PORT_MAX","RTP_PORT_MIN":%d,"RTP_PORT_MAX":%d}`+"\n",
            cfg.RTPPortMin, cfg.RTPPortMax)
        os.Exit(1)
    }

    return cfg
}
```

### Zerolog Child Logger Pattern (for Phase 6+ reference)

```go
// Source: https://pkg.go.dev/github.com/rs/zerolog v1.34.0
// Per-call child logger — carries callId and streamSid on every line

// In CallSession (Phase 6):
func newCallSession(baseLogger zerolog.Logger, callID, streamSid string) *CallSession {
    log := baseLogger.With().
        Str("callId", callID).
        Str("streamSid", streamSid).
        Logger()
    return &CallSession{log: log}
}

// Usage inside session (no field repetition needed):
s.log.Info().Str("rtp_port", strconv.Itoa(s.rtpPort)).Msg("call started")
// Output: {"level":"info","time":"...","callId":"abc123","streamSid":"MX...","rtp_port":"10000","message":"call started"}
```

---

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| `kelseyhightower/envconfig` (dominant 2018–2022) | `go-simpler.org/env` (simpler API, struct tags with `required`/`default`) | 2022+ | envconfig still maintained but API is older; go-simpler/env mirrors modern Zod-like patterns |
| `go.uber.org/zap` as standard structured logger | Both zap and zerolog widely adopted; zerolog preferred for high-frequency hot paths | 2022+ | zerolog's zero-allocation design better suited for audio/RTP goroutines |
| `FROM alpine` for "minimal" Go images | `FROM scratch` or `gcr.io/distroless/static` | 2020+ | Scratch images 8–15 MB vs Alpine 20+ MB; no shell attack surface |
| `golang:1.N` builder image | `golang:1.N-alpine` builder | 2019+ | Alpine builder ~300 MB vs ~800 MB for debian-based; smaller CI cache |
| Toolchain line in go.mod | No toolchain line (Go 1.25 changed behavior) | Go 1.25 (Aug 2025) | Go command no longer auto-adds toolchain line on update |
| `go mod init` defaults to current go version | `go mod init` defaults to `go 1.(N-1)` (Go 1.26 behavior) | Go 1.26 (Feb 2026) | New modules declare `go 1.25` when created with Go 1.26 toolchain |

**Deprecated/outdated:**
- `github.com/kelseyhightower/envconfig`: No active development since 2021; no struct-tag `required`; still functional but not the current recommendation.
- `golang:latest` as builder: Too large; always pin a specific version.
- Multi-platform `EXPOSE` with large port ranges in Docker Compose on non-Linux hosts: Known to stall Docker Desktop port proxy; use `network_mode: host` on Linux production instead.

---

## Open Questions

1. **Module path: `github.com/sipgate/audio-dock` vs a private/internal path**
   - What we know: The repo is at `sipgate/audio-dock` on GitHub. The module path should be the canonical GitHub URL.
   - What's unclear: Whether the repo is public or private; whether the module will ever be imported externally.
   - Recommendation: Use `github.com/sipgate/audio-dock` — it's a self-contained service (all in `internal/`), so the public/private distinction is irrelevant for import purposes. Consistent with Go conventions.

2. **`SDP_CONTACT_IP`: required vs optional with auto-detection fallback**
   - What we know: v1.0 marks `SDP_CONTACT_IP` as optional (`z.ipv4().optional()`) — the service worked without it (likely defaulted or the caller's IP was used). REQUIREMENTS.md (CFG-04) says it's configured via env var but doesn't say required.
   - What's unclear: Whether the Phase 4 scaffold should treat it as required or optional-with-fallback (e.g., auto-detect via `net.InterfaceAddrs()` or external IP service).
   - Recommendation: Treat as required for Phase 4 (simplest, safest, matches CFG-04 wording). Auto-detection can be added in Phase 5 if needed.

3. **`WS_URL` env var name: v1.0 uses `WS_TARGET_URL`, additional context says `WS_URL`**
   - What we know: The v1.0 `config/index.ts` schema uses `WS_TARGET_URL`. The additional context in this phase description says the env var name is `WS_URL`. These conflict.
   - What's unclear: Which name is canonical for drop-in compatibility.
   - Recommendation: Use `WS_URL` as specified in the phase additional context (it's shorter and the Go rewrite is the new canonical). If a `.env` file from v1.0 uses `WS_TARGET_URL`, add a note in the config about the rename.

4. **Does `docker-compose.yml` need to be replaced or modified in-place?**
   - What we know: The existing `docker-compose.yml` already has `network_mode: host` and `env_file: .env`. Only the `build` context changes (same `Dockerfile`, different content).
   - What's unclear: Whether to keep the v1.0 compose as backup or replace in-place.
   - Recommendation: Replace in-place — the v1.0 service is abandoned in favor of v2.0. Keep git history as the backup.

---

## Sources

### Primary (HIGH confidence)
- `rs/zerolog` pkg.go.dev — https://pkg.go.dev/github.com/rs/zerolog — v1.34.0 API verified (logger init, child loggers, ParseLevel, Ctx)
- `go-simpler.org/env` pkg.go.dev — https://pkg.go.dev/go-simpler.org/env — v0.12.0 API verified (required tag, default tag, NotSetError, int parsing)
- Go 1.26 Release Notes — https://go.dev/doc/go1.26 — `go mod init` default version behavior, toolchain line change
- Go Modules Layout — https://go.dev/doc/modules/layout — canonical cmd/ + internal/ structure for services
- `dasroot.net` Go container guide (Feb 2026) — https://dasroot.net/posts/2026/02/building-minimal-go-containers-high-throughput-apis/ — FROM scratch Dockerfile pattern, build flags
- v1.0 `src/config/index.ts` — `/Users/rotmanov/git/sipgate/audio-dock/src/config/index.ts` — reference for env var names and validation model

### Secondary (MEDIUM confidence)
- `github.com/rs/zerolog` GitHub — https://github.com/rs/zerolog — v1.34.0 release confirmed; README examples for child loggers
- BetterStack zerolog guide — https://betterstack.com/community/guides/logging/zerolog/ — per-request child logger pattern (verified against pkg.go.dev)
- golang-standards/project-layout — https://github.com/golang-standards/project-layout — cmd/ and internal/ directory conventions (note: this is community, not official; verified against official go.dev/doc/modules/layout)

### Tertiary (LOW confidence — needs validation)
- go-simpler/env latest version: pkg.go.dev shows v0.12.0 as the documented version; GitHub releases page confirms same. No newer version found as of research date. LOW confidence only because the module path `go-simpler.org/env` is not `github.com/go-simpler/env` — verify with `go get go-simpler.org/env@latest` at implementation time.

---

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH — zerolog v1.34.0 verified on pkg.go.dev (Mar 2025); go-simpler/env v0.12.0 verified; zerolog is locked decision (STATE.md)
- Architecture: HIGH — Go module layout verified against official go.dev/doc/modules/layout; Docker pattern verified against recent 2026 source
- Pitfalls: HIGH — chicken-and-egg logger issue is a known Go pattern problem; FROM scratch CA cert issue is documented; GOARCH mismatch is standard cross-compile issue; all verified

**Research date:** 2026-03-03
**Valid until:** 2026-09-03 (zerolog and go-simpler/env are stable; Go 1.26 is current — recheck if Go 1.27 releases before implementation)
