# Project Research Summary

**Project:** audio-dock v2.1
**Domain:** SIP-to-WebSocket audio bridge — Twilio Media Streams mark/clear protocol + SIP OPTIONS keepalive
**Researched:** 2026-03-05
**Confidence:** HIGH

## Executive Summary

audio-dock v2.1 adds three protocol-completion features to an already-shipped Go + Node.js SIP media gateway. The v2.0 codebase is complete and stable; this milestone is purely additive logic with zero new dependencies for either implementation. The Go implementation is the primary deliverable; the Node.js implementation is a reference that follows the same semantic patterns under a different concurrency model. All three features are fully specified in official sources and map cleanly onto the existing goroutine architecture without requiring new files or structural reorganization.

The recommended implementation strategy is sequential: first complete the Go mark/clear changes in the bridge layer, then add SIP OPTIONS keepalive in the SIP/config layer, then mirror both in Node.js. This ordering minimizes blast radius by keeping bridge-layer goroutine changes isolated before SIP-layer changes add more surface area. The single highest-risk change is the `packetQueue` type change from `chan []byte` to `chan outboundFrame` — it touches the most call sites and should be a standalone commit verified against existing tests before the logic additions follow.

The critical risks are all concurrency-related. The sole-writer invariant on the WebSocket connection must be respected for mark echoes: mark echo signals must route through `markEchoQueue` to `wsPacer` and must never be written from `wsToRTP` or `rtpPacer`. The RTP pacer must never be stopped during a clear flush — drain the queue and let the pacer fall back to silence naturally; stopping it causes an RTP timestamp discontinuity and an audible click. Concurrent `doRegister` calls from both `reregisterLoop` and the new `optionsKeepaliveLoop` must be serialized with a mutex to prevent stale-CSeq 400 rejections from sipgate. All three risks have concrete, low-cost mitigations fully documented in the research files.

## Key Findings

### Recommended Stack

The v2.0 stack is locked and sufficient for all v2.1 features. No changes to `go.mod` or `package.json` are required. See [STACK.md](.planning/research/STACK.md) for full version tables and API confirmations.

**Core technologies (unchanged, carrying forward):**
- `github.com/emiago/sipgo v1.2.0` (Go): SIP client for REGISTER and the new outbound OPTIONS — `client.Do()` with `siplib.OPTIONS` is confirmed available since v1.0.0 on pkg.go.dev
- `github.com/gobwas/ws v1.3.2` (Go): WebSocket client — mark/clear are JSON text frames handled by the existing `writeJSON`/`readWSMessage` helpers; no API changes needed
- `encoding/json`, `time`, `sync` (Go stdlib): JSON structs for new mark/clear event types, `time.NewTicker` for OPTIONS interval, Go channels for `markEchoQueue` and `clearSignal`
- `ws ^8.19.0` (Node.js): `ws.send()` already used for all outbound events; mark echo is a new `JSON.stringify` call in the existing message handler
- `dgram` (Node.js stdlib): `socket.send()` already used for REGISTER; identical call path for OPTIONS

**New dependency count: zero for both Go and Node.js.**

### Expected Features

All three v2.1 features are P1 — required for the milestone and for AI barge-in to function correctly. See [FEATURES.md](.planning/research/FEATURES.md) for full protocol reference, edge case analysis, and the complete MVP definition.

**Must have (table stakes for v2.1):**
- `mark` event: receive mark from WS server, echo it back after all preceding audio frames have drained from `packetQueue` — required for AI TTS backends to know when audio finished playing; also echo immediately if queue is empty at mark receipt
- `clear` event: receive clear from WS server, drain `packetQueue` immediately, and echo all pending mark names — required for barge-in (caller interrupts TTS mid-stream); Twilio spec: "empties the audio buffer and sends back mark messages matching any remaining mark messages"
- SIP OPTIONS keepalive: periodic out-of-dialog OPTIONS to sipgate every 30s, 5s timeout per probe; on timeout/5xx/404 trigger immediate re-registration — detects silent registration loss currently invisible to the existing `reregisterLoop` (90s worst-case blind window)

**Should have (add post-v2.1):**
- `sip_options_failures_total` Prometheus counter — ops alerting on registration health without waiting for missed calls
- Configurable `SIP_OPTIONS_INTERVAL_S` env var — tuning for deployment-specific trunk behavior (default 30s is correct for sipgate but should be adjustable)

**Defer to v2.x+:**
- Sequence number continuity for mark echoes across WS reconnects — `mark.name` is sufficient for correlation; cross-reconnect tracking adds state with no protocol benefit
- In-dialog OPTIONS for session refresh (RFC 4028) — different mechanism, different use case than trunk monitoring

### Architecture Approach

The v2.1 changes integrate into the existing goroutine model by extending existing goroutines with new queue types and select cases — no new per-call goroutines are added. The bridge changes involve three affected goroutines: `wsToRTP` gains two new event cases, `rtpPacer` inspects a clear signal and routes mark sentinels, and `wsPacer` gains one new select case for mark echoes. The SIP keepalive adds one service-scoped goroutine (`optionsKeepaliveLoop`) alongside the existing `reregisterLoop`, both owned by `Registrar`. See [ARCHITECTURE.md](.planning/research/ARCHITECTURE.md) for full goroutine maps, data flow diagrams, and anti-pattern analysis.

**The critical architectural constraint for all additions:** `wsPacer` is the sole goroutine allowed to write to `wsConn`. This invariant is documented in `session.go` and enforced structurally. Any new WS-direction write (including mark echoes) must be routed through `wsPacer` via a channel, never dispatched from `wsToRTP`, `rtpPacer`, or any other goroutine. `gobwas/ws` is not safe for concurrent writes.

**Major components affected:**

1. `go/internal/bridge/session.go` (~50 lines changed) — new `outboundFrame` struct replaces `chan []byte` element type; adds `markEchoQueue chan string` (capacity 10) and `clearSignal chan struct{}` (capacity 1) to `CallSession`; extends `wsToRTP`, `rtpPacer`, and `wsPacer` with new event handling
2. `go/internal/bridge/ws.go` (~20 lines added) — new `MarkEvent`, `MarkBody` JSON types and `sendMarkEcho()` function
3. `go/internal/sip/registrar.go` (~50 lines added) — new `optionsKeepaliveLoop` and `sendOptions` methods; `Register()` starts the new goroutine alongside `reregisterLoop`; `sync.Mutex` added to serialize concurrent `doRegister` calls
4. `go/internal/config/config.go` (~3 lines) — new `SIPOptionsInterval int` env var with default 30
5. `node/src/ws/wsClient.ts` (~30 lines) — extend `makeDrain` element type to a tagged union; handle `mark`/`clear` events in `ws.on('message')` handler; extend `WsClient` interface with `onMark`, `sendMark`, `sendClear`
6. `node/src/sip/userAgent.ts` (~30 lines) — `buildOptions()` helper function; keepalive `setInterval` started after registration success

**New Go files: none. New Node.js files: none.**

### Critical Pitfalls

Full catalogue with "Looks Done But Isn't" verification checklist in [PITFALLS.md](.planning/research/PITFALLS.md).

1. **Mark echo sent from the wrong goroutine — violates sole-writer invariant** — `wsToRTP` receives the inbound mark event; writing the echo response directly from `wsToRTP` causes a concurrent write to `wsConn`, which `gobwas/ws` does not support. Prevention: route mark names through `markEchoQueue chan string`; `wsPacer` (the sole WS writer) sends all echoes. Verify: `go test -race` must pass with zero concurrent-write reports.

2. **Clear stops the RTP pacer, causing RTP timestamp discontinuity and audible click** — the correct behavior is to drain `packetQueue` only; `rtpPacer` must keep running at 20ms intervals, falling back to silence frames when the queue is empty. Prevention: signal `rtpPacer` via `clearSignal chan struct{}` so it drains the queue at its own next tick; never stop or restart the pacer. Verify: unit test that RTP sequence numbers are monotonically increasing through a clear+new-audio cycle.

3. **OPTIONS 401 response treated as registration loss, triggering a re-registration storm** — some SIP servers return 401 meaning "auth required for this probe" (server is alive), not "registration lost". Prevention: define a response-code table before implementation — 401/407 means "server alive"; only 408/timeout, transport error, 404, and 5xx should trigger re-registration. Verify: unit test with mocked 401 confirms `r.registered` stays `true` and no REGISTER is sent.

4. **Clear does not echo pending marks, leaving the WS consumer stuck waiting** — when `packetQueue` is flushed, any mark sentinels already in the queue must also be echoed immediately (marks were queued but never reached playout). Prevention: use `outboundFrame` sentinel type so marks travel in-order with audio through `packetQueue`; on clear, drain all frames and route mark-type frames through `markEchoQueue` to `wsPacer`. Verify: integration test with mark → audio → clear sequence confirms WS server receives the mark echo.

5. **OPTIONS keepalive goroutine not tied to root context — goroutine leak in tests and on shutdown** — `time.NewTicker` goroutines using `context.Background()` instead of the app root context will not stop on SIGTERM or test teardown. Prevention: pass the same `ctx` as `reregisterLoop`; always include `defer ticker.Stop()`. Verify: goroutine-count assertion in registrar test; count is stable across multiple `Register()` + teardown cycles.

## Implications for Roadmap

Three phases are sufficient for this milestone. All are purely additive to the existing codebase — no phase requires structural refactoring or file creation. The ordering is driven by: (a) the `packetQueue` type change being the most disruptive single change and needing isolation; (b) SIP/config changes being independent of bridge changes and safer to review separately; (c) Node.js being a secondary deliverable that benefits from using the verified Go implementation as a behavioral reference.

### Phase 1: Go Bridge — mark/clear Protocol

**Rationale:** The `packetQueue` type change (`chan []byte` to `chan outboundFrame`) touches every existing producer and consumer of the queue and should be a standalone commit verified against all existing tests before any mark/clear logic is added. This phase has the highest code-level risk (most existing call sites modified) but the lowest protocol risk (all semantics are fully specified in official Twilio docs). The `markEchoQueue` channel must be established and wired to `wsPacer` before any mark handling is added to `wsToRTP` — this ordering enforces the sole-writer invariant structurally.

**Delivers:** Full Twilio Media Streams mark/clear protocol support in the Go implementation; barge-in works end-to-end on the Go path.

**Addresses:** `mark` event (echo via `markEchoQueue` after `packetQueue` drains), `clear` event (signal `rtpPacer` via `clearSignal`, echo pending marks)

**Avoids:**
- Pitfall 1: `markEchoQueue` established before mark logic in `wsToRTP` — structurally prevents sole-writer violation
- Pitfall 2: `clearSignal` approach (rtpPacer drains at own tick) — pacer never stopped
- Pitfall 4: `outboundFrame` sentinel design — marks travel in-order with audio; drain on clear echoes them automatically

**Build order within phase:**
1. Add `outboundFrame` type; change `packetQueue chan []byte` to `chan outboundFrame`; update all existing producers and consumers; verify existing tests pass (standalone commit)
2. Add `markEchoQueue chan string` and `clearSignal chan struct{}` to `CallSession`; initialize in `run()`
3. Add `MarkEvent`, `MarkBody` structs and `sendMarkEcho()` to `ws.go`
4. Extend `wsPacer` select with `case markName := <-s.markEchoQueue`
5. Extend `wsToRTP` switch with `case "mark"` (enqueue `outboundFrame{mark: name}` to `packetQueue`)
6. Extend `rtpPacer` to route mark-type frames to `markEchoQueue` and check `clearSignal` each tick

### Phase 2: Go SIP — OPTIONS Keepalive

**Rationale:** Completely independent of Phase 1 bridge changes — no shared data structures or goroutines. Keeping it as a separate phase makes rollback straightforward if a sipgo API issue surfaces. The concurrent `doRegister` mutex (Pitfall 9) must be added to `Registrar` before wiring `optionsKeepaliveLoop`, because the very act of starting the new goroutine introduces the concurrent-call scenario.

**Delivers:** Silent registration loss detected within one OPTIONS interval (30s worst case) and re-registration triggered automatically. Reduces the current 90s worst-case missed-call window to 30s.

**Addresses:** SIP OPTIONS keepalive with 30s probe interval, 5s per-probe timeout, and immediate re-registration on 408/5xx/404 response

**Avoids:**
- Pitfall 3: response-code table defined in `sendOptions` before implementation — 401/407 treated as "server alive"
- Pitfall 5: goroutine tied to root `ctx`; `defer ticker.Stop()` mandatory
- Pitfall 7: `SIP_OPTIONS_INTERVAL_S` env var with 30s default and minimum 10s validation
- Pitfall 8: Request-URI set to registrar host (`sip.sipgate.de:5060`), not the AoR
- Pitfall 9: `sync.Mutex` on `Registrar` serializes concurrent `doRegister` calls

**Build order within phase:**
1. Add `sync.Mutex` to `Registrar` struct; update `doRegister` to acquire it for the duration of the REGISTER transaction
2. Add `SIPOptionsInterval int` to `config.Config` with `env:"SIP_OPTIONS_INTERVAL" default:"30"`
3. Add `optionsInterval time.Duration` to `Registrar` struct; update `NewRegistrar` to accept and store it
4. Implement `sendOptions(ctx)` with the correct response-code table
5. Implement `optionsKeepaliveLoop(ctx)` with `time.NewTicker` and `defer ticker.Stop()`
6. Start `optionsKeepaliveLoop` from `Register()` alongside `reregisterLoop`

### Phase 3: Node.js Equivalents

**Rationale:** Node.js is a reference implementation; Go is the primary deliverable. The Node.js changes are smaller in scope (single-threaded event loop eliminates the sole-writer and concurrent-drain concerns) and follow the semantic patterns established in Phases 1 and 2. Doing this last means the verified Go implementation serves as a behavioral reference during development.

**Delivers:** Parity between Go and Node.js implementations for all three v2.1 features.

**Addresses:** mark/clear in `wsClient.ts`, OPTIONS keepalive in `userAgent.ts`

**Avoids:**
- Pitfall 10: `WsClient` interface extended with `onMark`/`sendMark`/`sendClear` before callManager changes; raw `ws` object never exposed to callManager
- Node.js analogue of Pitfall 4: mark sentinel type in `makeDrain` queue ensures in-order echo

**Build order within phase:**
1. Extend `makeDrain` element type from `Buffer` to `{type:'audio',payload:Buffer}|{type:'mark',name:string}`; handle mark sentinel in the send callback
2. Handle `mark` and `clear` events in `ws.on('message')` inside `buildClient()`; echo mark immediately if queue is empty, otherwise enqueue sentinel; drain on clear
3. Extend `WsClient` interface with `onMark`, `sendMark`, `sendClear`; ensure callManager uses the interface, not the raw socket
4. Add `buildOptions()` helper in `userAgent.ts` using the same raw SIP string-builder pattern as `buildRegister()`
5. Start keepalive `setInterval` after initial registration 200 OK; handle timeout/error with `sendRegister()` re-registration

### Phase Ordering Rationale

- Phase 1 before Phase 2: bridge layer and SIP layer are independent; the `outboundFrame` type change is the most disruptive change and benefits from an isolated review window
- Phase 2 before Phase 3: Go behavior is the authoritative reference; Node.js ports are simpler when there is a verified implementation to follow
- No phase requires new files, new packages, or cross-package refactoring — all changes fit within existing files, minimizing merge conflict risk
- Each phase can be tested independently: Phase 1 with Go unit tests (`go test -race`), Phase 2 with registrar unit tests and a mock SIP server, Phase 3 with Node.js unit tests

### Research Flags

**No phases in this milestone require `/gsd:research-phase` during planning.** All protocol semantics, library APIs, and architectural integration points are already resolved in the four research files.

Phases with well-documented patterns (skip additional research):
- **Phase 1 (Go Bridge mark/clear):** Twilio mark/clear protocol fully specified in official docs; integration patterns derived from existing `dtmfQueue`/`wsSignal` analogues in the v2.0 source — HIGH confidence throughout
- **Phase 2 (Go SIP OPTIONS):** RFC 3261 is normative; `sipgo client.Do()` API confirmed from pkg.go.dev; `optionsKeepaliveLoop` is a direct analogue of the existing `reregisterLoop` pattern — HIGH confidence
- **Phase 3 (Node.js):** Semantic behavior fully defined by Phases 1 and 2; Node.js concurrency model is simpler than Go's; no new protocol research needed

## Confidence Assessment

| Area | Confidence | Notes |
|------|------------|-------|
| Stack | HIGH | Zero new dependencies confirmed by direct inspection of v2.0 source and library API docs; all required capabilities verified to exist in currently pinned versions |
| Features | HIGH | Twilio mark/clear schemas and timing semantics verified against official Twilio docs; SIP OPTIONS pattern verified against RFC 3261 and IETF draft; edge cases from official docs and community report consistent with spec |
| Architecture | HIGH | Goroutine model verified by direct read of v2.0 source (`session.go`, `registrar.go`, `handler.go`); sole-writer invariant documented in source; integration patterns are direct analogues of existing `dtmfQueue` and `wsSignal` patterns |
| Pitfalls | HIGH (protocol) / MEDIUM (sipgo concurrency) | Protocol pitfalls from official Twilio docs and RFC; sipgo concurrent `client.Do()` thread-safety not explicitly documented — the chosen mitigation (mutex on `doRegister`) eliminates the dependency on undocumented library behavior |

**Overall confidence: HIGH**

### Gaps to Address

- **sipgo concurrent `client.Do()` thread-safety:** Not explicitly documented in the sipgo library. The `sync.Mutex` on `Registrar.doRegister` is mandatory (not optional) for this reason — it eliminates the dependency on undocumented sipgo concurrency guarantees. Treat the mutex as a correctness requirement, not a performance choice.

- **sipgate OPTIONS auth behavior:** Unknown whether sipgate requires digest auth on out-of-dialog OPTIONS probes. The response-code table (401/407 = "server alive, not a failure") handles this safely: if sipgate returns 401 on every probe, the keepalive still provides liveness confirmation without triggering spurious re-registration. The only failure scenario would be sipgate returning 401 AND silently dropping registrations simultaneously — considered unlikely. Monitor in production and add `DoDigestAuth` handling for OPTIONS if 401 is observed alongside actual registration loss.

- **Node.js `makeDrain` type extension:** The existing queue is a `Buffer[]`. The tagged union extension is a local change within the `makeDrain` closure; TypeScript's structural typing will surface any unconverted call sites at compile time as type errors, providing a complete safety net at no runtime cost.

## Sources

### Primary (HIGH confidence)
- [Twilio Media Streams WebSocket Messages](https://www.twilio.com/docs/voice/media-streams/websocket-messages) — mark and clear event schemas, echo timing semantics, clear-then-echo-pending-marks requirement
- [RFC 3261 §11](https://www.rfc-editor.org/rfc/rfc3261#section-11) — SIP OPTIONS method semantics, out-of-dialog use for capability probing
- [RFC 6223](https://www.rfc-editor.org/rfc/rfc6223) — Indication of Support for Keep-Alive in SIP
- [emiago/sipgo pkg.go.dev](https://pkg.go.dev/github.com/emiago/sipgo) — `Client.Do()`, `ClientRequestBuild`, `siplib.OPTIONS` method type confirmed; v1.2.0 confirmed latest (Feb 7, 2025)
- audio-dock v2.0 source: `go/internal/bridge/session.go`, `go/internal/bridge/ws.go` — goroutine model, sole-writer invariant, `dtmfQueue` channel pattern as direct model for `markEchoQueue`
- audio-dock v2.0 source: `go/internal/sip/registrar.go`, `go/internal/sip/handler.go` — `reregisterLoop` pattern, `client.Do()` usage, existing inbound OPTIONS handler
- audio-dock Node.js source: `node/src/ws/wsClient.ts` — mark/clear gap documented at line 259; `WsClient` interface structure
- audio-dock Node.js source: `node/src/sip/userAgent.ts` — OPTIONS inbound handler at lines 236-254; `dgram` socket and SIP string-builder pattern already present

### Secondary (MEDIUM confidence)
- [emiago/sipgo client.go](https://github.com/emiago/sipgo/blob/main/client.go) — `Do()` implementation, CSeq auto-increment, header auto-population behavior
- [IETF draft-jones-sip-options-ping-02](https://datatracker.ietf.org/doc/html/draft-jones-sip-options-ping-02) — OPTIONS ping interval policy, failure handling patterns
- [Cisco CUBE SIP Out-of-Dialog OPTIONS Ping](https://www.cisco.com/c/en/us/td/docs/ios-xml/ios/voice/cube_sipsip/configuration/15-mt/cube-sipsip-15-mt-book/voi-out-of-dialog.html) — 30s de facto standard keepalive interval; response code interpretation for registration loss detection
- [Twilio bidirectional streaming changelog](https://www.twilio.com/en-us/changelog/bi-directional-streaming-support-with-media-streams) — mark/clear introduction and barge-in use case context

### Tertiary (MEDIUM-LOW confidence)
- [OpenAI Community — truncation/barge-in pitfall](https://community.openai.com/t/openai-realtime-how-to-correctly-truncate-a-live-streaming-conversation-on-speech-interruption-twilio-media-streams/1371637) — pending mark accumulation issue; consistent with Twilio spec but community-sourced

---
*Research completed: 2026-03-05*
*Ready for roadmap: yes*
