# Phase 1: Foundation - Context

**Gathered:** 2026-03-03
**Status:** Ready for planning

<domain>
## Phase Boundary

Set up the project scaffold and prove that a Docker container registers with sipgate SIP trunking and stays registered. Fails fast with a clear error if required configuration is missing. No audio bridging — SIP registration + keepalive only.

</domain>

<decisions>
## Implementation Decisions

### Package manager
- pnpm — use throughout; lock file committed, Docker layer caches node_modules via `pnpm fetch` pattern

### Source layout
- Domain-layered: `src/config/`, `src/sip/`, `src/bridge/`, `src/logger/`
- Each domain owns its own files; Phase 2 adds `src/rtp/` and `src/ws/` into existing structure

### Local development workflow
- Run with `tsx --watch` directly — no Docker overhead for day-to-day development
- Docker used for integration testing and production only
- `package.json` dev script: `pnpm dev` → `tsx --watch src/index.ts`

### Logging
- Claude's Discretion: choose the best structured JSON logger (pino is the recommended default — fast, minimal, first-class JSON, good for high-volume RTP log paths in later phases)
- Log lines must include `callId` and `streamSid` context fields (OBS-01) — establish this pattern in Phase 1 even though calls don't exist yet (child logger with bound fields)

### Env validation
- Zod schema for all required env vars; `CFG-05`: on missing var, log a single structured error naming the missing variable(s) and exit with code 1
- Fail at startup before any network connections are attempted

</decisions>

<code_context>
## Existing Code Insights

### Reusable Assets
- None — greenfield project

### Established Patterns
- None yet — Phase 1 establishes all patterns

### Integration Points
- `src/index.ts` — entrypoint; loads config, initializes logger, boots SIP UserAgent
- SIP.js requires a custom Node.js transport (no official UDP transport for Node.js); `src/sip/` will contain the transport adapter and UserAgent setup

### Known Concerns from Research
- SIP.js has no official Node.js transport — `01-02` builds a custom WebSocket transport adapter (sipgate supports SIP over WSS)
- SIP.js viaHost timing issue (#1002): local socket address only known after `connect()`; apply community workaround, validate against SIP.js 0.21.x API
- sipgate SIP registrar URL and WSS endpoint need validation from sipgate portal (research found `wss://sip.sipgate.de` / `sipconnect.sipgate.de` at medium confidence)

</code_context>

<specifics>
## Specific Ideas

- SIP.js transport wraps a WebSocket connection to sipgate's WSS SIP endpoint (not raw UDP) — this is the supported path for SIP.js in Node.js
- Docker Compose uses `network_mode: host` for Linux production; for macOS integration testing, explicit RTP port range publishing required (separate override or documented workaround)

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 01-foundation*
*Context gathered: 2026-03-03*
