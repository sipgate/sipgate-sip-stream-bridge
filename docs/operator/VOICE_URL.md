# Voice-URL Webhook Runbook

## Overview

`VOICE_URL` is an alternative to `WS_TARGET_URL` for deployments where each
call needs its own WebSocket endpoint. Instead of a single static URL for every
call, the bridge POSTs Twilio-compatible call metadata to `VOICE_URL` **before
answering the call** and reads the per-call WS URL from the TwiML response.

This is byte-compatible with the [Twilio Media Streams webhook flow](https://www.twilio.com/docs/voice/media-streams).

## When to use this

| `WS_TARGET_URL` | `VOICE_URL` |
|---|---|
| Every call goes to the same bot | Per-call routing (multi-tenant, A/B testing, call-centre queuing) |
| Simple / single-bot deployment | Platform assigns a session WS URL dynamically |
| Lower latency (no webhook round-trip) | Needs call metadata before connecting the WS |

## Configuration

Set **exactly one** of `WS_TARGET_URL` or `VOICE_URL`. Both together, or neither, is a startup error.

```env
# Dynamic mode — comment out WS_TARGET_URL and set VOICE_URL
# WS_TARGET_URL=...
VOICE_URL=https://your-platform.example/twiml/voice
VOICE_METHOD=POST                        # POST (default) or GET
VOICE_FALLBACK_URL=https://backup.example/twiml/voice   # optional
VOICE_FALLBACK_METHOD=POST
VOICE_TIMEOUT_S=5                        # 1–15, default 5
```

## Request the bridge sends

```
POST https://your-platform.example/twiml/voice HTTP/1.1
Content-Type: application/x-www-form-urlencoded
X-Twilio-Signature: <HMAC-SHA1 over AUTH_TOKEN + URL + sorted form params>

CallSid=CAxxxxxxxxxxxxxxxx&AccountSid=ACxxxxxxxx&From=%2B49...&To=%2B49...&CallStatus=ringing&Direction=inbound&ApiVersion=2010-04-01&ForwardedFrom=
```

## Expected response

Your endpoint must return HTTP 200 with `Content-Type: text/xml` (or `application/xml`) and a TwiML body:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Response>
  <Connect>
    <Stream url="wss://your-bot.example/ws/session-123">
      <Parameter name="tenant" value="acme" />
      <Parameter name="language" value="de" />
    </Stream>
  </Connect>
</Response>
```

The `<Parameter>` children are forwarded to your WS backend as entries in the `start.customParameters` map alongside the reserved keys (`From`, `To`, `sipCallId`, `CallSid`, `AccountSid`). Reserved keys win on collision.

## Pre-answer semantics

The Voice-URL fetch happens **after 180 Ringing but before 200 OK**. If the fetch fails (timeout, HTTP 5xx, unparseable TwiML, no `<Stream url=...>`), the bridge sends **503 Service Unavailable** to the caller and the call is rejected cleanly.

Latency budget: `VOICE_TIMEOUT_S` (default 5 s) for primary + fallback combined. Keep your TwiML endpoint fast — it is on the critical path for call setup.

## Fallback logic

1. POST to `VOICE_URL` (primary).
2. On network error **or** HTTP 5xx: if `VOICE_FALLBACK_URL` is set, POST there.
3. On any other HTTP error (4xx): no fallback — reject call with 503 immediately.
4. If fallback also fails: reject call with 503.

## Security

- All outbound POSTs carry `X-Twilio-Signature` signed with `AUTH_TOKEN`.
  Your endpoint should verify this header (Twilio-compatible HMAC-SHA1).
- `VOICE_URL` must be `https://` (enforced at startup and per-request).
  Exception: `http://localhost` and `http://127.0.0.1` are allowed for local dev.

## Metrics

```
sipgate_bridge_voice_fetch_total{outcome="ok|timeout|http_error|twiml_error|fallback_ok"}
```

Alert example (Prometheus):
```yaml
- alert: VoiceUrlFetchErrors
  expr: rate(sipgate_bridge_voice_fetch_total{outcome=~"timeout|http_error|twiml_error"}[5m]) > 0.1
  annotations:
    summary: "Voice-URL webhook errors — calls may be rejected"
```

## Reconnect behaviour

On WS disconnect mid-call, the bridge reconnects using the **same WS URL** that was returned by the Voice-URL webhook for that call. It does **not** re-fetch `VOICE_URL` on reconnect — the session URL is captured at call setup.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Caller hears fast busy / 503 | Voice-URL fetch timed out or returned non-200 |
| Caller hears fast busy / 503 | TwiML missing `<Connect><Stream url=...>` |
| `voice_url_failed` in logs | Network error or fallback also failed |
| `voice_url_fetched` + `ws_connect_failed` | Voice-URL OK but WS endpoint unreachable |

Enable `LOG_LEVEL=debug` for per-request detail (`voice_url_fetched`, `urlUsed: primary|fallback`).
