# Phase 11: Node.js Equivalents - Context

**Gathered:** 2026-03-05
**Status:** Ready for planning

<domain>
## Phase Boundary

Add mark/clear Twilio Media Streams event handling and SIP OPTIONS keepalive to the Node.js reference implementation (`node/`), bringing it to full parity with the Go implementation shipped in Phases 9 and 10. Scope: `node/src/ws/wsClient.ts`, `node/src/sip/userAgent.ts`, `node/src/config/index.ts`, plus Vitest test infrastructure. The Go codebase is out of scope.

</domain>

<decisions>
## Implementation Decisions

### Mark sentinel strategy
- Use a **tagged union** in the outboundDrain queue: change `queue: Buffer[]` to `Array<Buffer | { markName: string }>`. Direct translation of Go's `outboundFrame` tagged union.
- When a mark sentinel is dequeued in the drain's own timer callback, call `onMarkReady(markName)` and immediately reschedule (no 20ms wait) — the sentinel has no audio to pace.
- **Empty-queue fast-path**: if the queue is empty and the drain timer is idle when a mark arrives, call `onMarkReady` immediately (do NOT enqueue). Matches Go's MARK-02 behavior exactly.
- `WsClient.sendClear` flushes the audio queue (call `outboundDrain.stop()`) AND echoes all pending mark sentinels still in the queue immediately. Matches Go MARK-03.

### WsClient interface extension (MRKN-03)
- Add three new methods to the `WsClient` interface: `onMark(handler: (markName: string) => void)`, `sendMark(markName: string)`, `sendClear()`.
- `CallManager` calls `ws.onMark(...)` and must not reference the raw WebSocket — interface only.
- Wiring of `onMarkReady` callback into `makeDrain` vs. post-construction registration: **Claude's discretion** (user had insufficient codebase insight to choose; choose the cleaner approach).

### OPTIONS interval config (OPTN-01/OPTN-02/OPTN-03)
- Add `SIP_OPTIONS_INTERVAL` to the Zod config schema as **integer seconds**: `z.coerce.number().int().positive().default(30)`. Consistent with the existing `SIP_EXPIRES` pattern — no custom duration parser needed.
- OPTIONS keepalive loop uses `setInterval` inside `createSipUserAgent`. The interval handle is stored in the closure and `clearInterval`-ed inside the existing **`SipHandle.stop()`** — no new interface method. Single call site, no interface change.
- Failure handling and re-registration logic mirrors Go: 2 consecutive failures (timeout or non-401/407 error) → call `sendRegister()` to re-register. 401/407 → server is alive, skip re-registration, do NOT increment failure counter.

### Logging (carried from Phase 9 & 10 decisions)
- Successful OPTIONS ping: **Debug** (protocol noise)
- OPTIONS failure: **Warn** (operationally significant)
- Re-registration triggered by OPTIONS keepalive: **Warn**
- Mark/clear event receipt and echo: **Debug**

### Test infrastructure
- **Vitest** as the test framework — TypeScript-native, fast, no build step needed.
- Test files go in `node/test/` directory (matches existing `node/test/fd-leak.mjs` convention).
- Add `vitest` to devDependencies, add `"test": "vitest run"` script to `package.json`.
- TDD approach matching Go phases: failing test first (red), then implementation (green), committed as separate commits.

### Claude's Discretion
- Exact `makeDrain` signature change for mark wiring (callback at construction vs. post-construction registration)
- Test file naming convention within `node/test/`

</decisions>

<code_context>
## Existing Code Insights

### Reusable Assets
- `makeDrain(sendPacket)` in `wsClient.ts`: change `queue: Buffer[]` to tagged union; add `onMarkReady` callback; add `enqueueMark(name)` method. The `stop()` method already clears the queue — extend it to also collect and echo pending marks.
- `sendDtmf(digit)` in `WsClient`: direct model for `sendMark(markName)` — same `ws.send(JSON.stringify({event, sequenceNumber, streamSid, ...}))` pattern.
- `ws.on('message', ...)` handler in `onAudio`: currently ignores mark/clear events (`// Ignore non-media events`). Extend this handler to also dispatch mark and clear events.
- `refreshTimer` pattern in `createSipUserAgent`: model for the OPTIONS keepalive `setInterval` — same closure-captured handle, same `clearInterval` in `stop()`.
- `sendRegister(authHeader?)` helper: call this on OPTIONS failure threshold, same as the refresh path.
- `parseStatusLine(raw)` and `getHeader(raw, name)`: already available for parsing OPTIONS responses on the UDP socket.

### Established Patterns
- **Event-loop concurrency**: No goroutines/mutexes needed. Node.js event loop serializes callbacks — no mutex needed for `sendRegister` (unlike Go's `sync.Mutex`).
- **Zod config**: integer seconds for time values (`z.coerce.number().int().positive().default(N)`).
- **pino logging**: `log.debug({event, ...}, msg)` / `log.warn({event, ...}, msg)`.
- **Socket message dispatch**: single `socket.on('message', ...)` handler already dispatches REGISTER responses and inbound OPTIONS. OPTIONS responses (to our outbound keepalive) will come through the same handler — route by matching the `CSeq` field to distinguish them.

### Integration Points
- `node/src/config/index.ts`: add `SIP_OPTIONS_INTERVAL` to Zod schema.
- `node/src/ws/wsClient.ts`: extend `WsClient` interface + `makeDrain` internal + `ws.on('message')` handler.
- `node/src/bridge/callManager.ts`: wire `ws.onMark(...)` after `ws.onAudio(...)` in `handleInvite()`. Re-wire in `startWsReconnectLoop()` on reconnect (same pattern as `newWs.onAudio`).
- `node/src/sip/userAgent.ts`: add OPTIONS keepalive `setInterval` after initial 200 OK registration; extend `stop()` to `clearInterval`.
- `node/package.json`: add `vitest` devDependency, add `"test": "vitest run"` script.

</code_context>

<specifics>
## Specific Ideas

- The outbound OPTIONS request can reuse the same UDP socket as REGISTER. A simple `buildOptions()` helper mirroring `buildRegister()` — same Via/From/To/Call-ID structure but `OPTIONS sip:registrar SIP/2.0` request-line and no `Contact` or `Expires` headers.
- CSeq matching to distinguish OPTIONS responses from REGISTER responses: the method field in CSeq (`OPTIONS` vs `REGISTER`) is sufficient — no separate Call-ID needed for the keepalive.
- `outboundDrain.stop()` (for `sendClear`) already sets `queue.length = 0` and clears the timer. Before the length reset, extract all mark sentinels from the queue and fire `onMarkReady` for each — that's the "echo pending marks immediately on clear" behavior.

</specifics>

<deferred>
## Deferred Ideas

- None — discussion stayed within phase scope.

</deferred>

---

*Phase: 11-node.js-equivalents*
*Context gathered: 2026-03-05*
