# Project Research Summary

**Project:** audio-dock v2.0 Go Rewrite
**Domain:** SIP-to-WebSocket audio bridge (SIP media gateway)
**Researched:** 2026-03-03
**Confidence:** HIGH

## Executive Summary

audio-dock v2.0 is a full Go rewrite of a production SIP/RTP/WebSocket audio bridge that connects sipgate trunking to an AI voice-bot backend via the Twilio Media Streams protocol. The rewrite is motivated by a concrete, measurable problem: Node.js GC pauses cause RTP timing violations that degrade audio quality. Go's goroutine model eliminates this natively — no drain-queue workarounds required. This is not a new product; it is a 1:1 behavioral replacement. Every feature from v1.0 must survive into v2.0, and the service must be a drop-in replacement in the same Docker Compose/Kubernetes environment.

The recommended approach is a pure-Go binary using `github.com/emiago/sipgo` v1.2.0 for SIP signaling, `net.UDPConn` (stdlib) for RTP, `github.com/gorilla/websocket` v1.5.3 for the outbound WebSocket client, and `log/slog` + `github.com/prometheus/client_golang` for observability. All chosen libraries are pure Go with CGO disabled, enabling a `FROM scratch` Docker image that cuts the image size from ~180MB (Node.js Alpine) to ~8-15MB. The architecture maps cleanly to six internal packages (`config`, `sip`, `rtp`, `ws`, `bridge`, `obs`) with 2-3 goroutines per active call, each owning a distinct I/O responsibility. The `sipgo` library is the only actively maintained pure-Go SIP library with the required features and has no CGO dependencies — there is no viable alternative.

The key risks concentrate in Phase 1 and Phase 2. Phase 1 must use sipgo v1.2.0 from day one (v0.x has a nil-pointer DoS CVE), implement the REGISTER/re-REGISTER cycle correctly with `ClientRequestRegisterBuild` to avoid CSeq double-increment, and enforce `CGO_ENABLED=0` in the Dockerfile. Phase 2 must replicate three sipgate-specific protocol behaviors discovered through v1.0 live testing: PT 113 for DTMF (not the RFC-common PT 101), Record-Route echoing in the 200 OK (required for proxy-routed BYE to reach the caller), and the CANCEL-while-processing race guard that prevents RTP socket and WS client leaks. These are not theoretical — they are documented bugs fixed in v1.0 that must be carried forward explicitly.

---

## Key Findings

### Recommended Stack

Go 1.23+ is required (driven by `prometheus/client_golang` v1.23.2's minimum Go version constraint). All libraries are pure Go with no CGO dependencies, enabling a fully static binary for the `FROM scratch` Docker image. The `sipgo` library at v1.2.0 is the only actively maintained pure-Go SIP library with the required features: digest auth via `DoDigestAuth`, dialog lifecycle management via `DialogServerCache`, and RFC 3261 compliant transaction handling. Alternatives (`ghettovoice/gosip` is abandoned since 2022; pjsip Go bindings require CGO).

For configuration, `kelseyhightower/envconfig` with struct tags provides the same fail-fast validation semantics as v1.0's Zod schema, preserving the same env var names for backward compatibility. `godotenv` is dev-only — production receives env vars from the container environment directly.

**Core technologies:**
- **Go 1.23+:** Language/runtime — goroutines replace the Node.js event loop; `net.UDPConn` replaces dgram; GC is concurrent with sub-1ms pause targets; no drain-queue workarounds needed
- **github.com/emiago/sipgo v1.2.0:** SIP signaling (REGISTER, INVITE, BYE, OPTIONS) — only actively maintained pure-Go SIP library; built-in digest auth; dialog lifecycle management; released February 2025
- **net.UDPConn (stdlib):** RTP audio transport per call — zero dependencies; direct syscall; goroutine-per-socket gives deterministic latency; replaces Node.js dgram 1:1
- **github.com/gorilla/websocket v1.5.3:** WebSocket client to AI backend — 42K importers; battle-tested; sufficient for Twilio Media Streams JSON-over-WS; writePump pattern required
- **github.com/rs/zerolog v1.34.0:** Structured JSON logging — zero-allocation; chainable API; replaces pino 1:1
- **github.com/prometheus/client_golang v1.23.2:** Prometheus metrics at `/metrics` — pure Go; requires Go 1.23+
- **github.com/kelseyhightower/envconfig v1.4.0:** Env var parsing into typed struct — struct tags provide fail-fast validation; same semantics as v1.0 Zod schema; feature-complete
- **FROM scratch Dockerfile:** Final image ~8-15MB vs ~180MB Node.js Alpine; `CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w"`

**Critical conditional:** If `WS_TARGET_URL` uses `wss://` (TLS), switch base image from `FROM scratch` to `gcr.io/distroless/static-debian12:nonroot` for CA certificate support.

**What NOT to use:** `ghettovoice/gosip` (abandoned 2022), `wernerd/GoRTP` (last commit 2014), any CGO-based library, `gorilla/mux` or Gin (overkill for two HTTP endpoints), `global prometheus.DefaultRegisterer` (use `prometheus.NewRegistry()` explicitly).

### Expected Features

This is a rewrite, not a new product. The MVP is full 1:1 feature parity with v1.0 plus two new observability endpoints. There is no "launch with less" — the service must be a drop-in replacement.

**Must have (table stakes — P1, all required for call handling to function):**
- SIP REGISTER on startup with digest auth + automatic re-REGISTER goroutine at 90% of server-specified `Expires`
- Accept inbound SIP INVITE with full lifecycle: 100 Trying → 180 Ringing → 200 OK + SDP → ACK → BYE/CANCEL
- SDP offer parsing (extract remote IP, port, PCMU/PT0) + SDP answer construction (PCMU + telephone-event PT 113)
- RTP socket per call (`net.UDPConn`) with goroutine read loop and RFC 3550 12-byte header encode/decode
- RFC 4733 DTMF via PT 113 (sipgate-specific): End-bit detection, 3-packet deduplication by RTP timestamp
- Per-call gorilla/websocket client with writePump/readPump goroutine pair (enforced by gorilla concurrency rules)
- Twilio Media Streams protocol: `connected` → `start` → `media` → `stop` → `dtmf` event sequence
- WS reconnect loop: exponential backoff (1s/2s/4s cap), 30-second budget, silence injection during reconnect
- `wsReconnecting atomic.Bool` gate suspending RTP-to-WS forwarding during reconnect window
- CallManager: `sync.RWMutex`-protected `map[string]*CallSession` for concurrent call management
- CANCEL race guard: `cancelledCalls sync.Map` + check after each blocking op in INVITE handler
- 200 OK retransmission until ACK (RFC 3261 §13.3.1.4: T1→T2 doubling)
- SIGTERM graceful shutdown: BYE all active calls + UNREGISTER + HTTP server drain via `sync.WaitGroup`
- GET /health: `{"registered":bool,"activeCalls":N}` for Kubernetes probes
- GET /metrics: 5 Prometheus counters/gauges via `promhttp.Handler()`
- Static Go binary Docker image (`FROM scratch` or distroless), `network_mode: host` required

**Should have (v2.x, after production validation):**
- `mark` / `clear` event support — trigger: AI backend uses barge-in/interruption
- SIP OPTIONS keepalive — trigger: silent registration failures observed in production
- `diago` higher-level API evaluation — trigger: RTP layer proves complex to maintain

**Defer (v3+, out of scope per PROJECT.md):**
- Outbound call initiation — different SIP state machine, different milestone
- Multi-codec support (G.722, Opus) — transcoding belongs in WS consumer
- SRTP media encryption — explicitly excluded per PROJECT.md
- Web UI / management dashboard, call recording, PSTN routing logic

### Architecture Approach

The architecture uses six internal Go packages wired by a minimal `cmd/audio-dock/main.go`. All packages are `internal` — this is a single binary, not a library. Each active call runs exactly 3 long-lived goroutines: `rtpReadLoop` (goroutine A, blocked on `UDPConn.ReadFromUDP`), `wsWriteLoop` (goroutine B, drains `writeCh chan []byte` — required to serialize all WS writes), and `wsReadLoop` (goroutine C, blocked on `wsConn.ReadMessage`). A transient goroutine D runs only during WS reconnect windows. The complete goroutine lifecycle is managed by a per-call `context.CancelFunc` and `sync.WaitGroup`, enabling clean termination without timer-handle management. `network_mode: host` is required — Docker port-proxy adds ~10ms UDP jitter to 20ms RTP frames.

**Major components:**
1. **internal/config** — env var struct with `kelseyhightower/envconfig`; fail-fast validation including cross-field (`RTP_PORT_MIN < RTP_PORT_MAX`); loaded once at startup; singleton exported value
2. **internal/sip** — sipgo UserAgent + Server + Client; REGISTER with digest auth (`DoDigestAuth`); re-register goroutine; `sdp.go` (parse/build, pure functions); `headers.go` (extract/build SIP headers); `agent.go` (network plumbing)
3. **internal/rtp** — one `net.UDPConn` per call; atomic port counter for allocation with `EADDRINUSE` retry; RFC 3550 12-byte header parse/build; PT=0 PCMU + PT=113 DTMF with End-bit deduplication; `sync.Pool` for MTU-sized read buffers
4. **internal/ws** — gorilla/websocket Dial with 2s timeout; `writeCh chan []byte` + single `wsWriteLoop` goroutine; Twilio Media Streams JSON types (`protocol.go`); disconnect notification channel
5. **internal/bridge** — `CallManager` with `sync.RWMutex`-protected session map; `handleInvite` orchestration goroutine (sequential: 100 Trying → RTP bind → CANCEL check → WS Dial → 200 OK → start goroutines); `reconnect.go` (flat `for`+`select` loop with backoff); silence ticker during reconnect; CANCEL + INVITE race guard via `sync.Map`
6. **internal/obs** — `net/http` mux; GET /health + GET /metrics; reads from bridge via `StatsProvider` interface (atomic reads, no lock needed)

**Data flow (inbound audio):** sipgate UDP RTP → `rtpReadLoop` → parse header (PT=0/113) → audioCh or dtmfCh → bridge wiring → `ws.SendAudio()` → `writeCh` → `wsWriteLoop` → AI backend WS

**Data flow (outbound audio):** AI backend WS → `wsReadLoop` → JSON unmarshal → base64 decode → 160-byte chunk → `rtp.SendAudio()` → build 12-byte header → `UDPConn.WriteToUDP` → sipgate media server → caller handset

### Critical Pitfalls

The research identified 10 pitfalls. The top 5 by severity and phase impact:

1. **sipgo dialog lifecycle: missing `defer dlg.Close()` and `<-dlg.Context().Done()`** — Without `defer dlg.Close()`, every completed call leaks memory from `DialogServerCache` indefinitely. Without `OnAck`/`OnBye` registered via `dialogSrv.ReadAck`/`ReadBye`, the dialog context never closes and goroutines block forever. Address in Phase 1 — must be established correctly from day one.

2. **gorilla/websocket concurrent write panic** — `WriteMessage` called from multiple goroutines simultaneously panics the entire process with `panic: concurrent write to websocket connection`, killing all active calls. All WS writes must route through a single buffered channel drained by one `wsWriteLoop` goroutine. Address in Phase 2 before wiring any RTP callbacks.

3. **CGO dependency breaks `FROM scratch`** — Any transitive CGO dependency produces a dynamically linked binary that exits immediately in the scratch container with no useful error output (`exec format error`). Enforce `CGO_ENABLED=0` in the Dockerfile builder stage and add `ldd audio-dock | grep "not a dynamic executable"` as a CI gate. Address in Phase 1.

4. **sipgo `ClientRequestRegisterBuild` omission — CSeq double-increment** — Omitting this option from `TransactionRequest` causes every re-REGISTER (~90 seconds) to fail with 400/401 from sipgate's registrar. Initial registration works; re-registration fails silently. Address in Phase 1 — must be correct before call acceptance is built on top.

5. **CANCEL race + Record-Route missing from 200 OK** — Two related Phase 2 pitfalls from v1.0 live testing. CANCEL arriving during the WS-connection wait (up to 2 seconds) causes a leaked RTP socket and WS client if not tracked in a `cancelledCalls sync.Map`. Record-Route headers not echoed in the 200 OK causes BYE to bypass sipgate's proxy chain, resulting in 403 rejections and calls never terminating cleanly. Both must be implemented before live sipgate testing.

---

## Implications for Roadmap

The research establishes a clear, dependency-driven phase structure. The pitfall-to-phase mapping from PITFALLS.md aligns exactly with the build order from ARCHITECTURE.md. Each phase unlocks the next.

### Phase 1: Foundation — Module, Config, Docker, SIP Registration

**Rationale:** Everything depends on config. The Go module, env var struct, and Dockerfile must be correct before any protocol work begins. SIP registration is the prerequisite for call acceptance — sipgate will not route calls to an unregistered endpoint. Three critical pitfalls (CGO binary, sipgo v0.x DoS, CSeq double-increment) must be addressed here or they corrupt all later work.

**Delivers:**
- Go module initialized (`github.com/sipgate/audio-dock`), all dependencies pinned at researched versions in `go.mod`
- `internal/config`: typed env struct with `kelseyhightower/envconfig`, fail-fast validation, cross-field checks
- `internal/sip/agent.go`: sipgo UserAgent + Client; REGISTER with digest auth (`DoDigestAuth`); re-register goroutine at 90% of server `Expires`; `OnAck`/`OnBye` handler registration pattern established
- Multi-stage Dockerfile (`golang:1.23-alpine` → `scratch`); `CGO_ENABLED=0`; `ldd` verification step in CI
- `SIGTERM` handling via `signal.NotifyContext` in `main.go`; root context propagated to all components

**Addresses:** SIP REGISTER, automatic re-REGISTER, Docker static binary, env config
**Avoids:** CGO in scratch container (P3), sipgo v0.x nil-pointer DoS (P8), CSeq double-increment on re-REGISTER (P6), dialog leak (P1 — lifecycle pattern established)
**Research flag:** SKIP — all patterns are HIGH-confidence with official documentation. sipgo REGISTER + `DoDigestAuth` verified against pkg.go.dev v1.2.0 API.

---

### Phase 2: Core Bridge — SIP Call Lifecycle, RTP, WebSocket, Twilio Protocol

**Rationale:** This is the largest and most complex phase. The feature dependency graph requires building in a strict order: bind RTP port before SDP answer, connect WS before 200 OK, start goroutines after ACK. All v1.0 production bugs that must be carried forward live here (PT 113 DTMF, Record-Route, CANCEL race). The gorilla/websocket concurrent write constraint requires the `writePump` architecture to be established before any other code writes to WS.

**Delivers:**
- `internal/sip/sdp.go`: `parseSdpOffer` + `buildSdpAnswer` with PT 113 telephone-event (pure functions, unit testable)
- `internal/sip/headers.go`: header extraction, Record-Route echoing, `buildResponse`, `buildBye`
- `internal/rtp/port.go`: atomic port allocator with `EADDRINUSE` retry; `conn.SetReadBuffer(1MB)` at bind
- `internal/rtp/handler.go`: `net.UDPConn` read loop with `SetReadDeadline` for ctx check; RFC 3550 12-byte header parse/build; PT=0 PCMU; PT=113 DTMF with End-bit + timestamp deduplication; `sync.Pool` for 1500-byte read buffers (GC hot path optimization)
- `internal/ws/protocol.go`: Twilio Media Streams JSON types (connected/start/media/stop/dtmf)
- `internal/ws/client.go`: gorilla/websocket Dial (2s timeout); `writeCh chan []byte` (buffer=128); `wsWriteLoop` goroutine; `wsReadLoop` goroutine; disconnect channel
- `internal/bridge/session.go`: `CallSession` struct with `context.CancelFunc` + `sync.WaitGroup` tracking all goroutines
- `internal/bridge/call_manager.go`: `handleInvite` orchestration (100 Trying → RTP bind → CANCEL check → WS Dial → 200 OK with Record-Route → ACK wait → start goroutines); `cancelledCalls sync.Map`; `pendingInvites sync.Map`; 200 OK retransmit T1→T2 goroutine

**Addresses:** All P1 SIP, RTP, WebSocket, and Twilio features from the MVP checklist
**Avoids:** gorilla/websocket concurrent write panic (P2), DTMF PT 113 wrong (P10), Record-Route missing (P9), CANCEL race (P7), UDP read goroutine leak (P5), GC hot path allocations (P4)
**Research flag:** CONSIDER research for sipgo `DialogServerCache.ReadAck`/`ReadBye` API behavior in v1.2.0. The exact goroutine coordination pattern for concurrent CANCEL + INVITE handling has subtle edge cases. Prototype and verify against v1.2.0 source before committing to the full `handleInvite` flow. Fallback: build raw SIP responses (as v1.0 did) — a known-viable alternative.

---

### Phase 3: Resilience — WS Reconnect Loop, Silence Injection, Graceful Shutdown

**Rationale:** The WS reconnect feature is complex enough to be its own phase. It introduces a transient goroutine D (silence ticker), a cross-goroutine state flag (`wsReconnecting atomic.Bool`), and the "BYE arrives mid-reconnect" race. These interactions are only testable after Phase 2 is complete and validated.

**Delivers:**
- `internal/bridge/reconnect.go`: WS reconnect goroutine with flat `for`+`select` loop; exponential backoff (1s/2s/4s cap, 30s budget); interruptible sleep via `select { case <-time.After(delay): case <-ctx.Done(): }`; `cancelCtx.Done()` check at loop top (BYE race guard); silence ticker stopped on reconnect success
- Silence ticker goroutine: `time.NewTicker(20*time.Millisecond)`; injects 160-byte `0xFF` RTP packets; runs only during reconnect window; context-cancelled on success or call end
- `wsReconnecting atomic.Bool` gate: suspends RTP-to-WS forwarding; resumed atomically on reconnect success
- `CallManager.TerminateAll(ctx)`: parallel BYE across all sessions with `sync.WaitGroup`; configurable drain deadline (5s); UNREGISTER after all sessions closed
- Full SIGTERM → BYE all → UNREGISTER → HTTP drain sequence validated end-to-end

**Addresses:** WS reconnect + backoff, silence injection during reconnect, graceful shutdown, SIGTERM
**Avoids:** Shutdown race on reconnect sleep (ctx.Done() checked at each loop iteration), goroutine leak after BYE during reconnect (P5), GC jitter during reconnect (P4 — silence packets use pre-built static byte slice)
**Research flag:** SKIP — flat `for`+`select` reconnect goroutine with `time.NewTicker` and context cancellation is textbook Go. Patterns are HIGH-confidence from official Go documentation.

---

### Phase 4: Observability — Metrics, Health, Prometheus, GC Tuning

**Rationale:** Observability must be built after the core bridge is functional so the metrics being measured reflect real behavior. Prometheus counters (RTP packets, WS reconnects, active calls) must instrument code that already exists. GC tuning (`GOGC=200 GOMEMLIMIT=256MiB`) should be applied after profiling actual hot paths from Phases 2/3.

**Delivers:**
- `internal/obs/server.go`: `net/http` mux; GET /health with `atomic.Bool registered` + `CallManager.count()`; GET /metrics with `promhttp.Handler()`
- 5 Prometheus metrics: `audio_dock_active_calls` (gauge), `audio_dock_calls_total`, `audio_dock_ws_reconnect_attempts_total`, `audio_dock_rtp_packets_rx_total`, `audio_dock_rtp_packets_tx_total`
- `runtime.NumGoroutine()` exposed as Prometheus gauge for goroutine leak detection in production
- `StatsProvider` interface on bridge — obs package reads atomic counters with no import cycle back to bridge
- `GOGC=200 GOMEMLIMIT=256MiB` added to Dockerfile and docker-compose after profiling confirms allocation rate
- pprof bound to `127.0.0.1` only; never on `0.0.0.0`

**Addresses:** GET /health, GET /metrics, GC tuning
**Avoids:** GC-induced RTP jitter in production (P4 — measured and tuned), pprof exposed publicly (security hardening)
**Research flag:** SKIP — `prometheus/client_golang` with `promhttp` and `net/http` ServeMux patterns are HIGH-confidence with official documentation.

---

### Phase 5: Integration Validation — Live sipgate Testing

**Rationale:** Several behaviors can only be validated with a live sipgate trunk: Record-Route routing through the proxy chain (visible via Wireshark), DTMF PT 113 on real PSTN calls, re-REGISTER under real network conditions, and BYE routing correctness. This phase is dedicated validation — no new feature code. The "Looks Done But Isn't" checklist from PITFALLS.md drives the acceptance criteria.

**Delivers:**
- Static binary verified: `ldd ./audio-dock` → `not a dynamic executable`; CI enforces this gate
- Live PSTN call: DTMF pressed → PT 113 packet observed → WS `dtmf` event confirmed
- Live call: BYE routes through `sip.sipgate.de` proxy (Wireshark capture), not directly to caller IP
- 10-sequential-call goroutine/FD leak test: count returns to pre-test baseline
- Fast-cancel test (<500ms after dial): no leaked RTP socket or WS client; 487 sent correctly
- 200 OK retransmit test: drop ACK via iptables; verify 500ms/1s/2s/4s retransmit schedule
- Soak test: 5 concurrent calls × 60 minutes; p99 RTP inter-packet timing < 2ms in Prometheus histogram
- Concurrent write test: 2+ calls + silence injection; zero panics over 1-hour soak
- Finalized Dockerfile and docker-compose with `network_mode: host`, `GOGC=200`, `GOMEMLIMIT=256MiB`

**Validates:** All 10 pitfalls from PITFALLS.md "Looks Done But Isn't" checklist
**Research flag:** CONSIDER research for sipgate SBC behavior under edge conditions (overlapping REGISTER refreshes, malformed SIP from internet scanners). MEDIUM confidence — third-party interop documentation only.

---

### Phase Ordering Rationale

- **Config before everything:** All packages depend on config; parsing errors must be caught before any network socket is opened.
- **RTP port before SDP, WS before 200 OK:** These are hard sequential dependencies within each call setup, enforced as a strict sequence in `handleInvite`.
- **Reconnect after core bridge:** The reconnect goroutine interacts with the RTP handler, WS client, and session state from Phase 2. Building it before those components exist is structurally impossible.
- **Observability after core bridge:** Metrics without real behavior to measure are meaningless. GC tuning requires profiling actual hot paths.
- **Integration testing last:** Record-Route proxy routing and PT 113 DTMF can only surface with real sipgate traffic. These cannot be unit tested reliably.

### Research Flags

Phases needing `/gsd:research-phase` during planning:
- **Phase 2:** sipgo `DialogServerCache` API — `ReadAck`/`ReadBye` registration pattern is verified in pkg.go.dev but not confirmed in a production Go SIP bridge. Prototype before committing to the full `handleInvite` architecture. If it does not behave as expected, the fallback (raw SIP responses) is documented and viable.
- **Phase 5:** sipgate SBC behavior under edge conditions (rapid re-registration, overlapping INVITE retransmissions, internet scanner traffic) is MEDIUM confidence based on third-party interop docs only.

Phases with well-documented patterns (skip research):
- **Phase 1:** Go module, `envconfig`, `CGO_ENABLED=0`, sipgo REGISTER with `DoDigestAuth` — all HIGH confidence, official documentation.
- **Phase 3:** Reconnect goroutine with `for`+`select`, `time.NewTicker`, context cancellation — textbook Go patterns, official Go docs.
- **Phase 4:** `prometheus/client_golang` with `promhttp`, `net/http` ServeMux — HIGH confidence, official docs.

---

## Confidence Assessment

| Area | Confidence | Notes |
|------|------------|-------|
| Stack | HIGH | All library versions sourced from pkg.go.dev and GitHub releases. sipgo v1.2.0 confirmed February 2025. All dependencies pure Go — no CGO risk in the chosen set. Only uncertainty: gorilla/websocket vs coder/websocket (both production-grade; gorilla chosen for stability and community adoption). |
| Features | HIGH | Protocol details (PT 113, Twilio Media Streams JSON payloads, SDP format, RFC 4733 DTMF) sourced from v1.0 production code, which is the authoritative behavioral reference. RFC citations for RTP/SIP are normative. sipgo API for REGISTER is HIGH confidence from official pkg.go.dev docs. |
| Architecture | HIGH (Go patterns) / MEDIUM (sipgo dialog API) | Go concurrency patterns (goroutine, channel, `sync.RWMutex`, context propagation) are HIGH confidence from official Go docs and widely verified production practice. `sipgo.DialogServerCache` and the exact `ReadAck`/`ReadBye` API surface are MEDIUM confidence — verified against pkg.go.dev but not confirmed in a deployed Go SIP bridge. |
| Pitfalls | HIGH | Top pitfalls sourced from: official security advisory (sipgo GHSA-c623-f998-8hhv), official library docs (gorilla concurrent write constraint), normative RFCs (Record-Route §12.1.2, RFC 4733 dedup), official Go GC guide (GOGC/GOMEMLIMIT), and v1.0 production bug history (PT 113, CANCEL race). |

**Overall confidence:** HIGH

### Gaps to Address

- **sipgo `DialogServerCache.ReadAck`/`ReadBye` exact behavior in v1.2.0:** The lifecycle pattern is documented on pkg.go.dev but unconfirmed in a deployed Go SIP bridge. Prototype this interaction before the full `handleInvite` implementation. If behavior diverges from docs, fall back to building raw SIP responses — a documented, viable alternative with known trade-offs (must handle Record-Route and Via echoing manually).

- **diago higher-level API suitability:** `emiago/diago` provides `AudioReader`/`AudioWriter` IO interfaces that could simplify the RTP layer significantly. Flagged as v2.x, not needed for the rewrite. Evaluate if Phase 2 RTP implementation proves complex to maintain. MEDIUM confidence — diago's compatibility with raw PCMU bytes (not decoded PCM) is unverified.

- **`SDP_CONTACT_IP` behavior across deployment topologies:** With `network_mode: host`, the container sees the host IP directly. The `SDP_CONTACT_IP` env var must be set to the host's reachable IP for sipgate to route RTP correctly. This is documented in v1.0 but the exact interaction with bare Docker, docker-compose, and K8s `hostNetwork: true` needs validation in Phase 5.

- **wss:// conditional path for `WS_TARGET_URL`:** If the WS target uses `wss://`, the `FROM scratch` base image must be swapped for `gcr.io/distroless/static-debian12:nonroot` for CA certificates. The research is clear on the solution but this path is not exercised in the v1.0 production deployment. Validate in Phase 5 if TLS WS is required.

---

## Sources

### Primary (HIGH confidence)
- `github.com/emiago/sipgo` releases — v1.2.0 February 2025; API on pkg.go.dev: UserAgent, Server, Client, DialogServerCache, DoDigestAuth, ClientRequestRegisterBuild
- sipgo security advisory GHSA-c623-f998-8hhv — nil pointer DoS on missing To header; fixed in v1.0.0
- `github.com/gorilla/websocket` pkg.go.dev — concurrent writer constraint explicitly documented; writePump pattern from official chat example; v1.5.3 June 2024
- `github.com/prometheus/client_golang` releases — v1.23.2; requires Go 1.23+; pure Go
- Go stdlib: `net.UDPConn`, `log/slog`, `os/signal.NotifyContext`, `sync.RWMutex`, `sync.WaitGroup`, `sync/atomic` — official Go docs
- Go GC Guide (official) — GOGC, GOMEMLIMIT, STW behavior, thrashing risk
- RFC 3261 §12.1.2 — Record-Route headers MUST be echoed verbatim in 200 OK
- RFC 4733 §2.5 — Telephone-event redundant End packet deduplication (3 packets, same timestamp)
- Twilio Media Streams WebSocket Messages — official Twilio docs: all event types, JSON payloads, audio format
- audio-dock v1.0 source (`src/rtp/rtpHandler.ts`, `src/bridge/callManager.ts`, `src/ws/wsClient.ts`, `src/sip/sdp.ts`) — authoritative reference for sipgate-specific protocol details: PT 113 DTMF, CANCEL race guard, Record-Route fix, makeDrain elimination rationale
- Go 1.22 routing enhancements — method routing in stdlib ServeMux (official Go blog)
- `GoogleContainerTools/distroless` — `static-debian12` for static Go binaries; CA cert handling

### Secondary (MEDIUM confidence)
- sipgo GitHub issue #59 — dialog terminating mid-call at 64*T1; directly describes the lifecycle problem
- `emiago/diago` pkg.go.dev — AudioReader/AudioWriter higher-level API over sipgo
- sipgate trunking: registrar `sipconnect.sipgate.de:5060` UDP — AudioCodes LiveHub interop doc (third-party)
- Go graceful shutdown patterns — VictoriaMetrics blog; multiple community sources agree on pattern
- Go standard project layout — `golang-standards/project-layout`; `internal/` usage is official Go spec
- `coder/websocket` (formerly `nhooyr.io/websocket`) — goroutine-safe alternative to gorilla; modern context-aware API

### Tertiary (LOW confidence, needs validation)
- gorilla/websocket vs coder/websocket consensus — Go Forum 2025 thread; community discussion only

---

*Research completed: 2026-03-03*
*Ready for roadmap: yes*
