# Pitfalls Research

**Domain:** Adding mark/clear Twilio Media Streams events + SIP OPTIONS keepalive to existing Go (sipgo) + Node.js (SIP.js) SIP↔WebSocket bridge
**Researched:** 2026-03-05
**Confidence:** HIGH for protocol pitfalls (verified against Twilio official docs + RFC 3261); MEDIUM for sipgo-specific OPTIONS send path (library docs are sparse; patterns inferred from sipgo client.go source + analogous REGISTER implementation already in codebase)

---

## Critical Pitfalls

### Pitfall 1: mark Echo Arrives on a Different WS Write Path — Violates Sole-Writer Invariant

**What goes wrong:**
The `mark` echo (incoming from the WS server) arrives in `wsToRTP` as a received event. The temptation is to immediately respond to the mark echo from `wsToRTP` — for example, logging it, storing it, or sending a confirmation back. But in the current architecture, `wsPacer` is the **sole goroutine allowed to write to `wsConn`** (see `session.go` WRITE-SAFETY INVARIANTS comment). Any write from `wsToRTP` or from a new goroutine breaks this invariant and causes `gobwas/ws` write corruption or a concurrent-write panic.

**Why it happens:**
The mark echo is a read-side event (arrives from WS server → `wsToRTP`). The clear event is a write-side action (must be sent to WS server → `wsPacer`). Developers wire both together and write back to the conn from the read goroutine — a common WebSocket concurrency mistake.

**How to avoid:**
The mark echo is inbound-only — sipgate-sip-stream-bridge reads it and stores state (e.g., the pending mark name), but does NOT write anything back in response. Mark echoes flow: WS server → `wsToRTP` → stores pending mark name in a channel or sync.Map → `wsPacer` may read that channel if it needs to track playout position.

For outbound mark events (sipgate-sip-stream-bridge echoing a mark *to* the WS server after outbound audio playout), the echo must be sent **exclusively from `wsPacer`**, just like `sendDTMF`. Use the same `dtmfQueue` pattern: add a `markEchoQueue chan string` and drain it in `wsPacer`'s select.

```go
// In wsPacer select — add alongside existing dtmfQueue case:
case markName := <-s.markEchoQueue:
    if err := sendMark(wsConn, s.streamSid, markName, seqNo); err != nil {
        sig.Signal()
        return
    }
    seqNo++
```

**Warning signs:**
- Data race detector (`go test -race`) reports concurrent writes to `wsConn`.
- `gobwas/ws` returns a write error mid-call even with no network disruption.
- Mark echo appears in logs but crashes follow shortly after.

**Phase to address:** Phase implementing mark/clear in Go — establish the outbound mark echo queue in `wsPacer` before any mark logic in `wsToRTP`.

---

### Pitfall 2: clear Flushes the packetQueue but Does Not Reset the RTP Pacer Timestamp — Audio Glitch After Clear

**What goes wrong:**
The clear event from the WS server instructs sipgate-sip-stream-bridge to discard all buffered outbound audio (drain `packetQueue`). If `packetQueue` is drained but `rtpPacer` continues sending silence frames at the same RTP timestamp progression, the call is fine. However, if the implementation stops the pacer during clear and then restarts it (common mistake when trying to "pause" audio), the RTP timestamp and sequence number jump. The caller's jitter buffer detects a sequence discontinuity, flushes its buffer, and the caller hears a ~200ms click or pop.

**Why it happens:**
Developers naturally think "pause playback = stop the RTP stream". But the RTP pacer must send at exactly 20ms/frame continuously — the silence frames between real audio are what keep the NAT traversal hole open and what keep the jitter buffer in a steady state. Stopping and restarting the pacer introduces a gap that jitter buffers detect as packet loss.

**How to avoid:**
Clear means **drain the queue only** — do not stop or interrupt `rtpPacer`. The pacer always runs at 20ms; it falls back to silence naturally when `packetQueue` is empty. The correct implementation:

```go
// In wsToRTP, on "clear" event:
case "clear":
    // Drain packetQueue non-blockingly — do not stop rtpPacer.
    for {
        select {
        case <-s.packetQueue:
            // discard one frame
        default:
            goto drained
        }
    }
drained:
    s.log.Info().Str("call_id", s.callID).Msg("wsToRTP: clear — outbound audio queue flushed")
    // If any pending marks should be echoed back, enqueue them now.
```

The RTP pacer sees an empty queue and sends silence — no timestamp reset, no sequence gap, no jitter buffer disruption.

**Warning signs:**
- Caller hears a click or pop immediately after TTS is interrupted via barge-in.
- Wireshark shows RTP sequence number or timestamp discontinuity aligned with the clear event.
- RTP timestamp resets to zero after clear (definitive sign that the pacer was restarted).

**Phase to address:** Phase implementing mark/clear — write the drain helper before wiring up the clear event handler; verify in tests that rtpPacer sequence numbers are monotonic through a clear+new-audio cycle.

---

### Pitfall 3: Outbound mark Message Schema — Missing streamSid Causes Silent Failure

**What goes wrong:**
The outbound `mark` message sent **from sipgate-sip-stream-bridge to the WS server** (echoing that outbound audio playout has reached a mark point) requires a `streamSid` field. If `streamSid` is omitted, Twilio-compatible WS consumers silently ignore the mark event — no error is returned, but the consumer never fires the callback that was waiting for the mark. Features built on mark-based synchronization (barge-in, playback confirmation) break without any error signal.

**Why it happens:**
The Twilio docs show two different mark schemas:
- **Server → Twilio** (outgoing): requires `streamSid`
- **Twilio → Server** (incoming echo): includes `sequenceNumber`

Developers add `sequenceNumber` (which they see on incoming marks) to outgoing marks, but forget `streamSid` (which is on all other outgoing events like `media`). The JSON is valid; the consumer just doesn't match it.

**How to avoid:**
Outgoing mark schema (sipgate-sip-stream-bridge → WS server):
```json
{
  "event": "mark",
  "streamSid": "MZ...",
  "mark": {
    "name": "my-label"
  }
}
```
No `sequenceNumber` needed on outgoing marks. Always include `streamSid`. Add a unit test asserting the marshalled JSON contains `streamSid`.

**Warning signs:**
- WS consumer never fires the mark callback even though audio completes.
- Debug log shows mark sent but consumer-side acknowledge never arrives.
- JSON schema mismatch is invisible without consumer-side logging of received mark names.

**Phase to address:** Phase implementing mark — add a test for `sendMark()` JSON output before integration testing.

---

### Pitfall 4: clear Event Must Also Echo All Pending mark Names Back to the WS Server

**What goes wrong:**
Per the Twilio Media Streams spec: "If your server sends a clear message, Twilio empties the audio buffer and sends back mark messages matching any remaining mark messages." In the sipgate-sip-stream-bridge inversion, sipgate-sip-stream-bridge is the bridge — when it receives a `clear` from the WS server and flushes `packetQueue`, it must also send back mark echoes for any marks that were queued in `packetQueue` but never reached playout. If these pending marks are not echoed, the WS consumer's barge-in state machine gets stuck waiting for marks that will never arrive.

**Why it happens:**
Developers implement clear as a simple queue drain without tracking which marks were interspersed with audio frames. If marks are queued separately (e.g., on a mark channel alongside packetQueue), they must also be drained and echoed.

**How to avoid:**
Keep a `pendingMarkQueue chan string` in the session that is populated when the WS server sends a `mark` event (before the audio following it has played). On `clear`:
1. Drain `packetQueue` (audio frames).
2. Drain `pendingMarkQueue` and for each mark name, enqueue it onto `markEchoQueue` so `wsPacer` sends the echo.

This preserves the sole-writer invariant: mark echoes are sent from `wsPacer`, not from `wsToRTP`.

**Warning signs:**
- After a barge-in (clear), the WS consumer hangs waiting for a mark acknowledgement that never arrives.
- Consumer-side timeout fires on mark wait after every barge-in.
- No mark echo appears in WS debug logs after a clear.

**Phase to address:** Phase implementing mark/clear — design the mark lifecycle (enqueue on receive, echo on playout or on clear) before coding either feature.

---

### Pitfall 5: SIP OPTIONS Keepalive Timer Leaks If Not Tied to the Root Context

**What goes wrong:**
A `time.NewTicker` or `time.AfterFunc` goroutine for OPTIONS keepalive that is started in `main()` or `Register()` but not cancelled with the application's root context leaks after the application shuts down. In tests, this means the goroutine is still running after the test exits. In production with multiple restarts (e.g., Kubernetes pod restarts), leftover timers from the previous instance do not exist because Go processes exit — but if OPTIONS keepalive is started per-test or per-connection, the leak accumulates within a test run.

**Why it happens:**
It is tempting to start the OPTIONS keepalive goroutine inside `Register()` (alongside `reregisterLoop`) for co-location. The `reregisterLoop` already has this pattern correct — it uses `ctx context.Context` and exits on `<-ctx.Done()`. Developers copy the goroutine skeleton but forget to wire the `ctx` parameter, writing `context.Background()` instead.

**How to avoid:**
The OPTIONS keepalive goroutine must accept the same root `ctx` that `reregisterLoop` uses:

```go
func (r *Registrar) Register(ctx context.Context) error {
    expiry, err := r.doRegister(ctx)
    if err != nil {
        return err
    }
    go r.reregisterLoop(ctx, expiry)
    go r.optionsKeepaliveLoop(ctx)  // same ctx — stops when app shuts down
    return nil
}

func (r *Registrar) optionsKeepaliveLoop(ctx context.Context) {
    ticker := time.NewTicker(30 * time.Second)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            r.sendOptions(ctx)
        }
    }
}
```

`ticker.Stop()` is mandatory — without it, the ticker goroutine leaks even if the outer goroutine exits.

**Warning signs:**
- `go test -count=1 -timeout=60s` takes longer than expected because OPTIONS goroutines keep running after the test's registrar is torn down.
- `runtime/pprof` goroutine dump shows `optionsKeepaliveLoop` goroutines after shutdown.
- Goroutine count grows by 1 per `Register()` call in tests that create multiple registrars.

**Phase to address:** Phase implementing SIP OPTIONS keepalive — pass `ctx` to the goroutine from day one; add a goroutine-count assertion to the registrar test.

---

### Pitfall 6: SIP OPTIONS Response Codes — 401/407 Does Not Mean Registration Is Lost; 408/5xx Does

**What goes wrong:**
The OPTIONS keepalive goroutine checks the response code to determine whether the registration is still valid. Some implementations treat **any non-200** response as a registration failure and trigger an immediate re-registration. This causes a re-registration storm if sipgate responds with `401 Unauthorized` to the OPTIONS probe (requiring digest auth), because the keepalive is not retrying with auth — it just triggers re-registration, which in turn gets a 401, which triggers another re-registration.

**Why it happens:**
RFC 3261 does not require OPTIONS probes to be authenticated for the purpose of keepalive — many SIP servers will respond 200 OK without auth. But some SIP providers (sipgate included based on general trunking behavior) may return `401` or `200` depending on configuration. Developers assume 401 = registration gone when it actually means "you need to authenticate this probe."

For registration-loss detection specifically:
- `408 Request Timeout` — no response at all; network path or registrar gone. Treat as failure.
- `5xx` (especially `503 Service Unavailable`) — registrar temporarily down. Treat as failure.
- `200 OK` — all good.
- `401/407` — auth required for OPTIONS probe; NOT a registration failure. Either add auth handling or ignore 401 as "server is alive but wants auth" (i.e., treat as "registered").
- `404` — user not found; this CAN indicate registration loss.

**How to avoid:**
```go
func (r *Registrar) sendOptions(ctx context.Context) {
    req := siplib.NewRequest(siplib.OPTIONS, siplib.Uri{Host: r.registrar, Port: 5060})
    // ... set From/To/Contact headers ...

    res, err := r.client.Do(ctx, req)
    if err != nil {
        // Network timeout or transaction failure — likely registration lost
        r.log.Warn().Err(err).Msg("OPTIONS keepalive: no response — triggering re-registration")
        r.triggerReregister(ctx)
        return
    }

    switch {
    case res.StatusCode == 200:
        r.log.Debug().Msg("OPTIONS keepalive: 200 OK — registration confirmed")
    case res.StatusCode == 401 || res.StatusCode == 407:
        // Auth required for probe — server is alive, registration not necessarily lost
        r.log.Debug().Int("status", res.StatusCode).Msg("OPTIONS keepalive: auth required — treating as alive")
    case res.StatusCode == 404:
        r.log.Warn().Msg("OPTIONS keepalive: 404 Not Found — re-registering")
        r.triggerReregister(ctx)
    case res.StatusCode >= 500:
        r.log.Warn().Int("status", res.StatusCode).Msg("OPTIONS keepalive: server error — triggering re-registration")
        r.triggerReregister(ctx)
    }
}
```

**Warning signs:**
- Log shows OPTIONS triggered re-registration but re-registration also fails with 401 (auth loop).
- Registration counter in `/metrics` oscillates rapidly (register → unregister → register cycle).
- Re-registration rate exceeds 1 per minute with no actual network outage.

**Phase to address:** Phase implementing SIP OPTIONS keepalive — define the failure/non-failure response code table in the spec before implementation.

---

### Pitfall 7: OPTIONS Keepalive Interval Too Aggressive — Sipgate Rate-Limits or Blacklists

**What goes wrong:**
An OPTIONS keepalive interval of < 10 seconds generates 6+ requests per minute per registered UA. At scale (multiple sipgate-sip-stream-bridge instances), or during testing where OPTIONS intervals are set to 1–2 seconds for faster feedback, sipgate may rate-limit or block the source IP. The registration itself may then fail because REGISTER traffic from the same IP is also rate-limited.

**Why it happens:**
Developers set the keepalive interval low during development to get fast feedback on the detection logic. They forget to increase it before deploying. The default in production VOIP systems is 30–60 seconds.

**How to avoid:**
- Default interval: 30 seconds. Make it configurable via env var (`SIP_OPTIONS_INTERVAL_S`), defaulting to 30.
- Do NOT use an interval below 10 seconds except in controlled test environments.
- During tests, use a mock response from a local test server rather than live sipgate traffic.

**Warning signs:**
- `REGISTER` starts failing with `503` or connection refused after OPTIONS traffic increases.
- sipgate portal shows registration drops correlated with high OPTIONS volume.
- Options responses are received but REGISTER renewals start timing out.

**Phase to address:** Phase implementing SIP OPTIONS keepalive — define the configurable interval env var and set the test harness to use a local mock.

---

### Pitfall 8: OPTIONS Keepalive Sends to Wrong URI — Must Target the Registrar, Not the AoR

**What goes wrong:**
The OPTIONS probe is an out-of-dialog request to detect whether the SIP trunk is reachable. It should be sent to the **registrar URI** (`sip.sipgate.de:5060`), not to the AoR (`sip:user@sipgate.de`). If the Request-URI is the AoR, the OPTIONS is treated as a call probe (does user X exist?) rather than a trunk health probe. The response behavior differs: an AoR probe may return 200 if the AoR is registered elsewhere, giving a false positive even if the trunk path is broken.

**Why it happens:**
Developers copy the `aorURI()` helper (already used in REGISTER requests) for the OPTIONS probe and send to `sip:user@sipgate.de`. The probe appears to work (200 OK) but is probing the wrong endpoint.

**How to avoid:**
OPTIONS target = registrar IP, not AoR:
```go
func (r *Registrar) sendOptions(ctx context.Context) {
    // Target the registrar, same as REGISTER Request-URI
    registrarURI := siplib.Uri{Host: r.registrar, Port: 5060}
    req := siplib.NewRequest(siplib.OPTIONS, registrarURI)

    // From/To should still be the AoR for identification
    aor := r.aorURI()
    fromH := &siplib.FromHeader{Address: aor, Params: siplib.NewParams()}
    fromH.Params.Add("tag", siplib.GenerateTagN(16))
    req.AppendHeader(fromH)
    req.AppendHeader(&siplib.ToHeader{Address: aor})
    // ...
}
```

**Warning signs:**
- OPTIONS returns 200 even after the sipgate trunk is intentionally taken offline.
- OPTIONS is being routed through the public PSTN (call to the AoR number) instead of the SIP control plane.
- OPTIONS target IP differs from the REGISTER target IP in packet captures.

**Phase to address:** Phase implementing SIP OPTIONS keepalive — unit test for the generated REQUEST-URI.

---

### Pitfall 9: Concurrent doRegister Calls — OPTIONS-Triggered Re-Registration Races the Periodic reregisterLoop

**What goes wrong:**
The OPTIONS keepalive goroutine detects registration loss and calls `doRegister()` directly. The `reregisterLoop` is also ticking and may call `doRegister()` at the same time. Two concurrent REGISTER transactions are sent to sipgate with different CSeq values. One wins, one gets a stale-CSeq 400 rejection. The loser leaves `r.registered` in an inconsistent state (one goroutine sets it `true`, the other sets it `false`).

**Why it happens:**
`doRegister()` is a free function today — it has no lock around the client.Do() call. The `reregisterLoop` calls it on a ticker; the OPTIONS goroutine calls it on a failure event. Nothing prevents concurrent calls.

**How to avoid:**
Add a `sync.Mutex` on the `Registrar` struct that `doRegister()` holds for the duration of the REGISTER transaction:

```go
type Registrar struct {
    // ...existing fields...
    mu sync.Mutex // serializes concurrent doRegister calls
}

func (r *Registrar) doRegister(ctx context.Context) (time.Duration, error) {
    r.mu.Lock()
    defer r.mu.Unlock()
    // ... existing doRegister body ...
}
```

Alternatively, use a `chan struct{}` with size 1 as a trylock so the OPTIONS goroutine can skip re-registration if one is already in progress rather than blocking.

**Warning signs:**
- Log shows two `REGISTER` requests in flight simultaneously (same Call-ID prefix, different CSeq).
- `r.registered` flips true→false→true in rapid succession without any actual network change.
- sipgate returns `400 Bad Request` on a REGISTER with a note about CSeq ordering.

**Phase to address:** Phase implementing SIP OPTIONS keepalive — add the mutex to `Registrar` before wiring `optionsKeepaliveLoop` to `doRegister`.

---

### Pitfall 10: Node.js wsClient.ts Ignores mark/clear Events — Needs Separate Callbacks, Not Inline Event Handling

**What goes wrong:**
The Node.js `wsClient.ts` currently handles inbound events via a single `ws.on('message', ...)` listener inside `onAudio()`, with a comment: `// Ignore non-media events (mark, clear, etc.)`. Adding mark/clear handling inline to this listener creates a structural problem: `onAudio()` is called once to set up the audio handler, but mark and clear have their own callbacks that the `callManager` needs to register separately. If mark/clear are handled inside the `onAudio` listener, the callManager has no way to receive mark echoes or inject the flush logic from the outside.

**Why it happens:**
The current design tunnels all WS events through `onAudio` for simplicity. This was fine for media-only use. Mark and clear are control-plane events that the call manager layer — not the audio layer — needs to act on (e.g., mark arrival → notify that TTS chunk has finished; clear → drain the outbound drain queue). Extending the existing single-listener approach makes the WsClient interface "leaky" (exposes protocol internals to callManager).

**How to avoid:**
Extend the `WsClient` interface with explicit callbacks for mark and clear:
```typescript
export interface WsClient {
    // ... existing methods ...
    /** Register handler called when WS server sends a mark echo (outbound audio reached the mark point) */
    onMark(handler: (name: string) => void): void;
    /** Send a clear message to the WS server (flush outbound buffer) */
    sendClear(): void;
    /** Send a mark message to the WS server (set a named position in the outbound stream) */
    sendMark(name: string): void;
}
```

Inside `createWsClient`, handle `mark` and `clear` in the same `ws.on('message')` listener, but dispatch to the separately registered `markHandler` and `clearHandler` callbacks. The outbound `sendClear()` and `sendMark()` calls are safe to call from any context because `ws.send()` in Node.js is inherently serial (unlike Go's gobwas/ws).

**Warning signs:**
- `callManager.ts` imports protocol constants from `wsClient.ts` directly (encapsulation violation).
- Test for mark/clear requires instantiating the entire WS protocol inside the test (no seam for injection).
- Adding mark handling introduces a `// HACK` comment inside `onAudio`.

**Phase to address:** Phase implementing mark/clear for Node.js — extend `WsClient` interface before implementing mark/clear logic in callManager.

---

## Technical Debt Patterns

| Shortcut | Immediate Benefit | Long-term Cost | When Acceptable |
|----------|-------------------|----------------|-----------------|
| Hardcode OPTIONS interval at 30s | No new env var to document | Cannot tune for different trunk providers; tests must wait 30s for detection | Never — add `SIP_OPTIONS_INTERVAL_S` env var with `30` default |
| Drain `packetQueue` with a goroutine that closes a done channel | Simpler code than channel-select drain | Race between drain goroutine and `rtpPacer` draining simultaneously; duplicate packets possible | Never — drain synchronously with a non-blocking select loop |
| Store pending mark names in a `[]string` slice (not a channel) | Simple append | Concurrent access from `wsToRTP` (writer) and `wsPacer` (reader) requires mutex; easy to miss | Acceptable only with an explicit `sync.Mutex`; a `chan string` is cleaner |
| Trigger re-registration on 401 to OPTIONS | Zero extra code for auth-aware OPTIONS handling | Re-registration storm when sipgate requires auth on OPTIONS probe | Never — 401 on OPTIONS does not mean registration is lost |
| Use `context.Background()` in OPTIONS keepalive goroutine | Simple, self-contained | Goroutine does not stop on shutdown; timer leak in tests | Never — always use the same root `ctx` as `reregisterLoop` |

---

## Integration Gotchas

| Integration | Common Mistake | Correct Approach |
|-------------|----------------|------------------|
| Twilio-compatible WS consumer (mark) | Outgoing mark message omits `streamSid` | Always include `streamSid` in outgoing mark; schema: `{event, streamSid, mark: {name}}` |
| Twilio-compatible WS consumer (clear) | Clear flushes queue but does not echo pending marks | After flush, echo all pending mark names back through `markEchoQueue → wsPacer` |
| sipgate SIP trunk (OPTIONS) | Sending OPTIONS to AoR URI instead of registrar URI | Request-URI = `sip:sip.sipgate.de:5060`; same target as REGISTER |
| sipgate SIP trunk (OPTIONS auth) | Treating 401 response to OPTIONS as registration loss | 401 on OPTIONS = server alive, auth required for probe; not a registration failure |
| sipgo `client.Do()` (OPTIONS) | No timeout context on OPTIONS transaction | Wrap with `context.WithTimeout(ctx, 5*time.Second)` per probe; leak otherwise when registrar is unreachable |
| gobwas/ws write safety (mark echo) | Sending mark echo from `wsToRTP` goroutine | All writes go through `wsPacer` only; use `markEchoQueue` channel for cross-goroutine delivery |
| Node.js `ws.send()` (mark/clear) | Sending mark/clear directly from `callManager` using the raw `ws` object | Expose `sendMark`/`sendClear` on the `WsClient` interface; keep the raw socket inside the factory closure |

---

## Performance Traps

| Trap | Symptoms | Prevention | When It Breaks |
|------|----------|------------|----------------|
| Drain `packetQueue` with a blocking `for range` in `wsToRTP` | `wsToRTP` blocks on empty queue after clear; new WS messages (including next `media`) are not read | Non-blocking select drain: `for { select { case <-s.packetQueue: default: goto done } }` | Immediately on first clear with no queued audio |
| `time.NewTicker` inside `sendOptions()` instead of in `optionsKeepaliveLoop` | A new ticker is created per OPTIONS send call (misplaced logic) | Ticker belongs in the loop, not in the send function | First time OPTIONS send is called more than once |
| Allocating a new `sip.Request` for OPTIONS on every tick | ~5 allocations per tick × 1 tick/30s = negligible | Pre-build the request once and reuse; sipgo CSeq auto-increments; no need to rebuild | Not a real performance trap at 1 tick/30s; but important for test clarity |
| Sending OPTIONS from every `CallSession` goroutine (per-call keepalive) | N calls × 1 OPTIONS/30s floods the registrar | OPTIONS keepalive is a service-level concern, not a per-call concern; one goroutine in `Registrar` | At 10+ concurrent calls |

---

## "Looks Done But Isn't" Checklist

- [ ] **mark outgoing schema:** `sendMark()` JSON output contains `streamSid` field — verify with a unit test on the marshalled bytes.
- [ ] **mark sole-writer invariant:** All mark echoes go through `wsPacer` via `markEchoQueue`; `wsToRTP` never calls `writeJSON` directly — verify with data race detector (`go test -race`).
- [ ] **clear does not reset RTP:** After a clear event, send a new media blob; verify in unit test that `rtpPacer` sequence numbers are monotonically increasing (no reset to zero or discontinuity).
- [ ] **clear echoes pending marks:** Send mark → queue audio → send clear before audio plays; verify the WS server receives a mark echo even though audio never played.
- [ ] **OPTIONS targets registrar URI:** Log the `Request-URI` of the first OPTIONS probe; confirm it matches `sip.sipgate.de:5060`, not `sip:user@sipgate.de`.
- [ ] **OPTIONS goroutine stops on shutdown:** Trigger SIGTERM; verify the OPTIONS ticker goroutine exits before the process exits (goroutine-count assertion or test with short context timeout).
- [ ] **concurrent doRegister safety:** Add `go test -race` test that calls `doRegister` from two goroutines simultaneously; verify no data race on `r.registered`.
- [ ] **401 on OPTIONS does not re-register:** Mock OPTIONS response as 401; verify `r.registered` stays `true` and no REGISTER is sent.
- [ ] **Node.js WsClient interface extended:** `WsClient` in `wsClient.ts` exports `onMark`, `sendMark`, `sendClear`; `callManager.ts` uses these — not the raw `ws` object.
- [ ] **OPTIONS interval configurable:** `SIP_OPTIONS_INTERVAL_S` env var accepted; defaults to 30; validated as integer ≥ 10 in config schema.

---

## Recovery Strategies

| Pitfall | Recovery Cost | Recovery Steps |
|---------|---------------|----------------|
| mark echo sent from wrong goroutine (concurrent write) | MEDIUM | Introduce `markEchoQueue chan string` in `CallSession`; move all mark sends to `wsPacer` select; re-run race detector |
| clear resets RTP pacer timestamp | LOW | Remove pacer stop/restart from clear handler; replace with queue drain only; verify with monotonic sequence test |
| OPTIONS triggers re-registration on 401 | LOW | Add response code switch to `sendOptions`; treat 401 as "alive"; redeploy |
| OPTIONS keepalive goroutine leak in tests | LOW | Pass root `ctx` to goroutine; add `defer ticker.Stop()` |
| Concurrent doRegister race | MEDIUM | Add `sync.Mutex` to `Registrar.doRegister`; add race detector test |
| Node.js mark/clear inline in onAudio | MEDIUM | Refactor `WsClient` interface to add `onMark`/`sendMark`/`sendClear`; update all consumers |

---

## Pitfall-to-Phase Mapping

| Pitfall | Prevention Phase | Verification |
|---------|------------------|--------------|
| mark echo sent from wrong goroutine | Phase: mark/clear Go — add `markEchoQueue` before mark logic | `go test -race` passes; no concurrent write panics in 2+ concurrent call test |
| clear resets RTP timestamp | Phase: mark/clear Go — drain-only flush from day one | Unit test: `rtpPacer` sequence monotonic through clear+media cycle |
| Outgoing mark schema missing streamSid | Phase: mark/clear Go/Node.js — add `sendMark()` function with test | Unit test on marshalled JSON asserts `streamSid` field present |
| clear must echo pending marks | Phase: mark/clear Go/Node.js — design mark lifecycle before coding | Integration test: mark → audio → clear → verify mark echo received |
| OPTIONS goroutine not tied to ctx | Phase: SIP OPTIONS keepalive Go — same goroutine pattern as reregisterLoop | `go test -race -count=5` goroutine count is stable across registrar teardowns |
| 401 treated as registration loss | Phase: SIP OPTIONS keepalive Go — response code table in spec | Unit test: mock 401 response → `registered` stays `true`, no REGISTER sent |
| OPTIONS interval too aggressive | Phase: SIP OPTIONS keepalive Go/Node.js — add `SIP_OPTIONS_INTERVAL_S` env var | Config validation test: interval < 10 is rejected; default is 30 |
| OPTIONS targets wrong URI | Phase: SIP OPTIONS keepalive Go — unit test for request URI | Unit test on generated OPTIONS request: `Request-URI` == registrar URI |
| Concurrent doRegister race | Phase: SIP OPTIONS keepalive Go — add mutex before wiring trigger | `go test -race` on registrar with concurrent doRegister calls |
| Node.js WsClient interface not extended | Phase: mark/clear Node.js — extend interface before callManager changes | TypeScript compile error if callManager accesses raw `ws` object |

---

## Sources

- Twilio Media Streams WebSocket Messages — mark and clear event schemas, timing semantics: https://www.twilio.com/docs/voice/media-streams/websocket-messages (HIGH confidence — official Twilio documentation)
- Twilio changelog — bidirectional streaming + mark/clear for barge-in: https://www.twilio.com/en-us/changelog/bi-directional-streaming-support-with-media-streams (HIGH confidence — official Twilio announcement)
- OpenAI Community — mark/clear implementation pitfall: no confirmation when audio flush completes, pending marks accumulate: https://community.openai.com/t/openai-realtime-how-to-correctly-truncate-a-live-streaming-conversation-on-speech-interruption-twilio-media-streams/1371637 (MEDIUM confidence — community discussion, consistent with Twilio spec)
- sipgo pkg.go.dev — client.Do() signature, TransactionRequest, no explicit concurrent-call safety documentation: https://pkg.go.dev/github.com/emiago/sipgo (MEDIUM confidence — official library docs; concurrent safety not explicitly stated)
- sipgo GitHub client.go — Do() implementation pattern, CSeq auto-increment behavior: https://github.com/emiago/sipgo/blob/main/client.go (MEDIUM confidence — source code review)
- RFC 3261 §11 — SIP OPTIONS request semantics; out-of-dialog use for capability probing: https://www.rfc-editor.org/rfc/rfc3261 (HIGH confidence — normative RFC)
- RFC 6223 — Indication of Support for Keep-Alive in SIP: https://www.rfc-editor.org/rfc/rfc6223 (HIGH confidence — normative RFC)
- Cisco CUBE documentation — out-of-dialog OPTIONS ping group, response code interpretation for registration loss detection: https://www.cisco.com/c/en/us/td/docs/ios-xml/ios/voice/cube_sipsip/configuration/15-mt/cube-sipsip-15-mt-book/voi-out-of-dialog.html (MEDIUM confidence — vendor documentation; response code table is consistent with RFC 3261)
- sipgate-sip-stream-bridge v2.0 source — `wsPacer` sole-writer invariant, `wsSignal` pattern, `dtmfQueue` pattern (direct model for `markEchoQueue`): `go/internal/bridge/session.go` and `go/internal/bridge/ws.go` (HIGH confidence — production code)
- sipgate-sip-stream-bridge v1.0 source — `wsClient.ts` `onAudio` listener with "Ignore non-media events (mark, clear, etc.)" comment: `node/src/ws/wsClient.ts` (HIGH confidence — production code, documents the gap to fill)

---
*Pitfalls research for: mark/clear Twilio Media Streams events + SIP OPTIONS keepalive — v2.1 milestone additions*
*Researched: 2026-03-05*
