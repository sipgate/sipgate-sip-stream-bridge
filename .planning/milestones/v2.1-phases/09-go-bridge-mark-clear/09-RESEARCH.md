# Phase 9: Go Bridge mark/clear - Research

**Researched:** 2026-03-05
**Domain:** Go WebSocket/RTP bridge — Twilio Media Streams mark and clear event handling
**Confidence:** HIGH

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions

#### Pending marks on WS reconnect
- Do NOT clear `markEchoQueue` on WS drop — natural/automatic behavior
- `rtpPacer` is persistent and continues processing `packetQueue` mark sentinels during reconnect; names accumulate in `markEchoQueue`
- After reconnect + handshake, `wsPacer` naturally sends pending marks on the new connection
- Rationale: marks represent "audio finished playing at this point" — that fact is true regardless of when the WS connection is available; the consumer gets them late but complete
- Twilio itself has no reconnect precedent (one-shot connection); this interpretation follows the semantic intent

#### Prometheus metrics
- Add `mark_echoed_total` and `clear_received_total` counters to `go/internal/observability/` in this phase (not deferred to v2.2)
- Follow existing pattern: counter on `observability.Metrics` struct, registered on custom registry, incremented in the relevant goroutine

#### Log level for mark/clear events
- Use Debug level for all mark/clear event logs (receipt, echo, clear flush)
- Rationale: mark/clear are protocol-noise in production, not error signals; zerolog `s.log.Debug()` is filtered by default at Info log level

### Claude's Discretion

None specified — all key choices are locked above.

### Deferred Ideas (OUT OF SCOPE)

- Sequence-number continuity for mark echoes across WS reconnects — v2.x
- In-dialog OPTIONS for session refresh (RFC 4028) — different feature
- `sip_options_failures_total` counter — Phase 10
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| MARK-01 | Audio-dock erkennt eingehende `mark`-Events vom WS-Server und echot den mark-Namen zurück, sobald alle vorherigen Audio-Frames gespielt wurden | outboundFrame sentinel in packetQueue → rtpPacer routes to markEchoQueue → wsPacer echoes |
| MARK-02 | Audio-dock echot einen mark sofort zurück, wenn die `packetQueue` beim Eingang bereits leer ist | wsToRTP checks len(packetQueue)==0 at mark receipt; if empty, enqueues directly to markEchoQueue instead of packetQueue |
| MARK-03 | Audio-dock erkennt eingehende `clear`-Events, leert die `packetQueue` und echot alle ausstehenden marks sofort zurück | clearSignal → rtpPacer drains packetQueue including mark sentinels, routes each found mark sentinel to markEchoQueue |
| MARK-04 | RTP-Pacer läuft während eines `clear`-Events weiter (keine Unterbrechung des RTP-Streams) | drain-only approach: rtpPacer never stops; it drains packetQueue then falls back to silence on the very next tick |
</phase_requirements>

---

## Summary

Phase 9 adds Twilio Media Streams `mark` and `clear` event handling to the Go WS↔RTP bridge. The changes are confined to `go/internal/bridge/session.go`, `go/internal/bridge/ws.go`, and `go/internal/observability/metrics.go`. No new dependencies are required — all capabilities are available in the existing v2.0 stack.

The core mechanic is replacing `packetQueue chan []byte` with `packetQueue chan outboundFrame` (a tagged union). When `wsToRTP` receives a `mark` event, it enqueues an `outboundFrame{mark: "label"}` sentinel at the correct position in the queue. When `rtpPacer` dequeues a sentinel, it routes the mark name to `markEchoQueue chan string`. `wsPacer` (the sole WS writer) drains `markEchoQueue` between ticks and sends the echo. For `clear`, a `clearSignal chan struct{}` (capacity 1) lets `wsToRTP` signal `rtpPacer` to drain the entire queue — including any mark sentinels, which are echoed immediately via `markEchoQueue` — while the pacer continues ticking at 20ms without interruption.

**Primary recommendation:** Implement in this order: (1) type-change `packetQueue` to `chan outboundFrame` and update all producers/consumers — standalone commit; (2) add mark sentinel path through rtpPacer → markEchoQueue → wsPacer; (3) add clearSignal drain path; (4) add Prometheus counters; (5) add tests. All changes are purely additive after the initial type rename.

---

## Standard Stack

### Core (unchanged — no new dependencies)

| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| encoding/json | stdlib | JSON encode/decode for MarkEvent / MarkBody | Already used for all WS message types |
| sync | stdlib | WaitGroup, Once (no new primitives needed) | Already used for wsSignal, wg patterns |
| time | stdlib | 20ms ticker in rtpPacer / wsPacer (no change) | Already used |
| github.com/gobwas/ws | v1.3.2 | WS text frame read/write via wsutil | Already used; writeJSON/readWSMessage unchanged |
| github.com/rs/zerolog | v1.34.0 | Debug-level logging for mark/clear events | Already used |
| github.com/prometheus/client_golang | v1.23.2 | mark_echoed_total, clear_received_total counters | Already used for existing counters |

### No Alternatives Considered

Zero new dependencies. Every required capability — JSON struct marshalling, buffered channel tagged unions, counter registration on a custom Prometheus registry — is already present in the v2.0 stack.

**Installation:** No `go get` needed.

---

## Architecture Patterns

### Recommended Project Structure (unchanged)

```
go/internal/bridge/
├── session.go    # CallSession struct + goroutines — primary change file
├── ws.go         # WS event structs + helpers — add MarkEvent/MarkBody/sendMarkEcho
├── ws_test.go    # Unit tests — add TestSendMarkEcho_JSONSchema
├── manager.go    # No changes
└── manager_test.go  # No changes

go/internal/observability/
└── metrics.go    # Add MarkEchoed + ClearReceived counters
```

### Pattern 1: outboundFrame Tagged Union (replacing `chan []byte`)

**What:** Replace `packetQueue chan []byte` with `packetQueue chan outboundFrame` where `outboundFrame` carries either PCMU audio or a mark sentinel. Only one field is set per frame instance.

**When to use:** Any time a FIFO queue must carry heterogeneous items in-order without breaking existing consumer logic.

**Example:**
```go
// Source: CONTEXT.md specifics / ARCHITECTURE.md Feature 1
// In session.go — new type definition (add before CallSession struct)
type outboundFrame struct {
    audio []byte // non-nil for PCMU frames (160 bytes)
    mark  string // non-empty for mark sentinel frames
}
```

The existing producer (`wsToRTP` media case) changes from:
```go
// Before
case s.packetQueue <- chunk:
```
to:
```go
// After
case s.packetQueue <- outboundFrame{audio: chunk}:
```

The existing consumer (`rtpPacer` ticker case) changes from:
```go
// Before
var chunk []byte
select {
case chunk = <-s.packetQueue:
default:
    chunk = pcmuSilenceFrame
}
```
to:
```go
// After
var frame outboundFrame
select {
case frame = <-s.packetQueue:
default:
    // empty — send silence below
}
if frame.mark != "" {
    select {
    case s.markEchoQueue <- frame.mark:
    default:
        s.log.Warn()...Msg("rtpPacer: markEchoQueue full — dropping mark echo")
    }
    continue // no RTP packet for sentinel frame
}
chunk := frame.audio
if chunk == nil {
    chunk = pcmuSilenceFrame
}
```

### Pattern 2: markEchoQueue Channel (mirrors dtmfQueue)

**What:** `markEchoQueue chan string` (capacity 10) on `CallSession`. `rtpPacer` produces mark names when a sentinel is dequeued; `wsPacer` consumes them between ticks and echoes to the WS server.

**When to use:** Any time a persistent goroutine (rtpPacer) needs to hand data to the sole WS writer (wsPacer) without violating the single-writer invariant.

**Example:**
```go
// Source: CONTEXT.md code_context / ARCHITECTURE.md Feature 1
// In wsPacer select — add before ticker.C (same priority as dtmfQueue):
case markName := <-s.markEchoQueue:
    s.log.Debug().Str("call_id", s.callID).Str("mark", markName).Msg("wsPacer: echoing mark")
    if err := sendMarkEcho(wsConn, s.streamSid, markName, seqNo); err != nil {
        s.log.Error().Err(err).Str("call_id", s.callID).Msg("wsPacer: sendMarkEcho failed")
        sig.Signal()
        return
    }
    seqNo++
    if s.metrics != nil {
        s.metrics.MarkEchoed.Inc()
    }
```

### Pattern 3: clearSignal Channel (capacity-1 buffered)

**What:** `clearSignal chan struct{}` (capacity 1) on `CallSession`. `wsToRTP` sends a non-blocking signal on clear receipt; `rtpPacer` checks it at the top of each tick and drains the entire `packetQueue` (including mark sentinels, which are routed to `markEchoQueue` for immediate echo).

**When to use:** When one goroutine needs to trigger a queue-flush in another goroutine without racing on the channel itself. Capacity 1 coalesces rapid clears.

**Example:**
```go
// Source: CONTEXT.md specifics + ARCHITECTURE.md Feature 2

// In wsToRTP case "clear":
s.log.Debug().Str("call_id", s.callID).Msg("wsToRTP: received clear")
select {
case s.clearSignal <- struct{}{}:
default:
    // previous clear not yet processed by rtpPacer — that's fine, queue will be cleared
}
if s.metrics != nil {
    s.metrics.ClearReceived.Inc()
}

// In rtpPacer, at top of ticker.C case — before normal dequeue:
select {
case <-s.clearSignal:
    s.log.Debug().Str("call_id", s.callID).Msg("rtpPacer: clear signal — draining packetQueue")
    for {
        select {
        case f := <-s.packetQueue:
            if f.mark != "" {
                // echo mark immediately — audio was cleared before it played
                select {
                case s.markEchoQueue <- f.mark:
                default:
                    s.log.Warn()...Msg("rtpPacer: markEchoQueue full during clear — dropping mark echo")
                }
            }
            // audio frame: discard silently
        default:
            goto clearDone
        }
    }
clearDone:
default:
}
```

### Pattern 4: MARK-02 Immediate Echo (empty queue at receipt)

**What:** When `wsToRTP` receives a `mark` event and `packetQueue` is already empty (no buffered audio), the mark should be echoed immediately rather than waiting for an audio drain that will never happen.

**When to use:** MARK-02 requirement.

**Example:**
```go
// Source: FEATURES.md edge cases / MARK-02 requirement
case "mark":
    var markMsg struct {
        Mark struct {
            Name string `json:"name"`
        } `json:"mark"`
    }
    if raw, ok := envelope["mark"]; ok {
        if err := json.Unmarshal(raw, &markMsg); err != nil {
            s.log.Warn()...Msg("wsToRTP: mark decode failed — skipping")
            continue
        }
    }
    markName := markMsg.Mark.Name
    s.log.Debug().Str("call_id", s.callID).Str("mark", markName).Msg("wsToRTP: received mark")

    if len(s.packetQueue) == 0 {
        // Queue empty — echo immediately (MARK-02)
        select {
        case s.markEchoQueue <- markName:
        default:
            s.log.Warn()...Msg("wsToRTP: markEchoQueue full — dropping immediate mark echo")
        }
    } else {
        // Queue has audio — enqueue sentinel to preserve order (MARK-01)
        select {
        case s.packetQueue <- outboundFrame{mark: markName}:
        case <-ctx.Done():
            return
        default:
            s.log.Warn()...Msg("wsToRTP: packetQueue full — dropping mark sentinel")
        }
    }
```

### Pattern 5: sendMarkEcho Function (mirrors sendDTMF)

**What:** `sendMarkEcho(conn net.Conn, streamSid, markName string, seqNo uint32) error` in `ws.go`. Called only from `wsPacer` (sole-writer invariant).

**Example:**
```go
// Source: CONTEXT.md specifics — same signature shape as sendDTMF
// In ws.go — new types and function:

type MarkEvent struct {
    Event          string   `json:"event"`
    SequenceNumber string   `json:"sequenceNumber"`
    StreamSid      string   `json:"streamSid"`
    Mark           MarkBody `json:"mark"`
}

type MarkBody struct {
    Name string `json:"name"`
}

func sendMarkEcho(conn net.Conn, streamSid, markName string, seqNo uint32) error {
    return writeJSON(conn, MarkEvent{
        Event:          "mark",
        SequenceNumber: fmt.Sprintf("%d", seqNo),
        StreamSid:      streamSid,
        Mark:           MarkBody{Name: markName},
    })
}
```

### Pattern 6: Prometheus Counters (mirrors existing pattern)

**What:** Two new counters on `observability.Metrics`: `mark_echoed_total` and `clear_received_total`. Registered on the custom registry, never on `prometheus.DefaultRegisterer`.

**Example:**
```go
// Source: go/internal/observability/metrics.go existing pattern
// Add to Metrics struct:
MarkEchoed    prometheus.Counter // mark_echoed_total
ClearReceived prometheus.Counter // clear_received_total

// Add to NewMetrics():
markEchoed := prometheus.NewCounter(prometheus.CounterOpts{
    Name: "mark_echoed_total",
    Help: "Total mark echo events sent to the WS server.",
})
clearReceived := prometheus.NewCounter(prometheus.CounterOpts{
    Name: "clear_received_total",
    Help: "Total clear events received from the WS server.",
})
reg.MustRegister(..., markEchoed, clearReceived)
```

### Anti-Patterns to Avoid

- **Writing to wsConn from rtpPacer:** When rtpPacer dequeues a mark sentinel, calling `writeJSON(wsConn, ...)` directly from rtpPacer violates the sole-writer invariant. gobwas/ws is not safe for concurrent writes. Always route through `markEchoQueue` to `wsPacer`.
- **Draining packetQueue from wsToRTP on clear:** `wsToRTP` and `rtpPacer` are concurrent goroutines both touching `packetQueue`. Using `clearSignal` and letting `rtpPacer` drain its own queue eliminates non-deterministic interleaving. Do NOT drain from `wsToRTP`.
- **Stopping rtpPacer on clear:** Stopping the ticker or pausing the goroutine introduces an RTP timestamp discontinuity. The caller's jitter buffer detects it as packet loss and the caller hears a click. Drain the queue only; silence frames fill the gap automatically.
- **Using `sync.Once` for clearSignal:** `clearSignal` is capacity-1, not single-fire. Multiple `clear` events in a call are valid. Use a buffered `chan struct{}` not `sync.Once`.

---

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Ordered mark/audio correlation | Custom mutex-guarded linked list | `chan outboundFrame` FIFO queue | Go channels are intrinsically FIFO; no additional synchronization needed for ordering |
| Mark echo rate-limiting | Custom throttle | Non-blocking enqueue with drop+warn | Mark events are rare (< 1/s in practice); the existing dtmfQueue pattern (capacity 10, drop on full) is sufficient |
| Mark name storage across reconnects | External state store or sync.Map | Leave `markEchoQueue` as-is | Marks accumulate in the buffered channel naturally; wsPacer drains them on the new connection after handshake |
| Cross-goroutine clear notification | Mutex + bool flag | `chan struct{}` (capacity 1) | Channels compose cleanly with `select`; a mutex flag requires explicit polling or condition variables |
| Prometheus metric collection | Custom expvar or log-based counting | `prometheus.NewCounter` on existing registry | The custom registry is already wired to `/metrics`; two lines to add a counter |

**Key insight:** This phase adds ~120 lines of logic to existing files. The complexity is all in goroutine interaction, which the existing channel patterns (dtmfQueue, wsSignal, packetQueue) already demonstrate correctly. The goal is to replicate, not invent.

---

## Common Pitfalls

### Pitfall 1: Mark Echo Written from Wrong Goroutine (Critical)

**What goes wrong:** When `rtpPacer` dequeues a mark sentinel and calls `writeJSON(wsConn, ...)` directly, it races with `wsPacer` writing audio frames. gobwas/ws WebSocket frames will be corrupted or a concurrent-write panic will fire.

**Why it happens:** rtpPacer "has" the mark name at the point of dequeue, making a direct write look natural. The intent to preserve the sole-writer invariant is there but the implementation violates it.

**How to avoid:** `rtpPacer` sends the mark name to `markEchoQueue`. `wsPacer` picks it up in the next select iteration and calls `sendMarkEcho`. The channel handoff is the only correct path.

**Warning signs:** `go test -race` reports concurrent write to `wsConn`. gobwas/ws returns write error mid-call with no network issue.

### Pitfall 2: clear Drain From wsToRTP (Concurrency Bug)

**What goes wrong:** `wsToRTP` draining `packetQueue` concurrently with `rtpPacer` reading from it causes partial drains. `rtpPacer` may send a few more frames after `wsToRTP` finishes the drain loop, delivering residual audio.

**Why it happens:** Draining from wsToRTP looks clean (it's in the same event handler as the clear receipt). Go channel reads ARE safe from multiple goroutines, but the semantic contract breaks — neither goroutine fully owns the drain.

**How to avoid:** `wsToRTP` sends to `clearSignal`. `rtpPacer` is the sole consumer of `packetQueue` and can drain it atomically at its next tick.

**Warning signs:** After clear, caller still hears 1-2 frames of old TTS audio. Wireshark shows RTP continuing ~40ms after clear was received.

### Pitfall 3: clear Does Not Echo Pending Marks

**What goes wrong:** `packetQueue` is drained but mark sentinels inside it are simply discarded. The WS server's barge-in state machine waits indefinitely for mark echoes that will never arrive. Consumer-side mark timeout fires after every barge-in.

**Why it happens:** The drain loop treats all frames uniformly (`<-s.packetQueue` with no inspection). Mark names are lost.

**How to avoid:** During the drain loop in `rtpPacer`, inspect each `outboundFrame`. If `frame.mark != ""`, route it to `markEchoQueue` (non-blocking). This echoes all pending marks immediately after the clear.

**Warning signs:** After barge-in, WS consumer logs "mark timeout" or "waiting for mark" indefinitely.

### Pitfall 4: MARK-02 — Immediate Echo Not Implemented

**What goes wrong:** When the WS server sends a mark with no preceding audio (common for "end-of-turn" marks), the sentinel sits in `packetQueue` waiting for audio that never comes. The echo is delayed until the next silence cycle dequeueing the sentinel — but the silence path uses `pcmuSilenceFrame`, not the queue.

**Why it happens:** The sentinel is only dequeued when `rtpPacer`'s `select { case frame = <-s.packetQueue: }` fires. If `packetQueue` is empty, the `default` branch runs and sends silence — the sentinel never arrives.

**How to avoid:** In `wsToRTP`, check `len(s.packetQueue) == 0` at mark receipt. If the queue is empty, route directly to `markEchoQueue` (skip the packetQueue entirely). If the queue has audio, enqueue the sentinel.

**Warning signs:** MARK-02 test hangs waiting for an immediate echo that never arrives.

### Pitfall 5: outboundFrame Type Change Breaks Existing Compile

**What goes wrong:** Changing `packetQueue chan []byte` to `chan outboundFrame` without updating all producers causes a compile error. The `wsToRTP` media case currently does `case s.packetQueue <- chunk:` where `chunk` is `[]byte`.

**Why it happens:** The field is used in 2 places in `wsToRTP` (the chunking loop) and 1 place in `rtpPacer` (the dequeue). All must be updated atomically.

**How to avoid:** Make the type change a standalone commit. The compiler will fail at every old use site — fix all before adding new logic. Run `go test ./...` after the type-only commit to confirm zero breakage.

**Warning signs:** Compile fails on `cannot use chunk (type []byte) as type outboundFrame`.

### Pitfall 6: markEchoQueue Not Initialized Before Goroutines Launch

**What goes wrong:** `markEchoQueue` is nil when `rtpPacer` or `wsPacer` start. A send to a nil channel blocks forever; a receive from a nil channel blocks forever. Either goroutine will deadlock silently.

**Why it happens:** `run()` initializes queues at lines 103-105. Adding a new queue but forgetting to initialize it (or initializing it after goroutines launch) causes a nil channel panic or deadlock.

**How to avoid:** Initialize `s.markEchoQueue` and `s.clearSignal` at the same place `s.packetQueue` is initialized in `run()`, before `wg.Add(2)` and the goroutine launches.

**Warning signs:** Test deadlock on first mark receipt. Nil pointer dereference if the channel is used without initialization check.

---

## Code Examples

Verified patterns from the v2.0 codebase (HIGH confidence — read from source):

### Initialization in run() (session.go lines 103-105 reference)

```go
// Source: go/internal/bridge/session.go run() initialization block
// Add alongside existing queue initializations:
s.packetQueue     = make(chan outboundFrame, packetQueueSize) // type changed from chan []byte
s.rtpInboundQueue = make(chan []byte, rtpInboundQueueSize)   // unchanged
s.dtmfQueue       = make(chan string, 10)                    // unchanged
s.markEchoQueue   = make(chan string, 10)                    // NEW
s.clearSignal     = make(chan struct{}, 1)                   // NEW
```

### Non-Blocking Enqueue Pattern (from wsToRTP and rtpPacer)

```go
// Source: go/internal/bridge/session.go existing dtmfQueue pattern (line 281-284)
// Apply same pattern for markEchoQueue in rtpPacer:
select {
case s.markEchoQueue <- frame.mark:
default:
    s.log.Warn().Str("call_id", s.callID).Str("mark", frame.mark).
        Msg("rtpPacer: markEchoQueue full — dropping mark echo")
}
```

### wsPacer select Priority Order (from session.go lines 333-379)

```go
// Source: go/internal/bridge/session.go wsPacer
// Correct priority order for the select cases:
for {
    select {
    case <-ctx.Done():
        return
    case digit := <-s.dtmfQueue:       // existing: control events first
        // ... existing DTMF handling
    case markName := <-s.markEchoQueue: // NEW: same priority as DTMF
        // ... sendMarkEcho
    case <-ticker.C:                    // lowest priority: audio tick
        // ... existing media forwarding
    }
}
```

### Test Pattern from ws_test.go (net.Pipe round-trip)

```go
// Source: go/internal/bridge/ws_test.go TestSendDTMF_JSONSchema pattern
// Apply same pattern for TestSendMarkEcho_JSONSchema:
func TestSendMarkEcho_JSONSchema(t *testing.T) {
    client, server := newPipe(t)
    defer client.Close()
    defer server.Close()

    streamSid := "MZtest123"
    markName  := "greeting-end"
    var seqNo uint32 = 7

    errCh := make(chan error, 1)
    go func() {
        errCh <- sendMarkEcho(client, streamSid, markName, seqNo)
    }()

    data, _, err := wsutil.ReadClientData(server)
    // ... assert got.Event == "mark", got.StreamSid == streamSid,
    //     got.SequenceNumber == "7", got.Mark.Name == markName
}
```

---

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| `packetQueue chan []byte` (audio only) | `chan outboundFrame` (audio or mark sentinel) | Phase 9 | Enables in-order mark/audio correlation without additional channels or mutexes |
| mark/clear silently ignored (default case in wsToRTP) | Handled explicitly in wsToRTP switch | Phase 9 | Enables barge-in end-to-end |
| No mark echo metrics | `mark_echoed_total`, `clear_received_total` counters | Phase 9 | Operational visibility into barge-in usage rate |

**Current state (v2.0 baseline):**
- `wsToRTP` line 477: `default:` case logs and skips `mark` and `clear` events
- `packetQueue` is `chan []byte` with audio frames only
- `CallSession` has no `markEchoQueue` or `clearSignal` fields

---

## Open Questions

1. **MARK-02 and concurrent clear race**
   - What we know: `len(s.packetQueue)` is a non-blocking length check on a buffered channel; it is safe to call from `wsToRTP`
   - What's unclear: between `len() == 0` and the enqueue to `markEchoQueue`, could `rtpPacer` concurrently drain? This would cause a double-immediate-echo — the len check sees empty, wsToRTP enqueues to markEchoQueue, and separately rtpPacer processes a mark sentinel that was concurrently enqueued
   - Recommendation: In practice, MARK-02 happens when the WS server sends mark with NO preceding media blob in the same connection iteration; the window for a race is negligible. Accept the "len() == 0 means echo immediately" heuristic — the worst case is one redundant mark echo, which is semantically benign (consumer dedups by name).

2. **markEchoQueue capacity under reconnect**
   - What we know: `markEchoQueue` is not cleared on WS drop (locked decision); marks accumulate
   - What's unclear: if many marks accumulate before reconnect (e.g., long outage + TTS-heavy session), markEchoQueue capacity 10 might overflow
   - Recommendation: Accept capacity 10 (same as dtmfQueue). Marks in flight during a WS outage of > 10 marks are an extreme edge case. Log a warning on drop.

---

## Validation Architecture

> `workflow.nyquist_validation` is not set to `true` in `.planning/config.json` — this section is skipped.

---

## Complete File Change Map

| File | Change Type | Description |
|------|-------------|-------------|
| `go/internal/bridge/session.go` | MODIFY | (1) New `outboundFrame` type; (2) `CallSession` gains `markEchoQueue chan string` + `clearSignal chan struct{}` fields; (3) `run()` initializes both channels; (4) `wsToRTP` adds `case "mark"` + `case "clear"`; (5) `rtpPacer` inspects `outboundFrame` and handles clearSignal drain; (6) `wsPacer` adds `markEchoQueue` select case |
| `go/internal/bridge/ws.go` | MODIFY | Add `MarkEvent`, `MarkBody` types + `sendMarkEcho()` function |
| `go/internal/bridge/ws_test.go` | MODIFY | Add `TestSendMarkEcho_JSONSchema` + channel-logic tests for mark/clear |
| `go/internal/observability/metrics.go` | MODIFY | Add `MarkEchoed`, `ClearReceived` counters to struct + `NewMetrics()` |

No new files. No changes to `manager.go`, `manager_test.go`, or any files outside `go/internal/bridge/` and `go/internal/observability/`.

---

## Sources

### Primary (HIGH confidence)

- `go/internal/bridge/session.go` (v2.0) — goroutine architecture, packetQueue ownership, dtmfQueue/wsSignal patterns, wsPacer sole-writer invariant, rtpPacer silence fallback
- `go/internal/bridge/ws.go` (v2.0) — DtmfEvent/DtmfBody struct pattern, sendDTMF signature, writeJSON/readWSMessage wrappers
- `go/internal/bridge/ws_test.go` (v2.0) — net.Pipe test pattern, TestSendDTMF_JSONSchema template for TestSendMarkEcho_JSONSchema
- `go/internal/observability/metrics.go` (v2.0) — counter registration pattern on custom registry
- `.planning/phases/09-go-bridge-mark-clear/09-CONTEXT.md` — locked decisions for markEchoQueue, clearSignal, Prometheus counters, log level
- `.planning/research/ARCHITECTURE.md` — outboundFrame tagged union design, clearSignal rationale, data flow diagrams
- `.planning/research/PITFALLS.md` — all 4 mark/clear-specific pitfalls (Pitfalls 1-4)
- Twilio Media Streams WebSocket Messages (official docs) — mark/clear JSON schemas, timing semantics: https://www.twilio.com/docs/voice/media-streams/websocket-messages

### Secondary (MEDIUM confidence)

- `.planning/research/FEATURES.md` — protocol semantics section, edge cases, MARK-02 empty-queue handling
- `.planning/research/STACK.md` — confirmed no new dependencies required

### Tertiary (LOW confidence)

None — all findings for this phase are backed by direct source code reads or official Twilio documentation.

---

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH — zero new dependencies; all libraries verified from go.mod and source
- Architecture: HIGH — goroutine model verified from session.go source; channel patterns modeled directly on dtmfQueue and wsSignal
- Protocol schemas: HIGH — verified against official Twilio docs
- Pitfalls: HIGH — derived from existing codebase invariants (sole-writer comment in session.go) and Twilio spec semantics

**Research date:** 2026-03-05
**Valid until:** 2026-06-05 (stable domain — Twilio Media Streams protocol is unchanged; Go concurrency patterns are version-stable)
