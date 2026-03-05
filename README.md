# audio-dock

A Node.js/TypeScript service that bridges inbound sipgate SIP calls to a WebSocket backend using the [Twilio Media Streams](https://www.twilio.com/docs/voice/media-streams) protocol. Drop-in compatible with any Twilio Media Streams consumer — AI voice bots, call recording, real-time transcription.

```
sipgate SIP trunk  ←──────────────────────────────────────────────────────→  caller
        │                                                                          │
        │ SIP (UDP :5060)                                                 RTP/UDP  │
        ▼                                                                          ▼
  ┌──────────────────────────────────────────────────────────────────────────────┐
  │                             audio-dock                                        │
  │                                                                               │
  │   SipUserAgent ──► CallManager ──► RtpHandler  (per-call UDP media socket)   │
  │                         │                                                     │
  │                         └──────► WsClient      (per-call WebSocket)          │
  └──────────────────────────────────────────────────────────────────────────────┘
                                        │
                              WebSocket (ws/wss)
                                        │
                                        ▼
                              AI voice-bot backend
                          (Twilio Media Streams protocol)
```

## Features

- Registers with sipgate SIP trunking and stays registered (auto re-registration)
- Accepts inbound SIP INVITEs, negotiates PCMU (G.711 μ-law 8 kHz)
- Opens one WebSocket connection per call to the configured backend URL
- Bridges audio bidirectionally: RTP ↔ base64 mulaw `media` events
- Forwards DTMF digits as `dtmf` events
- Survives transient WebSocket drops: exponential backoff reconnect (1 s → 2 s → 4 s, 30 s budget) with μ-law silence to the caller during the reconnect window
- Supports multiple concurrent calls, each fully isolated
- Structured JSON logs with `callId` and `streamSid` context on every line
- Docker image with multi-stage build on `node:22-alpine`
- Graceful shutdown on SIGTERM/SIGINT: sends SIP BYE to all active calls, unregisters, then exits

---

## Table of Contents

1. [Quick Start](#quick-start)
2. [Configuration](#configuration)
3. [Running Locally](#running-locally)
4. [Docker](#docker)
5. [WebSocket Protocol](#websocket-protocol)
6. [Architecture](#architecture)
7. [Testing](#testing)
8. [File Layout](#file-layout)

---

## Quick Start

```bash
git clone <repo-url>
cd audio-dock
cp .env.example .env
# Fill in SIP credentials (see Configuration below)
pnpm install
pnpm dev
```

You should see:
```json
{"level":30,"service":"audio-dock","event":"startup","sipUser":"e12345p0","sipDomain":"sipconnect.sipgate.de"}
{"level":30,"service":"audio-dock","event":"sip_registered","expires":120}
```

---

## Configuration

All configuration is via environment variables. Copy `.env.example` to `.env` and edit.

### Required

| Variable | Description | Example |
|----------|-------------|---------|
| `SIP_USER` | SIP-ID from sipgate portal (Connections › SIP Trunks) | `e12345p0` |
| `SIP_PASSWORD` | SIP password for the SIP-ID above | `s3cr3t` |
| `SIP_DOMAIN` | SIP domain — used in the `From`/`To` URI | `sipconnect.sipgate.de` |
| `SIP_REGISTRAR` | Hostname of the SIP registrar | `sipconnect.sipgate.de` |
| `WS_TARGET_URL` | WebSocket URL of the Twilio Media Streams consumer | `ws://localhost:8080` |

### Optional

| Variable | Default | Description |
|----------|---------|-------------|
| `SDP_CONTACT_IP` | *(auto-detect)* | External IP included in SDP contact and Via headers. **Set this when running behind NAT** — find your public IP with `curl -s ifconfig.me`. |
| `RTP_PORT_MIN` | `10000` | Start of UDP port range for RTP media sockets. |
| `RTP_PORT_MAX` | `10099` | End of UDP port range. A 100-port range supports ~50 concurrent calls. |
| `SIP_EXPIRES` | `120` | SIP registration expiry in seconds. The service re-registers at 90% of this value. |
| `LOG_LEVEL` | `info` | Pino log level: `trace` `debug` `info` `warn` `error` |

The service validates all variables on startup using Zod and exits immediately with a structured error if anything is missing or invalid:

```json
{"level":"error","errors":{"SIP_USER":["Required"]}}
```

---

## Running Locally

**Prerequisites:** Node.js ≥ 22, pnpm 10+

```bash
pnpm install        # install dependencies
pnpm dev            # TypeScript dev server with auto-reload
pnpm build          # compile to dist/
pnpm start          # run compiled build
pnpm typecheck      # type-check without emitting
```

### Quick integration test without a real AI backend

`test-listener.js` is a minimal Twilio Media Streams server that runs locally:

```bash
# Terminal 1 — start the listener on port 8080
MODE=log node test-listener.js          # log all events (default)
MODE=echo node test-listener.js         # echo caller audio back
node test-listener.js 3000              # custom port

# Terminal 2 — start audio-dock pointed at the listener
WS_TARGET_URL=ws://localhost:8080 pnpm dev
```

Then call your sipgate number. You should see `connected`, `start`, and `media` events logged by the listener.

---

## Docker

### Build

```bash
docker build -t audio-dock:latest .
```

The Dockerfile uses a four-stage build:
1. **base** — `node:22-alpine` with corepack
2. **fetcher** — downloads packages into the pnpm store (cached layer; only re-runs when `pnpm-lock.yaml` changes)
3. **builder** — offline install + TypeScript compilation
4. **production** — minimal runtime image with `dist/` and `node_modules` only

### Run with Docker Compose

```bash
cp .env.example .env   # fill in credentials
docker compose up -d
docker compose logs -f audio-dock
docker compose down
```

The included `docker-compose.yml` uses `network_mode: host`, which is required on Linux for RTP to work correctly — Docker's UDP port proxy introduces jitter and prevents the service from binding the expected ephemeral ports.

### macOS / Windows Docker Desktop

`network_mode: host` is ignored on Docker Desktop. Create `docker-compose.override.yml`:

```yaml
services:
  audio-dock:
    network_mode: bridge
    ports:
      - "10000-10099:10000-10099/udp"
```

> **Note:** Bridged networking adds ~10 ms UDP latency and may cause RTP issues with some SIP clients. Use a Linux host for production.

---

## WebSocket Protocol

audio-dock implements the [Twilio Media Streams WebSocket protocol](https://www.twilio.com/docs/voice/media-streams/websocket-messages). One WebSocket connection is opened per call to `WS_TARGET_URL`.

### audio-dock → backend (inbound events)

#### `connected` — sent immediately after WebSocket opens

```json
{
  "event": "connected",
  "protocol": "Call",
  "version": "1.0.0"
}
```

#### `start` — sent after `connected`, before any audio

```json
{
  "event": "start",
  "sequenceNumber": "1",
  "start": {
    "streamSid": "MZxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
    "accountSid": "",
    "callSid": "CAxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
    "tracks": ["inbound", "outbound"],
    "customParameters": {
      "From": "sip:+4915123456789@sipconnect.sipgate.de",
      "To": "sip:e12345p0@sipconnect.sipgate.de",
      "sipCallId": "abc123@192.168.1.1"
    },
    "mediaFormat": {
      "encoding": "audio/x-mulaw",
      "sampleRate": 8000,
      "channels": 1
    }
  },
  "streamSid": "MZxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
}
```

#### `media` — one message per RTP packet (20 ms, 160 bytes)

```json
{
  "event": "media",
  "sequenceNumber": "2",
  "media": {
    "track": "inbound",
    "chunk": "0",
    "timestamp": "0",
    "payload": "<base64-encoded mulaw 160 bytes>"
  },
  "streamSid": "MZxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
}
```

#### `dtmf` — DTMF digit pressed by caller

```json
{
  "event": "dtmf",
  "sequenceNumber": "42",
  "dtmf": {
    "track": "inbound_track",
    "digit": "5"
  },
  "streamSid": "MZxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
}
```

Digits: `0`–`9`, `*`, `#`, `A`–`D`.

#### `stop` — call ended

```json
{
  "event": "stop",
  "sequenceNumber": "1000",
  "stop": {
    "accountSid": "",
    "callSid": "CAxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
  },
  "streamSid": "MZxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
}
```

---

### backend → audio-dock (outbound events)

#### `media` — send audio to caller

```json
{
  "event": "media",
  "streamSid": "MZxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
  "media": {
    "payload": "<base64-encoded mulaw 160 bytes>"
  }
}
```

The payload is decoded and sent as RTP to the caller. Format must be G.711 μ-law, 8 kHz mono, 20 ms packets (160 bytes).

---

### WebSocket reconnect behaviour

If the WebSocket backend disconnects during an active call:

1. The SIP call **stays up** — audio-dock does not send SIP BYE.
2. μ-law silence (160 × `0xFF`) is sent to the caller every 20 ms to prevent dead air.
3. Inbound RTP from the caller is **dropped** (not buffered) during the reconnect window.
4. Reconnect is retried with exponential backoff: **1 s → 2 s → 4 s** (capped), up to a **30-second budget**.
5. On successful reconnect, the backend receives a fresh `connected` then `start` event (same `streamSid`/`callSid`) before any `media` events.
6. If the budget is exhausted, audio-dock sends SIP BYE and terminates the call.

---

## Architecture

### Component overview

| Component | File | Responsibility |
|-----------|------|----------------|
| Entrypoint | `src/index.ts` | Wires all components; handles SIGTERM/SIGINT graceful shutdown |
| Config | `src/config/index.ts` | Zod-validated environment variables; fails fast on startup |
| Logger | `src/logger/index.ts` | Pino JSON logger; `createChildLogger` adds `callId`/`streamSid` context |
| SIP UserAgent | `src/sip/userAgent.ts` | UDP socket on `:5060`; REGISTER with digest auth (MD5); dispatches INVITE/ACK/BYE/CANCEL |
| SDP | `src/sip/sdp.ts` | `parseSdpOffer` extracts remote IP/port/codec; `buildSdpAnswer` advertises PCMU + telephone-event |
| RTP Handler | `src/rtp/rtpHandler.ts` | Per-call UDP socket; strips 12-byte RTP headers; emits `audio` and `dtmf` events; prepends headers on send |
| WebSocket Client | `src/ws/wsClient.ts` | Per-call WebSocket; implements Twilio Media Streams event encoding/decoding |
| Call Manager | `src/bridge/callManager.ts` | Orchestrates the full call lifecycle; holds `Map<callId, CallSession>` |

### Call lifecycle

```
INVITE ──► 100 Trying
       ──► parse SDP offer     (reject 488 if no PCMU)
       ──► allocate RTP port   (bind UDP socket, hole-punch)
       ──► 180 Ringing
       ──► connect WebSocket   (2 s timeout → 503 if fails)
       ──► send 200 OK + SDP answer
       ──► retransmit 200 OK   (500 ms → 1 s → 2 s → 4 s until ACK)
       ──► wire audio bridge
           RTP audio  →  base64 mulaw  →  WS media event  →  backend
           WS media   →  base64 decode →  RTP packet      →  caller
           RTP DTMF   →  WS dtmf event →  backend

BYE  ──► 200 OK
     ──► ws.stop() (sends stop event)
     ──► rtp.dispose() (closes UDP socket)
```

### Implementation notes

- **No SIP library** — SIP signalling is raw UDP with regex header parsing. This avoids native module dependencies and keeps the image small.
- **NAT traversal** — One silence packet is sent on RTP initialisation to punch a hole in the router for inbound packets.
- **RFC 3261 §13.3.1.4** — 200 OK retransmit loop with exponential backoff until ACK is received.
- **DTMF** — RFC 4733 telephone-event (payload type 113, 8 kHz); emitted on the `End=1` flag to avoid duplicate events.
- **Port allocation** — Module-level counter steps sequentially through `RTP_PORT_MIN`–`RTP_PORT_MAX`, avoiding port reuse within a single run.

---

## Testing

### Verify no file descriptor leak

```bash
node --import tsx/esm test/fd-leak.mjs
```

Runs 20 sequential `createRtpHandler` + `dispose()` cycles and asserts the process FD count returns to baseline (±2 tolerance).

```
Baseline FDs: 46
Running 20 call cycles...
Final FDs: 46
Delta: 0
PASS: no FD leak detected
```

### Type-check

```bash
pnpm typecheck
```

---

## File Layout

```
audio-dock/
├── src/
│   ├── index.ts               # Entrypoint — wires SIP UA, CallManager, shutdown
│   ├── config/
│   │   └── index.ts           # Zod config schema; exits on missing/invalid vars
│   ├── logger/
│   │   └── index.ts           # Pino root logger; createChildLogger helper
│   ├── sip/
│   │   ├── userAgent.ts       # SIP REGISTER + INVITE dispatcher
│   │   └── sdp.ts             # SDP offer parsing and answer generation
│   ├── rtp/
│   │   └── rtpHandler.ts      # Per-call UDP socket; RTP header codec; DTMF decode
│   ├── ws/
│   │   └── wsClient.ts        # Per-call WebSocket; Twilio Media Streams protocol
│   └── bridge/
│       └── callManager.ts     # Call orchestrator; CallSession map; reconnect loop
├── test/
│   └── fd-leak.mjs            # FD leak detector (20 sequential alloc/dispose cycles)
├── test-listener.js           # Minimal Twilio Media Streams listener for local testing
├── Dockerfile                 # Four-stage multi-stage build (node:22-alpine)
├── docker-compose.yml         # Linux production: network_mode: host
├── .env.example               # All environment variables with descriptions
└── package.json
```

---

## Requirements not yet implemented (v2 backlog)

| Feature | Notes |
|---------|-------|
| `GET /health` | Returns JSON with `registered` status and `activeCalls` count |
| `GET /metrics` | Prometheus exposition: `active_calls_total`, `sip_registration_status`, RTP/WS counters |
| Outbound call initiation | Requires a different state machine; out of scope for v1 |
| Multi-codec transcoding | G.722, Opus; currently PCMU only |
| SRTP media encryption | Appropriate for internet-facing deployments |
