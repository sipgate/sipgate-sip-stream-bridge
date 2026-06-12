# sipgate-sip-stream-bridge — Go implementation (v3.0)

A Go service that bridges inbound sipgate SIP calls to a WebSocket backend using the [Twilio Media Streams](https://www.twilio.com/docs/voice/media-streams) protocol, **plus** a Twilio-compatible REST control plane for modifying calls already in progress. Drop-in compatible with any Twilio Media Streams consumer — AI voice bots, call recording, real-time transcription — and with the Twilio Call-resource REST API for live call control (list / read / modify, mid-call TwiML interrupt, `<Dial>` forwarding, status callbacks).

Default behaviour is unchanged from v2.x: every inbound call auto-answers and streams to `WS_TARGET_URL` with zero extra configuration. The v3.0 control plane is opt-in (set `AUTH_TOKEN` to enable the REST API; set `DIAL_ALLOWED_PREFIXES` to permit `<Dial>` forwarding).

```
sipgate SIP trunk  ←──────────────────────────────────────────────────────→  caller / callee
        │                                                                          │
        │ SIP (UDP/TCP, SIP_LISTEN_ADDR)                                  RTP/UDP  │
        ▼                                                                          ▼
  ┌──────────────────────────────────────────────────────────────────────────────┐
  │                          sipgate-sip-stream-bridge (Go)                         │
  │                                                                               │
  │   SIP Handler ──► CallManager ──► CallSession (data plane)                    │
  │                                       │                                       │
  │                          rtpReader ◄──┤──► wsPacer  (RTP → WS, paced 20 ms)  │
  │                          rtpPacer  ───┤                                       │
  │                          wsToRTP   ◄──┘──► reconnect loop (1s/2s/4s backoff)  │
  │                                                                               │
  │   REST API (chi + Basic Auth) ──► TwiML dispatch ──► Forwarder (B2BUA <Dial>) │
  │     /2010-04-01/Accounts/{AccountSid}/Calls...        │                        │
  │                                                       └──► dual-leg RTP relay │
  │   Status callbacks ──► X-Twilio-Signature HMAC-SHA1 POST (exp-backoff, SSRF)  │
  └──────────────────────────────────────────────────────────────────────────────┘
                                        │                          ▲
                              WebSocket (ws/wss)                    │ status-callback POSTs
                                        │                          │
                                        ▼                          ▼
                              AI voice-bot backend          customer endpoint
                          (Twilio Media Streams protocol)
```

## Features

### Data plane (streaming bridge)

- Registers with sipgate SIP trunking and stays registered (auto re-registration at 75% of server-granted Expires)
- Accepts inbound SIP INVITEs, negotiates codec per `AUDIO_MODE` (default `twilio`: PCMU/G.711 μ-law 8 kHz; `best`: G.722 16 kHz > PCMA > PCMU — highest quality sipgate offers)
- Supports optional **SRTP encrypted media** via `SRTP_ENABLED=true`: negotiates RTP/SAVP with SDES key exchange (AES-128-CM-HMAC-SHA1-80, RFC 3711/4568); falls back to plain RTP/AVP when the offer is unencrypted
- Opens one WebSocket connection per call to the configured backend URL
- Bridges audio bidirectionally: RTP ↔ base64-encoded `media` events (encoding determined by `AUDIO_MODE`)
- Forwards DTMF digits as `dtmf` events (RFC 4733, End-bit deduplication by RTP timestamp)
- Forwards `mark` events from the backend to the caller — echoes the mark name after all preceding audio frames have been sent; immediate echo when the outbound queue is idle
- Handles `clear` events from the backend — discards buffered outbound audio and echoes all pending mark names immediately
- SIP OPTIONS keepalive: probes the registrar every `SIP_OPTIONS_INTERVAL`; triggers re-registration after 2 consecutive failures (401/407 responses count as success — server is reachable)
- Survives transient WebSocket drops: exponential backoff reconnect (1 s → 2 s → 4 s, 30 s budget); call stays up, inbound RTP is dropped (not buffered) during reconnect
- After reconnect: backend receives fresh `connected` + `start` before any `media` events
- Inbound RTP playout buffer smooths sipgate sender bursts (50-frame queue, paced output at 20 ms)
- Supports multiple concurrent calls, each fully isolated

### v3.0 control plane (opt-in)

- **Twilio-compatible REST API** under `/2010-04-01/Accounts/{AccountSid}/...`: `GET /Calls.json` (list active + recently terminated), `GET /Calls/{CallSid}.json` (single call resource), `POST /Calls/{CallSid}.json` (modify in-flight call). chi router; snake_case JSON; Twilio-shaped error bodies. HTTP Basic Auth with `subtle.ConstantTimeCompare` and URL-path AccountSid validation.
- **Mid-call modify** via `POST /Calls/{CallSid}.json`: `Twiml=<Response>...</Response>` (inline, capped at the 64 KB body limit) **xor** `Url=`+`Method=` (TwiML fetched from a URL) **xor** `Status=completed` (terminate). The new verb chain interrupts any active stream/dial on the same call session.
- **TwiML verbs**: `<Hangup/>` (clean teardown) and `<Dial>` + `<Number>` (B2BUA forwarding). Unimplemented (`<Connect>`/`<Reject>`/`<Redirect>`) and unknown verbs are warn-and-skipped, matching Twilio's parser behaviour.
- **B2BUA `<Dial>` forwarding**: outbound INVITE over the shared sipgo dialog client (UDP source-port pinhole survives — same socket as the registrar), automatic 401/407 Digest re-challenge, dual-leg RTP relay, ring-timeout + time-limit watchdog timers, optional action callback. **Privacy gate hard-coded into the SIP layer**: the WS stream is closed cleanly (`stop reason="dial-forward"`) *before* the outbound INVITE is sent — the bot does not hear the forwarded conversation.
- **Toll-fraud guardrails** enforced at the SIP layer (before the outbound INVITE is constructed): `DIAL_ALLOWED_PREFIXES` allow-list (default empty = deny all), per-session cap (`DIAL_MAX_PER_SESSION`), and global rolling-minute cap (`DIAL_MAX_PER_MINUTE`).
- **Caller-ID resolution chain**: TwiML `callerId` → preserve inbound ANI → `DIAL_DEFAULT_CALLER_ID` → reject with Twilio code `13214`. `DIAL_CALLER_ID_COUNTRY_CODE` normalises display caller-IDs into the international format sipgate trunking requires.
- **Status callbacks**: Twilio lifecycle events (`initiated`/`ringing`/`answered`/`in-progress`/`completed`/`busy`/`failed`/`no-answer`/`canceled`) as HTTP POSTs, signed with an `X-Twilio-Signature` HMAC-SHA1 header byte-compatible with `twilio-python` and `twilio-node` (golden-vector tested), monotonic `SequenceNumber`, RFC 2822 `Timestamp`, 4-attempt exponential-backoff retry (1+2+4 s), per-call `http.Client` for blast-radius isolation, SSRF guard (rejects RFC 1918 / link-local / localhost via resolve-then-validate, defeats DNS rebinding). The operator-supplied default subscription (`STATUS_CALLBACK_DEFAULT_URL`) bypasses the SSRF guard (operators control deployment); REST-supplied URLs stay guarded.
- **REST hardening**: `Strict-Transport-Security` / `Content-Security-Policy` / `X-Content-Type-Options: nosniff` middleware on every response (mounted *before* Basic Auth, so headers are present on 401s); 64 KB request-body cap (`MaxBytesReader`, two-tier Content-Length + chunked-body defense) with a Twilio-shaped 413 JSON error.
- Operator runbook under [`../docs/operator/`](../docs/operator/); Grafana dashboard + Prometheus rules under [`../deploy/observability/`](../deploy/observability/).

### Cross-cutting

- Structured JSON logs (zerolog) with call context on every line; phone-number / URL fields masked at info level
- Static binary Docker image (`FROM scratch`, ~5.5 MB compressed; 6.0 MB CI budget)
- Graceful shutdown on SIGTERM/SIGINT: single drain entry-point (15 s budget) honouring dual-leg `<Dial>` sessions — stop accepting new HTTP → finish in-flight → drain SIP (BYE all legs) → unregister
- `GET /health` — live registration + call/forward counts
- `GET /metrics` — Prometheus exposition

---

## Configuration

All configuration is via environment variables. Copy `../.env.example` to `../.env` and edit. The service validates all variables at startup and exits immediately with a structured JSON error to stderr if anything is missing or malformed.

### Required

| Variable | Description | Example |
|----------|-------------|---------|
| `SIP_USER` | SIP-ID from sipgate portal (Connections › SIP Trunks) | `e12345p0` |
| `SIP_PASSWORD` | SIP password for the SIP-ID above | `s3cr3t` |
| `SIP_DOMAIN` | SIP domain — used in the `From`/`To` URI | `sipconnect.sipgate.de` |
| `SIP_REGISTRAR` | Hostname of the SIP registrar | `sipconnect.sipgate.de` |
| `WS_TARGET_URL` | WebSocket URL of the Twilio Media Streams consumer | `wss://my-bot.example.com/ws` |

### Data plane (optional)

| Variable | Default | Description |
|----------|---------|-------------|
| `SDP_CONTACT_IP` | (auto-detect) | Externally-reachable IP for the SDP contact line (where sipgate delivers INVITEs). Auto-detected via outbound UDP probe if unset. |
| `RTP_PORT_MIN` | `10000` | Start of UDP port range for RTP media sockets |
| `RTP_PORT_MAX` | `10099` | End of UDP port range (must be > `RTP_PORT_MIN`; 100 ports ≈ 50 concurrent calls) |
| `SIP_EXPIRES` | `120` | Requested registration expiry in seconds (re-registers at 75%) |
| `SIP_OPTIONS_INTERVAL` | `30s` | Interval between SIP OPTIONS keepalive pings (Go duration, e.g. `30s`, `1m`); 2 consecutive failures trigger re-registration |
| `LOG_LEVEL` | `info` | zerolog level: `trace` `debug` `info` `warn` `error` |
| `HTTP_PORT` | `9090` | Port for `/health`, `/metrics`, and the REST API |
| `AUDIO_MODE` | `twilio` | `twilio` — PCMU/G.711 μ-law 8 kHz (Twilio-compatible, `mediaFormat: {encoding:"audio/x-mulaw",sampleRate:8000}`); `best` — negotiates highest-quality codec sipgate offers: G.722 (16 kHz) > PCMA > PCMU. Negotiated codec is reflected in the `start` event `mediaFormat`. Validated to one of `twilio` / `best`. |
| `SRTP_ENABLED` | `false` | Enable encrypted media. Set `true`/`1` to negotiate SRTP (RTP/SAVP) with SDES key exchange (AES-128-CM-HMAC-SHA1-80, RFC 3711/4568). Falls back to plain RTP/AVP automatically when the offer is unencrypted. |

### SIP transport (optional)

| Variable | Default | Description |
|----------|---------|-------------|
| `SIP_LISTEN_ADDR` | `0.0.0.0:5060` | SIP listener bind address (`host:port`). Drives both the UDP/TCP listeners and the `Contact:` header port. Accepts IPv4 (`0.0.0.0:5060`), bracketed IPv6 (`[::]:5060`), or bare-port (`:5070`). Override for non-privileged ports, IPv6-only deploys, or a co-hosted registrar in the e2e harness. Port validated to `[1,65535]`. |
| `SIP_OUTBOUND_TARGET_PORT` | `0` | Explicit port for the outbound `<Dial>` INVITE Request-URI. `0` leaves sipgo's standard DNS/5060 routing in place. Set when the trunk listens on a non-standard port, behind a local SBC, or for an e2e UAS stub. Validated to `0` or `[1,65535]`. |

### v3.0 REST control plane (optional)

Set `AUTH_TOKEN` to enable the REST API. The Basic Auth username is a synthesised `AccountSid` = `AC` + first 32 chars of `sha256(SIP_USER)` (deterministic); the password is `AUTH_TOKEN`.

| Variable | Default | Description |
|----------|---------|-------------|
| `AUTH_TOKEN` | (unset) | Twilio-compatible auth token: REST Basic Auth password **and** HMAC-SHA1 signing key for `X-Twilio-Signature`. ≥32 chars recommended. |
| `PUBLIC_BASE_URL` | (unset) | External base URL when running behind a reverse proxy (e.g. `https://bridge.example.com`). Used for `X-Twilio-Signature` URL reconstruction. |

### v3.0 `<Dial>` / B2BUA forwarding (optional)

| Variable | Default | Description |
|----------|---------|-------------|
| `DIAL_ALLOWED_PREFIXES` | (empty = deny-all) | CSV of allowed E.164 prefixes for `<Dial>` targets. **Empty blocks every outbound dial — operator MUST opt in.** Each entry must match `^\+?[0-9]+$`. |
| `DIAL_DEFAULT_CALLER_ID` | (unset) | Explicit fallback caller-ID when TwiML `callerId=` is empty (higher priority than the inbound-To / preserve-ANI auto-fallback). |
| `DIAL_CALLER_ID_COUNTRY_CODE` | (unset) | E.164 country code without `+` (e.g. `49`) used to normalise display caller-IDs into the format sipgate trunking documents. Empty → only strip leading `+` / `00`. |
| `DIAL_RING_TIMEOUT_S` | `30` | Default ring timeout in seconds (validated to `[5,600]`; overridable per-`<Dial>` via TwiML `timeout=`). |
| `DIAL_MAX_PER_SESSION` | `3` | Max `<Dial>` calls per inbound session lifetime (validated ≥1). |
| `DIAL_MAX_PER_MINUTE` | `60` | Global rolling-minute outbound dial cap (toll-fraud / overload defense; validated ≥1). |

### v3.0 default status-callback subscription (optional)

A default `StatusCallback` subscription installed on every inbound call. Operator-supplied (trusted) — bypasses the SSRF guard at dial time. Leave the URL empty to disable.

| Variable | Default | Description |
|----------|---------|-------------|
| `STATUS_CALLBACK_DEFAULT_URL` | (unset = disabled) | Default StatusCallback URL. Must start with `http://` or `https://`. |
| `STATUS_CALLBACK_DEFAULT_METHOD` | `POST` | HTTP method: `POST` or `GET`. |
| `STATUS_CALLBACK_DEFAULT_EVENTS` | `initiated,ringing,answered,completed` | CSV subset of `initiated\|ringing\|answered\|in-progress\|completed\|busy\|failed\|no-answer\|canceled`. Must list at least one event when the URL is set. |

Startup validation example:

```json
{"level":"fatal","msg":"configuration error","error":"env: SIP_USER is required but not set"}
```

---

## Running Locally

**Prerequisites:** Go 1.26+

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

[`../test-listener/`](../test-listener/) is a minimal Twilio Media Streams server:

```bash
# Terminal 1 — install once, then start listener on port 8080
cd ../test-listener && npm install && node index.js

# Terminal 2 — the bridge against the local listener
cd ../go && WS_TARGET_URL=ws://localhost:8080 go run ./cmd/sipgate-sip-stream-bridge
```

Then call your sipgate number. The listener logs `connected`, `start`, `media`, `dtmf`, `mark`, `clear`, and `stop` events.

### Tests

```bash
go test ./...                  # all tests
go test -race -count=3 ./...    # with race detector (project default)
go run ./cmd/lint-metrics ./... # metrics-cardinality + secret-mask AST lint
go test -race -count=1 ./test/e2e/...   # Go integration e2e suite
```

The project [`../Makefile`](../Makefile) wraps the full suite: `make test`, `make lint`, `make lint-metrics`, `make build`, `make e2e`, `make image-size-check`.

---

## Docker

### Published image

Pre-built images are published to the GitHub Container Registry on every push to `main`:

```bash
docker pull ghcr.io/sipgate/sipgate-sip-stream-bridge-go:latest
docker run --env-file ../.env --network host ghcr.io/sipgate/sipgate-sip-stream-bridge-go:latest
```

Available tags: `latest`, `main`, `sha-<commit>`, `v<semver>`.

### Build locally

```bash
cd go
docker build -t sipgate-sip-stream-bridge:latest .
```

The Dockerfile uses a two-stage build:
1. **builder** — `golang:1.26-alpine`; produces a statically-linked binary (`CGO_ENABLED=0`, `-ldflags="-s -w" -trimpath`)
2. **runtime** — `FROM scratch`; only the binary and CA certificates (the latter required for `wss://` and `https://` TwiML/callback fetches)

The runtime image is ~5.5 MB compressed; a build-blocking CI gate enforces a 6.0 MB budget (`make image-size-check` — see [`../docs/operator/IMAGE_SIZE.md`](../docs/operator/IMAGE_SIZE.md)).

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

All endpoints are served on `HTTP_PORT` (default `9090`) by a single chi router.

### `GET /health`

Locked four-field JSON contract (snake_case):

```json
{"registered": true, "account_sid": "ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", "active_calls": 2, "active_forwards": 1}
```

Always returns HTTP 200. K8s-readiness scrapable.

### `GET /metrics`

Prometheus exposition format (custom registry). Selected metrics:

| Metric | Type | Description |
|--------|------|-------------|
| `active_calls_total` | Gauge | Currently active calls |
| `sip_registration_status` | Gauge | 1 = registered, 0 = unregistered |
| `rtp_packets_received_total` / `rtp_packets_sent_total` | Counter | RTP packets in / out |
| `ws_reconnect_attempts_total` | Counter | WebSocket reconnect attempts |
| `mark_echoed_total` / `clear_received_total` | Counter | mark/clear protocol events |
| `sip_options_failures_total` | Counter | OPTIONS keepalive failures |
| `sipgate_bridge_api_requests_total{route,method,status}` | Counter | REST API requests (bounded label enums) |
| `sipgate_bridge_api_request_duration_seconds{route}` | Histogram | REST API latency |
| `sipgate_bridge_twiml_modify_total{kind,outcome}` | Counter | Mid-call modify outcomes |
| `sipgate_bridge_forward_attempts_total` / `_success_total` / `_failed_total{reason}` | Counter | `<Dial>` B2BUA outcomes |
| `sipgate_bridge_forward_duration_seconds{outcome}` | Histogram | `<Dial>` durations |
| `sipgate_bridge_auth_challenge_kind_total{kind}` | Counter | Outbound 401/407 Digest challenges |
| `sipgate_bridge_status_callback_attempts_total{event}` / `_failures_total{reason}` | Counter | Status-callback delivery |
| `sipgate_bridge_rtp_port_pool_size` / `_in_use` | Gauge | RTP port-pool occupancy |
| `sipgate_bridge_rtp_port_acquire_failures_total` | Counter | RTP port exhaustion |

All `*Vec` metrics use bounded label enums, enforced by the `cmd/lint-metrics` CI gate. See [`../deploy/observability/`](../deploy/observability/) for the full PromQL inventory + alert rules.

### REST API (`/2010-04-01/Accounts/{AccountSid}/...`)

HTTP Basic Auth — username = synthesised `AccountSid`, password = `AUTH_TOKEN`. Requires `AUTH_TOKEN` to be set.

```bash
ACCOUNT_SID=$(printf '%s' "$SIP_USER" | openssl dgst -sha256 -hex | cut -c1-32 | sed 's/^/AC/')
BASE="http://localhost:9090/2010-04-01/Accounts/$ACCOUNT_SID"

# List active + recently-terminated calls
curl -u "$ACCOUNT_SID:$AUTH_TOKEN" "$BASE/Calls.json"

# Get a single call resource
curl -u "$ACCOUNT_SID:$AUTH_TOKEN" "$BASE/Calls/$CALL_SID.json"

# Modify in-flight call: forward via <Dial> to PSTN
curl -u "$ACCOUNT_SID:$AUTH_TOKEN" -X POST "$BASE/Calls/$CALL_SID.json" \
  --data-urlencode 'Twiml=<Response><Dial><Number>+4912345</Number></Dial></Response>'

# Modify in-flight call: hang up
curl -u "$ACCOUNT_SID:$AUTH_TOKEN" -X POST "$BASE/Calls/$CALL_SID.json" -d 'Status=completed'

# Modify via remote TwiML URL
curl -u "$ACCOUNT_SID:$AUTH_TOKEN" -X POST "$BASE/Calls/$CALL_SID.json" \
  -d 'Url=https://myapp.example.com/handler.xml' -d 'Method=POST'
```

`POST /Calls/{CallSid}.json` accepts exactly one of `Twiml=` / `Url=`+`Method=` / `Status=completed`. Per-`<Dial>` status callbacks are configured via `<Dial statusCallback="https://..." statusCallbackEvent="...">` — SSRF-guarded at parse time (`localhost`, RFC 1918, link-local rejected). Every response carries HSTS / CSP / `X-Content-Type-Options: nosniff`; request bodies are capped at 64 KB.

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

> **`mediaFormat` depends on `AUDIO_MODE`.**
> Default (`twilio`): `{"encoding":"audio/x-mulaw","sampleRate":8000,"channels":1}`.
> With `AUDIO_MODE=best` and G.722 negotiated: `{"encoding":"audio/G722","sampleRate":16000,"channels":1}`.
> The backend must decode inbound audio and encode outbound audio in the format indicated by `mediaFormat`.

#### `media` (inbound audio, 20 ms / 160 bytes)
```json
{
  "event": "media",
  "sequenceNumber": "2",
  "media": {"track": "inbound", "chunk": "0", "timestamp": "0", "payload": "<base64-encoded audio>"},
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

> When a call is forwarded via `<Dial>`, the bridge closes the WS stream with `stop` carrying `reason="dial-forward"` **before** the outbound INVITE is sent (privacy gate) — the bot does not hear the forwarded conversation.

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

Requests a mark echo. The bridge places a mark sentinel in the outbound audio queue. When all preceding audio frames have been sent to the caller, it sends a `mark` event back to the backend with the same name. If the queue is idle when the mark arrives, the echo is sent immediately (fast-path, no enqueue).

#### `clear`
```json
{
  "event": "clear",
  "streamSid": "MZxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
}
```

Instructs the bridge to discard all buffered outbound audio immediately. Any pending mark sentinels are echoed back to the backend before the queue is flushed. The outbound RTP pacer continues running — silence is sent to the caller during the gap.

### WebSocket reconnect behaviour

If the backend disconnects during an active call:

1. The SIP call **stays up** — no BYE is sent.
2. Inbound RTP from the caller is **dropped** (not buffered) during reconnect.
3. Reconnect is retried with exponential backoff: **1 s → 2 s → 4 s** (capped), up to a **30-second budget**.
4. On successful reconnect, the backend receives a fresh `connected` then `start` before any `media`.
5. If the budget is exhausted, the bridge sends SIP BYE and terminates the call.

---

## Architecture

### Components

| Component | Package / file | Responsibility |
|-----------|----------------|----------------|
| Entrypoint | `cmd/sipgate-sip-stream-bridge/main.go` | Wires all components; chi router (`/health`, `/metrics`, REST); signal handling; ordered drain |
| Config | `internal/config/config.go` | go-simpler/env validated config; fail-fast on missing/invalid vars |
| SIP Agent | `internal/sip/agent.go` | sipgo UA/Server/Client setup (shared socket) |
| SIP Handler | `internal/sip/handler.go` | INVITE/ACK/BYE/OPTIONS dispatch; shutdown guard (503 during drain) |
| Registrar | `internal/sip/registrar.go` | REGISTER + Digest Auth + re-register loop at 75% Expires |
| SDP | `internal/sip/sdp.go` | Parse SDP offer; build SDP answer (codec per `AUDIO_MODE` + telephone-event) |
| Forwarder | `internal/sip/forwarder.go` | B2BUA `<Dial>`: outbound INVITE, ring/time-limit watchdog, dual-leg activation |
| Digest factory | `internal/sip/digest.go` | sipgo `DialogClientCache`-backed dial client (auto-401/407 re-challenge) |
| Guardrails | `internal/sip/guardrails.go` | Toll-fraud allow-list + per-session / rolling-minute rate limits |
| Pre-registered | `internal/sip/preregistered.go` | Pre-registration test/handshake support |
| CallManager | `internal/bridge/manager.go` | sync.Map of active sessions; RTP port pool; `DrainAll` on shutdown |
| CallSession | `internal/bridge/session.go` | Per-call bridge goroutines: rtpReader, rtpPacer, wsPacer, wsToRTP |
| Leg / state | `internal/bridge/leg.go`, `state.go` | Callee leg (B2BUA) + session state machine (streaming ↔ forwarding) |
| WS helpers | `internal/bridge/ws.go` | sendConnected/Start/Media/Stop/DTMF; wsSignal; dialWS |
| REST API | `internal/api/server.go`, `calls.go`, `auth.go`, `security.go`, `errors.go`, `json.go`, `midcall_adapter.go` | chi routes, Basic Auth, security headers + body cap, Twilio JSON serialisation, mid-call adapter |
| TwiML | `internal/twiml/parse.go`, `dispatch.go`, `verbs.go`, `verb_dial.go`, `verb_hangup.go` | Parse `<Response>`; dispatch verb chain; `<Dial>`/`<Hangup>` handlers |
| Webhook | `internal/webhook/status.go`, `signer.go`, `ssrf.go`, `client.go`, `subscription.go` | Status callbacks, X-Twilio-Signature HMAC-SHA1, SSRF guard, retry queue |
| Identity | `internal/identity/account_sid.go`, `call_sid.go` | Synthesised `AccountSid` (`AC`+sha256) + `CallSid` generation |
| Observability | `internal/observability/metrics.go`, `logging.go` | Custom prometheus.Registry; bounded-cardinality metrics; secret-masking logger |

### Tooling commands

| Command | Purpose |
|---------|---------|
| `cmd/lint-metrics` | AST walker enforcing metric label-cardinality + secret-mask discipline (`make lint-metrics`) |
| `cmd/test-registrar` | Stub UDP registrar (REGISTER → 200 OK) used by the e2e harness |

### Shutdown sequence (SIGTERM/SIGINT)

Single drain entry-point, 15 s budget, dual-leg aware (fits inside the 30 s K8s pod-termination grace):

```
1. handler.SetShutdown()    — new INVITEs receive 503; stop accepting new HTTP
2. (in-flight HTTP requests allowed to finish)
3. callManager.DrainAll()   — Terminate() all sessions: BYE every leg (incl. <Dial> callee)
4. registrar.Unregister()   — REGISTER Expires:0
5. agent.UA.Close()         — close SIP transport (deferred)
```

---

## CI gates / Tests

Driven by the project [`../Makefile`](../Makefile); CI (`.github/workflows/docker-go.yml`) runs these as build-blocking gates plus image publish:

```bash
make test                # cd go && go test -race -count=3 ./...
make lint                # golangci-lint
make lint-metrics        # cmd/lint-metrics: cardinality + secret-mask AST lint
make build               # go build ./...
make e2e                 # Go integration e2e + all 8 sipp scenarios (build-blocking)
make image-size-check    # Docker image budget enforcer (≤6.0 MB compressed)
```

- **Metrics-cardinality lint** (`cmd/lint-metrics`): walks every Go package, asserts `*Vec` emit sites use only allow-listed bounded labels (no `call_sid`/`account_sid`/phone/URL labels) and that phone/URL fields are not logged at info level. Unit-tested via `cmd/lint-metrics/testdata/{clean,violations}`.
- **sipp e2e harness** (`../tests/e2e/sipp/`): 8 [sipp](http://sipp.sourceforge.net/) XML scenarios driving the bridge through realistic SIP flows against a co-hosted `cmd/test-registrar` + stub WS/UAS — `inbound-default-streaming`, `inbound-rest-dial-{answer,busy,no-answer,cancel,codec-mismatch}`, `rest-hangup-mid-stream`, `status-callback-flapping-host`. `make e2e` runs all eight as build-blocking gates (`run-sipp.sh` wires every component). Requires the `sipp` / `sip-tester` binary (installed in CI).
- **Go integration e2e** (`test/e2e/`): cross-component tests for ordered shutdown, the `<Dial>` privacy gate, goroutine-leak detection, and CANCEL/BYE race conditions (`go test -race -count=1 ./test/e2e/...`).

---

## File Layout

```
go/
├── cmd/
│   ├── sipgate-sip-stream-bridge/
│   │   └── main.go                # Entrypoint: chi router, drain, signal handling
│   ├── lint-metrics/              # Cardinality + secret-mask AST lint (CI gate)
│   │   ├── main.go
│   │   ├── walker.go
│   │   └── testdata/{clean,violations}/
│   └── test-registrar/
│       └── main.go                # Stub UDP registrar for the e2e harness
├── internal/
│   ├── config/config.go           # Validated env-var config
│   ├── identity/                   # Synthesised AccountSid + CallSid
│   ├── sip/
│   │   ├── agent.go                # sipgo UA/Server/Client (shared socket)
│   │   ├── handler.go              # INVITE/BYE/OPTIONS handler
│   │   ├── registrar.go            # REGISTER + re-register loop
│   │   ├── sdp.go                  # SDP offer/answer
│   │   ├── forwarder.go            # B2BUA <Dial> forwarding
│   │   ├── digest.go               # sipgo DialogClientCache (auto-401/407)
│   │   ├── guardrails.go           # Toll-fraud allow-list + rate limits
│   │   └── preregistered.go
│   ├── bridge/
│   │   ├── manager.go              # CallManager + RTP port pool + DrainAll
│   │   ├── session.go              # Per-call bridge goroutines
│   │   ├── leg.go                  # B2BUA callee leg
│   │   ├── state.go                # Session state machine (streaming ↔ forwarding)
│   │   └── ws.go                   # WebSocket helpers + wsSignal
│   ├── api/                        # chi REST API: server, calls, auth, security, errors, json, midcall_adapter
│   ├── twiml/                      # parse + dispatch + verb_dial + verb_hangup + verbs
│   ├── webhook/                    # status callbacks, signer (HMAC-SHA1), ssrf, client, subscription
│   └── observability/              # metrics (bounded labels) + masking logger
├── test/e2e/                       # Go integration e2e (shutdown, privacy gate, leaks, races)
├── Dockerfile                      # Two-stage: golang:1.26-alpine → FROM scratch
├── docker-compose.yml              # Linux production (network_mode: host)
└── go.mod
```

---

## Toolchain

| Component | Version |
|-----------|---------|
| Go | 1.26 |
| SIP library | [`github.com/emiago/sipgo`](https://github.com/emiago/sipgo) v1.3.1 (auto-401/407 in `WaitAnswer`) |
| REST router | [`github.com/go-chi/chi/v5`](https://github.com/go-chi/chi) v5.2.5 |
| WebSocket | `github.com/gobwas/ws` v1.4.0 |
| RTP / SDP / SRTP | `github.com/pion/{rtp,sdp,srtp}` |
| Metrics | `github.com/prometheus/client_golang` v1.23.2 |
| Logging | `github.com/rs/zerolog` v1.35.1 |
| Config | `go-simpler.org/env` v0.12.0 |
| Lint AST | `golang.org/x/tools` v0.44.0 (`cmd/lint-metrics` via `go/packages`) |

See [`../docs/release-notes/v3.0.0.md`](../docs/release-notes/v3.0.0.md) and [`../CHANGELOG.md`](../CHANGELOG.md) for the full v3.0 feature list, migration notes, and operator runbooks.
