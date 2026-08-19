# Pipecat example

A [Pipecat](https://github.com/pipecat-ai/pipecat) voice agent running against the
bridge — unchanged from a Twilio bot.

Pipecat ships a `TwilioFrameSerializer` that speaks the Twilio Media Streams
protocol. The bridge speaks the same protocol, so a Pipecat bot written for Twilio
runs against the bridge with **no changes to the bot's pipeline**: you point the
bridge at the bot's WebSocket instead of exposing the bot to Twilio, and the audio
path stays on your own infrastructure.

Verified end to end with a real inbound call (Pipecat 1.5.0, unmodified
`TwilioFrameSerializer`, no shim).

## What runs

`server.py` accepts the Media Streams WebSocket and hands it to `bot.py`, which
builds a standard Pipecat pipeline on the `TwilioFrameSerializer`. Two modes,
selected with `BOT_MODE`:

| Mode | Pipeline | Needs |
|------|----------|-------|
| `echo` (default) | `transport.input() -> echo -> transport.output()` | nothing (no API keys) |
| `voice` | `input -> Deepgram STT -> Google LLM -> Cartesia TTS -> output` | Deepgram / Google / Cartesia keys |

`echo` is the quickest way to confirm a call flows end to end; `voice` is a real
voice agent, the same pipeline as Pipecat's own `twilio-chatbot` example.

## Prerequisites

- Python 3.11+
- A sipgate SIP trunk (`SIP_USER` / `SIP_PASSWORD`) with a phone number routed to
  it — see [sipgate trunking](https://www.sipgate.de/trunking)
- Docker, or a locally built bridge (see the [root README](../../README.md#quick-start))

## 1. Start the bot

```bash
pip install "pipecat-ai[websocket]" fastapi uvicorn python-dotenv   # echo mode
# voice mode instead:
# pip install "pipecat-ai[websocket,silero,deepgram,google,cartesia]" fastapi uvicorn python-dotenv

cp env.example .env       # BOT_MODE=echo by default
python server.py          # serves ws://localhost:8765/ws
```

## 2. Point the bridge at the bot

Create a `.env` for the bridge (full variable reference in the
[root README](../../README.md#configuration)):

```env
SIP_USER=1234567t0
SIP_PASSWORD=********
SIP_DOMAIN=sipconnect.sipgate.de
SIP_REGISTRAR=sipconnect.sipgate.de

# Dial the bot's WebSocket directly (no TwiML needed):
WS_TARGET_URL=ws://localhost:8765/ws

# PCMU / mu-law 8 kHz — exactly the framing TwilioFrameSerializer expects:
AUDIO_MODE=twilio

# Behind NAT, set your externally-reachable IP or inbound RTP will not arrive:
# SDP_CONTACT_IP=203.0.113.10
```

```bash
docker run --env-file .env --network host \
  ghcr.io/sipgate/sipgate-sip-stream-bridge-go:latest
```

## 3. Call

Call your sipgate number. The bridge accepts the SIP call, opens a Media Streams
WebSocket to the bot, and forwards `start` / `media` / `dtmf` / `stop`. In `echo`
mode you hear your own voice back; in `voice` mode you talk to the agent. The live
call shows up in the operator UI at `http://localhost:9090/ui`.

## The bot never knows it is not Twilio

The pipeline code is unchanged from a Twilio bot. The bridge provides everything
the serializer needs:

- a `start` event with `streamSid`, `callSid`, and
  `mediaFormat = { encoding: "audio/x-mulaw", sampleRate: 8000, channels: 1 }`;
- inbound `media` as base64 PCMU, and it accepts outbound `media` / `clear`
  frames keyed on `streamSid`;
- DTMF as `dtmf` events.

**Hang-up (optional).** With `auto_hang_up` the serializer POSTs
`Status=completed` to the Twilio REST call resource. The bridge serves that exact
endpoint, so setting `BRIDGE_BASE_URL` / `BRIDGE_ACCOUNT_SID` / `BRIDGE_AUTH_TOKEN`
(see `env.example`) lets the bot hang up the SIP call itself — still no Twilio
account. Left unset, `auto_hang_up` stays off and the call ends when the caller
hangs up.

## Where the audio goes

The media path is: caller → sipgate SIP trunk → bridge → your bot. The audio is
not sent to a third-party media-streams cloud; it stays between the SIP trunk and
your own process.

## License

`bot.py` and `server.py` are derived from Pipecat's `twilio-chatbot` example and
are therefore licensed under BSD-2-Clause (see the SPDX headers), not the MIT
license covering the rest of this repository.
