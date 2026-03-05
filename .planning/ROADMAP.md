# Roadmap: audio-dock

## Milestones

- ✅ **v1.0 MVP** — Phases 1–3 (shipped 2026-03-03)
- ✅ **v2.0 Go Rewrite** — Phases 4–8 (shipped 2026-03-05)
- 🚧 **v2.1 Twilio Media Streams - Complete Protocol** — Phases 9–11 (in progress)

## Phases

<details>
<summary>✅ v1.0 MVP (Phases 1–3) — SHIPPED 2026-03-03</summary>

- [x] Phase 1: Foundation (3/3 plans) — SIP registration, config validation, Docker — completed 2026-03-03
- [x] Phase 2: Core Bridge (5/5 plans) — Bidirectional RTP↔WebSocket bridge (Twilio Media Streams) — completed 2026-03-03
- [x] Phase 3: Resilience (2/2 plans) — WS reconnect backoff + FD leak verification — completed 2026-03-03

See archive: `.planning/milestones/v1.0-ROADMAP.md`

</details>

<details>
<summary>✅ v2.0 Go Rewrite (Phases 4–8) — SHIPPED 2026-03-05</summary>

- [x] Phase 4: Go Scaffold (2/2 plans) — Go module, zerolog, fail-fast config, FROM scratch Docker — completed 2026-03-03
- [x] Phase 5: SIP Registration (1/1 plans) — sipgo Digest Auth, re-registration loop — completed 2026-03-03
- [x] Phase 6: Inbound Call + RTP Bridge (3/3 plans) — INVITE/ACK/BYE, PortPool, CallSession, full WS bridge — completed 2026-03-04
- [x] Phase 7: WebSocket Resilience + DTMF (2/2 plans) — reconnect backoff, RFC 4733 DTMF dedup — completed 2026-03-04
- [x] Phase 8: Lifecycle + Observability (2/2 plans) — graceful drain, /health, /metrics Prometheus — completed 2026-03-04

See archive: `.planning/milestones/v2.0-ROADMAP.md`

</details>

### 🚧 v2.1 Twilio Media Streams - Complete Protocol (In Progress)

**Milestone Goal:** Extend both Go and Node.js implementations with mark/clear protocol events and SIP OPTIONS keepalive for silent registration-loss detection.

- [x] **Phase 9: Go Bridge mark/clear** - Go implementation of Twilio mark and clear events with correct packetQueue goroutine wiring (completed 2026-03-05)
- [ ] **Phase 10: Go SIP OPTIONS Keepalive** - Periodic OPTIONS ping to sipgate for silent registration-loss detection and re-registration
- [ ] **Phase 11: Node.js Equivalents** - Port mark/clear and OPTIONS keepalive to the Node.js reference implementation

## Phase Details

### Phase 9: Go Bridge mark/clear
**Goal**: The Go implementation correctly handles Twilio mark and clear events — barge-in works end-to-end on the Go path
**Depends on**: Phase 8 (v2.0 complete)
**Requirements**: MARK-01, MARK-02, MARK-03, MARK-04
**Success Criteria** (what must be TRUE):
  1. When the WS server sends a mark event, audio-dock echoes the mark name back after all audio frames preceding it have finished playing
  2. When the WS server sends a mark and the outbound audio queue is already empty, the echo is sent immediately
  3. When the WS server sends a clear event, all buffered outbound audio is discarded and any pending mark names are echoed back immediately
  4. RTP packets continue flowing at the normal 20ms interval through a clear event — no timestamp discontinuity or audible click
  5. `go test -race` passes with zero data-race reports across all bridge tests
**Plans**: 3 plans

Plans:
- [x] 09-01-PLAN.md — outboundFrame type rename + new CallSession channels + Prometheus counters (scaffold) — completed 2026-03-05
- [ ] 09-02-PLAN.md — mark/clear protocol implementation in wsToRTP, rtpPacer, wsPacer
- [ ] 09-03-PLAN.md — tests: sendMarkEcho JSON schema + channel-logic tests for MARK-01 through MARK-04

### Phase 10: Go SIP OPTIONS Keepalive
**Goal**: The Go registrar detects silent registration loss within one OPTIONS interval and triggers re-registration automatically
**Depends on**: Phase 9
**Requirements**: OPTS-01, OPTS-02, OPTS-03, OPTS-04, OPTS-05
**Success Criteria** (what must be TRUE):
  1. A SIP OPTIONS request is sent to sipgate every 30 seconds (configurable via `SIP_OPTIONS_INTERVAL` env var)
  2. When sipgate returns a timeout, 5xx, or 404 response, re-registration is triggered immediately
  3. When sipgate returns 401 or 407, no re-registration is triggered (server is reachable; auth issue only)
  4. The OPTIONS keepalive goroutine stops cleanly on SIGTERM — no goroutine leak
  5. Concurrent re-registration calls from both the keepalive loop and the existing re-register loop do not race
**Plans**: 2 plans

Plans:
- [ ] 10-01-PLAN.md — SIPOptionsInterval config field + SIPOptionsFailures Prometheus counter (scaffold)
- [ ] 10-02-PLAN.md — optionsKeepaliveLoop implementation + sync.Mutex + sendOptions + tests

### Phase 11: Node.js Equivalents
**Goal**: The Node.js reference implementation has full parity with Go for mark/clear events and SIP OPTIONS keepalive
**Depends on**: Phase 10
**Requirements**: MRKN-01, MRKN-02, MRKN-03, OPTN-01, OPTN-02, OPTN-03
**Success Criteria** (what must be TRUE):
  1. Node.js echoes a mark name after all preceding audio frames have drained; echoes immediately if the queue is empty at mark receipt
  2. Node.js flushes the audio queue on a clear event and echoes all pending mark names immediately
  3. The `WsClient` interface exposes `onMark`, `sendMark`, and `sendClear` — call managers use the interface, not the raw socket
  4. Node.js sends a SIP OPTIONS request every 30 seconds and triggers re-registration on timeout or error (but not on 401/407)
  5. The OPTIONS keepalive interval is stopped cleanly when the User-Agent shuts down
**Plans**: TBD

## Progress

**Execution Order:**
Phases execute in numeric order: 9 → 10 → 11

| Phase | Milestone | Plans Complete | Status | Completed |
|-------|-----------|----------------|--------|-----------|
| 1. Foundation | v1.0 | 3/3 | Complete | 2026-03-03 |
| 2. Core Bridge | v1.0 | 5/5 | Complete | 2026-03-03 |
| 3. Resilience | v1.0 | 2/2 | Complete | 2026-03-03 |
| 4. Go Scaffold | v2.0 | 2/2 | Complete | 2026-03-03 |
| 5. SIP Registration | v2.0 | 1/1 | Complete | 2026-03-03 |
| 6. Inbound Call + RTP Bridge | v2.0 | 3/3 | Complete | 2026-03-04 |
| 7. WebSocket Resilience + DTMF | v2.0 | 2/2 | Complete | 2026-03-04 |
| 8. Lifecycle + Observability | v2.0 | 2/2 | Complete | 2026-03-04 |
| 9. Go Bridge mark/clear | v2.1 | 3/3 | Complete | 2026-03-05 |
| 10. Go SIP OPTIONS Keepalive | 1/2 | In Progress|  | - |
| 11. Node.js Equivalents | v2.1 | 0/TBD | Not started | - |
