# Requirements: audio-dock

**Defined:** 2026-03-05
**Core Value:** Incoming SIP calls from sipgate trunking are reliably bridged to a WebSocket endpoint in real-time — audio flows both ways, the connection stays alive, and the integration is drop-in compatible with Twilio Media Streams consumers.

## v2.1 Requirements

Requirements for the v2.1 milestone. Each maps to roadmap phases.

### Twilio Protocol — mark/clear (Go)

- [x] **MARK-01**: Audio-dock erkennt eingehende `mark`-Events vom WS-Server und echot den mark-Namen zurück, sobald alle vorherigen Audio-Frames gespielt wurden
- [x] **MARK-02**: Audio-dock echot einen mark sofort zurück, wenn die `packetQueue` beim Eingang bereits leer ist
- [x] **MARK-03**: Audio-dock erkennt eingehende `clear`-Events, leert die `packetQueue` und echot alle ausstehenden marks sofort zurück
- [x] **MARK-04**: RTP-Pacer läuft während eines `clear`-Events weiter (keine Unterbrechung des RTP-Streams)

### Twilio Protocol — mark/clear (Node.js)

- [ ] **MRKN-01**: Node.js-Implementierung erkennt `mark`-Events und echot den mark-Namen nach Playout aller vorherigen Audio-Frames zurück
- [ ] **MRKN-02**: Node.js-Implementierung erkennt `clear`-Events, leert die Audio-Queue und echot pending marks sofort
- [ ] **MRKN-03**: `WsClient`-Interface wird um `onMark`, `sendMark`, `sendClear` erweitert

### SIP OPTIONS Keepalive (Go)

- [x] **OPTS-01**: Go-Registrar sendet alle 30s einen SIP OPTIONS-Request an sipgate zur Liveness-Prüfung
- [x] **OPTS-02**: Bei Timeout, 5xx oder 404-Antwort wird sofort eine Re-Registrierung ausgelöst
- [x] **OPTS-03**: Bei 401/407-Antwort wird keine Re-Registrierung ausgelöst (Server ist erreichbar, nur Auth-Problem)
- [x] **OPTS-04**: OPTIONS keepalive-Goroutine ist an den Root-Context gebunden und stoppt sauber bei SIGTERM
- [x] **OPTS-05**: OPTIONS-Interval ist via `SIP_OPTIONS_INTERVAL` env-var konfigurierbar (Default: 30s)

### SIP OPTIONS Keepalive (Node.js)

- [ ] **OPTN-01**: Node.js-Implementierung sendet alle 30s einen SIP OPTIONS-Request an sipgate
- [ ] **OPTN-02**: Bei Timeout oder Fehler wird Re-Registrierung ausgelöst; bei 401/407 nicht
- [ ] **OPTN-03**: OPTIONS keepalive stoppt sauber beim Shutdown des User-Agents

## Future Requirements

### Observability

- **OBS-01**: Prometheus counter `sip_options_failures_total` für Alerting auf Registration-Health
- **OBS-02**: Konfigurierbare `SIP_OPTIONS_INTERVAL_S` env-var (in v2.1 bereits implementiert als OPTS-05)

### Protocol

- **PROT-01**: Sequence-number-Kontinuität für mark-Echoes über WS-Reconnects hinweg
- **PROT-02**: In-dialog OPTIONS für Session-Refresh (RFC 4028) — anderer Use-Case als Trunk-Monitoring

## Out of Scope

| Feature | Reason |
|---------|--------|
| Outbound call initiation | Inbound only — anderer State-Machine |
| Multi-codec support | PCMU only; Transcoding gehört in WS-Consumer |
| SRTP / media encryption | Plain RTP für interne Infrastruktur |
| Real-time `sip_options_failures_total` Prometheus counter | Defer to v2.2 |

## Traceability

Which phases cover which requirements. Updated during roadmap creation.

| Requirement | Phase | Status |
|-------------|-------|--------|
| MARK-01 | Phase 9 | Complete |
| MARK-02 | Phase 9 | Complete |
| MARK-03 | Phase 9 | Complete |
| MARK-04 | Phase 9 | Complete |
| MRKN-01 | Phase 11 | Pending |
| MRKN-02 | Phase 11 | Pending |
| MRKN-03 | Phase 11 | Pending |
| OPTS-01 | Phase 10 | Complete |
| OPTS-02 | Phase 10 | Complete |
| OPTS-03 | Phase 10 | Complete |
| OPTS-04 | Phase 10 | Complete |
| OPTS-05 | Phase 10 | Complete |
| OPTN-01 | Phase 11 | Pending |
| OPTN-02 | Phase 11 | Pending |
| OPTN-03 | Phase 11 | Pending |

**Coverage:**
- v2.1 requirements: 15 total
- Mapped to phases: 15
- Unmapped: 0 ✓

---
*Requirements defined: 2026-03-05*
*Last updated: 2026-03-05 — traceability filled during roadmap creation*
