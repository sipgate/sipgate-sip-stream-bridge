# test-listener

Minimal Twilio Media Streams WebSocket server for local testing of audio-dock.

## Setup

```bash
npm install   # or: pnpm install / yarn
```

## Usage

```bash
node index.js [port]   # default port: 8080
```

Set `WS_TARGET_URL=ws://localhost:8080` in your audio-dock `.env`, then start the listener before starting audio-dock.

## Modes

Controlled via the `MODE` environment variable (default: `log`):

| Mode | Behaviour |
|------|-----------|
| `log` | Log all events, send no audio back |
| `echo` | Echo caller audio back (loopback test) |
| `timing` | Like `log`, but print inter-arrival time for every media packet (jitter check) |
| `tone` | Send a synthetic sine-wave tone after `start` (simulates a TTS response) |

### Tone mode options

| Variable | Default | Description |
|----------|---------|-------------|
| `TONE_MS` | `3000` | Duration of each tone burst in ms |
| `TONE_HZ` | `440` | Tone frequency in Hz |
| `TONE_REPEAT` | `8` | Repeat interval in seconds (`0` = play once) |

## npm scripts

```bash
npm run log      # MODE=log
npm run echo     # MODE=echo
npm run timing   # MODE=timing
npm run tone     # MODE=tone
```

## Port conflicts

| Service | Default port |
|---------|-------------|
| test-listener (WebSocket) | **8080** |
| audio-dock Go `/health`+`/metrics` | **9090** |
| audio-dock Node `/health`+`/metrics` | **9090** |

Override with `PORT=<n> node index.js` or `node index.js <port>`.
