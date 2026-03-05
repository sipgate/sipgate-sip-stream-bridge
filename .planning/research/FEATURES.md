# Feature Research

**Domain:** SIP-to-WebSocket audio bridge / SIP media gateway — v2.1 milestone (mark/clear + OPTIONS keepalive)
**Researched:** 2026-03-05
**Confidence:** HIGH for Twilio mark/clear protocol (official docs verified); HIGH for SIP OPTIONS pattern (RFC 3261 + IETF draft verified); MEDIUM for sipgo-specific OPTIONS API (pkg.go.dev verified, no running example found)

---

## Scope of This Document

This document covers the **three new features** for the v2.1 milestone, added on top of the complete v2.0 feature set. The v2.0 feature research (connected/start/media/stop/dtmf, WS reconnect, SIGTERM drain, /health, /metrics) is preserved in the "Previously Shipped" section for reference but is not re-analyzed.

New features being researched:
1. `mark` event — WS server labels a point in the outbound audio stream; audio-dock echoes it when playback reaches that point
2. `clear` event — WS server flushes the outbound audio buffer and cancels pending marks
3. SIP OPTIONS keepalive — periodic out-of-dialog OPTIONS to sipgate to detect silent registration loss

---

## Feature Landscape

### Table Stakes (Users Expect These)

Features that a Twilio Media Streams-compatible bidirectional bridge must have. "Complete protocol support" means handling all documented event types without crashing or ignoring them silently.

| Feature | Why Expected | Complexity | Notes |
|---------|--------------|------------|-------|
| `mark` echo: receive mark from WS server, echo it when packetQueue drains to that point | Any AI TTS backend using barge-in or play-and-wait depends on mark acknowledgment to know when audio finished playing | MEDIUM | Requires correlating a mark name with a queue drain event. The mark must be echoed AFTER the last queued audio frame before the mark was enqueued — not immediately on receipt. See protocol semantics below. |
| `clear` flush: receive clear from WS server, drain packetQueue immediately | barge-in (caller interrupts TTS playback) requires flushing queued audio so the RTP pacer stops sending the old TTS audio | LOW | `clear` is a one-liner in principle: drain the buffered channel. The tricky part is concurrency — rtpPacer and wsToRTP both touch packetQueue. See edge cases below. |
| `clear` triggers pending mark echoes: after clear, immediately echo all marks that were waiting for audio to drain | WS server tracks which marks were cleared vs played — incorrect mark echo timing corrupts its state machine | MEDIUM | Twilio spec: "If your server sends a clear message, Twilio empties the audio buffer and sends back mark messages matching any remaining mark messages from your server." audio-dock must mirror this: clear → drain packetQueue → echo all pending marks immediately. |
| Unknown WS event types ignored gracefully (no crash, no reconnect) | Protocol extensibility — future Twilio events or WS server experiments must not break the bridge | LOW | Already handled in wsToRTP default case. Mark and clear must be added to the switch. |

### Differentiators (Beyond Minimum Protocol Compliance)

| Feature | Value Proposition | Complexity | Notes |
|---------|-------------------|------------|-------|
| SIP OPTIONS keepalive with re-registration trigger | Silent registration loss is invisible without it. sipgate can drop the binding without sending a 401/403 (NAT binding expired, server-side timeout, network hiccup). A dead registration means no inbound calls arrive — no error, just silence. | MEDIUM | Out-of-dialog OPTIONS to the sipgate registrar every N seconds (30–60s typical). On timeout or 5xx response: set `registered=false`, trigger immediate re-REGISTER. On 200 OK: do nothing (registration is live). |
| OPTIONS interval shorter than SIP_EXPIRES | Detects registration loss before the next scheduled re-REGISTER tick, reducing the window of missed calls | LOW | With SIP_EXPIRES=120s and re-register at 75% (90s), a silent loss could mean up to 90s of missed calls. OPTIONS at 30s reduces this to ~30s worst case. |
| OPTIONS failure metric in Prometheus | Ops team can alert on OPTIONS failures before callers start complaining | LOW | Increment a `sip_options_failures_total` counter on each failed probe. Complements existing `sip_reg_status` gauge. |

### Anti-Features (Commonly Requested, Often Problematic)

| Feature | Why Requested | Why Problematic | Alternative |
|---------|---------------|-----------------|-------------|
| `mark` event with in-order sequence number tracking across reconnects | "Sequence numbers should be continuous" | Mark names are opaque strings chosen by the WS server — the server already has full context. The sequence number on the echoed mark is advisory; WS consumers key on `mark.name` not `sequenceNumber`. Tracking cross-reconnect continuity adds state with no protocol benefit. | Echo mark with current seqNo at echo time. WS server uses mark.name for correlation. |
| Buffering audio during `clear` for next play | "What if the server sends new audio immediately after clear?" | New `media` events after `clear` are enqueued normally — no special handling needed. The packetQueue is empty after clear; new frames enqueue as usual. | Just drain on clear. New media events self-enqueue. |
| OPTIONS keepalive using in-dialog OPTIONS (within an active call) | "More granular" | In-dialog OPTIONS are a different mechanism (session refresh per RFC 4028). Out-of-dialog OPTIONS for trunk monitoring is the established pattern (Cisco CUBE, Asterisk PJSIP, etc.). | Use out-of-dialog OPTIONS to the registrar URI. |
| OPTIONS-based re-registration without a separate re-register attempt | "One step" | A 200 OK to OPTIONS means the server is reachable — it does NOT confirm the registration binding still exists. A separate REGISTER is required to confirm/restore the binding. | OPTIONS detects server reachability; REGISTER confirms/restores the binding. These are two distinct operations. |

---

## Protocol Semantics: mark and clear

### mark Event

**Direction: bidirectional.** WS server → audio-dock (outbound, received in wsToRTP). Audio-dock → WS server (echo, sent from wsPacer after queue drains).

**WS server sends to audio-dock:**
```json
{
  "event": "mark",
  "streamSid": "MZxxxxxxxxxx",
  "mark": {
    "name": "my-label"
  }
}
```
No `sequenceNumber` required on server-to-audio-dock direction. `mark.name` is an arbitrary string chosen by the WS server.

**Audio-dock echoes back to WS server:**
```json
{
  "event": "mark",
  "sequenceNumber": "7",
  "streamSid": "MZxxxxxxxxxx",
  "mark": {
    "name": "my-label"
  }
}
```
`sequenceNumber` is the current per-session counter at echo time. `mark.name` must match exactly what the server sent.

**Timing contract:** The echo is sent AFTER all `media` frames that were enqueued BEFORE the mark have been drained from `packetQueue`. This is what makes mark useful: the WS server knows the audio before the mark has finished playing at the caller.

**Edge case — empty queue at mark receipt:** If `packetQueue` is empty when the mark arrives, echo it immediately (no audio is pending). Twilio spec: "Twilio sends back a mark event with a matching name when the audio ends (or if there is no audio buffered)."

**Edge case — multiple marks in flight:** Marks queue in order. Each mark must be echoed after all audio preceding it has drained. A simple approach: use a per-session `markQueue chan string` that wsPacer checks after draining each audio frame, echoing the front mark if the queue is non-empty and packetQueue is currently empty.

**Confidence:** HIGH — schema and timing semantics verified against official Twilio docs at https://www.twilio.com/docs/voice/media-streams/websocket-messages

### clear Event

**Direction: WS server → audio-dock only.** Audio-dock does NOT echo a `clear` event back.

**WS server sends to audio-dock:**
```json
{
  "event": "clear",
  "streamSid": "MZxxxxxxxxxx"
}
```

**What audio-dock must do on receiving clear:**
1. Drain all frames from `packetQueue` (the outbound RTP buffer). This stops the RTP pacer from playing the old TTS audio.
2. Echo all pending marks immediately (marks that were waiting for audio to drain). This signals to the WS server which audio segments were cleared.
3. Discard the marks from `markQueue` after echoing them.

**Go implementation pattern for drain:**
```go
case "clear":
    // Drain packetQueue non-destructively by reading until empty
    for {
        select {
        case <-s.packetQueue:
        default:
            goto drained
        }
    }
drained:
    // Echo all pending marks immediately (Twilio spec: clear triggers mark echo)
    for {
        select {
        case name := <-s.markQueue:
            _ = sendMark(wsConn, s.streamSid, name, seqNo)
            seqNo++
        default:
            break
        }
    }
```

**Concurrency concern:** `packetQueue` is written by wsToRTP and read by rtpPacer. The drain above runs in wsToRTP. Draining a buffered channel from a goroutine that isn't the sole consumer is safe in Go — `chan []byte` reads are atomic per operation. The rtpPacer may concurrently pull one frame after the loop exits, which is acceptable (one ~20ms frame of residual audio is inaudible). A mutex around packetQueue is NOT needed.

**Confidence:** HIGH — behavior verified against official Twilio docs. Drain-then-echo-marks pattern derived from spec semantics. Concurrency analysis from Go channel guarantees (HIGH).

---

## Protocol Semantics: SIP OPTIONS keepalive

### Why OPTIONS, Not Re-REGISTER?

SIP REGISTER refreshes the binding but does NOT test whether the server can reach us. OPTIONS is a lightweight request with no side effects that confirms: (a) the registrar is reachable, (b) the registrar is processing SIP requests. A failed OPTIONS with a 200 OK on the previous one indicates a connectivity problem worth reacting to.

OPTIONS does NOT confirm the registration binding still exists. After detecting an OPTIONS failure, a REGISTER is needed to restore the binding.

**Confidence:** HIGH — RFC 3261 §11, IETF draft-jones-sip-options-ping-02

### Standard Pattern (Cisco CUBE / Asterisk PJSIP / FreePBX)

The industry-standard "OPTIONS ping" pattern for SIP trunk monitoring:
1. Send out-of-dialog OPTIONS to the registrar periodically (every 30–60s).
2. Expect a 200 OK response within a timeout (typically 5s).
3. On timeout or 4xx/5xx: log warning, increment failure counter, optionally trigger re-REGISTER.
4. On 200 OK: do nothing (registration presumed alive).
5. After N consecutive failures (e.g., 3): force-trigger immediate re-REGISTER (even if the re-register ticker hasn't fired yet).

**Confidence:** MEDIUM — Cisco CUBE documented; Asterisk PJSIP `qualify_frequency` documented; exact policy (N failures before re-register) varies by implementation. Single-failure re-register is also defensible.

### sipgo API for Out-of-Dialog OPTIONS

`Registrar.client` (type `*sipgo.Client`) already exists on the `Registrar` struct. It is the same client used for REGISTER. Use `client.Do(ctx, req)` with a `context.WithTimeout` for the per-probe timeout.

```go
req := siplib.NewRequest(siplib.OPTIONS, siplib.Uri{Host: r.registrar, Port: 5060})
// sipgo auto-adds: To, From (UA name), Via, CSeq, Call-ID, Max-Forwards
// Manually set From/To to the AoR (same as REGISTER) for consistency
req.AppendHeader(fromH) // sip:user@domain with tag
req.AppendHeader(&siplib.ToHeader{Address: r.aorURI()})

probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
defer cancel()
res, err := r.client.Do(probeCtx, req)
```

A 200 OK means the registrar is reachable. Any error (timeout, no response, transport error) or a 5xx response should be treated as a failure.

**Confidence:** MEDIUM — sipgo `Client.Do()` confirmed from pkg.go.dev. Header auto-population confirmed from sipgo source (client.go). No runnable example with OPTIONS specifically found; pattern derived from REGISTER implementation in registrar.go (same client, same pattern).

### Goroutine Architecture

The OPTIONS keepalive runs as a separate goroutine in the `Registrar`, alongside the existing `reregisterLoop`. It does NOT replace `reregisterLoop`.

```
Registrar
├── reregisterLoop (existing) — re-registers at 75% of Expires interval
└── optionsKeepaliveLoop (new) — probes every 30s; triggers doRegister on failure
```

Both goroutines call `doRegister` for re-registration. `doRegister` is safe for concurrent calls from both goroutines — it does not share mutable state beyond `r.registered` (atomic bool) and `r.metrics` (safe for concurrent writes).

**Race condition to avoid:** If both `reregisterLoop` and `optionsKeepaliveLoop` trigger `doRegister` simultaneously (OPTIONS detects failure at the same time the re-register ticker fires), two simultaneous REGISTER requests go out. This is benign — the server will process both; one will succeed and set `r.registered = true`. Duplicate registration is not harmful in SIP. However, adding a mutex around the `doRegister` call from optionsKeepalive is cleaner.

**Confidence:** HIGH — goroutine architecture based on existing registrar.go patterns. Race analysis from Go memory model knowledge (HIGH).

### Failure Policy

| Scenario | Response |
|----------|----------|
| OPTIONS returns 200 OK | Do nothing. |
| OPTIONS times out (5s) | Log warning, increment `sip_options_failures_total`. |
| OPTIONS returns 4xx or 5xx | Log warning, increment counter. |
| 1st consecutive failure | Log warning only. |
| 2nd consecutive failure | Trigger immediate `doRegister()`. |
| 3rd+ consecutive failure | Continue triggering `doRegister()` each probe interval. |

Using 2 failures before re-register avoids re-registering on a single transient packet loss. Using 1 is also defensible for faster recovery.

Reset consecutive-failure counter on next 200 OK.

**Confidence:** MEDIUM — policy derived from industry patterns. Exact count is implementation choice; 2 is a common convention.

---

## Feature Dependencies

```
[existing: packetQueue chan []byte in CallSession]
    └──drained by──> [clear event handler in wsToRTP]
    └──consumed by──> [rtpPacer goroutine]

[new: markQueue chan string in CallSession]
    └──written to by──> [wsToRTP on receiving mark from WS server]
    └──drained by──> [wsPacer after each packetQueue drain cycle]
    └──immediately drained by──> [wsToRTP on receiving clear from WS server]

[mark echo in wsPacer]
    └──requires──> [wsPacer is sole WS writer invariant — already upheld]
    └──requires──> [seqNo counter already in wsPacer scope]

[clear handler in wsToRTP]
    └──requires──> [access to markQueue — already in CallSession scope]
    └──requires──> [access to wsConn for sendMark echo — wsConn is a parameter]

[SIP OPTIONS keepalive goroutine]
    └──requires──> [existing Registrar.client *sipgo.Client]
    └──requires──> [existing Registrar.aorURI() method]
    └──calls on failure──> [existing Registrar.doRegister()]
    └──runs alongside──> [existing Registrar.reregisterLoop]
    └──started by──> [Registrar.Register() — alongside reregisterLoop goroutine]
```

### Dependency Notes

- **markQueue must be initialized before wsPacer and wsToRTP launch.** It belongs in `CallSession` alongside the existing `packetQueue` and `dtmfQueue`. Initialize in `run()` before the first `handshake()` call. Buffer size: 10 slots (marks are rare; a burst of 10 simultaneous marks is unrealistic but safe).
- **clear echo path**: wsToRTP handles clear AND echoes marks. This breaks the "wsPacer is sole writer" invariant. Resolution: signal wsPacer via a dedicated `clearSignal chan []string` (carrying the names to echo) rather than writing directly from wsToRTP. wsPacer processes the clear signal on its next select iteration, drains the names, and echoes them. This preserves the single-writer invariant.
- **OPTIONS goroutine context**: must share the same `ctx` as `reregisterLoop` so both stop cleanly on SIGTERM. Both are started from `Register()`.

---

## MVP Definition

### Launch With (v2.1)

- [ ] `mark` event: wsToRTP enqueues mark name to `markQueue`; wsPacer echoes after packetQueue drains to empty — both Go and Node.js
- [ ] `clear` event: wsToRTP drains packetQueue; signals wsPacer to echo pending marks — both Go and Node.js
- [ ] SIP OPTIONS keepalive goroutine in Registrar: 30s probe interval, 5s timeout per probe, re-register on 2nd consecutive failure — both Go and Node.js

### Add After Validation (v2.x)

- [ ] `sip_options_failures_total` Prometheus counter — add when ops team needs alerting on registration health
- [ ] Configurable OPTIONS interval via `SIP_OPTIONS_INTERVAL_S` env var — add if 30s proves wrong for some deployment

### Not in Scope

- Sequence number continuity for mark echoes across WS reconnects — mark.name is sufficient for correlation
- In-dialog OPTIONS (session refresh) — different protocol, different use case

---

## Feature Prioritization Matrix

| Feature | User Value | Implementation Cost | Priority |
|---------|------------|---------------------|----------|
| `mark` event — echo after queue drain | HIGH — required for AI barge-in to work correctly | MEDIUM — markQueue + wsPacer check after drain | P1 |
| `clear` event — drain packetQueue + echo pending marks | HIGH — required for AI barge-in to interrupt audio | LOW-MEDIUM — channel drain + signal to wsPacer | P1 |
| SIP OPTIONS keepalive — detect silent registration loss | HIGH — prevents invisible missed-call windows up to 90s | MEDIUM — new goroutine, client.Do, failure counter | P1 |
| `sip_options_failures_total` Prometheus counter | MEDIUM — ops alerting | LOW — one Counter.Inc() call | P2 |
| Configurable OPTIONS interval env var | LOW — 30s is fine for most cases | LOW | P3 |

All three P1 features are required for the v2.1 milestone per PROJECT.md.

---

## Edge Cases

| Edge Case | What Happens Without Handling | Mitigation |
|-----------|-------------------------------|------------|
| WS server sends mark with no preceding media | packetQueue already empty → echo immediately | Check `len(packetQueue) == 0` at mark receipt; echo if empty; otherwise enqueue to markQueue |
| WS server sends clear with empty packetQueue | No frames to drain → echo any pending marks immediately | Drain is a no-op if channel already empty; mark echo path unchanged |
| WS server sends multiple marks before any clear | All must be echoed in order after their corresponding audio drains | markQueue is a FIFO channel; wsPacer pops front after each audio drain |
| clear arrives while wsPacer is mid-frame (20ms tick) | One residual frame plays before the drain completes | Acceptable — one 20ms frame of residual audio is inaudible. Do not add a mutex. |
| OPTIONS keepalive fires during active REGISTER transaction | Two concurrent transactions on same client | client.Do uses a per-transaction call-ID; concurrent transactions are safe in sipgo |
| OPTIONS keepalive detects failure AND reregisterLoop fires simultaneously | Two concurrent doRegister calls | Benign — both succeed; second one overwrites registered=true with same value. Optional: mutex around doRegister in keepalive path |
| OPTIONS returns 401 (auth challenge) from sipgate | Challenge-response cycle needed for OPTIONS? | Most carriers do NOT require auth for OPTIONS to the registrar. If sipgate does: handle 401 with DoDigestAuth same as REGISTER. Log clearly if this occurs. Expected: 200 OK without auth. |
| SIGTERM during OPTIONS probe (blocking on client.Do) | Goroutine blocked for up to 5s | Use `context.WithTimeout(ctx, 5s)` where ctx is already cancelled by SIGTERM — the 5s timeout will be cut short by ctx.Done() |
| mark echo written from wrong goroutine (wsToRTP) | Violates single-writer invariant on wsConn | Use clearSignal channel: wsToRTP sends names to wsPacer for echo. wsPacer is sole writer. |

---

## Twilio Mark/Clear Protocol Reference

### Messages received FROM WS server (handled in wsToRTP)

```json
{
  "event": "mark",
  "streamSid": "MZxxxxxxxxxx",
  "mark": {
    "name": "greeting-end"
  }
}

{
  "event": "clear",
  "streamSid": "MZxxxxxxxxxx"
}
```

### Messages sent TO WS server (sent from wsPacer)

```json
{
  "event": "mark",
  "sequenceNumber": "7",
  "streamSid": "MZxxxxxxxxxx",
  "mark": {
    "name": "greeting-end"
  }
}
```

Note: `clear` is never echoed. Only `mark` events are echoed back.

### New Go structs needed in ws.go

```go
// MarkEvent is sent from audio-dock to WS server when outbound audio playback
// reaches the labeled point (echo of server's mark), or immediately after a clear.
type MarkEvent struct {
    Event          string   `json:"event"`
    SequenceNumber string   `json:"sequenceNumber"`
    StreamSid      string   `json:"streamSid"`
    Mark           MarkBody `json:"mark"`
}

type MarkBody struct {
    Name string `json:"name"` // echoes the name sent by the WS server
}

// InboundMarkMessage is the server→audio-dock mark message parsed from wsToRTP.
type InboundMarkMessage struct {
    Mark struct {
        Name string `json:"name"`
    } `json:"mark"`
}

// InboundClearMessage is the server→audio-dock clear message (no body beyond event+streamSid).
// Parsed in wsToRTP; triggers packetQueue drain + markQueue flush.
```

---

## Previously Shipped Features (v2.0 — Reference Only)

The complete v2.0 feature research is preserved in git history. Summary of what is already implemented:

- SIP REGISTER + digest auth + automatic re-REGISTER at 75% of Expires
- Accept inbound SIP INVITE, full 100/180/200/ACK/BYE/CANCEL lifecycle
- SDP offer parsing + SDP answer (PCMU + telephone-event PT 113)
- RTP socket per call, goroutine read loop, RFC 3550 header parse/build
- RFC 4733 DTMF: End-bit detection, 3-packet dedup by timestamp, PT 113
- gobwas/ws per-call WS connection, single-writer invariant (wsPacer)
- Twilio: connected + start + media + stop + dtmf events
- Inbound WS media → decode base64 → chunk → rtpPacer queue
- WS reconnect with exponential backoff 1s/2s/4s, 30s budget
- Silence injection (pcmuSilenceFrame 0xFF) when packetQueue empty
- CallManager with sync.RWMutex + map[string]*CallSession
- SIGTERM graceful shutdown: DrainAll BYE loop + UNREGISTER, exits within 10s
- GET /health: `{"registered":bool,"activeCalls":N}`
- GET /metrics: 5 Prometheus counters via custom registry
- FROM scratch Docker image, ~1 MB

Node.js v1.0 reference implementation preserved in `node/` directory.

---

## Sources

- Twilio Media Streams WebSocket Messages — mark/clear event schemas (HIGH confidence): https://www.twilio.com/docs/voice/media-streams/websocket-messages
- Twilio bidirectional streaming changelog — mark/clear introduction (MEDIUM confidence): https://www.twilio.com/en-us/changelog/bi-directional-streaming-support-with-media-streams
- RFC 3261 §11 — SIP OPTIONS method purpose and 200 OK response (HIGH confidence): https://www.rfc-editor.org/rfc/rfc3261.html
- IETF draft-jones-sip-options-ping-02 — OPTIONS ping pattern, interval policy, failure handling (MEDIUM confidence — IETF draft, not RFC): https://datatracker.ietf.org/doc/html/draft-jones-sip-options-ping-02
- RFC 6223 — Indication of Support for Keep-Alive (HIGH confidence — RFC, informational): https://www.rfc-editor.org/rfc/rfc6223.html
- sipgo pkg.go.dev — Client.Do() and Client.TransactionRequest() signatures (HIGH confidence): https://pkg.go.dev/github.com/emiago/sipgo
- sipgo client.go — auto-populated headers list (HIGH confidence): https://github.com/emiago/sipgo/blob/main/client.go
- Cisco CUBE — SIP Out-of-Dialog OPTIONS Ping Group (MEDIUM confidence — vendor docs confirming industry pattern): https://www.cisco.com/c/en/us/td/docs/ios-xml/ios/voice/cube/ios-xe/config/ios-xe-book/m_oodo-ping-group.html
- Go channel memory model — concurrent reads/writes to buffered channels (HIGH confidence): https://go.dev/ref/mem

---
*Feature research for: SIP-to-WebSocket audio bridge — v2.1 milestone (mark/clear + OPTIONS keepalive)*
*Researched: 2026-03-05*
