"""Minimal but representative Pipecat call process (Python process-per-call baseline).

Startup = import pipecat+openai, build SileroVADAnalyzer (ONNX load), a Pipeline with
VADProcessor + trivial processors, an OpenAI LLM service object (no network), start the
PipelineWorker via PipelineRunner. READY <monotonic_ns> is printed when the StartFrame
reaches the tail processor, i.e. the pipeline is ready for its first audio frame.
Exits when stdin closes.
"""
import asyncio
import os
import sys
import time

from loguru import logger

logger.remove()  # ponytail: a real bot logs; silenced so stdout carries only READY

import openai  # noqa: F401  (production-relevant import)
from pipecat.audio.vad.silero import SileroVADAnalyzer
from pipecat.frames.frames import Frame, StartFrame
from pipecat.pipeline.pipeline import Pipeline
from pipecat.workers.runner import WorkerRunner
from pipecat.pipeline.worker import PipelineParams, PipelineWorker
from pipecat.processors.audio.vad_processor import VADProcessor
from pipecat.processors.frame_processor import FrameDirection, FrameProcessor
from pipecat.services.openai.llm import OpenAILLMService


class Passthrough(FrameProcessor):
    async def process_frame(self, frame: Frame, direction: FrameDirection):
        await super().process_frame(frame, direction)
        await self.push_frame(frame, direction)


class ReadyTail(Passthrough):
    def __init__(self, out_fd: int, **kw):
        super().__init__(**kw)
        self._fd = out_fd

    async def process_frame(self, frame: Frame, direction: FrameDirection):
        await super().process_frame(frame, direction)
        if isinstance(frame, StartFrame) and self._fd >= 0:
            os.write(self._fd, f"READY {time.monotonic_ns()}\n".encode())
            self._fd = -1


async def run_bot(out_fd: int, stop: asyncio.Event):
    vad = SileroVADAnalyzer(sample_rate=16000)
    llm = OpenAILLMService(api_key="sk-dummy", settings=OpenAILLMService.Settings(model="gpt-4o-mini"))  # constructed, never called
    pipeline = Pipeline([Passthrough(), VADProcessor(vad_analyzer=vad), Passthrough(), llm, ReadyTail(out_fd)])
    task = PipelineWorker(
        pipeline,
        params=PipelineParams(audio_in_sample_rate=16000),
        idle_timeout_secs=None,
    )
    runner = WorkerRunner(handle_sigint=False)
    run = asyncio.create_task(runner.run(task))
    await stop.wait()
    await task.cancel()
    await run


async def main():
    loop = asyncio.get_running_loop()
    stop = asyncio.Event()
    loop.add_reader(sys.stdin.fileno(), lambda: sys.stdin.buffer.read1(4096) or stop.set())
    await run_bot(sys.stdout.fileno(), stop)


if __name__ == "__main__":
    asyncio.run(main())
