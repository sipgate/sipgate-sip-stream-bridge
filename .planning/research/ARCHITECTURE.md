# Architecture Research

**Domain:** SIP-to-WebSocket audio bridge — v2.1 additions: mark/clear events + SIP OPTIONS keepalive
**Researched:** 2026-03-05
**Confidence:** HIGH (Go concurrency model, existing codebase verified by direct read); HIGH (Twilio Media Streams mark/clear protocol — official docs); MEDIUM (sipgo OPTIONS keepalive — API verified, integration pattern inferred from existing REGISTER pattern)

---

## What This Document Covers

This is a **subsequent-milestone architecture document**. The v2.0 codebase is already shipped and verified. This document maps exactly three new features onto the existing goroutine and component architecture:

1. **`mark` event** — WS server sends mark message → audio-dock echoes it when outbound audio playout reaches that point
2. **`clear` event** — WS server sends clear → audio-dock flushes buffered outbound PCMU frames from `packetQueue`
3. **SIP OPTIONS keepalive** — audio-dock sends periodic OPTIONS to sipgate; 408/503 response triggers re-registration

---

## Current v2.0 Architecture (Baseline)

Understanding the existing goroutine structure is the prerequisite for all three additions.

### Goroutine Map Per Call (v2.0 baseline)

```
StartSession goroutine (exits after run() returns)
    │
    └─ session.run(sessionCtx, initialWsConn)
           │
           ├── goroutine: rtpReader(sessionCtx, rtpConn)
           │     Reads UDP from sipgate → routes PCMU to rtpInboundQueue
           │     Routes DTMF End packets → dtmfQueue
           │     Lifecycle: exits when rtpConn.Close() is called on ctx.Done()
           │
           ├── goroutine: rtpPacer(sessionCtx, rtpConn)
           │     Ticks every 20ms → drains packetQueue → writes PCMU RTP to caller
           │     Falls back to pcmuSilenceFrame when packetQueue is empty
           │     Lifecycle: exits on ctx.Done() or WriteTo error
           │
           └── WS reconnect loop (for{} in run()):
                 ├── goroutine: wsPacer(sessionCtx, wsConn, wsWg, sig)
                 │     Ticks every 20ms → drains rtpInboundQueue → sends media events
                 │     Also drains dtmfQueue between ticks (priority select)
                 │     SOLE WS writer during audio forwarding
                 │     Lifecycle: new goroutine each WS connection; exits on ctx.Done() or write error
                 │
                 └── goroutine: wsToRTP(sessionCtx, wsConn, wsWg, sig)
                       Reads WS frames → decodes media events → chunks PCMU → packetQueue
                       Handles stop event → dlg.Bye + cancel
                       Lifecycle: new goroutine each WS connection; exits on read error or ctx.Done()
```

### Key Queues and Channels

| Queue | Direction | Buffer | Goroutine Pair |
|-------|-----------|--------|----------------|
| `packetQueue chan []byte` | WS → RTP | 500 frames (10s) | wsToRTP produces, rtpPacer consumes |
| `rtpInboundQueue chan []byte` | RTP → WS | 50 frames (1s) | rtpReader produces, wsPacer consumes |
| `dtmfQueue chan string` | RTP → WS | 10 slots | rtpReader produces, wsPacer consumes |

### WS Writer Invariant (CRITICAL for all additions)

**wsPacer is the sole goroutine that writes to wsConn during audio forwarding.**
This is enforced structurally: only wsPacer holds the wsConn write path. Any new WS-direction
write (including mark echo) must be routed through wsPacer, not dispatched from wsToRTP or any
other goroutine. gobwas/ws is not safe for concurrent writes.

---

## Feature 1: `mark` Event

### Protocol (Twilio Media Streams — official docs)

**From WS server to audio-dock (inbound direction on wsToRTP):**
```json
{
  "event": "mark",
  "streamSid": "MZxxx",
  "mark": { "name": "my-label" }
}
```

**From audio-dock to WS server (outbound direction from wsPacer — the echo):**
```json
{
  "event": "mark",
  "sequenceNumber": "N",
  "streamSid": "MZxxx",
  "mark": { "name": "my-label" }
}
```

**Semantics:** When the WS server sends a mark after a sequence of media frames, audio-dock must
echo the mark back when the outbound audio playout reaches that point in the queue — i.e., when
the mark arrives at the front of the draining packetQueue. This signals "I have played everything
up to this label."

### Integration Point: How Mark Fits the Goroutine Architecture

The challenge: mark must be echoed only after all previously queued PCMU frames have been played
(sent by rtpPacer). This requires the mark to travel through `packetQueue` in order.

**Approach: sentinel value in packetQueue**

`packetQueue` currently carries `[]byte` PCMU payloads. The simplest and most goroutine-safe
solution is to extend the queue element type to carry either audio or a mark sentinel, in order.
This preserves the FIFO property and requires no additional channel or synchronization.

**New type for queue elements (Go):**

```go
// outboundFrame is a tagged union for packetQueue elements.
// Audio frames carry a 160-byte PCMU payload. Mark frames carry a label to echo.
// Only one field is non-nil per instance.
type outboundFrame struct {
    audio []byte // non-nil for PCMU frames
    mark  string // non-empty for mark sentinel frames
}
```

**Data flow change:**

```
wsToRTP reads mark event from WS
    │ enqueues outboundFrame{mark: "label"} into packetQueue (after all prior audio)
    v
rtpPacer dequeues outboundFrame
    │ if frame.mark != "" → send mark name to markEchoQueue (new channel)
    │ if frame.audio != nil → send RTP packet as before
    v
wsPacer select loop gains new case:
    │ case markName := <-markEchoQueue → sendMarkEcho(wsConn, streamSid, markName, seqNo)
    │                                  → seqNo++
```

**New channel on CallSession:**

```go
markEchoQueue chan string  // capacity 10 — mark names to echo back to WS server
               // rtpPacer produces (when mark sentinel dequeued)
               // wsPacer consumes (in existing select loop)
```

**wsPacer select loop extension (adds one case):**

```go
for {
    select {
    case <-ctx.Done():
        return
    case digit := <-s.dtmfQueue:
        // existing DTMF handling
    case markName := <-s.markEchoQueue:   // NEW
        if err := sendMarkEcho(wsConn, s.streamSid, markName, seqNo); err != nil {
            sig.Signal()
            return
        }
        seqNo++
    case <-ticker.C:
        // existing 20ms media tick
    }
}
```

**Why markEchoQueue goes through wsPacer (not direct write from rtpPacer):**
rtpPacer must not write to wsConn — that violates the sole-writer invariant. The channel
handoff to wsPacer preserves the invariant without any mutex.

### New Components (Go)

| Item | File | Type |
|------|------|------|
| `outboundFrame` struct | `go/internal/bridge/session.go` | New type replacing `chan []byte` |
| `markEchoQueue chan string` | `go/internal/bridge/session.go` | New field on `CallSession` |
| `sendMarkEcho()` | `go/internal/bridge/ws.go` | New function (alongside sendDTMF) |
| `MarkEvent` / `MarkBody` structs | `go/internal/bridge/ws.go` | New JSON types |

### Modified Components (Go)

| File | Change |
|------|--------|
| `go/internal/bridge/session.go` (run) | Initialize `markEchoQueue` alongside other queues; change `packetQueue` to `chan outboundFrame` |
| `go/internal/bridge/session.go` (wsToRTP) | Add `case "mark"` to event switch; enqueue `outboundFrame{mark: name}` |
| `go/internal/bridge/session.go` (rtpPacer) | Change `packetQueue` drain to inspect `outboundFrame`; route mark to `markEchoQueue` |
| `go/internal/bridge/session.go` (wsPacer) | Add `case markName := <-s.markEchoQueue` to select |
| `go/internal/bridge/ws.go` | Add `MarkEvent`, `MarkBody` types and `sendMarkEcho()` |

---

## Feature 2: `clear` Event

### Protocol (Twilio Media Streams — official docs)

**From WS server to audio-dock (inbound on wsToRTP):**
```json
{
  "event": "clear",
  "streamSid": "MZxxx"
}
```

**Semantics:** Flush all buffered outbound audio immediately. Any mark sentinels already queued
must also be discarded (they refer to audio that will never play). The WS server uses this for
barge-in: when the user starts speaking, the backend sends clear to stop the TTS playback.

### Integration Point: How Clear Fits the Goroutine Architecture

`packetQueue` is a `chan outboundFrame` (after the mark change above). Draining a Go channel from
a non-owning goroutine requires care:

- **rtpPacer owns `packetQueue`** (consumer). Only rtpPacer should drain it.
- **wsToRTP receives the clear event** (the event arrives on the WS read path).

**Approach: clearSignal channel (single-fire, same pattern as wsSignal)**

```go
clearSignal chan struct{}  // closed by wsToRTP when clear event received
                           // inspected by rtpPacer each tick
```

**Why not drain the channel directly from wsToRTP?**
If wsToRTP drains `packetQueue` while rtpPacer is concurrently reading from it, both goroutines
race on the same channel. A clear signal channel avoids this: rtpPacer checks it each tick and
drains the queue itself, which is safe because rtpPacer is the sole consumer.

**rtpPacer clear handling:**

```go
case <-ticker.C:
    // Check for pending clear — drain entire queue before next audio frame
    select {
    case <-s.clearSignal:
        // Drain packetQueue (including any mark sentinels — barge-in discards them)
        for {
            select {
            case <-s.packetQueue:
            default:
                goto clearedDone
            }
        }
    clearedDone:
    default:
    }

    // Normal dequeue: next audio frame or silence
    var frame outboundFrame
    select {
    case frame = <-s.packetQueue:
    default:
        // send silence as before
    }
    // handle frame.audio or frame.mark as above
```

**New channel on CallSession:**

```go
clearSignal chan struct{}  // capacity 1 — wsToRTP sends, rtpPacer drains-on-next-tick
```

**Why capacity 1?** Multiple clear events within a single 20ms window collapse into one drain
operation at the rtpPacer tick. A buffered channel of 1 avoids wsToRTP blocking if clearSignal
is not drained before the next clear arrives.

**wsToRTP clear handling (new case in event switch):**

```go
case "clear":
    select {
    case s.clearSignal <- struct{}{}:
    default:
        // rtpPacer hasn't drained previous signal yet; that's fine — queue will be cleared
    }
```

### New Components (Go)

| Item | File | Type |
|------|------|------|
| `clearSignal chan struct{}` | `go/internal/bridge/session.go` | New field on `CallSession` |

### Modified Components (Go)

| File | Change |
|------|--------|
| `go/internal/bridge/session.go` (run) | Initialize `clearSignal` alongside other queues |
| `go/internal/bridge/session.go` (wsToRTP) | Add `case "clear"` to event switch; non-blocking send to `clearSignal` |
| `go/internal/bridge/session.go` (rtpPacer) | On each tick, check `clearSignal` and drain `packetQueue` before dequeue |

---

## Feature 3: SIP OPTIONS Keepalive

### Protocol and Semantics

Audio-dock sends an outbound OPTIONS request to `SIP_REGISTRAR` at a configurable interval (e.g.,
30s). If the registrar replies with 200 OK, the registration is confirmed alive. If the reply
is 408 (Timeout), 503 (Unavailable), or if no reply arrives within a timeout window, audio-dock
treats the registration as silently lost and triggers an immediate re-registration.

This solves the case where sipgate drops the registration record silently (no REGISTER expiry
timeout fires because the timer hasn't elapsed) — a scenario invisible to the existing
`reregisterLoop`.

### Integration Point: Where OPTIONS Keepalive Lives

The existing `Registrar` in `go/internal/sip/registrar.go` already manages registration state
(`registered atomic.Bool`, `reregisterLoop` goroutine). OPTIONS keepalive is a registration
health concern — it belongs in this same struct.

**New method on Registrar:**

```go
// optionsKeepaliveLoop sends periodic OPTIONS to the registrar and triggers
// immediate re-registration if the reply indicates liveness loss.
// Runs as a goroutine alongside reregisterLoop, both started from Register().
func (r *Registrar) optionsKeepaliveLoop(ctx context.Context, interval time.Duration) {
    ticker := time.NewTicker(interval)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            if err := r.sendOptions(ctx); err != nil {
                r.log.Warn().Err(err).Msg("OPTIONS keepalive failed — triggering re-registration")
                r.registered.Store(false)
                if r.metrics != nil {
                    r.metrics.SIPRegStatus.Set(0)
                }
                // Trigger immediate re-registration (not waiting for next reregisterLoop tick)
                go func() {
                    if _, err2 := r.doRegister(ctx); err2 != nil {
                        r.log.Error().Err(err2).Msg("Re-registration after OPTIONS failure failed")
                    }
                }()
            }
        }
    }
}
```

**sendOptions implementation with sipgo Client:**

OPTIONS uses the same `sipgo.Client` that already exists on `Registrar` for REGISTER. The pattern
is identical to `doRegister`: construct a `siplib.Request`, call `r.client.Do(ctx, req)`.

```go
func (r *Registrar) sendOptions(ctx context.Context) error {
    registrarURI := siplib.Uri{Host: r.registrar, Port: 5060}
    req := siplib.NewRequest(siplib.OPTIONS, registrarURI)

    aor := r.aorURI()
    fromH := &siplib.FromHeader{Address: aor, Params: siplib.NewParams()}
    fromH.Params.Add("tag", siplib.GenerateTagN(16))
    req.AppendHeader(fromH)
    req.AppendHeader(&siplib.ToHeader{Address: registrarURI})
    req.AppendHeader(siplib.NewHeader("User-Agent", "audio-dock/2.0"))

    optCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
    defer cancel()

    res, err := r.client.Do(optCtx, req)
    if err != nil {
        return fmt.Errorf("OPTIONS send: %w", err)
    }
    if res.StatusCode != 200 {
        return fmt.Errorf("OPTIONS rejected %d %s", res.StatusCode, res.Reason)
    }
    return nil
}
```

**Register() change — start optionsKeepaliveLoop alongside reregisterLoop:**

```go
func (r *Registrar) Register(ctx context.Context) error {
    expiry, err := r.doRegister(ctx)
    if err != nil {
        return err
    }
    r.log.Info()...Msg("SIP registration successful")
    go r.reregisterLoop(ctx, expiry)
    go r.optionsKeepaliveLoop(ctx, r.optionsInterval) // NEW
    return nil
}
```

**New config field:**

```go
// SIPOptionsInterval — interval for OPTIONS keepalive pings (0 = disabled)
SIPOptionsInterval int `env:"SIP_OPTIONS_INTERVAL" default:"30" usage:"SIP OPTIONS keepalive interval in seconds (0 to disable)"`
```

Adding `SIPOptionsInterval` to `config.Config` and passing it through `NewRegistrar` is the
only wiring change required.

### Response to inbound OPTIONS (existing handler.go)

`handler.go` already handles inbound OPTIONS at line 168 (`onOptions` sends 200 OK). This
is for OPTIONS probes sipgate sends TO audio-dock (checking if audio-dock is alive). The new
feature is audio-dock sending OPTIONS TO sipgate (checking if the registration is still active).
These are two distinct directions and require no changes to the existing inbound handler.

### New Components (Go)

| Item | File | Type |
|------|------|------|
| `optionsKeepaliveLoop` method | `go/internal/sip/registrar.go` | New method on `Registrar` |
| `sendOptions` method | `go/internal/sip/registrar.go` | New private method |
| `SIPOptionsInterval` field | `go/internal/config/config.go` | New env var field |

### Modified Components (Go)

| File | Change |
|------|--------|
| `go/internal/config/config.go` | Add `SIPOptionsInterval int` field |
| `go/internal/sip/registrar.go` | Add `optionsInterval` field to struct; update `NewRegistrar`; add `optionsKeepaliveLoop` + `sendOptions`; start loop from `Register()` |
| `go/cmd/audio-dock/main.go` | Pass `cfg.SIPOptionsInterval` to `NewRegistrar` (if interval is promoted to a parameter rather than read from cfg directly inside the registrar) |

---

## Node.js Equivalents

The Node.js implementation uses a different architecture (single-threaded event loop, callbacks,
no goroutines) but the same semantic behavior is required.

### mark/clear in Node.js (wsClient.ts)

The Node.js `wsClient.ts` already has `ws.on('message', ...)` in `onAudio()`. The `mark` and
`clear` events are currently ignored (line 258: "Ignore non-media events (mark, clear, etc.)").

**mark event:**
- When WS server sends mark, `wsToRTP` callback must enqueue a mark sentinel into the
  outbound drain queue (between the audio frames already queued there).
- When the sentinel reaches the front of the drain (i.e., the `sendPacket` callback fires with
  it), audio-dock sends the mark echo via `ws.send(...)`.
- Implementation: extend the `outboundDrain` queue element type from `Buffer` to
  `{ type: 'audio', payload: Buffer } | { type: 'mark', name: string }` and handle the mark
  type in the `sendPacket` callback inside `makeDrain`.

**clear event:**
- When WS server sends clear, drain the outbound queue immediately: `outboundDrain.stop()` then
  restart. Any pending marks also get discarded.
- `outboundDrain.stop()` already exists for the `stop()` path — reuse it.

**Files changed:**
- `node/src/ws/wsClient.ts`: extend `makeDrain` type, handle mark/clear in `onAudio` callback

### SIP OPTIONS keepalive in Node.js (userAgent.ts)

`userAgent.ts` builds raw SIP messages as strings. The OPTIONS request is structurally similar
to REGISTER (same header set, no body). Adding keepalive requires:

- A new `sendOptions()` function inside `createSipUserAgent` using the same socket
- A `setInterval` (or `setTimeout` chain) started after the initial 200 OK from REGISTER
- On timeout/error response to OPTIONS, immediately call `sendRegister()` to re-register

**Files changed:**
- `node/src/sip/userAgent.ts`: add `sendOptions()` helper and start interval in `resolve()` callback

---

## Complete Component Modification Summary

### Go — New Files

None. All additions fit within existing files.

### Go — Modified Files

| File | Lines Changed | Change Description |
|------|--------------|-------------------|
| `go/internal/bridge/session.go` | ~50 lines | New `outboundFrame` type; `markEchoQueue` + `clearSignal` fields; `run()` initializes new channels; `wsToRTP` handles mark/clear; `rtpPacer` inspects clear signal and mark frames; `wsPacer` drains `markEchoQueue` |
| `go/internal/bridge/ws.go` | ~20 lines | `MarkEvent`, `MarkBody` types; `sendMarkEcho()` function |
| `go/internal/sip/registrar.go` | ~50 lines | `optionsInterval` field; `optionsKeepaliveLoop` + `sendOptions` methods; `Register()` starts new goroutine |
| `go/internal/config/config.go` | ~3 lines | `SIPOptionsInterval` env var field |

### Node.js — Modified Files

| File | Lines Changed | Change Description |
|------|--------------|-------------------|
| `node/src/ws/wsClient.ts` | ~30 lines | Extend drain type to union; handle mark/clear in `onAudio` message handler |
| `node/src/sip/userAgent.ts` | ~30 lines | `sendOptions()` helper; keepalive interval started on registration success |

---

## Updated Data Flow Diagrams

### mark Event Data Flow (Go)

```
WS server sends:
{"event":"mark","streamSid":"MZ...","mark":{"name":"end-of-greeting"}}
    │
    v
wsToRTP reads WS frame
    │ case "mark": enqueue outboundFrame{mark: "end-of-greeting"} to packetQueue
    v
packetQueue [audio][audio][audio][mark:"end-of-greeting"]  ← mark is in-order
    │
    v
rtpPacer dequeues outboundFrame at 20ms tick
    │ frame.audio != nil → send RTP packet (as before)
    │ frame.mark != "" → non-blocking send to markEchoQueue
    v
markEchoQueue <- "end-of-greeting"
    │
    v
wsPacer select: case markName := <-s.markEchoQueue
    │ sendMarkEcho(wsConn, streamSid, "end-of-greeting", seqNo)
    v
WS server receives:
{"event":"mark","sequenceNumber":"N","streamSid":"MZ...","mark":{"name":"end-of-greeting"}}
```

### clear Event Data Flow (Go)

```
WS server sends:
{"event":"clear","streamSid":"MZ..."}
    │
    v
wsToRTP reads WS frame
    │ case "clear": non-blocking send to clearSignal channel
    v
clearSignal <- struct{}{}
    │
    v
rtpPacer next 20ms tick:
    │ select { case <-s.clearSignal: drain entire packetQueue }
    │ packetQueue drained → all buffered audio and mark sentinels discarded
    │ next tick: packetQueue is empty → send silence frame (as normal)
    v
Caller hears silence immediately (TTS playback interrupted)
```

### SIP OPTIONS Keepalive Data Flow (Go)

```
optionsKeepaliveLoop ticks every SIP_OPTIONS_INTERVAL seconds
    │
    v
sendOptions(ctx):
    │ construct OPTIONS sip:registrar SIP/2.0
    │ client.Do(ctx, req) → 5s timeout
    │
    ├── 200 OK → log trace; registration confirmed alive; no action
    │
    └── error or non-200:
            registered.Store(false)
            metrics.SIPRegStatus.Set(0)
            go doRegister(ctx) → immediate re-registration attempt
                │
                ├── success → registered.Store(true); metrics.SIPRegStatus.Set(1)
                └── failure → logged as error; reregisterLoop will retry next tick
```

---

## Goroutine Lifecycle Changes (v2.1)

The goroutine map per call gains no new goroutines. The changes are:

- `rtpPacer` gains a `clearSignal` check on each tick (non-blocking select)
- `wsPacer` gains a `markEchoQueue` case in its existing select loop
- `wsToRTP` handles two new event types (`mark`, `clear`) in its existing switch

The `Registrar` gains one new goroutine: `optionsKeepaliveLoop`. This goroutine is service-scoped
(not per-call), started alongside `reregisterLoop`, and exits when the root context is cancelled.

---

## Suggested Build Order

The three features are independent — they share no new code paths with each other. Build in this
order to minimize blast radius and enable incremental testing:

### Step 1: mark/clear (Go) — bridge layer only

1. Add `outboundFrame` type and change `packetQueue chan []byte` to `chan outboundFrame`
   - Update all existing producers (wsToRTP) and consumers (rtpPacer) to use the new type
   - All existing tests must still pass after this rename
2. Add `markEchoQueue`, `clearSignal` to `CallSession`; initialize in `run()`
3. Extend `wsToRTP` switch with `case "mark"` and `case "clear"`
4. Extend `rtpPacer` to route mark frames to `markEchoQueue` and drain on clear signal
5. Extend `wsPacer` select with `markEchoQueue` case
6. Add `MarkEvent`, `MarkBody`, `sendMarkEcho()` to `ws.go`

**Why this order:** Step 1 (type change) touches the most call sites and should be a standalone
commit before the logic is added. Steps 2-6 are purely additive.

### Step 2: SIP OPTIONS keepalive (Go) — sip/config layer

7. Add `SIPOptionsInterval` to `config.Config` with default 30
8. Add `optionsInterval` to `Registrar` struct; update `NewRegistrar`
9. Implement `sendOptions()` method
10. Implement `optionsKeepaliveLoop()` and start it from `Register()`

**Why after mark/clear:** Completely independent of bridge changes. Keeping them separate makes
rollback easier if there is a sipgo API issue.

### Step 3: Node.js equivalents

11. Extend `wsClient.ts` drain type; handle mark/clear in message handler
12. Add OPTIONS keepalive to `userAgent.ts`

**Why last:** Node.js is a reference implementation. Go changes are the primary deliverable.
The Node.js changes are smaller in scope and follow the same semantic patterns.

---

## Anti-Patterns to Avoid

### Anti-Pattern 1: Writing mark echo from rtpPacer directly to wsConn

**What people do:** When rtpPacer dequeues a mark sentinel, call `writeJSON(wsConn, markEvent)`
directly from rtpPacer.

**Why it's wrong:** rtpPacer and wsPacer are concurrent goroutines. wsConn is not safe for
concurrent writes. This violates the SOLE WS WRITER invariant and will cause frame corruption
or panics under concurrent load.

**Do this instead:** Route the mark name through `markEchoQueue` to wsPacer. wsPacer is the
sole writer and handles it in the same select loop as DTMF and media.

### Anti-Pattern 2: Draining packetQueue from wsToRTP on clear

**What people do:** When `case "clear"` is received in wsToRTP, drain `packetQueue` in a
`for { select { case <-packetQueue: ... default: break } }` loop from wsToRTP.

**Why it's wrong:** `packetQueue` has two endpoints: wsToRTP writes and rtpPacer reads. Draining
from wsToRTP while rtpPacer simultaneously dequeues is a data race on a `chan`, which in Go is
safe for concurrent sends/receives but causes non-deterministic interleaving: wsToRTP may drain
only some frames while rtpPacer drains others, leaving partial frame sequences delivered to the
caller mid-clear.

**Do this instead:** Signal rtpPacer via `clearSignal`. rtpPacer is the sole consumer and can
safely drain the entire queue in one atomic drain loop at its next tick.

### Anti-Pattern 3: Starting optionsKeepaliveLoop from reregisterLoop

**What people do:** Inside `reregisterLoop`, after a successful `doRegister`, also fire an
OPTIONS check inline.

**Why it's wrong:** reregisterLoop fires at 75% of the server-granted expiry interval (90s at
default Expires=120). That is too infrequent for liveness detection. OPTIONS keepalive needs its
own independent timer (default 30s).

**Do this instead:** `optionsKeepaliveLoop` is a separate goroutine with its own `time.Ticker`,
started once from `Register()`, independent of `reregisterLoop`.

### Anti-Pattern 4: Re-registration triggered by OPTIONS running in optionsKeepaliveLoop goroutine

**What people do:** Call `r.doRegister(ctx)` directly in `optionsKeepaliveLoop` (blocking the
keepalive ticker while re-registration is in flight, which can take up to 5s).

**Why it's wrong:** If re-registration takes the full 5s timeout and OPTIONS interval is 30s,
the keepalive ticker misses its next fire. Under repeated failure this cascades into drift.

**Do this instead:** Fire `go doRegister(ctx)` from `optionsKeepaliveLoop` so the ticker is not
blocked by the network round-trip. Concurrent re-registration with a scheduled `reregisterLoop`
tick is safe because `doRegister` is stateless (constructs a new request each time) and
`registered.Store` is atomic.

---

## Integration Points

### External Services (unchanged from v2.0)

| Service | Integration Pattern | New in v2.1 |
|---------|---------------------|-------------|
| sipgate trunking (SIP) | `sipgo.Client.Do()` over UDP:5060 | OPTIONS keepalive added: new request type on same client |
| AI voice-bot backend (WS) | gobwas/ws text frames, Twilio Media Streams JSON | mark echo (audio-dock → server) and mark/clear parsing (server → audio-dock) |

### Internal Boundaries (changed/new in v2.1)

| Boundary | Communication | Notes |
|----------|---------------|-------|
| `wsToRTP` → `rtpPacer` | `packetQueue chan outboundFrame` | Type changes from `chan []byte` to `chan outboundFrame`; both audio and mark sentinels travel in-order |
| `wsToRTP` → `rtpPacer` (clear) | `clearSignal chan struct{}` | New channel; wsToRTP signals, rtpPacer acts |
| `rtpPacer` → `wsPacer` (mark echo) | `markEchoQueue chan string` | New channel; rtpPacer produces mark names, wsPacer echoes them to WS server |
| `Registrar.optionsKeepaliveLoop` → `Registrar.doRegister` | Direct goroutine call | Self-contained within Registrar; no new cross-package dependency |
| `Registrar` → `config.Config` | `SIPOptionsInterval int` field | New config field read by NewRegistrar |

---

## Sources

- [Twilio Media Streams WebSocket Messages](https://www.twilio.com/docs/voice/media-streams/websocket-messages) — HIGH confidence; official Twilio docs, verified mark/clear schemas and semantics
- [emiago/sipgo GitHub](https://github.com/emiago/sipgo) — MEDIUM confidence; sipgo `client.Do()` for OPTIONS follows same pattern as for REGISTER; API verified via pkg.go.dev
- [sipgo pkg.go.dev](https://pkg.go.dev/github.com/emiago/sipgo) — MEDIUM confidence; `siplib.OPTIONS` method type, `NewRequest`, `Client.Do` return type
- Existing codebase — HIGH confidence; `session.go`, `ws.go`, `registrar.go`, `handler.go` read directly; goroutine model, queue types, and write invariants verified from source

---
*Architecture research for: audio-dock v2.1 (mark/clear events + SIP OPTIONS keepalive)*
*Researched: 2026-03-05*
