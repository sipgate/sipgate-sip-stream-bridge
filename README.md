# sipgate-sip-stream-bridge

A service that bridges inbound sipgate SIP calls to a WebSocket backend using the [Twilio Media Streams](https://www.twilio.com/docs/voice/media-streams) protocol. Drop-in compatible with any Twilio Media Streams consumer — AI voice bots, call recording, real-time transcription.

```
sipgate SIP trunk  ←──────────────────────────────────────────────────────→  caller
        │                                                                          │
        │ SIP (UDP :5060)                                                 RTP/UDP  │
        ▼                                                                          ▼
  ┌──────────────────────────────────────────────────────────────────────────────┐
  │                             sipgate-sip-stream-bridge                                        │
  │                                                                               │
  │              SIP Handler ──► CallManager ──► per-call RTP socket             │
  │                                                      │                        │
  │                                               per-call WebSocket              │
  └──────────────────────────────────────────────────────────────────────────────┘
                                        │
                              WebSocket (ws/wss)
                                        │
                                        ▼
                              AI voice-bot backend
                          (Twilio Media Streams protocol)
```

## Implementations

Two implementations are available in this repository:

| | [Go (v2.0)](./go/) | [Node.js (v1.0)](./node/) |
|---|---|---|
| **Language** | Go 1.25 | TypeScript / Node.js 22 |
| **Status** | ✅ Current | 📦 Reference |
| **Docker image** | ~10 MB (`FROM scratch`) | ~120 MB (`node:22-alpine`) |
| **SIP library** | emiago/sipgo | Raw UDP (no library) |
| **Logging** | zerolog (JSON) | pino (JSON) |
| **Config validation** | go-simpler/env | Zod |
| **Playout buffer** | ✅ 50-frame queue, paced 20 ms | ✗ |
| **WS reconnect** | ✅ 1s/2s/4s, 30s budget | ✅ 1s/2s/4s, 30s budget |
| **DTMF forwarding** | ✅ RFC 4733 End-bit dedup | ✅ RFC 4733 End-bit dedup |
| **Mark/clear protocol** | ✅ echo after drain, fast-path | ✅ echo after drain, fast-path |
| **SIP OPTIONS keepalive** | ✅ re-register after 2 failures | ✅ re-register after 2 failures |
| **Graceful shutdown** | ✅ DrainAll + UNREGISTER | ✅ BYE + UNREGISTER |
| **`GET /health`** | ✅ port 9090 | ✅ port 9090 |
| **`GET /metrics`** | ✅ Prometheus | ✅ Prometheus |

→ **[Go implementation README](./go/README.md)**

→ **[Node.js implementation README](./node/README.md)**

---

## Quick Start

### Go (recommended)

```bash
cp .env.example .env   # fill in SIP credentials
cd go
go run ./cmd/sipgate-sip-stream-bridge
```

### Node.js

```bash
cp .env.example .env
cd node
pnpm install
pnpm dev
```

---

## Configuration

Both implementations share the same environment variables. Copy `.env.example` to `.env`:

### Required

| Variable | Description | Example |
|----------|-------------|---------|
| `SIP_USER` | SIP-ID from sipgate portal | `e12345p0` |
| `SIP_PASSWORD` | SIP password | `s3cr3t` |
| `SIP_DOMAIN` | SIP domain for `From`/`To` URI | `sipconnect.sipgate.de` |
| `SIP_REGISTRAR` | SIP registrar hostname | `sipconnect.sipgate.de` |
| `WS_TARGET_URL` | WebSocket URL of the backend | `ws://localhost:8080` |

`SDP_CONTACT_IP` is additionally required in the Go implementation (reachable IP for SDP contact).

See the individual READMEs for the full option list.

---

## Integration Testing

[`test-listener/`](./test-listener/) is a minimal Twilio Media Streams WebSocket server for local testing. It has its own `package.json` with the `ws` dependency.

```bash
# Terminal 1 — install once, then start listener on port 8080
cd test-listener && npm install
node index.js             # MODE=log (default) — log all events
MODE=echo node index.js   # echo caller audio back
MODE=tone node index.js   # send a synthetic tone (simulates TTS)
MODE=timing node index.js # print inter-arrival time per packet (jitter check)

# Terminal 2 — start sipgate-sip-stream-bridge (Go example)
cd go && go run ./cmd/sipgate-sip-stream-bridge
```

Then call your sipgate number. The listener logs `connected`, `start`, `media`, and `stop` events.

Set `WS_TARGET_URL=ws://localhost:8080` in `.env` (the listener's default port).

---

## Repository Layout

```
sipgate-sip-stream-bridge/
├── go/               # Go v2.0 implementation
│   ├── cmd/          # Entrypoint
│   ├── internal/     # SIP, bridge, observability packages
│   ├── Dockerfile
│   └── docker-compose.yml
├── node/             # Node.js v1.0 implementation
│   ├── src/          # TypeScript source
│   ├── test/         # FD leak test
│   └── package.json
├── test-listener/    # Local Twilio Media Streams listener (own package.json)
├── .env.example      # Shared environment variable template
└── .planning/        # GSD project planning artifacts
```
