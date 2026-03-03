# Feature Research

**Domain:** SIP-to-WebSocket audio bridge / SIP media gateway — Go rewrite (v2.0)
**Researched:** 2026-03-03
**Confidence:** HIGH for protocol details and Go stdlib patterns; MEDIUM for sipgo/diago API specifics (v1.2.0, February 2025)

---

## Scope of This Document

This document covers all features that must be re-implemented in Go (v2.0 rewrite). The v1.0 Node.js/TypeScript implementation is the behavioral reference. Each feature section includes:
- Expected behavior (what must be preserved)
- Go-specific implementation pattern (how to do it idiomatically)
- Key edge cases the implementation must handle
- Complexity in the Go context (may differ from the original TypeScript complexity)

---

## Feature Landscape

### Table Stakes (Users Expect These)

Features that must exist or the service does not function at all. The rewrite is a 1:1 feature-parity migration — every table-stakes item from v1.0 must survive into v2.0.

| Feature | Why Expected | Complexity | Go Implementation Notes |
|---------|--------------|------------|------------------------|
| SIP REGISTER on startup with digest auth | Service cannot receive inbound calls without registration | MEDIUM | Use `emiago/sipgo` v1.2.0 `Client.DoDigestAuth` for the 401 challenge-response cycle. Send initial REGISTER → receive 401 → call `DoDigestAuth` → receive 200 OK. See below. |
| Automatic re-REGISTER before expiry | SIP registrations expire; failure causes calls to stop silently | LOW | sipgo does NOT provide built-in refresh timers. Implement a goroutine that sleeps for `expires * 0.9` seconds then re-sends REGISTER (with incremented CSeq via `ClientRequestRegisterBuild`). |
| Accept inbound SIP INVITE | Core function — receive and answer incoming calls | HIGH | Use `sipgo.NewServer` + `srv.OnInvite(handler)`. Handler receives `*sip.Request` and `sip.ServerTransaction`. Use `DialogServerCache.ReadInvite` for dialog lifecycle management. |
| SIP INVITE lifecycle: 100 Trying → 180 Ringing → 200 OK + SDP → ACK | Correct SIP dialog state machine | HIGH | `dlg.Respond(100, "Trying", nil)` then `dlg.Respond(180, "Ringing", nil)` then `dlg.RespondSDP(sdpBytes)`. Wait for ACK via `select { case <-tx.Acks(): }`. sipgo handles retransmission of 200 OK automatically within the transaction. |
| SDP offer parsing (extract remote IP, port, PCMU) | Must know where to send RTP before answering | MEDIUM | Plain string parsing with `regexp` or line-by-line scan. Parse `c=IN IP4 <ip>`, `m=audio <port> RTP/AVP <pts>`. Check for payload type `0` (PCMU). Reject with 488 if absent. |
| SDP answer construction (local IP + RTP port, PCMU + telephone-event) | Must tell caller where to send RTP | LOW | Build SDP string directly. Lines joined with `\r\n`. Advertise `m=audio <localPort> RTP/AVP 0 113`, `a=rtpmap:0 PCMU/8000`, `a=rtpmap:113 telephone-event/8000`, `a=fmtp:113 0-16`, `a=ptime:20`. |
| RTP socket per call (net.UDPConn) | Each call needs an isolated UDP socket bound to a port in the configured range | HIGH | `net.ListenUDP("udp4", &net.UDPAddr{Port: port})`. One goroutine per socket runs the read loop (`conn.ReadFromUDP`). Use atomic port counter + retry on `EADDRINUSE`. |
| RTP receive: parse RFC 3550 header, emit PCMU/PT0 payload | Inbound audio from caller → forward to WebSocket | HIGH | Parse 12-byte fixed header manually: `buf[0]` (version/flags), `buf[1] & 0x7f` (payload type), CSRC count, extension bit, variable header length. Extract payload after `headerLen` bytes. |
| RTP DTMF: RFC 4733 telephone-event PT 113 | sipgate uses PT 113 for telephone-event/8000 | MEDIUM | On PT 113 packet: check End bit (`buf[headerLen+1] & 0x80`). Deduplicate by RTP timestamp (RFC 4733 mandates 3 redundant End packets). Map event code to digit via lookup table (0-15 → 0-9, *, #, A-D). |
| RTP send: build RFC 3550 12-byte header + PCMU payload | Outbound audio from WebSocket → RTP to caller | MEDIUM | Build header: `0x80` (V=2), `0x00` (M=0, PT=0), seq (16-bit BE), timestamp (32-bit BE, +160 per 20ms frame), SSRC (random, fixed per call). Use `binary.BigEndian.PutUint16` / `PutUint32`. `conn.WriteToUDP(packet, remoteAddr)`. |
| WebSocket connection per call (gorilla/websocket or coder/websocket) | Each call needs an independent WS connection | MEDIUM | Use `github.com/gorilla/websocket` Dialer with 2-second timeout. One connection per call. On open: send `connected` then `start` JSON immediately. |
| Twilio Media Streams: connected + start + media + stop + dtmf events | WS server expects exact protocol sequence | LOW | `json.Marshal` each event struct. `sequenceNumber` and `chunk` are per-session counters as strings. `streamSid` is `MZ` + 32 hex chars. `callSid` is `CA` + 32 hex chars. |
| Inbound WS media → RTP (outbound audio) | Bidirectional bridge — WS server sends audio back | MEDIUM | Read goroutine calls `conn.ReadMessage()` in a loop. Parse JSON, extract `media.payload`, `base64.StdEncoding.DecodeString`, chunk into 160-byte slices, packetize each as RTP, send. Pad short final chunk to 160 bytes with `0xFF` (mulaw silence). |
| WS reconnect with exponential backoff (1s/2s/4s, 30s budget) | Transient WS drops should not force caller to redial | MEDIUM | Dedicated goroutine started on WS disconnect. `time.Sleep` between attempts. On success: re-wire audio callbacks, re-send `connected` + `start`. Use `atomic.Bool` as `wsReconnecting` flag to gate RTP forwarding. |
| Silence injection during WS reconnect | Keep NAT pinholes open; prevent caller from hearing dead silence | LOW | `time.NewTicker(20 * time.Millisecond)` goroutine. On each tick: `conn.WriteToUDP(silencePacket, remoteAddr)`. Stop ticker by closing a `done` channel on reconnect success or call termination. |
| Multiple concurrent call sessions | Production service must handle N simultaneous calls | MEDIUM | `CallManager` holds `sync.RWMutex` + `map[string]*CallSession`. Write lock only for add/remove. Read lock for lookup during BYE/CANCEL. Alternatively `sync.Map` if reads dominate writes (they will). |
| SIGTERM graceful shutdown | BYE all active calls + UNREGISTER + close WS connections | LOW | `signal.NotifyContext(ctx, syscall.SIGTERM, os.Interrupt)`. On context cancel: iterate all sessions, call `session.Terminate()` (sends SIP BYE + WS stop), then send REGISTER with `Expires: 0` to UNREGISTER. Use `sync.WaitGroup` to wait for all sessions to finish. |
| GET /health JSON endpoint | Kubernetes liveness/readiness probes | LOW | `net/http` stdlib only. `http.HandleFunc("/health", ...)`. Return `{"registered":true,"activeCalls":N}`. Read `registered` from atomic bool set by SIP registration callback. Read `activeCalls` from `len(sessions)` under read lock. |
| GET /metrics Prometheus endpoint | Counter/gauge metrics for observability | LOW | `github.com/prometheus/client_golang/prometheus` + `promhttp`. Register 5 counters: `sip_calls_total`, `sip_calls_active` (gauge), `ws_reconnect_attempts_total`, `rtp_packets_rx_total`, `rtp_packets_tx_total`. Use `promauto` for auto-registration. Expose via `promhttp.Handler()`. |
| Environment variable configuration | Docker/K8s requires all config via env | LOW | `os.Getenv` + manual validation at startup. Fail fast with `log.Fatalf` on missing required vars. Use `strconv.Atoi` for numeric vars. No external validation library needed in Go. Same env var names as v1.0. |
| Docker static binary (FROM scratch or distroless) | Smaller image, no Node.js runtime | LOW | `CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o audio-dock ./cmd/audio-dock`. Produces a fully static binary. Use `FROM scratch` or `gcr.io/distroless/static` as final stage. Add `ca-certificates` if TLS WS connection needed. |

---

### Differentiators (Production Hardening)

Features that distinguish the Go rewrite from the Node.js v1.0, beyond the core feature parity.

| Feature | Value Proposition | Complexity | Go Implementation Notes |
|---------|-------------------|------------|------------------------|
| Deterministic sub-millisecond RTP latency | No GC-induced audio jitter (primary rewrite motivation) | LOW (free from architecture) | Go's GC is concurrent and has sub-1ms pause targets. Raw `net.UDPConn` reads bypass any drain queue. 20ms RTP frames arrive and are forwarded in the same goroutine tick. No drain queue needed (unlike the Node.js makeDrain helper). |
| Static binary Docker image | Dramatically smaller image (< 20MB vs 180MB Node.js alpine) | LOW | `FROM scratch` with static binary. No Alpine, no Node.js runtime, no npm packages. Faster CI pull times, smaller attack surface. |
| log/slog structured logging | Standard library structured logging (Go 1.21+) | LOW | `log/slog` with `slog.NewJSONHandler(os.Stdout, nil)`. Add per-call attributes via `slog.With("callId", id, "streamSid", sid)`. No external dependency needed (unlike pino in Node.js). |
| context-propagated cancellation | Clean goroutine lifecycle management native to Go | MEDIUM | All long-running goroutines accept `context.Context`. On `ctx.Done()` close: stop ticker, close UDP socket, close WS. Avoids the timer/interval cleanup ceremony required in Node.js. |
| Optional diago higher-level API | `emiago/diago` wraps sipgo with AudioReader/AudioWriter IO interfaces, making the bridge cleaner | HIGH (research needed) | diago v0.x provides `dialog.AudioReader()` / `dialog.AudioWriter()` as `io.Reader` / `io.Writer` wrappers over RTP. May simplify the RTP layer significantly. However: diago's media config must support raw PCMU bytes (not decoded PCM), which needs verification. |

---

### Anti-Features (Commonly Requested, Often Problematic)

Carry forward from v1.0 — these are still out of scope in v2.0.

| Feature | Why Requested | Why Problematic | Alternative |
|---------|---------------|-----------------|-------------|
| Outbound call initiation | "Make outbound calls too" | Different SIP state machine, different dialog flow. Doubles surface area. | Reject in v2.0. Different milestone. |
| Multiple WS target URLs / routing | "Route different callers to different bots" | Routing is application-layer logic. Bridge should be a dumb pipe. | Implement routing at the WS server using `customParameters` in the `start` event. |
| Audio transcoding (G.722, Opus) | "Support more codecs" | Transcoding adds latency and CPU. Each codec multiplies test surface. | Fix to PCMU. Reject SDP offers without PT 0. Transcoding belongs in WS consumer. |
| SRTP media encryption | "Encrypted audio" | DTLS handshake + key exchange in SDP negotiation — significant complexity. Project.md explicitly excludes this. | VPN/private subnet for internal deployments. |
| Web UI / management dashboard | "Monitor calls visually" | Entirely orthogonal to the bridge. Requires frontend build pipeline. | `/health` + `/metrics` → Grafana dashboard externally. |
| Call recording / CDR database | "Store call history" | GDPR surface, storage requirements. Bridge is a pipe, not a recorder. | WS server receives all audio already — record there. |

---

## Feature Dependencies

```
[SIP REGISTER on startup]
    └──required before──> [Accept inbound SIP INVITE]

[DialogServerCache.ReadInvite]
    └──required for──> [100 Trying → 180 Ringing → 200 OK lifecycle]
    └──required for──> [BYE handling via dlg.Bye()]

[RTP socket bind (net.UDPConn)]
    └──must succeed before──> [SDP answer construction]
    (local RTP port must be known before building the SDP c= and m= lines)

[SDP answer construction]
    └──must complete before──> [Send 200 OK to INVITE]

[WebSocket connected]
    └──must succeed before──> [Send 200 OK to INVITE]
    (fail fast: reject with 503 if WS unavailable at call setup time)

[Twilio: connected event]
    └──must precede──> [Twilio: start event]
                           └──must precede──> [Twilio: media events]

[RTP read goroutine]
    └──feeds──> [Twilio: media events (inbound)]

[Twilio: inbound media events (from WS)]
    └──feeds──> [RTP send (WriteToUDP)]

[WS reconnect goroutine]
    └──requires──> [Silence ticker goroutine] (during reconnect window)
    └──restores──> [RTP → WS forwarding] (after success)
    └──terminates call if──> [30s budget exhausted]

[sync.RWMutex + sessions map]
    └──required by──> [Multiple concurrent calls]
    └──required by──> [SIGTERM graceful shutdown] (iterate all sessions)

[atomic.Bool registered]
    └──feeds──> [GET /health JSON response]

[prometheus counters/gauges]
    └──feeds──> [GET /metrics Prometheus response]
```

### Dependency Notes

- **RTP socket before SDP**: The port must be known when building the 200 OK SDP answer. Bind first, then answer.
- **WS before 200 OK**: The fail-fast design means WS availability gates the SIP answer. The 2-second WS connect timeout must complete before sending 200 OK.
- **Silence ticker vs. RTP forward gate**: These are mutually exclusive. When `wsReconnecting == true`, the RTP forward path drops packets and the ticker injects silence. When reconnect succeeds, the ticker stops and the forward path resumes.
- **gorilla/websocket one-writer rule**: Only one goroutine may call write methods. In Go this is enforced by routing all WS writes through a single goroutine with a channel — the `writePump` pattern (see Go-specific patterns below).

---

## Go-Specific Implementation Patterns

### 1. SIP REGISTER with Digest Auth (sipgo)

sipgo v1.2.0 does not auto-register or provide refresh timers. The full lifecycle must be implemented manually.

```go
// One-time registration with challenge-response
func register(ctx context.Context, client *sipgo.Client, uri sip.Uri, auth sipgo.DigestAuth, expiresSec int) error {
    req := sip.NewRequest(sip.REGISTER, uri)
    req.SetExpires(uint32(expiresSec))
    // ... add From, To, Contact headers

    res, err := client.TransactionRequest(ctx, req, sipgo.ClientRequestBuild)
    if err != nil {
        return err
    }
    if res.StatusCode == 401 || res.StatusCode == 407 {
        res, err = client.DoDigestAuth(ctx, req, res, auth)
        if err != nil {
            return err
        }
    }
    if res.StatusCode != 200 {
        return fmt.Errorf("REGISTER failed: %d %s", res.StatusCode, res.Reason)
    }
    return nil
}

// Re-registration goroutine
func reregisterLoop(ctx context.Context, client *sipgo.Client, ..., expiresSec int) {
    for {
        select {
        case <-ctx.Done():
            return
        case <-time.After(time.Duration(float64(expiresSec)*0.9) * time.Second):
            if err := register(ctx, client, ...); err != nil {
                // log error, retry with backoff
            }
        }
    }
}
```

**Key points:**
- CSeq is auto-incremented by `ClientRequestRegisterBuild` on each new transaction — do not manage manually.
- Parse the `Expires` value from the 200 OK `Contact` header to use the server-authoritative expiry (not the value you sent).
- On network reconnect: call `register()` unconditionally — do not rely on state assumptions.
- For UNREGISTER on shutdown: send REGISTER with `Expires: 0`.

---

### 2. RTP Goroutine Model

One goroutine per call socket. The goroutine owns the read loop exclusively.

```go
type RTPSession struct {
    conn       *net.UDPConn
    remoteAddr *net.UDPAddr
    outSeq     uint16         // only accessed from send goroutine
    outTS      uint32
    outSSRC    uint32
    // ... other fields
}

// Read loop — one goroutine per call
func (r *RTPSession) readLoop(ctx context.Context, onAudio func([]byte), onDTMF func(string)) {
    buf := make([]byte, 1500) // MTU-safe buffer, reuse across reads
    for {
        select {
        case <-ctx.Done():
            return
        default:
        }
        r.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
        n, _, err := r.conn.ReadFromUDP(buf)
        if err != nil {
            if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
                continue // check ctx.Done() again
            }
            return // real error
        }
        r.parsePacket(buf[:n], onAudio, onDTMF)
    }
}
```

**Key points:**
- `net.UDPConn` is safe for concurrent reads from multiple goroutines — but use exactly one reader to avoid partial-read races on the same buffer.
- Reuse a stack-allocated or long-lived buffer inside the goroutine. Do NOT allocate a new slice per packet (GC pressure at 50 packets/sec per call).
- `SetReadDeadline` with periodic renewal allows `ctx.Done()` to be checked without blocking forever on `ReadFromUDP`.
- Sending (outbound RTP) can happen from any goroutine — `WriteToUDP` is safe for concurrent writers because UDP writes are atomic at the OS level for packets < MTU. Avoid concurrent writes anyway to prevent interleaving.

---

### 3. WebSocket: gorilla/websocket Read/Write Pump Pattern

gorilla/websocket enforces: **at most one concurrent reader, at most one concurrent writer**. Violation corrupts the frame stream silently. The canonical pattern is a `readPump` goroutine and a `writePump` goroutine per connection, communicating via a channel.

```go
type WSSession struct {
    conn    *websocket.Conn
    sendCh  chan []byte       // buffered channel, writePump drains it
    done    chan struct{}      // closed when WS is disconnected
}

// writePump — only goroutine that calls conn.WriteMessage
func (s *WSSession) writePump() {
    defer close(s.done)
    for {
        msg, ok := <-s.sendCh
        if !ok {
            return // channel closed — connection being torn down
        }
        if err := s.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
            return
        }
    }
}

// readPump — only goroutine that calls conn.ReadMessage
func (s *WSSession) readPump(onAudio func([]byte)) {
    defer close(s.sendCh) // triggers writePump exit
    for {
        _, data, err := s.conn.ReadMessage()
        if err != nil {
            return // disconnect
        }
        // parse JSON, dispatch to onAudio callback
    }
}

// Send from any goroutine — non-blocking with drop on full
func (s *WSSession) Send(msg []byte) {
    select {
    case s.sendCh <- msg:
    default:
        // channel full — drop packet (same policy as v1.0 Node.js)
    }
}
```

**Key points:**
- Channel size of 10-20 frames is sufficient. RTP arrives at 50 packets/sec per call; a full 20-frame buffer represents 400ms of audio to discard if WS is slow — acceptable.
- The `writePump` pattern is documented in the official gorilla/websocket chat example. This is not optional — concurrent writes will panic or corrupt frames.
- `websocket.Conn.Close()` and `WriteControl()` are the only methods safe to call concurrently with all other methods.
- `coder/websocket` (formerly nhooyr.io/websocket) is a modern alternative with context-aware APIs, fewer footguns around the one-writer rule, and idiomatic Go. Consider it if gorilla/websocket creates pain. Both are production-grade in 2025.

---

### 4. CallManager: struct + sync.RWMutex

Go's official guidance: use `sync.Mutex` for shared state, channels for communication. A `CallManager` is shared state (a map). Use `sync.RWMutex`.

```go
type CallManager struct {
    mu       sync.RWMutex
    sessions map[string]*CallSession
    // ... other fields
}

func (m *CallManager) add(callID string, s *CallSession) {
    m.mu.Lock()
    defer m.mu.Unlock()
    m.sessions[callID] = s
}

func (m *CallManager) get(callID string) (*CallSession, bool) {
    m.mu.RLock()
    defer m.mu.RUnlock()
    s, ok := m.sessions[callID]
    return s, ok
}

func (m *CallManager) remove(callID string) {
    m.mu.Lock()
    defer m.mu.Unlock()
    delete(m.sessions, callID)
}

func (m *CallManager) count() int {
    m.mu.RLock()
    defer m.mu.RUnlock()
    return len(m.sessions)
}
```

**Alternative: `sync.Map`** — built-in concurrent map. Optimized for "write once, read many" (call sessions are created once and mostly read). `sync.Map.Range` is safe for iteration during shutdown. Slightly less ergonomic than a typed map + mutex but eliminates lock management.

**Do NOT use channels for CallManager**: A channel-based actor pattern (all map access via a serializing goroutine) creates a single-goroutine bottleneck. Under concurrent call handling it reduces throughput. Mutex is the right choice here.

---

### 5. /health and /metrics with net/http + prometheus/client_golang

The Go standard library `net/http` handles both endpoints. No framework needed.

```go
import (
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promauto"
    "github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
    activeCalls = promauto.NewGauge(prometheus.GaugeOpts{
        Name: "audio_dock_active_calls",
        Help: "Number of currently active SIP call sessions",
    })
    callsTotal = promauto.NewCounter(prometheus.CounterOpts{
        Name: "audio_dock_calls_total",
        Help: "Total SIP calls received",
    })
    wsReconnectsTotal = promauto.NewCounter(prometheus.CounterOpts{
        Name: "audio_dock_ws_reconnect_attempts_total",
        Help: "Total WebSocket reconnect attempts",
    })
    rtpRxTotal = promauto.NewCounter(prometheus.CounterOpts{
        Name: "audio_dock_rtp_packets_rx_total",
        Help: "Total RTP packets received from callers",
    })
    rtpTxTotal = promauto.NewCounter(prometheus.CounterOpts{
        Name: "audio_dock_rtp_packets_tx_total",
        Help: "Total RTP packets sent to callers",
    })
)

// HTTP server setup
mux := http.NewServeMux()
mux.Handle("/metrics", promhttp.Handler())
mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "application/json")
    fmt.Fprintf(w, `{"registered":%v,"activeCalls":%d}`,
        registered.Load(), callManager.count())
})
srv := &http.Server{Addr: ":8080", Handler: mux}
go srv.ListenAndServe()
```

**Key points:**
- `promauto` registers counters/gauges automatically with the default registry — no manual `prometheus.MustRegister()` calls.
- `promhttp.Handler()` wraps the default gatherer and returns a `http.Handler` — drop-in.
- Use `atomic.Bool` for the `registered` flag; it is read on every `/health` request and written from the SIP registration goroutine.
- The HTTP server runs in its own goroutine. On shutdown, call `srv.Shutdown(ctx)` with a timeout.

---

### 6. Silence Injection During WS Reconnect: ticker goroutine pattern

```go
func (s *CallSession) startSilenceTicker(ctx context.Context) context.CancelFunc {
    tickCtx, cancel := context.WithCancel(ctx)
    silencePkt := buildRTPSilence(s.rtp) // 160 bytes of 0xFF + 12-byte header
    go func() {
        ticker := time.NewTicker(20 * time.Millisecond)
        defer ticker.Stop()
        for {
            select {
            case <-tickCtx.Done():
                return
            case <-ticker.C:
                s.rtp.Send(silencePkt)
            }
        }
    }()
    return cancel // caller calls cancel() when reconnect succeeds or call ends
}
```

**Key points:**
- `time.NewTicker` is the idiomatic Go replacement for `setInterval`. Call `ticker.Stop()` on exit to release resources.
- `context.WithCancel` provides clean cancellation without the Node.js problem of storing interval handles that could be accessed after disposal.
- The cancel function is returned and stored on the session struct. Calling it from `terminateSession` or on reconnect success is safe from any goroutine.
- `time.Ticker` fires approximately every 20ms. Go's runtime scheduler and OS timer resolution may introduce ±1-2ms jitter. At 8kHz PCMU, this is negligible — the remote jitter buffer handles it.

---

### 7. SIGTERM Handling with os/signal

```go
ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
defer stop()

// Main service loop...

// Shutdown sequence triggered by <-ctx.Done()
<-ctx.Done()
log.Info("shutdown signal received — terminating calls")

shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()

var wg sync.WaitGroup
for _, session := range callManager.getAllSessions() {
    wg.Add(1)
    go func(s *CallSession) {
        defer wg.Done()
        s.Terminate(shutdownCtx) // sends SIP BYE + WS stop
    }(session)
}
wg.Wait()

// Send SIP UNREGISTER (REGISTER with Expires: 0)
if err := unregister(shutdownCtx, sipClient); err != nil {
    log.Warn("UNREGISTER failed", "err", err)
}

httpServer.Shutdown(shutdownCtx)
```

**Key points:**
- `signal.NotifyContext` (Go 1.16+) is the modern idiom — no manual channel or `signal.Notify` needed.
- `context.WithTimeout` on shutdown bounds the cleanup time. Kubernetes sends SIGKILL after `terminationGracePeriodSeconds` (default 30s) — keep shutdown well under that.
- `sync.WaitGroup` coordinates parallel BYE sends across all active sessions.
- Double-signal (second SIGTERM or SIGINT while shutting down) should force immediate exit: call `stop()` again or handle the second signal on the original `ctx`.

---

## MVP Definition

### This is a Rewrite, Not a New Product

The MVP is full 1:1 feature parity with v1.0, plus the two new observability endpoints. There is no "launch with less" — the service must be a drop-in replacement.

### Launch With (v2.0 — Go rewrite)

- [ ] SIP REGISTER + digest auth + automatic re-REGISTER goroutine — SIP registration layer
- [ ] Accept inbound SIP INVITE, full lifecycle (100/180/200/ACK/BYE/CANCEL) — call handling layer
- [ ] SDP offer parsing + SDP answer generation (PCMU + telephone-event PT 113) — media negotiation
- [ ] RTP socket per call with goroutine read loop — audio receive
- [ ] RTP RFC 3550 header construction + WriteToUDP — audio send
- [ ] RFC 4733 DTMF: End-bit detection, 3-packet deduplication by timestamp — DTMF passthrough
- [ ] gorilla/websocket per-call connection with writePump/readPump goroutine pair — WS client
- [ ] Twilio Media Streams: connected + start + media + stop + dtmf events — WS protocol
- [ ] Inbound WS media → decode base64 → chunk → RTP send — WS-to-RTP bridge
- [ ] WS reconnect loop: exponential backoff 1s/2s/4s, 30s budget — resilience
- [ ] Silence ticker goroutine during WS reconnect — comfort noise
- [ ] `wsReconnecting` atomic flag gating RTP forward — reconnect gate
- [ ] CallManager: sync.RWMutex + map[string]*CallSession — concurrency
- [ ] 200 OK retransmit until ACK (RFC 3261 §13.3.1.4) — timer doubling T1→T2
- [ ] SIGTERM graceful shutdown: BYE all + UNREGISTER — shutdown
- [ ] GET /health: `{"registered":bool,"activeCalls":N}` — observability
- [ ] GET /metrics: 5 Prometheus counters/gauges via promhttp.Handler() — observability
- [ ] Environment variable config (same names as v1.0) with fail-fast — configuration
- [ ] Static Go binary Docker image (FROM scratch or distroless) — packaging

### Add After Validation (v2.x)

- [ ] `mark` event support — trigger: AI backend uses barge-in
- [ ] `clear` message support — trigger: AI backend uses interruption
- [ ] SIP OPTIONS keepalive — trigger: observing silent registration failures in production
- [ ] `diago` higher-level API evaluation — trigger: RTP layer proves complex to maintain

### Future Consideration (v3+)

- [ ] Outbound call initiation — different use case, different milestone
- [ ] Multi-codec support (G.722, Opus) — transcoding belongs in WS consumer
- [ ] SRTP media encryption — explicitly out of scope per PROJECT.md

---

## Feature Prioritization Matrix

| Feature | User Value | Implementation Cost (Go) | Priority |
|---------|------------|--------------------------|----------|
| SIP REGISTER + digest auth | HIGH | MEDIUM | P1 |
| SIP INVITE lifecycle (100/180/200/ACK/BYE) | HIGH | HIGH | P1 |
| SDP offer parse + answer build | HIGH | MEDIUM | P1 |
| RTP socket + read goroutine | HIGH | HIGH | P1 |
| RTP RFC 3550 header parse/build | HIGH | MEDIUM | P1 |
| gorilla/websocket per-call writePump+readPump | HIGH | MEDIUM | P1 |
| Twilio protocol (connected/start/media/stop) | HIGH | LOW | P1 |
| WS → RTP (inbound media decode) | HIGH | MEDIUM | P1 |
| WS reconnect + silence ticker | HIGH | MEDIUM | P1 |
| CallManager (sync.RWMutex + map) | HIGH | LOW | P1 |
| SIGTERM graceful shutdown | MEDIUM | LOW | P1 |
| GET /health | MEDIUM | LOW | P1 |
| GET /metrics (Prometheus) | MEDIUM | LOW | P1 |
| RFC 4733 DTMF PT 113 passthrough | MEDIUM | MEDIUM | P1 |
| Static Docker binary (FROM scratch) | MEDIUM | LOW | P1 |
| Automatic re-REGISTER goroutine | HIGH | LOW | P1 |
| 200 OK retransmit timer (T1→T2) | MEDIUM | LOW | P1 |
| mark / clear event support | LOW | LOW | P2 |
| SIP OPTIONS keepalive | MEDIUM | MEDIUM | P2 |
| diago higher-level API | LOW | HIGH (eval needed) | P3 |

---

## Edge Cases the Go Implementation Must Handle

| Edge Case | What Happens | Go Mitigation |
|-----------|--------------|---------------|
| INVITE retransmission (same Call-ID arrives twice during setup) | Without dedup: allocate two RTP sockets, attempt two WS connections | `pendingInvites sync.Map` set before first async op, cleared in deferred cleanup |
| CANCEL arrives while INVITE is being processed (async gap) | Without gate: send 200 OK to a cancelled INVITE | `cancelledInvites sync.Map`; check after each `await`-equivalent (channel receive) |
| BYE arrives while WS reconnect goroutine is sleeping | Without check: zombie reconnect creates new WS after call is dead | Check `sessions.Load(callID)` at top of each reconnect attempt loop iteration |
| RTP send after socket closed | `use of closed network connection` error | Recover from `conn.WriteToUDP` error; check `ctxDone` before each send |
| gorilla/websocket concurrent write from RTP goroutine + reconnect goroutine | Frame corruption / panic | All WS writes go through `sendCh` → `writePump`. Never call `conn.WriteMessage` directly. |
| Port exhaustion (all RTP ports in range in use) | Service cannot allocate socket for new call → must reject with 503 | Return error from `allocateRTPSocket`, translate to 503 in INVITE handler |
| WS `sendCh` full when RTP goroutine tries to send | Drop packet silently (acceptable — same policy as v1.0) | Non-blocking send: `select { case ch<-msg: default: counter.Inc() }` |
| Shutdown while reconnect goroutine is in backoff sleep | Reconnect goroutine blocks for up to 4s after SIGTERM | Reconnect loop must `select` on `ctx.Done()` alongside `time.After(delay)` |
| SIP 401 challenge on re-REGISTER after network restore | Re-registration silently fails if digest challenge not handled | Always use `DoDigestAuth` in the registration retry path, not just the initial path |
| `net.UDPConn.ReadFromUDP` blocks forever if remote never sends | Goroutine leak | Always set `SetReadDeadline` with periodic renewal inside the read loop |

---

## Twilio Media Streams Protocol Reference

Exact JSON payloads — unchanged from v1.0 (protocol is external spec, not language-dependent).

### Messages sent by this bridge TO the WebSocket server

```json
{"event":"connected","protocol":"Call","version":"1.0.0"}

{
  "event": "start",
  "sequenceNumber": "1",
  "start": {
    "streamSid": "MZ<32-hex-chars>",
    "accountSid": "",
    "callSid": "CA<32-hex-chars>",
    "tracks": ["inbound", "outbound"],
    "customParameters": {
      "From": "<sip-from-uri>",
      "To": "<sip-to-uri>",
      "sipCallId": "<sip-call-id>"
    },
    "mediaFormat": {
      "encoding": "audio/x-mulaw",
      "sampleRate": 8000,
      "channels": 1
    }
  },
  "streamSid": "MZ<32-hex-chars>"
}

{
  "event": "media",
  "sequenceNumber": "3",
  "media": {
    "track": "inbound",
    "chunk": "1",
    "timestamp": "20",
    "payload": "<base64-encoded-mulaw-audio>"
  },
  "streamSid": "MZ<32-hex-chars>"
}

{
  "event": "stop",
  "sequenceNumber": "100",
  "stop": {
    "accountSid": "",
    "callSid": "CA<32-hex-chars>"
  },
  "streamSid": "MZ<32-hex-chars>"
}

{
  "event": "dtmf",
  "streamSid": "MZ<32-hex-chars>",
  "sequenceNumber": "5",
  "dtmf": {
    "track": "inbound_track",
    "digit": "5"
  }
}
```

### Messages received FROM the WebSocket server

```json
{
  "event": "media",
  "streamSid": "MZ<32-hex-chars>",
  "media": {
    "payload": "<base64-encoded-mulaw-audio>"
  }
}
```

### Audio format specifics (unchanged from v1.0)

- Encoding: `audio/x-mulaw` (G.711 mu-law / PCMU)
- Sample rate: 8000 Hz, 1 channel
- Payload encoding: Base64, raw mulaw bytes only — no WAV header
- RTP frame: 20ms = 160 bytes raw = ~216 chars base64
- RTP payload type: 0 (PCMU), 113 (telephone-event/8000 — sipgate-specific)
- `sequenceNumber`: monotonic integer string, increments per WS event of any type
- `chunk`: monotonic integer string, increments per media event only
- `timestamp` in media event: milliseconds from call start (NOT RTP timestamp)
- `streamSid` generated by this bridge: `MZ` + `crypto/rand` 16 bytes hex
- `callSid` generated by this bridge: `CA` + `crypto/rand` 16 bytes hex

---

## Go-Specific Pitfalls for These Features

| Feature | Pitfall | Prevention |
|---------|---------|------------|
| gorilla/websocket concurrent writes | Frame corruption if more than one goroutine calls WriteMessage | writePump pattern — all writes through buffered channel |
| `net.UDPConn` read loop | ReadFromUDP blocks forever; context cancellation has no effect | SetReadDeadline with periodic renewal inside loop |
| sync.RWMutex + map | Forgetting RLock on read path | Use `sync.Map` to eliminate lock management entirely, or add a `countSessions()` accessor that takes RLock |
| context propagation to goroutines | Goroutine started without ctx → leaks on shutdown | Every goroutine launched must accept `context.Context` and check `ctx.Done()` |
| time.Ticker resource leak | Ticker.Stop() not called → goroutine and channel leak | Defer `ticker.Stop()` immediately after `time.NewTicker()` |
| sipgo CSeq on re-REGISTER | Reusing the same `*sip.Request` object for re-REGISTER without incrementing CSeq | Use `ClientRequestRegisterBuild` option in `TransactionRequest` — it auto-increments CSeq |
| `encoding/json` + per-packet allocation | `json.Marshal` allocates for every RTP frame (50/sec/call) | Pre-build JSON templates as byte slices with format strings; only substitute `payload` field via `fmt.Sprintf` or manual string concatenation |
| RTP buffer reuse | Allocating new `[]byte` per packet in read loop | Allocate one `[1500]byte` buffer at goroutine start; pass `buf[:n]` slices to handlers (do NOT store them — they are overwritten on next read) |
| Shutdown race: BYE sent after socket closed | `conn.WriteToUDP` on closed connection during shutdown | Check `ctx.Err()` before each outbound send; recover from write errors gracefully |

---

## Sources

- sipgo v1.2.0 release notes (HIGH confidence — official GitHub releases): https://github.com/emiago/sipgo/releases
- sipgo pkg.go.dev documentation (HIGH confidence — official): https://pkg.go.dev/github.com/emiago/sipgo
- sipgo client.go — DoDigestAuth, ClientRequestRegisterBuild (HIGH confidence — source): https://github.com/emiago/sipgo/blob/main/client.go
- diago pkg.go.dev — higher-level VoIP abstraction over sipgo (MEDIUM confidence): https://pkg.go.dev/github.com/emiago/diago
- diago GitHub — AudioReader/AudioWriter pattern (MEDIUM confidence): https://github.com/emiago/diago
- gorilla/websocket concurrency rules (HIGH confidence — official docs): https://pkg.go.dev/github.com/gorilla/websocket
- gorilla/websocket read/write pump pattern (HIGH confidence — official example): https://github.com/gorilla/websocket/blob/main/examples/chat/README.md
- gorilla/websocket concurrent write panic issue (HIGH confidence): https://github.com/gorilla/websocket/issues/913
- coder/websocket (formerly nhooyr.io/websocket) — modern alternative (MEDIUM confidence): https://github.com/coder/websocket
- Go wiki: Mutex or Channel? (HIGH confidence — official Go wiki): https://go.dev/wiki/MutexOrChannel
- sync.Map documentation (HIGH confidence — official): https://pkg.go.dev/sync#Map
- prometheus/client_golang promhttp (HIGH confidence — official): https://pkg.go.dev/github.com/prometheus/client_golang/prometheus/promhttp
- promauto for automatic counter registration (HIGH confidence — official): https://pkg.go.dev/github.com/prometheus/client_golang/prometheus/promauto
- Go os/signal NotifyContext (HIGH confidence — official): https://pkg.go.dev/os/signal#NotifyContext
- Go by Example: Signals (HIGH confidence — official): https://gobyexample.com/signals
- Go by Example: Tickers (HIGH confidence — official): https://gobyexample.com/tickers
- Twilio Media Streams WebSocket Messages (HIGH confidence — official Twilio docs): https://www.twilio.com/docs/voice/media-streams/websocket-messages
- Go net.UDPConn documentation (HIGH confidence — official): https://pkg.go.dev/net#UDPConn
- Go Graceful Shutdown patterns (MEDIUM confidence — multiple community sources agree): https://victoriametrics.com/blog/go-graceful-shutdown/

---
*Feature research for: SIP-to-WebSocket audio bridge — Go rewrite (audio-dock v2.0)*
*Researched: 2026-03-03*
