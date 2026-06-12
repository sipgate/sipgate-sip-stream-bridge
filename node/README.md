# sipgate-sip-stream-bridge — Node.js implementation (v3.0)

A Node.js/TypeScript service that bridges inbound sipgate SIP calls to a WebSocket backend using the [Twilio Media Streams](https://www.twilio.com/docs/voice/media-streams) protocol — drop-in compatible with any Twilio Media Streams consumer (AI voice bots, call recording, real-time transcription) — **plus** a Twilio-compatible REST control plane for modifying calls already in progress (mid-call TwiML interrupt, B2BUA `<Dial>` forwarding, status callbacks).

This implementation is at **v3.0**: full data-plane parity (PCMU/PCMA/G.722, mark/clear/dtmf, SRTP, OPTIONS keepalive, graceful drain) and the same Twilio-compatible REST control plane as the [Go implementation](../go/). The outbound SIP leg is a purpose-built UAC dialog on the shared SIP socket — no external SIP library.

```
                    ┌─ sipgate trunk (SIP/UDP) ─┐
                    │                            │
                    ▼                            │
                ┌────────────────────────────────────────────┐
   inbound ───►│            sipgate-sip-stream-bridge        │◄─── REST control
                │                  (Node.js)                  │
                │                                            │
                │  SipUserAgent ─► CallManager ─► RtpHandler │
                │                       │                    │
                │              per-call WebSocket            │
                │                       │                    │
                │           Twilio Media Streams protocol    │
                │              (mark / clear / dtmf)         │
                │                                            │
                │  B2BUA forwarder:                          │
                │    <Dial> → outbound INVITE → trunk        │
                │    privacy gate: WS stop BEFORE INVITE     │
                └────────────────────────────────────────────┘
                            │                ▲
                            ▼                │ status-callback POSTs
                  AI voice-bot backend       │ (X-Twilio-Signature HMAC-SHA1)
                  (Twilio Media Streams)     │
                                             ▼
                                  customer endpoint
```

## Features

### Data plane (streaming bridge)

- Registers with sipgate SIP trunking and stays registered (auto re-registration)
- Accepts inbound SIP INVITEs, negotiates codec per `AUDIO_MODE` (default `twilio`: PCMU/G.711 μ-law 8 kHz; `best`: G.722 16 kHz > PCMA > PCMU — highest quality sipgate offers)
- Supports optional **SRTP encrypted media** via `SRTP_ENABLED=true`: negotiates RTP/SAVP with SDES key exchange (AES-128-CM-HMAC-SHA1-80, RFC 3711/4568); falls back to plain RTP/AVP when the offer is unencrypted
- Opens one WebSocket connection per call to the configured backend URL
- Bridges audio bidirectionally: RTP ↔ base64-encoded `media` events (encoding determined by `AUDIO_MODE`)
- Forwards DTMF digits as `dtmf` events
- Forwards `mark` events from the backend to the caller — echoes the mark name after all preceding audio frames have been sent; immediate echo when the outbound queue is idle
- Handles `clear` events from the backend — discards buffered outbound audio and echoes all pending mark names immediately
- SIP OPTIONS keepalive: probes the registrar every `SIP_OPTIONS_INTERVAL` seconds; triggers re-registration after 2 consecutive failures (401/407 responses count as success — server is reachable)
- Survives transient WebSocket drops: exponential backoff reconnect (1 s → 2 s → 4 s, 30 s budget) with codec silence to the caller during the reconnect window
- Supports multiple concurrent calls, each fully isolated
- Structured JSON logs (pino) with `callId` and `streamSid` context on every line; `AUTH_TOKEN` and `SIP_PASSWORD` are masked from log output

### v3.0 control plane

- **Twilio-compatible REST API** under `/2010-04-01/Accounts/{AccountSid}/Calls...` with HTTP Basic Auth (`subtle`/`timingSafeEqual`). The `AccountSid` is derived deterministically from `SIP_USER`; the password is `AUTH_TOKEN`.
- **Mid-call modify**: `POST /Calls/{Sid}.json` accepting `Twiml=` (≤4000 chars, inline) **xor** `Url=`+`Method=` (TwiML fetched over HTTPS) **xor** `Status=completed` (terminate). Interrupts any active stream/dial and dispatches the new chain on the same call session.
- **TwiML verbs**: `<Dial>` (with `<Number>` noun) for B2BUA forwarding and `<Hangup/>` for clean teardown. Unknown verbs warn-and-skip per Twilio's parser behaviour.
- **B2BUA `<Dial>` forwarding**: an outbound INVITE on the **shared** SIP socket (the REGISTER source-port pinhole survives) via a purpose-built UAC dialog, auto-401/407 Digest with `qop=auth` (cnonce/nc), dual-leg RTP relay, ring/timeLimit timers, codec fail-fast (outbound leg requires PCMU; otherwise ACK + immediate BYE).
- **Privacy gate** (hard-coded into the SIP layer): the WS stream is closed cleanly **before** the outbound INVITE is sent — the bot does not hear the forwarded conversation.
- **Toll-fraud guardrails**: `DIAL_ALLOWED_PREFIXES` allow-list (default empty = **deny-all**), per-session cap (`DIAL_MAX_PER_SESSION`), and a global rolling-minute cap (`DIAL_MAX_PER_MINUTE`).
- **Caller-ID resolution**: outbound `From` user-part follows TwiML `callerId` → `DIAL_DEFAULT_CALLER_ID` → `SIP_USER` → inbound ANI; the displayed caller-ID (preserve-ANI) is carried separately in `P-Preferred-Identity` (RFC 3325), normalised via `DIAL_CALLER_ID_COUNTRY_CODE`.
- **Status callbacks**: Twilio lifecycle (`initiated` / `ringing` / `answered` / `in-progress` / `completed` / `busy` / `failed` / `no-answer` / `canceled`) HTTP POSTs, `X-Twilio-Signature` HMAC-SHA1 byte-compatible with `twilio-python` / `twilio-node`, monotonic `SequenceNumber`, RFC 2822 `Timestamp`, exponential-backoff retry (`0/1/2/4 s`, 4 attempts) on transport errors / 408 / 429 / 5xx, SSRF guard (RFC 1918 + link-local + localhost rejection — never retried).
- **REST hardening**: security-headers middleware (HSTS / CSP / `X-Content-Type-Options: nosniff`) on every response including 401/413, mounted **before** Basic Auth; 64 KB request-body cap with a Twilio-shaped 413 JSON error.
- Docker image with multi-stage build on `node:24-alpine`
- Graceful shutdown on SIGTERM/SIGINT: 15 s drain budget — stop accepting new HTTP → dual-leg BYE on all calls (+ close WS + drain status callbacks) → unregister → exit

---

## Configuration

All configuration is via environment variables. Copy `../.env.example` to `../.env` and edit. The service validates every variable at startup using Zod and exits immediately with a structured error if anything is missing or invalid:

```json
{"level":"error","msg":"Configuration validation failed","errors":{"SIP_USER":["SIP_USER is required — your sipgate SIP-ID (e.g. e12345p0)"]}}
```

### Required

| Variable | Description | Example |
|----------|-------------|---------|
| `SIP_USER` | SIP-ID from sipgate portal (Connections › SIP Trunks) | `e12345p0` |
| `SIP_PASSWORD` | SIP password for the SIP-ID above | `s3cr3t` |
| `SIP_DOMAIN` | SIP domain — used in the `From`/`To` URI | `sipconnect.sipgate.de` |
| `SIP_REGISTRAR` | Hostname of the SIP registrar | `sipconnect.sipgate.de` |
| `WS_TARGET_URL` | WebSocket URL of the Twilio Media Streams consumer | `ws://localhost:8080` |

### Data plane (optional)

| Variable | Default | Description |
|----------|---------|-------------|
| `SDP_CONTACT_IP` | *(auto-detect)* | External IPv4 in SDP contact and Via headers. **Set this when running behind NAT.** |
| `RTP_PORT_MIN` | `10000` | Start of UDP port range for RTP media sockets (1024–65534) |
| `RTP_PORT_MAX` | `10099` | End of UDP port range (1025–65535; 100 ports ≈ 50 concurrent calls). Must be greater than `RTP_PORT_MIN`. |
| `SIP_EXPIRES` | `120` | SIP registration expiry in seconds (re-registers at 90%) |
| `SIP_OPTIONS_INTERVAL` | `30` | Interval in seconds between SIP OPTIONS keepalive pings; 2 consecutive failures trigger re-registration |
| `HTTP_PORT` | `9090` | HTTP server port (`/health`, `/metrics`, REST control plane) |
| `LOG_LEVEL` | `info` | Pino log level: `trace` `debug` `info` `warn` `error` |
| `AUDIO_MODE` | `twilio` | Audio codec mode: `twilio` — PCMU/G.711 μ-law 8 kHz (Twilio-compatible, `mediaFormat: {encoding:"audio/x-mulaw",sampleRate:8000}`); `best` — negotiates highest-quality codec sipgate offers: G.722 (16 kHz) > PCMA > PCMU; negotiated codec is reflected in the `start` event `mediaFormat` and RTP payload is forwarded as-is |
| `SRTP_ENABLED` | `false` | Enable encrypted media. Set to `true` (or `1`) to negotiate SRTP (RTP/SAVP) with sipgate using SDES key exchange (AES-128-CM-HMAC-SHA1-80, RFC 3711/4568). Falls back to plain RTP/AVP automatically when the offer is unencrypted. Each accepted call logs `srtp_negotiated` when active. |

### v3.0 REST control plane / `<Dial>` (optional)

| Variable | Default | Description |
|----------|---------|-------------|
| `AUTH_TOKEN` | `""` (empty) | REST Basic Auth password **and** the HMAC key used to sign `X-Twilio-Signature`. Empty is allowed (streaming-only operation); ≥32 chars recommended when the control plane is exposed. |
| `PUBLIC_BASE_URL` | *(unset)* | External base URL when running behind a reverse proxy. Used to reconstruct the signed URL for `X-Twilio-Signature` when the public URL differs from the bound one. Must be a valid URL. |
| `SIP_OUTBOUND_TARGET_PORT` | `0` | Explicit port for the outbound INVITE Request-URI. `0` = DNS/default. (0–65535) |
| `SIP_LISTEN_ADDR` | `0.0.0.0:5060` | SIP listener bind address (`host:port`). Drives both the inbound UDP socket bind **and** the Via/Contact port. The sipp e2e harness sets `127.0.0.1:5070` so the bridge can coexist with a test-registrar stub on `:5060`. |
| `STATUS_CALLBACK_DEFAULT_URL` | *(unset = disabled)* | Operator-supplied default StatusCallback installed on every inbound call. Must be `http(s)://`. Trusted — bypasses the SSRF guard. |
| `STATUS_CALLBACK_DEFAULT_METHOD` | `POST` | HTTP method for the default StatusCallback: `POST` or `GET`. |
| `STATUS_CALLBACK_DEFAULT_EVENTS` | `initiated,ringing,answered,completed` | CSV of subscribed events. Valid: `initiated`, `ringing`, `answered`, `in-progress`, `completed`, `busy`, `failed`, `no-answer`, `canceled`. |
| `DIAL_ALLOWED_PREFIXES` | `""` (= **deny-all**) | Toll-fraud allow-list of E.164 prefixes (CSV). Empty denies all `<Dial>` targets — the operator must opt in explicitly. Entries match `^\+?[0-9]*$` (e.g. `+49`, `0`, or `+` for all). |
| `DIAL_DEFAULT_CALLER_ID` | *(unset)* | Fallback caller-ID when TwiML `<Dial callerId>` is absent. |
| `DIAL_CALLER_ID_COUNTRY_CODE` | *(unset)* | E.164 country code without `+` (digits only, e.g. `49`) used to normalise the trunk caller-ID. |
| `DIAL_RING_TIMEOUT_S` | `30` | Default `<Dial>` ring timeout in seconds (5–600; per-`<Dial>` override via `timeout=`). |
| `DIAL_MAX_PER_SESSION` | `3` | Maximum `<Dial>` calls per inbound session (≥1). |
| `DIAL_MAX_PER_MINUTE` | `60` | Global rolling-minute outbound dial cap (≥1). |

---

## Running Locally

**Prerequisites:** Node.js ≥ 24 (`package.json` `engines`), pnpm (pinned via `packageManager`, enabled through corepack)

```bash
cd node
cp ../.env.example ../.env   # fill in SIP credentials (shared with Go)
pnpm install
pnpm dev                     # TypeScript dev server with auto-reload (reads ../.env)
pnpm build                   # compile to dist/ (tsup, ESM)
pnpm start                   # run compiled build (reads ../.env)
pnpm typecheck               # type-check without emitting
```

You should see:
```json
{"level":30,"event":"startup","sipUser":"e12345p0","sipDomain":"sipconnect.sipgate.de"}
{"level":30,"event":"sip_booted","msg":"SIP registrar started — waiting for calls"}
{"level":30,"event":"http_server_started","port":9090}
```

### Quick integration test

`../test-listener/` is a minimal Twilio Media Streams server:

```bash
# Terminal 1 — listener on port 8080
cd ../test-listener && npm install
node index.js                # MODE=log (default) — log all events
MODE=echo node index.js      # echo caller audio back

# Terminal 2 — sipgate-sip-stream-bridge
cd ../node && WS_TARGET_URL=ws://localhost:8080 pnpm dev
```

Then call your sipgate number. The listener logs `connected`, `start`, and `media` events.

---

## HTTP Endpoints

The service exposes a single HTTP server on `HTTP_PORT` (default `9090`):

| Method | Path | Auth | Purpose |
|--------|------|------|---------|
| `GET` | `/health` | none | Liveness/health JSON (SIP-registered flag, counters) |
| `GET` | `/metrics` | none | Prometheus text exposition |
| `GET` | `/2010-04-01/Accounts/{AccountSid}/Calls.json` | Basic | List active + recently-terminated calls (paginated) |
| `GET` | `/2010-04-01/Accounts/{AccountSid}/Calls/{CallSid}.json` | Basic | Fetch a single Call resource |
| `POST` | `/2010-04-01/Accounts/{AccountSid}/Calls/{CallSid}.json` | Basic | Modify a call in progress |

The REST routes are Twilio-shaped: snake_case JSON, RFC 2822 timestamps, nullable fields serialised as explicit `null`, paginated list envelope, and Twilio-shaped error bodies (`{code, message, more_info, status}`).

**Authentication.** HTTP Basic Auth — username = the derived `AccountSid`, password = `AUTH_TOKEN`. The `AccountSid` is `AC` + the first 16 bytes of `SHA-256(SIP_USER)` as 32 lowercase hex chars (deterministic across restarts). Both the credential and the `{AccountSid}` path segment are compared in constant time; a mismatch returns `401`, not `404`.

```bash
ACCOUNT_SID="AC$(printf '%s' "$SIP_USER" | openssl dgst -sha256 -hex | cut -c1-32)"
BASE="http://localhost:9090/2010-04-01/Accounts/$ACCOUNT_SID"

# List active + recently-terminated calls
curl -u "$ACCOUNT_SID:$AUTH_TOKEN" "$BASE/Calls.json"

# Get a single call resource
curl -u "$ACCOUNT_SID:$AUTH_TOKEN" "$BASE/Calls/$CALL_SID.json"

# Modify in-flight call: forward via <Dial> to PSTN
curl -u "$ACCOUNT_SID:$AUTH_TOKEN" -X POST "$BASE/Calls/$CALL_SID.json" \
  --data-urlencode 'Twiml=<Response><Dial><Number>+4912345</Number></Dial></Response>'

# Modify in-flight call: hang up
curl -u "$ACCOUNT_SID:$AUTH_TOKEN" -X POST "$BASE/Calls/$CALL_SID.json" \
  -d 'Status=completed'

# Modify via remote TwiML URL (fetched over HTTPS, SSRF-guarded)
curl -u "$ACCOUNT_SID:$AUTH_TOKEN" -X POST "$BASE/Calls/$CALL_SID.json" \
  -d 'Url=https://myapp.example.com/handler.xml' -d 'Method=POST'
```

`Twiml`, `Url`, and `Status=completed` are mutually exclusive (at most one). `StatusCallback` may be supplied on its own to install a subscription without otherwise mutating the call. `Status=completed` is idempotent on an already-terminated call (`200`, not `21220`).

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

> **Note:** `start.accountSid` (and `stop.accountSid`) are emitted as the empty string in the WS payload — the data-plane stream does not carry the derived REST `AccountSid`.

> **`mediaFormat` depends on `AUDIO_MODE`.**
> Default (`twilio`): `{"encoding":"audio/x-mulaw","sampleRate":8000,"channels":1}`.
> With `AUDIO_MODE=best` and G.722 negotiated: `{"encoding":"audio/G722","sampleRate":16000,"channels":1}`.
> The backend must decode inbound audio and encode outbound audio in the format indicated by `mediaFormat`.

#### `media`
```json
{
  "event": "media",
  "media": {"track": "inbound", "chunk": "0", "timestamp": "0", "payload": "<base64-encoded audio>"},
  "streamSid": "MZxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
}
```

#### `dtmf`
```json
{
  "event": "dtmf",
  "dtmf": {"track": "inbound_track", "digit": "5"},
  "streamSid": "MZxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
}
```

#### `mark` (echo)
```json
{
  "event": "mark",
  "mark": {"name": "my-label"},
  "streamSid": "MZxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
}
```

Sent after all preceding outbound audio frames have been delivered to the caller, confirming the audio up to this point has played out.

#### `stop`
```json
{
  "event": "stop",
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
  "media": {"payload": "<base64-encoded audio, 160 bytes per 20 ms frame>"}
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

Requests a mark echo. sipgate-sip-stream-bridge places a mark sentinel in the outbound audio queue. When all preceding audio frames have been sent to the caller, it sends a `mark` event back to the backend with the same name. If the queue is idle at the moment the mark arrives, the echo is sent immediately (fast-path, no enqueue).

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
5. On reconnect, the backend receives fresh `connected` + `start` before any `media`.
6. If the budget is exhausted, sipgate-sip-stream-bridge sends SIP BYE.

---

## Architecture

### Components

| Component | File | Responsibility |
|-----------|------|----------------|
| Entrypoint | `src/index.ts` | Wires all components; HTTP server (REST + health/metrics); 15 s graceful drain |
| Version | `src/version.ts` | SIP User-Agent / Server header (`sipgate-sip-stream-bridge/3.0`) |
| Config | `src/config/index.ts` | Zod-validated environment variables; fails fast on startup |
| Logger | `src/logger/index.ts` | Pino JSON logger; `createChildLogger` adds call context; masks `AUTH_TOKEN` / `SIP_PASSWORD` |
| Identity | `src/identity/sid.ts` | `newCallSid()` (random) + `deriveAccountSid()` (deterministic from `SIP_USER`) |
| Observability | `src/observability/metrics.ts` | In-memory counters/gauges with bounded-cardinality labels; Prometheus text output |
| SIP UserAgent | `src/sip/userAgent.ts` | Shared UDP socket; REGISTER (Digest MD5) + OPTIONS keepalive + inbound dispatch + outbound-dialog routing hook |
| SIP SDP | `src/sip/sdp.ts` | `parseSdpOffer` / `buildSdpAnswer` (inbound) + `buildSdpOffer` / `acceptSdpAnswer` (outbound `<Dial>` leg) |
| SIP Digest | `src/sip/digest.ts` | Digest with `qop=auth` (cnonce/nc) for outbound INVITE 401/407 |
| SIP Dialog | `src/sip/dialog.ts` | UAC client dialog: outbound INVITE/ACK/CANCEL/BYE, response matching, P-Preferred-Identity |
| SIP Forwarder | `src/sip/forwarder.ts` | `<Dial>` orchestration: guardrails → caller-ID → INVITE → SDP/codec fail-fast → dual-leg relay → timers |
| SIP Guardrails | `src/sip/guardrails.ts` | `DIAL_ALLOWED_PREFIXES` allow-list (default-deny) + per-session / rolling-minute caps |
| SIP Caller-ID | `src/sip/callerId.ts` | From / display caller-ID resolution chain + trunk normalisation |
| RTP Handler | `src/rtp/rtpHandler.ts` | Per-call UDP socket; RTP header codec; `audio` and `dtmf` events |
| SRTP | `src/srtp/srtpContext.ts` | SRTP/SDES crypto context (AES-128-CM-HMAC-SHA1-80) |
| WebSocket Client | `src/ws/wsClient.ts` | Per-call WebSocket; Twilio Media Streams encoding/decoding |
| Call Manager | `src/bridge/callManager.ts` | Call lifecycle; `CallSession` map + CallSid index + recently-terminated TTL; reconnect loop; dual-leg teardown |
| Bridge state | `src/bridge/state.ts` | Call-state enum + transitions |
| REST API | `src/api/{server,router,auth,security,errors,json,calls,midcallAdapter}.ts` | Composed handler: security headers → 64 KB cap → Basic Auth → list/get/modify |
| TwiML | `src/twiml/{parse,verbs,dispatch,verbDial,verbHangup}.ts` | `<Response>` parser + verb dispatch (`<Dial>` / `<Hangup>` / unknown→warn-skip) |
| Webhook | `src/webhook/{signer,ssrf,subscription,status,callbackForm}.ts` | X-Twilio-Signature HMAC-SHA1, SSRF guard, per-call retrying status-callback queue |

### Implementation notes

- **No SIP library** — both the inbound UA and the outbound `<Dial>` UAC dialog are hand-rolled over a single shared UDP socket (raw header parsing). The outbound INVITE leaves from the same source port as REGISTER, keeping the NAT pinhole valid.
- **Privacy gate** — `CallManager.closeStream()` tears down the WS stream before `createCalleeLeg()` / the outbound INVITE; the ordering is a tested invariant.
- **RFC 3261 §13.3.1.4** — 200 OK retransmit loop with exponential backoff until ACK.
- **DTMF** — RFC 4733 telephone-event; fired on the End bit to avoid duplicates.
- **HMAC byte-compatibility** — `webhook/signer.ts` matches `twilio-python` / `twilio-node` exactly (sorted keys, no delimiter, raw values, standard base64, URL verbatim) and is validated against the shared Go fixtures.

---

## Testing

### Unit + integration tests

```bash
pnpm test          # vitest run
```

Runs the Vitest suite — **436 tests across 28 files** (REST list/get/modify, TwiML parse + dispatch, status-callback signer/SSRF/subscription/retry, SIP dialog/digest/forwarder/guardrails/caller-ID, SDP, SRTP, identity, logger masking, metrics, mark/clear drain, OPTIONS keepalive, BYE + reconnect). Completes in well under 10 s.

### End-to-end (sipp)

The language-neutral sipp harness in `../tests/e2e/sipp/` can be driven against the Node bridge — all 8 scenarios pass (inbound streaming, REST `<Dial>` answer/busy/no-answer/cancel/codec-mismatch, mid-stream hangup, status-callback flapping):

```bash
BRIDGE_BIN="$(pwd)/tests/e2e/sipp/run-node-bridge.sh" tests/e2e/sipp/run-sipp.sh
```

`run-node-bridge.sh` sets `SIP_LISTEN_ADDR` so the bridge coexists with the test-registrar stub.

### Type-check

```bash
pnpm typecheck
```

---

## Docker

### Published image

Pre-built images are published to the GitHub Container Registry on every push to `main`:

```bash
docker pull ghcr.io/sipgate/sipgate-sip-stream-bridge-node:latest
docker run --env-file ../.env --network host ghcr.io/sipgate/sipgate-sip-stream-bridge-node:latest
```

Available tags: `latest`, `main`, `sha-<commit>`, `v<semver>`.

### Build locally

```bash
cd node
docker build -t sipgate-sip-stream-bridge-node:latest .
```

The Dockerfile uses a four-stage build on `node:24-alpine`:
1. **base** — `node:24-alpine` with corepack (pnpm auto-selected from the pinned `packageManager`)
2. **fetcher** — downloads packages into the pnpm store (cached; re-runs only when `pnpm-lock.yaml` changes)
3. **builder** — offline install + TypeScript compilation
4. **production** — minimal runtime image with `dist/` + production `node_modules`, running as the non-root `node` user

### Run with Docker Compose

```bash
cp ../.env.example ../.env
docker compose up -d
docker compose logs -f sipgate-sip-stream-bridge
docker compose down
```

The `docker-compose.yml` uses `network_mode: host`, which is required on Linux for RTP.

---

## File Layout

```
node/
├── src/
│   ├── index.ts                 # Entrypoint; HTTP server; graceful drain
│   ├── version.ts               # User-Agent / Server header
│   ├── config/index.ts          # Zod config schema
│   ├── config/listenAddr.ts     # SIP_LISTEN_ADDR host:port parsing
│   ├── logger/index.ts          # Pino root logger + secret masking
│   ├── identity/sid.ts          # CallSid / AccountSid minting
│   ├── observability/metrics.ts # Bounded-cardinality metrics; Prometheus output
│   ├── sip/
│   │   ├── userAgent.ts         # Shared socket: REGISTER + OPTIONS + dispatch
│   │   ├── sdp.ts               # SDP offer/answer (inbound + outbound)
│   │   ├── digest.ts            # Digest qop=auth (outbound INVITE)
│   │   ├── dialog.ts            # UAC client dialog
│   │   ├── forwarder.ts         # <Dial> B2BUA orchestration
│   │   ├── guardrails.ts        # Toll-fraud allow-list + caps
│   │   └── callerId.ts          # Caller-ID resolution chain
│   ├── rtp/rtpHandler.ts        # Per-call UDP socket; RTP codec; DTMF
│   ├── srtp/srtpContext.ts      # SRTP/SDES crypto context
│   ├── ws/wsClient.ts           # Per-call WebSocket; Twilio protocol
│   ├── bridge/
│   │   ├── callManager.ts       # Call orchestrator; reconnect loop; teardown
│   │   └── state.ts             # Call-state enum + transitions
│   ├── api/
│   │   ├── server.ts            # Composed handler (security → cap → auth → dispatch)
│   │   ├── router.ts            # Route matcher
│   │   ├── auth.ts              # Basic Auth (timing-safe)
│   │   ├── security.ts          # Security headers + 64 KB body cap
│   │   ├── errors.ts            # Twilio-shaped error bodies
│   │   ├── json.ts              # Call/page serialisation + RFC 2822
│   │   ├── calls.ts             # list/get/modify handlers
│   │   └── midcallAdapter.ts    # bridge ↔ twiml seam (privacy gate ordering)
│   ├── twiml/
│   │   ├── parse.ts             # <Response> parser
│   │   ├── verbs.ts             # Verb types
│   │   ├── dispatch.ts          # Verb-chain dispatch
│   │   ├── verbDial.ts          # <Dial> handler
│   │   └── verbHangup.ts        # <Hangup> handler
│   └── webhook/
│       ├── signer.ts            # X-Twilio-Signature HMAC-SHA1
│       ├── ssrf.ts              # SSRF guard
│       ├── subscription.ts      # Event subscription matching
│       ├── status.ts            # Per-call retrying status-callback queue
│       └── callbackForm.ts      # Status-callback form fields
├── test/                         # vitest suite + fd-leak.mjs
├── Dockerfile
├── package.json
├── pnpm-lock.yaml
└── tsconfig.json
```
