# Requirements: audio-dock

**Defined:** 2026-03-03
**Milestone:** v2.0 Go Rewrite
**Core Value:** Incoming SIP calls from sipgate trunking are reliably bridged to a WebSocket endpoint in real-time — audio flows both ways, the connection stays alive, and the integration is drop-in compatible with Twilio Media Streams consumers.

## v1 Requirements

### SIP Core

- [ ] **SIP-01**: Service registers with sipgate SIP trunking on startup using configured credentials (Digest Auth via sipgo v1.2.0)
- [ ] **SIP-02**: Service automatically re-registers at 90% of server-granted Expires timer (re-register goroutine with `ClientRequestRegisterBuild`)
- [ ] **SIP-03**: Service accepts inbound SIP INVITE and negotiates PCMU (G.711 mu-law 8kHz) codec
- [ ] **SIP-04**: Service rejects inbound INVITE with 503 if the target WebSocket cannot be connected
- [ ] **SIP-05**: Service sends SIP BYE when call ends from either side

### WebSocket Bridge

- [ ] **WSB-01**: Service sends `connected` event after WebSocket connection is established for a call
- [ ] **WSB-02**: Service sends `start` event with streamSid, callSid, tracks, and mediaFormat before forwarding audio
- [ ] **WSB-03**: Service forwards inbound RTP audio as `media` events (base64 mulaw payload) to the WebSocket
- [ ] **WSB-04**: Service sends `stop` event when the SIP call ends
- [ ] **WSB-05**: Service receives `media` events from WebSocket and converts them to outbound RTP to the caller
- [ ] **WSB-06**: Call metadata (From, To, SIP Call-ID) is included in `start.customParameters`
- [ ] **WSB-07**: Service forwards DTMF digits as `dtmf` events (PT 113 telephone-event, RFC 4733 End-bit deduplication by RTP timestamp)

### WebSocket Resilience

- [ ] **WSR-01**: If WebSocket disconnects during an active call, service reconnects with exponential backoff (1s/2s/4s cap, 30s budget)
- [ ] **WSR-02**: After WebSocket reconnect, service re-sends `connected` then `start` before forwarding audio
- [ ] **WSR-03**: Audio arriving from RTP during WebSocket reconnect window is dropped (not buffered indefinitely)

### Concurrency

- [ ] **CON-01**: Multiple simultaneous calls are supported, each with an independent WebSocket connection
- [ ] **CON-02**: Per-call RTP sockets and goroutines are cleaned up after call ends (no goroutine or file descriptor leak)

### Configuration

- [x] **CFG-01**: SIP credentials (user, password, domain/registrar) are configured via environment variables (same names as v1.0 for drop-in compatibility)
- [x] **CFG-02**: Target WebSocket URL is configured via environment variable
- [x] **CFG-03**: RTP port range (min/max) is configured via environment variables
- [x] **CFG-04**: External/reachable IP for SDP contact line is configured via environment variable (`SDP_CONTACT_IP`)
- [x] **CFG-05**: Service fails to start with a descriptive error if any required configuration is missing

### Docker & Runtime

- [x] **DCK-01**: Service is built as a static Go binary with `CGO_ENABLED=0` — no Node.js runtime
- [x] **DCK-02**: Docker Compose file is provided with `network_mode: host` for Linux production use
- [x] **DCK-03**: Dockerfile enforces `CGO_ENABLED=0`; final image uses `FROM scratch` or distroless (~8–15 MB vs 180 MB Node.js Alpine)

### Observability

- [x] **OBS-01**: Service logs structured JSON with callId and streamSid context on each relevant log line (zerolog)
- [ ] **OBS-02**: `GET /health` returns JSON with `registered: true/false` and `activeCalls: N` reflecting current state
- [ ] **OBS-03**: `GET /metrics` returns valid Prometheus exposition format including `active_calls_total`, `sip_registration_status`, `rtp_packets_received_total`, `rtp_packets_sent_total`, `ws_reconnect_attempts_total`

### Lifecycle

- [ ] **LCY-01**: On SIGTERM/SIGINT, service sends SIP BYE to all active calls, UNREGISTER, and closes all WebSocket connections before exiting

## v2 Requirements

### Extended Protocol

- **EXT-01**: `mark` and `clear` event support for barge-in / audio interruption
- **EXT-02**: SIP OPTIONS keepalive to detect silent registration loss
- **EXT-03**: Configurable behavior on WebSocket unavailable (currently fixed: reject call)

### Outbound Calls

- **OUT-01**: Service initiates outbound SIP calls
- Note: Requires entirely different state machine — deferred

### Advanced Audio

- **ADV-01**: Multi-codec support (G.722, Opus) with transcoding
- **ADV-02**: SRTP media encryption support
- **ADV-03**: `diago` higher-level RTP API evaluation (emiago/diago AudioReader/AudioWriter)

## Out of Scope

| Feature | Reason |
|---------|--------|
| Web UI / management dashboard | Config is env-only; no user-facing interface needed |
| Call recording / storage | Audio is streamed, not persisted; recording belongs in WS consumer |
| Multiple target WebSocket URLs / routing | Single fixed URL keeps the bridge as a dumb pipe |
| B2BUA functionality | Use Kamailio/OpenSIPS for call routing; audio-dock is a bridge only |
| Outbound call initiation (v2.0) | Different product capability; deferred to v3+ |
| SRTP media encryption (v2.0) | Network-level encryption is appropriate for internal infrastructure |
| Multi-codec support (v2.0) | PCMU only; transcoding belongs in WS consumer |

## Traceability

Which phases cover which requirements. Updated during roadmap creation.

| Requirement | Phase | Status |
|-------------|-------|--------|
| SIP-01 | Phase 5 | Pending |
| SIP-02 | Phase 5 | Pending |
| SIP-03 | Phase 6 | Pending |
| SIP-04 | Phase 6 | Pending |
| SIP-05 | Phase 6 | Pending |
| WSB-01 | Phase 6 | Pending |
| WSB-02 | Phase 6 | Pending |
| WSB-03 | Phase 6 | Pending |
| WSB-04 | Phase 6 | Pending |
| WSB-05 | Phase 6 | Pending |
| WSB-06 | Phase 6 | Pending |
| WSB-07 | Phase 7 | Pending |
| WSR-01 | Phase 7 | Pending |
| WSR-02 | Phase 7 | Pending |
| WSR-03 | Phase 7 | Pending |
| CON-01 | Phase 6 | Pending |
| CON-02 | Phase 6 | Pending |
| CFG-01 | Phase 4 | Complete |
| CFG-02 | Phase 4 | Complete |
| CFG-03 | Phase 4 | Complete |
| CFG-04 | Phase 4 | Complete |
| CFG-05 | Phase 4 | Complete |
| DCK-01 | Phase 4 | Complete |
| DCK-02 | Phase 4 | Complete |
| DCK-03 | Phase 4 | Complete |
| OBS-01 | Phase 4 | Complete |
| OBS-02 | Phase 8 | Pending |
| OBS-03 | Phase 8 | Pending |
| LCY-01 | Phase 8 | Pending |

**Coverage:**
- v1 requirements: 29 total
- Mapped to phases: 29
- Unmapped: 0

---
*Requirements defined: 2026-03-03*
*Milestone: v2.0 Go Rewrite*
