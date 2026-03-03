# Roadmap: audio-dock

## Milestones

- ✅ **v1.0 MVP** — Phases 1–3 (shipped 2026-03-03)
- 📋 **v1.1 Observability** — Phase 4 (planned)

## Phases

<details>
<summary>✅ v1.0 MVP (Phases 1–3) — SHIPPED 2026-03-03</summary>

- [x] Phase 1: Foundation (3/3 plans) — SIP registration, config validation, Docker — completed 2026-03-03
- [x] Phase 2: Core Bridge (5/5 plans) — Bidirectional RTP↔WebSocket bridge (Twilio Media Streams) — completed 2026-03-03
- [x] Phase 3: Resilience (2/2 plans) — WS reconnect backoff + FD leak verification — completed 2026-03-03

See archive: `.planning/milestones/v1.0-ROADMAP.md`

</details>

### 📋 v1.1 Observability (Planned)

- [ ] **Phase 4: Observability** - Health endpoint and Prometheus metrics expose production state

#### Phase 4: Observability
**Goal**: Operators can query the service's registration status and call volume over HTTP, and a Prometheus scraper can track key counters
**Depends on**: Phase 3
**Requirements**: OBS-02, OBS-03
**Success Criteria** (what must be TRUE):
  1. `GET /health` returns JSON with `registered: true/false` and `activeCalls: N` reflecting current state within one second
  2. `GET /metrics` returns valid Prometheus exposition format including `active_calls_total`, `sip_registration_status`, `rtp_packets_received_total`, `rtp_packets_sent_total`, and `ws_reconnect_attempts_total`
**Plans**: 2 plans

Plans:
- [ ] 04-01: Fastify HTTP server with /health endpoint wired to CallManager and Registerer state
- [ ] 04-02: Prometheus metrics registry with all five counters/gauges wired to CallManager and WS Client

## Progress

| Phase | Milestone | Plans Complete | Status | Completed |
|-------|-----------|----------------|--------|-----------|
| 1. Foundation | v1.0 | 3/3 | Complete | 2026-03-03 |
| 2. Core Bridge | v1.0 | 5/5 | Complete | 2026-03-03 |
| 3. Resilience | v1.0 | 2/2 | Complete | 2026-03-03 |
| 4. Observability | v1.1 | 0/2 | Not started | - |
