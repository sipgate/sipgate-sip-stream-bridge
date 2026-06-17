# sipgate-sip-stream-bridge

Bridges inbound sipgate SIP calls to a WebSocket backend using the [Twilio Media Streams](https://www.twilio.com/docs/voice/media-streams) protocol — drop-in compatible with any Twilio Media Streams consumer (AI voice bots, transcription, recording, IVR backends).

Two implementations live in this repository, both deployed in production:

- **[Go](./go/)** — at **v3.1**: streaming bridge + Twilio-compatible REST control plane (list / read / modify calls in progress, mid-call TwiML interrupt, B2BUA `<Dial>` for forwarding, status-callback HTTP POSTs with `X-Twilio-Signature` HMAC).
- **[Node.js / TypeScript](./node/)** — at **v3.1**: full data-plane parity (PCMU/PCMA/G.722, mark/clear/dtmf, SRTP, OPTIONS keepalive, graceful drain) **plus** the same Twilio-compatible REST control plane (REST API, mid-call TwiML `<Hangup>`/`<Dial>`, B2BUA forwarding, status callbacks). The outbound SIP leg is a purpose-built UAC dialog on the shared socket (no external SIP library).

Pick either implementation — both cover streaming, dynamic mid-call control, and `<Dial>` forwarding.

```
                    ┌─ sipgate trunk (SIP/UDP) ─┐
                    │                            │
                    ▼                            │
                ┌────────────────────────────────────────────┐
   inbound ───►│            sipgate-sip-stream-bridge        │◄─── REST control
                │           (Go or Node.js streaming bridge)  │
                │                                            │
                │  SIP Handler ─► CallManager ─► RTP socket  │
                │                       │                    │
                │              per-call WebSocket            │
                │                       │                    │
                │           Twilio Media Streams protocol    │
                │              (mark / clear / dtmf)         │
                │                                            │
                │  B2BUA forwarder (Go + Node.js):           │
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

## v3.0 control plane

Both implementations cover the streaming bridge use case at v2.1 parity **and** the v3.0 Twilio-compatible REST API for **modifying calls already in progress** (mid-call TwiML interrupt, B2BUA `<Dial>` forwarding, status callbacks). The Go implementation additionally ships operational tooling (metrics-cardinality lint, sipp e2e harness, operator runbook); the Node.js control plane is built on a purpose-built UAC dialog over its hand-rolled SIP stack.

| Feature | Description |
|---------|-------------|
| **REST API** | `GET /Calls.json` (list active + recently terminated), `GET /Calls/{Sid}.json` (call resource), `POST /Calls/{Sid}.json` (modify in-flight call). Path prefix `/2010-04-01/Accounts/{AccountSid}/...`, snake_case JSON, RFC 2822 timestamps, Twilio-shaped error bodies. Basic Auth with `subtle.ConstantTimeCompare`. |
| **Mid-call modify** | `POST /Calls/{Sid}.json` accepting `Twiml=<Response>...</Response>` (≤4000 chars, inline) **xor** `Url=`+`Method=` (TwiML fetched from a URL) **xor** `Status=completed` (terminate). Verb-chain interrupt cancels any active stream/dial and dispatches the new chain on the same call session. |
| **TwiML verbs** | `<Dial>` (with `<Number>` noun) for B2BUA forwarding, `<Hangup/>` for clean teardown. Unknown verbs warn-and-skip per Twilio's parser behaviour. |
| **B2BUA `<Dial>`** | Outbound INVITE via shared SIP client (UDP source-port pinhole survives), auto-401/407 Digest, dual-leg RTP relay, ring/timeLimit timers, action-callback. **Privacy gate hard-coded into the SIP layer**: the WS stream is closed cleanly before the outbound INVITE is sent — the bot does not hear the forwarded conversation. |
| **Toll-fraud guardrails** | `DIAL_ALLOWED_PREFIXES` allow-list (default empty = deny all), per-session cap, global rolling-minute cap, codec fail-fast. |
| **Status callbacks** | Twilio lifecycle (`initiated` / `ringing` / `in-progress` / `completed` / `busy` / `failed` / `no-answer` / `canceled`) HTTP POSTs, HMAC-SHA1 `X-Twilio-Signature` byte-compatible with `twilio-python` and `twilio-node`, monotonic `SequenceNumber`, RFC 2822 `Timestamp`, 3× exponential-backoff retry on customer-endpoint failure, distinct `http.Client` per call (blast-radius isolation), SSRF guard (RFC 1918 + link-local + localhost rejection). |
| **REST hardening** | HSTS / CSP / `X-Content-Type-Options: nosniff` middleware on every response (mounted before Basic Auth). 64 KB `MaxBytesReader` request-body cap with Twilio-shaped 413 JSON error. |
| **Graceful shutdown** | Single drain entry-point with 15 s budget for dual-leg BYE, ordered as: stop accepting new HTTP → finish in-flight → drain SIP → unregister. Fits inside the 30 s K8s pod-termination grace. Goroutine count returns to baseline after the drain. |
| **Observability** | Bounded label cardinality enforced by a CI lint (`make lint-metrics`); secret masking on phone-number / URL log fields; per-call structured logs with `call_sid` / `account_sid` / `forward_leg_id`; RTP port-pool gauge. |
| **Image budget** | Static `FROM scratch` runtime, build-blocking CI gate at ≤6.0 MB. |
| **Operator UI** | Read-only web monitor at `/ui` (same origin → no CORS) listing active + recently-terminated calls and SIP-registration / active-call health. Static bundle served unauthenticated; the UI logs in via its own form and calls the REST API with `AccountSid:AUTH_TOKEN` explicitly, so the Twilio API stays strict. One Svelte + Vite bundle embedded into both backends (Go `//go:embed`, Node static serve); polls `/health` + `GET /Calls.json`. `/` redirects to `/ui/`. sipgate-branded. Build: `make ui-go` / `make ui-node`. |

## Quick Start

```bash
cp .env.example .env       # fill in SIP credentials
```

### Go

```bash
make build                 # cd go && go build ./...
make test                  # full module: go test -race -count=3 ./...
cd go && go run ./cmd/sipgate-sip-stream-bridge
```

### Node.js

```bash
cd node
pnpm install
pnpm dev                   # or: node --import tsx/esm src/index.ts
```

Both implementations register with the configured `SIP_REGISTRAR`, listen for inbound INVITEs (Go: configurable via `SIP_LISTEN_ADDR`; Node.js: `:5060`), and expose `GET /health` + `GET /metrics` on port 9090. The Go implementation additionally serves the REST API at `/2010-04-01/Accounts/{AccountSid}/Calls/...` (Basic Auth: synthesised `AccountSid` + `AUTH_TOKEN`).

The synthesised `AccountSid` is `AC` + first 32 chars of `sha256(SIP_USER)` (deterministic).

## Configuration

Environment variables only — `.env` is auto-loaded; production sets vars directly via secrets. See [`.env.example`](./.env.example) for the complete inventory.

### Required (both implementations)

| Variable | Description |
|----------|-------------|
| `SIP_USER` | SIP-ID from sipgate portal |
| `SIP_PASSWORD` | SIP password |
| `SIP_DOMAIN` | SIP registrar domain (e.g. `sipconnect.sipgate.de`) |
| `SIP_REGISTRAR` | SIP registrar address |
| `WS_TARGET_URL` **or** `VOICE_URL` | Exactly one must be set (mutually exclusive). `WS_TARGET_URL` — static WebSocket URL for every call. `VOICE_URL` — Twilio-compatible webhook that returns TwiML `<Connect><Stream>` per call; bridge POSTs before answering and uses the WS URL from the TwiML response. |

### Required by the v3.0 REST control plane (Go + Node.js)

| Variable | Description |
|----------|-------------|
| `AUTH_TOKEN` | Twilio-compatible auth token: REST Basic Auth password and HMAC key for status-callback signing. ≥32 chars recommended. |

### Go REST / `<Dial>` options

| Variable | Default | Description |
|----------|---------|-------------|
| `PUBLIC_BASE_URL` | (auto) | External base URL when running behind a reverse proxy. Used for `X-Twilio-Signature` URL reconstruction. |
| `DIAL_ALLOWED_PREFIXES` | (empty = deny-all) | CSV of allowed E.164 prefixes for `<Dial>` targets. Operator MUST opt in explicitly. |
| `DIAL_DEFAULT_CALLER_ID` | (auto: inbound-To) | Explicit fallback caller-ID when TwiML `callerId=` is empty. |
| `DIAL_CALLER_ID_COUNTRY_CODE` | (empty) | E.164 country code without `+` (e.g. `49`) used to normalise caller-IDs into the format sipgate trunking documents. |
| `DIAL_RING_TIMEOUT_S` | `30` | Default ring timeout (5–600 s; per-`<Dial>` override via `timeout=`). |
| `DIAL_MAX_PER_SESSION` | `3` | Max `<Dial>` calls per inbound session. |
| `DIAL_MAX_PER_MINUTE` | `60` | Global rolling-minute outbound dial cap. |

### Transport / runtime

| Variable | Default | Description | Implementations |
|----------|---------|-------------|-----------------|
| `SIP_LISTEN_ADDR` | `0.0.0.0:5060` | SIP listener bind address (`host:port`). Drives both UDP/TCP listeners and the `Contact:` header port. Override for non-privileged ports, IPv6-only, or single-host harness setups. | Go |
| `HTTP_PORT` | `9090` | HTTP server port (`/health`, `/metrics`, REST API). | Both |
| `RTP_PORT_MIN` / `RTP_PORT_MAX` | `10000` / `10099` | UDP port range for RTP. | Both |
| `SDP_CONTACT_IP` | (auto-detect) | Externally-reachable IP for SDP and `Contact:` headers. | Both |
| `AUDIO_MODE` | `twilio` | `twilio` (PCMU only) or `best` (G.722 > PCMA > PCMU). Negotiated codec is reflected in the `start.mediaFormat` event. | Both |
| `SRTP_ENABLED` | `false` | Negotiate SRTP (RTP/SAVP) with SDES (AES-128-CM-HMAC-SHA1-80). Falls back to plain RTP/AVP automatically. | Both |
| `SIP_OPTIONS_INTERVAL` | `30s` | OPTIONS keepalive interval; re-register after 2 consecutive failures. | Both |

## REST API (Go)

Authentication: HTTP Basic Auth — username = synthesised `AccountSid`, password = `AUTH_TOKEN`.

```bash
ACCOUNT_SID=$(echo -n "$SIP_USER" | openssl dgst -sha256 -hex | cut -c1-32 | sed 's/^/AC/')
BASE="http://localhost:9090/2010-04-01/Accounts/$ACCOUNT_SID"

# List active + recently-terminated calls
curl -u "$ACCOUNT_SID:$AUTH_TOKEN" "$BASE/Calls.json"

# Get a call resource
curl -u "$ACCOUNT_SID:$AUTH_TOKEN" "$BASE/Calls/$CALL_SID.json"

# Modify in-flight call: forward via <Dial> to PSTN
curl -u "$ACCOUNT_SID:$AUTH_TOKEN" -X POST "$BASE/Calls/$CALL_SID.json" \
  --data-urlencode 'Twiml=<Response><Dial>+4912345</Dial></Response>'

# Modify in-flight call: hang up
curl -u "$ACCOUNT_SID:$AUTH_TOKEN" -X POST "$BASE/Calls/$CALL_SID.json" \
  -d 'Status=completed'

# Modify via remote TwiML URL
curl -u "$ACCOUNT_SID:$AUTH_TOKEN" -X POST "$BASE/Calls/$CALL_SID.json" \
  -d 'Url=https://myapp.example.com/handler.xml' -d 'Method=POST'
```

Status-callback subscriptions are configured per-`<Dial>` via `<Dial statusCallback="https://..." statusCallbackEvent="..."/>`. SSRF-guarded — `localhost`, RFC 1918, and link-local destinations are rejected at parse time.

## Build, test, deploy

The Go implementation drives the project Makefile:

```bash
make build               # cd go && go build ./...
make test                # go test -race -count=3 ./...    (12 packages)
make lint                # golangci-lint
make lint-metrics        # cardinality + secret-mask discipline
make e2e                 # Go integration tests + all 8 sipp scenarios  (build-blocking)
make image-size-check    # docker image budget enforcer (≤6.0 MB)
```

CI (`.github/workflows/docker-go.yml`) runs all of the above as build-blocking gates plus the e2e-advisory step (`continue-on-error: true`) and publishes the multi-stage Docker image.

```bash
docker pull ghcr.io/sipgate/sipgate-sip-stream-bridge-go:latest
docker run --env-file .env --network host \
  ghcr.io/sipgate/sipgate-sip-stream-bridge-go:latest
```

`network_mode: host` is recommended — Docker port-proxy adds ~10 ms UDP jitter on RTP. Image is `FROM scratch` with the static Go binary (`-ldflags="-s -w" -trimpath`) plus the Alpine builder's CA bundle for `wss://` and `https://` TwiML fetches; ~5.5 MB compressed.

For the Node.js implementation:

```bash
cd node
pnpm install
pnpm test
pnpm build
docker build -t sipgate-sip-stream-bridge-node node/
```

A pre-built Node.js image is published on every push:

```bash
docker pull ghcr.io/sipgate/sipgate-sip-stream-bridge-node:latest
```

## Operator runbook

`docs/operator/` ships pre-deployment guidance for the Go implementation:

- [TOLL_FRAUD.md](./docs/operator/TOLL_FRAUD.md) — `DIAL_ALLOWED_PREFIXES` recipes per market + alert response
- [CALLER_ID.md](./docs/operator/CALLER_ID.md) — caller-ID fallback chain; Twilio code 13214
- [INCIDENT_RESPONSE.md](./docs/operator/INCIDENT_RESPONSE.md) — `AUTH_TOKEN` + `SIP_PASSWORD` rotation
- [SESSION_TIMERS.md](./docs/operator/SESSION_TIMERS.md) — RFC 4028 stance (reject-with-BYE)
- [IMAGE_SIZE.md](./docs/operator/IMAGE_SIZE.md) — Docker image budget
- [DASHBOARDS.md](./docs/operator/DASHBOARDS.md) — Grafana provisioning + alert thresholds
- [RELEASE_CHECKLIST.md](./docs/operator/RELEASE_CHECKLIST.md) — operator pre-deploy checklist

Release notes: [`docs/release-notes/`](./docs/release-notes/). Changelog: [`CHANGELOG.md`](./CHANGELOG.md) (Keep a Changelog v1.1.0).

## Local Integration Testing

[`test-listener/`](./test-listener/) is a minimal Twilio Media Streams WebSocket server for manual testing with either implementation — has its own `package.json` with the `ws` dependency.

```bash
# Terminal 1 — install once, then start listener on port 8080
cd test-listener && npm install
node index.js                # MODE=log (default)
MODE=echo node index.js      # echo caller audio back
MODE=tone node index.js      # send a synthetic tone (simulates TTS)
MODE=timing node index.js    # print inter-arrival times (jitter check)

# Terminal 2 — start either bridge against the local listener
WS_TARGET_URL=ws://localhost:8080 cd go && go run ./cmd/sipgate-sip-stream-bridge
# or
WS_TARGET_URL=ws://localhost:8080 cd node && pnpm dev
```

Then call your sipgate number; the listener logs `connected`, `start`, `media`, `stop`, `dtmf`, `mark`, `clear` events.

## End-to-end SIP testing (sipp)

`tests/e2e/sipp/` ships eight [sipp](http://sipp.sourceforge.net/) XML scenarios driving the bridge through realistic SIP flows against a co-hosted test-registrar stub + stub WS server (+ a sipp UAS stub for the `<Dial>` scenarios): inbound default-streaming, REST-driven `<Dial>` answer/busy/no-answer/cancel/codec-mismatch, mid-stream hangup, and status-callback flapping. `make e2e` runs the Go integration tests plus all eight scenarios as a build-blocking gate.

```bash
make e2e                                          # Go integration tests + all 8 scenarios
bash tests/e2e/sipp/run-sipp.sh inbound-rest-dial-answer   # a single scenario

# Run the same harness against the Node.js bridge:
BRIDGE_BIN="$(pwd)/tests/e2e/sipp/run-node-bridge.sh" bash tests/e2e/sipp/run-sipp.sh
```

## Repository Layout

```
sipgate-sip-stream-bridge/
├── go/                                # Go implementation (current)
│   ├── cmd/
│   │   ├── sipgate-sip-stream-bridge/ # Entrypoint
│   │   ├── lint-metrics/              # Cardinality + secret-mask CI lint
│   │   └── test-registrar/            # Test stub (UDP REGISTER → 200 OK)
│   ├── internal/                      # SIP, bridge, api, webhook, twiml, observability
│   ├── test/e2e/                      # Cross-protocol Go integration tests
│   └── Dockerfile                     # multi-stage; FROM scratch runtime
├── node/                              # Node.js / TypeScript implementation
│   └── src/                           # bridge, sip, rtp, srtp, ws, observability
├── tests/e2e/sipp/                    # 8 sipp XML scenarios + harness
├── test-listener/                     # Local Twilio Media Streams listener
├── docs/
│   ├── operator/                      # Operator runbooks
│   └── release-notes/
├── deploy/observability/              # Grafana dashboard + Prometheus rules
├── CHANGELOG.md
├── Makefile
└── .env.example
```

## Implementation comparison

| Capability | Go | Node.js |
|------------|----|---------|
| Language / runtime | Go 1.26 | TypeScript / Node.js 24 |
| Docker image | ~5.5 MB (`FROM scratch`) | ~120 MB (`node:22-alpine`) |
| SIP library | emiago/sipgo v1.3.1 | Raw UDP (no library) |
| Twilio Media Streams `media` | ✅ | ✅ |
| `mark` event echo | ✅ | ✅ |
| `clear` event flush | ✅ | ✅ |
| `dtmf` (RFC 4733 End-bit dedup) | ✅ | ✅ |
| SRTP (`SRTP_ENABLED=true`) | ✅ AES-128-CM-HMAC-SHA1-80 SDES | ✅ AES-128-CM-HMAC-SHA1-80 SDES |
| WS reconnect (1s/2s/4s, 30s budget) | ✅ | ✅ |
| SIP OPTIONS keepalive | ✅ | ✅ |
| Graceful shutdown | ✅ ordered drain (15 s budget, dual-leg) | ✅ ordered drain (15 s budget, dual-leg) |
| `GET /health` + `GET /metrics` | ✅ | ✅ |
| **REST API (`/2010-04-01/...`)** | ✅ chi + Basic Auth | ✅ mini-router + Basic Auth |
| **Mid-call `POST /Calls/{Sid}`** | ✅ TwiML interrupt | ✅ TwiML interrupt |
| **B2BUA `<Dial>` + privacy gate** | ✅ | ✅ UAC dialog on shared socket |
| **Voice-URL webhook (`VOICE_URL`)** | ✅ pre-answer, fallback, HMAC | ✅ pre-answer, fallback, HMAC |
| **Status callbacks + HMAC** | ✅ X-Twilio-Signature | ✅ X-Twilio-Signature |
| **Toll-fraud guardrails** | ✅ DIAL_ALLOWED_PREFIXES + rate limits | ✅ DIAL_ALLOWED_PREFIXES + rate limits |
| **Cardinality lint (CI)** | ✅ `make lint-metrics` | ✗ (convention + review) |
| **Security headers + 64 KB body cap** | ✅ | ✅ |

Both implementations now expose the v3.0 Twilio-compatible control plane. The Go implementation additionally ships the metrics-cardinality lint and the sipp e2e harness as build gates; the Node.js control plane is unit/integration-tested and shares the same byte-level Twilio fixtures (X-Twilio-Signature HMAC).

→ **[Go implementation README](./go/README.md)**

→ **[Node.js implementation README](./node/README.md)**
