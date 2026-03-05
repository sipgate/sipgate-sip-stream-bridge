# Phase 11: Node.js Equivalents - Research

**Researched:** 2026-03-05
**Domain:** TypeScript/Node.js — Twilio Media Streams mark/clear protocol + SIP OPTIONS keepalive
**Confidence:** HIGH

---

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions

**Mark sentinel strategy**
- Use a tagged union in the outboundDrain queue: change `queue: Buffer[]` to `Array<Buffer | { markName: string }>`. Direct translation of Go's `outboundFrame` tagged union.
- When a mark sentinel is dequeued in the drain's own timer callback, call `onMarkReady(markName)` and immediately reschedule (no 20ms wait) — the sentinel has no audio to pace.
- Empty-queue fast-path: if the queue is empty and the drain timer is idle when a mark arrives, call `onMarkReady` immediately (do NOT enqueue). Matches Go's MARK-02 behavior exactly.
- `WsClient.sendClear` flushes the audio queue (call `outboundDrain.stop()`) AND echoes all pending mark sentinels still in the queue immediately. Matches Go MARK-03.

**WsClient interface extension (MRKN-03)**
- Add three new methods to the `WsClient` interface: `onMark(handler: (markName: string) => void)`, `sendMark(markName: string)`, `sendClear()`.
- `CallManager` calls `ws.onMark(...)` and must not reference the raw WebSocket — interface only.
- Wiring of `onMarkReady` callback into `makeDrain` vs. post-construction registration: Claude's discretion.

**OPTIONS interval config (OPTN-01/OPTN-02/OPTN-03)**
- Add `SIP_OPTIONS_INTERVAL` to the Zod config schema as integer seconds: `z.coerce.number().int().positive().default(30)`. Consistent with the existing `SIP_EXPIRES` pattern.
- OPTIONS keepalive loop uses `setInterval` inside `createSipUserAgent`. The interval handle is stored in the closure and `clearInterval`-ed inside the existing `SipHandle.stop()` — no new interface method.
- Failure handling: 2 consecutive failures (timeout or non-401/407 error) → call `sendRegister()` to re-register. 401/407 → server is alive, skip re-registration, do NOT increment failure counter.

**Logging (carried from Phase 9 & 10 decisions)**
- Successful OPTIONS ping: Debug
- OPTIONS failure: Warn
- Re-registration triggered by OPTIONS keepalive: Warn
- Mark/clear event receipt and echo: Debug

**Test infrastructure**
- Vitest as the test framework — TypeScript-native, fast, no build step needed.
- Test files go in `node/test/` directory (matches existing `node/test/fd-leak.mjs` convention).
- Add `vitest` to devDependencies, add `"test": "vitest run"` script to `package.json`.
- TDD approach: failing test first (red), then implementation (green), committed as separate commits.

### Claude's Discretion
- Exact `makeDrain` signature change for mark wiring (callback at construction vs. post-construction registration)
- Test file naming convention within `node/test/`

### Deferred Ideas (OUT OF SCOPE)
- None — discussion stayed within phase scope.
</user_constraints>

---

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| MRKN-01 | Node.js implementation recognizes `mark` events and echoes the mark name after playout of all preceding audio frames | Tagged union drain queue; mark sentinel dequeue in timer callback; empty-queue fast-path for immediate echo |
| MRKN-02 | Node.js implementation recognizes `clear` events, flushes the audio queue, and echoes pending marks immediately | `sendClear()` calls `outboundDrain.stop()` + extracts mark sentinels before queue truncation |
| MRKN-03 | `WsClient` interface extended with `onMark`, `sendMark`, `sendClear` | Interface additions + `ws.on('message')` event dispatch extension; CallManager wiring |
| OPTN-01 | Node.js implementation sends SIP OPTIONS every 30s to sipgate | `setInterval` inside `createSipUserAgent`; `buildOptions()` helper reusing UDP socket |
| OPTN-02 | Timeout/error triggers re-registration; 401/407 does not | `applyOptionsResponse` pure-function state machine; consecutive-failure threshold=2 |
| OPTN-03 | OPTIONS keepalive stops cleanly on user-agent shutdown | `clearInterval` inside existing `SipHandle.stop()` |
</phase_requirements>

---

## Summary

Phase 11 ports the mark/clear and OPTIONS keepalive features from the Go implementation (Phases 9 and 10) to the Node.js reference implementation. The codebase is well-structured for these additions: `makeDrain`, `refreshTimer`, `sendRegister`, and the `ws.on('message')` handler are exact structural analogues to the Go components they mirror.

The key architectural difference from Go is that Node.js uses a single-threaded event loop rather than goroutines. This eliminates the need for Go's `sync.Mutex`, `markEchoQueue` channel, and `clearSignal` channel — in Node.js, all callbacks run sequentially in the event loop, so there are no concurrent mutation hazards. The mark/clear state machine is entirely in the timer callbacks and `ws.on('message')` handlers, and options keepalive is a simple `setInterval` with a closure-captured failure counter.

The test infrastructure gap is the most mechanically involved setup: Vitest must be installed into a pnpm `type: "module"` project with TypeScript, and Vitest's ESM support must be configured correctly. The pattern from the existing `fd-leak.mjs` test (direct `tsx/esm` execution) tells us the project prefers no build step; Vitest delivers this natively. Once Vitest is wired, the test patterns mirror the Go channel-logic tests: synchronous unit tests exercising pure functions and state mutations, no live sockets or real timers needed.

**Primary recommendation:** Follow the Go phase structure — Wave 0 creates Vitest infrastructure, Wave 1 adds mark/clear (wsClient + callManager), Wave 2 adds OPTIONS keepalive (userAgent + config). Each wave: failing tests first, then implementation.

---

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| vitest | ^3.x (latest) | Test runner | TypeScript-native, ESM-first, no build step, fastest option for this stack |
| @vitest/coverage-v8 | ^3.x | Coverage (optional) | Bundled with vitest ecosystem, no separate config |

**No new runtime dependencies** — all mark/clear and OPTIONS logic uses existing Node.js built-ins (`dgram`, `crypto`) and already-installed packages (`ws`, `zod`, `pino`).

### Current installed runtime deps (no change needed)
| Library | Version | Purpose |
|---------|---------|---------|
| ws | ^8.19.0 | WebSocket client — already handles mark/clear JSON dispatch |
| zod | ^4.3.6 | Config validation — add `SIP_OPTIONS_INTERVAL` field |
| pino | ^10.3.1 | Structured logging — `log.debug` / `log.warn` as decided |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| vitest | jest | Jest needs `ts-jest` transform or babel; extra config; slower. Vitest is already ESM-native. |
| vitest | node:test (built-in) | node:test has no TypeScript support without a transform; less ergonomic describe/it API |
| setInterval (OPTIONS) | separate timer class | Overkill; `refreshTimer` pattern already in production use in same file |

### Installation
```bash
# In node/ directory
pnpm add -D vitest
```

---

## Architecture Patterns

### Recommended Project Structure
```
node/
├── src/
│   ├── config/index.ts        # + SIP_OPTIONS_INTERVAL field
│   ├── ws/wsClient.ts         # + tagged union queue, mark methods
│   ├── sip/userAgent.ts       # + OPTIONS keepalive setInterval
│   └── bridge/callManager.ts  # + ws.onMark wiring
├── test/
│   ├── fd-leak.mjs            # existing
│   ├── wsClient.mark.test.ts  # NEW: MRKN-01/02/03 unit tests
│   └── userAgent.options.test.ts # NEW: OPTN-01/02/03 unit tests
└── package.json               # + "test": "vitest run"
```

### Pattern 1: Tagged Union Drain Queue (MRKN-01/02)

**What:** Replace `queue: Buffer[]` in `makeDrain` with `Array<Buffer | { markName: string }>`. The timer callback checks the dequeued item's type — if it has `markName`, call `onMarkReady` and reschedule immediately (no 20ms wait). If queue was empty when mark arrived and timer is idle, call `onMarkReady` directly without enqueuing.

**When to use:** Mirrors Go's `outboundFrame` tagged union (`audio []byte` vs `mark string`). TypeScript's union type gives a compile-checked discriminated union — no magic sentinel values.

**Node.js advantage over Go:** No `markEchoQueue` channel needed. `onMarkReady` callback fires synchronously from within the timer callback — same event-loop tick, no goroutine coordination.

**Example (drain queue type change):**
```typescript
// Source: derived from existing makeDrain in node/src/ws/wsClient.ts
type DrainItem = Buffer | { markName: string };

function makeDrain(
  sendPacket: (pkt: Buffer) => void,
  onMarkReady: (name: string) => void,
): {
  enqueue(pkt: Buffer): void;
  enqueueMark(name: string): void;
  stop(): void;
} {
  const queue: DrainItem[] = [];
  let timer: ReturnType<typeof setTimeout> | null = null;
  let base = 0;
  let count = 0;

  function schedule(): void {
    const now = Date.now();
    const deadline = base + count * PACKET_MS;
    const behind = now - deadline;
    let delay: number;
    if (behind > PACKET_MS) {
      base = now; count = 0; delay = 0;
    } else {
      delay = Math.max(0, deadline - now);
    }
    timer = setTimeout(() => {
      timer = null;
      const item = queue.shift();
      if (item === undefined) return; // queue empty — drain stops
      if (Buffer.isBuffer(item)) {
        count++;
        sendPacket(item);
        schedule();
      } else {
        // mark sentinel — fire callback, reschedule immediately (no pacing wait)
        onMarkReady(item.markName);
        if (queue.length > 0) schedule();
        // else: drain stops; next enqueue() restarts
      }
    }, delay);
  }

  return {
    enqueue(pkt: Buffer): void {
      queue.push(pkt);
      if (timer === null) { base = Date.now(); count = 0; schedule(); }
    },
    enqueueMark(name: string): void {
      if (queue.length === 0 && timer === null) {
        // MARK-02 fast-path: queue idle — fire immediately
        onMarkReady(name);
      } else {
        queue.push({ markName: name });
        if (timer === null) { base = Date.now(); count = 0; schedule(); }
      }
    },
    stop(): void {
      // MARK-03: extract mark sentinels BEFORE zeroing the queue
      for (const item of queue) {
        if (!Buffer.isBuffer(item)) onMarkReady(item.markName);
      }
      queue.length = 0;
      if (timer !== null) { clearTimeout(timer); timer = null; }
    },
  };
}
```

### Pattern 2: WsClient Interface Extension (MRKN-03)

**What:** Add `onMark`, `sendMark`, `sendClear` to the `WsClient` interface. Extend `ws.on('message')` in `onAudio` to also dispatch `mark` and `clear` events.

**Key insight:** `onAudio` currently registers the `ws.on('message')` listener. The mark handler needs access to `outboundDrain.enqueueMark` and `outboundDrain.stop`. This means `onMark`/`sendMark`/`sendClear` must be implemented in `buildClient()` alongside the drain setup — they share closure scope with `outboundDrain`.

**Wiring approach (Claude's discretion recommendation):** Accept `onMarkReady` as a parameter to `makeDrain` at construction time (shown in Pattern 1 above). Store the mark handler in a `let markHandler` variable inside `buildClient`, set via `onMark(handler)`. The `onMarkReady` callback from the drain calls `markHandler?.(name)`. This keeps the drain construction synchronous and avoids post-construction callback registration complexity.

**Example (interface additions):**
```typescript
// Source: derived from existing WsClient interface in node/src/ws/wsClient.ts
export interface WsClient {
  sendAudio(payload: Buffer): void;
  sendDtmf(digit: string): void;
  stop(): void;
  onAudio(handler: (payload: Buffer) => void): void;
  onDisconnect(handler: () => void): void;
  // Phase 11 additions:
  onMark(handler: (markName: string) => void): void;
  sendMark(markName: string): void;
  sendClear(): void;
}
```

**Message handler extension (within existing `onAudio` ws.on handler):**
```typescript
// Source: extending the existing ws.on('message') in wsClient.ts
ws.on('message', (data) => {
  let msg: {
    event: string;
    media?: { payload: string };
    mark?: { name: string };
  };
  try {
    msg = JSON.parse(data.toString()) as typeof msg;
  } catch { return; }

  switch (msg.event) {
    case 'media':
      if (msg.media?.payload !== undefined) {
        enqueueOutbound(Buffer.from(msg.media.payload, 'base64'));
      }
      break;
    case 'mark':
      if (msg.mark?.name !== undefined) {
        log.debug({ event: 'mark_received', name: msg.mark.name }, 'mark event received');
        outboundDrain.enqueueMark(msg.mark.name);
      }
      break;
    case 'clear':
      log.debug({ event: 'clear_received' }, 'clear event received');
      outboundDrain.stop(); // stop() echoes pending marks then flushes (MARK-03)
      break;
  }
});
```

### Pattern 3: sendMark Implementation (MRKN-03)

**What:** Mirror `sendDtmf` exactly — same `ws.send(JSON.stringify({...}))` pattern, same `seq++` sequence number. The mark echo event matches the Twilio Media Streams JSON schema.

```typescript
// Source: Twilio Media Streams spec + existing sendDtmf pattern in wsClient.ts
sendMark(markName: string): void {
  if (ws.readyState !== WebSocket.OPEN) {
    log.warn({ event: 'ws_send_skipped', reason: 'not_open' }, 'WS not OPEN — dropping mark echo');
    return;
  }
  ws.send(JSON.stringify({
    event: 'mark',
    sequenceNumber: String(seq++),
    streamSid,
    mark: { name: markName },
  }));
  log.debug({ event: 'mark_echoed', name: markName }, 'mark echoed');
},
```

### Pattern 4: OPTIONS Keepalive Loop (OPTN-01/02/03)

**What:** `setInterval` after successful registration inside `createSipUserAgent`. Mirrors `refreshTimer` closure pattern already in the file. Consecutive-failure counter is a closure-captured `let` variable — no mutex needed (event loop serialization).

**Behavioral spec (from Go's `applyOptionsResponse` pure function):**
- 401/407 response → server alive, reset `consecutiveFailures = 0`, no re-register
- Timeout (send callback error) or 5xx or 404 → `consecutiveFailures++`; if `>= 2` → call `sendRegister()`, reset counter
- 200 OK → reset `consecutiveFailures = 0`

**buildOptions helper:** Mirrors `buildRegister` — same Via/From/To/Call-ID/fromTag structure. Request line is `OPTIONS sip:${registrar} SIP/2.0`. No `Contact` or `Expires` headers. CSeq uses same `cseq++` counter with method `OPTIONS`.

**CSeq routing in socket.on('message'):** The existing handler already branches on `SIP/2.0` response prefix. Currently all `SIP/2.0` responses are assumed to be REGISTER responses. After Phase 11, REGISTER and OPTIONS responses both arrive. The CONTEXT.md notes: "Route by matching the `CSeq` field to distinguish them." Parse `CSeq` header — if it contains `OPTIONS`, route to OPTIONS response handler; if it contains `REGISTER`, route to existing REGISTER handler.

**OPTIONS timeout:** Implemented via response absence within a timeout window. Since the UDP socket uses callbacks (no request/response pairing), implement as: `setTimeout` when OPTIONS is sent; if response arrives before timeout, cancel the timer; if timer fires first, treat as timeout → increment failure counter.

**Alternative (simpler):** Use a per-ping timer. Send OPTIONS, set a `pingTimer = setTimeout(() => handleFailure(), 5000)`. On OPTIONS response arrival (identified by CSeq method=OPTIONS), `clearTimeout(pingTimer)` and process status code. This is the recommended approach — simple, direct, no state machine needed beyond the timer handle.

```typescript
// Source: pattern derived from Go registrar.go + existing refreshTimer pattern in userAgent.ts
let consecutiveFailures = 0;
let pingTimer: ReturnType<typeof setTimeout> | null = null;
let optionsTimer: ReturnType<typeof setInterval> | null = null;

// In socket.on('message') handler, add OPTIONS response routing:
const cseqVal = getHeader(raw, 'CSeq') ?? '';
if (cseqVal.includes('OPTIONS')) {
  if (pingTimer !== null) { clearTimeout(pingTimer); pingTimer = null; }
  const { status } = parseStatusLine(raw);
  handleOptionsResponse(status, null);
  return;
}

function handleOptionsResponse(status: number, err: Error | null): void {
  const isFailure = err !== null || status === 404 || status >= 500;
  const isAuth = status === 401 || status === 407;
  if (isFailure) {
    consecutiveFailures++;
    log.warn({ event: 'options_failure', consecutiveFailures }, 'OPTIONS keepalive: failure');
    if (consecutiveFailures >= 2) {
      consecutiveFailures = 0;
      log.warn({ event: 'options_reregister' }, 'OPTIONS keepalive: 2 consecutive failures — re-registering');
      sendRegister();
    }
  } else if (isAuth) {
    consecutiveFailures = 0;
    log.debug({ event: 'options_auth', status }, 'OPTIONS keepalive: 401/407 — server reachable');
  } else {
    consecutiveFailures = 0;
    log.debug({ event: 'options_ok' }, 'OPTIONS keepalive: success');
  }
}

optionsTimer = setInterval(() => {
  const optionsBuf = Buffer.from(buildOptions({
    user: config.SIP_USER, domain: config.SIP_DOMAIN,
    registrar, localIp, localPort, callId, fromTag,
    seq: cseq++, branch: `z9hG4bK${randomHex(6)}`,
  }));
  socket.send(optionsBuf, registrarPort, registrarIp, (err) => {
    if (err) { handleOptionsResponse(0, err); return; }
  });
  // Start per-ping timeout
  pingTimer = setTimeout(() => {
    pingTimer = null;
    handleOptionsResponse(0, new Error('OPTIONS timeout'));
  }, 5000);
}, config.SIP_OPTIONS_INTERVAL * 1000);

// In stop():
if (refreshTimer) clearTimeout(refreshTimer);
if (optionsTimer) clearInterval(optionsTimer);
if (pingTimer) clearTimeout(pingTimer);
socket.close();
```

### Pattern 5: CallManager Wiring (MRKN-03)

**What:** After `ws.onAudio(...)` in `handleInvite`, add `ws.onMark(...)`. On reconnect in `startWsReconnectLoop`, re-wire `newWs.onMark(...)` alongside `newWs.onAudio(...)`.

```typescript
// In handleInvite, after ws.onAudio(...):
ws.onMark((markName) => {
  // Forward mark echo to WS backend (ws.sendMark handles ws.readyState check)
  session.ws.sendMark(markName);
});

// In startWsReconnectLoop, after newWs.onAudio(...):
newWs.onMark((markName) => {
  session.ws.sendMark(markName);
});
```

**Note:** `session.ws` is swapped to `newWs` before the mark handler fires, so `session.ws.sendMark` correctly references the new connection on reconnect.

Wait — on closer reading: `session.ws = newWs` is set before re-wiring handlers. The `onMark` handler is set on `newWs` and captures `session.ws` by reference. After `session.ws = newWs`, `session.ws.sendMark` IS `newWs.sendMark`. This is self-consistent. But the handler registered via `ws.onMark(...)` in `handleInvite` is on the original `ws` object — when the WS reconnects, `newWs` needs its own `onMark` handler (same as `newWs.onAudio` already needs re-wiring).

### Anti-Patterns to Avoid
- **Registering mark handler on `ws` but not `newWs`:** The reconnect loop calls `newWs.onAudio` and `newWs.onDisconnect` but currently does NOT call `newWs.onMark`. This must be added in `startWsReconnectLoop` or marks will stop working after first WS reconnect.
- **Stopping the outbound drain timer on clear (MARK-04):** `outboundDrain.stop()` currently stops the timer. After MARK-03, calling `stop()` for clear MUST be followed by restarting the drain if audio is subsequently enqueued — which already happens because `enqueue()` restarts the timer when `timer === null`. The RTP pacer is unaffected (it's the inbound drain from WS → RTP, not the outbound one). Wait: re-reading the code: `outboundDrain` is WS → RTP. The `inboundDrain` is RTP → WS. A `sendClear()` from the WS backend means clear the outbound queue (WS-sent audio to RTP direction). So `outboundDrain.stop()` is correct for clear. The inbound drain (RTP → WS) is unaffected — this matches Go's "RTP pacer never stops during clear."
- **Assuming CSeq counter monotonicity across OPTIONS and REGISTER:** `cseq++` is shared between REGISTER and OPTIONS messages. This is RFC-correct (per-dialog, and these are both out-of-dialog requests using the same Call-ID). The Go implementation also shares a single cseq. However, sipgate may reject an OPTIONS immediately followed by a REGISTER if the CSeq regresses. Since OPTIONS and REGISTER use different CSeq method fields, most servers track them independently in practice. The existing Node.js code already uses a shared `cseq` for REGISTER only — OPTIONS will extend the same counter, which is safe.
- **Not clearing `pingTimer` in `stop()`:** If `stop()` fires while a 5-second OPTIONS ping timeout is pending, the timeout would fire on a closed socket. Always clear all three timers (`refreshTimer`, `optionsTimer`, `pingTimer`) in `stop()`.

---

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| TypeScript test runner with ESM support | Custom ts-node + mocha config | Vitest | Vitest natively handles `type: "module"` pnpm projects with zero config |
| Test assertion library | Custom matchers | Vitest built-in `expect` | Already included in vitest; `expect(x).toBe(y)` / `toEqual` / `toStrictEqual` |
| Fake timer for OPTIONS interval tests | Manual `setInterval` mock | `vi.useFakeTimers()` from vitest | Built-in — advance timers without real waiting |
| Type-checking in test files | babel transform | Vitest with tsx/esbuild transform (built-in) | Vitest transforms TypeScript natively via esbuild |

**Key insight:** The existing `fd-leak.mjs` test runs via `node --import tsx/esm` which requires `tsx` to be installed. Vitest does NOT use `tsx` — it uses its own esbuild-based transform. Both approaches coexist without conflict.

---

## Common Pitfalls

### Pitfall 1: Vitest ESM Config with pnpm `type: "module"`
**What goes wrong:** Vitest may fail to find/transform `.ts` files if `vitest.config.ts` is not present, or may error on `import.meta` in source files.
**Why it happens:** pnpm `type: "module"` makes `.js` files ESM by default. Vitest's default config works with this, but TypeScript's `moduleResolution: "NodeNext"` requires explicit `.js` extensions in imports. Vitest's bundler handles this transparently when `resolve.conditions` includes `node` and `module`.
**How to avoid:** No `vitest.config.ts` is needed for basic usage. `"test": "vitest run"` in `package.json` is sufficient. Vitest auto-discovers test files matching `**/*.test.ts`. If `.ts` import resolution fails, add a minimal `vitest.config.ts`:
```typescript
import { defineConfig } from 'vitest/config';
export default defineConfig({
  test: { environment: 'node' }
});
```
**Warning signs:** `Error: Cannot find module` for local imports in test files; `SyntaxError: Cannot use import statement`.

### Pitfall 2: Vitest cannot mock `setInterval` without `vi.useFakeTimers()`
**What goes wrong:** Tests for OPTIONS keepalive that call `createSipUserAgent` will hang waiting for real 30-second intervals, or the function will not return because it awaits a real registration.
**Why it happens:** `createSipUserAgent` returns a Promise that resolves only after a successful REGISTER 200 OK — which requires real UDP. The OPTIONS `setInterval` starts only after the Promise resolves. Tests should NOT test the full flow end-to-end — test the pure `applyOptionsResponse`-equivalent logic as a standalone function, mirroring Go's `applyOptionsResponse` extraction.
**How to avoid:** Extract the OPTIONS response classification logic into a standalone pure function (analogous to Go's `applyOptionsResponse`) that takes `{consecutiveFailures, status, isError}` and returns `{newCount, triggerRegister}`. Test this function directly without any timers or sockets.

### Pitfall 3: `stop()` Called on `outboundDrain` for Clear — Timer Restart
**What goes wrong:** After `sendClear()` calls `outboundDrain.stop()`, subsequent audio from the WS backend is not played because the timer never restarts.
**Why it happens:** `stop()` sets `timer = null` and `queue.length = 0`. The next call to `enqueue()` checks `if (timer === null)` and DOES restart — this is already the correct behavior in the existing `makeDrain`. The pitfall is only if `stop()` is overridden to NOT nullify the timer.
**How to avoid:** Ensure `stop()` always sets `timer = null` (it already does). The existing implementation is correct — document this clearly in the plan.

### Pitfall 4: Mark Handler Not Re-Wired on WS Reconnect
**What goes wrong:** After a WS reconnect, marks received from the new WS connection are silently ignored.
**Why it happens:** `newWs.onMark(...)` is not called in `startWsReconnectLoop`, unlike `newWs.onAudio(...)` which is explicitly re-wired.
**How to avoid:** Treat `onMark` wiring as parallel to `onAudio` wiring — both must appear in the initial `handleInvite` setup AND in the `startWsReconnectLoop` success path.

### Pitfall 5: `sendClear` echoes pending marks BEFORE calling `stop()` vs. AFTER
**What goes wrong:** If `stop()` sets `queue.length = 0` before mark sentinels are extracted, they are lost.
**Why it happens:** Current `stop()` does `queue.length = 0` which destroys all items immediately.
**How to avoid:** The modified `stop()` MUST iterate the queue for mark sentinels and call `onMarkReady` for each BEFORE executing `queue.length = 0`. The code example in Pattern 1 above shows this correctly.

### Pitfall 6: OPTIONS CSeq Routing — Inbound OPTIONS vs. Outbound OPTIONS Response
**What goes wrong:** The existing `socket.on('message')` handler already responds to inbound `OPTIONS` requests (sipgate sending keepalives to us, line 236-255 in `userAgent.ts`). An OPTIONS *response* (to our outbound keepalive) arrives as `SIP/2.0 200 OK` with `CSeq: N OPTIONS`, not as `OPTIONS sip:...`. The existing `SIP/2.0` branch currently assumes ALL responses are REGISTER responses.
**Why it happens:** The handler checks `firstLine.startsWith('SIP/2.0')` and then directly handles 401/407/200 as REGISTER responses. After OPTIONS keepalive is added, a `200 OK` to our OPTIONS ping would be misrouted into the REGISTER 200 OK handler, potentially triggering a spurious `refreshTimer` reset.
**How to avoid:** Inside the `SIP/2.0` branch, read the `CSeq` header first. If `CSeq` contains `OPTIONS`, route to the OPTIONS response handler. If `CSeq` contains `REGISTER`, continue to existing REGISTER logic. The `getHeader(raw, 'CSeq')` helper is already available.

---

## Code Examples

### Vitest test file structure (TypeScript, ESM, no config needed)
```typescript
// Source: vitest docs + project type:module pattern
// node/test/wsClient.mark.test.ts
import { describe, it, expect } from 'vitest';

describe('outboundDrain mark sentinel (MRKN-01)', () => {
  it('dequeues mark sentinel and calls onMarkReady', () => {
    const fired: string[] = [];
    const onMarkReady = (name: string) => fired.push(name);
    // ... test makeDrain with mock sendPacket + onMarkReady
    expect(fired).toEqual(['sentinel-name']);
  });
});
```

### applyOptionsResponse pure function (Node.js equivalent of Go's pure function)
```typescript
// Source: derived from Go registrar.go applyOptionsResponse
// Extracted for unit testability — no timers, no sockets
export function applyOptionsResponse(
  consecutiveFailures: number,
  status: number,      // HTTP-style SIP status code; 0 = timeout/error
  isError: boolean,    // true if send failed or response timed out
): { newCount: number; triggerRegister: boolean } {
  const threshold = 2;
  const isAuth = status === 401 || status === 407;
  const isFailure = isError || status === 404 || status >= 500;

  if (isAuth) return { newCount: 0, triggerRegister: false };
  if (isFailure) {
    const next = consecutiveFailures + 1;
    if (next >= threshold) return { newCount: 0, triggerRegister: true };
    return { newCount: next, triggerRegister: false };
  }
  return { newCount: 0, triggerRegister: false };
}
```

### buildOptions helper
```typescript
// Source: derived from buildRegister in userAgent.ts
function buildOptions(p: {
  user: string; domain: string; registrar: string;
  localIp: string; localPort: number;
  callId: string; fromTag: string; seq: number; branch: string;
}): string {
  const sipUri = `sip:${p.user}@${p.domain}`;
  const registrarUri = `sip:${p.registrar}`;
  const lines = [
    `OPTIONS ${registrarUri} SIP/2.0`,
    `Via: SIP/2.0/UDP ${p.localIp}:${p.localPort};branch=${p.branch};rport`,
    'Max-Forwards: 70',
    `From: <${sipUri}>;tag=${p.fromTag}`,
    `To: <${sipUri}>`,
    `Call-ID: ${p.callId}`,
    `CSeq: ${p.seq} OPTIONS`,
    `User-Agent: ${USER_AGENT}`,
    'Content-Length: 0',
    '',
  ];
  return lines.join('\r\n') + '\r\n';
}
```

### Vitest fake timer for OPTIONS interval test
```typescript
// Source: vitest docs vi.useFakeTimers()
import { vi, it, expect, afterEach } from 'vitest';

afterEach(() => { vi.useRealTimers(); });

it('OPTIONS interval fires after 30s', () => {
  vi.useFakeTimers();
  const sends: number[] = [];
  // ... set up a minimal fixture that captures OPTIONS sends
  vi.advanceTimersByTime(30_000);
  expect(sends).toHaveLength(1);
  vi.advanceTimersByTime(30_000);
  expect(sends).toHaveLength(2);
});
```

---

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| jest + ts-jest for TypeScript | vitest (ESM-native) | 2023+ | No transform config, faster, better ESM support |
| `queue: Buffer[]` in drain | `Array<Buffer \| { markName: string }>` | Phase 11 | Type-safe tagged union, no magic sentinel values |
| `SIP/2.0` responses assumed REGISTER | CSeq-routed dispatch | Phase 11 | Required for OPTIONS keepalive coexistence |

**Deprecated/outdated:**
- `// Ignore non-media events (mark, clear, etc.)` comment in `wsClient.ts`: This line will be removed in Phase 11 as mark/clear handling is added.

---

## Open Questions

1. **Vitest version compatibility with zod v4.3.6 and pnpm lockfileVersion 9.0**
   - What we know: zod v4 is a recent major version (released early 2025). Vitest 3.x is compatible with all pnpm lockfile versions.
   - What's unclear: Whether any vitest peer dependency conflicts with zod v4's new exports map.
   - Recommendation: Run `pnpm add -D vitest` and check for peer dependency warnings. If any, they will be non-blocking (devDependency isolation).

2. **OPTIONS response timeout window (5 seconds vs. other value)**
   - What we know: Go uses `context.WithTimeout(ctx, 10*time.Second)` for OPTIONS. CONTEXT.md does not specify a timeout for Node.js.
   - What's unclear: Whether 5 or 10 seconds is more appropriate for the `pingTimer` timeout.
   - Recommendation: Use 5000ms (5s). The sipgate network is low-latency; 5s is more than enough and avoids timer overlap with the 30s interval.

3. **CSeq routing: shared counter between REGISTER and OPTIONS**
   - What we know: RFC 3261 §20.16 specifies CSeq is per-dialog. Both REGISTER and OPTIONS here are out-of-dialog requests using the same Call-ID and from-tag. RFC allows a single monotonic counter across methods for the same dialog.
   - What's unclear: Whether sipgate enforces strict per-method CSeq monotonicity.
   - Recommendation: Use shared `cseq++` counter (matches Go behavior). If sipgate rejects, switch to separate counters — but this is unlikely.

---

## Validation Architecture

> Note: `workflow.nyquist_validation` is not present in `.planning/config.json` — this section describes the test infrastructure the planner should wire per the locked decisions.

### Test Framework
| Property | Value |
|----------|-------|
| Framework | Vitest (to be installed) |
| Config file | none — `vitest run` discovers `node/test/**/*.test.ts` automatically |
| Quick run command | `cd node && pnpm test` |
| Full suite command | `cd node && pnpm test` |

### Phase Requirements → Test Map
| Req ID | Behavior | Test Type | Automated Command | File Exists? |
|--------|----------|-----------|-------------------|-------------|
| MRKN-01 | Mark sentinel dequeued → `onMarkReady` called after preceding audio | unit | `pnpm test --reporter=verbose` | Wave 0 |
| MRKN-01 | Empty queue fast-path → `onMarkReady` called immediately | unit | `pnpm test` | Wave 0 |
| MRKN-02 | `sendClear()` → `outboundDrain.stop()` → pending marks echoed immediately | unit | `pnpm test` | Wave 0 |
| MRKN-03 | `WsClient.sendMark` writes correct JSON schema (event="mark") | unit | `pnpm test` | Wave 0 |
| OPTN-01 | OPTIONS failure classification state machine (pure function) | unit | `pnpm test` | Wave 0 |
| OPTN-02 | 2 consecutive failures triggers re-registration; 401/407 does not | unit | `pnpm test` | Wave 0 |
| OPTN-03 | `stop()` clears `optionsTimer` and `pingTimer` | unit | `pnpm test` | Wave 0 |

### Wave 0 Gaps
- [ ] `node/test/wsClient.mark.test.ts` — covers MRKN-01, MRKN-02, MRKN-03
- [ ] `node/test/userAgent.options.test.ts` — covers OPTN-01, OPTN-02, OPTN-03
- [ ] `node/package.json` — add `"test": "vitest run"` script
- [ ] Vitest install: `cd node && pnpm add -D vitest`

---

## Sources

### Primary (HIGH confidence)
- Direct codebase reading: `node/src/ws/wsClient.ts` — full `makeDrain` and `WsClient` implementation
- Direct codebase reading: `node/src/sip/userAgent.ts` — full `createSipUserAgent`, `refreshTimer`, `sendRegister`, socket message dispatch
- Direct codebase reading: `node/src/bridge/callManager.ts` — full reconnect loop, `onAudio`/`onDisconnect` wiring pattern
- Direct codebase reading: `node/src/config/index.ts` — Zod schema with `SIP_EXPIRES` pattern
- Direct codebase reading: `go/internal/bridge/session.go` — behavioral spec for mark/clear (tagged union, empty-queue fast-path, drain on clear)
- Direct codebase reading: `go/internal/sip/registrar.go` — behavioral spec for OPTIONS keepalive (`applyOptionsResponse` pure function, consecutive-failure threshold, 401/407 treatment)
- Direct codebase reading: `go/internal/bridge/session_mark_test.go` — test patterns for channel-logic testing
- Direct codebase reading: `go/internal/sip/registrar_test.go` — pure-function test patterns for OPTIONS classification
- `.planning/phases/11-node.js-equivalents/11-CONTEXT.md` — all locked decisions

### Secondary (MEDIUM confidence)
- Vitest documentation (vitest.dev) — Vitest 3.x ESM support with `type: "module"` pnpm projects: standard configuration, zero-config discovery of `**/*.test.ts`
- `node/package.json` — confirms `"type": "module"`, Node >= 22, existing devDependencies (no vitest yet)
- `node/pnpm-lock.yaml` — lockfileVersion 9.0, confirms pnpm 9.x

### Tertiary (LOW confidence)
- None

---

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH — all packages are directly observed in the codebase or are well-known
- Architecture: HIGH — implementation patterns derived from direct reading of both Go reference and Node.js source
- Pitfalls: HIGH — CSeq routing pitfall and mark re-wiring pitfall are directly observable from code structure; timer cleanup pitfall is standard Node.js practice

**Research date:** 2026-03-05
**Valid until:** 2026-06-05 (stable domain — no fast-moving libraries)
