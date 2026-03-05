# Stack Research

**Domain:** SIP/RTP/WebSocket audio bridge — v2.1 milestone additions (mark/clear + OPTIONS keepalive)
**Researched:** 2026-03-05
**Confidence:** HIGH — all conclusions derived from reading the actual v2.0 source, verified Twilio protocol docs, and confirmed sipgo v1.2.0 API via pkg.go.dev.

---

## TL;DR for This Milestone

**Zero new dependencies required.** Both new features (mark/clear protocol handling and SIP OPTIONS keepalive) are implemented entirely with libraries and stdlib already in the project. This milestone is pure logic addition, not dependency addition.

| Feature | Go change | Node.js change |
|---------|-----------|----------------|
| `mark` echo | New JSON structs + markQueue channel in session.go/ws.go | Add `mark` handler in wsClient.ts `onAudio` message loop |
| `clear` flush | Drain `packetQueue` in wsToRTP, send mark confirmations | Drain outbound queue in `makeDrain`, forward pending marks |
| SIP OPTIONS keepalive | Periodic `client.Do(OPTIONS)` goroutine in registrar.go | Periodic `socket.send(OPTIONS)` in userAgent.ts via setInterval |

---

## Existing Stack (unchanged)

The v2.0 stack is locked. These are NOT re-researched — listed here only for context.

### Go (go/go.mod)

| Technology | Version | Purpose |
|------------|---------|---------|
| github.com/emiago/sipgo | v1.2.0 | SIP: REGISTER, INVITE/BYE dialogs, inbound OPTIONS handler |
| github.com/gobwas/ws | v1.3.2 | WebSocket client (outbound to AI backend) |
| github.com/pion/rtp | v1.10.1 | RTP packet marshal/unmarshal |
| github.com/pion/sdp/v3 | v3.0.18 | SDP offer parsing |
| github.com/rs/zerolog | v1.34.0 | Structured JSON logging |
| github.com/prometheus/client_golang | v1.23.2 | Prometheus metrics |
| go-simpler.org/env | v0.12.0 | Env var config parsing |
| golang.org/x/sync | v0.16.0 | errgroup / singleflight |
| encoding/json, net, sync, time | stdlib | JSON events, UDP sockets, goroutines, timers |

### Node.js (node/package.json)

| Technology | Version | Purpose |
|------------|---------|---------|
| ws | ^8.19.0 | WebSocket client to AI backend |
| pino | ^10.3.1 | Structured logging |
| zod | ^4.3.6 | Config validation |
| Node.js dgram (stdlib) | — | Raw UDP SIP socket |
| Node.js crypto, dns (stdlib) | — | Digest auth, DNS resolution |

---

## New Feature: mark/clear Protocol (Go)

### What needs to change

The `wsToRTP` goroutine in `go/internal/bridge/session.go` currently ignores `mark` and `clear` events (line 477: `default: log.Debug`). The `wsPacer` goroutine has no mechanism to echo marks back.

**No new libraries needed.** The changes are:

1. **New JSON structs in `ws.go`** — `InboundMarkEvent` (received from WS server), `OutboundMarkEvent` (sent back to WS server when playout reaches the mark), `ClearEvent` (received from WS server).

2. **New `markQueue chan string` in `CallSession`** — mirrors the existing `dtmfQueue` pattern. `wsToRTP` enqueues mark names; `wsPacer` drains them between audio ticks.

3. **`clear` handling in `wsToRTP`** — drain `packetQueue` (discard buffered audio), then echo any pending marks as `mark` responses. Uses non-blocking channel drain loop (`for { select { case <-s.packetQueue: default: break }}`) — no new synchronization primitives.

### Integration point

```
wsToRTP goroutine:
  case "mark":  → s.markQueue <- markName  (mirrors dtmfQueue pattern)
  case "clear": → drain s.packetQueue + echo pending marks via markQueue or direct write

wsPacer goroutine (select priority):
  case markName := <-s.markQueue: → sendMark(wsConn, streamSid, markName, seqNo)
  case <-ticker.C: → normal audio forwarding
```

The `wsPacer` is the sole WS writer — mark echoes must go through `wsPacer`, not written directly from `wsToRTP`. This preserves the existing write-safety invariant documented in session.go.

### Twilio mark/clear protocol (verified from official docs)

**Inbound mark (WS server → sipgate-sip-stream-bridge):**
```json
{
  "event": "mark",
  "streamSid": "MZxxx",
  "mark": { "name": "custom_identifier" }
}
```

**Outbound mark echo (sipgate-sip-stream-bridge → WS server, when playout reaches mark):**
```json
{
  "event": "mark",
  "sequenceNumber": "N",
  "streamSid": "MZxxx",
  "mark": { "name": "custom_identifier" }
}
```

**Inbound clear (WS server → sipgate-sip-stream-bridge):**
```json
{
  "event": "clear",
  "streamSid": "MZxxx"
}
```
Upon clear: sipgate-sip-stream-bridge flushes `packetQueue`, then sends back `mark` echoes for every undelivered mark that was in the buffer. This allows the WS server to know which marks were cancelled.

---

## New Feature: mark/clear Protocol (Node.js)

### What needs to change

`node/src/ws/wsClient.ts` line 259 already says `// Ignore non-media events (mark, clear, etc.)`. The `onAudio` handler receives all WS messages and currently only processes `"media"`.

**No new npm packages needed.** The existing `ws` library WebSocket instance already receives all frames.

1. **Add `mark` case in the `ws.on('message', ...)` handler** inside `buildClient()`. When a `mark` event arrives, immediately echo it back on the same WS connection with the same `mark.name`. Uses `ws.send(JSON.stringify(...))` — same pattern already used for all other sends.

2. **Add `clear` case** — call `outboundDrain.stop()` to discard buffered audio, then echo any pending marks. Because `makeDrain` is a self-contained closure with a private `queue: Buffer[]`, clearing it is just calling the existing `.stop()` method.

3. **Expose `onMark` and `sendMark` on the `WsClient` interface** — or handle entirely internally (preferred: marks are a protocol detail the bridge handles autonomously, not surfaced to callManager).

### Integration point in wsClient.ts

```typescript
// Inside ws.on('message') in buildClient():
if (msg.event === 'mark' && msg.mark?.name !== undefined) {
  // Echo mark back: playback has reached this point (or audio was cleared)
  ws.send(JSON.stringify({
    event: 'mark',
    streamSid,
    sequenceNumber: String(seq++),
    mark: { name: msg.mark.name },
  }));
}
if (msg.event === 'clear') {
  outboundDrain.stop(); // flush buffered audio
  // Mark echoes for cleared marks should be sent by caller before clear in practice,
  // but any pending marks are now stale — no echo needed per protocol semantics.
}
```

---

## New Feature: SIP OPTIONS Keepalive (Go)

### What needs to change

The current `go/internal/sip/registrar.go` handles inbound OPTIONS in `go/internal/sip/handler.go` (line 168: `onOptions` returns 200 OK). But there is no outbound OPTIONS ping from sipgate-sip-stream-bridge to sipgate. Silent registration loss (sipgate drops the binding without sending a failure response) goes undetected until the next re-REGISTER tick.

**No new libraries needed.** The existing `sipgo.Client` already supports outgoing OPTIONS via `client.Do()`.

Add a `keepaliveLoop(ctx context.Context)` method to `Registrar` that:
1. Runs as a goroutine started from `Register()` alongside `reregisterLoop`.
2. Fires a `time.NewTicker` every `keepaliveInterval` (recommended: 30 seconds — standard SIP OPTIONS ping interval per RFC 3261 and Cisco interop guides).
3. Sends `OPTIONS sip:SIP_REGISTRAR SIP/2.0` with `ClientRequestBuild` auto-filling Via/From/To/CSeq.
4. On timeout (no 200 OK within 5 seconds): marks `registered` as false, increments a new `SIPKeepaliveFailed` counter, triggers immediate re-REGISTER attempt.
5. On 200 OK: no action needed (normal path).

### sipgo API used (confirmed from pkg.go.dev)

```go
// Send OPTIONS — existing client.Do() handles request/response
optReq := siplib.NewRequest(siplib.OPTIONS, siplib.Uri{Host: r.registrar, Port: 5060})
ctx5s, cancel := context.WithTimeout(ctx, 5*time.Second)
res, err := r.client.Do(ctx5s, optReq, sipgo.ClientRequestBuild)
cancel()
// err != nil or res.StatusCode != 200 → registration loss detected
```

`sipgo.ClientRequestBuild` automatically adds Via, From (from UA name), To, CSeq, Call-ID, Max-Forwards. The OPTIONS request does not require a body or Contact header for a keepalive probe.

### Why 30 seconds

- RFC 3261 does not mandate an interval; 30 s is the de facto standard (Cisco CUBE, Asterisk, Kamailio all default to 30 s for out-of-dialog OPTIONS pings).
- SIP_EXPIRES is typically 120 s; a 30 s ping detects loss within one interval. Detecting faster than 30 s offers diminishing returns and increases SIP server load.
- The existing re-register ticker at 75% of Expires (90 s for Expires=120) is too slow for prompt loss detection — the OPTIONS loop fills this gap.

### Integration point

```
registrar.go:

Register() {
    expiry, _ := r.doRegister(ctx)
    go r.reregisterLoop(ctx, expiry)    // existing
    go r.keepaliveLoop(ctx)             // NEW
}

keepaliveLoop(ctx):
    ticker := time.NewTicker(30 * time.Second)
    for {
        select {
        case <-ctx.Done(): return
        case <-ticker.C:
            send OPTIONS → if fail: registered.Store(false), trigger doRegister
        }
    }
```

---

## New Feature: SIP OPTIONS Keepalive (Node.js)

### What needs to change

`node/src/sip/userAgent.ts` already handles inbound OPTIONS from sipgate (lines 236-254). For outbound OPTIONS (sipgate-sip-stream-bridge probing sipgate), the same raw `dgram.Socket` already present in the module is used.

**No new npm packages needed.** Node.js `dgram` (stdlib) and the existing SIP message builder pattern (used by `buildRegister`) is sufficient.

Add `sendOptions()` function using the same raw SIP message builder pattern, and start a `setInterval` in the `createSipUserAgent` promise resolver after first registration.

```typescript
function buildOptions(p: { user: string; domain: string; registrar: string;
  localIp: string; localPort: number; branch: string; cseq: number }): string {
  return [
    `OPTIONS sip:${p.registrar} SIP/2.0`,
    `Via: SIP/2.0/UDP ${p.localIp}:${p.localPort};branch=${p.branch};rport`,
    'Max-Forwards: 70',
    `From: <sip:${p.user}@${p.domain}>;tag=${randomHex(6)}`,
    `To: <sip:${p.registrar}>`,
    `Call-ID: ${randomHex(10)}@sipgate-sip-stream-bridge`,
    `CSeq: ${p.cseq} OPTIONS`,
    'Content-Length: 0',
    '',
  ].join('\r\n') + '\r\n';
}
```

The `socket.on('message')` handler already routes inbound `SIP/2.0` responses. Add a check for 200 OK responses to OPTIONS requests (match by CSeq method `OPTIONS`) to detect keepalive success/failure.

---

## No New Dependencies Required

### Go — nothing to add to go.mod

All needed capabilities are already in the module:
- JSON struct definitions: `encoding/json` (stdlib)
- Channel-based mark queue: `sync` + built-in Go channels
- OPTIONS request: `sipgo.Client.Do()` + `siplib.NewRequest(siplib.OPTIONS, ...)`
- Periodic timer: `time.NewTicker` (stdlib)

### Node.js — nothing to add to package.json

- WebSocket send/receive for mark/clear: `ws` library already used
- JSON encoding: `JSON.stringify/parse` (stdlib)
- OPTIONS ping: `dgram.Socket.send()` already used in userAgent.ts
- Periodic timer: `setInterval` (Node.js stdlib)

---

## Alternatives Considered

| Recommended | Alternative | Why Not |
|-------------|-------------|---------|
| Channel-based markQueue (mirrors dtmfQueue) | Shared map[string]struct{} with mutex | Channels are the idiomatic Go concurrency primitive here; map adds lock contention for no benefit since marks are ordered |
| Drain packetQueue with non-blocking loop on clear | Reset/close and recreate channel | Recreating a buffered channel races with concurrent writers (rtpPacer reads from it); drain loop is safe without additional locking |
| sipgo Client.Do() for OPTIONS | sipgo Client.WriteRequest() (fire-and-forget) | WriteRequest skips transaction tracking; we need the response to detect registration loss; Do() with 5 s timeout is correct |
| 30 s OPTIONS interval | Match re-register interval (90 s) | 90 s is too slow for prompt detection; standard keepalive practice is 30 s |
| Inline OPTIONS in registrar.go | New dedicated keepalive.go | Registrar already owns registration state; keepalive is a registration concern; no new file needed |
| Autonomous mark echo (no WsClient interface change) | Surface mark events to callManager | Marks are a WS protocol detail; callManager should not know about stream-level bookmarks |

---

## What NOT to Use

| Avoid | Why | Use Instead |
|-------|-----|-------------|
| Any new Go SIP library for OPTIONS | sipgo v1.2.0 already sends any SIP method via `client.Do()`; adding a second SIP library creates transport conflicts | sipgo Client.Do() with sip.OPTIONS |
| SIP.js in Node.js for outbound OPTIONS | The v1 Node.js implementation deliberately avoids SIP.js in favor of raw dgram to eliminate the C++ sidecar dependency; SIP.js 0.21.x is the v1.0 stack but was replaced with hand-rolled SIP in the preserved node/ implementation | dgram.Socket with buildOptions() |
| bufio or additional buffering for mark queue | The markQueue receives at most one mark per audio segment; a 10-element channel (same as dtmfQueue) is more than sufficient | make(chan string, 10) |
| time.Sleep for OPTIONS interval | Leaks goroutine if context is cancelled during Sleep; does not respect ctx.Done() | time.NewTicker with select ctx.Done() |

---

## Version Compatibility (unchanged)

| Package | Version | Notes |
|---------|---------|-------|
| github.com/emiago/sipgo | v1.2.0 | Confirmed latest as of Feb 7, 2025; Do() + ClientRequestBuild available since v1.0.0 |
| github.com/gobwas/ws | v1.3.2 | No changes needed; ws.go writeJSON/readWSMessage already handles mark/clear frames |
| encoding/json | stdlib | NewDecoder/Unmarshal for new InboundMarkEvent and ClearEvent structs |
| ws (npm) | ^8.19.0 | ws.send() for mark echo already used; no API changes needed |
| dgram (Node.js stdlib) | — | socket.send() already used in userAgent.ts for REGISTER; identical for OPTIONS |

---

## Sources

- [Twilio Media Streams WebSocket Messages](https://www.twilio.com/docs/voice/media-streams/websocket-messages) — mark and clear JSON schemas verified (HIGH confidence)
- [emiago/sipgo releases](https://github.com/emiago/sipgo/releases) — v1.2.0 confirmed latest, released Feb 7, 2025 (HIGH confidence)
- [pkg.go.dev/github.com/emiago/sipgo](https://pkg.go.dev/github.com/emiago/sipgo) — Client.Do(), ClientRequestBuild signatures verified (HIGH confidence)
- [github.com/emiago/sipgo/blob/main/client.go](https://github.com/emiago/sipgo/blob/main/client.go) — ClientRequestOption type, Do/TransactionRequest/WriteRequest confirmed (HIGH confidence)
- v2.0 source: `go/internal/bridge/session.go` — dtmfQueue/markQueue channel pattern, packetQueue drain model (HIGH confidence — read directly)
- v2.0 source: `go/internal/sip/registrar.go` — reregisterLoop pattern, client.Do() usage, context propagation (HIGH confidence — read directly)
- v2.0 source: `go/internal/sip/handler.go` — inbound OPTIONS handler (onOptions) confirms sipgo Server.OnOptions() works (HIGH confidence — read directly)
- v1.0 source: `node/src/sip/userAgent.ts` — dgram socket, OPTIONS response handler already present at lines 236-254 (HIGH confidence — read directly)
- v1.0 source: `node/src/ws/wsClient.ts` — mark/clear already commented as ignored at line 259; WsClient interface and makeDrain closure (HIGH confidence — read directly)
- [RFC 3261 §11 — OPTIONS method](https://www.rfc-editor.org/rfc/rfc3261#section-11) — OPTIONS as capability query and keepalive (HIGH confidence)
- [Cisco CUBE SIP Out-of-Dialog OPTIONS Ping](https://www.cisco.com/c/en/us/td/docs/ios-xml/ios/voice/cube/configuration/cube-book/cube-book_chapter_01000111.pdf) — 30 s as standard keepalive interval (MEDIUM confidence — Cisco interop guide, not sipgate-specific)

---

*Stack research for: sipgate-sip-stream-bridge v2.1 — mark/clear + SIP OPTIONS keepalive*
*Researched: 2026-03-05*
