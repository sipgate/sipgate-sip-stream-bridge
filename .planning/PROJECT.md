# audio-dock

## What This Is

A Node.js/TypeScript service that registers with sipgate trunking via SIP, accepts incoming calls, and bridges the audio bidirectionally to a configurable WebSocket endpoint. The WebSocket protocol follows the Twilio Media Streams format, making it compatible with AI voice-bot backends that expect this interface. Runs as a Docker container.

## Core Value

Incoming SIP calls from sipgate trunking are reliably bridged to a WebSocket endpoint in real-time — audio flows both ways, the connection stays alive, and the integration is drop-in compatible with Twilio Media Streams consumers.

## Requirements

### Validated

(None yet — ship to validate)

### Active

- [ ] Service registers with sipgate trunking via SIP on startup
- [ ] Service accepts incoming SIP calls
- [ ] Each call establishes a dedicated WebSocket connection to the configured target URL
- [ ] Audio from caller is forwarded to WebSocket in Twilio Media Streams format (base64 mulaw 8kHz)
- [ ] Audio received from WebSocket is played back to the caller (bidirectional)
- [ ] Multiple concurrent calls supported, each with independent WebSocket connection
- [ ] If WebSocket is unavailable at call start, the SIP call is rejected
- [ ] If WebSocket drops during an active call, the service reconnects and keeps the call alive
- [ ] All credentials and target URL configured via environment variables
- [ ] Service runs in Docker

### Out of Scope

- Outbound call initiation — inbound only
- Web UI or management dashboard — config is env-only
- Call recording or storage — audio is streamed, not persisted
- Multiple target URLs / routing logic — single fixed WebSocket URL
- SRTP / media encryption — plain RTP assumed for internal use

## Context

- **SIP library**: SIP.js (pure JavaScript SIP stack, no sidecar process required)
- **Audio format**: Twilio Media Streams uses mulaw (PCMU) 8kHz, base64-encoded, streamed as JSON events over WebSocket
- **Twilio Media Streams events**: `connected`, `start`, `media`, `stop` (inbound); `media`, `mark` (outbound from WS)
- **sipgate trunking**: Standard SIP REGISTER + INVITE flow; credentials are SIP user/password + domain
- **Reconnect behavior**: During WS reconnect, call stays up (RTP continues); audio may be briefly dropped

## Constraints

- **Tech Stack**: Node.js + TypeScript — no compiled sidecar processes
- **SIP Library**: SIP.js — pure JS, manageable Docker image
- **Transport**: UDP/TCP SIP, RTP audio
- **Config**: Environment variables only — no config files, suitable for Docker/K8s secrets

## Key Decisions

| Decision | Rationale | Outcome |
|----------|-----------|---------|
| SIP.js over drachtio | No C++ sidecar, simpler Docker setup, user preference | — Pending |
| Twilio Media Streams format | Drop-in compatibility with existing AI voice backends | — Pending |
| Reject call if WS unavailable | Fail fast is cleaner than silently dropping audio | — Pending |
| Reconnect on WS drop, hold call | Better UX than forcing caller to redial on transient WS issues | — Pending |

---
*Last updated: 2026-03-03 after initialization*
