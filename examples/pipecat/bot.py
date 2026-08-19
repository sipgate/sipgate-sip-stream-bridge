#
# SPDX-License-Identifier: BSD-2-Clause
#

"""Pipecat bot wired to the sipgate SIP Stream Bridge.

Nothing here is bridge-specific: the pipeline and the `TwilioFrameSerializer`
are exactly what a Twilio Media Streams bot uses. The bridge speaks the same
protocol, so the same bot runs unchanged.

Two modes, selected with the `BOT_MODE` env var:

  * `echo`  (default) — a dependency-free pipeline that echoes the caller's audio
    back. Needs no STT/LLM/TTS API keys, so it is the quickest way to prove that
    a call flows end to end over the bridge.
  * `voice`           — a real STT -> LLM -> TTS voice agent (Deepgram / Google /
    Cartesia), identical to Pipecat's own `twilio-chatbot` example.

Hang-up (control plane): with `auto_hang_up` the serializer POSTs
`Status=completed` to the Twilio REST call resource. The bridge serves that exact
endpoint, so pointing `BRIDGE_BASE_URL` at the bridge lets the bot hang up the
SIP call itself — no Twilio account involved. If those vars are unset we keep
`auto_hang_up=False` and the call ends when the caller hangs up.
"""

import os

from fastapi import WebSocket
from loguru import logger

from pipecat.frames.frames import Frame, InputAudioRawFrame, OutputAudioRawFrame
from pipecat.pipeline.pipeline import Pipeline
from pipecat.pipeline.worker import PipelineParams, PipelineWorker
from pipecat.processors.frame_processor import FrameDirection, FrameProcessor
from pipecat.serializers.twilio import TwilioFrameSerializer
from pipecat.transports.websocket.fastapi import (
    FastAPIWebsocketParams,
    FastAPIWebsocketTransport,
)
from pipecat.workers.runner import WorkerRunner

# Twilio Media Streams (and therefore the bridge) is 8 kHz mu-law.
SAMPLE_RATE = 8000


def _build_serializer(stream_sid: str, call_sid: str | None) -> TwilioFrameSerializer:
    """Build the Twilio serializer, optionally routing hang-up to the bridge."""
    base_url = os.getenv("BRIDGE_BASE_URL")  # e.g. http://localhost:9090
    account_sid = os.getenv("BRIDGE_ACCOUNT_SID")
    auth_token = os.getenv("BRIDGE_AUTH_TOKEN")
    auto_hang_up = bool(base_url and account_sid and auth_token)

    if auto_hang_up:
        logger.info(f"auto_hang_up enabled -> bridge control plane at {base_url}")

    return TwilioFrameSerializer(
        stream_sid=stream_sid,
        call_sid=call_sid,
        account_sid=account_sid,
        auth_token=auth_token,
        base_url=base_url,
        params=TwilioFrameSerializer.InputParams(auto_hang_up=auto_hang_up),
    )


class AudioEcho(FrameProcessor):
    """Re-emit inbound audio as outbound audio; pass everything else through."""

    async def process_frame(self, frame: Frame, direction: FrameDirection):
        await super().process_frame(frame, direction)
        if isinstance(frame, InputAudioRawFrame):
            await self.push_frame(
                OutputAudioRawFrame(
                    audio=frame.audio,
                    sample_rate=frame.sample_rate,
                    num_channels=frame.num_channels,
                )
            )
        else:
            await self.push_frame(frame, direction)


async def run_bot(websocket: WebSocket, stream_sid: str, call_sid: str | None):
    mode = os.getenv("BOT_MODE", "echo").lower()
    serializer = _build_serializer(stream_sid, call_sid)

    if mode == "voice":
        await _run_voice_bot(websocket, serializer)
    else:
        await _run_echo_bot(websocket, serializer)


async def _run_echo_bot(websocket: WebSocket, serializer: TwilioFrameSerializer):
    """Zero-dependency proof: caller hears their own audio echoed back."""
    transport = FastAPIWebsocketTransport(
        websocket=websocket,
        params=FastAPIWebsocketParams(
            audio_in_enabled=True,
            audio_out_enabled=True,
            add_wav_header=False,
            serializer=serializer,
        ),
    )

    pipeline = Pipeline([transport.input(), AudioEcho(), transport.output()])
    worker = PipelineWorker(
        pipeline,
        params=PipelineParams(
            audio_in_sample_rate=SAMPLE_RATE,
            audio_out_sample_rate=SAMPLE_RATE,
        ),
    )

    runner = WorkerRunner(handle_sigint=False, force_gc=True)
    await runner.add_workers(worker)
    logger.info("Echo bot running")
    await runner.run()


async def _run_voice_bot(websocket: WebSocket, serializer: TwilioFrameSerializer):
    """A real STT -> LLM -> TTS voice agent (same pipeline as twilio-chatbot)."""
    from pipecat.audio.vad.silero import SileroVADAnalyzer
    from pipecat.frames.frames import LLMRunFrame
    from pipecat.processors.aggregators.llm_context import LLMContext
    from pipecat.processors.aggregators.llm_response_universal import (
        LLMContextAggregatorPair,
        LLMUserAggregatorParams,
    )
    from pipecat.services.cartesia.tts import CartesiaTTSService
    from pipecat.services.deepgram.stt import DeepgramSTTService
    from pipecat.services.google.llm import GoogleLLMService

    transport = FastAPIWebsocketTransport(
        websocket=websocket,
        params=FastAPIWebsocketParams(
            audio_in_enabled=True,
            audio_out_enabled=True,
            add_wav_header=False,
            vad_analyzer=SileroVADAnalyzer(),
            serializer=serializer,
        ),
    )

    stt = DeepgramSTTService(api_key=os.getenv("DEEPGRAM_API_KEY"))
    llm = GoogleLLMService(
        api_key=os.getenv("GOOGLE_API_KEY"),
        settings=GoogleLLMService.Settings(
            system_instruction=(
                "You are a friendly assistant on a phone call. Your output is read "
                "aloud, so answer in one or two short sentences and avoid special "
                "characters."
            ),
        ),
    )
    tts = CartesiaTTSService(
        api_key=os.getenv("CARTESIA_API_KEY"),
        settings=CartesiaTTSService.Settings(
            voice="71a7ad14-091c-4e8e-a314-022ece01c121",
        ),
    )

    context = LLMContext()
    user_aggregator, assistant_aggregator = LLMContextAggregatorPair(
        context,
        user_params=LLMUserAggregatorParams(vad_analyzer=SileroVADAnalyzer()),
    )

    pipeline = Pipeline(
        [
            transport.input(),
            stt,
            user_aggregator,
            llm,
            tts,
            transport.output(),
            assistant_aggregator,
        ]
    )
    worker = PipelineWorker(
        pipeline,
        params=PipelineParams(
            audio_in_sample_rate=SAMPLE_RATE,
            audio_out_sample_rate=SAMPLE_RATE,
            enable_metrics=True,
        ),
    )

    @transport.event_handler("on_client_connected")
    async def on_client_connected(transport, client):
        context.add_message(
            {"role": "developer", "content": "Greet the caller in one sentence."}
        )
        await worker.queue_frames([LLMRunFrame()])

    @transport.event_handler("on_client_disconnected")
    async def on_client_disconnected(transport, client):
        await worker.cancel()

    runner = WorkerRunner(handle_sigint=False, force_gc=True)
    await runner.add_workers(worker)
    logger.info("Voice bot running")
    await runner.run()
