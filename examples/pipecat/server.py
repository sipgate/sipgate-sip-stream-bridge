#
# SPDX-License-Identifier: BSD-2-Clause
#

"""FastAPI WebSocket server for a Pipecat bot behind the sipgate SIP Stream Bridge.

The bridge emulates the Twilio Media Streams protocol on top of a standard SIP
trunk, so from Pipecat's point of view this looks exactly like an incoming Twilio
Media Stream: a `connected` event, then a `start` event carrying `streamSid` /
`callSid` / `mediaFormat`, then base64 mu-law `media` frames.

We read the first two control messages to learn the stream identifiers, then hand
the socket to a standard Pipecat pipeline built on the `TwilioFrameSerializer` —
the same serializer a Twilio bot uses, unchanged.
"""

import json
import os

from dotenv import load_dotenv
from fastapi import FastAPI, WebSocket
from loguru import logger

from bot import run_bot

load_dotenv(override=True)

app = FastAPI()


@app.get("/")
async def health():
    return {"status": "ok"}


@app.websocket("/ws")
async def websocket_endpoint(websocket: WebSocket):
    await websocket.accept()
    logger.info("WebSocket connection accepted")

    # The bridge (like Twilio) sends `connected` first, then `start` with the
    # stream metadata, before any audio. Read both so we can key the serializer
    # on this call's streamSid / callSid.
    ws_messages = websocket.iter_text()
    await ws_messages.__anext__()  # {"event": "connected", ...}
    start_msg = json.loads(await ws_messages.__anext__())  # {"event": "start", ...}

    start = start_msg["start"]
    stream_sid = start["streamSid"]
    call_sid = start.get("callSid")
    logger.info(
        f"Media stream started: streamSid={stream_sid} callSid={call_sid} "
        f"mediaFormat={start.get('mediaFormat')}"
    )

    try:
        await run_bot(websocket, stream_sid, call_sid)
    except Exception:
        logger.exception("Bot terminated with an error")
    finally:
        try:
            await websocket.close()
        except RuntimeError:
            pass  # already closed by the transport


if __name__ == "__main__":
    import uvicorn

    port = int(os.getenv("PORT", "8765"))
    uvicorn.run(app, host="0.0.0.0", port=port)
