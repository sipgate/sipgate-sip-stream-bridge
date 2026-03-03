# Pitfalls Research

**Domain:** Go SIP/RTP/WebSocket audio bridge (v2.0 Go rewrite of audio-dock)
**Researched:** 2026-03-03
**Confidence:** MEDIUM-HIGH — sipgo library pitfalls verified via GitHub issues, official docs, and release notes; Go UDP/GC/CGO patterns verified via official Go docs and multiple community sources; WebSocket concurrency verified via gorilla/websocket official docs

---

## Critical Pitfalls

### Pitfall 1: sipgo Dialog Termination at 64*T1 (~32s) Without Explicit `defer dlg.Close()`

**What goes wrong:**
SIP dialogs in sipgo silently terminate after approximately 32 seconds (64 × T1 = 64 × 500ms) even during active calls. The call appears to be live — RTP is flowing, no BYE was received — but the dialog object is cleaned up internally by the transaction timeout mechanism. The call goroutine then leaks or accesses a dead dialog state.

**Why it happens:**
RFC 3261 defines Timer B/F at 64*T1 for transaction timeouts. sipgo's INVITE server transaction implements this timer. Before sipgo fixed the dialog lifecycle (issue #59 addressed in v0.x), the transaction timeout directly terminated the dialog. Even after the fix, misuse is common: developers that handle the INVITE without calling `defer dlg.Close()` and without waiting on `<-dlg.Context().Done()` leave the dialog in limbo. The handler goroutine exits but the dialog stays in the cache, accumulating memory for every call.

**How to avoid:**
```go
srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
    dlg, err := dialogSrv.ReadInvite(req, tx)
    if err != nil { return }
    defer dlg.Close() // MANDATORY — cleans up from DialogServerCache

    dlg.Respond(sip.StatusTrying, "Trying", nil)
    // ... setup RTP, WS, etc ...
    dlg.Respond(sip.StatusOK, "OK", sdpBody)

    // MANDATORY — block until dialog terminates (BYE received or context cancelled)
    <-dlg.Context().Done()
    // cleanup RTP socket, WS here
})

srv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {
    dialogSrv.ReadAck(req, tx) // MANDATORY — routes ACK to correct dialog
})

srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
    dialogSrv.ReadBye(req, tx) // MANDATORY — terminates dialog context
})
```
Without `ReadAck` and `ReadBye` registered, the dialog context never closes and goroutines block forever.

**Warning signs:**
- Calls hang after 30–32 seconds and are dropped without a BYE.
- `runtime/pprof` goroutine dump shows goroutines blocked on `<-dlg.Context().Done()` for terminated calls.
- Memory grows proportionally to total call count (not concurrent call count).

**Phase to address:** Phase 1 (SIP foundation) — INVITE handler pattern must be established correctly from day one.

---

### Pitfall 2: gorilla/websocket Concurrent Write Panic — "concurrent write to websocket connection"

**What goes wrong:**
The RTP receive loop, the silence injection timer, and the DTMF forwarder all run in separate goroutines and all call `ws.WriteMessage()`. gorilla/websocket panics with `panic: concurrent write to websocket connection` when two goroutines call any write method simultaneously. The panic crashes the entire process — not just the affected call.

**Why it happens:**
gorilla/websocket explicitly states: "Connections support one concurrent reader and one concurrent writer. Applications are responsible for ensuring that no more than one goroutine calls the write methods concurrently." The library detects concurrent writes with an `isWriting` flag and panics deliberately — the buffer is corrupted before detection occurs. Adding a `sync.Mutex` around individual `WriteMessage` calls is not sufficient if the outer state (e.g., framing state across multiple `NextWriter` calls) is interleaved.

**How to avoid:**
Use a dedicated writer goroutine and a channel to serialize all writes:
```go
type wsWriter struct {
    conn    *websocket.Conn
    writeCh chan []byte
    done    chan struct{}
}

func newWsWriter(conn *websocket.Conn) *wsWriter {
    w := &wsWriter{
        conn:    conn,
        writeCh: make(chan []byte, 128), // buffer = ~2.5 seconds of audio at 50pps
        done:    make(chan struct{}),
    }
    go w.pump()
    return w
}

func (w *wsWriter) pump() {
    defer close(w.done)
    for msg := range w.writeCh {
        if err := w.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
            return
        }
    }
}

func (w *wsWriter) Send(msg []byte) {
    select {
    case w.writeCh <- msg:
    default:
        // drop if full — prevents blocking the RTP read goroutine
    }
}

func (w *wsWriter) Close() {
    close(w.writeCh)
    <-w.done
}
```
Alternative: use `nhooyr.io/websocket` which is goroutine-safe for concurrent writes by design (all write methods may be called concurrently).

**Warning signs:**
- Process crashes with `panic: concurrent write to websocket connection`.
- Crashes happen intermittently under load (2+ concurrent calls) but never with a single call.
- Stack trace shows multiple goroutines in `websocket.(*Conn).WriteMessage` at the same time.

**Phase to address:** Phase 2 (WebSocket bridge) — design the writer architecture before wiring RTP callbacks.

---

### Pitfall 3: CGO in Any Dependency Silently Breaks `FROM scratch` Docker Image

**What goes wrong:**
The Go binary links against libc at runtime if any transitive dependency uses CGO. The binary appears to compile fine with `CGO_ENABLED=1` (default). The `FROM scratch` Docker image has no libc. At container start, the binary immediately exits with `exec format error` or `no such file or directory` — with no useful error message on Alpine/scratch.

**Why it happens:**
Go's default CGO_ENABLED=1 causes the `net` package to use the system C resolver for DNS lookups. If `os/user` or any imported package contains `import "C"`, the resulting binary has a dynamic dependency on glibc/musl. The developer doesn't notice because local builds (macOS/glibc Linux) work fine. The problem only surfaces on the scratch Docker image.

**Specific risk for this project:** sipgo itself is pure Go. However, popular logging or monitoring libraries can pull in CGO-dependent packages. Prometheus client (`github.com/prometheus/client_golang`) is pure Go — safe. Any SQLite or system-call-heavy library would not be.

**How to avoid:**
```dockerfile
# In Dockerfile builder stage — explicitly disable CGO
FROM golang:1.24-alpine AS builder
ENV CGO_ENABLED=0
ENV GOOS=linux
ENV GOARCH=amd64
RUN go build -a -ldflags="-s -w" -o audio-dock ./cmd/audio-dock

# Verify no dynamic linking before shipping
RUN ldd audio-dock 2>&1 | grep -q "not a dynamic executable" || exit 1
```
Add to CI: `file ./audio-dock` must output `statically linked`, not `dynamically linked`.
Verify after adding every new dependency: `go build -v ./... 2>&1 | grep -i cgo` should be empty.

**Warning signs:**
- Container starts but exits immediately with no log output.
- `docker logs` shows `exec format error` or `/lib/x86_64-linux-gnu/libc.so.6: no such file`.
- `ldd ./audio-dock` on the build host shows `linux-vdso.so` and `libc` entries.
- `go build` output contains lines with `cgo` for unexpected packages.

**Phase to address:** Phase 1 (Docker foundation) — enforce `CGO_ENABLED=0` from the first Dockerfile commit.

---

### Pitfall 4: Go GC Pauses Cause RTP Packet Bursts (20ms Timing Violation)

**What goes wrong:**
Go's GC pauses goroutine execution for STW (stop-the-world) phases. At default `GOGC=100`, a heap that doubles triggers a GC cycle. In a high-allocation path (JSON serialization per RTP packet, base64 encoding, WebSocket frame allocation), GC pauses accumulate to 5–15ms per cycle. The RTP receiver goroutine is paused during GC. When it resumes, the OS socket buffer has queued 3–8 packets. These arrive at the WebSocket backend as a burst, causing the jitter buffer to overflow or underflow — audible as clicks, gaps, or garbled audio.

**Why it happens:**
v1.0 Node.js built a drain-queue workaround precisely because of this problem (see `makeDrain()` in wsClient.ts). Go's GC is far better than V8's but not zero-latency. The default `GOGC=100` is optimized for throughput, not latency. Per-packet allocations (JSON objects, base64 strings, byte slices) drive frequent GC cycles.

**How to avoid:**
Two complementary strategies:

1. **Reduce allocations in the hot path (primary):**
   - Reuse a `sync.Pool` of `[]byte` buffers for RTP reads: no per-packet allocation on the read path.
   - Build the Twilio JSON envelope with a pre-allocated `[]byte` buffer using `fmt.Appendf` or a fixed-layout template; avoid `json.Marshal` per packet.
   - The base64 encoding of 160 bytes is always exactly 216 characters — pre-allocate the output buffer.

2. **Tune GC for lower latency (secondary):**
   ```
   # In container env vars:
   GOGC=200          # GC runs less often; tolerable for a bounded-memory service
   GOMEMLIMIT=256MiB # Hard cap prevents runaway heap; combined with GOGC=200 is safe
   ```
   Do NOT set `GOGC=off` without `GOMEMLIMIT` — unbounded heap growth until OOM kill.
   Official Go GC guide warning: memory limit + no GOGC can cause GC thrashing if the limit is too tight.

**Warning signs:**
- Audio quality degrades under load (multiple concurrent calls).
- `go tool pprof` heap profile shows high allocation rate in the RTP receive path.
- WS backend observes irregular inter-packet timing (jitter > 5ms) even though Go RTP handler runs in a dedicated goroutine.
- `GODEBUG=gccheckmark=1` reveals frequent GC activity during call processing.

**Phase to address:** Phase 2 (RTP implementation) — establish allocation-free hot path; Phase 4 (observability) — expose GC metrics via Prometheus to monitor in production.

---

### Pitfall 5: UDP Read Loop Goroutine Leak on Call Teardown

**What goes wrong:**
The UDP read goroutine for each call blocks on `conn.ReadFromUDP()`. When the call ends and `conn.Close()` is called, the read goroutine must unblock and exit. If `conn.Close()` races with the goroutine (e.g., called from a different goroutine before the reader has exited), the goroutine may block indefinitely on a closed socket or loop on an error it doesn't recognize as a shutdown signal. After 10+ calls, `GOMAXPROCS` goroutines are stuck in a blocking syscall, consuming file descriptors and memory.

**Why it happens:**
`net.UDPConn.ReadFromUDP()` is a blocking call. Closing the connection from another goroutine causes it to return with an error (`use of closed network connection`). If the read loop checks `err != nil` and returns without special-casing this error string (which is an internal Go error, not exported), the goroutine exits cleanly. But if the loop retries on any error, it will spin forever on a closed socket.

**How to avoid:**
```go
func (h *rtpHandler) readLoop(ctx context.Context) {
    buf := make([]byte, 1500)
    for {
        n, _, err := h.conn.ReadFromUDP(buf)
        if err != nil {
            // conn.Close() from another goroutine causes this.
            // Also triggered by ctx cancellation if SetReadDeadline is used.
            return // any error = stop reading; let cleanup caller handle it
        }
        h.handlePacket(buf[:n])
    }
}

// Shutdown: close the conn, then wait for the goroutine to exit.
func (h *rtpHandler) Close() {
    h.conn.Close() // unblocks ReadFromUDP
    h.wg.Wait()   // wait for readLoop to exit
}
```
Use `sync.WaitGroup` to ensure the goroutine has exited before `Close()` returns. Callers that proceed to close other resources (WS, session map) after `Close()` need this guarantee.

Alternatively, use `conn.SetReadDeadline(time.Now())` before closing — causes `ReadFromUDP` to return with a deadline error immediately.

**Warning signs:**
- Goroutine count grows with each completed call and never decreases.
- `GET /debug/pprof/goroutine` shows goroutines blocked in `net.(*UDPConn).ReadFromUDP` for calls that ended minutes ago.
- File descriptor count (check `/proc/<pid>/fd/`) grows with call count.
- `netstat -u` shows UDP sockets in `CLOSE_WAIT` or orphaned.

**Phase to address:** Phase 2 (RTP) — establish clean shutdown pattern in initial UDP implementation; verify with goroutine-count assertion in tests.

---

### Pitfall 6: sipgo `ClientRequestRegisterBuild` Not Used — CSeq Double-Increment Breaks Re-REGISTER

**What goes wrong:**
sipgo's `client.TransactionRequest()` automatically increments the CSeq header before sending the request (RFC 3261 requirement for new transactions). When `REGISTER` is called for re-registration, the CSeq is already set in the request object and gets incremented again — producing a double-increment. sipgate's registrar then rejects the re-REGISTER with a 400 or 401 because the CSeq sequence is unexpected. Registration works once at startup but fails silently on every subsequent refresh.

**Why it happens:**
sipgo documentation states: "Every new transaction will have implicitly CSEQ increase if present." For REGISTER requests, the correct option is `sipgo.ClientRequestRegisterBuild`, which builds the request correctly per RFC and handles the CSeq counter. Developers copying examples that use `TransactionRequest` without this option miss the subtle API requirement.

**How to avoid:**
```go
// WRONG — CSeq will be double-incremented:
tx, err := client.TransactionRequest(ctx, registerReq)

// CORRECT:
tx, err := client.TransactionRequest(ctx, registerReq,
    sipgo.ClientRequestRegisterBuild)

// Or use the high-level helper:
res, err := client.Do(ctx, registerReq, sipgo.ClientRequestRegisterBuild)
```
Also: the `REGISTER` transaction object must be stored and `defer tx.Terminate()` called on completion. Forgetting `tx.Terminate()` leaks the transaction goroutine.

**Warning signs:**
- First REGISTER succeeds, first re-REGISTER (90s later) gets 400 Bad Request or 401 Unauthorized.
- sipgate logs show CSeq values that are not monotonically increasing by 1.
- `tx.Terminate()` not called — transaction goroutine leaks (observable in pprof goroutine profile).

**Phase to address:** Phase 1 (SIP registration) — must be correct before call acceptance is built.

---

### Pitfall 7: CANCEL After 200 OK Race — Must Handle `cancelledInvites` Set

**What goes wrong:**
sipgate sends a CANCEL while the INVITE is being processed (WS connection is being established, which takes up to 2 seconds). The CANCEL arrives before the 200 OK is sent. If the handler does not track this state, the 200 OK is sent anyway, the SIP session is "confirmed" from the server's perspective, but the caller has already hung up. The RTP handler and WS client are allocated but will never be used. They leak until the 64*T1 dialog timer fires.

This exact race condition was implemented in v1.0 (see `cancelledInvites` set in callManager.ts lines 338–340, 385–389).

**Why it happens:**
The INVITE handler is async (it awaits WS connection). CANCEL can arrive at any point during this async window. In Go, this translates to CANCEL arriving on a separate goroutine while the INVITE handler goroutine is blocked in `createWsClient()`. Without shared state and a check after each await point, the CANCEL is lost.

**How to avoid:**
Use an atomic or mutex-protected set of "cancelled call IDs":
```go
type callManager struct {
    pendingInvites  sync.Map // callID -> struct{}
    cancelledCalls  sync.Map // callID -> struct{}
    activeSessions  sync.Map // callID -> *callSession
}

// In INVITE handler:
h.pendingInvites.Store(callID, struct{}{})
defer h.pendingInvites.Delete(callID)
defer h.cancelledCalls.Delete(callID)

ws, err := createWsClient(...)

// Check CANCEL race after every blocking operation:
if _, cancelled := h.cancelledCalls.Load(callID); cancelled {
    ws.Close()
    rtp.Close()
    return // 487 was already sent by CANCEL handler
}

// In CANCEL handler:
h.cancelledCalls.Store(callID, struct{}{})
// If still pending, send 487:
if _, pending := h.pendingInvites.Load(callID); pending {
    tx.Respond(sip.NewResponseFromRequest(req, 487, "Request Terminated", nil))
}
```

**Warning signs:**
- Calls that are cancelled quickly (caller dials and immediately cancels) show lingering RTP sockets.
- Log shows "200 OK sent" followed immediately by "BYE received" from sipgate (sipgate sends BYE when it receives 200 OK for a cancelled INVITE).
- Goroutine leak in WS connection goroutines for calls that were cancelled.

**Phase to address:** Phase 2 (call lifecycle) — implement CANCEL race guard before testing with live sipgate calls.

---

### Pitfall 8: sipgo v0.x Security Advisory — Nil Pointer Dereference on Missing To Header (GHSA-c623-f998-8hhv)

**What goes wrong:**
Any malformed SIP request without a `To` header causes `sipgo.NewResponseFromRequest()` to panic with a nil pointer dereference, crashing the entire service. One malformed UDP packet kills all active calls.

**Why it happens:**
`NewResponseFromRequest` assumes the `To` header is present without nil-checking it. This is a parsing oversight. The flaw exists in versions `>= v0.3.0, < v1.0.0-alpha-1`.

**How to avoid:**
Pin sipgo to `>= v1.0.0` (stable release December 2024). The v1.0.0 release is marked as API-stable: "No breaking changes planned in the near future." Current stable version as of research: **v1.2.0** (February 2025).

```go
// go.mod
require github.com/emiago/sipgo v1.2.0
```

**Warning signs:**
- Any use of sipgo versions 0.x.
- Service panics with `nil pointer dereference` in response to incoming SIP traffic.

**Phase to address:** Phase 1 (library selection) — start with v1.2.0 from day one; never use v0.x.

---

### Pitfall 9: Record-Route Headers Reversed or Missing in 200 OK (Proxy-Unroutable BYE)

**What goes wrong:**
The in-dialog BYE is routed using the Route set constructed from the Record-Route headers in the 200 OK. If Record-Route headers are missing from the 200 OK, or their order is reversed, the BYE goes directly to the caller's Contact address instead of through the sipgate proxy chain. sipgate's SBC then rejects the BYE with 403 or drops it, and the call is never terminated cleanly from the server side. The caller hangs up locally but the server session stays open.

This was a known v1.0 bug fixed in callManager.ts — `extractAllRecordRoutes()` and the `recordRoutes` parameter to `buildResponse()` were added specifically for this.

**Why it happens:**
If using sipgo's `dlg.Respond()`, it handles Record-Route automatically from the INVITE. But if building responses manually (as in audio-dock's raw UDP approach), Record-Route headers must be explicitly extracted from the INVITE and echoed verbatim (in the same order) into the 200 OK. RFC 3261 §12.1.2 MUST requirement. Developers forget this and the response "works" in simple test environments (no proxy) but fails with sipgate's multi-proxy topology.

**How to avoid:**
If using sipgo's dialog API (`dlg.Respond()`): this is handled automatically — no action needed.
If building raw SIP responses:
```go
// Extract ALL Record-Route headers from the INVITE (preserve order):
var recordRoutes []string
for _, rr := range req.RecordRoute() {
    recordRoutes = append(recordRoutes, rr.String())
}
// Include in 200 OK — MUST echo verbatim, same order as in INVITE:
// RFC 3261 §12.1.2: The UAS MUST copy all Record-Route header field values
// from the request into the response.
```
Validate by checking that outbound BYE (after the call) routes through the proxy (visible in SIP packet capture as routed through sip.sipgate.de, not directly to the caller IP).

**Warning signs:**
- BYE is sent but caller never receives it; call stays on their phone after server sends BYE.
- SIP packet capture shows BYE going to caller's direct IP, bypassing sip.sipgate.de proxy.
- sipgate SBC returns 403 to BYE.
- In-dialog requests (BYE, re-INVITE) get 481 "Call/Transaction Does Not Exist".

**Phase to address:** Phase 2 (call lifecycle) — verify Record-Route echoing with a multi-call test against live sipgate before Phase 3.

---

### Pitfall 10: sipgate Non-Standard DTMF Payload Type 113 (Not RFC Standard PT 101)

**What goes wrong:**
sipgate uses RTP payload type 113 for `telephone-event/8000`, not the commonly assumed PT 101. A Go implementation that listens only on PT 101 (or dynamically assigned PT 97/98/101) receives DTMF packets but silently ignores them, reporting no DTMF events. The caller presses keys, nothing happens.

This was already solved in v1.0 (`if payloadType === 113` in rtpHandler.ts line 150) and documented in the SDP answer (`a=rtpmap:113 telephone-event/8000`).

**Why it happens:**
RFC 4733 defines telephone-event as a dynamic payload type (96–127); the specific value is negotiated in SDP. sipgate negotiates PT 113. Many Go SIP/RTP examples hard-code PT 101 or 96 from pion/rtp defaults. If the SDP answer doesn't advertise PT 113, or if the RTP handler checks for the wrong PT, DTMF is silently lost.

**How to avoid:**
In the SDP answer, always declare PT 113 explicitly:
```
m=audio <port> RTP/AVP 0 113
a=rtpmap:0 PCMU/8000
a=rtpmap:113 telephone-event/8000
a=fmtp:113 0-16
```
In the RTP handler, check for PT 113 (not 101):
```go
const ptTelephoneEvent = 113 // sipgate-specific, negotiated in SDP

switch payloadType {
case 0:
    h.handlePCMU(payload)
case ptTelephoneEvent:
    h.handleDTMF(payload, rtpTimestamp)
}
```

**Warning signs:**
- DTMF events never arrive despite caller pressing keys (confirmed in network capture showing PT 113 packets).
- SDP answer contains `a=rtpmap:101 telephone-event/8000` instead of `a=rtpmap:113 telephone-event/8000`.
- RTP handler reports "unknown payload type 113" in debug logs.

**Phase to address:** Phase 2 (RTP) — replicate the PT 113 logic from v1.0 exactly.

---

## Technical Debt Patterns

| Shortcut | Immediate Benefit | Long-term Cost | When Acceptable |
|----------|-------------------|----------------|-----------------|
| `CGO_ENABLED=1` (default) with `FROM scratch` | No build changes needed | Binary won't run in the container; silent failure | Never — set `CGO_ENABLED=0` from day one |
| Using `time.After()` inside a per-packet loop | Readable timeout logic | One goroutine + 201-byte timer object created per packet; destroys latency at 50pps | Never in hot paths — use `time.NewTimer` and `Reset()` |
| `json.Marshal(envelope)` per RTP packet | Simple code | ~3 allocations per packet × 50pps × N calls = high GC pressure | Never in production hot path — use pre-built templates |
| Skipping `tx.Terminate()` on REGISTER transactions | Less code | Transaction goroutines leak; pprof goroutine count grows with each re-REGISTER cycle | Never |
| Building raw SIP responses instead of using sipgo dialog API | Full control, matches v1.0 architecture | Must manually handle Record-Route, Via echoing — correctness burden; high risk | Acceptable if the team has SIP expertise and tests against sipgate live |
| `GOGC=off` without `GOMEMLIMIT` | Fewer GC pauses | Unbounded heap growth; OOM kill in container | Never — must pair with `GOMEMLIMIT` if used |
| Ignoring `dlg.Context().Done()` in INVITE handler | Handler exits faster | Dialog never terminates cleanly; memory leak per call | Never |

---

## Integration Gotchas

| Integration | Common Mistake | Correct Approach |
|-------------|----------------|------------------|
| sipgate trunk (UDP SIP) | Forgetting that sipgate uses UDP SIP (not WSS for trunk-side); the trunk sends INVITE directly to the public IP:5060 | Bind a raw UDP listener on port 5060 for SIP (not a WebSocket transport); sipgate trunk does not use WSS for call delivery |
| sipgate trunk | Advertising Docker-internal IP (`172.x.x.x`) in SDP `c=` line | Set `SDP_CONTACT_IP` env var to host's public/reachable IP; verify with `tcpdump udp port 5060` that the correct IP appears in the SDP |
| sipgate trunk DTMF | Hardcoding PT 101 for telephone-event | Advertise and handle PT 113; confirmed behavior from v1.0 live testing |
| sipgate trunk re-REGISTER | Not refreshing at 90% of server-specified `Expires` | Read `Expires` from 200 OK Contact header; schedule re-REGISTER at 90% of that value |
| gorilla/websocket AI backend | Calling `WriteMessage` from RTP goroutine AND from silence-injection timer goroutine | Route all writes through a single channel+goroutine writer; or switch to `nhooyr.io/websocket` |
| Docker `FROM scratch` | Any CGO dependency in the dependency tree | Enforce `CGO_ENABLED=0` in Dockerfile; add `ldd` check as build verification step |
| Go module proxy | `GOFLAGS=-mod=vendor` must match whether `vendor/` directory is present | If using `-mod=vendor`, run `go mod vendor` in the builder stage before `go build`; or omit vendor and rely on module cache downloaded in the builder stage |

---

## Performance Traps

| Trap | Symptoms | Prevention | When It Breaks |
|------|----------|------------|----------------|
| Per-packet `json.Marshal()` for Twilio envelope | GC pressure; jitter at 50 pps × N calls | Pre-build envelope template; only substitute base64 payload field using `fmt.Appendf` | At 3+ concurrent calls (150+ pps) |
| `make([]byte, 1500)` inside RTP read loop | ~1500 bytes allocated per packet; GC thrashes | Use `sync.Pool` for read buffers; reset and return on each iteration | At 50+ concurrent calls |
| Separate goroutine per 20ms silence packet (using `time.AfterFunc`) | Goroutine spawn overhead at 50/s; timer heap grows | Use `time.NewTicker(20*time.Millisecond)` — reuses a single timer goroutine | Immediately visible in goroutine profiler |
| `net.UDPConn` default OS receive buffer (~208 KB on Linux) | Packets dropped by kernel during GC pauses | Call `conn.SetReadBuffer(1024*1024)` (1 MB) on each RTP socket at bind time | During GC pauses if default buffer fills in < 1 second |
| `sync.Map` for active sessions when all mutations are lock-protected anyway | Slower than `map` + `sync.RWMutex` for read-heavy workloads (due to sync.Map's pointer indirection) | For <100 concurrent sessions, a plain `map[string]*session` with `sync.RWMutex` is faster and simpler | Not a blocking issue at expected call volumes |

---

## Security Mistakes

| Mistake | Risk | Prevention |
|---------|------|------------|
| Using sipgo v0.x (< v1.0.0-alpha-1) | Remote DoS: one malformed SIP packet (missing To header) crashes the entire service — no authentication required | Pin `github.com/emiago/sipgo >= v1.2.0` in go.mod |
| Exposing `net/http/pprof` on the public SIP interface | Memory/goroutine dumps accessible without authentication | Bind pprof to `127.0.0.1` only; never on `0.0.0.0`; use a separate management port |
| Not validating source IP for SIP INVITEs | Unauthorized call injection from non-sipgate IPs | Validate source against sipgate's published IP ranges at the UDP listener level |
| Logging raw SIP messages at DEBUG level in production | SIP credentials (Authorization header with digest hash) and PSTN numbers in log aggregator | Redact `Authorization` headers before logging; use structured logging field exclusion |

---

## "Looks Done But Isn't" Checklist

- [ ] **SIP Registration:** `REGISTER` succeeds at startup — verify a re-REGISTER occurs at ~90% of the `Expires` value returned in the 200 OK Contact header (not just the value sent).
- [ ] **DTMF:** Digits are pressed on the PSTN phone — verify PT 113 packets arrive and `telephone-event` events are forwarded to WS backend; check SDP answer has `a=rtpmap:113 telephone-event/8000`.
- [ ] **RTP socket cleanup:** Run 10 sequential calls end-to-end — verify goroutine count and open file descriptor count return to pre-test baseline.
- [ ] **CANCEL race:** Dial and cancel within 500ms (before WS connects) — verify no RTP socket or WS client is leaked; verify 487 was sent.
- [ ] **Record-Route:** Make a call through sipgate (which uses a proxy) — verify BYE routes through the proxy chain, not directly to the caller IP.
- [ ] **Static binary:** Run `ldd ./audio-dock` on the compiled binary — must output `not a dynamic executable`. Check `file ./audio-dock` — must say `statically linked`.
- [ ] **200 OK retransmit:** Intercept the ACK (drop one with iptables) — verify 200 OK is retransmitted with intervals capped at T2=4s; verify retransmit stops when ACK arrives.
- [ ] **Concurrent writes:** Run 2+ concurrent calls while silence injection is active — verify no `panic: concurrent write to websocket connection` in logs.
- [ ] **GC under load:** Run 5 concurrent calls with `GODEBUG=gcstoptheworld=1` and verify audio timing; check pprof heap allocation rate is not growing per call.
- [ ] **sipgo version:** Confirm `go.mod` pins `github.com/emiago/sipgo v1.2.0` or later; verify no v0.x transitive dependency.

---

## Recovery Strategies

| Pitfall | Recovery Cost | Recovery Steps |
|---------|---------------|----------------|
| Dialog leak (missing `dlg.Close()`) | LOW | Add `defer dlg.Close()` and `<-dlg.Context().Done()` to every INVITE handler; existing leaks clear on process restart |
| gorilla/websocket concurrent write panic | MEDIUM | Introduce writer-channel goroutine pattern; refactor all call paths that write to WS to use the channel; or migrate to `nhooyr.io/websocket` |
| CGO binary fails in scratch container | LOW | Set `CGO_ENABLED=0` in Dockerfile and rebuild; add `ldd` verification step |
| DTMF not working (wrong PT) | LOW | Change PT 101 → PT 113 in SDP answer and RTP handler; redeploy |
| CSeq double-increment on re-REGISTER | LOW | Add `sipgo.ClientRequestRegisterBuild` option to the REGISTER transaction call |
| Record-Route missing from 200 OK | MEDIUM | Extract Record-Route headers from INVITE and include in 200 OK; test with live sipgate call to confirm BYE routing |
| GC jitter causing audio degradation | MEDIUM | Profile hot path allocations; introduce `sync.Pool` for RTP buffers; tune `GOGC=200 GOMEMLIMIT=256MiB`; measure improvement with Prometheus jitter histogram |
| CANCEL race causing resource leak | MEDIUM | Add `sync.Map`-based cancelled-invites tracking; add check after each async wait in INVITE handler |

---

## Pitfall-to-Phase Mapping

| Pitfall | Prevention Phase | Verification |
|---------|------------------|--------------|
| sipgo v0.x nil pointer DoS | Phase 1 (library selection) | `go.mod` shows `v1.2.0`; run sipgo basic tests with and without To header |
| `ClientRequestRegisterBuild` missing — CSeq double-increment | Phase 1 (SIP registration) | Send 3 REGISTER renewals; verify each CSeq increments by exactly 1 |
| Dialog leak — missing `dlg.Close()` + `<-dlg.Context().Done()` | Phase 1 (INVITE handler pattern) | Run 10 calls; goroutine count returns to baseline |
| DTMF PT 113 | Phase 2 (RTP handler) | Press keys during live call; DTMF events appear in WS stream |
| gorilla/websocket concurrent write panic | Phase 2 (WebSocket bridge) | 2+ concurrent calls + silence injection; zero panics in 1-hour soak |
| UDP read goroutine leak | Phase 2 (RTP handler) | 20 sequential calls; FD count and goroutine count return to baseline |
| CANCEL race / resource leak | Phase 2 (call lifecycle) | Fast-cancel test (< 500ms after dial); zero leaked RTP sockets |
| Record-Route missing in 200 OK | Phase 2 (call lifecycle) | BYE routes through sipgate proxy (verify in wireshark) |
| CGO in `FROM scratch` Docker image | Phase 1 (Docker foundation) | `ldd ./audio-dock` → `not a dynamic executable`; CI enforces this |
| GC-induced RTP jitter | Phase 2/3 (RTP + resilience) | Prometheus jitter histogram shows p99 < 2ms on 5-call soak |
| 200 OK retransmit timer correctness | Phase 2 (SIP call lifecycle) | Drop one ACK with iptables; verify retransmit schedule (500ms, 1s, 2s, 4s, 4s…) |

---

## Sources

- sipgo GitHub issue #59 — Dialog terminating mid-call at 64*T1: https://github.com/emiago/sipgo/issues/59 (MEDIUM confidence — issue directly describes the problem)
- sipgo security advisory GHSA-c623-f998-8hhv — Nil pointer DoS via missing To header: https://github.com/emiago/sipgo/security/advisories/GHSA-c623-f998-8hhv (HIGH confidence — official security advisory)
- sipgo release v1.2.0 (February 2025) — UDP memory leak fix, ACK routing fix: https://github.com/emiago/sipgo/releases (HIGH confidence — official release notes)
- sipgo pkg.go.dev documentation — `ClientRequestRegisterBuild` requirement, `defer dlg.Close()` pattern: https://pkg.go.dev/github.com/emiago/sipgo (HIGH confidence — official documentation)
- gorilla/websocket pkg.go.dev — concurrent write constraint: https://pkg.go.dev/github.com/gorilla/websocket (HIGH confidence — official documentation)
- gorilla/websocket issue #913 — "panic: concurrent write to websocket connection": https://github.com/gorilla/websocket/issues/913 (HIGH confidence — confirmed in multiple issues)
- nhooyr.io/websocket comparison — goroutine-safe writes: https://pkg.go.dev/nhooyr.io/websocket (MEDIUM confidence — docs state "all methods may be called concurrently except Reader and Read")
- Go GC Guide (official) — GOGC, GOMEMLIMIT, thrashing risk: https://tip.golang.org/doc/gc-guide (HIGH confidence — official Go documentation)
- golang/go issue #43451 — UDPConn.ReadFromUDP allocates per call: https://github.com/golang/go/issues/43451 (HIGH confidence — official Go issue tracker)
- Building static Go binaries (Eli Bendersky, 2024) — CGO unexpected dependencies: https://eli.thegreenplace.net/2024/building-static-binaries-with-go-on-linux/ (HIGH confidence — verified author, current)
- RFC 3261 §12.1.2 — Record-Route MUST be echoed in 200 OK: https://www.rfc-editor.org/rfc/rfc3261 (HIGH confidence — normative RFC)
- RFC 4733 §2.5 — Telephone-event redundant End packet deduplication: https://datatracker.ietf.org/doc/html/rfc4733 (HIGH confidence — normative RFC)
- RFC 5407 — Example call flows of race conditions (CANCEL/200 OK): https://datatracker.ietf.org/doc/html/rfc5407 (HIGH confidence — normative RFC)
- audio-dock v1.0 source — PT 113 implementation, CANCEL race, Record-Route: src/rtp/rtpHandler.ts, src/bridge/callManager.ts (HIGH confidence — production-verified behavior)

---
*Pitfalls research for: Go SIP/RTP/WebSocket audio bridge (audio-dock v2.0 Go rewrite)*
*Researched: 2026-03-03*
