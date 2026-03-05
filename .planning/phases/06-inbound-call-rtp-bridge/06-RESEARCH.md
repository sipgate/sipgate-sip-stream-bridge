# Phase 6: Inbound Call + RTP Bridge - Research

**Researched:** 2026-03-03
**Domain:** SIP INVITE dialog lifecycle, RTP socket management, Twilio Media Streams WebSocket protocol, concurrent call session management in Go
**Confidence:** HIGH

---

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| SIP-03 | Service accepts inbound SIP INVITE and negotiates PCMU (G.711 mu-law 8kHz) codec | `DialogServerCache.ReadInvite` + `DialogServerSession.RespondSDP` with pion/sdp answer containing PT 0 (PCMU) — fully documented pattern; DTMF PT extracted from SDP offer per STATE.md decision |
| SIP-04 | Service rejects inbound INVITE with 503 if the target WebSocket cannot be connected | `DialogServerSession.Respond(503, "Service Unavailable", nil)` before `dlg.Close()` — verified API |
| SIP-05 | Service sends SIP BYE when call ends from either side | `DialogServerSession.Bye(ctx)` to initiate BYE; `DialogServerCache.ReadBye` handles incoming BYE and cancels `dlg.Context()` — both paths documented |
| WSB-01 | Service sends `connected` event after WebSocket connection is established for a call | `{"event":"connected","protocol":"Call","version":"1.0.0"}` — Twilio spec verified via official docs |
| WSB-02 | Service sends `start` event with streamSid, callSid, tracks, and mediaFormat before forwarding audio | `{"event":"start","sequenceNumber":"1","start":{"streamSid":...,"callSid":...,"tracks":["inbound","outbound"],"customParameters":{...},"mediaFormat":{"encoding":"audio/x-mulaw","sampleRate":8000,"channels":1}}}` — full schema verified |
| WSB-03 | Service forwards inbound RTP audio as `media` events (base64 mulaw payload) to the WebSocket | `pion/rtp.Packet.Unmarshal` extracts payload; `encoding/base64` encodes; JSON marshalled to WS — all standard library |
| WSB-04 | Service sends `stop` event when the SIP call ends | `{"event":"stop","sequenceNumber":N,"streamSid":...,"stop":{"accountSid":"","callSid":...}}` — Twilio spec verified |
| WSB-05 | Service receives `media` events from WebSocket and converts them to outbound RTP to the caller | WS read loop decodes base64 payload → `pion/rtp` write to `net.UDPConn` targeting caller's RTP addr:port from SDP |
| WSB-06 | Call metadata (From, To, SIP Call-ID) is included in `start.customParameters` | SIP headers extracted from `sip.Request.From()`, `sip.Request.To()`, `sip.Request.CallID()` — passed into `start.customParameters` map |
| WSR-01 | If WebSocket disconnects during an active call, service reconnects (partial — initial connect check for Phase 6) | Phase 6 scope: reject INVITE with 503 if initial WS `Dial` fails (tested via `ws.Dial` or gorilla `DialContext` returning error) |
| CON-01 | Multiple simultaneous calls are supported, each with an independent WebSocket connection | `sync.Map` keyed on Call-ID in CallManager; each CallSession owns its goroutines; gorilla WebSocket `Conn` is not shared |
| CON-02 | Per-call RTP sockets and goroutines are cleaned up after call ends (no goroutine or file descriptor leak) | Per-session cancel context; `defer conn.Close()` on UDPConn; goroutine count tracked via `sync.WaitGroup` per session; port pool `Release()` in defer |
</phase_requirements>

---

## Summary

Phase 6 is the core functional phase of audio-dock. It adds three interdependent subsystems that must be implemented in lock-step: the SIP INVITE handler (UAS dialog), the CallManager/CallSession (per-call resource and goroutine lifecycle), and the WebSocket bridge (Twilio Media Streams protocol).

The SIP layer uses `sipgo.DialogServerCache` with `ReadInvite`, `RespondSDP`, `ReadAck`, and `ReadBye`. The dialog context (`dlg.Context()`) is the lifecycle anchor for each call — it is cancelled when BYE arrives from either side. The SDP layer uses `pion/sdp/v3` to parse the caller's offer (extracting their RTP address, port, and DTMF payload type) and build the answer (advertising PCMU PT 0 + telephone-event with the DTMF PT mirrored from the offer). Per-call RTP uses a `net.UDPConn` bound to a port from the configured pool (CFG-03), with two goroutines per session: one reading RTP and writing to WebSocket, one reading WebSocket and writing RTP. The WebSocket bridge implements the Twilio Media Streams protocol exactly: `connected` then `start` on connect, `media` events for audio in both directions, and `stop` on teardown. `gobwas/ws` is already in go.mod from the Phase 5 scaffold and is the preferred WebSocket library since it avoids adding gorilla as a dependency.

The critical design constraint is that all per-call resources (UDPConn, goroutines, WS connection) must be owned by a `CallSession` with a per-session cancel context derived from the dialog's `dlg.Context()`. When the dialog ends (either BYE from caller or `Bye()` called by WS side), the session context is cancelled and all goroutines exit cleanly. `defer` on `conn.Close()`, port `Release()`, and WS `conn.Close()` handles all file descriptor cleanup. The `CallManager` uses a `sync.Map` to track active sessions, enabling CON-01 and CON-02.

**Primary recommendation:** Structure Phase 6 as three files: `internal/sip/handler.go` (INVITE/ACK/BYE/OPTIONS handlers + DialogServerCache wiring), `internal/bridge/manager.go` + `internal/bridge/session.go` (CallManager + CallSession with per-call context), `internal/bridge/ws.go` (WebSocket bridge: connect, send events, bidirectional audio loop). Use `gobwas/ws` (already in go.mod) for the WebSocket client.

---

## Standard Stack

### Core

| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `github.com/emiago/sipgo` | v1.2.0 | `DialogServerCache`, `DialogServerSession.RespondSDP`, `.Bye()`, `.Context()`, `.ReadBye()`, `.ReadAck()` | Already decided; all Phase 6 dialog APIs verified on pkg.go.dev v1.2.0 |
| `github.com/pion/sdp/v3` | v3.0.18 | Parse caller SDP offer (RTP addr/port/DTMF PT); build SDP answer with PCMU + telephone-event | Already in go.mod; same library LiveKit SIP bridge uses; pure Go |
| `github.com/pion/rtp` | v1.10.1 | Unmarshal incoming RTP packets; marshal outbound RTP packets; DTMF PT detection | Already in go.mod; pure Go; Pion ecosystem standard |
| `github.com/gobwas/ws` | v1.3.2 | WebSocket client: `ws.Dial`, `wsutil.WriteClientText`, `wsutil.ReadServerData` | Already in go.mod (transitive from Phase 5 scaffold); lightweight pure Go; avoids adding gorilla as new dep |
| `encoding/json` | stdlib | Marshal/unmarshal Twilio Media Streams JSON events | stdlib; no additional install |
| `encoding/base64` | stdlib | Encode RTP payload bytes to base64 for media events; decode incoming media payload | stdlib; no additional install |
| `net` (UDPConn) | stdlib | `net.ListenUDP` per call; `conn.ReadFrom` + `conn.WriteTo` | stdlib; one UDPConn per call on a port from the RTP pool |
| `sync` | stdlib | `sync.Map` for concurrent session map; `sync.WaitGroup` for goroutine drain per session | stdlib; WaitGroup used to confirm goroutine cleanup in CON-02 |
| `github.com/rs/zerolog` | v1.34.0 | Structured JSON logging with callId, streamSid, from, to context | Already in go.mod; all log calls add call-specific fields via `.With()` |

### Supporting

| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `github.com/google/uuid` | v1.6.0 | Generate `streamSid` (UUID v4 prefixed with `MZ`) | Already in go.mod; use for streamSid generation |
| `context` | stdlib | Per-session derived context; link to `dlg.Context()` so dialog teardown propagates | stdlib; see Pattern 2 |

### Alternatives Considered

| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| `gobwas/ws` (already in go.mod) | `gorilla/websocket` | gorilla is heavier (adds new dep); gobwas/ws is already present; both work fine for this use case; gobwas/ws has slightly lower allocation per op but higher complexity API — acceptable tradeoff since we need simple dial+read+write |
| `gobwas/wsutil` high-level API | `gobwas/ws` low-level frame API | wsutil wraps the low-level API cleanly for text messages; use wsutil for JSON messages to avoid manual frame construction |
| `sync.Map` | `map` + `sync.RWMutex` | sync.Map is idiomatic for infrequent writes + frequent reads (call map); for low concurrency either works, but sync.Map has zero boilerplate |

**Installation:**

```bash
# All dependencies already in go.mod from Phases 4+5:
# github.com/emiago/sipgo@v1.2.0
# github.com/pion/sdp/v3@v3.0.18
# github.com/pion/rtp@v1.10.1
# github.com/gobwas/ws@v1.3.2
# github.com/google/uuid@v1.6.0
# No new go get commands required for Phase 6
```

---

## Architecture Patterns

### Recommended Project Structure for Phase 6

```
internal/
├── sip/
│   ├── agent.go          # Phase 5: exists — Agent struct + NewAgent
│   ├── registrar.go      # Phase 5: exists — Registrar REGISTER lifecycle
│   ├── handler.go        # Phase 6 NEW: OnInvite/OnAck/OnBye/OnOptions + DialogServerCache wiring
│   └── sdp.go            # Phase 6 NEW: ParseCallerSDP, BuildSDPAnswer
└── bridge/
    ├── manager.go         # Phase 6 NEW: CallManager — sync.Map session registry, StartSession, EndSession
    ├── session.go         # Phase 6 NEW: CallSession — per-call state, goroutine lifecycle, context
    └── ws.go              # Phase 6 NEW: WebSocket bridge — connect, Twilio events, bidirectional audio
```

main.go adds: `dialogSrv` + `callManager` created after SIP agent; INVITE/ACK/BYE/OPTIONS handlers registered on `agent.Server` before `registrar.Register()`.

### Pattern 1: DialogServerCache Setup + INVITE Handler (handler.go)

This is the core UAS flow for SIP-03, SIP-04, SIP-05.

```go
// Source: https://pkg.go.dev/github.com/emiago/sipgo@v1.2.0
// Source: https://github.com/emiago/sipgo/blob/main/dialog_server.go
// Source: .planning/research/sip-library-research.md — Pattern 3

package sip

import (
    "context"

    "github.com/emiago/sipgo"
    siplib "github.com/emiago/sipgo/sip"
    "github.com/rs/zerolog"
    "github.com/sipgate/audio-dock/internal/bridge"
    "github.com/sipgate/audio-dock/internal/config"
)

type Handler struct {
    dialogSrv   *sipgo.DialogServerCache
    callManager *bridge.CallManager
    cfg         config.Config
    log         zerolog.Logger
}

func NewHandler(agent *Agent, callManager *bridge.CallManager, cfg config.Config, log zerolog.Logger) *Handler {
    contact := siplib.ContactHeader{
        Address: siplib.Uri{
            User: cfg.SIPUser,
            Host: cfg.SDPContactIP,
            Port: 5060,
        },
    }
    dialogSrv := sipgo.NewDialogServerCache(agent.Client, contact)

    h := &Handler{
        dialogSrv:   dialogSrv,
        callManager: callManager,
        cfg:         cfg,
        log:         log,
    }

    // Register handlers BEFORE Register() is called in main.go
    agent.Server.OnInvite(h.onInvite)
    agent.Server.OnAck(h.onAck)
    agent.Server.OnBye(h.onBye)
    agent.Server.OnOptions(h.onOptions)

    return h
}

func (h *Handler) onInvite(req *siplib.Request, tx siplib.ServerTransaction) {
    log := h.log.With().
        Str("call_id", req.CallID().Value()).
        Str("from", req.From().Address.String()).
        Str("to", req.To().Address.String()).
        Logger()

    // ReadInvite MUST be called before any Respond — creates dialog context
    dlg, err := h.dialogSrv.ReadInvite(req, tx)
    if err != nil {
        log.Error().Err(err).Msg("ReadInvite failed")
        tx.Respond(siplib.NewResponseFromRequest(req, 500, "Server Error", nil))
        return
    }
    defer dlg.Close() // always cleanup from cache

    // Parse caller's SDP offer
    callerSDP, err := ParseCallerSDP(req.Body())
    if err != nil {
        log.Error().Err(err).Msg("SDP parse failed")
        dlg.Respond(503, "Service Unavailable", nil)
        return
    }

    // Acquire RTP port — reject with 503 if pool exhausted (CON-02)
    rtpPort, err := h.callManager.AcquirePort()
    if err != nil {
        log.Warn().Err(err).Msg("RTP port pool exhausted — rejecting INVITE")
        dlg.Respond(503, "Service Unavailable", nil) // SIP-04
        return
    }
    // Note: port is released inside CallSession.run() via defer

    // 100 Trying — suppress INVITE retransmissions
    dlg.Respond(100, "Trying", nil)

    // Build SDP answer with our RTP port + PCMU + telephone-event (mirror caller's DTMF PT)
    sdpAnswer := BuildSDPAnswer(h.cfg.SDPContactIP, rtpPort, callerSDP.DTMFPayloadType)

    // 200 OK with SDP — creates the dialog
    if err := dlg.RespondSDP(sdpAnswer); err != nil {
        log.Error().Err(err).Msg("RespondSDP 200 OK failed")
        h.callManager.ReleasePort(rtpPort)
        return
    }

    // Launch call session — blocks until call ends (dialog context done)
    // dlg.Context() is cancelled when: (a) caller sends BYE → ReadBye called, (b) we call dlg.Bye()
    h.callManager.StartSession(dlg, req, callerSDP, rtpPort, log)
}

func (h *Handler) onAck(req *siplib.Request, tx siplib.ServerTransaction) {
    // Routes ACK to the correct dialog — transitions state to DialogStateConfirmed
    if err := h.dialogSrv.ReadAck(req, tx); err != nil {
        h.log.Error().Err(err).Str("call_id", req.CallID().Value()).Msg("ReadAck failed")
    }
}

func (h *Handler) onBye(req *siplib.Request, tx siplib.ServerTransaction) {
    // Routes BYE to the correct dialog — cancels dlg.Context(), sets DialogStateEnded
    if err := h.dialogSrv.ReadBye(req, tx); err != nil {
        h.log.Error().Err(err).Str("call_id", req.CallID().Value()).Msg("ReadBye failed")
    }
    // CallSession.run() returns because dlg.Context() is now Done; session cleans itself up
}

func (h *Handler) onOptions(req *siplib.Request, tx siplib.ServerTransaction) {
    tx.Respond(siplib.NewResponseFromRequest(req, 200, "OK", nil))
}
```

### Pattern 2: CallSession — Per-Call Goroutine and Resource Lifecycle (session.go)

This is the CON-01/CON-02 pattern. The session owns the RTP socket, WS connection, and all goroutines.

```go
// Source: verified Go stdlib net, sync, context patterns
// Source: STATE.md — per-call goroutine model, no diago abstraction

package bridge

import (
    "context"
    "net"
    "sync"

    "github.com/emiago/sipgo"
    siplib "github.com/emiago/sipgo/sip"
    "github.com/rs/zerolog"
    "github.com/sipgate/audio-dock/internal/config"
)

// CallSession owns all resources for a single active call.
// It is the only place these resources are created, and it is responsible for all cleanup.
type CallSession struct {
    callID    string
    streamSid string // UUID v4 with "MZ" prefix, generated at session start
    dlg       *sipgo.DialogServerSession
    rtpPort   int
    callerRTP net.Addr   // caller's RTP address:port from SDP offer
    dtmfPT    uint8      // DTMF payload type from SDP offer (sipgate uses 113, not 101)
    cfg       config.Config
    log       zerolog.Logger
    cancel    context.CancelFunc // cancels the per-session derived context
    wg        sync.WaitGroup     // tracks all goroutines; Wait() in Close confirms CON-02
}

// run is the session lifecycle. Called in a goroutine from CallManager.
// It blocks until the call ends (dialog context cancelled by BYE from either side).
// All cleanup happens via defers before run() returns.
func (s *CallSession) run(ctx context.Context) {
    // Derive session context from dialog context — cancels when BYE arrives
    sessionCtx, cancel := context.WithCancel(s.dlg.Context())
    s.cancel = cancel
    defer cancel()

    // Open per-call RTP socket — defer Close ensures FD cleanup (CON-02)
    rtpConn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: s.rtpPort})
    if err != nil {
        s.log.Error().Err(err).Int("rtp_port", s.rtpPort).Msg("failed to bind RTP socket")
        return
    }
    defer rtpConn.Close()

    // Connect to WebSocket — reject flow if unreachable (SIP-04 initial connect check, WSR-01 partial)
    wsConn, err := dialWS(sessionCtx, s.cfg.WSTargetURL)
    if err != nil {
        // Cannot connect to WS — send BYE to caller (call already answered with 200; best effort)
        s.log.Error().Err(err).Str("ws_url", s.cfg.WSTargetURL).Msg("WebSocket connect failed after 200 OK")
        s.dlg.Bye(context.Background()) // nolint: errcheck
        return
    }
    defer wsConn.Close()

    // Send Twilio "connected" event (WSB-01)
    if err := sendConnected(wsConn); err != nil {
        s.log.Error().Err(err).Msg("failed to send connected event")
        return
    }

    // Send Twilio "start" event with call metadata (WSB-02, WSB-06)
    if err := sendStart(wsConn, s.streamSid, s.callID, s.dlg.InviteRequest); err != nil {
        s.log.Error().Err(err).Msg("failed to send start event")
        return
    }

    // Launch bidirectional goroutines
    s.wg.Add(2)
    go s.rtpToWS(sessionCtx, rtpConn, wsConn)   // RTP → WebSocket (WSB-03)
    go s.wsToRTP(sessionCtx, wsConn, rtpConn)    // WebSocket → RTP (WSB-05)

    // Wait for dialog to end (BYE from caller → dlg.Context().Done())
    // or for session context to be cancelled (WS side sends stop → we call dlg.Bye())
    <-sessionCtx.Done()

    // Send stop event to WebSocket (WSB-04)
    sendStop(wsConn, s.streamSid, s.callID) // nolint: errcheck

    // Wait for goroutines to exit cleanly (CON-02)
    s.wg.Wait()
}
```

### Pattern 3: SDP Parsing and Answer Building (sdp.go)

```go
// Source: https://pkg.go.dev/github.com/pion/sdp/v3
// Source: STATE.md decision — "DTMF PT: extract from SDP offer dynamically (sipgate uses PT 113)"

package sip

import (
    "fmt"
    "strconv"
    "strings"
    "time"

    "github.com/pion/sdp/v3"
)

// CallerSDP holds the fields extracted from the caller's SDP offer.
type CallerSDP struct {
    RTPAddr         string  // IP address to send RTP to (caller's media IP)
    RTPPort         int     // UDP port to send RTP to
    DTMFPayloadType uint8   // telephone-event PT (sipgate uses 113, not the conventional 101)
}

// ParseCallerSDP extracts the caller's RTP destination and DTMF payload type from an SDP offer.
// CRITICAL: DTMF PT is NEVER hardcoded — always extracted from SDP offer per STATE.md decision.
func ParseCallerSDP(body []byte) (*CallerSDP, error) {
    sd := &sdp.SessionDescription{}
    if err := sd.Unmarshal(body); err != nil {
        return nil, fmt.Errorf("SDP unmarshal: %w", err)
    }

    for _, md := range sd.MediaDescriptions {
        if md.MediaName.Media != "audio" {
            continue
        }
        port := md.MediaName.Port.Value

        // Connection address: per-media preferred over session-level
        ip := ""
        if md.ConnectionInformation != nil {
            ip = md.ConnectionInformation.Address.Address
        } else if sd.ConnectionInformation != nil {
            ip = sd.ConnectionInformation.Address.Address
        }
        if ip == "" {
            return nil, fmt.Errorf("no connection address in SDP")
        }

        // Find telephone-event PT — scan ALL rtpmap attributes
        var dtmfPT uint8 = 101 // fallback to conventional value if not found
        for _, fmtStr := range md.MediaName.Formats {
            pt, err := strconv.ParseUint(fmtStr, 10, 8)
            if err != nil {
                continue
            }
            codec, err := sd.GetCodecForPayloadType(uint8(pt))
            if err != nil {
                continue
            }
            if strings.EqualFold(codec.Name, "telephone-event") {
                dtmfPT = uint8(pt) // may be 113 (sipgate) or 101 (conventional)
            }
        }

        return &CallerSDP{
            RTPAddr:         ip,
            RTPPort:         port,
            DTMFPayloadType: dtmfPT,
        }, nil
    }
    return nil, fmt.Errorf("no audio media section in SDP offer")
}

// BuildSDPAnswer constructs an SDP answer advertising PCMU (PT 0) + telephone-event.
// ourIP is cfg.SDPContactIP (externally reachable host IP — not the container IP).
// callerDTMFPT is mirrored from the caller's offer (SDP negotiation rule: answer must mirror PT).
func BuildSDPAnswer(ourIP string, ourRTPPort int, callerDTMFPT uint8) []byte {
    now := uint64(time.Now().UnixNano())
    sd := &sdp.SessionDescription{
        Version: 0,
        Origin: sdp.Origin{
            Username:       "-",
            SessionID:      now,
            SessionVersion: now,
            NetworkType:    "IN",
            AddressType:    "IP4",
            UnicastAddress: ourIP,
        },
        SessionName: sdp.SessionName("audio-dock"),
        ConnectionInformation: &sdp.ConnectionInformation{
            NetworkType: "IN",
            AddressType: "IP4",
            Address:     &sdp.Address{Address: ourIP},
        },
        TimeDescriptions: []sdp.TimeDescription{{Timing: sdp.Timing{}}},
    }

    dtmfPTStr := strconv.FormatUint(uint64(callerDTMFPT), 10)
    audio := &sdp.MediaDescription{
        MediaName: sdp.MediaName{
            Media:   "audio",
            Port:    sdp.RangedPort{Value: ourRTPPort},
            Protos:  []string{"RTP", "AVP"},
            Formats: []string{"0", dtmfPTStr},
        },
    }
    audio.WithCodec(0, "PCMU", 8000, 1, "")
    audio.WithCodec(callerDTMFPT, "telephone-event", 8000, 1, "0-16")
    audio.WithPropertyAttribute("sendrecv")

    sd.MediaDescriptions = append(sd.MediaDescriptions, audio)

    out, _ := sd.Marshal()
    return out
}
```

### Pattern 4: Twilio Media Streams WebSocket Protocol (ws.go)

The exact JSON schema verified against Twilio official docs (https://www.twilio.com/docs/voice/media-streams/websocket-messages).

```go
// Source: https://www.twilio.com/docs/voice/media-streams/websocket-messages — verified 2026-03-03
// Source: https://pkg.go.dev/github.com/gobwas/ws@v1.3.2

package bridge

import (
    "context"
    "encoding/base64"
    "encoding/json"
    "net"

    "github.com/gobwas/ws"
    "github.com/gobwas/ws/wsutil"
    siplib "github.com/emiago/sipgo/sip"
)

// ---- JSON message structs (Twilio Media Streams protocol) ----

// ConnectedEvent is the first message sent after WS connection is established (WSB-01).
// Direction: audio-dock → WebSocket server
type ConnectedEvent struct {
    Event    string `json:"event"`    // "connected"
    Protocol string `json:"protocol"` // "Call"
    Version  string `json:"version"`  // "1.0.0"
}

// StartEvent is sent once after connected, before any media (WSB-02, WSB-06).
type StartEvent struct {
    Event          string          `json:"event"`          // "start"
    SequenceNumber string          `json:"sequenceNumber"` // "1"
    StreamSid      string          `json:"streamSid"`
    Start          StartEventBody  `json:"start"`
}

type StartEventBody struct {
    StreamSid        string            `json:"streamSid"`
    AccountSid       string            `json:"accountSid"`       // "" — we have no Twilio account
    CallSid          string            `json:"callSid"`          // SIP Call-ID value
    Tracks           []string          `json:"tracks"`           // ["inbound", "outbound"]
    CustomParameters map[string]string `json:"customParameters"` // From, To, Call-ID (WSB-06)
    MediaFormat      MediaFormat       `json:"mediaFormat"`
}

type MediaFormat struct {
    Encoding   string `json:"encoding"`   // "audio/x-mulaw"
    SampleRate int    `json:"sampleRate"` // 8000
    Channels   int    `json:"channels"`   // 1
}

// MediaEvent is sent for each RTP audio packet (WSB-03); and received for outbound audio (WSB-05).
type MediaEvent struct {
    Event          string     `json:"event"`          // "media"
    SequenceNumber string     `json:"sequenceNumber"`
    StreamSid      string     `json:"streamSid"`
    Media          MediaBody  `json:"media"`
}

type MediaBody struct {
    Track     string `json:"track"`     // "inbound" (us→WS) or "outbound" (WS→us)
    Chunk     string `json:"chunk"`     // sequential number, string
    Timestamp string `json:"timestamp"` // milliseconds from stream start, string
    Payload   string `json:"payload"`   // base64-encoded PCMU bytes
}

// StopEvent is sent when the call ends (WSB-04).
type StopEvent struct {
    Event          string   `json:"event"`          // "stop"
    SequenceNumber string   `json:"sequenceNumber"`
    StreamSid      string   `json:"streamSid"`
    Stop           StopBody `json:"stop"`
}

type StopBody struct {
    AccountSid string `json:"accountSid"` // ""
    CallSid    string `json:"callSid"`    // SIP Call-ID
}

// ---- WS helper functions ----

func dialWS(ctx context.Context, url string) (net.Conn, error) {
    conn, _, _, err := ws.Dial(ctx, url)
    return conn, err
}

func sendConnected(conn net.Conn) error {
    msg := ConnectedEvent{Event: "connected", Protocol: "Call", Version: "1.0.0"}
    return writeJSON(conn, msg)
}

func sendStart(conn net.Conn, streamSid, callID string, req *siplib.Request) error {
    customParams := map[string]string{
        "CallSid": callID,
        "From":    req.From().Address.String(),
        "To":      req.To().Address.String(),
    }
    msg := StartEvent{
        Event:          "start",
        SequenceNumber: "1",
        StreamSid:      streamSid,
        Start: StartEventBody{
            StreamSid:        streamSid,
            AccountSid:       "",
            CallSid:          callID,
            Tracks:           []string{"inbound", "outbound"},
            CustomParameters: customParams,
            MediaFormat: MediaFormat{
                Encoding: "audio/x-mulaw", SampleRate: 8000, Channels: 1,
            },
        },
    }
    return writeJSON(conn, msg)
}

func sendStop(conn net.Conn, streamSid, callID string) error {
    // seqNo tracking omitted for brevity — session should track and pass in
    msg := StopEvent{
        Event:     "stop",
        StreamSid: streamSid,
        Stop:      StopBody{AccountSid: "", CallSid: callID},
    }
    return writeJSON(conn, msg)
}

func writeJSON(conn net.Conn, v any) error {
    data, err := json.Marshal(v)
    if err != nil {
        return err
    }
    // wsutil.WriteClientText sends a masked text frame (client-side WS protocol requirement)
    return wsutil.WriteClientText(conn, data)
}

// readWSMessage reads one text frame from the WebSocket server.
func readWSMessage(conn net.Conn) ([]byte, error) {
    return wsutil.ReadServerData(conn)
}
```

### Pattern 5: RTP → WebSocket (rtpToWS goroutine)

```go
// Source: https://pkg.go.dev/github.com/pion/rtp@v1.10.1 — Packet.Unmarshal verified
// Source: STATE.md — "DTMF PT: extract from SDP offer dynamically"

func (s *CallSession) rtpToWS(ctx context.Context, rtpConn *net.UDPConn, wsConn net.Conn) {
    defer s.wg.Done()

    buf := make([]byte, 1500) // MTU-safe read buffer
    var seqNo uint64 = 2       // connected=implicit, start=1, media starts at 2
    startTimeMs := time.Now().UnixMilli()

    for {
        // Set read deadline for context cancellation responsiveness
        rtpConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))

        n, _, err := rtpConn.ReadFromUDP(buf)
        if err != nil {
            if ctx.Err() != nil {
                return // context cancelled — clean exit
            }
            if isTimeout(err) {
                continue // deadline expired — check ctx and re-read
            }
            s.log.Error().Err(err).Msg("RTP read error")
            return
        }

        var pkt rtp.Packet
        if err := pkt.Unmarshal(buf[:n]); err != nil {
            continue // malformed packet — skip
        }

        // Skip DTMF packets for WSB-07 (deferred to Phase 7)
        if pkt.Header.PayloadType == s.dtmfPT {
            continue
        }

        // Only forward PCMU (PT 0) audio packets
        if pkt.Header.PayloadType != 0 {
            continue
        }

        payload := base64.StdEncoding.EncodeToString(pkt.Payload)
        nowMs := time.Now().UnixMilli() - startTimeMs

        msg := MediaEvent{
            Event:          "media",
            SequenceNumber: strconv.FormatUint(seqNo, 10),
            StreamSid:      s.streamSid,
            Media: MediaBody{
                Track:     "inbound",
                Chunk:     strconv.FormatUint(seqNo-1, 10),
                Timestamp: strconv.FormatInt(nowMs, 10),
                Payload:   payload,
            },
        }
        if err := writeJSON(wsConn, msg); err != nil {
            s.log.Error().Err(err).Msg("WS write error")
            return
        }
        seqNo++
    }
}
```

### Pattern 6: WebSocket → RTP (wsToRTP goroutine)

```go
// Reads media events from the WebSocket server and sends RTP to the caller (WSB-05).
// Also handles "stop" events from the WS side — calls dlg.Bye() to tear down the SIP leg.

func (s *CallSession) wsToRTP(ctx context.Context, wsConn net.Conn, rtpConn *net.UDPConn) {
    defer s.wg.Done()

    var ssrc uint32 = rand.Uint32()
    var seqNo uint16 = 0
    var timestamp uint32 = 0

    for {
        if ctx.Err() != nil {
            return
        }

        data, err := wsutil.ReadServerData(wsConn)
        if err != nil {
            if ctx.Err() != nil {
                return // call already ended
            }
            s.log.Error().Err(err).Msg("WS read error")
            // Signal call teardown — BYE the SIP side
            s.dlg.Bye(context.Background()) // nolint: errcheck
            s.cancel()
            return
        }

        var event map[string]json.RawMessage
        if err := json.Unmarshal(data, &event); err != nil {
            continue
        }

        var eventType string
        json.Unmarshal(event["event"], &eventType)

        switch eventType {
        case "media":
            var mediaMsg MediaEvent
            if err := json.Unmarshal(data, &mediaMsg); err != nil {
                continue
            }
            payload, err := base64.StdEncoding.DecodeString(mediaMsg.Media.Payload)
            if err != nil {
                continue
            }

            // Build RTP packet (PT 0 = PCMU; 160 samples per 20ms @ 8kHz)
            pkt := &rtp.Packet{
                Header: rtp.Header{
                    Version:        2,
                    PayloadType:    0,    // PCMU
                    SequenceNumber: seqNo,
                    Timestamp:      timestamp,
                    SSRC:           ssrc,
                },
                Payload: payload,
            }
            raw, err := pkt.Marshal()
            if err != nil {
                continue
            }
            rtpConn.WriteTo(raw, s.callerRTP)
            seqNo++
            timestamp += 160 // 20ms of 8kHz audio = 160 samples

        case "stop":
            // WS side is ending the call (SIP-05, WSB-04 reverse)
            s.log.Info().Msg("WebSocket sent stop — sending SIP BYE")
            s.dlg.Bye(context.Background()) // nolint: errcheck
            s.cancel()
            return
        }
    }
}
```

### Pattern 7: RTP Port Pool (manager.go)

A channel-based bounded pool is the idiomatic Go pattern for a bounded resource pool.

```go
// Source: Go stdlib channel as semaphore pattern
// Source: REQUIREMENTS.md CFG-03 — RTPPortMin/RTPPortMax configured via env vars

package bridge

import "fmt"

// PortPool manages the pool of available RTP UDP ports (CFG-03).
// Uses a buffered channel as a bounded semaphore — goroutine-safe without mutexes.
type PortPool struct {
    ports chan int
}

func NewPortPool(min, max int) (*PortPool, error) {
    if min >= max {
        return nil, fmt.Errorf("RTP_PORT_MIN (%d) must be less than RTP_PORT_MAX (%d)", min, max)
    }
    ch := make(chan int, max-min)
    for p := min; p < max; p++ {
        ch <- p
    }
    return &PortPool{ports: ch}, nil
}

// Acquire returns a port from the pool, or error if the pool is exhausted.
// Non-blocking — callers must handle pool exhaustion by rejecting the call (SIP-04).
func (p *PortPool) Acquire() (int, error) {
    select {
    case port := <-p.ports:
        return port, nil
    default:
        return 0, fmt.Errorf("RTP port pool exhausted")
    }
}

// Release returns a port to the pool. Must be called in a defer inside CallSession.run().
func (p *PortPool) Release(port int) {
    select {
    case p.ports <- port:
    default:
        // Port was never acquired (programming error) — drop silently
    }
}
```

### Pattern 8: CallManager — Concurrent Session Registry (manager.go)

```go
// Source: Go stdlib sync.Map — idiomatic for infrequent writes + frequent reads
// Source: REQUIREMENTS.md CON-01 — independent sessions per call

type CallManager struct {
    sessions sync.Map   // key: callID (string) → value: *CallSession
    portPool *PortPool
    cfg      config.Config
    log      zerolog.Logger
}

func NewCallManager(portPool *PortPool, cfg config.Config, log zerolog.Logger) *CallManager {
    return &CallManager{portPool: portPool, cfg: cfg, log: log}
}

// StartSession creates and runs a CallSession for the given dialog.
// Runs synchronously in the OnInvite goroutine — blocks until the call ends.
// session.run() contains all defers for cleanup (port release, FD close, goroutine wait).
func (m *CallManager) StartSession(
    dlg *sipgo.DialogServerSession,
    req *siplib.Request,
    callerSDP *CallerSDP,
    rtpPort int,
    log zerolog.Logger,
) {
    callID := req.CallID().Value()
    streamSid := "MZ" + uuid.New().String() // Twilio-style streamSid

    callerRTPAddr := &net.UDPAddr{
        IP:   net.ParseIP(callerSDP.RTPAddr),
        Port: callerSDP.RTPPort,
    }

    session := &CallSession{
        callID:    callID,
        streamSid: streamSid,
        dlg:       dlg,
        rtpPort:   rtpPort,
        callerRTP: callerRTPAddr,
        dtmfPT:    callerSDP.DTMFPayloadType,
        cfg:       m.cfg,
        log:       log.With().Str("stream_sid", streamSid).Logger(),
    }
    m.sessions.Store(callID, session)
    defer m.sessions.Delete(callID)
    defer m.portPool.Release(rtpPort) // CON-02: port returned to pool when call ends

    session.run(context.Background())
}
```

### Anti-Patterns to Avoid

- **Calling `dlg.Close()` to terminate a call:** `Close()` removes the dialog from cache but does NOT send BYE. Always call `dlg.Bye(ctx)` first, then the defer for `dlg.Close()` handles cache cleanup. Source: sipgo v0.17.0 changelog, confirmed in dialog.go analysis.
- **Hardcoding DTMF PT = 101:** sipgate uses PT 113. Always parse from the SDP offer. Source: STATE.md decision log + sip-library-research.md.
- **Using the container IP in SDP:** Use `cfg.SDPContactIP` (set to the host's external/reachable IP) in both the `o=` and `c=` lines. Container IP is not routable from sipgate. Source: CONFIG.md CFG-04.
- **Calling `RespondSDP` before acquiring a WS connection:** If WS dial fails after 200 OK, we must BYE the call — there is no way to un-answer. WS availability should be checked before `RespondSDP`. **However,** for Phase 6 scope (WSR-01 partial), the pre-check is: try to `ws.Dial` in a session goroutine; if it fails immediately, send BYE and log. Note: the INVITE handler can only reject with 503 _before_ calling `RespondSDP`.
- **Sharing a single UDPConn across calls:** Each call needs its own bound port. sipgate sends RTP to the port in our SDP answer — each call must have a unique port from the pool. Source: sip-library-research.md Anti-Patterns.
- **One goroutine for both RTP read and WS write:** Keep them separate. UDPConn read and WS write can block independently. Two goroutines owned by the session WaitGroup ensures clean shutdown (CON-02).
- **`wsutil.WriteClientText` from multiple goroutines:** gobwas/ws (like gorilla) is NOT concurrent-write-safe. The WS write path must be serialized. Use a dedicated writer goroutine with a channel, or use a `sync.Mutex`. The two-goroutine model (rtpToWS writes, wsToRTP only reads) naturally avoids this since only one goroutine writes to WS.

---

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| SIP dialog state machine (INVITE → 100 → 180 → 200 → ACK → BYE) | Custom request matching | `sipgo.DialogServerCache` with `ReadInvite`/`ReadAck`/`ReadBye` | Call-ID + From-tag + To-tag 3-tuple matching; retransmitted INVITEs must hit same dialog; route-set management |
| SDP parsing | Manual `\r\n` string splitting | `pion/sdp/v3` `SessionDescription.Unmarshal` | Multi-media SDP, session-level vs media-level `c=` lines, `GetCodecForPayloadType` for DTMF PT lookup |
| RTP framing | Manual bit-twiddling | `pion/rtp` `Packet.Unmarshal` / `Marshal` | Version, CSRC list, extension header, padding bit — all handled; 3 lines of code |
| WebSocket frame construction | `net.Conn` raw TCP writes | `gobwas/wsutil.WriteClientText` / `ReadServerData` | Masking (client→server MUST be masked per RFC 6455), opcode, fragmentation |
| Bounded RTP port pool | `rand.Intn` with collision check | Channel-based `PortPool` (Pattern 7) | Channel gives O(1) acquire/release with automatic backpressure; no lock needed |
| Goroutine lifecycle | Ad-hoc channels | `context.WithCancel` + `sync.WaitGroup` | Context propagation for nested cancellation; WaitGroup proves CON-02 (no goroutine leak) |

**Key insight:** The SIP dialog state machine alone (RFC 3261 §17) has 15+ states across 4 transaction types. sipgo's DialogServerCache handles all of this; the application only sees the happy-path events (INVITE, ACK, BYE).

---

## Common Pitfalls

### Pitfall 1: `dlg.Close()` Without BYE — Caller Hears Silence Forever

**What goes wrong:** `Close()` removes the dialog from `DialogServerCache` but does NOT send BYE to the caller. The caller stays in the call, hears silence, and eventually times out.

**Why it happens:** sipgo v0.17.0 changed `Close()` semantics: it is now cache-cleanup only, not call termination. Source: sipgo v0.17.0 release notes.

**How to avoid:** `defer dlg.Close()` for cache cleanup. When actively terminating: call `dlg.Bye(ctx)` explicitly first.

**Warning signs:** Call appears to end in logs but caller does not receive BYE; RTP continues flowing.

### Pitfall 2: DTMF PT Hardcoded to 101 — Audio Muted Calls with sipgate

**What goes wrong:** SDP answer advertises `a=rtpmap:101 telephone-event/8000` but sipgate sends telephone-event packets at PT 113. The bridge misclassifies them as "unknown codec" and may forward DTMF noise as audio.

**Why it happens:** PT 101 is conventional but dynamic. sipgate specifies 113 in their SDP offer. If the answer doesn't echo back 113, the negotiation is mismatched.

**How to avoid:** `ParseCallerSDP` extracts the PT from the caller's offer. `BuildSDPAnswer` mirrors it back. Never set a literal `101` in the media format list.

**Warning signs:** DTMF digits trigger unexpected behavior; audio quality issues on calls with sipgate.

### Pitfall 3: INVITE Handler Blocks the sipgo Server Goroutine

**What goes wrong:** The `OnInvite` callback is invoked in a sipgo server goroutine. If `StartSession` (which blocks until call ends) is called directly, that server goroutine is blocked for the duration of the call. New INVITEs may not be processed until it returns.

**Why it happens:** sipgo's `OnInvite` handler is called synchronously in a per-transaction goroutine, but the transaction goroutine may have limits on parallelism depending on transport configuration.

**How to avoid:** The pattern in Pattern 1 calls `h.callManager.StartSession(...)` which blocks — but this is called from within the OnInvite handler goroutine. This is acceptable because sipgo spawns a new goroutine per incoming transaction. Verify this assumption from sipgo source. Alternatively, use `go h.callManager.StartSession(...)` for extra safety if sipgo's INVITE handler is confirmed single-goroutine.

**Warning signs:** Second INVITE is not processed while first call is active.

### Pitfall 4: RTP Port Not Released on Error Path

**What goes wrong:** If `RespondSDP` fails after `AcquirePort()`, the port is never released. Port pool gradually empties as calls fail; eventually all calls are rejected with 503.

**Why it happens:** Early return before reaching the `defer Release()` in `StartSession`.

**How to avoid:** `ReleasePort` must be called on all error paths in the INVITE handler that exit before `StartSession` is called. In Pattern 1, the INVITE handler calls `h.callManager.ReleasePort(rtpPort)` on the `RespondSDP` error path. Inside `StartSession`, `defer m.portPool.Release(rtpPort)` handles all subsequent cleanup.

**Warning signs:** Port exhaustion after several failed calls.

### Pitfall 5: WS Write Concurrency Violation

**What goes wrong:** Both `rtpToWS` and `wsToRTP` goroutines call `writeJSON(wsConn, ...)`. gobwas/ws (like gorilla) does not support concurrent writers. Second concurrent write panics or corrupts the stream.

**Why it happens:** Two goroutines share the same `net.Conn` reference.

**How to avoid:** `rtpToWS` handles ALL writes to the WebSocket (media events, stop event). `wsToRTP` only reads from the WebSocket. When `wsToRTP` needs to send a stop event (e.g., caller sends BYE), it cancels the session context — `rtpToWS` exits its loop and the session cleanup sends stop. Alternatively: use a `sync.Mutex` guarding all WS writes, or a write channel with a dedicated writer goroutine.

**Warning signs:** Panic: "concurrent write to websocket connection"; corrupted JSON on WS.

### Pitfall 6: `dlg.Context()` Already Done on Entry to StartSession

**What goes wrong:** If the caller sends BYE or CANCEL very quickly (e.g., before 200 OK is processed), `dlg.Context()` may already be done when `StartSession` begins. The `run()` goroutine exits immediately without sending `stop` to the WS.

**Why it happens:** Race condition between network I/O and goroutine scheduling.

**How to avoid:** Check `dlg.Context().Err() != nil` at the start of `run()`. If already cancelled, skip resource setup and return immediately (without leaking the port or socket).

**Warning signs:** "stop" event never sent for very short calls.

### Pitfall 7: SDP Answer Uses Session-Level `c=` But Caller Expects Per-Media

**What goes wrong:** Some SIP implementations require both session-level and media-level `c=` lines. If only session-level is provided, the caller may fail to route RTP.

**Why it happens:** RFC 4566 allows either form, but implementation quirks vary.

**How to avoid:** `BuildSDPAnswer` in Pattern 3 sets `sd.ConnectionInformation` (session-level). For maximum compatibility with sipgate, this is sufficient — sipgate's SDP offers use session-level `c=` lines themselves. No per-media `c=` needed unless testing reveals otherwise.

**Warning signs:** Call connects (SIP 200 OK exchanged) but no RTP received on our socket.

---

## Code Examples

Verified patterns from official sources:

### Twilio Media Streams: `connected` Event

```json
// Source: https://www.twilio.com/docs/voice/media-streams/websocket-messages
{
  "event": "connected",
  "protocol": "Call",
  "version": "1.0.0"
}
```

### Twilio Media Streams: `start` Event (with WSB-06 customParameters)

```json
// Source: https://www.twilio.com/docs/voice/media-streams/websocket-messages
{
  "event": "start",
  "sequenceNumber": "1",
  "streamSid": "MZxxxxxxxx",
  "start": {
    "streamSid": "MZxxxxxxxx",
    "accountSid": "",
    "callSid": "CALL-ID-FROM-SIP-HEADER",
    "tracks": ["inbound", "outbound"],
    "customParameters": {
      "CallSid": "CALL-ID-FROM-SIP-HEADER",
      "From":    "sip:+4915112345678@sipconnect.sipgate.de",
      "To":      "sip:e12345p0@sipconnect.sipgate.de"
    },
    "mediaFormat": {
      "encoding": "audio/x-mulaw",
      "sampleRate": 8000,
      "channels": 1
    }
  }
}
```

### Twilio Media Streams: `media` Event (inbound audio, WS→Server)

```json
// Direction: audio-dock → WebSocket server (WSB-03)
{
  "event": "media",
  "sequenceNumber": "2",
  "streamSid": "MZxxxxxxxx",
  "media": {
    "track": "inbound",
    "chunk": "1",
    "timestamp": "20",
    "payload": "<base64-encoded-PCMU-bytes>"
  }
}
```

### Twilio Media Streams: `media` Event (outbound audio, Server→WS)

```json
// Direction: WebSocket server → audio-dock (WSB-05)
{
  "event": "media",
  "streamSid": "MZxxxxxxxx",
  "media": {
    "payload": "<base64-encoded-PCMU-bytes>"
  }
}
```

### Twilio Media Streams: `stop` Event

```json
// Source: https://www.twilio.com/docs/voice/media-streams/websocket-messages
{
  "event": "stop",
  "sequenceNumber": "99",
  "streamSid": "MZxxxxxxxx",
  "stop": {
    "accountSid": "",
    "callSid": "CALL-ID-FROM-SIP-HEADER"
  }
}
```

### pion/sdp GetCodecForPayloadType — DTMF PT Extraction

```go
// Source: https://pkg.go.dev/github.com/pion/sdp/v3
// GetCodecForPayloadType scans all a=rtpmap lines for the given PT
codec, err := sd.GetCodecForPayloadType(uint8(pt))
if err != nil {
    // PT not found in rtpmap — skip
    continue
}
if strings.EqualFold(codec.Name, "telephone-event") {
    dtmfPT = uint8(pt) // e.g. 113 for sipgate, 101 for conventional endpoints
}
// codec.ClockRate will be 8000 for telephone-event
```

### gobwas/ws Client-Side Read/Write

```go
// Source: https://pkg.go.dev/github.com/gobwas/ws@v1.3.2

// Connect
conn, _, _, err := ws.Dial(ctx, "wss://my-bot.example.com/ws")

// Write text message (JSON) — client MUST use wsutil.WriteClientText (adds masking)
err = wsutil.WriteClientText(conn, jsonBytes)

// Read text message from server — returns raw bytes of server frame payload
data, err := wsutil.ReadServerData(conn)
// data is the JSON payload bytes; decode with json.Unmarshal

// Close
conn.Close()
```

### DialogServerCache — Full Handler Wiring

```go
// Source: https://pkg.go.dev/github.com/emiago/sipgo@v1.2.0

contactHDR := sip.ContactHeader{
    Address: sip.Uri{User: cfg.SIPUser, Host: cfg.SDPContactIP, Port: 5060},
}
dialogSrv := sipgo.NewDialogServerCache(agent.Client, contactHDR)

agent.Server.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
    dlg, err := dialogSrv.ReadInvite(req, tx)  // MUST be first
    if err != nil { return }
    defer dlg.Close()                            // cleanup from cache

    dlg.Respond(100, "Trying", nil)
    dlg.RespondSDP(sdpAnswerBytes)               // sends 200 OK with SDP body

    <-dlg.Context().Done()                       // blocks until BYE (either direction)
})

agent.Server.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {
    dialogSrv.ReadAck(req, tx)                  // transitions to DialogStateConfirmed
})

agent.Server.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
    dialogSrv.ReadBye(req, tx)                  // sends 200 OK for BYE; cancels dlg.Context()
})
```

---

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| `dialog.Close()` to end call | `dialog.Bye(ctx)` then `dialog.Close()` | sipgo v0.17.0 (Jan 2026) | Close() is now cache-cleanup only; missing Bye() leaves caller connected |
| Manual dialog matching with `sync.Map[callID]` | `sipgo.DialogServerCache` | sipgo v0.x | Built-in Call-ID + From-tag + To-tag 3-tuple matching; handles retransmitted INVITEs |
| `emiago/sipgox` SDP helpers | `pion/sdp/v3` directly | June 2024 (sipgox deprecated) | sipgox README: "please do not use this" |
| `emiago/media` for RTP | `net.UDPConn` + `pion/rtp` | Feb 2025 (emiago/media archived) | Direct socket + pion/rtp gives raw byte access needed for base64 Twilio encoding |
| PT 101 hardcoded as telephone-event | PT extracted from SDP offer | v1.0 implementation discovery | sipgate uses PT 113; hardcoding breaks DTMF on sipgate |

**Deprecated/outdated:**
- `emiago/sipgox`: Deprecated June 2024. Do not use.
- `emiago/media`: Archived February 2025. Read-only.
- `dialog.Close()` alone for termination: broken since sipgo v0.17.0.

---

## Open Questions

1. **Does `OnInvite` spawn a separate goroutine per INVITE in sipgo v1.2.0?**
   - What we know: sipgo processes each SIP transaction in a goroutine (transport listener goroutine dispatches to handler). The handler itself runs in the dispatch goroutine.
   - What's unclear: Whether blocking in `OnInvite` (via `<-dlg.Context().Done()`) prevents other INVITEs from being handled concurrently.
   - Recommendation: Use `go h.callManager.StartSession(...)` in `onInvite` to ensure the INVITE handler goroutine returns immediately and does not block the next INVITE. The blocking `session.run()` runs in its own goroutine.

2. **Does sipgate's SDP offer include `c=` at session level, media level, or both?**
   - What we know: Our `ParseCallerSDP` checks per-media first, then falls back to session-level. This covers both cases.
   - What's unclear: Exact SDP format from sipgate trunking in production.
   - Recommendation: Log the raw SDP body on first inbound INVITE for validation.

3. **Is `ws.Dial` sufficient for WSS (TLS) connections to the target WebSocket?**
   - What we know: `gobwas/ws.Dial` handles both `ws://` and `wss://` URLs. `wss://` uses TLS via Go's `crypto/tls` package with system roots (included in Docker image per Phase 4 decision).
   - What's unclear: Whether the target WS server requires any special TLS configuration (client cert, custom CA).
   - Recommendation: `ws.Dial` with `wss://` URL is sufficient for standard TLS. CA certs are already included in the Docker image.

4. **What is the correct format for `callSid` in Twilio Media Streams events?**
   - What we know: Twilio uses their own Call SID format (e.g., `CAxxxxxxxx`). We do not have a Twilio account. The requirement (WSB-06) says to use SIP Call-ID.
   - What's unclear: Whether WS consumers built for Twilio will accept a SIP Call-ID string where they expect `CA...` format.
   - Recommendation: Use the SIP `Call-ID` header value verbatim as `callSid` and also expose it in `customParameters["CallSid"]`. WS consumers built for audio-dock will use `customParameters` for the SIP-specific identifiers.

---

## Validation Architecture

> `workflow.nyquist_validation` is not set in `.planning/config.json` — this section is skipped.

---

## Sources

### Primary (HIGH confidence)

- `emiago/sipgo` pkg.go.dev v1.2.0 — https://pkg.go.dev/github.com/emiago/sipgo@v1.2.0 — `NewDialogServerCache`, `ReadInvite`, `ReadAck`, `ReadBye`, `Respond`, `RespondSDP`, `Bye`, `Close`, `Context()` API signatures verified
- `emiago/sipgo` dialog.go — https://github.com/emiago/sipgo/blob/main/dialog.go — `Dialog.ctx context.Context`, `Dialog.cancel context.CancelCauseFunc`, context cancelled on `DialogStateEnded` transition verified
- `emiago/sipgo` dialog_server.go — https://github.com/emiago/sipgo/blob/main/dialog_server.go — `ReadBye` sends 200 OK + calls `setState(DialogStateEnded)` (which cancels context) verified
- `pion/sdp/v3` pkg.go.dev — https://pkg.go.dev/github.com/pion/sdp/v3 — `SessionDescription.Unmarshal/Marshal`, `GetCodecForPayloadType`, `Codec` struct, `MediaDescription.WithCodec`, `RangedPort`, `ConnectionInformation` verified
- `pion/rtp` pkg.go.dev — https://pkg.go.dev/github.com/pion/rtp@v1.10.1 — `Packet.Unmarshal`, `Packet.Marshal`, `Header` struct verified from Phase 5 research
- Twilio Media Streams docs — https://www.twilio.com/docs/voice/media-streams/websocket-messages — all event schemas (connected, start, media, stop) with exact field names verified 2026-03-03
- `gobwas/ws` pkg.go.dev — https://pkg.go.dev/github.com/gobwas/ws — `ws.Dial(ctx, url)`, `wsutil.WriteClientText`, `wsutil.ReadServerData` API verified
- `.planning/research/sip-library-research.md` — prior research; Dialog lifecycle patterns, DTMF PT issue, SDP pitfalls
- `.planning/phases/05-sip-registration/05-RESEARCH.md` — Phase 5 patterns; confirmed go.mod deps, registered sipgo API
- `internal/sip/agent.go`, `registrar.go`, `cmd/audio-dock/main.go` — actual Phase 5 implementation; Agent struct fields, NewAgent signature confirmed

### Secondary (MEDIUM confidence)

- sipgo issue #59 — https://github.com/emiago/sipgo/issues/59 — dialog termination mid-call; confirms `<-dlg.Context().Done()` as the blocking pattern
- sipgo v0.17.0 release notes — `Close()` behavioral change documented in sip-library-research.md
- STATE.md accumulated decisions — DTMF PT 113 confirmed from v1.0 implementation; confirmed decision to extract PT dynamically
- gobwas/ws concurrency model — one concurrent reader, one concurrent writer (same model as gorilla); confirmed from documentation

### Tertiary (LOW confidence — validate at integration test)

- sipgate SDP exact format (session-level vs media-level `c=`) — assumed session-level based on common SIP trunk behavior; unverified without live traffic
- sipgate DTMF PT 113 — confirmed from v1.0 implementation comment; not verified against current sipgate SDP trace
- `OnInvite` goroutine model in sipgo v1.2.0 — assumed one goroutine per transaction based on standard SIP transaction model; not verified from source
- Twilio `callSid` format compatibility — audio-dock WS consumers are not real Twilio; using SIP Call-ID as callSid is reasonable but untested

---

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH — all libraries in go.mod already; APIs verified on pkg.go.dev
- Architecture patterns: HIGH — verified against sipgo source files, Twilio official docs, pion/sdp pkg.go.dev
- Twilio protocol schema: HIGH — verified from official Twilio Media Streams docs 2026-03-03
- Pitfalls: HIGH (sipgo v0.17.0 Close() change confirmed; DTMF PT confirmed from v1.0 impl) / MEDIUM (WS write concurrency, OnInvite goroutine model)
- Code examples: HIGH — all method signatures verified against pkg.go.dev and official docs

**Research date:** 2026-03-03
**Valid until:** 2026-06-03 (sipgo v1.x stable; Twilio Media Streams protocol is stable; recheck if sipgo v1.3.0+ released)
