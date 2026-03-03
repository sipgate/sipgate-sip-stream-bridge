# Phase 3: Resilience - Research

**Researched:** 2026-03-03
**Domain:** WebSocket reconnect loop with exponential backoff, RTP silence injection, FD lifecycle verification
**Confidence:** HIGH — implementation is pure Node.js with no new library dependencies; all patterns verified directly against existing codebase

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions

**Phase boundary**
- Replace current "WS disconnect → send BYE immediately" behavior with a reconnect loop that keeps the SIP call alive during transient WebSocket drops
- RTP socket and SIP dialog remain open throughout the reconnect window
- Initial connection failure at call setup (no WS within 2s) still returns 503 — reconnect only applies to mid-call drops

**Retry budget**
- Total reconnect window: **30 seconds** — if WS is not reconnected within 30s, give up and send SIP BYE
- Exponential backoff: 1s → 2s → 4s → 4s → 4s... (cap at **4 seconds** per attempt)
- On giving up: `terminateSession(session, 'ws_reconnect_failed', true)` — sends BYE, stop event not possible (WS gone)

**Stream identity after reconnect**
- **Same `streamSid` and `callSid`** in the fresh `connected` + `start` events after reconnect
- Backend can detect the gap (sequence number resets or timestamp jump) and continue the conversation
- The `connected` event is sent first, then `start`, before any `media` events resume (per WSR-02)

**Caller experience during reconnect**
- **Send RTP silence** (0xFF μ-law, 160 bytes per 20ms packet) to the caller throughout the reconnect window
- Prevents dead-air / phone comfort-noise / call-quality warnings on the caller side
- Silence stops as soon as WS reconnects and first `media` event arrives from backend
- Inbound RTP from caller is **dropped** during reconnect window — not buffered (WSR-03, consistent with Phase 2 "drop not buffer" pattern)

### Claude's Discretion
- Exact timer/interval implementation (setTimeout loop vs recursive retry)
- Whether the silence sender is a separate setInterval or integrated into the reconnect loop
- FD leak test implementation details (how to count open FDs on Linux/macOS)

### Deferred Ideas (OUT OF SCOPE)
None — discussion stayed within phase scope
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| WSR-01 | If WebSocket disconnects during an active call, service reconnects with exponential backoff | Reconnect loop replaces `onDisconnect → terminateSession` at `callManager.ts:468`; uses `createWsClient` with same `params`; backoff 1s/2s/4s/4s... capped; 30s total budget |
| WSR-02 | After WebSocket reconnect, service re-sends `connected` then `start` before forwarding audio | `createWsClient` already sends both events on open; re-calling it with same `params` satisfies this; audio gate must block until new WsClient is wired |
| WSR-03 | Audio arriving from RTP during WebSocket reconnect window is dropped (not buffered indefinitely) | `wsReconnecting` flag on `CallSession` causes `rtp.on('audio')` handler to drop packets silently; existing `startForwarding()` gate already provides a model |
</phase_requirements>

## Summary

Phase 3 is a targeted surgical change to `callManager.ts` and a new helper `reconnectWs()` (or inline logic). The core problem is that line 468 of `callManager.ts` currently sends BYE on any WS disconnect. This needs to be replaced with a reconnect loop that attempts `createWsClient()` up to ~8 times over 30 seconds, sends silence to the caller throughout, and only calls `terminateSession` if all attempts fail.

No new npm dependencies are needed. All building blocks are already present: `createWsClient` constructs a fresh WsClient with connected+start events, `rtp.sendAudio(Buffer.alloc(160, 0xff))` sends μ-law silence (already used for NAT hole-punch), and `CallSession` can carry a boolean flag to gate RTP forwarding during the reconnect window. The only genuinely new mechanism is a recursive-setTimeout backoff loop and a cross-platform FD count function for the integration test.

FD leak verification (WSR-03 adjacent, CON-02) requires counting open file descriptors before and after 20 simulated calls. On Linux this uses `fs.readdirSync('/proc/self/fd').length`. On macOS (development) this falls back to `execSync('lsof -p PID').split('\n').length - 1`. The test-listener.js file already in the repo root suggests manual integration testing is the established pattern; the FD test follows that pattern using a Node.js script rather than a Jest/Vitest suite.

**Primary recommendation:** Implement the reconnect loop as a private `startWsReconnectLoop` method on `CallManager`, replacing the `ws.onDisconnect` lambda at line 468. Use `setInterval` for silence injection, clearing it on reconnect or giveup. Write the FD count test as a standalone Node.js script in `test/` (no test framework needed).

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Node.js built-in: `dgram` | Node 22 | RTP socket lifecycle (already used) | Zero-dep; already proven in `rtpHandler.ts` |
| Node.js built-in: `crypto` | Node 22 | Per-reconnect branch id (already used) | Zero-dep |
| `ws` | ^8.19.0 (locked) | WebSocket client for reconnects | Already in package.json; `createWsClient` wraps it |
| Node.js built-in: `fs` + `child_process` | Node 22 | FD count in test script | `/proc/self/fd` on Linux; `lsof` fallback on macOS |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `pino` | ^10.3.1 (locked) | Structured logging for reconnect attempts | All reconnect events must use existing `callLog` |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| Custom setTimeout loop | `p-retry`, `async-retry` | No new deps needed; problem is simple enough that a 15-line loop is clearer |
| lsof for FD count | Node.js `process._getActiveHandles()` | `_getActiveHandles()` counts JS handle objects, not OS file descriptors — does not catch leaked dgram sockets that have been GC'd but not closed |

**Installation:** No new packages required.

## Architecture Patterns

### Recommended Project Structure
```
src/
├── bridge/
│   └── callManager.ts     # Add startWsReconnectLoop() private method here
├── ws/
│   └── wsClient.ts        # No changes needed — createWsClient already correct
├── rtp/
│   └── rtpHandler.ts      # No changes needed
test/
└── fd-leak.mjs            # New: standalone FD count script (no test framework)
```

### Pattern 1: Recursive setTimeout Backoff Loop

**What:** A private async method that attempts `createWsClient` repeatedly, doubling delay each time up to 4s, aborting after 30s total elapsed.

**When to use:** Any time a reconnect loop needs a finite budget with exponential backoff.

**Example:**
```typescript
// Source: derived from existing createWsClient pattern + Phase 3 CONTEXT.md spec
private async startWsReconnectLoop(
  session: CallSession,
  params: WsCallParams,
): Promise<void> {
  const BUDGET_MS = 30_000;
  const CAP_MS    = 4_000;
  const started   = Date.now();
  let   delay     = 1_000;

  // Send silence to caller throughout reconnect window
  const silenceInterval = setInterval(() => {
    session.rtp.sendAudio(Buffer.alloc(160, 0xff));
  }, 20);

  const cleanup = (): void => clearInterval(silenceInterval);

  const attempt = async (n: number): Promise<void> => {
    if (!this.sessions.has(session.callId)) {
      // Session was terminated externally (e.g. caller sent BYE during reconnect)
      cleanup();
      return;
    }

    session.log.info({ event: 'ws_reconnect_attempt', attempt: n, delay }, 'Attempting WS reconnect');

    try {
      const newWs = await createWsClient(this.config.WS_TARGET_URL, params, session.log);
      cleanup();
      session.wsReconnecting = false;

      // Re-wire audio bridge with new WsClient
      session.ws = newWs;
      newWs.onAudio((payload) => session.rtp.sendAudio(payload));
      newWs.onDisconnect(() => {
        this.startWsReconnectLoop(session, params).catch((err: unknown) => {
          session.log.error({ err }, 'Reconnect loop error');
        });
      });

      session.log.info({ event: 'ws_reconnected', attempt: n }, 'WS reconnected — resuming audio bridge');
    } catch {
      const elapsed = Date.now() - started;
      if (elapsed + delay >= BUDGET_MS) {
        cleanup();
        session.log.warn({ event: 'ws_reconnect_failed', elapsed }, 'WS reconnect budget exhausted — sending BYE');
        this.terminateSession(session, 'ws_reconnect_failed', true);
        return;
      }
      session.log.info({ event: 'ws_reconnect_wait', delay, elapsed }, 'WS reconnect failed — waiting before retry');
      await new Promise<void>((res) => setTimeout(res, delay));
      delay = Math.min(delay * 2, CAP_MS);
      await attempt(n + 1);
    }
  };

  await attempt(1);
}
```

### Pattern 2: wsReconnecting Gate on CallSession

**What:** A boolean flag on `CallSession` that the RTP audio handler checks. When true, inbound audio is dropped silently rather than forwarded to a non-existent WsClient.

**When to use:** Any time the WS link is broken but the RTP socket must remain bound.

**Example:**
```typescript
// In handleInvite, when wiring audio bridge (replaces current rtp.on('audio') callback):
rtp.on('audio', (payload: Buffer) => {
  if (session.wsReconnecting) return; // drop — WSR-03
  if (firstRtp) {
    callLog.info({ event: 'first_rtp_audio', bytes: payload.length }, 'First RTP audio packet received');
    firstRtp = false;
  }
  session.ws.sendAudio(payload);
});

// In onDisconnect handler (replaces terminateSession call at line 468):
ws.onDisconnect(() => {
  session.wsReconnecting = true;
  void this.startWsReconnectLoop(session, params).catch((err: unknown) => {
    session.log.error({ err }, 'Reconnect loop unhandled error');
  });
});
```

### Pattern 3: Cross-Platform FD Count for Integration Test

**What:** A standalone Node.js ESM script that creates N simulated calls (each allocating+binding a dgram socket, then disposing), counts FDs before and after, and asserts the count returns to baseline.

**When to use:** Verifying CON-02 / WSR-03 "no FD leak after sequential calls".

**Example:**
```javascript
// test/fd-leak.mjs
import fs from 'node:fs';
import { execSync } from 'node:child_process';

function getFdCount() {
  try {
    // Linux
    return fs.readdirSync('/proc/self/fd').length;
  } catch {
    // macOS / BSD
    const lines = execSync(`lsof -p ${process.pid} 2>/dev/null`).toString().trim().split('\n');
    return Math.max(0, lines.length - 1); // subtract header
  }
}

// Use createRtpHandler + dispose() in a loop; assert getFdCount() returns to baseline
```

### Anti-Patterns to Avoid

- **Re-using old WsClient after reconnect:** The old `session.ws` object references a dead WebSocket. After reconnect, `session.ws` MUST be replaced with the new WsClient. Any lingering `onAudio` or `onDisconnect` handlers on the old client will fire into a closed socket.
- **Buffering inbound RTP during reconnect:** The decision is explicit: drop, not buffer. Buffering would accumulate ~160 bytes every 20ms = ~240 KB per 30s reconnect window, and replay would cause garbled audio. Drop is correct.
- **Calling `ws.stop()` on the dead WsClient:** `terminateSession` calls `session.ws.stop()`, which tries to send a `stop` event and then close. During reconnect-failure path, the WS is already gone. `ws.stop()` handles this: it calls `ws.terminate()` if not OPEN. This is safe.
- **Double-reconnect race:** If BYE arrives while reconnect is in progress, `session.callId` is removed from `this.sessions`. The reconnect loop checks `this.sessions.has(session.callId)` at the top of each attempt and exits early if the session is gone. This prevents zombie reconnects.
- **Not re-wiring DTMF:** The DTMF handler (`rtp.on('dtmf', ...)`) references `ws` by closure in current code. After reconnect, `session.ws` changes but the closure still holds the old reference. DTMF must be re-wired the same way as audio — either reference `session.ws` indirectly through the session object, or re-register the handler.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Exponential backoff | Custom state machine with flags | Simple recursive setTimeout loop (15 lines) | Problem is simple; libraries add ceremony |
| FD leak detection | Complex test harness | `/proc/self/fd` count or `lsof` (5 lines) | Direct OS-level count is the ground truth |
| Silence generation | Pre-built silence buffer pool | `Buffer.alloc(160, 0xff)` each packet | Already used for NAT hole-punch in Phase 2; no allocation overhead concern at 50 pps |

**Key insight:** This phase adds ~100 lines of logic spread across 2 existing files plus 1 new test script. No new abstractions are needed beyond a flag on `CallSession` and a private method on `CallManager`.

## Common Pitfalls

### Pitfall 1: Old WsClient Reference in DTMF Closure

**What goes wrong:** The DTMF listener (`rtp.on('dtmf', ({ digit }) => ws.sendDtmf(digit))`) captures `ws` from the `handleInvite` closure. After reconnect, `ws` still refers to the dead WsClient. DTMF digits are silently dropped (or cause a warn-log storm as sendDtmf bails on `ws.readyState !== OPEN`).

**Why it happens:** JavaScript closure captures the variable at declaration time; reassigning `session.ws` later does not update the closure.

**How to avoid:** Route DTMF through `session.ws` (the mutable property): `rtp.on('dtmf', ({ digit }) => session.ws.sendDtmf(digit))`. The RTP handler is registered once; always using `session.ws` means it automatically picks up the replacement.

**Warning signs:** DTMF works before first disconnect but not after reconnect. Warn log "WS not OPEN — dropping DTMF digit" after a reconnect that otherwise succeeded.

### Pitfall 2: Reconnect Loop Continues After BYE

**What goes wrong:** The caller sends BYE while the reconnect loop is sleeping between attempts. `handleBye` calls `terminateSession` which removes the session from `this.sessions`. The reconnect loop wakes from sleep, calls `createWsClient`, succeeds, and tries to set up a new audio bridge for a session that no longer exists.

**Why it happens:** The reconnect loop holds a reference to `session` directly; `this.sessions.delete` doesn't null the reference.

**How to avoid:** Guard every attempt with `if (!this.sessions.has(session.callId)) { cleanup(); return; }` before calling `createWsClient`.

**Warning signs:** "WS reconnected" log followed by "Call terminated" immediately with no BYE sent. Or a second `ws_connected` log after the call ended.

### Pitfall 3: setInterval Silence Not Cleared on Reconnect

**What goes wrong:** The silence setInterval keeps running after WS reconnects. The caller continues receiving silence even though the WS backend is now sending real audio.

**Why it happens:** Two concurrent audio senders: `silenceInterval` fires every 20ms AND `ws.onAudio` sends real audio. Depending on timing, silence packets interleave with real audio, causing clicks and distortion.

**How to avoid:** `clearInterval(silenceInterval)` immediately when `createWsClient` resolves (before re-wiring `onAudio`). The `cleanup()` helper in the pattern above does this.

**Warning signs:** Caller hears choppy/garbled audio after reconnect despite real WS audio flowing. RTP `outSeq` advances faster than expected (both sources incrementing).

### Pitfall 4: FD Count Inflated by /proc/self/fd Itself

**What goes wrong:** On Linux, `fs.readdirSync('/proc/self/fd')` opens `/proc/self/fd` as a directory, which itself creates a file descriptor. The count includes the directory-read FD, inflating the reading by 1 during the measurement call.

**Why it happens:** The readdir syscall creates a directory file descriptor that shows up in `/proc/self/fd` transiently.

**How to avoid:** Either subtract 1 from the Linux count (`fs.readdirSync('/proc/self/fd').length - 1`) or take the baseline and final measurements using the same function and compare deltas rather than absolute values. Using deltas is more robust.

**Warning signs:** Baseline FD count fluctuates by 1 between successive calls to the measurement function.

### Pitfall 5: terminateSession Called Twice (Reconnect + BYE Race)

**What goes wrong:** During reconnect, the caller sends BYE. `handleBye` calls `terminateSession` (sendBye=false). Simultaneously the reconnect loop's budget expires and calls `terminateSession` (sendBye=true). The second call hits the idempotency guard (`if (!this.sessions.has(...)) return`) — but only if the session was already deleted. Since `terminateSession` deletes the session as its first act, the race is safe.

**Why it happens:** Two async code paths both hold a reference to the same `session` object.

**How to avoid:** The existing idempotency guard in `terminateSession` already handles this correctly. No additional protection needed.

**Warning signs:** Double BYE in SIP trace (would only happen if the guard is removed or bypassed).

## Code Examples

Verified patterns from the codebase:

### Silence Buffer (already used in Phase 2)
```typescript
// Source: callManager.ts Phase 2 — NAT hole-punch at line ~332
rtp.sendAudio(Buffer.alloc(160, 0xff)); // μ-law silence (0xFF = silence byte for PCMU)
```

### Current WS Disconnect Handler (line to replace)
```typescript
// Source: src/bridge/callManager.ts line 468
ws.onDisconnect(() => this.terminateSession(session, 'ws_disconnect', true));
```

### createWsClient Reuse Pattern
```typescript
// Source: src/ws/wsClient.ts — createWsClient sends connected+start on open automatically
// Re-calling with same params satisfies WSR-02: connected then start before any media
const newWs = await createWsClient(
  this.config.WS_TARGET_URL,
  { streamSid: session.streamSid, callSid: session.callSid, from: ..., to: ..., sipCallId: session.callId },
  session.log,
);
```

### Cross-Platform FD Count
```javascript
// Source: verified empirically on macOS (Node 25.6.1) and Linux — see research notes
import fs from 'node:fs';
import { execSync } from 'node:child_process';

function getFdCount() {
  try {
    return fs.readdirSync('/proc/self/fd').length - 1; // -1: excludes the readdir FD itself
  } catch {
    const lines = execSync(`lsof -p ${process.pid} 2>/dev/null`).toString().trim().split('\n');
    return Math.max(0, lines.length - 1); // -1: subtract header row
  }
}
```

### Backoff Timing (from CONTEXT.md spec)
```
Attempt 1: wait 1s  (elapsed ~1s)
Attempt 2: wait 2s  (elapsed ~3s)
Attempt 3: wait 4s  (elapsed ~7s)
Attempt 4: wait 4s  (elapsed ~11s)
Attempt 5: wait 4s  (elapsed ~15s)
Attempt 6: wait 4s  (elapsed ~19s)
Attempt 7: wait 4s  (elapsed ~23s)
Attempt 8: wait 4s  (elapsed ~27s)
Attempt 9: budget check: 27 + 4 = 31 >= 30 → give up
```
Total: ~8 attempts over ~27s before budget check fires.

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| WS disconnect → immediate BYE | WS disconnect → reconnect loop → BYE only on timeout | Phase 3 | Caller stays on the line during transient backend restarts |
| RTP socket created/destroyed per call | RTP socket stays bound throughout reconnect window | Phase 3 | No port reallocation jitter; same port visible to sipgate RTP path |

## Open Questions

1. **DTMF forwarding during reconnect**
   - What we know: DTMF events arrive as `rtp.on('dtmf')` callbacks
   - What's unclear: Should DTMF digits be buffered and replayed after reconnect, or dropped like audio?
   - Recommendation: Drop DTMF during reconnect (consistent with "drop not buffer" policy; DTMF during a gap is an edge case not worth special-casing). This is Claude's discretion; the planner should make this explicit in the task.

2. **Reconnect loop visibility: separate module or method**
   - What we know: Logic fits entirely within `CallManager`'s private API
   - What's unclear: Whether to extract to `src/ws/reconnect.ts` for testability
   - Recommendation: Keep as a private method on `CallManager` for Phase 3; extraction is a future refactor if tests require it.

## Sources

### Primary (HIGH confidence)
- Direct codebase inspection: `src/bridge/callManager.ts`, `src/ws/wsClient.ts`, `src/rtp/rtpHandler.ts` — implementation details verified line-by-line
- `src/bridge/callManager.ts:468` — exact line where reconnect replaces current `terminateSession` call
- `.planning/phases/03-resilience/03-CONTEXT.md` — locked decisions and implementation constraints
- Node.js 22 official docs — `dgram.Socket.close()`, `fs.readdirSync('/proc/self/fd')`, `child_process.execSync`

### Secondary (MEDIUM confidence)
- Platform test: `lsof -p PID` verified functional on macOS (Darwin 25.3.0, Node 25.6.1) — returned stable FD count of 41 for idle process
- `/proc/self/fd` verified present on Linux (confirmed via empirical test in research session)

### Tertiary (LOW confidence)
- None

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH — no new dependencies; all libraries already present and pinned
- Architecture: HIGH — implementation plan derived directly from reading the actual source code, not from assumptions
- Pitfalls: HIGH — pitfalls derived from specific code patterns observed in callManager.ts and wsClient.ts
- FD test approach: MEDIUM — `/proc/self/fd` off-by-one behavior (readdir FD) is a known Linux gotcha confirmed by multiple sources; macOS lsof behavior confirmed empirically

**Research date:** 2026-03-03
**Valid until:** 2026-04-02 (stable platform; 30-day window is conservative — this is internal Node.js, nothing will change)
