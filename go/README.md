# sipgate-sip-stream-bridge — Go implementation (v2.0)

A Go service that bridges inbound sipgate SIP calls to a WebSocket backend using the [Twilio Media Streams](https://www.twilio.com/docs/voice/media-streams) protocol. Drop-in compatible with any Twilio Media Streams consumer — AI voice bots, call recording, real-time transcription.

```
sipgate SIP trunk  ←──────────────────────────────────────────────────────→  caller
        │                                                                          │
        │ SIP (UDP/TCP :5060)                                             RTP/UDP  │
        ▼                                                                          ▼
  ┌──────────────────────────────────────────────────────────────────────────────┐
  │                             sipgate-sip-stream-bridge (Go)                                   │
  │                                                                               │
  │   SIP Handler ──► CallManager ──► CallSession                                 │
  │                                       │                                       │
  │                          rtpReader ◄──┤──► wsPacer  (RTP → WS, paced 20 ms)  │
  │                          rtpPacer  ───┤                                       │
  │                          wsToRTP   ◄──┘──► reconnect loop (1s/2s/4s backoff)  │
  └──────────────────────────────────────────────────────────────────────────────┘
                                        │
                              WebSocket (ws/wss)
                                        │
                                        ▼
                              AI voice-bot backend
                          (Twilio Media Streams protocol)
```

## Features

- Registers with sipgate SIP trunking and stays registered (auto re-registration at 75% of server-granted Expires)
- Accepts inbound SIP INVITEs, negotiates PCMU (G.711 μ-law 8 kHz)
- Opens one WebSocket connection per call to the configured backend URL
- Bridges audio bidirectionally: RTP ↔ base64 mulaw `media` events
- Forwards DTMF digits as `dtmf` events (RFC 4733, End-bit deduplication by RTP timestamp)
- Forwards `mark` events from the backend to the caller — echoes the mark name after all preceding audio frames have been sent; immediate echo when the outbound queue is idle
- Handles `clear` events from the backend — discards buffered outbound audio and echoes all pending mark names immediately
- SIP OPTIONS keepalive: probes the registrar every `SIP_OPTIONS_INTERVAL` seconds; triggers re-registration after 2 consecutive failures (401/407 responses count as success — server is reachable)
- Survives transient WebSocket drops: exponential backoff reconnect (1 s → 2 s → 4 s, 30 s budget); call stays up, inbound RTP is dropped (not buffered) during reconnect
- After reconnect: backend receives fresh `connected` + `start` before any `media` events
- Inbound RTP playout buffer smooths sipgate sender bursts (50-frame queue, paced output at 20 ms)
- Supports multiple concurrent calls, each fully isolated
- Structured JSON logs (zerolog) with call context on every line
- Static binary Docker image (`FROM scratch`, ~10 MB)
- Graceful shutdown on SIGTERM/SIGINT: rejects new INVITEs, sends SIP BYE to all active calls, unregisters, then exits
- `GET /health` — live registration and call count
- `GET /metrics` — Prometheus exposition (active calls, SIP status, RTP/WS counters)

---

## Configuration

All configuration is via environment variables. Copy `../.env.example` to `../.env` and edit.

### Required

| Variable | Description | Example |
|----------|-------------|---------|
| `SIP_USER` | SIP-ID from sipgate portal (Connections › SIP Trunks) | `e12345p0` |
| `SIP_PASSWORD` | SIP password for the SIP-ID above | `s3cr3t` |
| `SIP_DOMAIN` | SIP domain — used in the `From`/`To` URI | `sipconnect.sipgate.de` |
| `SIP_REGISTRAR` | Hostname of the SIP registrar | `sipconnect.sipgate.de` |
| `SDP_CONTACT_IP` | Reachable IP for SDP contact (where sipgate delivers INVITEs) | `1.2.3.4` |
| `WS_TARGET_URL` | WebSocket URL of the Twilio Media Streams consumer | `ws://localhost:8080` |

### Optional

| Variable | Default | Description |
|----------|---------|-------------|
| `RTP_PORT_MIN` | `10000` | Start of UDP port range for RTP media sockets |
| `RTP_PORT_MAX` | `10099` | End of UDP port range (100 ports ≈ 50 concurrent calls) |
| `SIP_EXPIRES` | `120` | Requested registration expiry in seconds (re-registers at 75%) |
| `SIP_OPTIONS_INTERVAL` | `30` | Interval in seconds between SIP OPTIONS keepalive pings; 2 consecutive failures trigger re-registration |
| `LOG_LEVEL` | `info` | zerolog level: `trace` `debug` `info` `warn` `error` |
| `HTTP_PORT` | `8080` | Port for `/health` and `/metrics` HTTP endpoints |

The service validates all variables at startup and exits immediately with a structured error if anything is missing:

```json
{"level":"error","message":"missing required env var: SIP_USER"}
```

---

## Running Locally

**Prerequisites:** Go 1.25+

```bash
cd go
cp ../.env.example ../.env   # fill in SIP credentials
go run ./cmd/sipgate-sip-stream-bridge       # run directly
go build -o sipgate-sip-stream-bridge ./cmd/sipgate-sip-stream-bridge && ./sipgate-sip-stream-bridge   # or build first
```

You should see:
```json
{"level":"info","sip_user":"e12345p0","message":"sipgate-sip-stream-bridge starting"}
{"level":"info","registrar":"sipconnect.sipgate.de","server_expires_s":120,"message":"SIP registration successful"}
{"level":"info","message":"SIP registration active — ready to accept inbound calls"}
```

### Quick integration test

`test-listener.js` (in the repo root) is a minimal Twilio Media Streams server:

```bash
# Terminal 1 — listener on port 8080
node ../test-listener.js

# Terminal 2 — sipgate-sip-stream-bridge
go run ./cmd/sipgate-sip-stream-bridge
```

Then call your sipgate number. The listener logs `connected`, `start`, and `media` events.

### Tests

```bash
go test ./...            # all tests
go test ./... -race      # with race detector
```

---

## Docker

### Build

```bash
cd go
docker build -t sipgate-sip-stream-bridge:latest .
```

The Dockerfile uses a two-stage build:
1. **builder** — `golang:1.26-alpine`; produces a statically-linked binary (`CGO_ENABLED=0`)
2. **runtime** — `FROM scratch`; only the binary and CA certificates (~10 MB total)

### Run with Docker Compose

```bash
cp ../.env.example ../.env   # fill in credentials
docker compose up -d
docker compose logs -f sipgate-sip-stream-bridge
docker compose down
```

The `docker-compose.yml` uses `network_mode: host`, which is required on Linux for RTP — Docker's UDP port proxy introduces jitter.

**macOS / Windows Docker Desktop:** `network_mode: host` is silently ignored. Create `docker-compose.override.yml` in `go/`:

```yaml
services:
  sipgate-sip-stream-bridge:
    network_mode: bridge
    ports:
      - "5060:5060/udp"
      - "5060:5060/tcp"
      - "10000-10099:10000-10099/udp"
```

> **Note:** Limit the UDP port range to ≤100 ports — larger ranges stall Docker Desktop's port proxy on startup.

---

## HTTP Endpoints

Both endpoints are served on `HTTP_PORT` (default `8080`).

### `GET /health`

```json
{"registered": true, "activeCalls": 2}
```

Always returns HTTP 200.

### `GET /metrics`

Prometheus exposition format. Metrics:

| Metric | Type | Description |
|--------|------|-------------|
| `active_calls_total` | Gauge | Currently active calls |
| `sip_registration_status` | Gauge | 1 = registered, 0 = unregistered |
| `rtp_packets_received_total` | Counter | RTP packets received from callers |
| `rtp_packets_sent_total` | Counter | RTP packets sent to callers |
| `ws_reconnect_attempts_total` | Counter | WebSocket reconnect attempts |

---

## WebSocket Protocol

sipgate-sip-stream-bridge implements the [Twilio Media Streams WebSocket protocol](https://www.twilio.com/docs/voice/media-streams/websocket-messages). One WebSocket connection is opened per call to `WS_TARGET_URL`.

### sipgate-sip-stream-bridge → backend

#### `connected`
```json
{"event": "connected", "protocol": "Call", "version": "1.0.0"}
```

#### `start`
```json
{
  "event": "start",
  "sequenceNumber": "1",
  "start": {
    "streamSid": "MZxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
    "callSid": "CAxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
    "tracks": ["inbound", "outbound"],
    "customParameters": {
      "From": "sip:+4915123456789@sipconnect.sipgate.de",
      "To": "sip:e12345p0@sipconnect.sipgate.de",
      "sipCallId": "abc123@192.168.1.1"
    },
    "mediaFormat": {"encoding": "audio/x-mulaw", "sampleRate": 8000, "channels": 1}
  },
  "streamSid": "MZxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
}
```

#### `media` (inbound audio, 20 ms / 160 bytes)
```json
{
  "event": "media",
  "sequenceNumber": "2",
  "media": {"track": "inbound", "chunk": "0", "timestamp": "0", "payload": "<base64 mulaw>"},
  "streamSid": "MZxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
}
```

#### `dtmf`
```json
{
  "event": "dtmf",
  "sequenceNumber": "42",
  "dtmf": {"track": "inbound_track", "digit": "5"},
  "streamSid": "MZxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
}
```

#### `mark` (echo)
```json
{
  "event": "mark",
  "sequenceNumber": "99",
  "mark": {"name": "my-label"},
  "streamSid": "MZxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
}
```

Sent after all preceding outbound audio frames have been delivered to the caller, confirming the audio up to this point has played out.

#### `stop`
```json
{
  "event": "stop",
  "sequenceNumber": "1000",
  "stop": {"callSid": "CAxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"},
  "streamSid": "MZxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
}
```

### backend → sipgate-sip-stream-bridge

#### `media` (outbound audio to caller)
```json
{
  "event": "media",
  "streamSid": "MZxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
  "media": {"payload": "<base64 mulaw 160 bytes>"}
}
```

#### `mark`
```json
{
  "event": "mark",
  "streamSid": "MZxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
  "mark": {"name": "my-label"}
}
```

Requests a mark echo. sipgate-sip-stream-bridge places a mark sentinel in the outbound audio queue. When all preceding audio frames have been sent to the caller, sipgate-sip-stream-bridge sends a `mark` event back to the backend with the same name. If the queue is idle at the moment the mark arrives, the echo is sent immediately (fast-path, no enqueue).

#### `clear`
```json
{
  "event": "clear",
  "streamSid": "MZxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
}
```

Instructs sipgate-sip-stream-bridge to discard all buffered outbound audio immediately. Any pending mark sentinels in the queue are echoed back to the backend before the queue is flushed. The outbound RTP pacer continues running — silence is sent to the caller during the gap.

### WebSocket reconnect behaviour

If the backend disconnects during an active call:

1. The SIP call **stays up** — no BYE is sent.
2. Inbound RTP from the caller is **dropped** (not buffered) during reconnect.
3. Reconnect is retried with exponential backoff: **1 s → 2 s → 4 s** (capped), up to a **30-second budget**.
4. On successful reconnect, the backend receives a fresh `connected` then `start` before any `media`.
5. If the budget is exhausted, sipgate-sip-stream-bridge sends SIP BYE and terminates the call.

---

## Architecture

### Components

| Component | File | Responsibility |
|-----------|------|----------------|
| Entrypoint | `cmd/sipgate-sip-stream-bridge/main.go` | Wires all components; signal handling; graceful shutdown sequence |
| Config | `internal/config/config.go` | go-simpler/env validated config; exits on missing vars |
| SIP Agent | `internal/sip/agent.go` | sipgo UA/Server/Client setup |
| SIP Handler | `internal/sip/handler.go` | INVITE/ACK/BYE/OPTIONS dispatch; shutdown guard (503 during drain) |
| Registrar | `internal/sip/registrar.go` | REGISTER + Digest Auth + re-register loop at 75% Expires |
| SDP | `internal/sip/sdp.go` | Parse SDP offer; build SDP answer (PCMU + telephone-event) |
| CallManager | `internal/bridge/manager.go` | sync.Map of active sessions; port pool; DrainAll on shutdown |
| CallSession | `internal/bridge/session.go` | 4-goroutine per-call bridge: rtpReader, rtpPacer, wsPacer, wsToRTP |
| WS helpers | `internal/bridge/ws.go` | sendConnected/Start/Media/Stop/DTMF; wsSignal; dialWS |
| Observability | `internal/observability/metrics.go` | Custom prometheus.Registry; 5 metrics |

### Shutdown sequence (SIGTERM/SIGINT)

```
1. handler.SetShutdown()    — new INVITEs receive 503
2. callManager.DrainAll()   — BYE all active sessions (8 s deadline)
3. registrar.Unregister()   — REGISTER Expires:0 (5 s deadline)
4. httpServer.Shutdown()    — drain in-flight HTTP requests (2 s)
5. agent.UA.Close()         — close SIP transport (deferred)
```

---

## File Layout

```
go/
├── cmd/sipgate-sip-stream-bridge/
│   └── main.go                    # Entrypoint
├── internal/
│   ├── config/
│   │   └── config.go              # Environment variable config
│   ├── sip/
│   │   ├── agent.go               # sipgo UA/Server/Client
│   │   ├── handler.go             # SIP INVITE/BYE/OPTIONS handler
│   │   ├── registrar.go           # SIP REGISTER + re-register loop
│   │   └── sdp.go                 # SDP offer/answer
│   ├── bridge/
│   │   ├── manager.go             # CallManager + PortPool
│   │   ├── session.go             # Per-call 4-goroutine bridge
│   │   └── ws.go                  # WebSocket helpers + wsSignal
│   └── observability/
│       └── metrics.go             # Prometheus metrics
├── Dockerfile                     # Two-stage: golang:1.26-alpine → FROM scratch
├── docker-compose.yml             # Linux production (network_mode: host)
└── go.mod
```
