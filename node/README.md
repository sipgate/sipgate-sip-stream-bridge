# sipgate-sip-stream-bridge — Node.js implementation (v1.0)

A Node.js/TypeScript service that bridges inbound sipgate SIP calls to a WebSocket backend using the [Twilio Media Streams](https://www.twilio.com/docs/voice/media-streams) protocol. Drop-in compatible with any Twilio Media Streams consumer — AI voice bots, call recording, real-time transcription.

```
sipgate SIP trunk  ←──────────────────────────────────────────────────────→  caller
        │                                                                          │
        │ SIP (UDP :5060)                                                 RTP/UDP  │
        ▼                                                                          ▼
  ┌──────────────────────────────────────────────────────────────────────────────┐
  │                          sipgate-sip-stream-bridge (Node.js)                                 │
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

> **Note:** This is the v1.0 reference implementation. The Go implementation (`../go/`) is the current production version and adds /health, /metrics, a playout buffer, and a static binary Docker image.

## Features

- Registers with sipgate SIP trunking and stays registered (auto re-registration)
- Accepts inbound SIP INVITEs, negotiates PCMU (G.711 μ-law 8 kHz)
- Opens one WebSocket connection per call to the configured backend URL
- Bridges audio bidirectionally: RTP ↔ base64 mulaw `media` events
- Forwards DTMF digits as `dtmf` events
- Forwards `mark` events from the backend to the caller — echoes the mark name after all preceding audio frames have been sent; immediate echo when the outbound queue is idle
- Handles `clear` events from the backend — discards buffered outbound audio and echoes all pending mark names immediately
- SIP OPTIONS keepalive: probes the registrar every `SIP_OPTIONS_INTERVAL` seconds; triggers re-registration after 2 consecutive failures (401/407 responses count as success — server is reachable)
- Survives transient WebSocket drops: exponential backoff reconnect (1 s → 2 s → 4 s, 30 s budget) with μ-law silence to the caller during the reconnect window
- Supports multiple concurrent calls, each fully isolated
- Structured JSON logs (pino) with `callId` and `streamSid` context on every line
- Docker image with multi-stage build on `node:22-alpine`
- Graceful shutdown on SIGTERM/SIGINT: sends SIP BYE to all active calls, unregisters, then exits

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
| `WS_TARGET_URL` | WebSocket URL of the Twilio Media Streams consumer | `ws://localhost:8080` |

### Optional

| Variable | Default | Description |
|----------|---------|-------------|
| `SDP_CONTACT_IP` | *(auto-detect)* | External IP in SDP contact and Via headers. **Set this when running behind NAT.** |
| `RTP_PORT_MIN` | `10000` | Start of UDP port range for RTP media sockets |
| `RTP_PORT_MAX` | `10099` | End of UDP port range (100 ports ≈ 50 concurrent calls) |
| `SIP_EXPIRES` | `120` | SIP registration expiry in seconds (re-registers at 90%) |
| `SIP_OPTIONS_INTERVAL` | `30` | Interval in seconds between SIP OPTIONS keepalive pings; 2 consecutive failures trigger re-registration |
| `LOG_LEVEL` | `info` | Pino log level: `trace` `debug` `info` `warn` `error` |

The service validates all variables at startup using Zod and exits immediately with a structured error if anything is missing:

```json
{"level":"error","errors":{"SIP_USER":["Required"]}}
```

---

## Running Locally

**Prerequisites:** Node.js ≥ 22, pnpm 10+

```bash
cd node
cp ../.env.example ../.env   # fill in SIP credentials (shared with Go)
pnpm install
pnpm dev                     # TypeScript dev server with auto-reload (reads ../.env)
pnpm build                   # compile to dist/
pnpm start                   # run compiled build (reads ../.env)
pnpm typecheck               # type-check without emitting
```

You should see:
```json
{"level":30,"service":"sipgate-sip-stream-bridge","event":"startup","sipUser":"e12345p0","sipDomain":"sipconnect.sipgate.de"}
{"level":30,"service":"sipgate-sip-stream-bridge","event":"sip_registered","expires":120}
```

### Quick integration test

`test-listener.js` (in the repo root) is a minimal Twilio Media Streams server:

```bash
# Terminal 1 — listener on port 8080
MODE=log node ../test-listener.js      # log all events (default)
MODE=echo node ../test-listener.js     # echo caller audio back

# Terminal 2 — sipgate-sip-stream-bridge
pnpm dev
```

Then call your sipgate number. The listener logs `connected`, `start`, and `media` events.

---

## Docker

### Build

```bash
cd node
docker build -t sipgate-sip-stream-bridge-node:latest .
```

The Dockerfile uses a four-stage build:
1. **base** — `node:22-alpine` with corepack
2. **fetcher** — downloads packages into the pnpm store (cached; re-runs only when `pnpm-lock.yaml` changes)
3. **builder** — offline install + TypeScript compilation
4. **production** — minimal runtime image with `dist/` and `node_modules` only

### Run with Docker Compose

```bash
cp ../.env.example ../.env
docker compose up -d
docker compose logs -f sipgate-sip-stream-bridge
docker compose down
```

The `docker-compose.yml` uses `network_mode: host`, which is required on Linux for RTP.

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
    "accountSid": "",
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

#### `media`
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
  "stop": {"accountSid": "", "callSid": "CAxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"},
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

Instructs sipgate-sip-stream-bridge to discard all buffered outbound audio immediately. Any pending mark sentinels in the queue are echoed back to the backend before the queue is flushed.

### WebSocket reconnect behaviour

If the backend disconnects during an active call:

1. The SIP call **stays up** — no BYE is sent.
2. μ-law silence (160 × `0xFF`) is sent to the caller every 20 ms to prevent dead air.
3. Inbound RTP is **dropped** (not buffered) during reconnect.
4. Reconnect retries with backoff: **1 s → 2 s → 4 s** (capped), up to a **30-second budget**.
5. On reconnect, backend receives fresh `connected` + `start` before any `media`.
6. If the budget is exhausted, sipgate-sip-stream-bridge sends SIP BYE.

---

## Architecture

### Components

| Component | File | Responsibility |
|-----------|------|----------------|
| Entrypoint | `src/index.ts` | Wires all components; SIGTERM/SIGINT graceful shutdown |
| Config | `src/config/index.ts` | Zod-validated environment variables; fails fast on startup |
| Logger | `src/logger/index.ts` | Pino JSON logger; `createChildLogger` adds call context |
| SIP UserAgent | `src/sip/userAgent.ts` | Raw UDP :5060; REGISTER with Digest Auth (MD5); INVITE/ACK/BYE/CANCEL |
| SDP | `src/sip/sdp.ts` | `parseSdpOffer` / `buildSdpAnswer` (PCMU + telephone-event) |
| RTP Handler | `src/rtp/rtpHandler.ts` | Per-call UDP socket; RTP header codec; `audio` and `dtmf` events |
| WebSocket Client | `src/ws/wsClient.ts` | Per-call WebSocket; Twilio Media Streams encoding/decoding |
| Call Manager | `src/bridge/callManager.ts` | Call lifecycle orchestration; `Map<callId, CallSession>` |

### Implementation notes

- **No SIP library** — SIP signalling is raw UDP with regex header parsing.
- **NAT traversal** — One silence packet sent on RTP init to hole-punch the router.
- **RFC 3261 §13.3.1.4** — 200 OK retransmit loop with exponential backoff until ACK.
- **DTMF** — RFC 4733 telephone-event (PT 113); fired on `End=1` flag to avoid duplicates.

---

## Testing

### Unit tests

```bash
pnpm test
```

Runs the Vitest suite — covers mark/clear drain behavior (MRKN-01/02/03) and OPTIONS keepalive state machine (OPTN-01/02/03). All 12 tests complete in under 200 ms.

### FD leak check

```bash
node --import tsx/esm test/fd-leak.mjs
```

Runs 20 sequential alloc/dispose cycles and asserts the FD count returns to baseline.

### Type-check

```bash
pnpm typecheck
```

---

## File Layout

```
node/
├── src/
│   ├── index.ts               # Entrypoint
│   ├── config/index.ts        # Zod config schema
│   ├── logger/index.ts        # Pino root logger
│   ├── sip/
│   │   ├── userAgent.ts       # SIP REGISTER + INVITE dispatcher
│   │   └── sdp.ts             # SDP offer/answer
│   ├── rtp/rtpHandler.ts      # Per-call UDP socket; RTP codec; DTMF
│   ├── ws/wsClient.ts         # Per-call WebSocket; Twilio protocol
│   └── bridge/callManager.ts  # Call orchestrator; reconnect loop
├── test/
│   ├── fd-leak.mjs            # FD leak detector
│   ├── wsClient.mark.test.ts  # Unit tests — mark/clear drain (MRKN-01/02/03)
│   └── userAgent.options.test.ts # Unit tests — OPTIONS keepalive state (OPTN-01/02/03)
├── package.json
├── pnpm-lock.yaml
└── tsconfig.json
```
