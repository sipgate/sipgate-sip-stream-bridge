# Roadmap: audio-dock

## Overview

audio-dock is built in four phases that follow the dependency order of the problem: first prove SIP.js works in Node.js and Docker networking is sane (Phase 1), then build the complete bidirectional audio bridge end-to-end (Phase 2), then harden against transient failures (Phase 3), then add production observability (Phase 4). Each phase delivers a verifiable, coherent capability — nothing is left partially built.

## Phases

**Phase Numbering:**
- Integer phases (1, 2, 3): Planned milestone work
- Decimal phases (2.1, 2.2): Urgent insertions (marked with INSERTED)

Decimal phases appear between their surrounding integers in numeric order.

- [ ] **Phase 1: Foundation** - SIP registration running in Docker with validated configuration
- [x] **Phase 2: Core Bridge** - Bidirectional audio flowing end-to-end for concurrent calls (completed 2026-03-03)
- [ ] **Phase 3: Resilience** - Service survives WebSocket drops and cleans up resources correctly
- [ ] **Phase 4: Observability** - Health endpoint and Prometheus metrics expose production state

## Phase Details

### Phase 1: Foundation
**Goal**: A Docker container registers with sipgate SIP trunking, stays registered, and fails fast with a clear error if configuration is missing
**Depends on**: Nothing (first phase)
**Requirements**: CFG-01, CFG-02, CFG-03, CFG-04, CFG-05, SIP-01, SIP-02, DCK-01, DCK-02, DCK-03, OBS-01
**Success Criteria** (what must be TRUE):
  1. Running `docker compose up` with valid env vars produces a structured JSON log line confirming SIP 200 OK registration within 10 seconds
  2. Running `docker compose up` with a missing required env var exits immediately with a descriptive error naming the missing variable
  3. After the SIP registration Expires timer would have lapsed, the service re-registers automatically without manual intervention
  4. Docker image builds successfully via multi-stage build on node:22-alpine and starts without native module errors
**Plans**: 3 plans

Plans:
- [ ] 01-01-PLAN.md — Project scaffold, TypeScript config, env validation (Zod), structured JSON logger
- [x] 01-02-PLAN.md — SIP.js Node.js transport adapter, UserAgent construction, SIP REGISTER + re-REGISTER
- [ ] 01-03-PLAN.md — Dockerfile (multi-stage, node:22-alpine), Docker Compose with host networking, RTP port range comments

### Phase 2: Core Bridge
**Goal**: An inbound SIP call from sipgate reaches the WebSocket backend as Twilio Media Streams events, and audio from the backend plays back to the caller — multiple concurrent calls work independently
**Depends on**: Phase 1
**Requirements**: SIP-03, SIP-04, SIP-05, WSB-01, WSB-02, WSB-03, WSB-04, WSB-05, WSB-06, WSB-07, CON-01, CON-02, LCY-01
**Success Criteria** (what must be TRUE):
  1. An inbound SIP call triggers a `connected` then `start` event on the WebSocket backend (with correct streamSid, callSid, From, To, Call-ID in customParameters) before any `media` events arrive
  2. Caller audio arrives at the WebSocket backend as `media` events containing base64 mulaw 8kHz payloads
  3. Audio sent from the WebSocket backend as `media` events plays back audibly to the caller via RTP
  4. If the WebSocket backend is unreachable when the call arrives, the caller receives a SIP 503 rejection
  5. Two simultaneous calls each receive independent `streamSid` values and independent WebSocket connections; audio does not cross between them
  6. After a call ends (either side), the service sends SIP BYE and a `stop` event; on SIGTERM all active calls are terminated and the service UNREGISTERS before exiting
**Plans**: 5 plans

Plans:
- [ ] 02-01-PLAN.md — SDP offer parser + answer builder, per-call RTP handler with DTMF detection
- [ ] 02-02-PLAN.md — WS client with Twilio Media Streams protocol (connected/start/media/stop/dtmf)
- [ ] 02-03-PLAN.md — SIP UserAgent INVITE/ACK/BYE/OPTIONS dispatch extension + unregister
- [x] 02-04-PLAN.md — CallManager: INVITE lifecycle orchestration, CallSession map, audio bridge wiring
- [ ] 02-05-PLAN.md — Entrypoint wiring + SIGTERM/SIGINT graceful shutdown (terminateAll + unregister)

### Phase 3: Resilience
**Goal**: The service survives WebSocket drops mid-call without hanging up the caller, and correctly releases all file descriptors after calls end
**Depends on**: Phase 2
**Requirements**: WSR-01, WSR-02, WSR-03
**Success Criteria** (what must be TRUE):
  1. When the WebSocket backend disconnects during an active call, the SIP call stays up and the service reconnects to the WebSocket with exponential backoff; the caller hears a brief gap and the call continues
  2. After WebSocket reconnect, the backend receives a fresh `connected` then `start` event before any `media` events resume
  3. After 20 sequential calls, the process file descriptor count returns to the same baseline as after 0 calls (no descriptor leak)
**Plans**: TBD

Plans:
- [ ] 03-01: Exponential backoff reconnect loop in WS Client with drop-not-buffer policy during reconnect window
- [ ] 03-02: Re-emit connected + start sequence after WS reconnect before forwarding audio
- [ ] 03-03: RTP socket lifecycle registry — verify cleanup with fd-count test after sequential calls

### Phase 4: Observability
**Goal**: Operators can query the service's registration status and call volume over HTTP, and a Prometheus scraper can track key counters
**Depends on**: Phase 3
**Requirements**: OBS-02, OBS-03
**Success Criteria** (what must be TRUE):
  1. `GET /health` returns JSON with `registered: true/false` and `activeCalls: N` reflecting current state within one second
  2. `GET /metrics` returns valid Prometheus exposition format including `active_calls_total`, `sip_registration_status`, `rtp_packets_received_total`, `rtp_packets_sent_total`, and `ws_reconnect_attempts_total`
**Plans**: TBD

Plans:
- [ ] 04-01: Fastify HTTP server with /health endpoint wired to CallManager and Registerer state
- [ ] 04-02: Prometheus metrics registry with all five counters/gauges wired to CallManager and WS Client

## Progress

**Execution Order:**
Phases execute in numeric order: 1 → 2 → 3 → 4

| Phase | Plans Complete | Status | Completed |
|-------|----------------|--------|-----------|
| 1. Foundation | 2/3 | In Progress|  |
| 2. Core Bridge | 5/5 | Complete   | 2026-03-03 |
| 3. Resilience | 0/3 | Not started | - |
| 4. Observability | 0/2 | Not started | - |
