# sipgate-sip-stream-bridge

## What This Is

A Go service that registers with sipgate trunking via SIP, accepts incoming calls, and bridges audio bidirectionally to a configurable WebSocket endpoint. The WebSocket protocol implements the full Twilio Media Streams format (connected/start/media/stop/dtmf/mark/clear events), making it drop-in compatible with AI voice-bot backends. Ships as a ~1 MB Docker image from a static Go binary. Includes `/health` and `/metrics` (Prometheus) endpoints. A Node.js/TypeScript reference implementation (v1.0 + v2.1 parity updates) is preserved in the `node/` directory.

## Core Value

Incoming SIP calls from sipgate trunking are reliably bridged to a WebSocket endpoint in real-time — audio flows both ways, the connection stays alive through transient drops, and the integration is drop-in compatible with Twilio Media Streams consumers including barge-in via mark/clear events.

## Current State (v2.1 — shipped 2026-03-05)

**Tech stack:** Go 1.26, emiago/sipgo v1.2.0, pion/sdp v3.0.18, pion/rtp v1.10.1, rs/zerolog, gobwas/ws, prometheus/client_golang v1.23.2. Docker: golang:1.26-alpine builder + FROM scratch runtime. Node.js: TypeScript, SIP.js, Vitest 4.0.18.

**Codebase:** ~17 Go source files, ~3,300 LOC. Layout:
- `go/cmd/sipgate-sip-stream-bridge/main.go` — entrypoint, wiring, graceful drain
- `go/internal/config/` — env var schema + validation (incl. SIPOptionsInterval)
- `go/internal/sip/` — Agent, Registrar (incl. optionsKeepaliveLoop), Handler, SDP parsing
- `go/internal/bridge/` — PortPool, CallManager, CallSession, WS bridge (incl. mark/clear)
- `go/internal/observability/` — Prometheus metrics (incl. mark_echoed_total, clear_received_total, sip_options_failures_total)

**Node.js (node/):** TypeScript reference with full v2.1 parity — DrainItem tagged-union queue, WsClient mark/clear interface, OPTIONS keepalive, Vitest tests.

**Known issues / tech debt:**
- Human verification items require live sipgate credentials (E2E audio, concurrent calls, OPTIONS response classification)
- DTMF PT mismatch watch: sipgate uses PT 113; service extracts dynamically from SDP offer
- VALIDATION.md frontmatter not updated from draft in Phases 9–11 (tests passed; run `/gsd:validate-phase` retroactively)
- OPTIONS timeout asymmetry: Go=10s, Node.js=5s — minor, both correct

## Requirements

### Validated

- ✓ Service registers with sipgate trunking via SIP on startup — v1.0 (SIP.js 0.21.x), v2.0 (sipgo v1.2.0)
- ✓ Service accepts incoming SIP calls and negotiates PCMU codec — v1.0, v2.0
- ✓ Each call establishes a dedicated WebSocket connection to the configured target URL — v1.0, v2.0
- ✓ Audio from caller is forwarded to WebSocket in Twilio Media Streams format (base64 mulaw 8kHz) — v1.0, v2.0
- ✓ Audio received from WebSocket is played back to the caller (bidirectional) — v1.0, v2.0
- ✓ Multiple concurrent calls supported, each with independent WebSocket connection — v1.0, v2.0
- ✓ If WebSocket is unavailable at call start, the SIP call is rejected with 503 — v1.0, v2.0
- ✓ If WebSocket drops during an active call, service reconnects and keeps call alive — v1.0, v2.0 (exponential backoff 1s/2s/4s, 30s budget)
- ✓ All credentials and target URL configured via environment variables — v1.0, v2.0 (same env var names)
- ✓ Service runs in Docker with multi-stage build — v1.0 (node:22-alpine), v2.0 (FROM scratch ~1 MB)
- ✓ DTMF digits forwarded as `dtmf` events (RFC 4733 End-bit deduplication) — v1.0, v2.0
- ✓ Automatic re-registration at server-granted Expires interval — v1.0 (refreshFrequency 90%), v2.0 (75% re-register goroutine)
- ✓ Graceful SIGTERM shutdown — v2.0 (DrainAll BYE loop + UNREGISTER, exits within 10s)
- ✓ `GET /health` returns JSON with registered status and active call count — v2.0
- ✓ `GET /metrics` returns Prometheus exposition with key counters — v2.0
- ✓ `mark` event: WS server sends mark, sipgate-sip-stream-bridge echoes it when outbound audio playout reaches that point — v2.1 (Go + Node.js)
- ✓ `clear` event: WS server sends clear, sipgate-sip-stream-bridge flushes buffered outbound RTP audio and echoes pending marks — v2.1 (Go + Node.js)
- ✓ SIP OPTIONS keepalive: periodic OPTIONS ping to sipgate to detect silent registration loss (2-failure threshold triggers re-registration) — v2.1 (Go + Node.js)

### Active

*(None — v2.1 shipped all planned requirements. Next milestone to be defined.)*

### Out of Scope

- Outbound call initiation — inbound only (different state machine)
- Web UI or management dashboard — config is env-only
- Call recording or storage — audio is streamed, not persisted
- Multiple target URLs / routing logic — single fixed WebSocket URL (dumb pipe by design)
- SRTP / media encryption — plain RTP for internal infrastructure use
- Multi-codec support (G.722, Opus) — PCMU only; transcoding belongs in WS consumer
- In-dialog OPTIONS for Session-Refresh (RFC 4028) — different use case from trunk monitoring

## Context

**v1.0** shipped 2026-03-03 with 1,648 TypeScript LOC, 39 files (preserved in `node/`).
**v2.0** shipped 2026-03-05 with ~2,900 Go LOC, 15 files.
**v2.1** shipped 2026-03-05 with +4,195 / -159 lines across 40 files (Go + Node.js).

**Drop-in compatibility:** All env var names identical to v1.0. Same Twilio Media Streams protocol — now including mark/clear.

**Protocol completeness:** All Twilio Media Streams inbound-call events now implemented: connected, start, media, stop, dtmf, mark, clear.

## Constraints

- **Language:** Go — deterministic GC, goroutines, direct syscall UDP access
- **SIP Library:** emiago/sipgo v1.2.0
- **Transport:** UDP/TCP SIP, RTP audio
- **Config:** Environment variables only — no config files, suitable for Docker/K8s secrets
- **Backwards compat:** Same env var names as v1.0, same WS protocol — drop-in replacement

## Key Decisions

| Decision | Rationale | Outcome |
|----------|-----------|---------|
| SIP.js over drachtio (v1.0) | No C++ sidecar, simpler Docker setup | ✓ Good — works in Node.js with ws polyfill |
| Twilio Media Streams format | Drop-in compatibility with existing AI voice backends | ✓ Good — full protocol now implemented |
| Reject call if WS unavailable | Fail fast is cleaner than silently dropping audio | ✓ Good — 503 rejection implemented |
| Reconnect on WS drop, hold call | Better UX than forcing caller to redial on transient WS issues | ✓ Good — backoff loop in both v1.0 and v2.0 |
| network_mode: host for Docker | RTP requires no NAT; Docker port-proxy adds ~10ms UDP jitter | ✓ Good — essential for RTP timing |
| Zod 4 for config validation (v1.0) | Single-source-of-truth env var schema with fail-fast | ✓ Good |
| Drop RTP during WS reconnect (not buffer) | Bounded memory; audio gap is brief and acceptable | ✓ Good — consistent across v1.0 and v2.0 |
| Go rewrite for v2.0 | Eliminate GC-induced jitter, shrink Docker image, add observability | ✓ Good — ~1 MB image, goroutine-based determinism |
| emiago/sipgo v1.2.0 | Pure Go, stable v1.x, production-proven via LiveKit | ✓ Good — Digest Auth + dialog cache work correctly |
| FROM scratch Docker image | ~1 MB vs 180 MB Node.js Alpine; no shell, no runtime attack surface | ✓ Good — achieved 1.06 MB |
| No diago higher-level RTP | Raw RTP byte access needed for Twilio base64 encoding | ✓ Good — diago abstraction fights this use case |
| Custom prometheus.Registry | Excludes Go runtime noise from /metrics scrape | ✓ Good — only 5 sipgate-sip-stream-bridge metrics exposed |
| wsSignal with sync.Once | Double-close-safe signaling when multiple goroutines can fail | ✓ Good — prevents panic on simultaneous failures |
| outboundFrame tagged union (v2.1) | Separate audio/mark fields over magic-byte sentinel — idiomatic nil check for silence fallback | ✓ Good — enables clear/mark routing without encoding coupling |
| wsPacer sole-writer invariant (v2.1) | Only wsPacer writes to wsConn — mark echoes route via markEchoQueue channel | ✓ Good — no concurrent write races on WebSocket |
| rtpPacer never stopped on clear (v2.1) | Drain packetQueue only; seqNo/timestamp advance on sentinel frames | ✓ Good — no RTP timestamp discontinuity or audible click |
| OPTIONS 401/407 = server alive (v2.1) | Treat as auth issue, not registration loss | ✓ Good — prevents spurious re-registration storms |
| doRegister mutex (v2.1) | sipgo concurrent client.Do() thread-safety undocumented | ✓ Good — serializes reregisterLoop + optionsKeepaliveLoop |
| CSeq routing for OPTIONS responses (Node.js v2.1) | Prevent OPTIONS 401/407 from triggering REGISTER auth challenge handler | ✓ Good — correct protocol separation |

---
*Last updated: 2026-03-05 after v2.1 milestone*
