# Phase 9: Go Bridge mark/clear - Context

**Gathered:** 2026-03-05
**Status:** Ready for planning

<domain>
## Phase Boundary

Add `mark` and `clear` Twilio Media Streams event handling to the Go WS↔RTP bridge. Scope: `session.go`, `ws.go` in `go/internal/bridge/`. The phase delivers barge-in support end-to-end on the Go path. SIP layer (Phase 10) and Node.js (Phase 11) are out of scope here.

</domain>

<decisions>
## Implementation Decisions

### Pending marks on WS reconnect
- **Do NOT clear `markEchoQueue` on WS drop** — natural/automatic behavior
- `rtpPacer` is persistent and continues processing `packetQueue` mark sentinels during reconnect; names accumulate in `markEchoQueue`
- After reconnect + handshake, `wsPacer` naturally sends pending marks on the new connection
- Rationale: marks represent "audio finished playing at this point" — that fact is true regardless of when the WS connection is available; the consumer gets them late but complete
- Twilio itself has no reconnect precedent (one-shot connection); this interpretation follows the semantic intent

### Prometheus metrics
- Add `mark_echoed_total` and `clear_received_total` counters to `go/internal/observability/` **in this phase** (not deferred to v2.2)
- Follow existing pattern: counter on `observability.Metrics` struct, registered on custom registry, incremented in the relevant goroutine

### Log level for mark/clear events
- Use **Debug level** for all mark/clear event logs (receipt, echo, clear flush)
- Rationale: mark/clear are protocol-noise in production, not error signals; zerolog `s.log.Debug()` is filtered by default at Info log level

</decisions>

<code_context>
## Existing Code Insights

### Reusable Assets
- `dtmfQueue chan string` (capacity 10) in `CallSession` — direct model for `markEchoQueue chan string` (same capacity, same select-case pattern in wsPacer)
- `wsSignal` with `sync.Once` — existing pattern for single-fire signaling; `clearSignal chan struct{}` follows same idea but buffered (capacity 1) so rtpPacer can signal without blocking
- `sendDTMF()` in ws.go — model for `sendMarkEcho()`: same `writeJSON(conn, event)` pattern, same seqNo increment in wsPacer
- `DtmfEvent` / `DtmfBody` struct pair in ws.go — model for `MarkEvent` / `MarkBody` JSON struct pair
- `net.Pipe()` + `wsutil.WriteServerText` / `wsutil.ReadClientData` pattern in ws_test.go — reuse for `TestSendMarkEcho_JSONSchema`

### Established Patterns
- **sole-writer invariant**: `wsPacer` is the ONLY goroutine writing to `wsConn` — mark echoes MUST route through `markEchoQueue` to `wsPacer`, never sent directly from `wsToRTP` or `rtpPacer`
- **non-blocking enqueue**: all queues use `select { case q <- v: default: drop+warn }` — follow for markEchoQueue in rtpPacer
- `packetQueue chan []byte` → change to `chan outboundFrame` (tagged union); existing producers: `wsToRTP`; existing consumer: `rtpPacer`
- Tests follow unit-test-per-function pattern for ws.go functions + channel-logic tests for dedup — add analogous tests for mark/clear

### Integration Points
- `CallSession` struct in session.go: add `markEchoQueue chan string` and `clearSignal chan struct{}` fields; initialize in `run()` alongside `packetQueue` and `dtmfQueue`
- `run()` initialization block (line 103–105): add `s.markEchoQueue = make(chan string, 10)` and `s.clearSignal = make(chan struct{}, 1)`
- `wsPacer` select (line 333–379): add `case markName := <-s.markEchoQueue` before `ticker.C` — same priority as dtmfQueue (control event, not audio)
- `wsToRTP` switch (line 430): add `case "mark"` and `case "clear"` branches
- `rtpPacer` ticker case (line 506–544): after dequeue, check if `outboundFrame` is a mark sentinel → route to `markEchoQueue`; add `clearSignal` drain at start of each tick
- `observability/metrics.go`: add `MarkEchoed prometheus.Counter` and `ClearReceived prometheus.Counter`

</code_context>

<specifics>
## Specific Ideas

- `outboundFrame` as a simple struct: `struct { payload []byte; markName string }` — if `payload` is nil it's a mark sentinel, otherwise it's audio. Simple, no interface boxing.
- `clearSignal` capacity 1 (buffered): `wsToRTP` can signal without blocking even if `rtpPacer` is mid-tick; excess clears are coalesced (correct — a second clear before the first is processed is idempotent)
- Mark echo echoes only the `name` field from the inbound mark event — no other fields from the inbound event are forwarded
- Inbound `clear` event schema: follow existing pattern (check `event` field only, ignore `streamSid` — same as current `media` handling)
- `sendMarkEcho(conn, streamSid, markName, seqNo)` — same signature shape as `sendDTMF`, seqNo incremented in wsPacer after send

</specifics>

<deferred>
## Deferred Ideas

- Sequence-number continuity for mark echoes across WS reconnects — v2.x
- In-dialog OPTIONS for session refresh (RFC 4028) — different feature
- `sip_options_failures_total` counter — Phase 10

</deferred>

---

*Phase: 09-go-bridge-mark-clear*
*Context gathered: 2026-03-05*
