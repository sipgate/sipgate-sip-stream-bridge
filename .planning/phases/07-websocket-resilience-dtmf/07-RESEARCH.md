# Phase 7: WebSocket Resilience + DTMF - Research

**Researched:** 2026-03-04
**Domain:** WebSocket reconnect state machine in Go, RFC 4733 telephone-event DTMF deduplication
**Confidence:** HIGH

---

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| WSR-01 | If WebSocket disconnects during an active call, service reconnects with exponential backoff (1s/2s/4s cap, 30s budget) | Already marked Complete in REQUIREMENTS.md — structure exists in session.go via wsToRTP error handler; Phase 7 adds the full reconnect loop replacing the current single-BYE path |
| WSR-02 | After WebSocket reconnect, service re-sends `connected` then `start` before forwarding audio | `sendConnected` + `sendStart` already exist in ws.go; Phase 7 calls them again on each new wsConn after reconnect |
| WSR-03 | Audio arriving from RTP during WebSocket reconnect window is dropped (not buffered indefinitely) | rtpInboundQueue is bounded (50 frames); when full, rtpReader drops silently (already implemented); `wsPacer` must detect "reconnecting" state and skip sends — simplest: set wsConn to nil or use an atomic flag |
| WSB-07 | Service forwards DTMF digits as `dtmf` events (PT 113 telephone-event, RFC 4733 End-bit deduplication by RTP timestamp) | RFC 4733 verified: 4-byte payload (event, E+R+volume, duration); deduplication by RTP timestamp (sender retransmits end packet 3x with same timestamp); pion/rtp has no DTMF parser — hand-parse 4 bytes; Twilio `dtmf` event schema verified |
</phase_requirements>

---

## Summary

Phase 7 adds two independent features to the existing `CallSession` lifecycle in `session.go`: WebSocket reconnect resilience and DTMF forwarding.

The **reconnect feature** (WSR-01/WSR-02/WSR-03) requires refactoring the `run()` function in `session.go` so that a WebSocket error triggers a reconnect loop rather than immediately sending SIP BYE. The reconnect loop performs `ws.Dial` with exponential backoff (1s → 2s → 4s cap, 30s total budget). On each reconnect attempt, `wsPacer` and `wsToRTP` goroutines must be stopped (since they hold the old dead `net.Conn`) and restarted with the new connection. The `rtpReader` and `rtpPacer` goroutines are unaffected and continue running throughout reconnect. During the reconnect window, `wsPacer` must not write to the old dead connection; the simplest approach is to drain-and-restart the goroutines atomically around the new `wsConn`. After successful reconnect, `connected` then `start` are re-sent before audio forwarding resumes (WSR-02). RTP packets arriving during the window are already dropped by the bounded `rtpInboundQueue` without blocking `rtpReader` (WSR-03 already satisfied by existing design).

The **DTMF feature** (WSB-07) requires detecting telephone-event RTP packets in `rtpReader`, parsing the 4-byte RFC 4733 payload to extract the event digit and End bit, deduplicating retransmitted end-packets by RTP timestamp, and emitting a Twilio `dtmf` JSON event through `wsPacer` (the sole WS writer). The telephone-event payload is exactly 4 bytes: `event` (1 byte, 0–15 maps to digit), `E|R|volume` (1 byte — E is bit 7), `duration` (2 bytes big-endian). No external library is needed. Deduplication is done by tracking the last emitted RTP timestamp per call; if the incoming telephone-event packet's RTP timestamp equals the last emitted timestamp, it is a retransmission and is dropped.

**Primary recommendation:** Implement Phase 7 as two focused plans — 07-01 for WS reconnect loop (WSR-01/WSR-02/WSR-03) refactoring session.go + manager.go, and 07-02 for DTMF parsing + forwarding (WSB-07) adding to rtpReader + wsPacer.

---

## Standard Stack

### Core (no new dependencies required)

| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `github.com/gobwas/ws` | v1.3.2 | Re-dial WebSocket on reconnect: `ws.Dial(ctx, url)` | Already in go.mod; same API used in Phase 6 `dialWS` |
| `github.com/gobwas/ws/wsutil` | v1.3.2 | `WriteClientText` for re-sending connected/start after reconnect; `ReadServerData` in wsToRTP | Already in go.mod |
| `net` stdlib | stdlib | `net.Conn.SetReadDeadline` to unblock wsToRTP on reconnect; TCP\_NODELAY pattern already in dialWS | stdlib |
| `time` stdlib | stdlib | Exponential backoff ticker: `time.Sleep(backoff)` with doubling up to 4s cap | stdlib |
| `context` stdlib | stdlib | Reconnect budget: `context.WithTimeout(dlgCtx, 30s)` wraps the reconnect loop | stdlib |
| `github.com/pion/rtp` | v1.10.1 | `rtp.Packet.Unmarshal` — already used in rtpReader; DTMF payload parsed manually from `pkt.Payload` (4 bytes) | Already in go.mod |
| `encoding/binary` | stdlib | `binary.BigEndian.Uint16(payload[2:4])` for reading duration field in telephone-event payload | stdlib |

### No New Dependencies

All Phase 7 features use libraries already in `go.mod`. No `go get` commands are required.

---

## Architecture Patterns

### Recommended File Changes for Phase 7

```
internal/bridge/
├── session.go    # MODIFY: refactor run() for reconnect loop; add DTMF state to CallSession
├── ws.go         # MODIFY: add DtmfEvent struct + sendDTMF helper
└── (no new files needed)
```

### Pattern 1: WebSocket Reconnect Loop in run()

**What:** Replace the current model (wsToRTP error → BYE) with a reconnect loop that restarts
only the two WS-dependent goroutines (`wsPacer`, `wsToRTP`) while keeping `rtpReader` and
`rtpPacer` running throughout.

**When to use:** When `wsToRTP` or `wsPacer` detects a WS write/read error and the dialog
context is still alive (i.e., the SIP call is still active).

**Key insight:** The 4-goroutine model splits naturally into two groups:
- **RTP goroutines** (`rtpReader`, `rtpPacer`): touch only `rtpConn` — not affected by WS loss
- **WS goroutines** (`wsPacer`, `wsToRTP`): touch only `wsConn` — must restart on reconnect

This means reconnect only needs to stop and restart the 2 WS goroutines, not all 4.

```go
// Source: Go stdlib context + time patterns; gobwas/ws.Dial API from pkg.go.dev

// In session.go — refactored run():

func (s *CallSession) run(ctx context.Context, initialWsConn net.Conn) {
    // ... existing Pitfall-6 check, defer wsConn.Close(), sessionCtx, rtpConn setup ...

    // sendConnected + sendStart on initial connection
    wsConn := initialWsConn
    if err := s.handshake(wsConn); err != nil {
        // ... error handling unchanged
    }

    // Launch RTP goroutines (persistent for full call lifetime — never restarted)
    s.wg.Add(2)
    go s.rtpReader(sessionCtx, rtpConn)
    go s.rtpPacer(sessionCtx, rtpConn)

    // WS goroutines are started/stopped per connection; wsDone signals WS-layer failure
    for {
        wsDone := make(chan struct{})
        wsWg := &sync.WaitGroup{}
        wsWg.Add(2)
        go s.wsPacer(sessionCtx, wsConn, wsWg, wsDone)
        go s.wsToRTP(sessionCtx, wsConn, wsWg, wsDone)

        select {
        case <-sessionCtx.Done():
            // Normal call end — unblock WS goroutines and wait
            wsConn.SetReadDeadline(time.Now())
            wsWg.Wait()
            rtpConn.Close() // unblock rtpReader
            s.wg.Wait()     // drain rtp goroutines
            sendStop(wsConn, s.streamSid, s.callSidToken) // nolint: errcheck
            return

        case <-wsDone:
            // WS error — attempt reconnect
            wsConn.Close()
            wsWg.Wait() // drain old WS goroutines before reconnecting
            newConn, ok := s.reconnect(sessionCtx)
            if !ok {
                // Budget exhausted — BYE the SIP call
                s.log.Error().Str("call_id", s.callID).Msg("WS reconnect budget exhausted — sending BYE")
                _ = s.dlg.Bye(context.Background())
                s.cancel()
                rtpConn.Close()
                s.wg.Wait()
                return
            }
            wsConn = newConn
            // WSR-02: re-send handshake on every reconnect
            if err := s.handshake(wsConn); err != nil {
                // treat as another WS failure — loop again
                close(wsDone) // will be re-created next iteration
                continue
            }
        }
    }
}

// handshake sends connected + start events on a fresh wsConn.
func (s *CallSession) handshake(wsConn net.Conn) error {
    if err := sendConnected(wsConn); err != nil {
        return err
    }
    return sendStart(wsConn, s.streamSid, s.callSidToken, s.callID, s.dlg.InviteRequest)
}
```

### Pattern 2: Exponential Backoff Reconnect (WSR-01)

**What:** Reconnect loop with 1s → 2s → 4s (capped) backoff, 30s total budget.

```go
// Source: Go stdlib time.Sleep + context.WithDeadline patterns
// Source: REQUIREMENTS.md WSR-01 — "1s/2s/4s cap, 30s budget"

func (s *CallSession) reconnect(ctx context.Context) (net.Conn, bool) {
    // 30-second total budget for reconnect attempts (WSR-01)
    budget, cancel := context.WithTimeout(ctx, 30*time.Second)
    defer cancel()

    backoff := time.Second // start at 1s (WSR-01 spec)
    const maxBackoff = 4 * time.Second // cap at 4s (WSR-01 spec)
    attempt := 0

    for {
        attempt++
        s.log.Warn().
            Str("call_id", s.callID).
            Int("attempt", attempt).
            Dur("backoff", backoff).
            Msg("WebSocket disconnected — reconnecting")

        // Wait for backoff duration or budget expiry
        select {
        case <-budget.Done():
            s.log.Error().Str("call_id", s.callID).Msg("WS reconnect budget exhausted")
            return nil, false
        case <-time.After(backoff):
        }

        // Double backoff for next attempt, capped at maxBackoff
        backoff *= 2
        if backoff > maxBackoff {
            backoff = maxBackoff
        }

        dialCtx, dialCancel := context.WithTimeout(budget, 5*time.Second)
        conn, err := dialWS(dialCtx, s.cfg.WSTargetURL)
        dialCancel()
        if err != nil {
            s.log.Warn().Err(err).Str("call_id", s.callID).Msg("WS reconnect dial failed")
            continue
        }

        s.log.Info().Str("call_id", s.callID).Int("attempt", attempt).Msg("WebSocket reconnected")
        return conn, true
    }
}
```

### Pattern 3: WS Goroutine Signaling with wsDone Channel

**What:** `wsPacer` and `wsToRTP` each close a shared `wsDone` channel on error. The run() loop
selects on `wsDone` to detect WS failure and trigger reconnect.

**Critical:** `close(wsDone)` is safe to call from either goroutine exactly once because Go's
close panics on double-close. Use `sync.Once` to guard the close.

```go
// Source: Go stdlib sync.Once pattern for single-fire signaling

type wsSignal struct {
    once sync.Once
    ch   chan struct{}
}

func newWsSignal() *wsSignal {
    return &wsSignal{ch: make(chan struct{})}
}

func (w *wsSignal) Signal() {
    w.once.Do(func() { close(w.ch) })
}

func (w *wsSignal) Done() <-chan struct{} {
    return w.ch
}
```

**Alternative (simpler):** `wsPacer` and `wsToRTP` each call `s.cancel()` + set an atomic flag
`s.wsError.Store(true)`. run() detects this via sessionCtx.Done() and checks the flag.
This avoids the wsDone channel complexity but makes it harder to distinguish WS error vs
normal BYE from SIP side.

**Recommendation:** Use the `wsDone` channel approach with `sync.Once` — it's clear and
distinguishes WS failure from SIP teardown.

### Pattern 4: RFC 4733 Telephone-Event Payload Parsing (WSB-07)

**What:** Parse the 4-byte telephone-event RTP payload manually. No external library needed.

**RFC 4733 payload structure (verified from RFC text):**

```
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|     event     |E|R| volume    |          duration             |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
```

- **Byte 0**: event code (0=`0`, 1=`1`, ... 9=`9`, 10=`*`, 11=`#`, 12=`A`, 13=`B`, 14=`C`, 15=`D`)
- **Byte 1, bit 7 (MSB)**: End bit — 1 means this is the last packet for this digit
- **Byte 1, bits 0-5**: volume (not needed for forwarding)
- **Bytes 2-3**: duration in timestamp units (not needed for forwarding)

```go
// Source: RFC 4733 Section 2.3 — https://www.rfc-editor.org/rfc/rfc4733.html
// Source: pion/rtp Packet.Unmarshal for payload extraction

// dtmfEventToDigit maps RFC 4733 event codes 0-15 to digit characters.
var dtmfEventToDigit = [16]string{
    "0", "1", "2", "3", "4", "5", "6", "7", "8", "9",
    "*", "#", "A", "B", "C", "D",
}

// parseTelephoneEvent parses the 4-byte RFC 4733 telephone-event payload.
// Returns (digit, isEnd, ok). ok is false if payload is malformed.
func parseTelephoneEvent(payload []byte) (digit string, isEnd bool, ok bool) {
    if len(payload) < 4 {
        return "", false, false
    }
    eventCode := payload[0]
    if eventCode > 15 {
        return "", false, false // only 0-15 (standard DTMF digits) supported
    }
    isEnd = (payload[1] & 0x80) != 0 // End bit = MSB of byte 1
    return dtmfEventToDigit[eventCode], isEnd, true
}
```

### Pattern 5: DTMF Deduplication by RTP Timestamp (WSB-07)

**What:** RFC 4733 requires the sender to retransmit the final (End=1) packet 3 times, all
with the same RTP timestamp. Deduplication prevents 3 `dtmf` events per keypress.

**Mechanism:** Track the last forwarded DTMF RTP timestamp per session. When an End=1 packet
arrives with the same timestamp as the last emitted digit, drop it as a retransmission.

```go
// Source: RFC 4733 §2.5 — "The final packet for each event SHOULD be sent a total of
// three times at the interval used by the source for updates."
// Source: STATE.md — "End-bit deduplication by RTP timestamp" (WSB-07 requirement)

// In CallSession struct, add:
type CallSession struct {
    // ... existing fields ...
    lastDtmfTS uint32 // RTP timestamp of the last forwarded DTMF End packet (for dedup)
}

// In rtpReader, when pkt.PayloadType == s.dtmfPT:
digit, isEnd, ok := parseTelephoneEvent(pkt.Payload)
if !ok {
    continue
}
if isEnd {
    if pkt.Header.Timestamp == s.lastDtmfTS {
        continue // duplicate End packet — drop (RFC 4733 retransmission dedup)
    }
    s.lastDtmfTS = pkt.Header.Timestamp
    // Enqueue DTMF event for wsPacer to send
    select {
    case s.dtmfQueue <- digit:
    default:
        s.log.Warn().Str("digit", digit).Msg("rtpReader: DTMF queue full — dropping digit")
    }
}
// Non-End packets: drop silently (digit not yet complete)
```

**Why End bit, not any packet?** Multiple telephone-event packets arrive per keypress (one per
20 ms update interval while the key is held). Only the final End=1 packet signals that the
key was released — that is the correct moment to emit one `dtmf` event.

**Why timestamp, not sequence number?** RFC 4733 specifies that retransmissions of the End
packet carry the same RTP timestamp. Sequence numbers advance. The timestamp is the correct
dedup key.

### Pattern 6: DTMF Forwarding Channel (wsToRTP to wsPacer)

**What:** A small bounded channel carries digit strings from `rtpReader` to `wsPacer`.
`wsPacer` is the sole WS writer and handles the actual JSON send.

```go
// In CallSession struct:
dtmfQueue chan string // carries digit strings ("0"-"9","*","#","A"-"D") to wsPacer
// Size: 10 — more than enough for any realistic keypress burst

// In run(), after rtpInboundQueue init:
s.dtmfQueue = make(chan string, 10)

// In wsPacer's select loop, add a case alongside the 20ms ticker:
select {
case <-ctx.Done():
    return
case digit := <-s.dtmfQueue:
    // DTMF event takes priority over audio — send immediately (not paced)
    if err := sendDTMF(wsConn, s.streamSid, digit, seqNo); err != nil {
        s.log.Error().Err(err).Str("digit", digit).Msg("wsPacer: sendDTMF failed")
        return
    }
    seqNo++
case <-ticker.C:
    // ... existing audio forwarding ...
}
```

### Pattern 7: Twilio `dtmf` Event JSON Schema

**What:** The Twilio Media Streams `dtmf` event (verified from official Twilio docs 2026-03-04).

```go
// Source: https://www.twilio.com/docs/voice/media-streams/websocket-messages
// Verified 2026-03-04

type DtmfEvent struct {
    Event          string   `json:"event"`          // "dtmf"
    StreamSid      string   `json:"streamSid"`
    SequenceNumber string   `json:"sequenceNumber"`
    Dtmf           DtmfBody `json:"dtmf"`
}

type DtmfBody struct {
    Track string `json:"track"` // "inbound_track" (Twilio spec)
    Digit string `json:"digit"` // "0"-"9", "*", "#"
}

func sendDTMF(conn net.Conn, streamSid, digit string, seqNo uint32) error {
    return writeJSON(conn, DtmfEvent{
        Event:          "dtmf",
        StreamSid:      streamSid,
        SequenceNumber: fmt.Sprintf("%d", seqNo),
        Dtmf: DtmfBody{
            Track: "inbound_track",
            Digit: digit,
        },
    })
}
```

### Anti-Patterns to Avoid

- **Stopping rtpReader/rtpPacer on WS reconnect:** These goroutines are independent of the WS
  connection. Stopping them would interrupt NAT keepalive RTP and cause unnecessary latency.
  Only the 2 WS-layer goroutines need to be restarted.

- **Using a mutex to guard wsConn across reconnect:** The simplest and safest approach is to
  drain the old WS goroutines before replacing `wsConn`. A mutex shared with `wsPacer` and
  `wsToRTP` creates complexity and still requires the goroutines to be aware of the "reconnecting"
  state. Drain-and-restart is cleaner.

- **Double-close on wsDone channel:** If both `wsPacer` and `wsToRTP` close the channel, the
  second close panics. Always use `sync.Once` to guard the close (Pattern 3 above).

- **Forwarding DTMF on every telephone-event packet:** Multiple packets arrive per keypress.
  Only the End=1 packet signals digit completion. Forwarding on every packet sends 3-10 `dtmf`
  events per keypress.

- **Using sequence number for DTMF deduplication:** RFC 4733 retransmissions advance the
  sequence number. The RTP timestamp stays the same. Always deduplicate by timestamp.

- **Parsing DTMF event codes > 15:** Only 0-15 are standard DTMF digits. Codes 16-255 exist
  for tones/signals outside the scope of this project. Guard with `if eventCode > 15 { continue }`.

---

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Telephone-event payload parser | Full codec library | 4 lines of manual byte parsing | RFC 4733 payload is exactly 4 bytes, completely specified; pion/rtp has no DTMF parser — this IS the hand-roll, but it's appropriate because the spec is trivial |
| Exponential backoff with jitter | Custom retry library | `time.Sleep(backoff)` with double-and-cap | The backoff schedule is fixed (1s/2s/4s cap, 30s) per WSR-01 spec; no jitter needed; stdlib is sufficient |
| WS write serialization | Global mutex | Sole-writer goroutine pattern (wsPacer) | Already established in Phase 6; extending it with DTMF channel maintains the invariant without new primitives |
| WS reconnect library | Third-party reconnect package | Manual `dialWS` loop in `reconnect()` | The reconnect sequence includes application-level state (re-send connected+start); a generic library can't know this |

**Key insight:** The reconnect sequence is not just "re-dial" — it requires re-sending the
Twilio handshake (`connected` + `start`) before audio resumes. Any generic reconnect library
would need to be told about this application-level state anyway, so a custom 20-line
`reconnect()` function is both simpler and more correct.

---

## Common Pitfalls

### Pitfall 1: Double-Close Panic on wsDone Channel

**What goes wrong:** Both `wsPacer` and `wsToRTP` detect WS failure (write error and read
error respectively) and both call `close(wsDone)`. Second `close` panics with
`close of closed channel`.

**Why it happens:** The two goroutines share the signal channel but don't coordinate which
one closes it.

**How to avoid:** Use `sync.Once` to wrap the close (Pattern 3 above). Both goroutines call
`sig.Signal()` — only the first call closes the channel.

**Warning signs:** Panic stacktrace pointing to a `close(wsDone)` call.

### Pitfall 2: wsPacer Continues Writing to Dead wsConn During Reconnect

**What goes wrong:** After WS error, `wsPacer` is still running and tries to write to the
closed/dead `net.Conn`. This produces cascading errors and blocks the reconnect path.

**Why it happens:** `wsPacer` has its own ticker loop and doesn't know the connection died
unless signaled.

**How to avoid:** The `wsDone` channel signal causes `run()` to call
`wsConn.SetReadDeadline(time.Now())` which unblocks `wsToRTP`, AND close `wsConn` which
causes `wsPacer`'s next `writeJSON` to fail, which makes wsPacer exit. Wait on `wsWg.Wait()`
before creating the new connection.

**Warning signs:** Multiple "writeJSON failed" log lines racing after reconnect begins.

### Pitfall 3: RTP Goroutines Blocked When WS Reconnect Completes

**What goes wrong:** `rtpReader` is blocked on `ReadFromUDP`, which is fine. But `rtpPacer`
is sending RTP to the caller even when WS is down — this is correct behavior. The issue is
if someone mistakenly closes `rtpConn` during reconnect, which would stop `rtpReader` and
require it to be restarted too.

**Why it happens:** Confusion between WS-layer shutdown and full session shutdown.

**How to avoid:** During WS reconnect, ONLY close the `wsConn`. Never close `rtpConn` until
the session is fully ending. The `rtpConn.Close()` call in `run()` is guarded to only happen
after `sessionCtx.Done()`.

**Warning signs:** RTP goroutines do not restart after reconnect; silence on both directions.

### Pitfall 4: DTMF Digit Forwarded Multiple Times Per Keypress

**What goes wrong:** `dtmf` events appear 3x per key press on the WebSocket.

**Why it happens:** RFC 4733 mandates that the End=1 packet is transmitted 3 times. Without
deduplication, all 3 end packets trigger a `sendDTMF` call.

**How to avoid:** Deduplicate by RTP timestamp (Pattern 5). `lastDtmfTS` tracks the timestamp
of the last forwarded digit. Any End packet with the same timestamp is a retransmission.

**Warning signs:** Each DTMF keypress produces exactly 3 identical `dtmf` events.

### Pitfall 5: DTMF Packet with End=0 Triggers Premature Forwarding

**What goes wrong:** `dtmf` events appear every 20 ms while a key is held (as many packets as
the key is held for).

**Why it happens:** Forwarding DTMF on any telephone-event packet rather than waiting for
End=1.

**How to avoid:** Only forward when `isEnd == true` (Pattern 5).

**Warning signs:** Holding a key for 1 second produces ~50 `dtmf` events instead of 1.

### Pitfall 6: DTMF Queue Not Initialized Before rtpReader Starts

**What goes wrong:** `rtpReader` calls `s.dtmfQueue <- digit` but `s.dtmfQueue` is nil —
send on nil channel blocks forever, hanging `rtpReader`.

**Why it happens:** `dtmfQueue` is initialized in `run()` after goroutines are launched.

**How to avoid:** Initialize `dtmfQueue` before launching `rtpReader` (same pattern as
`rtpInboundQueue` in the current code).

**Warning signs:** `rtpReader` hangs immediately on first DTMF keypress.

### Pitfall 7: Reconnect Loop Does Not Stop When Call Ends

**What goes wrong:** `reconnect()` blocks inside `time.After(backoff)` while the SIP side
has sent BYE and `sessionCtx` is already done. The session hangs for up to 4 seconds after
BYE.

**Why it happens:** `time.After` is not context-aware.

**How to avoid:** The `select` in `reconnect()` must select on both `budget.Done()` AND
`time.After(backoff)`. Since `budget` is derived from `ctx` (which is `sessionCtx`), it
cancels when the dialog ends (Pattern 2 above).

**Warning signs:** Session goroutine still active in logs several seconds after BYE received.

---

## Code Examples

Verified patterns from official sources:

### RFC 4733 Telephone-Event Byte Layout

```
 Byte 0:  event code (0-15 for DTMF)
 Byte 1:  E(1) R(1) volume(6)   — E=0x80 mask
 Byte 2-3: duration (big-endian uint16, timestamp units)
```

```go
// Source: RFC 4733 Section 2.3 — https://www.rfc-editor.org/rfc/rfc4733.html#section-2.3
// Parsing example:
payload := pkt.Payload // from pion/rtp Packet.Unmarshal
if len(payload) < 4 { continue }
eventCode := payload[0]                  // 0="0" ... 9="9", 10="*", 11="#"
isEnd     := (payload[1] & 0x80) != 0   // End bit = MSB of byte 1
// duration = binary.BigEndian.Uint16(payload[2:4]) // not needed for forwarding
```

### Twilio DTMF Event JSON

```json
// Source: https://www.twilio.com/docs/voice/media-streams/websocket-messages
// Verified 2026-03-04
{
  "event": "dtmf",
  "streamSid": "MZxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
  "sequenceNumber": "5",
  "dtmf": {
    "track": "inbound_track",
    "digit": "1"
  }
}
```

### Exponential Backoff (1s → 2s → 4s cap)

```go
// Source: Go stdlib time.Sleep; backoff schedule from REQUIREMENTS.md WSR-01
backoff := time.Second
const maxBackoff = 4 * time.Second
for attempt := 1; ; attempt++ {
    select {
    case <-budget.Done():
        return nil, false
    case <-time.After(backoff):
    }
    conn, err := dialWS(dialCtx, url)
    if err == nil {
        return conn, true
    }
    backoff *= 2
    if backoff > maxBackoff {
        backoff = maxBackoff
    }
}
```

### gobwas/ws: Unblocking a Blocked ReadServerData for Clean Shutdown

```go
// Source: pkg.go.dev/github.com/gobwas/ws — "It is a caller responsibility to manage
// I/O deadlines on conn."
// Set an expired deadline to unblock wsToRTP (ReadServerData) immediately:
_ = wsConn.SetReadDeadline(time.Now())
// wsToRTP's readWSMessage call returns an error; ctx.Err() != nil check makes it exit cleanly.
```

---

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| WS error → immediate SIP BYE | WS error → reconnect loop (1s/2s/4s, 30s budget) | Phase 7 | Call survives transient WS disconnects |
| DTMF packets silently dropped in rtpReader | DTMF packets parsed and forwarded as `dtmf` events | Phase 7 | WSB-07 satisfied; callers can interact with IVR/dialpad consumers |
| wsToRTP owns WS-error-triggered BYE | wsDone channel signals run() loop | Phase 7 | Reconnect logic centralized in run(); wsToRTP/wsPacer become pure I/O workers |

---

## Open Questions

1. **Should wsPacer or run() send the `dtmf` event?**
   - What we know: `wsPacer` is the sole WS writer; sending DTMF from any other goroutine
     violates the write-safety invariant.
   - What's unclear: Whether DTMF should be paced at 20 ms intervals or sent immediately on
     receipt from `dtmfQueue`.
   - Recommendation: `wsPacer` sends DTMF immediately when a digit arrives in `dtmfQueue`
     (between ticker ticks). DTMF is a control event, not audio — it should not be delayed by
     the 20 ms audio tick. Add a `case digit := <-s.dtmfQueue:` to `wsPacer`'s select that
     fires before the audio tick case.

2. **What happens if WS reconnect succeeds but the WS consumer has no memory of the stream?**
   - What we know: WSR-02 requires re-sending `connected` + `start` on every reconnect.
     This resets the WS consumer's stream state.
   - What's unclear: Whether the WS consumer handles a fresh `connected` + `start` mid-call
     gracefully (e.g., does it expect a unique streamSid each time, or is reusing the same
     streamSid idiomatic?).
   - Recommendation: Reuse the same `streamSid` and `callSidToken` (they identify the SIP
     call, not the WS connection). The WS consumer should treat the reconnected stream as a
     continuation of the same call. Document this assumption in code comments.

3. **Should `lastDtmfTS` be reset between reconnects?**
   - What we know: After a WS reconnect, the WS consumer sees a fresh stream. If the same
     digit is pressed again after reconnect, its RTP timestamp will be different (new keypress),
     so deduplication still works correctly without resetting `lastDtmfTS`.
   - Recommendation: Do not reset `lastDtmfTS` on reconnect. RTP timestamps increment
     continuously and will never collide across different keypresses.

---

## Sources

### Primary (HIGH confidence)

- RFC 4733 — https://www.rfc-editor.org/rfc/rfc4733.html — 4-byte telephone-event payload
  wire format verified; End bit semantics and 3x retransmission requirement verified
- Twilio Media Streams docs — https://www.twilio.com/docs/voice/media-streams/websocket-messages
  — `dtmf` event schema verified 2026-03-04: `{event, streamSid, sequenceNumber, dtmf: {track, digit}}`
- `gobwas/ws` pkg.go.dev v1.3.2 — https://pkg.go.dev/github.com/gobwas/ws@v1.3.2 — `ws.Dial`
  API and `net.Conn` deadline management confirmed; no built-in reconnect helpers confirmed
- `internal/bridge/session.go` (current implementation) — 4-goroutine model (rtpReader,
  wsPacer, wsToRTP, rtpPacer) with channels verified; existing RTP drop behavior under queue
  saturation confirmed
- `internal/bridge/ws.go` (current implementation) — `sendConnected`, `sendStart`, `sendStop`,
  `writeJSON`, `dialWS` (TCP_NODELAY) all confirmed available for reuse
- `internal/bridge/manager.go` (current implementation) — `StartSession` structure confirmed;
  WS dial happens here before `session.run()`

### Secondary (MEDIUM confidence)

- `github.com/pion/rtp@v1.10.1` pkg.go.dev — no DTMF-specific types confirmed; manual parsing
  of `pkt.Payload` is the correct approach
- Go stdlib `sync.Once` documentation — guaranteed single-execution; correct primitive for
  single-fire channel close

### Tertiary (LOW confidence — validate at integration test)

- DTMF digit map (event codes 0-15): 0-9 map to "0"-"9", 10="*", 11="#", 12="A"-15="D" —
  standard RFC 4733 mapping, unverified against sipgate's actual event code values
- sipgate's End-bit retransmission behavior: RFC 4733 mandates 3 retransmissions of End=1;
  assuming sipgate complies — deduplication is needed but exact count unverified

---

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH — no new deps; all libraries already in go.mod; APIs verified
- Architecture patterns: HIGH — goroutine model directly derived from existing session.go;
  RFC 4733 payload is a spec document (authoritative)
- Reconnect backoff: HIGH — schedule is specified in REQUIREMENTS.md WSR-01; stdlib is sufficient
- DTMF parsing: HIGH — RFC 4733 is stable (2006); 4-byte payload format unchanged
- DTMF deduplication: HIGH — RFC 4733 §2.5 specifies 3x retransmission; timestamp dedup is
  the canonical approach
- Twilio dtmf schema: HIGH — verified from official Twilio docs 2026-03-04
- Pitfalls: HIGH — derived from existing implementation patterns and Go concurrency fundamentals

**Research date:** 2026-03-04
**Valid until:** 2026-06-04 (RFC 4733 is stable; Twilio Media Streams protocol is stable;
gobwas/ws v1.3.x API is stable; recheck if pion/rtp gains native DTMF support)
