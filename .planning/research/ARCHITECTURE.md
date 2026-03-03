# Architecture Research

**Domain:** SIP-to-WebSocket audio bridge — Go rewrite of audio-dock
**Researched:** 2026-03-03
**Confidence:** HIGH (Go concurrency model, stdlib patterns); MEDIUM (sipgo API surface — verified against pkg.go.dev; goroutine lifecycle patterns — verified via official Go docs and multiple sources)

---

## Standard Architecture

### System Overview

```
  sipgate trunking
  UDP :5060 (SIP signaling)
  UDP :10000-10099 (RTP media)
         |
         v
  ┌──────────────────────────────────────────────────────────────────────────┐
  │                         audio-dock (Go binary)                           │
  │                                                                          │
  │  main()                                                                  │
  │  signal.NotifyContext(SIGTERM/SIGINT) → root ctx                         │
  │     │                                                                    │
  │     ├─ internal/config  ─── os.LookupEnv + validation, loaded once      │
  │     │                                                                    │
  │     ├─ internal/sip ──────────────────────────────────────────────────┐  │
  │     │    sipgo UserAgent + Server (ListenAndServe goroutine)           │  │
  │     │    sipgo Client (REGISTER + re-register goroutine)              │  │
  │     │    srv.OnInvite / OnAck / OnBye / OnCancel / OnOptions          │  │
  │     │    raw SIP helpers: SDP parser/builder, header extractors        │  │
  │     └────────────────────────────────┬───────────────────────────────┘  │
  │                                      │ SipCallbacks interface            │
  │     ┌────────────────────────────────v───────────────────────────────┐  │
  │     │                  internal/bridge.CallManager                    │  │
  │     │                                                                  │  │
  │     │  mu sync.RWMutex                                                 │  │
  │     │  sessions map[string]*CallSession      ← callId key              │  │
  │     │  pending  map[string]struct{}          ← dedup retransmissions   │  │
  │     │  cancelled map[string]struct{}         ← CANCEL race guard       │  │
  │     │                                                                  │  │
  │     │  handleInvite() → goroutine            [one goroutine per call]  │  │
  │     │     │                                                            │  │
  │     │     ├─ allocate RTP port (internal/rtp)                         │  │
  │     │     ├─ connect WS (internal/ws)                                 │  │
  │     │     └─ start call goroutines:                                   │  │
  │     │           rtpReadLoop  (goroutine A: net.UDPConn.ReadFromUDP)   │  │
  │     │           wsReadLoop   (goroutine B: wsConn.Read)               │  │
  │     │           silenceTick  (goroutine C: during WS reconnect only)  │  │
  │     └──────────────────────────────────────────────────────────────┘  │
  │                                                                          │
  │  internal/obs                                                            │
  │     net/http mux: GET /health, GET /metrics                              │
  │     (reads from CallManager via interface — no lock needed for counters) │
  └──────────────────────────────────────────────────────────────────────────┘
         |                                       |
         | RTP/UDP (PCMU 8kHz, 20ms packets)     | WebSocket (ws:// or wss://)
         v                                       v
  sipgate media servers                 AI voice-bot backend
                                        (Twilio Media Streams consumer)
```

### Component Responsibilities

| Component | Responsibility | Go Implementation |
|-----------|---------------|-------------------|
| **internal/config** | Parse + validate env vars at startup; fail fast | Plain struct + `os.LookupEnv`; validated with hand-written checks or `github.com/kelseyhightower/envconfig`; exported singleton |
| **internal/sip** | SIP registration + inbound message dispatch | `sipgo` `UserAgent`, `Server`, `Client`; registers via `ClientRequestRegisterBuild`; dispatches INVITE/BYE/ACK/CANCEL/OPTIONS via `srv.OnX` handlers; raw SDP helpers as pure functions |
| **internal/rtp** | Per-call UDP socket; RTP encode/decode; port allocation | `net.UDPConn` per call; atomic port counter (`sync/atomic`); 12-byte header read/write; PT=0 PCMU, PT=113 telephone-event |
| **internal/ws** | Per-call WebSocket client; Twilio Media Streams protocol | `github.com/gorilla/websocket`; separate read goroutine + write channel; connected/start/media/stop/dtmf JSON messages |
| **internal/bridge** | CallManager: session registry + INVITE orchestration; reconnect loop | `sync.RWMutex`-protected `map[string]*CallSession`; per-call goroutine set; reconnect goroutine with `time.Sleep` + budget |
| **internal/obs** | HTTP health + Prometheus metrics | `net/http` stdlib only; `prometheus/client_golang`; reads atomic counters from CallManager |
| **cmd/audio-dock** | Entry point: wire all components, start goroutines, handle SIGTERM | `signal.NotifyContext`; `sync.WaitGroup` for shutdown drain; 5s deadline context |

---

## Recommended Project Structure

```
audio-dock/
├── cmd/
│   └── audio-dock/
│       └── main.go              # Entry point: load config, wire, start, shutdown
├── internal/
│   ├── config/
│   │   └── config.go            # Env var struct, Load() func, validation
│   ├── sip/
│   │   ├── agent.go             # sipgo UserAgent+Server+Client factory; REGISTER loop
│   │   ├── sdp.go               # parseSdpOffer(), buildSdpAnswer() — pure functions
│   │   └── headers.go           # extractHeader(), extractAllVias(), buildResponse(), buildBye()
│   ├── rtp/
│   │   ├── handler.go           # RtpHandler struct: UDPConn, read loop, sendAudio()
│   │   └── port.go              # portAllocator: atomic counter, bindPort()
│   ├── ws/
│   │   ├── client.go            # WsClient: dial, readLoop goroutine, writeCh channel
│   │   └── protocol.go          # Twilio Media Streams JSON types + encode/decode
│   ├── bridge/
│   │   ├── call_manager.go      # CallManager struct, handleInvite, handleBye, terminateAll
│   │   ├── session.go           # CallSession struct definition
│   │   └── reconnect.go         # startWsReconnectLoop() — goroutine with backoff
│   └── obs/
│       └── server.go            # net/http mux: /health + /metrics handlers
├── Dockerfile                   # multi-stage: golang:1.24-bookworm → scratch/distroless
├── go.mod
└── go.sum
```

### Structure Rationale

- **cmd/audio-dock/main.go:** Minimal wiring — creates components, calls their Start methods, blocks on shutdown context. No logic here.
- **internal/:** All packages are `internal` — this is a single-binary service, not a library. Nothing should be importable from outside.
- **internal/sip/:** Separates the three concerns within SIP: the network plumbing (agent.go), SDP text processing (sdp.go), and SIP message construction helpers (headers.go). Each is independently testable.
- **internal/rtp/handler.go + port.go:** Port allocation is separate so it can use a package-level atomic without coupling to handler struct internals.
- **internal/ws/:** `client.go` owns the goroutine lifecycle. `protocol.go` contains only typed structs and JSON helpers — no I/O, fully unit testable.
- **internal/bridge/:** Split across three files because reconnect logic is complex enough to deserve its own file. `session.go` is data-only (no methods), making the struct easy to read.
- **internal/obs/:** Isolated so observability can be added or removed without touching any other package.

---

## Architectural Patterns

### Pattern 1: Two Goroutines Per Call (Read Loops Only; Write Is Synchronous)

**What:** Each active call runs exactly two long-lived goroutines: `rtpReadLoop` and `wsReadLoop`. These are the only goroutines blocked on I/O. All writes (RTP send, WS send) are performed inline from these read goroutines — no separate write goroutines needed because:
- `net.UDPConn.WriteTo` is safe to call from any goroutine concurrently.
- `gorilla/websocket` requires one concurrent writer; a buffered `chan []byte` (the `writeCh`) serializes all WS writes into a single `wsWriteLoop` goroutine.

**When to use:** Always for this service. The pattern eliminates the V8-drain-queue workaround entirely: goroutines block cheaply at the OS level; Go's runtime multiplexes them without event loop stalls.

**Trade-offs:** Goroutine-per-read is idiomatic Go. The write channel for WS adds one channel send per packet, which is negligible (~10ns). The silence goroutine (goroutine C) is only alive during reconnect windows.

**Example:**
```go
// internal/bridge/session.go
type CallSession struct {
    CallID       string
    CallSid      string
    StreamSid    string
    LocalTag     string
    RemoteTag    string
    RemoteURI    string
    RemoteTarget string
    CSeq         atomic.Int32

    rtp *rtp.Handler
    ws  *ws.Client    // replaced atomically during reconnect

    // Reconnect state — guarded by reconnMu
    reconnMu      sync.Mutex
    wsReconnecting bool

    cancel context.CancelFunc  // cancels call-scoped context → stops all goroutines
    wg     sync.WaitGroup      // tracks rtpReadLoop + wsReadLoop + wsWriteLoop

    log *slog.Logger
}

// internal/bridge/call_manager.go  (goroutine start)
func (cm *CallManager) startCallGoroutines(ctx context.Context, s *CallSession) {
    callCtx, cancel := context.WithCancel(ctx)
    s.cancel = cancel

    s.wg.Add(2)
    go s.rtpReadLoop(callCtx)   // goroutine A
    go s.wsReadLoop(callCtx)    // goroutine B
}
```

### Pattern 2: sync.RWMutex-Protected Session Map (Not sync.Map)

**What:** `CallManager` holds `sessions map[string]*CallSession` protected by `sync.RWMutex`. Reads use `RLock`; adds/removes use `Lock`. The `*CallSession` pointer is read under lock; once a goroutine holds the pointer it operates on the session without the manager lock.

**When to use:** When the workload is heavily read-biased (most messages arrive after setup — BYE lookups and audio routing — vs. the rare INVITE that adds a session).

**Trade-offs:** `sync.Map` is appropriate when keys are stable once written and reads far outnumber writes — fits here, but `sync.RWMutex` + regular map is faster and more readable for small N (tens of concurrent calls). Avoid `sync.Map` for this use case: it allocates more and has worse DX.

**Example:**
```go
type CallManager struct {
    mu       sync.RWMutex
    sessions map[string]*CallSession

    // Pending/cancelled are only accessed during INVITE setup — short-lived sets
    pendingMu  sync.Mutex
    pending    map[string]struct{}
    cancelled  map[string]struct{}

    sipHandle sip.Handle
    cfg       *config.Config
    log       *slog.Logger
}

func (cm *CallManager) session(callID string) (*CallSession, bool) {
    cm.mu.RLock()
    s, ok := cm.sessions[callID]
    cm.mu.RUnlock()
    return s, ok
}

func (cm *CallManager) addSession(s *CallSession) {
    cm.mu.Lock()
    cm.sessions[s.CallID] = s
    cm.mu.Unlock()
}
```

### Pattern 3: Reconnect Loop as Goroutine With for+select (Not Recursive Timer)

**What:** Replace the TypeScript recursive `async attempt(n)` pattern with a flat `for` loop inside a goroutine. The loop uses `time.Sleep` for backoff delay and checks the call context for early exit. No recursion, no timer handles to cancel.

**Why:** In Go there is no call stack risk from recursion depth and no "timer handle leak" problem. But a flat `for` loop is clearer and avoids the synchronization complexity of `time.AfterFunc` callbacks.

**Trade-offs:** The goroutine blocks during `time.Sleep` but costs only ~2KB of stack. Context cancellation (BYE arrives mid-sleep) is detected at the top of each loop iteration.

**Example:**
```go
// internal/bridge/reconnect.go
func (cm *CallManager) startWsReconnectLoop(
    ctx context.Context,
    s *CallSession,
    params ws.CallParams,
) {
    go func() {
        const budget = 30 * time.Second
        const cap    = 4 * time.Second
        started := time.Now()
        delay   := time.Second

        // Silence injection during reconnect window (WSR-01)
        silenceTicker := time.NewTicker(20 * time.Millisecond)
        defer silenceTicker.Stop()
        go func() {
            silence := bytes.Repeat([]byte{0xFF}, 160)
            for {
                select {
                case <-silenceTicker.C:
                    s.rtp.SendAudio(silence)
                case <-ctx.Done():
                    return
                }
            }
        }()

        attempt := 0
        for {
            // BYE race guard
            select {
            case <-ctx.Done():
                return
            default:
            }

            attempt++
            newWs, err := ws.Dial(ctx, cm.cfg.WSTargetURL, params, s.log)
            if err == nil {
                silenceTicker.Stop()
                // Swap WsClient under reconnMu
                s.reconnMu.Lock()
                s.ws = newWs
                s.wsReconnecting = false
                s.reconnMu.Unlock()
                // Restart wsReadLoop with new connection — send on channel
                s.wsReplaceCh <- newWs
                return
            }

            elapsed := time.Since(started)
            if elapsed+delay >= budget {
                cm.terminateSession(s, "ws_reconnect_failed", true)
                return
            }

            // Interruptible sleep
            select {
            case <-time.After(delay):
            case <-ctx.Done():
                return
            }
            if delay*2 < cap {
                delay *= 2
            } else {
                delay = cap
            }
        }
    }()
}
```

### Pattern 4: Context-Propagated Shutdown (signal.NotifyContext)

**What:** `main()` creates a root context via `signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)`. This context is passed to all long-running components. When SIGTERM arrives the context is cancelled, causing all goroutines blocked on `ctx.Done()` to unblock and exit cleanly.

**When to use:** Always — this is the idiomatic Go 1.16+ shutdown pattern. It replaces the TypeScript `process.on('SIGTERM', ...)` handler.

**Trade-offs:** Components must be written to respect context cancellation. The 5-second drain timeout is implemented as a second `context.WithTimeout` wrapping the shutdown sequence.

**Example:**
```go
// cmd/audio-dock/main.go
func main() {
    cfg := config.Load()

    ctx, stop := signal.NotifyContext(context.Background(),
        syscall.SIGTERM, syscall.SIGINT)
    defer stop()

    cm := bridge.NewCallManager(cfg, logger)
    sipHandle, err := sip.Start(ctx, cfg, logger, cm.Callbacks())
    // ...

    // Block until signal
    <-ctx.Done()
    stop() // release signal resources

    shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    cm.TerminateAll(shutdownCtx)      // BYE all calls + close WS
    sipHandle.Unregister(shutdownCtx) // REGISTER Expires:0
}
```

### Pattern 5: WS Write Channel (Serialized Concurrent Writers)

**What:** `gorilla/websocket` panics on concurrent writes. The `ws.Client` owns a `writeCh chan []byte` and a single `wsWriteLoop` goroutine that drains it. All callers — `rtpReadLoop` and the reconnect silence goroutine — send to `writeCh`. The channel is buffered (e.g., 32 entries) to avoid blocking callers under momentary write stalls.

**When to use:** Any time gorilla/websocket is used with multiple potential writers.

**Trade-offs:** One extra goroutine per call. The buffer prevents drops; if the buffer fills (WS stall), packets are dropped with a log warning — same behaviour as the TypeScript version's `ws.readyState !== OPEN` guard.

---

## Module-to-Package Mapping (TypeScript → Go)

| TypeScript Module | Go Package | Migration Notes |
|-------------------|-----------|-----------------|
| `src/config/index.ts` (Zod schema) | `internal/config/config.go` | Replace Zod with `os.LookupEnv` + manual validation or `kelseyhightower/envconfig`. Same env var names required (backward compat). |
| `src/logger/index.ts` (pino) | stdlib `log/slog` | `slog.New(slog.NewJSONHandler(os.Stdout, opts))`. Per-call logger via `slog.With("callId", ...)`. No external dep. |
| `src/sip/userAgent.ts` (custom raw SIP) | `internal/sip/agent.go` | Use `sipgo` instead of raw UDP. `sipgo.NewUA` + `sipgo.NewServer` + `sipgo.NewClient`. REGISTER auth via `client.DoDigestAuth`. |
| `src/sip/sdp.ts` (pure functions) | `internal/sip/sdp.go` | Direct port — same logic. `parseSdpOffer` and `buildSdpAnswer` as package-level funcs. |
| `src/rtp/rtpHandler.ts` (dgram EventEmitter) | `internal/rtp/handler.go` | Replace EventEmitter with channels: `audioCh chan []byte`, `dtmfCh chan string`. UDPConn.ReadFromUDP in goroutine. |
| `src/ws/wsClient.ts` (makeDrain + WsClient) | `internal/ws/client.go` | **Eliminate drain queues entirely** — they exist only to smooth V8 GC bursts. Go goroutines read from UDPConn at the natural OS buffer rate; no burst problem. |
| `src/bridge/callManager.ts` (CallSession Map) | `internal/bridge/call_manager.go` | Replace JS event-loop single-threaded safety with `sync.RWMutex`. Replace Promise-chain INVITE flow with sequential goroutine code. |

---

## Data Flow

### Inbound Call Setup (INVITE → audio bridge active)

```
sipgate (UDP :5060)
    │ SIP INVITE
    v
sipgo srv.OnInvite handler (called in sipgo goroutine)
    │ fire-and-forget: go cm.handleInvite(req, tx)
    v
cm.handleInvite goroutine
    │ 1. dedup check (pendingMu)
    │ 2. tx.Respond(100 Trying)
    │ 3. parse SDP body → remoteIp, remotePort, hasPcmu
    │ 4. rtp.AllocPort() → bind net.UDPConn on :10000-10099
    │ 5. rtp.SendAudio(silence) — NAT hole-punch
    │ 6. check cancelled set (CANCEL race guard)
    │ 7. ws.Dial(ctx, url, params) — 2s timeout
    │    failure → tx.Respond(503); rtp.Close(); return
    │ 8. tx.Respond(200 OK with SDP answer)
    │ 9. store *CallSession in cm.sessions
    │ 10. start rtpReadLoop (goroutine A)
    │     start wsWriteLoop (goroutine B)
    │     start wsReadLoop  (goroutine C)
    v
Call active — goroutines running
```

### RTP → WebSocket (inbound audio path)

```
sipgate media server
    │ UDP datagram: [12-byte RTP header][160-byte PCMU payload]
    v
rtp.Handler.rtpReadLoop (goroutine A — blocked on UDPConn.ReadFromUDP)
    │ parse RTP header: version, CSRC count, extension bit, payload type
    │ PT=0 (PCMU):  send payload to audioCh
    │ PT=113 (DTMF): decode event, check End bit, dedup by timestamp → dtmfCh
    v
audioCh <- payload   (non-blocking; drop if channel full — same as TS drop policy)
    │
    v
bridge wiring in handleInvite:
    for payload := range s.rtp.AudioCh() {
        if s.wsReconnecting { continue }   // WSR-03 gate
        s.ws.SendAudio(payload)            // enqueues to writeCh
    }
    v
ws.Client.writeCh <- JSON bytes
    v
wsWriteLoop goroutine → wsConn.WriteMessage(websocket.TextMessage, json)
    v
AI voice-bot backend
```

### WebSocket → RTP (outbound audio path)

```
AI voice-bot backend
    │ WS text frame: {"event":"media","media":{"payload":"<base64>"}}
    v
ws.Client.wsReadLoop (goroutine C — blocked on wsConn.ReadMessage)
    │ json.Unmarshal → TwilioMessage
    │ event == "media": base64.Decode payload → []byte
    │ chunk into 160-byte slices, pad last slice with 0xFF if short
    v
s.rtp.SendAudio(slice)  — direct call, no intermediate channel
    │ build 12-byte RTP header (seq++, ts += 160, ssrc fixed)
    │ UDPConn.WriteTo(packet, remoteAddr)  — thread-safe
    v
sipgate media server → caller handset
```

### WebSocket Reconnect Flow

```
ws.Client wsReadLoop receives error / EOF
    │ signal disconnect via disconnCh <- struct{}{}
    v
handleInvite bridge wiring detects disconnect:
    s.reconnMu.Lock(); s.wsReconnecting = true; s.reconnMu.Unlock()
    go cm.startWsReconnectLoop(callCtx, s, params)
    v
reconnect goroutine:
    ┌─ start silence ticker (20ms) → rtp.SendAudio(silence) [WSR-01]
    │
    └─ loop:
         check callCtx.Done() — exit if BYE arrived
         ws.Dial(ctx, url, params)
         success → swap s.ws, send on s.wsReplaceCh, stop ticker, return
         failure → time.Sleep(delay); delay = min(delay*2, 4s)
         budget  → terminateSession(s, "ws_reconnect_failed", true)
```

### SIGTERM Shutdown Flow

```
OS → SIGTERM
    │
    v
signal.NotifyContext cancels root ctx
    │
    v
main() unblocks from <-ctx.Done()
    │
    v
shutdownCtx, _ := context.WithTimeout(ctx, 5s)
    │
    ├─ cm.TerminateAll(shutdownCtx)
    │     for each session: terminateSession(s, "shutdown", true)
    │       → BYE + ws.Stop() + rtp.Close()
    │       → s.cancel() → rtpReadLoop + wsReadLoop exit
    │       → s.wg.Wait() — drain goroutines
    │
    └─ sipHandle.Unregister(shutdownCtx)
          → REGISTER Expires:0, Contact:*
```

---

## Goroutine Lifecycle Per Call

```
handleInvite goroutine (exits after session stored + goroutines started)
│
├── goroutine A: rtpReadLoop
│     blocked: net.UDPConn.ReadFromUDP
│     exits:   ctx.Done() signals UDPConn.SetReadDeadline(past) OR conn.Close()
│
├── goroutine B: wsWriteLoop
│     blocked: chan []byte (writeCh) OR wsConn.WriteMessage
│     exits:   writeCh closed (ws.Stop() closes it)
│
├── goroutine C: wsReadLoop
│     blocked: wsConn.ReadMessage
│     exits:   wsConn.Close() from ws.Stop() OR wsConn.SetReadDeadline(past)
│
└── goroutine D: wsReconnectLoop (only during reconnect window)
      blocked: time.Sleep OR ws.Dial
      exits:   reconnect success, budget exhausted, or ctx.Done()
```

**Key invariant:** `s.wg.Wait()` in `terminateSession` ensures all A/B/C goroutines have exited before `TerminateAll` returns. This is the Go equivalent of the TypeScript `Promise.all` in `terminateAll()`.

---

## Concurrency Decision Matrix

| Concern | TypeScript Approach | Go Approach | Rationale |
|---------|--------------------|-----------| ---------|
| Session map safety | JS event loop (single-threaded) | `sync.RWMutex` | Go is multi-threaded; map is not goroutine-safe |
| Per-call audio send | EventEmitter callback chain | Channel + goroutine | Channels decouple producer/consumer without callback nesting |
| WS writes | `ws.readyState` guard + drain queue | `writeCh chan []byte` + single writer goroutine | gorilla/websocket: one concurrent writer only |
| RTP writes | Synchronous in callback | `UDPConn.WriteTo` direct (thread-safe) | `net.PacketConn` is safe for concurrent writes |
| Reconnect loop | Recursive `async attempt()` | Flat `for` loop in goroutine | No call stack risk; `ctx.Done()` interrupts cleanly |
| SIGTERM | `process.on('SIGTERM')` | `signal.NotifyContext` + root ctx | Context propagates to all goroutines automatically |
| Timer cleanup (silence) | `clearInterval(session.silenceInterval)` | `ticker.Stop()` + `ctx.Done()` select branch | No handles stored on struct; goroutine self-terminates |
| 200 OK retransmit | `setTimeout` + timer handle on session | goroutine + `time.Sleep(T1)` loop, exits on ACK channel | No timer handle leak; simpler cancellation |

---

## Scaling Considerations

| Scale | Architecture Adjustments |
|-------|--------------------------|
| 1-20 concurrent calls | Single Go binary; ~4 goroutines per call = ~80 goroutines total; negligible overhead |
| 20-100 concurrent calls | Monitor `runtime.NumGoroutine()` via /metrics. UDP SO_RCVBUF tuning may be needed per socket. Still single-process. |
| 100+ concurrent calls | Port range expansion (RTP_PORT_MIN/MAX). Goroutines remain cheap (Go handles millions). Bottleneck is likely network bandwidth, not CPU. Horizontal scale if needed — each instance is stateless within a call. |

### Scaling Priorities

1. **First bottleneck:** JSON serialization for Twilio Media Streams at high call volume. At 50 calls × 50 packets/sec = 2,500 JSON encodes/sec. This is fast in Go but can be profiled and switched to pre-computed byte slices if needed.
2. **Second bottleneck:** UDP receive buffer exhaustion — tune `net.UDPConn.SetReadBuffer()` if packets drop under load. Default OS buffer is typically 208KB; increase to 4MB for audio workloads.
3. **Third bottleneck:** Port exhaustion from the 10000-10099 default range (100 ports). Configurable via env vars; expand range for higher concurrency.

---

## Anti-Patterns

### Anti-Pattern 1: Shared UDPConn Across All Calls

**What people do:** One UDP socket bound to a fixed port, multiplexed by source IP:port.
**Why it's wrong:** SDP negotiation requires advertising a unique local port per call. A single socket makes this impossible. Demuxing is error-prone.
**Do this instead:** One `net.UDPConn` per call, bound to a port from the configured range. Close it in `terminateSession`.

### Anti-Pattern 2: Goroutine Per Packet

**What people do:** `go handlePacket(buf)` inside the read loop.
**Why it's wrong:** For 20ms/160-byte RTP packets at 50/sec per call, spawning a goroutine per packet creates unnecessary scheduler pressure. The handler is CPU-cheap (parse 12 bytes, send to channel).
**Do this instead:** Process the packet inline in the read loop; send to a channel only if crossing a goroutine boundary is needed.

### Anti-Pattern 3: Using time.AfterFunc for Reconnect / Retransmit Loops

**What people do:** Port the TypeScript `setTimeout(fn, delay)` pattern directly using `time.AfterFunc`.
**Why it's wrong:** `time.AfterFunc` callbacks run in a new goroutine each time, which makes clean cancellation complex. Storing timer handles on the session struct recreates the exact cleanup problem from TypeScript's `okRetransmitTimer`.
**Do this instead:** A single goroutine with `time.Sleep` or `time.NewTimer` + `select { case <-timer.C: case <-ctx.Done(): }`. Context cancellation terminates cleanly without explicit handle management.

### Anti-Pattern 4: Ignoring WS Drain Queues in the Go Port

**What people do:** Port the `makeDrain` queue from TypeScript assuming it is needed for audio smoothing.
**Why it's wrong:** The drain queue in TypeScript compensates for V8 GC pauses causing burst delivery. Go goroutines are scheduled by the runtime without GC-pause jitter. The underlying OS network stack delivers UDP packets at their natural rate to a reading goroutine without burst accumulation.
**Do this instead:** Eliminate the drain queue entirely. The `rtpReadLoop` reads one packet per iteration; audio flows at the wire cadence with no accumulation.

### Anti-Pattern 5: Accepting INVITE Before WS Connects (Fail-Open)

**What people do:** Accept the INVITE, then dial the WebSocket asynchronously, and silently drop audio if WS fails.
**Why it's wrong:** Same as in the TypeScript version — the caller hears a connected call with no backend processing it.
**Do this instead:** In `handleInvite`, dial the WebSocket with a 2-second timeout context before calling `tx.Respond(200 OK)`. If `ws.Dial` returns an error, `tx.Respond(503 Service Unavailable)`.

### Anti-Pattern 6: CSeq and Sequence Fields as Non-Atomic

**What people do:** Store `CSeq int` on `CallSession` and increment with `session.CSeq++`.
**Why it's wrong:** `handleBye` (sipgo callback goroutine) and `terminateSession` (called from reconnect goroutine or main goroutine) may both read CSeq concurrently.
**Do this instead:** Use `atomic.Int32` for CSeq on the session struct, or read CSeq under the session's own mutex.

---

## Integration Points

### External Services

| Service | Integration Pattern | Notes |
|---------|---------------------|-------|
| sipgate trunking | `sipgo.NewServer` + `sipgo.NewClient` over UDP:5060; REGISTER with Digest Auth | `sipgo.ClientRequestRegisterBuild` handles auth challenge. Re-register at 90% of SIP_EXPIRES via ticker goroutine. |
| AI voice-bot backend | `gorilla/websocket` Dial as client; Twilio Media Streams JSON | One WS connection per call. Backend acts as server. audio-dock sends connected/start/media/stop events. |

### Internal Boundaries

| Boundary | Communication | Notes |
|----------|---------------|-------|
| `internal/sip` → `internal/bridge` | `SipCallbacks` interface (function fields) | Set at startup; no import cycle. sip package calls bridge methods. |
| `internal/bridge` → `internal/sip` | `SipHandle` interface (sendRaw, unregister) | Bridge holds interface; sip package implements it. |
| `internal/rtp` → `internal/bridge` | `audioCh <-chan []byte`, `dtmfCh <-chan string` on `RtpHandler` | Read-only channels exposed; bridge range-reads from them in goroutine. |
| `internal/ws` → `internal/bridge` | `disconnCh <-chan struct{}` on `WsClient`; `SendAudio()` method | Bridge detects disconnect via channel receive; sends audio via method call. |
| `internal/bridge` → `internal/obs` | `StatsProvider` interface: `ActiveCalls() int`, `Registered() bool` | Obs package calls into bridge; no reverse dependency. Atomic reads only — no lock needed. |

---

## Suggested Build Order

The dependency graph forces this implementation sequence. Each step unlocks the next.

```
Phase 1: Foundation (no external deps)
  1a. internal/config — pure env var parsing; validate all required vars
  1b. log/slog setup in main.go — JSON handler, level from config
  1c. internal/sip/sdp.go — pure functions, no deps, unit testable immediately

Phase 2: Transport Layer (blocking I/O, each package independently testable)
  2a. internal/rtp/port.go — port allocator (atomic counter, bindPort)
  2b. internal/rtp/handler.go — UDPConn + read loop + RTP header codec
      [test: bind socket, send RTP packet, verify audioCh receives payload]
  2c. internal/ws/protocol.go — Twilio JSON types + encode/decode (pure)
  2d. internal/ws/client.go — Dial + writeCh + read/write goroutines
      [test: mock WS server, verify connected+start sent, media echo]

Phase 3: SIP Layer (depends on config only)
  3a. internal/sip/headers.go — extractHeader, buildResponse, buildBye (pure)
  3b. internal/sip/agent.go — sipgo UA + REGISTER + re-register loop
      [test: mock UDP registrar, verify 401→auth→200 flow]
      [test: INVITE dispatches to OnInvite callback]

Phase 4: Bridge (depends on all of Phase 2 + 3)
  4a. internal/bridge/session.go — CallSession struct (data only)
  4b. internal/bridge/call_manager.go — handleInvite orchestration
      [test: fake rtp+ws, verify 200 OK + session stored]
  4c. internal/bridge/reconnect.go — WS reconnect loop
      [test: drop WS mid-call, verify silence injection + reconnect + resume]

Phase 5: Observability (depends on bridge interface only)
  5a. internal/obs/server.go — /health + /metrics on net/http
      [test: HTTP GET /health returns active call count]

Phase 6: Wiring + Dockerfile
  6a. cmd/audio-dock/main.go — component wiring + signal.NotifyContext shutdown
  6b. Dockerfile multi-stage: golang:1.24 → scratch
      [test: docker build + health probe]
```

**Critical path:** RTP port binding (2a) must succeed before SDP answer can be built (1c produces the template; the port is the variable). WS must connect (2d) before 200 OK is sent. These are enforced by the sequential flow in `handleInvite`.

**Goroutine dependency at startup:** The sipgo server goroutine (from `srv.ListenAndServe`) must be running before `cm.Callbacks()` are invoked. The REGISTER goroutine blocks until 200 OK is received before `main()` signals ready. No INVITE can arrive before REGISTER completes because sipgate will not route calls to an unregistered endpoint.

---

## Sources

- [emiago/sipgo GitHub](https://github.com/emiago/sipgo) — MEDIUM confidence; README and pkg.go.dev API surface verified; goroutine model inferred from source structure
- [sipgo pkg.go.dev](https://pkg.go.dev/github.com/emiago/sipgo) — MEDIUM confidence; API types, DialogServerCache, OnInvite handler pattern
- [gorilla/websocket pkg.go.dev](https://pkg.go.dev/github.com/gorilla/websocket) — HIGH confidence; concurrent writer constraint is explicitly documented
- [coder/websocket GitHub](https://github.com/coder/websocket) — MEDIUM confidence; alternative to gorilla with better context support (nhooyr.io/websocket successor)
- [Go Wiki: sync.Mutex or channel?](https://go.dev/wiki/MutexOrChannel) — HIGH confidence; official Go wiki recommendation
- [signal.NotifyContext — Go stdlib](https://pkg.go.dev/os/signal#NotifyContext) — HIGH confidence; official stdlib docs; Go 1.16+
- [Graceful Shutdown in Go — VictoriaMetrics](https://victoriametrics.com/blog/go-graceful-shutdown/) — MEDIUM confidence; widely referenced production pattern
- [Go standard project layout](https://github.com/golang-standards/project-layout) — MEDIUM confidence; community standard, not official; `internal/` usage is official Go spec
- [log/slog — Go 1.21+](https://pkg.go.dev/log/slog) — HIGH confidence; official stdlib since Go 1.21
- [kelseyhightower/envconfig](https://pkg.go.dev/github.com/kelseyhightower/envconfig) — MEDIUM confidence; widely used; struct-tag-based env var parsing

---
*Architecture research for: audio-dock Go rewrite (SIP/RTP/WebSocket bridge)*
*Researched: 2026-03-03*
