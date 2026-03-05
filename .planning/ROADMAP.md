# Roadmap: audio-dock

## Milestones

- ✅ **v1.0 MVP** — Phases 1–3 (shipped 2026-03-03)
- ✅ **v2.0 Go Rewrite** — Phases 4–8 (shipped 2026-03-05)
- ✅ **v2.1 Twilio Media Streams - Complete Protocol** — Phases 9–11 (shipped 2026-03-05)

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

<details>
<summary>✅ v2.1 Twilio Media Streams - Complete Protocol (Phases 9–11) — SHIPPED 2026-03-05</summary>

- [x] Phase 9: Go Bridge mark/clear (3/3 plans) — outboundFrame tagged union, mark/clear protocol, channel-logic tests — completed 2026-03-05
- [x] Phase 10: Go SIP OPTIONS Keepalive (2/2 plans) — optionsKeepaliveLoop, applyOptionsResponse, sync.Mutex — completed 2026-03-05
- [x] Phase 11: Node.js Equivalents (3/3 plans) — Vitest, DrainItem queue, WsClient extension, OPTIONS keepalive — completed 2026-03-05

See archive: `.planning/milestones/v2.1-ROADMAP.md`

</details>

## Progress

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
| 10. Go SIP OPTIONS Keepalive | v2.1 | 2/2 | Complete | 2026-03-05 |
| 11. Node.js Equivalents | v2.1 | 3/3 | Complete | 2026-03-05 |
