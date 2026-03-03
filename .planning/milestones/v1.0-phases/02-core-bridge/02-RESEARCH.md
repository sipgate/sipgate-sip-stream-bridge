# Phase 2: Core Bridge - Research

**Researched:** 2026-03-03
**Domain:** Raw SIP/SDP over UDP, RTP audio (PCMU/telephone-event), WebSocket (ws), Twilio Media Streams wire protocol, Node.js graceful shutdown
**Confidence:** HIGH — all core technical domains are stable, well-documented RFCs or verified official docs; Twilio Media Streams event format fetched directly from official Twilio docs

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions

**Pre-answer ringing behavior**
- Send `100 Trying` → `180 Ringing` → attempt WebSocket connection → `200 OK` or `503`
- WebSocket connect timeout: **2 seconds** — send 503 if WS not connected within 2s
- Forward audio immediately after WS `open` event — no application-level handshake wait
- RTP audio arriving during the 180 Ringing / WS connect window is **dropped** (not buffered)

**DTMF transport**
- RFC 2833 / RFC 4733 telephone-event carried in RTP stream (payload type 101)
- SDP answer includes `a=rtpmap:101 telephone-event/8000` and `a=fmtp:101 0-16`
- Emit one `dtmf` event per keypress — triggered on the RTP telephone-event packet with `End=true`
- **PCMU only** — SDP answer offers only PCMU (payload type 0); if caller offers no PCMU, send `488 Not Acceptable Here`

**Call & Stream ID format**
- `callSid`: `CA` + 32 random hex chars (Twilio-style, e.g. `CA3a2b1c...`)
- `streamSid`: `MZ` + 32 random hex chars (Twilio-style, e.g. `MZf4e3d2...`)
- SIP Call-ID is included as `sipCallId` in `start.customParameters` (alongside `From`, `To`, `Call-ID`) for sipgate log correlation
- Both IDs are generated fresh per call at INVITE time

**Graceful shutdown**
- Handle both **SIGTERM** and **SIGINT** with the same handler (production + dev Ctrl+C)
- Sequence: send SIP BYE to all active calls **and** close their WebSocket connections **in parallel** → then send SIP UNREGISTER
- **5 second drain timeout**: if cleanup is not complete in 5s, force `process.exit(0)`
- Each active call's `stop` event is sent to the WebSocket before closing the WS connection

### Claude's Discretion
- RTP port pool implementation details (counter vs pool, RTCP port handling)
- Exact SDP body format (as long as PCMU + telephone-event are correctly advertised)
- Internal CallSession data structure
- Error logging verbosity for individual RTP packet drops

### Deferred Ideas (OUT OF SCOPE)
None — discussion stayed within phase scope
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| SIP-03 | Service accepts inbound SIP INVITE and negotiates PCMU (G.711 mu-law 8kHz) codec | SDP offer/answer pattern; PCMU payload type 0; 488 if PCMU absent; 100/180/200 response construction |
| SIP-04 | Service rejects inbound INVITE with 503 if target WebSocket cannot be connected | WS 2-second timeout before 200 OK; send 503 Service Unavailable if timeout fires |
| SIP-05 | Service sends SIP BYE when call ends from either side | BYE construction from dialog state (From, To with tags, Call-ID, CSeq+1); send to remote Contact URI |
| WSB-01 | Service sends `connected` event after WS connection is established for a call | Twilio protocol: `{event:"connected", protocol:"Call", version:"1.0.0"}` |
| WSB-02 | Service sends `start` event with streamSid, callSid, tracks, mediaFormat before forwarding audio | Twilio protocol: `start` with tracks:["inbound","outbound"], mediaFormat:{encoding:"audio/x-mulaw",sampleRate:8000,channels:1} |
| WSB-03 | Service forwards inbound RTP audio as `media` events (base64 mulaw payload) | Strip 12-byte RTP header; base64-encode remaining payload; emit `media` JSON |
| WSB-04 | Service sends `stop` event when SIP call ends | Twilio protocol: `{event:"stop",stop:{callSid,accountSid},streamSid}` |
| WSB-05 | Service receives `media` events from WS and converts them to outbound RTP to caller | base64-decode payload; prepend 12-byte RTP header; dgram.send to remote RTP address:port |
| WSB-06 | Call metadata (From, To, SIP Call-ID) included in `start.customParameters` | `customParameters:{From,To,sipCallId}` extracted from INVITE headers |
| WSB-07 | Service forwards DTMF digits as `dtmf` events to WS | RFC 4733 telephone-event; payload type 101; End bit in byte 1; digit in byte 0; emit on End=true |
| CON-01 | Multiple simultaneous calls each have independent WS connections | CallSession Map keyed by Call-ID; each entry owns its own `dgram.Socket` and `WebSocket` instance |
| CON-02 | Per-call RTP sockets cleaned up after call ends (no file descriptor leak) | `socket.close()` in CallSession.dispose(); verify with lsof/fd-count check |
| LCY-01 | On SIGTERM/SIGINT: SIP BYE all active calls, UNREGISTER, close all WS before exiting | SIGTERM+SIGINT handler; Promise.all(BYE+WS.close); then UNREGISTER; 5s drain timeout; process.exit(0) |
</phase_requirements>

---

## Summary

Phase 2 builds the complete bidirectional audio bridge on top of Phase 1's raw-UDP SIP foundation. The existing `src/sip/userAgent.ts` implements SIP over a raw `dgram` UDP socket — not SIP.js — so "Custom SessionDescriptionHandler" in the roadmap means extending that socket's `message` handler to parse and respond to INVITE/ACK/BYE. Every component is built from Node.js built-ins (`dgram`, `crypto`) plus the already-installed `ws` package.

The five plans decompose cleanly: SIP/SDP handler (parse INVITE SDP, generate answer SDP, send provisional + final responses), RTP handler (bind a dgram socket per call, strip/add 12-byte headers), WS client (open a `ws` WebSocket per call, serialize Twilio protocol events), Audio Bridge (stateless base64 conversion layer connecting RTP payload ↔ WS media event), and Call Manager (owns the CallSession Map, orchestrates INVITE lifecycle, fail-fast 2-second WS pre-connect, graceful SIGTERM/SIGINT shutdown). There are no new npm dependencies required — `ws` is already installed.

The Twilio Media Streams wire protocol is well-documented and stable (HIGH confidence). The RTP header format (RFC 3550) and PCMU/telephone-event formats (RFC 4733) are stable RFCs. The only tricky area is correctly constructing SIP dialog state (From-tag, To-tag, remote Contact URI) for sending BYE — this must use the values from the INVITE/200 OK exchange, not re-derive them.

**Primary recommendation:** Extend the existing dgram `message` handler in `src/sip/userAgent.ts` to dispatch INVITE/ACK/BYE to a `CallManager` via callback — CallManager owns all call state and coordinates the five sub-components. Keep each component in its own module under its domain directory (`src/sip/`, `src/rtp/`, `src/ws/`).

---

## Standard Stack

### Core

| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| node:dgram | Node.js 22 built-in | UDP sockets for SIP (existing) and per-call RTP | Already used in Phase 1; zero deps; native performance |
| node:crypto | Node.js 22 built-in | `randomBytes` for callSid/streamSid generation | Already used in Phase 1; `crypto.randomBytes(16).toString('hex')` = 32 hex chars |
| ws | 8.19.0 (already installed) | Per-call WebSocket client to WS backend | Already in dependencies; production-grade RFC 6455 |
| pino | 10.3.1 (already installed) | Per-call child logger with callId+streamSid bindings | Already established OBS-01 pattern; `createChildLogger` helper exists |

### Supporting

| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| node:buffer | Node.js 22 built-in | `Buffer.readUInt8`, `readUInt16BE`, `readUInt32BE` for RTP header parsing | RTP handler only; read big-endian fields from RTP packets |
| node:timers | Node.js 22 built-in | `setTimeout` for 2-second WS connect timeout and 5-second shutdown drain | Call Manager WS pre-connect timeout and shutdown timer |

### Alternatives Considered

| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| Raw RTP header construction (Buffer) | `rtp.js` npm library | rtp.js adds a dep; RTP header is 12 fixed bytes — trivial to read/write with Buffer; do not add a dependency |
| Raw SDP string generation | `sdp-transform` npm | sdp-transform adds a dep; Phase 2 SDP is a fixed template with only port/IP substitution; do not add a dependency |
| Manual WS protocol serialization | Twilio Media Streams SDK | No official Node.js SDK for the server side of Twilio Media Streams; the protocol is simple JSON; hand-roll is correct here |

**Installation:** No new packages required. All dependencies are already installed (`ws`, `pino`, `zod`, node built-ins).

---

## Architecture Patterns

### Recommended Project Structure

```
src/
├── config/index.ts           # Unchanged from Phase 1
├── logger/index.ts           # Unchanged from Phase 1
├── sip/
│   ├── userAgent.ts          # EXTEND: add dispatchInvite/dispatchBye/dispatchAck callback params
│   └── sdp.ts                # NEW: parseSdpOffer(body), buildSdpAnswer(localIp, localPort)
├── rtp/
│   └── rtpHandler.ts         # NEW: createRtpHandler(localPort) — dgram socket per call
├── ws/
│   └── wsClient.ts           # NEW: createWsClient(url, callSession) — one WS per call
├── bridge/
│   └── callManager.ts        # NEW: CallManager class — orchestrates all per-call lifecycle
└── index.ts                  # EXTEND: instantiate CallManager, wire to createSipUserAgent
```

### Pattern 1: Extending the Existing dgram Socket for INVITE Dispatch

**What:** The existing `socket.on('message')` handler in `src/sip/userAgent.ts` currently handles only SIP responses (1xx/2xx/4xx to REGISTER). Phase 2 adds parsing for SIP requests (INVITE, ACK, BYE) and dispatches them to the CallManager via injected callbacks.

**When to use:** All inbound SIP message handling — do NOT open a second UDP socket.

**Example:**

```typescript
// src/sip/userAgent.ts — extend the message handler
socket.on('message', (buf, rinfo) => {
  const raw = buf.toString();
  const firstLine = raw.split('\r\n')[0];

  if (firstLine.startsWith('SIP/2.0')) {
    // Response — existing REGISTER handling (unchanged)
    handleRegisterResponse(raw);
  } else if (firstLine.startsWith('INVITE')) {
    callbacks.onInvite?.(raw, rinfo);
  } else if (firstLine.startsWith('ACK')) {
    callbacks.onAck?.(raw, rinfo);
  } else if (firstLine.startsWith('BYE')) {
    callbacks.onBye?.(raw, rinfo);
  } else if (firstLine.startsWith('OPTIONS')) {
    // Respond 200 OK to keepalive OPTIONS
    sendOptionsOk(raw, rinfo);
  }
});
```

### Pattern 2: SIP INVITE Response Sequence

**What:** RFC 3261 requires copying Via, From, To, Call-ID, CSeq from the INVITE into every response. The To header gets a `tag=` parameter added starting with 180 Ringing. The Contact header in 200 OK identifies our address for subsequent in-dialog requests (ACK, BYE).

**When to use:** Constructing 100 Trying, 180 Ringing, 200 OK, 488, 503 responses to INVITE.

**Example:**

```typescript
// Source: RFC 3261 §8.2.6 and §12.1.1
function buildInviteResponse(p: {
  status: number;
  reason: string;
  viaHeader: string;
  fromHeader: string;
  toHeader: string;       // without tag for 100; with tag for 180+
  callId: string;
  cseqHeader: string;
  localIp: string;
  localPort: number;
  sipUser: string;
  sipDomain: string;
  body?: string;
}): string {
  const lines = [
    `SIP/2.0 ${p.status} ${p.reason}`,
    `Via: ${p.viaHeader}`,
    `From: ${p.fromHeader}`,
    `To: ${p.toHeader}`,
    `Call-ID: ${p.callId}`,
    `CSeq: ${p.cseqHeader}`,
  ];
  if (p.status === 200) {
    lines.push(`Contact: <sip:${p.sipUser}@${p.localIp}:${p.localPort}>`);
    lines.push('Content-Type: application/sdp');
    lines.push(`Content-Length: ${Buffer.byteLength(p.body ?? '')}`);
    lines.push('');
    lines.push(p.body ?? '');
  } else {
    lines.push('Content-Length: 0', '');
  }
  return lines.join('\r\n') + '\r\n';
}
```

### Pattern 3: SDP Offer Parsing

**What:** Extract remote RTP address and port from SDP body of the INVITE. Check for `PCMU` (payload type 0) in the `m=audio` line. Regex is sufficient for our constrained input (sipgate sends well-formed SDP).

**When to use:** Inside INVITE handler, before generating 180 Ringing.

**Example:**

```typescript
// Source: RFC 4566 §5.14 (m= line format), §5.7 (c= line format)
// Pattern: "m=audio <port> RTP/AVP <pt1> <pt2> ..."
// Pattern: "c=IN IP4 <addr>"
function parseSdpOffer(sdpBody: string): {
  remoteIp: string;
  remotePort: number;
  hasPcmu: boolean;
} | null {
  const mLine = sdpBody.match(/^m=audio\s+(\d+)\s+RTP\/AVP\s+([\d ]+)/m);
  const cLine = sdpBody.match(/^c=IN IP4\s+([\d.]+)/m);
  if (!mLine || !cLine) return null;
  const payloadTypes = mLine[2].split(' ');
  return {
    remoteIp: cLine[1],
    remotePort: parseInt(mLine[1], 10),
    hasPcmu: payloadTypes.includes('0'),
  };
}
```

### Pattern 4: SDP Answer Generation

**What:** Build an SDP answer advertising PCMU and telephone-event, using our local RTP port and `SDP_CONTACT_IP`. The answer MUST use the same session-level connection address as the media-level `c=` line.

**When to use:** Building the 200 OK body.

**Example:**

```typescript
// Source: RFC 4566 (SDP), RFC 3551 §4.5.14 (PCMU), RFC 4733 (telephone-event)
function buildSdpAnswer(localIp: string, localRtpPort: number): string {
  const lines = [
    'v=0',
    `o=audio-dock 0 0 IN IP4 ${localIp}`,
    's=audio-dock',
    `c=IN IP4 ${localIp}`,
    't=0 0',
    `m=audio ${localRtpPort} RTP/AVP 0 101`,
    'a=rtpmap:0 PCMU/8000',
    'a=rtpmap:101 telephone-event/8000',
    'a=fmtp:101 0-16',
    'a=ptime:20',
    'a=sendrecv',
  ];
  return lines.join('\r\n') + '\r\n';
}
```

### Pattern 5: RTP Header Reading and Writing

**What:** Every RTP packet has a 12-byte fixed header (+ possible CSRC list). For PCMU audio from sipgate, the header will be exactly 12 bytes (no CSRC, no extension). Payload type 0 = PCMU audio, type 101 = telephone-event.

**When to use:** RTP Handler — strip header to get mulaw payload; add header when sending audio from WS back to caller.

**Example:**

```typescript
// Source: RFC 3550 §5.1 (RTP fixed header fields)
// Byte layout:
//   Byte 0:  V(2) P(1) X(1) CC(4)
//   Byte 1:  M(1) PT(7)
//   Bytes 2-3:  Sequence Number (uint16 big-endian)
//   Bytes 4-7:  Timestamp (uint32 big-endian)
//   Bytes 8-11: SSRC (uint32 big-endian)
//   Bytes 12+: Payload

function parseRtpHeader(buf: Buffer): {
  version: number;
  paddingBit: boolean;
  extensionBit: boolean;
  csrcCount: number;
  marker: boolean;
  payloadType: number;
  sequenceNumber: number;
  timestamp: number;
  ssrc: number;
  headerLength: number;
} {
  const byte0 = buf.readUInt8(0);
  const byte1 = buf.readUInt8(1);
  const csrcCount = byte0 & 0x0f;
  const extensionBit = (byte0 & 0x10) !== 0;
  const headerLength = 12 + csrcCount * 4 + (extensionBit ? 4 + buf.readUInt16BE(14) * 4 : 0);
  return {
    version: (byte0 >> 6) & 0x03,
    paddingBit: (byte0 & 0x20) !== 0,
    extensionBit,
    csrcCount,
    marker: (byte1 & 0x80) !== 0,
    payloadType: byte1 & 0x7f,
    sequenceNumber: buf.readUInt16BE(2),
    timestamp: buf.readUInt32BE(4),
    ssrc: buf.readUInt32BE(8),
    headerLength,
  };
}

function buildRtpHeader(p: {
  payloadType: number;
  sequenceNumber: number;
  timestamp: number;
  ssrc: number;
}): Buffer {
  const header = Buffer.alloc(12);
  header.writeUInt8(0x80, 0);                    // V=2, P=0, X=0, CC=0
  header.writeUInt8(p.payloadType & 0x7f, 1);    // M=0, PT=payloadType
  header.writeUInt16BE(p.sequenceNumber, 2);
  header.writeUInt32BE(p.timestamp, 4);
  header.writeUInt32BE(p.ssrc, 8);
  return header;
}
```

### Pattern 6: RFC 4733 Telephone-Event DTMF Detection

**What:** When a telephone-event RTP packet arrives (payload type 101), the 4-byte payload encodes the digit, End bit, and duration. Emit a `dtmf` event only on the last packet of a keypress (End bit = 1).

**When to use:** RTP Handler message processing, after stripping the 12-byte header.

**Example:**

```typescript
// Source: RFC 4733 §2.3 — telephone-event payload format
// Byte 0: Event (0-9 = digits, 10=*, 11=#, 12-15=A-D)
// Byte 1: bit7=E(End), bit6=R(reserved), bits5-0=Volume (-dBm0)
// Bytes 2-3: Duration (uint16 big-endian, in timestamp units)

const DTMF_DIGITS: Record<number, string> = {
  0: '0', 1: '1', 2: '2', 3: '3', 4: '4',
  5: '5', 6: '6', 7: '7', 8: '8', 9: '9',
  10: '*', 11: '#', 12: 'A', 13: 'B', 14: 'C', 15: 'D',
};

function parseTelephoneEvent(payload: Buffer): {
  digit: string;
  isEnd: boolean;
} | null {
  if (payload.length < 4) return null;
  const eventCode = payload.readUInt8(0);
  const flags = payload.readUInt8(1);
  const isEnd = (flags & 0x80) !== 0;
  const digit = DTMF_DIGITS[eventCode];
  if (digit === undefined) return null;
  return { digit, isEnd };
}
```

### Pattern 7: Twilio Media Streams Wire Protocol

**What:** All WS messages are JSON strings. The sequence is: `connected` → `start` → N×`media` → `stop`. The WS server also receives `media` (and optionally `dtmf`) events from this service. Track an incrementing `sequenceNumber` and `chunk` counter per call.

**When to use:** All WebSocket message serialization.

**Example:**

```typescript
// Source: https://www.twilio.com/docs/voice/media-streams/websocket-messages (verified 2026-03-03)

function makeConnectedEvent(): string {
  return JSON.stringify({
    event: 'connected',
    protocol: 'Call',
    version: '1.0.0',
  });
}

function makeStartEvent(p: {
  streamSid: string;
  callSid: string;
  from: string;
  to: string;
  sipCallId: string;
  sequenceNumber: number;
}): string {
  return JSON.stringify({
    event: 'start',
    sequenceNumber: String(p.sequenceNumber),
    start: {
      streamSid: p.streamSid,
      accountSid: '',      // not applicable for audio-dock; empty string
      callSid: p.callSid,
      tracks: ['inbound', 'outbound'],
      customParameters: {
        From: p.from,
        To: p.to,
        sipCallId: p.sipCallId,
      },
      mediaFormat: {
        encoding: 'audio/x-mulaw',
        sampleRate: 8000,
        channels: 1,
      },
    },
    streamSid: p.streamSid,
  });
}

function makeMediaEvent(p: {
  streamSid: string;
  sequenceNumber: number;
  chunk: number;
  timestamp: number;
  payload: Buffer;  // raw mulaw bytes (no RTP header)
}): string {
  return JSON.stringify({
    event: 'media',
    sequenceNumber: String(p.sequenceNumber),
    media: {
      track: 'inbound',
      chunk: String(p.chunk),
      timestamp: String(p.timestamp),
      payload: p.payload.toString('base64'),
    },
    streamSid: p.streamSid,
  });
}

function makeStopEvent(p: {
  streamSid: string;
  callSid: string;
  sequenceNumber: number;
}): string {
  return JSON.stringify({
    event: 'stop',
    sequenceNumber: String(p.sequenceNumber),
    stop: {
      accountSid: '',
      callSid: p.callSid,
    },
    streamSid: p.streamSid,
  });
}

function makeDtmfEvent(p: {
  streamSid: string;
  sequenceNumber: number;
  digit: string;
}): string {
  return JSON.stringify({
    event: 'dtmf',
    streamSid: p.streamSid,
    sequenceNumber: String(p.sequenceNumber),
    dtmf: {
      track: 'inbound_track',
      digit: p.digit,
    },
  });
}
```

### Pattern 8: WS Client with 2-Second Connect Timeout

**What:** Before sending 200 OK to the INVITE, open a WebSocket to the configured backend and wait up to 2 seconds for the `open` event. If the timer fires first, send 503 and abort the call.

**When to use:** Call Manager INVITE handling — this is the fail-fast guard (SIP-04).

**Example:**

```typescript
// Source: ws v8 API (official GitHub README, verified 2026-03-03)
import { WebSocket } from 'ws';

function connectWithTimeout(url: string, timeoutMs: number): Promise<WebSocket> {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(url);
    const timer = setTimeout(() => {
      ws.terminate();
      reject(new Error(`WS connect timeout after ${timeoutMs}ms`));
    }, timeoutMs);

    ws.once('open', () => {
      clearTimeout(timer);
      resolve(ws);
    });

    ws.once('error', (err) => {
      clearTimeout(timer);
      reject(err);
    });
  });
}
```

### Pattern 9: SIGTERM / SIGINT Graceful Shutdown

**What:** Register both signals with the same async handler. Drain all active calls (send BYE + stop event + close WS in parallel), then UNREGISTER, then exit. A 5-second hard timeout forces exit if cleanup stalls.

**When to use:** `src/index.ts` — wire after `createSipUserAgent` resolves.

**Example:**

```typescript
// Source: Node.js process.on('SIGTERM') docs + RFC 3261 §8.1.1.3 (UNREGISTER with Expires:0)
async function shutdown(callManager: CallManager, sipHandle: SipHandle): Promise<void> {
  const drainTimeout = setTimeout(() => {
    log.warn({ event: 'shutdown_forced' }, 'Shutdown drain timeout — forcing exit');
    process.exit(0);
  }, 5000);

  try {
    await callManager.terminateAll(); // BYE + stop event + WS.close in parallel
    await sipHandle.unregister();     // REGISTER with Expires:0, Contact:*
  } finally {
    clearTimeout(drainTimeout);
    process.exit(0);
  }
}

process.on('SIGTERM', () => shutdown(callManager, sipHandle));
process.on('SIGINT',  () => shutdown(callManager, sipHandle));
```

### Pattern 10: SIP BYE Construction

**What:** A BYE sent from us (UAS) to end a call must use the dialog state established during the INVITE exchange: From with our tag, To with the caller's tag, the same Call-ID, CSeq incremented from INVITE, and must be sent to the remote Contact URI (not the address the INVITE came from).

**When to use:** Call termination from our side or on SIGTERM.

**Example:**

```typescript
// Source: RFC 3261 §15.1.2 (UAS BYE), §8.1.1 (Request construction within a dialog)
function buildBye(p: {
  remoteTarget: string;   // Contact URI from caller's INVITE
  localIp: string;
  localPort: number;
  fromUri: string;        // our SIP URI
  fromTag: string;        // our local tag (assigned at 180 Ringing time)
  toUri: string;          // caller's URI from INVITE From header
  toTag: string;          // caller's from-tag (from INVITE From: tag=xxx)
  callId: string;
  cseq: number;           // next CSeq value
}): string {
  const branch = `z9hG4bK${randomHex(6)}`;
  const lines = [
    `BYE ${p.remoteTarget} SIP/2.0`,
    `Via: SIP/2.0/UDP ${p.localIp}:${p.localPort};branch=${branch};rport`,
    `Max-Forwards: 70`,
    `From: <${p.fromUri}>;tag=${p.fromTag}`,
    `To: <${p.toUri}>;tag=${p.toTag}`,
    `Call-ID: ${p.callId}`,
    `CSeq: ${p.cseq} BYE`,
    `Content-Length: 0`,
    '',
  ];
  return lines.join('\r\n') + '\r\n';
}
```

### Pattern 11: UNREGISTER (REGISTER with Expires:0)

**What:** To deregister all bindings, send REGISTER with `Contact: *` and `Expires: 0`. This is the same as Phase 1's REGISTER but with these two header changes.

**When to use:** Graceful shutdown sequence (LCY-01).

**Example:**

```typescript
// Source: RFC 3261 §10.2.2 — "wildcard Contact with Expires:0 removes all bindings"
// Reuse buildRegister() from Phase 1 with expires:0 and contact:'*'
```

### Anti-Patterns to Avoid

- **Opening a second UDP socket for SIP:** The Phase 1 socket on port 5060 MUST handle all SIP traffic. Opening a second socket causes port conflicts and missed messages.
- **Buffering RTP during WS connect window:** CONTEXT.md locked decision — drop, not buffer. Buffering unboundedly fills memory; the backend must handle gaps.
- **Using the INVITE source rinfo for BYE:** Proxies and NAT may route INVITE from a different IP than in-dialog requests. Use the Contact URI from the INVITE for in-dialog BYE routing.
- **Sending 200 OK before WS is connected:** The locked decision is: attempt WS → on open send 200 OK → start forwarding audio. If WS open fires before 200 OK is sent, that is fine; sending 200 OK before WS open means audio would arrive before the connection exists.
- **Sharing RTP socket across calls:** Each call MUST have its own dgram socket on an independent port from the RTP_PORT_MIN..RTP_PORT_MAX range. Cross-call socket reuse causes audio mixing (violates CON-01).
- **Incrementing the same CSeq counter for both REGISTER and BYE:** REGISTER and INVITE/BYE CSeqs are per-dialog. The REGISTER dialog has its own CSeq counter (in Phase 1). Each INVITE call creates an independent dialog with its own CSeq starting at 1.
- **Forgetting to clear the RTP socket's `message` listener before closing:** If the RTP socket emits one final late packet after `socket.close()`, it will try to send over a closed WS connection. Remove or ignore messages after the stop event is sent.

---

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| WebSocket client | Raw `net.Socket` + HTTP upgrade | `ws` (already installed) | RFC 6455 framing, TLS, ping/pong, error handling — hundreds of edge cases |
| Base64 codec | Custom encode/decode loop | `Buffer.toString('base64')` / `Buffer.from(str, 'base64')` | Node.js built-in; zero allocation overhead; no library needed |
| Random ID generation | UUID library | `crypto.randomBytes(16).toString('hex')` (already used in Phase 1) | Already in codebase; produces 32 hex chars; no new dependency |
| SDP full parser | Regex spider for all SDP fields | Single-purpose regex for `m=audio` port and `c=IN IP4` address | Only two fields needed; full SDP parser (sdp-transform) is unnecessary complexity |
| SIP message parsing library | Custom tokenizer | String split + targeted regex (already pattern in Phase 1) | Phase 1 already has `getHeader()` and `parseStatusLine()` helpers; extend this pattern |
| G.711 mulaw codec | Codec transcoding library | None — sipgate sends PCMU and caller sends PCMU; no transcoding needed | PCMU is passed through as raw bytes; the WS backend consumes and produces PCMU directly |

**Key insight:** Phase 2 adds zero new npm dependencies. Every capability — UDP sockets, base64, random bytes, WebSocket — is either built into Node.js or already in `dependencies`. The entire bridge is implemented with stdlib + ws.

---

## Common Pitfalls

### Pitfall 1: SIP Tag Confusion in BYE

**What goes wrong:** The BYE is rejected with 481 Call/Transaction Does Not Exist or delivered to the wrong UA because the From/To tags are swapped or the Contact target URI is wrong.

**Why it happens:** In SIP dialog state: the UAS (us) has a From-tag we generate (our local tag); the UAC (caller) has a From-tag they sent in the INVITE (their local tag, which becomes our To-tag). In a BYE from us: `From: <our-URI>;tag=our-tag`, `To: <caller-URI>;tag=caller-from-tag`. Getting this backwards causes the BYE to not match any dialog at the remote end.

**How to avoid:** When processing the INVITE, store: `{remoteUri, remoteTag}` = From header values; `{localTag}` = generated when sending 180 Ringing. Use `remoteUri;tag=remoteTag` as the `To:` in BYE.

**Warning signs:** Getting `481 Call/Transaction Does Not Exist` in response to BYE. Call stays alive on sipgate's end after we think we've hung up.

### Pitfall 2: RTP Port Pool Exhaustion

**What goes wrong:** Under load, all ports in RTP_PORT_MIN..RTP_PORT_MAX are in use when a new call arrives. The call fails with an obscure dgram bind error instead of a clean 503.

**Why it happens:** Default port range is 100 ports (10000-10099). At 1 port per call (RTCP is not implemented in Phase 2), this supports 100 concurrent calls. But failed/hung calls that didn't clean up leave ports occupied.

**How to avoid:** The RTP port allocator should scan the range sequentially and catch `EADDRINUSE` from `socket.bind()`. On bind failure, try the next port. Log a warn if fewer than 10 ports remain free. Ensure `socket.close()` is called in all call termination paths (normal, error, SIGTERM).

**Warning signs:** `Error: bind EADDRINUSE` in RTP handler; active call count never decreasing after calls end; growing fd count.

### Pitfall 3: WS `open` Race with RTP Arrival

**What goes wrong:** sipgate sends RTP before we've sent the WS `connected` and `start` events, or before the WS `open` event fires. We attempt to send a `media` event on a WS that is still connecting.

**Why it happens:** The locked decision (CONTEXT.md) is to send 200 OK AFTER WS connects — this prevents most RTP-before-WS scenarios. But there is a window between 200 OK being sent and ACK arriving where sipgate may buffer early RTP. The RTP socket should only start forwarding after the `open` event fires.

**How to avoid:** The RTP handler should not start emitting payload events until the WS client emits `ready` (after `connected` and `start` are sent). The locked decision is to drop RTP arriving before this; the RTP handler's message listener should check a `forwarding` flag before sending to WS.

**Warning signs:** `WebSocket is not open: readyState 0 (CONNECTING)` errors in ws send callbacks.

### Pitfall 4: Simultaneous Call State Collision

**What goes wrong:** Two concurrent calls share state (port counter, sequence number, or ssrc) causing audio from one call to appear in another.

**Why it happens:** If the port counter, sequence number, or SSRC generator is module-level (shared singleton) rather than per-CallSession, two calls at the same time can produce conflicting values.

**How to avoid:** All mutable per-call state (port, sequence number, SSRC, WS client, RTP socket) must live inside the `CallSession` object, created fresh for each INVITE. The port counter is the only shared resource — protect it with a simple integer scan in a locked (synchronous) function.

**Warning signs:** Two callers hearing each other's audio; `media` events arriving on wrong WS connection; SSRC conflicts in RTP logs.

### Pitfall 5: dgram Socket Not Closed on WS Error

**What goes wrong:** The WebSocket to the backend closes unexpectedly during a call (not handled by Phase 2 reconnect — that's Phase 3). The RTP socket keeps receiving and silently discards packets (write to closed WS). The dgram socket is never closed, leaking a file descriptor.

**Why it happens:** The `ws` `close` and `error` events are not wired to `callSession.dispose()`. Phase 2 should handle WS close by terminating the call (send BYE, clean up). Phase 3 will replace this with reconnect logic.

**How to avoid:** In the WS client, handle `ws.on('close')` and `ws.on('error')` by calling `callSession.onWsDisconnect()` which sends BYE and calls `dispose()`. This is the correct Phase 2 behavior; Phase 3 changes `onWsDisconnect` to trigger reconnect instead.

**Warning signs:** Active call count never declines after WS backend restart; `lsof` shows growing dgram socket count; error logs show `send` on closed WebSocket.

### Pitfall 6: SIP Request-URI for BYE Routing

**What goes wrong:** BYE is sent to the source IP of the INVITE (`rinfo.address`), but sipgate routes in-dialog requests differently — the BYE must be sent to the IP/port in the `Contact:` header of the INVITE.

**Why it happens:** The INVITE arrives from sipgate's SIP proxy. The `Contact:` header in the INVITE contains the actual address for subsequent in-dialog requests. Sending BYE to `rinfo.address` (the proxy's IP) may work by accident, but it is wrong per RFC 3261 §12.2.

**How to avoid:** Parse the `Contact:` header from the inbound INVITE (regex: `Contact:\s*<([^>]+)>`). Extract the host:port. Send BYE to that target. If no Contact is present, fall back to `rinfo.address:rinfo.port`.

**Warning signs:** BYE is accepted (200 OK returned) from a different IP than expected; no 200 OK returned for BYE (proxy doesn't match dialog).

---

## Code Examples

Verified patterns from official sources:

### RTP Header: Full 12-byte Big-Endian Read

```typescript
// Source: RFC 3550 §5.1 — RTP Fixed Header Fields
// All integers are in network byte order (big-endian)
const buf: Buffer = /* packet from dgram */ Buffer.alloc(172); // 12 header + 160 payload
const version     = (buf[0] >> 6) & 0x03;          // should be 2
const payloadType = buf[1] & 0x7f;                  // 0=PCMU, 101=telephone-event
const seq         = buf.readUInt16BE(2);
const timestamp   = buf.readUInt32BE(4);
const ssrc        = buf.readUInt32BE(8);
const payload     = buf.subarray(12);               // mulaw bytes — no copy
```

### Twilio Media Streams: connected + start Sequence

```typescript
// Source: https://www.twilio.com/docs/voice/media-streams/websocket-messages (HIGH)
// Send immediately after ws.on('open')
ws.send(JSON.stringify({ event: 'connected', protocol: 'Call', version: '1.0.0' }));
ws.send(JSON.stringify({
  event: 'start',
  sequenceNumber: '1',
  start: {
    streamSid,
    accountSid: '',
    callSid,
    tracks: ['inbound', 'outbound'],
    customParameters: { From, To, sipCallId },
    mediaFormat: { encoding: 'audio/x-mulaw', sampleRate: 8000, channels: 1 },
  },
  streamSid,
}));
```

### Inbound WS media Event Handling (WS backend → outbound RTP)

```typescript
// Source: https://www.twilio.com/docs/voice/media-streams/websocket-messages (HIGH)
ws.on('message', (data: Buffer | string) => {
  const msg = JSON.parse(data.toString());
  if (msg.event !== 'media') return;
  const rawPayload = Buffer.from(msg.media.payload, 'base64');
  // Build RTP header and send
  const rtpHeader = buildRtpHeader({
    payloadType: 0,          // PCMU
    sequenceNumber: outSeq++,
    timestamp: outTimestamp,
    ssrc: localSsrc,
  });
  outTimestamp += 160;      // 20ms at 8kHz = 160 samples
  const packet = Buffer.concat([rtpHeader, rawPayload]);
  rtpSocket.send(packet, remoteRtpPort, remoteRtpIp);
});
```

### UNREGISTER All Bindings (Shutdown)

```typescript
// Source: RFC 3261 §10.2.2 — "Contact: * with Expires: 0 removes all bindings"
// Reuse buildRegister from Phase 1 but with:
//   expires: 0
//   contact: '*' (override the Contact line)
// The Phase 1 buildRegister function needs a `contact?: string` override param,
// or build a dedicated buildUnregister() using the same pattern.
```

---

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| SIP.js SessionDescriptionHandler | Raw dgram + manual SDP construction | Phase 1 implemented without SIP.js | Phase 2 must extend the raw SIP parser, not SIP.js |
| Separate RTCP socket per call | Single RTP socket per call (RTCP omitted) | Phase 2 decision | Simpler; RTCP is optional per RFC 3550; add in Phase 3 if needed |
| Buffering audio during WS connect | Drop RTP during connect window | CONTEXT.md locked decision | Simpler; AI backends tolerate a sub-2-second gap at call start |
| One WS per service | One WS per call | Core architecture decision | Required for CON-01 — independent sessions for concurrent calls |

**Deprecated/outdated:**
- SIP.js `SessionDescriptionHandler`: Not applicable — Phase 1 used raw UDP, not SIP.js. Do not introduce SIP.js in Phase 2.
- `x-law` npm package: archived August 2025 per Phase 1 research; not needed (no transcoding in Phase 2).
- `alawmulaw` npm package: not needed — mulaw bytes pass through untouched.

---

## Open Questions

1. **Does sipgate send OPTIONS keepalives to registered UAs?**
   - What we know: Many SIP providers send OPTIONS requests to verify registration liveness. Phase 1's userAgent.ts does not handle OPTIONS.
   - What's unclear: Whether sipgate's trunking product does this in practice.
   - Recommendation: Add an `OPTIONS` handler to the message dispatcher that responds with `200 OK` (same Via/From/To/Call-ID/CSeq copy-back pattern as INVITE responses). This is a one-liner and prevents spurious 400 errors in sipgate's logs. The CONTEXT.md does not explicitly list OPTIONS handling, but it is low-risk to include as defensive code.

2. **RTP port allocator: linear scan vs. pre-allocated pool?**
   - What we know: CONTEXT.md leaves this to Claude's discretion.
   - What's unclear: At high concurrency, a linear scan over 100 ports touching the OS bind API might cause momentary delays.
   - Recommendation: Use a simple counter (`nextPort`) that increments modulo (MAX-MIN) and wraps. If `bind()` fails with EADDRINUSE (port in use from a slow-cleanup call), try the next. This is O(N) worst case but with N=100 it is milliseconds. Pre-allocating a pool of sockets is more complex with marginal benefit at this scale.

3. **SSRC value for outbound RTP (WS → caller)**
   - What we know: The SSRC is a 32-bit synchronization source identifier. For sessions we originate, we choose our own SSRC.
   - What's unclear: Whether sipgate validates that the outbound RTP SSRC matches what was in the SDP (SDP does not carry SSRC, so sipgate should not validate).
   - Recommendation: Generate a random 32-bit SSRC per call using `crypto.randomBytes(4).readUInt32BE(0)`. This is the standard approach.

---

## Sources

### Primary (HIGH confidence)
- Twilio Media Streams WebSocket Messages — all event types with exact field names, fetched directly from Twilio docs: https://www.twilio.com/docs/voice/media-streams/websocket-messages
- RFC 3550 — RTP fixed header fields, byte layout, timestamp semantics: https://www.rfc-editor.org/rfc/rfc3550
- RFC 4733 — telephone-event payload format (4-byte structure: event, E/R/volume, duration): https://www.rfc-editor.org/rfc/rfc4733.html
- RFC 3261 — SIP INVITE response construction, dialog state, BYE, UNREGISTER: https://datatracker.ietf.org/doc/html/rfc3261
- RFC 4566 — SDP format, `m=audio`, `c=IN IP4`, `a=rtpmap`, `a=fmtp`: https://www.rfc-editor.org/rfc/rfc4566
- RFC 3551 — PCMU payload type 0, 8000 Hz clock rate, 1 channel: https://tools.ietf.org/html/rfc3551
- Node.js dgram API — `createSocket`, `bind`, `send`, `close`, `message` event, `rinfo`: https://nodejs.org/api/dgram.html (v22)
- ws npm README — `WebSocket` constructor, `open`/`message`/`close`/`error` events, `ws.send()`, `ws.terminate()`: https://github.com/websockets/ws

### Secondary (MEDIUM confidence)
- RTP timestamp calculation — 20ms ptime at 8kHz = 160 timestamp units per packet: https://lmtools.com/content/rtp-timestamp-calculation
- DTMF RFC 4733 IANA registry — event codes 0-9, 10=*, 11=#: https://www.iana.org/assignments/rtp-parameters
- Twilio DTMF GA announcement — dtmf event format confirmed for bidirectional streams: https://www.twilio.com/en-us/changelog/twilio-media-streams--connect--stream--dtmf-support-now-generall

### Tertiary (LOW confidence)
- None — all critical claims are verified from RFC or official Twilio docs.

---

## Metadata

**Confidence breakdown:**
- Twilio Media Streams wire format: HIGH — fetched directly from official Twilio docs; exact field names verified
- RTP header parsing/construction: HIGH — RFC 3550 is a stable standard; byte offsets are fixed and unambiguous
- RFC 4733 telephone-event format: HIGH — RFC text fetched and byte layout extracted
- SIP INVITE/BYE/UNREGISTER construction: HIGH — RFC 3261 is the definitive SIP spec; extends patterns already proven in Phase 1
- SDP offer/answer pattern: HIGH — RFC 4566 + RFC 3264 (offer/answer model); simple regex extraction verified
- ws library API: HIGH — official GitHub README + package in use since Phase 1
- Node.js dgram API: HIGH — official Node.js 22 docs; in use since Phase 1

**Research date:** 2026-03-03
**Valid until:** 2026-04-03 (30 days — all stable RFCs and library APIs; Twilio Media Streams protocol is long-stable)
