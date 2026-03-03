# Roadmap: audio-dock

## Milestones

- ✅ **v1.0 MVP** — Phases 1–3 (shipped 2026-03-03)
- 🚧 **v2.0 Go Rewrite** — Phases 4–8 (in progress)

## Phases

<details>
<summary>✅ v1.0 MVP (Phases 1–3) — SHIPPED 2026-03-03</summary>

- [x] Phase 1: Foundation (3/3 plans) — SIP registration, config validation, Docker — completed 2026-03-03
- [x] Phase 2: Core Bridge (5/5 plans) — Bidirectional RTP↔WebSocket bridge (Twilio Media Streams) — completed 2026-03-03
- [x] Phase 3: Resilience (2/2 plans) — WS reconnect backoff + FD leak verification — completed 2026-03-03

See archive: `.planning/milestones/v1.0-ROADMAP.md`

</details>

### 🚧 v2.0 Go Rewrite (In Progress)

**Milestone Goal:** Rewrite audio-dock in Go — same external interface, deterministic audio, static binary Docker image, observability included.

- [x] **Phase 4: Go Scaffold** - Go module, zerolog structured logging, fail-fast config validation, Docker static binary
- [ ] **Phase 5: SIP Registration** - Connect to sipgate, register with Digest Auth, automatic re-registration loop
- [ ] **Phase 6: Inbound Call + RTP Bridge** - Accept INVITE, negotiate PCMU, bidirectional RTP↔WebSocket bridge with full Twilio Media Streams protocol
- [ ] **Phase 7: WebSocket Resilience + DTMF** - Reconnect with exponential backoff, silence drop during reconnect, DTMF forwarding
- [ ] **Phase 8: Lifecycle + Observability** - Graceful SIGTERM shutdown, /health and /metrics HTTP endpoints

## Phase Details

### Phase 4: Go Scaffold
**Goal**: A runnable Go binary that validates all required environment variables at startup, logs structured JSON with zerolog, exits with a descriptive error on misconfiguration, and ships as a static FROM-scratch Docker image
**Depends on**: Nothing (first v2.0 phase)
**Requirements**: CFG-01, CFG-02, CFG-03, CFG-04, CFG-05, OBS-01, DCK-01, DCK-02, DCK-03
**Success Criteria** (what must be TRUE):
  1. Running the binary without required env vars (e.g., no `SIP_USER`) prints a descriptive error message naming the missing variable and exits non-zero
  2. Running the binary with all required env vars starts and emits structured JSON log lines (zerolog format) to stdout
  3. `docker build` produces a static binary image using `FROM scratch` or distroless with `CGO_ENABLED=0`; `docker images` shows the image is under 20 MB
  4. `docker compose up` starts the service using the provided Compose file with `network_mode: host`
**Plans**: 2 plans

Plans:
- [x] 04-01-PLAN.md — Go module init + config validation (go-simpler/env) + zerolog entry point (CFG-01..05, OBS-01)
- [x] 04-02-PLAN.md — Two-stage Dockerfile (golang:1.26-alpine + FROM scratch) + docker-compose.yml update (DCK-01..03)

### Phase 5: SIP Registration
**Goal**: The service connects to sipgate's SIP registrar, authenticates with Digest Auth, and maintains registration automatically via a re-register loop — registration status is visible in logs
**Depends on**: Phase 4
**Requirements**: SIP-01, SIP-02
**Success Criteria** (what must be TRUE):
  1. On startup with valid credentials, the service sends REGISTER, handles the 401 Digest Auth challenge, and logs a structured JSON line confirming successful registration
  2. The service re-registers automatically at 75–90% of the server-granted Expires interval without manual intervention; re-registration is visible in logs
  3. On startup with invalid credentials, the service logs a structured error and exits non-zero
**Plans**: TBD

Plans:
- [ ] 05-01: sipgo UserAgent + Server + Client construction, UDP/TCP listener goroutines, REGISTER with DoDigestAuth, re-register ticker at 75% of server-granted Expires (SIP-01, SIP-02)

### Phase 6: Inbound Call + RTP Bridge
**Goal**: The service accepts an inbound SIP INVITE, negotiates PCMU, opens a per-call RTP socket, connects a dedicated WebSocket, and forwards audio bidirectionally — the full Twilio Media Streams protocol (connected/start/media/stop) flows end-to-end; multiple simultaneous calls work independently; all resources are cleaned up when a call ends
**Depends on**: Phase 5
**Requirements**: SIP-03, SIP-04, SIP-05, WSB-01, WSB-02, WSB-03, WSB-04, WSB-05, WSB-06, WSR-01 (partial — initial connect check), CON-01, CON-02
**Success Criteria** (what must be TRUE):
  1. An inbound call triggers a `connected` then `start` event on the WebSocket with correct streamSid, callSid, tracks, mediaFormat, and From/To/Call-ID in `start.customParameters`
  2. Voice audio spoken into the call appears as `media` events (base64 PCMU) on the WebSocket; audio sent as `media` events from the WebSocket is heard by the caller
  3. When the caller hangs up, the service sends SIP BYE and a `stop` event to the WebSocket; when the WebSocket sends `stop`, the service sends SIP BYE to the caller
  4. If the target WebSocket is unreachable at call start, the INVITE is rejected with SIP 503 and no RTP socket is leaked
  5. Two simultaneous calls each get an independent WebSocket connection and independent RTP socket; hanging up one call does not affect the other; after each call ends there are no leaked goroutines or file descriptors
**Plans**: TBD

Plans:
- [ ] 06-01: SIP INVITE handler — DialogServerCache, SDP parsing (pion/sdp), SDP answer with PCMU + telephone-event, 503 rejection if WS unavailable (SIP-03, SIP-04, SIP-05)
- [ ] 06-02: CallManager + CallSession — per-call RTP socket pool, goroutine lifecycle, concurrent session map, FD cleanup on call end (CON-01, CON-02)
- [ ] 06-03: WebSocket bridge — Twilio Media Streams protocol (connected/start/media/stop events), call metadata in customParameters, bidirectional audio forwarding (WSB-01..06)

### Phase 7: WebSocket Resilience + DTMF
**Goal**: When the WebSocket drops mid-call, the service reconnects with exponential backoff, re-sends the handshake sequence, and resumes audio forwarding — DTMF digits pressed by the caller are forwarded as `dtmf` events exactly once per keypress
**Depends on**: Phase 6
**Requirements**: WSR-01, WSR-02, WSR-03, WSB-07
**Success Criteria** (what must be TRUE):
  1. If the WebSocket drops during an active call, the service attempts reconnection with 1s/2s/4s exponential backoff up to a 30-second budget; the call is not torn down during reconnect attempts
  2. After a successful reconnect, the service re-sends `connected` then `start` before forwarding audio; the WebSocket consumer sees the full handshake again
  3. RTP audio arriving during the reconnect window is dropped (not buffered); the service does not accumulate memory or block the RTP receive goroutine
  4. Pressing a DTMF digit on the caller's keypad produces exactly one `dtmf` event on the WebSocket (RFC 4733 End-bit deduplication by RTP timestamp; no duplicate digits)
**Plans**: TBD

Plans:
- [ ] 07-01: WebSocket reconnect loop with exponential backoff (1s/2s/4s cap, 30s budget), reconnecting flag for RTP drop, re-send connected+start on reconnect (WSR-01, WSR-02, WSR-03)
- [ ] 07-02: RFC 4733 telephone-event detection from RTP stream, DTMF PT extracted from SDP offer, End-bit deduplication by timestamp, `dtmf` event forwarding (WSB-07)

### Phase 8: Lifecycle + Observability
**Goal**: The service shuts down cleanly on SIGTERM — all active calls get BYE, the SIP registration is cancelled, and the process exits without leaving remote peers in an unknown state — and operators can query live status and scrape Prometheus metrics
**Depends on**: Phase 7
**Requirements**: LCY-01, OBS-02, OBS-03
**Success Criteria** (what must be TRUE):
  1. On SIGTERM, the service sends SIP BYE to all active calls, sends SIP UNREGISTER, closes all WebSocket connections, and exits within 10 seconds
  2. `GET /health` returns HTTP 200 with JSON body `{"registered": true/false, "activeCalls": N}` reflecting the live state of the SIP registration and call map
  3. `GET /metrics` returns HTTP 200 with valid Prometheus exposition format including `active_calls_total`, `sip_registration_status`, `rtp_packets_received_total`, `rtp_packets_sent_total`, and `ws_reconnect_attempts_total`
**Plans**: TBD

Plans:
- [ ] 08-01: SIGTERM/SIGINT handler — drain active calls with BYE, UNREGISTER, close UA; shutdownFlag to stop accepting new INVITEs during drain (LCY-01)
- [ ] 08-02: net/http server on configurable port — GET /health JSON endpoint wired to CallManager and registration state; GET /metrics Prometheus exposition with all five counters/gauges (OBS-02, OBS-03)

## Progress

**Execution Order:** 4 → 5 → 6 → 7 → 8

| Phase | Milestone | Plans Complete | Status | Completed |
|-------|-----------|----------------|--------|-----------|
| 1. Foundation | v1.0 | 3/3 | Complete | 2026-03-03 |
| 2. Core Bridge | v1.0 | 5/5 | Complete | 2026-03-03 |
| 3. Resilience | v1.0 | 2/2 | Complete | 2026-03-03 |
| 4. Go Scaffold | v2.0 | 2/2 | Complete | 2026-03-03 |
| 5. SIP Registration | v2.0 | 0/1 | Not started | - |
| 6. Inbound Call + RTP Bridge | v2.0 | 0/3 | Not started | - |
| 7. WebSocket Resilience + DTMF | v2.0 | 0/2 | Not started | - |
| 8. Lifecycle + Observability | v2.0 | 0/2 | Not started | - |
