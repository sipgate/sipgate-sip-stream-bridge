# Phase 1: Foundation - Research

**Researched:** 2026-03-03
**Domain:** TypeScript project scaffold + SIP.js Node.js registration + Docker multi-stage build
**Confidence:** MEDIUM — TypeScript/pino/Zod stack is HIGH; SIP.js Node.js transport layer is MEDIUM due to undocumented community paths; sipgate WSS endpoint is MEDIUM (community sources agree, no official WSS doc found)

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions

**Package manager**
- pnpm — use throughout; lock file committed, Docker layer caches node_modules via `pnpm fetch` pattern

**Source layout**
- Domain-layered: `src/config/`, `src/sip/`, `src/bridge/`, `src/logger/`
- Each domain owns its own files; Phase 2 adds `src/rtp/` and `src/ws/` into existing structure

**Local development workflow**
- Run with `tsx --watch` directly — no Docker overhead for day-to-day development
- Docker used for integration testing and production only
- `package.json` dev script: `pnpm dev` → `tsx --watch src/index.ts`

**Logging**
- Claude's Discretion: choose the best structured JSON logger (pino is the recommended default — fast, minimal, first-class JSON, good for high-volume RTP log paths in later phases)
- Log lines must include `callId` and `streamSid` context fields (OBS-01) — establish this pattern in Phase 1 even though calls don't exist yet (child logger with bound fields)

**Env validation**
- Zod schema for all required env vars; `CFG-05`: on missing var, log a single structured error naming the missing variable(s) and exit with code 1
- Fail at startup before any network connections are attempted

### Claude's Discretion

- Choose the best structured JSON logger (pino recommended)

### Deferred Ideas (OUT OF SCOPE)

None — discussion stayed within phase scope
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| CFG-01 | SIP credentials (user, password, domain/registrar URL) configured via env vars | Zod schema pattern; `SIP_USER`, `SIP_PASSWORD`, `SIP_DOMAIN`, `SIP_REGISTRAR` fields |
| CFG-02 | Target WebSocket URL configured via env var | Zod `z.string().url()` for `WS_TARGET_URL` |
| CFG-03 | RTP port range (min/max) configured via env vars | Zod `z.coerce.number()` for `RTP_PORT_MIN` / `RTP_PORT_MAX`; range validation |
| CFG-04 | External/reachable IP for SDP contact line configured via env var (`SDP_CONTACT_IP`) | Zod `z.string().ip()` for `SDP_CONTACT_IP` |
| CFG-05 | Service fails with descriptive error if any required config missing | `safeParse` + `error.flatten().fieldErrors` + `process.exit(1)` pattern |
| SIP-01 | Service registers with sipgate SIP trunking on startup using configured credentials | SIP.js `UserAgent` + `Registerer`; WSS to sipgate; `global.WebSocket = ws`; custom transport constructor pattern |
| SIP-02 | Service automatically re-registers before SIP registration expires | `Registerer` `refreshFrequency` option (% of expires, 50–99); unconditional `register()` call in `onConnect` delegate |
| DCK-01 | Multi-stage Docker image on node:22-alpine | pnpm `fetch` layer-caching pattern; builder + production stages |
| DCK-02 | Docker Compose with `network_mode: host` for Linux production | Documented; macOS workaround: constrained UDP port range |
| DCK-03 | Dockerfile documents RTP port range requirement in comments | Comment pattern in Dockerfile EXPOSE and `docker-compose.yml` |
| OBS-01 | Structured JSON with callId and streamSid context on each log line | pino `logger.child({ callId, streamSid })` pattern; establish in Phase 1 even without live calls |
</phase_requirements>

---

## Summary

Phase 1 establishes the greenfield project and validates the single highest-risk assumption before any business logic is built on top of it: that SIP.js can be made to register with sipgate's SIP trunking from Node.js. SIP.js is officially browser-only, but its `transportConstructor` option allows injecting a custom transport class — the community pattern uses `global.WebSocket = require('ws')` and the built-in `WebSocketTransport` with explicit `Sec-WebSocket-Protocol: sip` header. Sipgate appears to support SIP over WSS (WebSocket Secure on port 443) in addition to UDP port 5060, meaning SIP.js's standard `WebSocketTransport` may work with minimal adaptation. This hypothesis must be verified in Phase 1 before Phase 2 builds call handling on top.

The configuration and logging layers are straightforward. Zod 4 (now stable, released mid-2025) provides env validation with `safeParse()` and `error.flatten()` for structured error reporting. pino 10.x (latest, requiring Node.js 20+) provides first-class JSON structured logging with `logger.child()` for bound-context log lines — the `callId`/`streamSid` binding pattern should be established in Phase 1 even though those fields will be empty until Phase 2 has live calls.

The Docker layer uses pnpm's `pnpm fetch` command for layer-cache-friendly builds: fetch packages from lockfile into pnpm store in one layer, install from store in the next. Combined with a multi-stage Dockerfile on `node:22-alpine`, the production image has no build tooling and no native module dependencies (this entire stack is pure JS). The macOS development constraint (`network_mode: host` is Linux-only) is documented but not solved until Phase 2 needs actual RTP; Phase 1 only needs SIP signaling which works over WSS without host networking.

**Primary recommendation:** Use SIP.js `WebSocketTransport` with `global.WebSocket = require('ws')` and explicit `Sec-WebSocket-Protocol: sip` header via `transportOptions.wsServerVia`; validate REGISTER succeeds with a packet capture before writing any call-handling logic.

---

## Standard Stack

### Core (Phase 1 scope)

| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Node.js | 22 LTS | Runtime | LTS until April 2027; required by pino 10.x (Node 20+ min); no native compilation for any dep |
| TypeScript | 5.7+ | Language | Full type safety; `moduleResolution: NodeNext` required by rtp.js in later phases; set now |
| SIP.js | 0.21.2 | SIP signaling (REGISTER, re-REGISTER) | Only pure-JS SIP library with extensible transport; custom transport via `transportConstructor` |
| pino | 10.3.1 | Structured JSON logger | Fastest Node.js logger; native JSON output; `child()` for bound context; used by Fastify |
| zod | 4.x | Env var schema validation | TypeScript-first; `safeParse` + `flatten()` for structured error reporting; v4 stable since 2025 |
| ws | 8.19.0 | WebSocket (polyfill for SIP.js in Node.js + later Phase 2 outbound) | Required to polyfill `global.WebSocket` for SIP.js |

### Supporting (Phase 1 scope)

| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| tsx | 4.19+ | Run TypeScript directly in Node.js | `pnpm dev` script; hot-reload during development; no compile step |
| tsup | latest | Production bundler | Build TypeScript to `dist/` for Docker image; handles ESM/CJS |
| @types/node | 22.x | Node.js type definitions | Pin to Node.js 22 for `dgram`, `net`, `process` types |
| @types/ws | 8.x | Type definitions for ws | Required for TypeScript usage of ws library |
| pnpm | 9.x | Package manager | Locked decision; `pnpm fetch` for Docker layer caching |

### Alternatives Considered

| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| pino | winston | winston has more plugins; pino is 5x faster; JSON-first; use pino (better for high-volume RTP paths) |
| zod | joi, yup | zod is TypeScript-native; compile-time type inference from schema; only choice for TypeScript-first validation |
| SIP.js 0.21.2 | kirm/sip (`sip` npm) | sip handles UDP/TCP natively; SIP.js has higher-level abstractions (UserAgent, Registerer); use sip as fallback only if WSS transport proves unreliable |
| tsx | ts-node | tsx uses esbuild (faster); ts-node is slower for watch mode; tsx is the 2025 standard |

**Installation (Phase 1):**

```bash
pnpm add sip.js ws pino zod
pnpm add -D typescript tsx tsup @types/node @types/ws
```

**tsconfig.json key settings (set now, required for rtp.js in Phase 2):**

```json
{
  "compilerOptions": {
    "target": "ES2022",
    "module": "NodeNext",
    "moduleResolution": "NodeNext",
    "strict": true,
    "esModuleInterop": true,
    "outDir": "dist",
    "rootDir": "src"
  }
}
```

---

## Architecture Patterns

### Recommended Project Structure

```
src/
├── config/
│   └── index.ts          # Zod schema + validated Config export (CFG-01..05)
├── logger/
│   └── index.ts          # pino root logger; createChildLogger(bindings) helper
├── sip/
│   ├── userAgent.ts      # UserAgent + Registerer factory; transport options
│   └── transport.ts      # Custom WebSocket transport class (if SIP.js WebSocketTransport insufficient)
└── index.ts              # Entry: loadConfig -> initLogger -> startSip -> block
```

Note: `src/bridge/` is included in directory layout but has no files in Phase 1 (Phase 2 adds content). Create the directory with a `.gitkeep` or add it when Phase 2 plans call for it.

### Pattern 1: Zod Env Validation with Fail-Fast Exit

**What:** Parse `process.env` against a Zod schema at module load time. On failure, log structured errors naming each missing/invalid variable and exit with code 1.

**When to use:** Always — this is the first thing `index.ts` does before any network connection.

**Example:**

```typescript
// src/config/index.ts
import { z } from 'zod';

const configSchema = z.object({
  SIP_USER:         z.string().min(1, 'SIP_USER is required'),
  SIP_PASSWORD:     z.string().min(1, 'SIP_PASSWORD is required'),
  SIP_DOMAIN:       z.string().min(1, 'SIP_DOMAIN is required'),
  SIP_REGISTRAR:    z.string().url('SIP_REGISTRAR must be a valid WSS URL (e.g. wss://sip.sipgate.de)'),
  WS_TARGET_URL:    z.string().url('WS_TARGET_URL must be a valid WebSocket URL'),
  RTP_PORT_MIN:     z.coerce.number().int().min(1024).max(65535).default(10000),
  RTP_PORT_MAX:     z.coerce.number().int().min(1024).max(65535).default(10099),
  SDP_CONTACT_IP:   z.string().ip({ version: 'v4' }).optional(),
  SIP_EXPIRES:      z.coerce.number().int().positive().default(120),
  LOG_LEVEL:        z.enum(['trace', 'debug', 'info', 'warn', 'error']).default('info'),
});

export type Config = z.infer<typeof configSchema>;

function loadConfig(): Config {
  const result = configSchema.safeParse(process.env);
  if (!result.success) {
    const errors = result.error.flatten().fieldErrors;
    // Log before logger is initialized — use console.error with JSON
    console.error(JSON.stringify({
      level: 'error',
      msg: 'Configuration validation failed — missing or invalid environment variables',
      errors,
    }));
    process.exit(1);
  }
  return Object.freeze(result.data);
}

export const config: Config = loadConfig();
```

### Pattern 2: pino Child Logger with Bound Context Fields

**What:** Create a root pino logger in `src/logger/index.ts`. Expose a `createChildLogger` helper that binds context fields. In Phase 1, the `callId` and `streamSid` fields are bound as empty strings or omitted; Phase 2 binds real values.

**When to use:** All log statements. Never use `console.log` in production code.

**Example:**

```typescript
// src/logger/index.ts
import pino from 'pino';
import type { Logger } from 'pino';

export const rootLogger: Logger = pino({
  level: process.env.LOG_LEVEL ?? 'info',
  base: { service: 'audio-dock' },
});

export function createChildLogger(bindings: {
  component: string;
  callId?: string;
  streamSid?: string;
}): Logger {
  return rootLogger.child(bindings);
}

// Usage in sip/userAgent.ts:
// const log = createChildLogger({ component: 'sip' });
// log.info({ event: 'registered', expires: 120 }, 'SIP REGISTER 200 OK');
```

### Pattern 3: SIP.js UserAgent with WebSocket Transport in Node.js

**What:** SIP.js ships a built-in `WebSocketTransport` (RFC 7118 compliant). To use it in Node.js: polyfill `global.WebSocket` with `ws` before any SIP.js import; pass `transportOptions` to set the `server` URL and headers. The `viaHost` must be set at construction time.

**When to use:** All SIP.js usage in Node.js — this is the mandatory setup sequence.

**Key finding on sipgate transport:** Multiple community sources and sipgate configuration guides confirm that `sipconnect.sipgate.de` is the registrar hostname using UDP port 5060. However, search results also indicate that `wss://sip.sipgate.de` is the WSS endpoint (SIP over WebSocket on port 443). Since SIP.js only ships WebSocketTransport, the WSS endpoint is the correct path for this project — but this endpoint URL is MEDIUM confidence and MUST be verified from the sipgate portal before implementation.

**Example:**

```typescript
// src/sip/userAgent.ts
// CRITICAL: import ws before sip.js
import { WebSocket } from 'ws';
// Polyfill global WebSocket for SIP.js internal use
(globalThis as any).WebSocket = WebSocket;

import { UserAgent, Registerer, RegistererState } from 'sip.js';
import type { Config } from '../config/index.js';
import type { Logger } from 'pino';

export async function createSipUserAgent(config: Config, log: Logger) {
  const uri = UserAgent.makeURI(`sip:${config.SIP_USER}@${config.SIP_DOMAIN}`);
  if (!uri) throw new Error(`Invalid SIP URI: ${config.SIP_USER}@${config.SIP_DOMAIN}`);

  const ua = new UserAgent({
    uri,
    viaHost: config.SDP_CONTACT_IP ?? config.SIP_DOMAIN,
    authorizationUsername: config.SIP_USER,
    authorizationPassword: config.SIP_PASSWORD,
    transportConstructor: UserAgent.defaultTransportConstructor, // built-in WebSocketTransport
    transportOptions: {
      server: config.SIP_REGISTRAR, // e.g. 'wss://sip.sipgate.de'
      // NOTE: SIP.js WebSocketTransport sets Sec-WebSocket-Protocol: sip internally
      // per RFC 7118 — verify with Wireshark that the header is present
    },
    logLevel: 'warn',
    logBuiltinEnabled: false, // route SIP.js logs through pino instead
  });

  const registerer = new Registerer(ua, {
    expires: config.SIP_EXPIRES,
    refreshFrequency: 90, // re-register at 90% of expires time
  });

  // Unconditionally re-register on every transport connect
  ua.transport.onConnect = () => {
    log.debug({ event: 'transport_connected' }, 'SIP transport connected — sending REGISTER');
    registerer.register().catch((err: Error) => {
      log.error({ err, event: 'register_failed' }, 'SIP REGISTER failed after transport connect');
    });
  };

  ua.transport.onDisconnect = (error?: Error) => {
    if (error) {
      log.warn({ err: error, event: 'transport_disconnected' }, 'SIP transport disconnected with error');
    }
  };

  registerer.stateChange.addListener((state: RegistererState) => {
    log.info({ event: 'registerer_state', state }, `SIP registration state: ${state}`);
    if (state === RegistererState.Registered) {
      log.info({ event: 'registered' }, 'SIP 200 OK — registration confirmed');
    }
    if (state === RegistererState.Unregistered) {
      log.warn({ event: 'unregistered' }, 'SIP registration lost');
    }
  });

  await ua.start();
  await registerer.register();

  return { ua, registerer };
}
```

### Pattern 4: pnpm + Docker Multi-Stage Build with Layer Caching

**What:** Use `pnpm fetch` to download all packages from `pnpm-lock.yaml` into the pnpm store in an early Docker layer. Subsequent `pnpm install --offline` reads from the store — this layer only invalidates when `pnpm-lock.yaml` changes, not when `package.json` or source changes.

**When to use:** All Docker builds with pnpm.

**Example:**

```dockerfile
FROM node:22-alpine AS base
RUN corepack enable && corepack prepare pnpm@latest --activate
WORKDIR /app

# Layer 1: fetch packages — invalidates only when lockfile changes
FROM base AS fetcher
COPY pnpm-lock.yaml ./
RUN pnpm fetch

# Layer 2: install and build
FROM fetcher AS builder
COPY package.json pnpm-lock.yaml ./
# --offline: install from pnpm store fetched in previous layer
RUN pnpm install --frozen-lockfile --offline
COPY . .
RUN pnpm build

# Layer 3: production image — no dev deps, no build tooling
FROM node:22-alpine AS production
RUN corepack enable && corepack prepare pnpm@latest --activate
WORKDIR /app
COPY package.json pnpm-lock.yaml ./
# Copy pnpm store from fetcher stage for offline prod install
COPY --from=fetcher /root/.local/share/pnpm/store /root/.local/share/pnpm/store
RUN pnpm install --prod --frozen-lockfile --offline
COPY --from=builder /app/dist ./dist
# Non-root user for security
USER node
# RTP PORT RANGE: This service requires UDP ports in range RTP_PORT_MIN..RTP_PORT_MAX
# Default: 10000-10099 (100 ports, supports ~50 concurrent calls at 2 ports each)
# Use network_mode: host on Linux (docker-compose.yml) to avoid Docker port-proxy overhead.
# On macOS/Windows: explicitly publish only this range with -p 10000-10099:10000-10099/udp
EXPOSE 10000-10099/udp
CMD ["node", "dist/index.js"]
```

**docker-compose.yml (Linux production):**

```yaml
services:
  audio-dock:
    build: .
    network_mode: host   # Linux only — gives container direct access to host network
    environment:
      - SIP_USER=${SIP_USER}
      - SIP_PASSWORD=${SIP_PASSWORD}
      - SIP_DOMAIN=${SIP_DOMAIN}
      - SIP_REGISTRAR=${SIP_REGISTRAR}
      - WS_TARGET_URL=${WS_TARGET_URL}
      - SDP_CONTACT_IP=${SDP_CONTACT_IP}
      - RTP_PORT_MIN=10000
      - RTP_PORT_MAX=10099
    restart: unless-stopped
```

### Anti-Patterns to Avoid

- **`global.WebSocket = require('ws')` without setting it before any SIP.js import:** SIP.js reads `globalThis.WebSocket` at import time. Set it first or SIP.js uses `undefined` and the transport fails silently.
- **Using `console.log` for structured logs:** pino is the single log pathway; mixing breaks log aggregation in Docker stdout JSON parsing.
- **Hardcoding `viaHost`:** `viaHost` must match the IP that sipgate can reach for SIP responses. Use `SDP_CONTACT_IP` env var or the machine's primary interface.
- **Publishing the full RTP port range in Docker bridge mode:** 10,000+ `EXPOSE` entries in compose freeze Docker startup for minutes. Use `network_mode: host` on Linux.
- **Calling `registerer.register()` only once at startup:** After any transport disconnect/reconnect, the Registerer's state machine can lose sync with the server. Always re-call `register()` in the `onConnect` transport handler.

---

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| SIP signaling state machine | Custom SIP request parser | SIP.js 0.21.2 | SIP dialog management, 401/407 digest auth, request sequencing — hundreds of RFC edge cases |
| Env var validation | Custom `if (!process.env.X)` guards | Zod 4 `z.object().safeParse()` | Automatic TypeScript type inference, structured error reporting, coercion of string to number |
| JSON structured logging | Custom `JSON.stringify` wrapper | pino 10.x | async I/O, child logger inheritance, redaction, automatic serializers |
| WebSocket connection (for SIP transport polyfill) | Raw net.Socket + HTTP upgrade | `ws` npm package | RFC 6455 compliance, TLS, `Sec-WebSocket-Protocol` header, ping/pong keepalive |
| Docker layer caching for node_modules | Copy entire node_modules between layers | `pnpm fetch` + `--offline` install | Lockfile-keyed cache: only re-downloads when pnpm-lock.yaml changes |

**Key insight:** The SIP.js abstractions (UserAgent, Registerer, RegistererState, stateChange emitter) save hundreds of lines of SIP RFC 3261 implementation. The price is the Node.js transport compatibility work — but that work is bounded (polyfill + verify headers) compared to re-implementing SIP from scratch.

---

## Common Pitfalls

### Pitfall 1: Missing `Sec-WebSocket-Protocol: sip` Header Silently Breaks Registration

**What goes wrong:** The WebSocket upgrade to sipgate's WSS SIP endpoint succeeds at the TCP level, but sipgate's SIP proxy rejects the connection because RFC 7118 requires the `Sec-WebSocket-Protocol: sip` subprotocol header. Registration appears to time out or returns 400 with no clear error message.

**Why it happens:** The `ws` npm library does not automatically add this header. SIP.js's built-in `WebSocketTransport` DOES set it internally (it uses the `'sip'` subprotocol string in its WebSocket constructor), but only if `global.WebSocket` is the `ws` class, not a partial polyfill.

**How to avoid:** Set `global.WebSocket = WebSocket` using the named export from `ws` (not `require('ws')`). Capture the WebSocket handshake with Wireshark or `tcpdump -A port 443` to verify `Sec-WebSocket-Protocol: sip` is present in the HTTP upgrade request.

**Warning signs:** WebSocket connection closes immediately after opening; logs show `transport disconnected` within milliseconds of `ua.start()`; no SIP 401/200 received at all.

### Pitfall 2: `viaHost` Set to Wrong IP, SIP Responses Never Arrive

**What goes wrong:** The `viaHost` option in `UserAgentOptions` determines what IP appears in the SIP `Via:` header. If it is set to `localhost`, `127.0.0.1`, or the Docker bridge IP (`172.x.x.x`), sipgate sends SIP responses (200 OK, 401, etc.) to an unreachable address. The REGISTER appears to send but never gets a response — timeout with no error.

**Why it happens:** The `viaHost` is set at `UserAgent` construction time before the WebSocket connection is established (SIP.js issue #1002). It cannot be automatically derived from the outbound socket address. In Node.js the developer must provide it explicitly.

**How to avoid:** Set `viaHost` to the value of `SDP_CONTACT_IP` (the external/reachable IP of the host). In a non-Docker local dev environment, set it to the machine's LAN IP (not 127.0.0.1). For Docker with `network_mode: host`, the host's IP is directly accessible so this is straightforward.

**Warning signs:** `ua.start()` resolves, `registerer.register()` is called, but `RegistererState.Registered` never fires; no SIP 200 OK appears in packet capture on the host interface.

### Pitfall 3: Docker `network_mode: host` Silently Ignored on macOS

**What goes wrong:** `docker-compose.yml` has `network_mode: host` but the developer is on macOS. Docker Desktop on macOS silently ignores host networking (it is Linux-only). The container starts without error, but SIP over WSS works (TCP, unaffected) while RTP ports are inaccessible (UDP, not published).

**Why it happens:** Docker Desktop on macOS runs Linux in a VM; host networking reaches the VM's network, not the macOS host's network. This is documented Docker Desktop behavior.

**How to avoid for Phase 1:** Phase 1 only needs SIP signaling (WSS = TCP/TLS), which works through Docker's port mapping without host networking. No special workaround needed in Phase 1. Document in `docker-compose.override.yml` (or comments) that for Phase 2 RTP testing on macOS, an explicit UDP port range must be published.

**Warning signs:** SIP REGISTER works on macOS (WSS is TCP), but RTP packets from Phase 2 never arrive. This is an expected limitation — Phase 1 is safe.

### Pitfall 4: Zod 4 Import Path Changed from v3

**What goes wrong:** Developers familiar with Zod 3 try `import z from 'zod'` (default import) which now fails in Zod 4 which uses named `z` export. Additionally, Zod 4 is now `import * as z from 'zod'` or `import { z } from 'zod'` — the API shape is the same but `ZodError.format()` has been replaced by `error.flatten()`.

**How to avoid:** Use `import { z } from 'zod'` and install `zod@^4.0.0`. Use `error.flatten().fieldErrors` instead of `error.format()` for per-field error extraction.

### Pitfall 5: sipgate Registrar URL Is Not Verified

**What goes wrong:** The project builds against `wss://sip.sipgate.de` (MEDIUM confidence from community sources) but the actual sipgate trunking WSS endpoint URL is different or requires a specific path segment. Registration fails with a 404 WebSocket response or DNS lookup failure.

**How to avoid:** Before writing any code, log into the sipgate trunking portal and find the exact SIP connection settings. Community sources agree on `sipconnect.sipgate.de` as the registrar hostname (for UDP). The WSS endpoint `wss://sip.sipgate.de` has been cited in multiple sources but is not confirmed in official sipgate trunking documentation found during research. Configure `SIP_REGISTRAR` as an env var so the URL can be changed without rebuilding.

**Warning signs:** `ua.start()` throws a DNS or connection refused error; WebSocket connection to the configured URL fails at the TCP level (not SIP level).

---

## Code Examples

Verified patterns from official sources:

### Zod 4 Env Validation with Field-Level Errors

```typescript
// Source: https://zod.dev/ (official Zod 4 documentation, HIGH confidence)
import { z } from 'zod';

const schema = z.object({
  SIP_USER:       z.string().min(1),
  SIP_PASSWORD:   z.string().min(1),
  RTP_PORT_MIN:   z.coerce.number().int().min(1024).max(65534),
  RTP_PORT_MAX:   z.coerce.number().int().min(1025).max(65535),
  SDP_CONTACT_IP: z.string().ip({ version: 'v4' }).optional(),
}).refine(
  (d) => !d.RTP_PORT_MIN || !d.RTP_PORT_MAX || d.RTP_PORT_MIN < d.RTP_PORT_MAX,
  { message: 'RTP_PORT_MIN must be less than RTP_PORT_MAX', path: ['RTP_PORT_MIN'] }
);

const result = schema.safeParse(process.env);
if (!result.success) {
  const fieldErrors = result.error.flatten().fieldErrors;
  console.error(JSON.stringify({ level: 'error', msg: 'Config invalid', fieldErrors }));
  process.exit(1);
}
const config = Object.freeze(result.data);
```

### pino Child Logger Binding (Establishes OBS-01 Pattern)

```typescript
// Source: https://github.com/pinojs/pino/blob/main/docs/child-loggers.md (HIGH confidence)
import pino from 'pino';

const root = pino({ level: 'info', base: { service: 'audio-dock' } });

// Phase 1: no live calls yet; bind component only
const sipLog = root.child({ component: 'sip' });
sipLog.info({ event: 'ua_started' }, 'SIP UserAgent started');

// Phase 2: bind per-call context when a call arrives
// const callLog = root.child({ component: 'bridge', callId: 'abc123', streamSid: 'MZxxx' });
// callLog.info({ event: 'rtp_packet_received', bytes: 160 }, 'RTP packet');
```

### SIP.js Registerer State Listener for Registration Confirmation

```typescript
// Source: https://github.com/onsip/SIP.js/blob/master/docs/api/sip.js.registereroptions.md (HIGH confidence)
import { Registerer, RegistererState } from 'sip.js';

const registerer = new Registerer(ua, {
  expires: 120,        // request 120s expiry from server
  refreshFrequency: 90 // re-register at 90% of server-granted expiry (auto)
});

// Listen for state changes to confirm registration
registerer.stateChange.addListener((state: RegistererState) => {
  switch (state) {
    case RegistererState.Registered:
      log.info({ event: 'sip_registered' }, 'SIP REGISTER 200 OK — registration confirmed');
      break;
    case RegistererState.Unregistered:
      log.warn({ event: 'sip_unregistered' }, 'SIP registration expired or rejected');
      break;
    case RegistererState.Terminated:
      log.error({ event: 'sip_terminated' }, 'SIP Registerer terminated');
      break;
  }
});

// Initial registration
await registerer.register();
```

### Docker Compose `network_mode: host` Pattern

```yaml
# Source: Docker documentation + sipgate trunking community configs (MEDIUM confidence)
# docker-compose.yml — Linux production
services:
  audio-dock:
    build: .
    network_mode: host     # Linux only — bypasses Docker NAT for RTP/SIP
    # On macOS: remove network_mode, add explicit port publish:
    # ports:
    #   - "10000-10099:10000-10099/udp"  # RTP port range (max 100 ports to avoid Docker proxy stall)
    env_file: .env
    restart: unless-stopped
```

---

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| `require('ws')` global polyfill (`global.WebSocket = require('ws')`) | Named import: `import { WebSocket } from 'ws'` then `globalThis.WebSocket = WebSocket` | Node.js 18+ (native WebSocket global added in Node.js 21, but ws is still needed for SIP.js compatibility) | Cleaner TypeScript typing; `ws` WebSocket class is the correct polyfill for SIP.js |
| Zod 3 (`import { z } from 'zod'`, `error.format()`) | Zod 4 (`import { z } from 'zod'`, `error.flatten()`) | 2025 (v4 stable mid-2025) | 14x faster parsing; `error.flatten()` replaces `error.format()` for field-level errors |
| pino 9.x | pino 10.x | Jan 2025 | Dropped Node.js 18 support; Node.js 20+ required; identical API for child loggers |
| SIP.js 0.20 (`autoStart`, `autoStop` on SimpleUser) | SIP.js 0.21 (new `SessionManager` class; `autoStart`/`autoStop` removed) | Oct 2024 | Use `UserAgent` + `Registerer` directly; avoid `SimpleUser` for server-side code |
| pnpm `install --frozen-lockfile` in Dockerfile COPY | `pnpm fetch` + `pnpm install --offline` pattern | ~2023 (pnpm fetch feature) | Docker layer only invalidates on lockfile change, not package.json change |

**Deprecated/outdated:**
- `SIP.js SimpleUser.register(options)` with `autoStart: true`: removed in 0.21.0; use `UserAgent.start()` then `Registerer.register()` separately.
- `zod@3 error.format()`: still works in Zod 4 but `error.flatten().fieldErrors` is the idiomatic v4 pattern.
- `sipjs-udp` and `sipjs-udp-transport` npm packages: target SIP.js 0.0.7/0.15.x; incompatible with 0.21.x API; do not use.
- `x-law` npm package (G.711 codec): archived by author August 2025; use `alawmulaw` instead (Phase 2 concern).

---

## Open Questions

1. **Exact sipgate trunking WSS endpoint URL**
   - What we know: `sipconnect.sipgate.de` is the registrar hostname (UDP port 5060, HIGH confidence from multiple official-ish configs). `wss://sip.sipgate.de` is cited as the WSS endpoint in community research (MEDIUM confidence).
   - What's unclear: Whether sipgate's SIP trunking product explicitly supports RFC 7118 SIP over WebSocket, and the exact WSS URL format (path required? subdomain?).
   - Recommendation: Before writing code, log into the sipgate trunking portal and find "SIP connection settings" or "WebSocket SIP" endpoint. Configure as `SIP_REGISTRAR` env var (WSS URL). If only UDP is available from the portal, the `transportConstructor` must be a custom TCP transport class (community gist approach) — this increases Phase 1 scope significantly.

2. **SIP.js `viaHost` timing issue (#1002) — does WebSocketTransport work around it?**
   - What we know: The issue is that `viaHost` must be set at UserAgent construction time, but the local socket address is only known after `connect()`. For TCP transports (raw net.Socket), this is a hard problem.
   - What's unclear: For WebSocket transports (`ws` library), the Via header is typically set to the WebSocket's origin hostname, not the local port. Since sipgate is WSS and SIP.js's WebSocketTransport sets Via to the configured `viaHost`, this may not be a problem in practice — the `viaHost` just needs to be the externally reachable IP, which is known at construction time (from `SDP_CONTACT_IP` env var).
   - Recommendation: Set `viaHost: config.SDP_CONTACT_IP` at UserAgent construction. The timing issue only matters for TCP raw-socket transports; WSS transports are unaffected because Via host is the caller's reachable hostname, not the ephemeral local port.

3. **pnpm store path inside Docker for pnpm fetch pattern**
   - What we know: The pnpm store is by default at `~/.local/share/pnpm/store` on Linux.
   - What's unclear: The exact COPY path needed in the Dockerfile to transfer the store from the `fetcher` stage to the `production` stage may vary by pnpm version.
   - Recommendation: Use `pnpm config get store-dir` in the Dockerfile to determine the store path dynamically, or use `--store-dir /app/.pnpm-store` to fix it.

---

## Sources

### Primary (HIGH confidence)
- SIP.js GitHub releases — version 0.21.2 confirmed, Oct 2024: https://github.com/onsip/SIP.js/releases
- SIP.js RegistererOptions API doc — `expires`, `refreshFrequency` fields: https://github.com/onsip/SIP.js/blob/master/docs/api/sip.js.registereroptions.md
- SIP.js UserAgentOptions — `transportConstructor`, `transportOptions`, `viaHost`: https://github.com/onsip/SIP.js/blob/master/docs/api/sip.js.useragentoptions.md
- SIP.js Transport interface — `connect()`, `disconnect()`, `send()`, `isConnected()` methods: https://github.com/onsip/SIP.js/blob/master/docs/transport/sip.js.transport.md
- Zod 4 official docs — `safeParse`, `flatten()`, import path, version: https://zod.dev/
- Zod 4 migration guide — `error.flatten()` replaces `error.format()`: https://zod.dev/v4/changelog
- pino GitHub releases — latest v10.3.1, Feb 9 2025; Node.js 20+ required: https://github.com/pinojs/pino/releases
- pino child loggers docs — `.child(bindings)` API: https://github.com/pinojs/pino/blob/main/docs/child-loggers.md
- pnpm Docker guide — `pnpm fetch` + `--offline` pattern: https://pnpm.io/docker
- tsx watch mode — `tsx --watch src/index.ts` syntax: https://tsx.is/watch-mode
- RFC 7118 — SIP over WebSocket; `Sec-WebSocket-Protocol: sip` requirement: https://www.rfc-editor.org/rfc/rfc7118.html

### Secondary (MEDIUM confidence)
- SIP.js issue #1002 — viaHost timing issue with TCP transports; `hackViaTcp` workaround: https://github.com/onsip/SIP.js/issues/1002
- sipgate FreePBX configuration — confirms `sipconnect.sipgate.de`, SIP-ID credentials format: https://teamhelp.sipgate.co.uk/hc/en-gb/articles/115005388769-FreePBX-Configuration-sipgate-SIP-Trunking
- AudioCodes LiveHub sipgate config — confirms port 5060 UDP for sipgate trunking: https://techdocs.audiocodes.com/livehub/Content/LiveHub/interop_configuration_sipgate_sip_trunk.htm
- Community TCP transport gist (phanmn) — targets SIP.js 0.13.7, patterns applicable to 0.21 custom transport: https://gist.github.com/phanmn/de0929fc4945c435cebfb6635366e87c
- Docker RTP port range problem — confirms `network_mode: host` requirement for RTP: https://www.engagespark.com/blog/rtp-port-ranges-for-freeswitch-in-docker/

### Tertiary (LOW confidence)
- `wss://sip.sipgate.de` as WSS SIP endpoint — cited in community search snippets; not found in official sipgate trunking portal documentation during research; must be verified directly.

---

## Metadata

**Confidence breakdown:**
- Standard stack (pino, Zod, tsx, pnpm, TypeScript): HIGH — all verified from official sources with current versions
- SIP.js Node.js transport path: MEDIUM — API documented; WSS polyfill pattern community-verified; `Sec-WebSocket-Protocol` requirement confirmed by RFC 7118
- sipgate WSS endpoint URL: MEDIUM — community sources agree; official sipgate trunking docs not accessible during research
- Docker pnpm fetch pattern: HIGH — official pnpm docs + multiple 2025 community sources
- Architecture patterns (Zod config, pino child logger, UserAgent/Registerer): HIGH — derived directly from official library APIs

**Research date:** 2026-03-03
**Valid until:** 2026-04-03 (30 days — stable library APIs; re-check sipgate endpoint from portal before implementation)
