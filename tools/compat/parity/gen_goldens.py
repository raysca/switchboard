"""Executes Pipecat components from the pinned pipecat-impl checkout on scripted inputs and
writes the golden JSON under tools/compat/testdata/parity/. Run via `make parity-regen`.

Goldens are the single source of truth for both sides: `pipecat` holds what the reference did;
`intentional_difference.go` holds what go-voice is declared to do instead (with the reason).
The Go tests (tools/compat/parity_fixtures_test.go) run the same inputs through go-voice.
No network, no audio hardware, no wall clock: asyncio runs on a virtual clock.
"""
import asyncio
import json
import os
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.abspath(os.path.join(HERE, "..", "..", ".."))
IMPL = os.path.join(ROOT, "pipecat-impl")
OUT = os.path.join(ROOT, "tools", "compat", "testdata", "parity")
sys.path.insert(0, IMPL)  # for tests.frame_processor_helpers
os.environ.setdefault("LOGURU_LEVEL", "ERROR")

from loguru import logger  # noqa: E402

logger.remove()

REVISION = subprocess.check_output(["git", "-C", IMPL, "rev-parse", "HEAD"], text=True).strip()


class VirtualLoop(asyncio.SelectorEventLoop):
    """Event loop whose clock only moves when every task is idle: instant and deterministic."""

    def __init__(self):
        super().__init__()
        self._vt = 0.0
        real = self._selector.select

        def select(timeout=None):
            if timeout and timeout > 0:
                self._vt += timeout
            return real(0)

        self._selector.select = select

    def time(self):
        return self._vt


def run_virtual(coro_fn):
    loop = VirtualLoop()
    asyncio.set_event_loop(loop)
    try:
        return loop.run_until_complete(coro_fn())
    finally:
        loop.close()


def ms(loop):
    return int(round(loop.time() * 1000))


async def settle():
    for _ in range(8):
        await asyncio.sleep(0)


async def sleep_until(t_ms):
    loop = asyncio.get_running_loop()
    d = t_ms / 1000 - loop.time()
    if d > 0:
        await asyncio.sleep(d)
    await settle()


# --------------------------------------------------------------------------- turn start/stop

def _turn(strategies_start, stop_ms, steps, end_ms):
    from pipecat.frames.frames import (
        BotStartedSpeakingFrame, BotStoppedSpeakingFrame, InterimTranscriptionFrame,
        TranscriptionFrame, VADUserStartedSpeakingFrame, VADUserStoppedSpeakingFrame)
    from pipecat.turns.user_start import (
        MinWordsUserTurnStartStrategy, TranscriptionUserTurnStartStrategy,
        VADUserTurnStartStrategy, WakePhraseUserTurnStartStrategy)
    from pipecat.turns.user_stop import SpeechTimeoutUserTurnStopStrategy
    from pipecat.turns.user_turn_controller import UserTurnController
    from pipecat.turns.user_turn_strategies import UserTurnStrategies
    from pipecat.utils.asyncio.task_manager import TaskManager
    from tests.frame_processor_helpers import frame_processor_setup

    def build(s):
        k = s["kind"]
        if k == "vad":
            return VADUserTurnStartStrategy()
        if k == "min_words":
            return MinWordsUserTurnStartStrategy(min_words=s["min_words"], use_interim=s["use_interim"])
        if k == "transcription":
            return TranscriptionUserTurnStartStrategy(use_interim=s["use_interim"])
        if k == "wake":
            return WakePhraseUserTurnStartStrategy(
                phrases=s["phrases"], timeout=s["timeout_ms"] / 1000, single_activation=s.get("single", False))
        raise ValueError(k)

    async def main():
        loop = asyncio.get_running_loop()
        out = []
        ctrl = UserTurnController(
            user_turn_strategies=UserTurnStrategies(
                start=[build(s) for s in strategies_start],
                stop=[SpeechTimeoutUserTurnStopStrategy(user_speech_timeout=stop_ms / 1000)]),
            user_turn_stop_timeout=1e6)
        await ctrl.setup(frame_processor_setup(TaskManager()))
        await ctrl.start()

        @ctrl.event_handler("on_user_turn_started")
        async def _s(c, strategy, params):
            out.append(f"{ms(loop)}:turn_started")

        @ctrl.event_handler("on_user_turn_stopped")
        async def _e(c, strategy, params):
            out.append(f"{ms(loop)}:turn_stopped")

        for st in steps:
            await sleep_until(st["t"])
            ev = st["ev"]
            f = {"vad_start": lambda: VADUserStartedSpeakingFrame(),
                 "vad_stop": lambda: VADUserStoppedSpeakingFrame(stop_secs=0.2),
                 "bot_start": lambda: BotStartedSpeakingFrame(),
                 "bot_stop": lambda: BotStoppedSpeakingFrame(),
                 "interim": lambda: InterimTranscriptionFrame(text=st["text"], user_id="u", timestamp=""),
                 "final": lambda: TranscriptionFrame(text=st["text"], user_id="u", timestamp="")}[ev]()
            await ctrl.process_frame(f)
            await settle()
        await sleep_until(end_ms)
        await ctrl.cleanup()
        return out

    return run_virtual(main)


VAD = {"kind": "vad"}


def S(t, ev, text=None):
    d = {"t": t, "ev": ev}
    if text is not None:
        d["text"] = text
    return d


def turn_scenarios():
    wake = {"kind": "wake", "phrases": ["hey pipecat"], "timeout_ms": 3000}
    return [
        dict(id="min_words_bot_speaking_needs_threshold", pipecat_module="turns/user_start/min_words_user_turn_start_strategy.py",
             params={"start": [{"kind": "min_words", "min_words": 2, "use_interim": True}], "stop_ms": 600, "end_ms": 3000},
             steps=[S(100, "bot_start"), S(200, "final", "Hello"), S(400, "final", "Hello there!")]),
        dict(id="min_words_bot_silent_single_word_starts", pipecat_module="turns/user_start/min_words_user_turn_start_strategy.py",
             params={"start": [{"kind": "min_words", "min_words": 3, "use_interim": True}], "stop_ms": 600, "end_ms": 3000},
             steps=[S(200, "final", "Hello")]),
        dict(id="min_words_interim_ignored_when_disabled", pipecat_module="turns/user_start/min_words_user_turn_start_strategy.py",
             params={"start": [{"kind": "min_words", "min_words": 1, "use_interim": False}], "stop_ms": 600, "end_ms": 3000},
             steps=[S(200, "interim", "Hello there"), S(500, "final", "Hello there")]),
        dict(id="transcription_interim_starts_turn", pipecat_module="turns/user_start/transcription_user_turn_start_strategy.py",
             params={"start": [{"kind": "transcription", "use_interim": True}], "stop_ms": 600, "end_ms": 3000},
             steps=[S(200, "interim", "hel")]),
        dict(id="transcription_interim_ignored_when_disabled", pipecat_module="turns/user_start/transcription_user_turn_start_strategy.py",
             params={"start": [{"kind": "transcription", "use_interim": False}], "stop_ms": 600, "end_ms": 3000},
             steps=[S(200, "interim", "hel"), S(500, "final", "hello")]),
        dict(id="wake_phrase_punctuation_stripped", pipecat_module="turns/user_start/wake_phrase_user_turn_start_strategy.py",
             params={"start": [wake], "stop_ms": 600, "end_ms": 4000},
             steps=[S(200, "final", "Hello there."), S(600, "final", "Hey, Pipecat!")]),
        dict(id="wake_phrase_split_across_finals", pipecat_module="turns/user_start/wake_phrase_user_turn_start_strategy.py",
             params={"start": [wake], "stop_ms": 600, "end_ms": 4000},
             steps=[S(200, "final", "hey"), S(500, "final", "pipecat what time is it")]),
        dict(id="wake_phrase_blocks_vad_until_awake", pipecat_module="turns/user_start/wake_phrase_user_turn_start_strategy.py",
             params={"start": [wake, VAD], "stop_ms": 600, "end_ms": 6000},
             steps=[S(100, "vad_start"), S(900, "vad_stop"), S(1000, "final", "just chatting"),
                    S(2000, "final", "hey pipecat"), S(2300, "vad_start"), S(2900, "vad_stop")]),
        dict(id="wake_phrase_timeout_returns_to_idle", pipecat_module="turns/user_start/wake_phrase_user_turn_start_strategy.py",
             params={"start": [wake, VAD], "stop_ms": 600, "end_ms": 12000},
             steps=[S(100, "final", "hey pipecat"), S(300, "vad_start"), S(1000, "vad_stop"), S(1200, "final", "what time is it"),
                    S(8000, "vad_start"), S(8500, "vad_stop")]),
        dict(id="wake_phrase_single_activation", pipecat_module="turns/user_start/wake_phrase_user_turn_start_strategy.py",
             params={"start": [dict(wake, single=True), VAD], "stop_ms": 600, "end_ms": 12000},
             steps=[S(100, "final", "hey pipecat"), S(300, "vad_start"), S(1000, "vad_stop"), S(1200, "final", "what time is it"),
                    S(3500, "vad_start"), S(4100, "vad_stop"), S(4200, "final", "again")]),
        dict(id="vad_start_then_speech_timeout_stop", pipecat_module="turns/user_stop/speech_timeout_user_turn_stop_strategy.py",
             params={"start": [VAD], "stop_ms": 600, "end_ms": 5000},
             steps=[S(100, "vad_start"), S(1000, "vad_stop"), S(1300, "final", "hello there")]),
        dict(id="speech_timeout_user_resumes_restarts_clock", pipecat_module="turns/user_stop/speech_timeout_user_turn_stop_strategy.py",
             params={"start": [VAD], "stop_ms": 600, "end_ms": 6000},
             steps=[S(100, "vad_start"), S(1000, "vad_stop"), S(1300, "vad_start"), S(2000, "vad_stop"), S(2100, "final", "hello again")]),
        dict(id="speech_timeout_waits_for_transcript", pipecat_module="turns/user_stop/speech_timeout_user_turn_stop_strategy.py",
             params={"start": [VAD], "stop_ms": 600, "end_ms": 6000},
             steps=[S(100, "vad_start"), S(1000, "vad_stop"), S(2500, "final", "late words")]),
        dict(id="speech_timeout_no_transcript_never_stops", pipecat_module="turns/user_stop/speech_timeout_user_turn_stop_strategy.py",
             params={"start": [VAD], "stop_ms": 600, "end_ms": 6000},
             steps=[S(100, "vad_start"), S(1000, "vad_stop")]),
    ]


# --------------------------------------------------------------------------- idle

def _idle(timeout_ms, steps, end_ms):
    from pipecat.frames.frames import (
        BotStartedSpeakingFrame, BotStoppedSpeakingFrame, UserStartedSpeakingFrame, UserStoppedSpeakingFrame)
    from pipecat.turns.user_idle_controller import UserIdleController
    from pipecat.utils.asyncio.task_manager import TaskManager
    from tests.frame_processor_helpers import frame_processor_setup

    async def main():
        loop = asyncio.get_running_loop()
        out = []
        c = UserIdleController(user_idle_timeout=timeout_ms / 1000)
        await c.setup(frame_processor_setup(TaskManager()))

        @c.event_handler("on_user_turn_idle")
        async def _i(ctrl):
            out.append(f"{ms(loop)}:idle")

        for st in steps:
            await sleep_until(st["t"])
            f = {"bot_start": BotStartedSpeakingFrame, "bot_stop": BotStoppedSpeakingFrame,
                 "user_start": UserStartedSpeakingFrame, "user_stop": UserStoppedSpeakingFrame}[st["ev"]]()
            await c.process_frame(f)
            await settle()
        await sleep_until(end_ms)
        await c.cleanup()
        return out

    return run_virtual(main)


def idle_scenarios():
    p = {"timeout_ms": 3000, "end_ms": 20000}
    return [
        dict(id="idle_fires_after_bot_stops", pipecat_module="turns/user_idle_controller.py", params=p,
             steps=[S(0, "bot_start"), S(1000, "bot_stop")]),
        dict(id="idle_cancelled_when_user_speaks", pipecat_module="turns/user_idle_controller.py", params=p,
             steps=[S(0, "bot_start"), S(1000, "bot_stop"), S(2000, "user_start"), S(2500, "user_stop")]),
        dict(id="idle_cancelled_when_bot_speaks_again", pipecat_module="turns/user_idle_controller.py", params=p,
             steps=[S(0, "bot_start"), S(1000, "bot_stop"), S(2000, "bot_start")]),
        dict(id="idle_suppressed_when_bot_stops_during_user_turn", pipecat_module="turns/user_idle_controller.py", params=p,
             steps=[S(0, "bot_start"), S(500, "user_start"), S(1000, "bot_stop")]),
        dict(id="idle_rearms_after_user_turn_and_next_bot_reply", pipecat_module="turns/user_idle_controller.py", params=p,
             steps=[S(0, "bot_start"), S(1000, "bot_stop"), S(2000, "user_start"), S(2500, "user_stop"),
                    S(3000, "bot_start"), S(4000, "bot_stop")]),
        dict(id="idle_no_clock_before_first_bot_speech", pipecat_module="turns/user_idle_controller.py", params=p, steps=[]),
    ]


# --------------------------------------------------------------------------- user mute

def _mute(kind, steps):
    from pipecat.frames.frames import (
        BotStartedSpeakingFrame, BotStoppedSpeakingFrame, ErrorFrame, FunctionCallCancelFrame,
        FunctionCallFromLLM, FunctionCallResultFrame, FunctionCallsStartedFrame, UserStartedSpeakingFrame)
    from pipecat.turns.user_mute import (
        AlwaysUserMuteStrategy, FirstSpeechUserMuteStrategy, FunctionCallUserMuteStrategy,
        MuteUntilFirstBotCompleteUserMuteStrategy)

    s = {"always": AlwaysUserMuteStrategy, "first_speech": FirstSpeechUserMuteStrategy,
         "function_call": FunctionCallUserMuteStrategy,
         "until_first_bot_complete": MuteUntilFirstBotCompleteUserMuteStrategy}[kind]()

    def frame(st):
        ev = st["ev"]
        if ev == "tool_start":
            return FunctionCallsStartedFrame(function_calls=[FunctionCallFromLLM(
                function_name="f", tool_call_id=st["id"], arguments={}, context=None)])
        return {"probe": lambda: UserStartedSpeakingFrame(), "bot_start": lambda: BotStartedSpeakingFrame(),
                "bot_stop": lambda: BotStoppedSpeakingFrame(),
                "error": lambda: ErrorFrame(error="boom"),
                "tool_result": lambda: FunctionCallResultFrame(function_name="f", tool_call_id=st["id"], arguments={}, result="ok"),
                "tool_cancel": lambda: FunctionCallCancelFrame(function_name="f", tool_call_id=st["id"])}[ev]()

    async def main():
        out, prev = [], False
        for st in steps:
            now = await s.process_frame(frame(st))
            if now != prev:
                out.append(f"{st['t']}:{'muted' if now else 'unmuted'}")
                prev = now
        return out

    return run_virtual(main)


def mute_scenarios():
    mk = lambda kind, sid, steps: dict(id=sid, pipecat_module=f"turns/user_mute/{ {'always': 'always_user_mute_strategy', 'first_speech': 'first_speech_user_mute_strategy', 'function_call': 'function_call_user_mute_strategy', 'until_first_bot_complete': 'mute_until_first_bot_complete_user_mute_strategy'}[kind]}.py",
                                      params={"strategy": kind}, steps=steps)
    return [
        mk("always", "mute_always_follows_bot_speech", [S(0, "probe"), S(100, "bot_start"), S(200, "bot_stop"), S(300, "bot_start"), S(400, "bot_stop")]),
        mk("first_speech", "mute_first_speech_only_first_turn", [S(0, "probe"), S(100, "bot_start"), S(200, "bot_stop"), S(300, "bot_start"), S(400, "bot_stop")]),
        mk("until_first_bot_complete", "mute_until_first_bot_complete_from_start", [S(0, "probe"), S(100, "bot_start"), S(200, "bot_stop"), S(300, "bot_start")]),
        mk("until_first_bot_complete", "mute_until_first_bot_complete_released_by_error", [S(0, "probe"), S(100, "error"), S(200, "probe")]),
        mk("function_call", "mute_function_call_until_result", [S(0, "probe"), S(100, "tool_start") | {"id": "a"}, S(200, "tool_result") | {"id": "a"}]),
        mk("function_call", "mute_function_call_parallel_calls_and_cancel", [S(0, "probe"), S(100, "tool_start") | {"id": "a"}, S(110, "tool_start") | {"id": "b"},
                                                                          S(200, "tool_result") | {"id": "a"}, S(300, "tool_cancel") | {"id": "b"}]),
    ]


# --------------------------------------------------------------------------- DTMF aggregator

def _dtmf(timeout_ms, tail_ms, steps):
    from pipecat.audio.dtmf.types import KeypadEntry
    from pipecat.frames.frames import InputDTMFFrame, InterruptionFrame, TranscriptionFrame
    from pipecat.processors.aggregators.dtmf_aggregator import DTMFAggregator
    from pipecat.tests.utils import SleepFrame, run_test

    pad = {"0": KeypadEntry.ZERO, "1": KeypadEntry.ONE, "2": KeypadEntry.TWO, "3": KeypadEntry.THREE,
           "4": KeypadEntry.FOUR, "5": KeypadEntry.FIVE, "6": KeypadEntry.SIX, "7": KeypadEntry.SEVEN,
           "8": KeypadEntry.EIGHT, "9": KeypadEntry.NINE, "#": KeypadEntry.POUND, "*": KeypadEntry.STAR}

    async def main():
        frames, last = [], 0
        for st in steps:
            if st["t"] > last:
                frames.append(SleepFrame((st["t"] - last) / 1000))
            last = st["t"]
            frames.append(InputDTMFFrame(button=pad[st["digit"]]))
        if tail_ms:
            frames.append(SleepFrame(tail_ms / 1000))
        down, _ = await run_test(DTMFAggregator(timeout=timeout_ms / 1000), frames_to_send=frames)
        out = []
        for f in down:
            if isinstance(f, InterruptionFrame):
                out.append("interrupt")
            elif isinstance(f, TranscriptionFrame):
                out.append("text:" + f.text)
        return out

    return run_virtual(main)


def dtmf_scenarios():
    d = lambda t, digit: {"t": t, "digit": digit}
    p = {"timeout_ms": 100, "tail_ms": 300}
    m = "processors/aggregators/dtmf_aggregator.py"
    return [
        dict(id="dtmf_pound_flushes_including_terminator", pipecat_module=m, params=p,
             steps=[d(0, "1"), d(10, "2"), d(20, "3"), d(30, "#")]),
        dict(id="dtmf_idle_timeout_flushes", pipecat_module=m, params=p, steps=[d(0, "1"), d(10, "2"), d(500, "3"), d(510, "#")]),
        dict(id="dtmf_each_run_interrupts_once", pipecat_module=m, params=p,
             steps=[d(0, "4"), d(10, "5"), d(20, "#"), d(300, "7"), d(310, "8"), d(600, "9")]),
        dict(id="dtmf_pending_digits_flush_on_end_of_stream", pipecat_module=m, params={"timeout_ms": 60000, "tail_ms": 0}, steps=[d(0, "4"), d(10, "2")]),
        dict(id="dtmf_star_is_an_ordinary_digit", pipecat_module=m, params=p, steps=[d(0, "*"), d(10, "1"), d(20, "#")]),
        dict(id="dtmf_digit_right_after_terminator_starts_new_run", pipecat_module=m, params=p,
             steps=[d(0, "1"), d(10, "2"), d(20, "#"), d(30, "3")]),
    ]


# --------------------------------------------------------------------------- text aggregation

def chunks_of(text):
    parts = text.split(" ")
    return [parts[0]] + [" " + w for w in parts[1:]]


def _text(chunks):
    from pipecat.utils.text.simple_text_aggregator import SimpleTextAggregator

    async def main():
        agg, out = SimpleTextAggregator(), []
        for c in chunks:
            async for a in agg.aggregate(c):
                out.append(a.text)
        last = await agg.flush()
        if last and last.text:
            out.append(last.text)
        return out

    return run_virtual(main)


def text_scenarios():
    m = "utils/text/simple_text_aggregator.py"
    texts = {
        "sentences_split_at_terminators": "Hello there friend. How are you today? I am doing fine!",
        "short_sentences_not_merged_by_reference": "Hi. How are you doing?",
        "abbreviation_does_not_split": "Dr. Smith is here today. He left already.",
        "decimal_does_not_split": "It costs $29.99 today. Thanks a lot!",
        "ellipsis_and_interrobang": "Wait... what happened here? Really?! Yes it did.",
        "no_terminator_flushes_remainder": "Hello there my good friend",
        "closing_quote_stays_with_sentence": 'He said "stop now!" and left quickly. Done then.',
        "url_does_not_split": "Visit example.com for more details. Then call us.",
        "cjk_sentences": "你好，今天天气很好。我们出去玩吧！",
        "cjk_long_sentences_split": "你好，今天的天气真的非常好。我们一起去公园散步吧！",
        "long_unpunctuated_run_is_not_capped": " ".join(["word"] * 60),
        "numbered_list_lines": "Steps follow.\n1. First item here\n2. Second item here\n",
    }
    return [dict(id=k, pipecat_module=m, params={"aggregation": "sentence"}, steps=[{"chunk": c} for c in chunks_of(v)]) for k, v in texts.items()]


# --------------------------------------------------------------------------- assistant context (interrupted reply)

def _assistant(units, interrupt_ms):
    from pipecat.frames.frames import InterruptionFrame, LLMFullResponseEndFrame, LLMFullResponseStartFrame, TTSTextFrame
    from pipecat.processors.aggregators.llm_context import LLMContext
    from pipecat.processors.aggregators.llm_response_universal import LLMAssistantAggregator
    from pipecat.tests.utils import SleepFrame, run_test

    async def main():
        ctx = LLMContext()
        agg = LLMAssistantAggregator(ctx)
        stopped = []

        @agg.event_handler("on_assistant_turn_stopped")
        async def _h(a, m):
            stopped.append(m)

        frames, offset = [LLMFullResponseStartFrame()], 0
        for u in units:
            for w in u["words"]:
                if interrupt_ms is None or offset + w["s"] < interrupt_ms:  # a word is spoken when its audio starts
                    frames.append(TTSTextFrame(w["w"], aggregated_by="word"))
            offset += u["dur_ms"]
        frames.append(SleepFrame(0.05))
        frames.append(LLMFullResponseEndFrame() if interrupt_ms is None else InterruptionFrame())
        await run_test(agg, frames_to_send=frames)
        if not stopped or not stopped[-1].content:
            return ["no_message"]
        return ["content:" + stopped[-1].content, f"interrupted:{str(stopped[-1].interrupted).lower()}"]

    return run_virtual(main)


def _unit(text, dur, starts):
    ws = text.split()
    return {"text": text, "dur_ms": dur,
            "words": [{"w": w, "s": s, "e": e} for w, (s, e) in zip(ws, starts)]}


def assistant_scenarios():
    m = "processors/aggregators/llm_response_universal.py"
    u1 = _unit("Hello there, how are you?", 1500, [(0, 250), (300, 550), (600, 700), (750, 850), (900, 1000), (1050, 1500)])
    u2 = _unit("I am fine.", 800, [(0, 200), (250, 400), (450, 800)])
    return [
        dict(id="reply_fully_played_is_recorded_whole", pipecat_module=m, params={}, steps=[], units=[u1, u2], interrupt_ms=None),
        dict(id="interrupt_between_words_keeps_spoken_prefix", pipecat_module=m, params={}, steps=[], units=[u1], interrupt_ms=700),
        dict(id="interrupt_mid_second_unit_keeps_first_unit_and_prefix", pipecat_module=m, params={}, steps=[], units=[u1, u2], interrupt_ms=1500 + 420),
        dict(id="interrupt_before_any_audio_records_nothing", pipecat_module=m, params={}, steps=[], units=[u1], interrupt_ms=0),
        dict(id="interrupt_inside_a_word_first_half", pipecat_module=m, params={}, steps=[], units=[u1], interrupt_ms=630),
        dict(id="interrupt_inside_a_word_second_half", pipecat_module=m, params={}, steps=[], units=[u1], interrupt_ms=680),
    ]


# --------------------------------------------------------------------------- summarization cut point

def _summary(messages, keep_messages):
    from pipecat.processors.aggregators.llm_context import LLMContext
    from pipecat.utils.context.llm_context_summarization import LLMContextSummarizationUtil as U

    msgs = []
    for m in messages:
        k = m["kind"]
        if k == "system":
            msgs.append({"role": "system", "content": m["text"]})
        elif k == "user":
            msgs.append({"role": "user", "content": m["text"]})
        elif k == "assistant":
            msgs.append({"role": "assistant", "content": m["text"]})
        elif k == "tool_call":
            msgs.append({"role": "assistant", "tool_calls": [{"id": m["id"], "type": "function", "function": {"name": "f", "arguments": "{}"}}]})
        elif k == "tool_result":
            msgs.append({"role": "tool", "tool_call_id": m["id"], "content": m["text"]})
    r = U.get_messages_to_summarize(LLMContext(messages=msgs), keep_messages)
    return [f"summarize:{len(r.messages)}", f"last_index:{r.last_summarized_index}"]


def summary_scenarios():
    def turns(n, start=1, tool=None):
        out = []
        for i in range(start, start + n):
            out.append({"kind": "user", "text": f"u{i}", "turn": i})
            if tool == i:
                out += [{"kind": "tool_call", "id": f"c{i}", "turn": i}, {"kind": "tool_result", "id": f"c{i}", "text": "ok", "turn": i}]
            out.append({"kind": "assistant", "text": f"a{i}", "turn": i})
        return out
    sysm = {"kind": "system", "text": "be nice"}
    m = "utils/context/llm_context_summarization.py"
    pend = lambda i: [{"kind": "user", "text": f"u{i}", "turn": i}, {"kind": "tool_call", "id": f"c{i}", "turn": i}]
    return [
        dict(id="summarize_all_but_newest_turns", pipecat_module=m, params={"keep_turns": 2}, steps=[], messages=[sysm] + turns(6)),
        dict(id="too_few_messages_summarize_nothing", pipecat_module=m, params={"keep_turns": 2}, steps=[], messages=[sysm] + turns(2)),
        dict(id="resolved_tool_exchange_is_summarized", pipecat_module=m, params={"keep_turns": 2}, steps=[], messages=[sysm] + turns(5, tool=2)),
        dict(id="no_system_prompt_starts_at_first_message", pipecat_module=m, params={"keep_turns": 2}, steps=[], messages=turns(5)),
        dict(id="unresolved_tool_call_stops_cut_before_it", pipecat_module=m, params={"keep_turns": 2}, steps=[],
             messages=[sysm] + turns(2) + pend(3) + turns(2, start=4)),
    ]


# --------------------------------------------------------------------------- constants

def constants_scenarios():
    from pipecat.turns import user_turn_completion_mixin as m
    from pipecat.turns.user_turn_completion_mixin import UserTurnCompletionConfig
    c = UserTurnCompletionConfig()
    return [dict(id="turn_completion_markers", pipecat_module="turns/user_turn_completion_mixin.py", params={}, steps=[],
                 pipecat_out=[f"complete:{c.complete_marker}", f"short:{c.incomplete_short_marker}", f"long:{c.incomplete_long_marker}"])]


# --------------------------------------------------------------------------- declared intentional differences
# id -> (reason, what go-voice does instead). The Go test fails if go-voice stops matching this or starts matching Pipecat.
MINLEN = "go-voice merges a sentence shorter than assistant_context min_unit_length (10 runes) into the next unit so TTS gets prosodic context; Pipecat's SimpleTextAggregator emits every sentence."
DIFFS = {
    "turn/transcription_interim_starts_turn": (
        "go-voice starts the silence clock when a turn opens from a transcript, so speech-timeout ends it 600ms later; Pipecat's speech-timeout waits for a final transcript or VAD stop (only user_turn_stop_timeout, default 5s, ends it).",
        ["200:turn_started", "800:turn_stopped"]),
    "turn/speech_timeout_waits_for_transcript": (
        "go-voice SpeechTimeout has no wait-for-transcript gate (ponytail in core/turn/stop.go): the turn ends silence-after-VAD-stop even if the transcript is late; Pipecat holds the stop until a transcript arrives.",
        ["100:turn_started", "1600:turn_stopped"]),
    "turn/speech_timeout_no_transcript_never_stops": (
        "Same missing transcript gate: go-voice ends a transcript-less turn after the silence timeout (the empty turn is then dropped downstream); Pipecat never stops it.",
        ["100:turn_started", "1600:turn_stopped"]),
    "turn/wake_phrase_single_activation": (
        "go-voice WakePhraseStart has only the inactivity-timeout mode (ponytail in core/turn/start.go): activity keeps it awake, so the later speech starts a turn; Pipecat single_activation stops refreshing the window and falls back to idle.",
        ["100:turn_started", "1600:turn_stopped", "3500:turn_started", "4700:turn_stopped"]),
    "idle/idle_cancelled_when_user_speaks": (
        "go-voice re-arms the idle clock when the user stops speaking with no bot reply (filtered backchannel); Pipecat re-arms only after the next bot stop or an explicit wait_for_user().",
        ["5500:idle"]),
    "idle/idle_no_clock_before_first_bot_speech": (
        "go-voice starts the user-idle clock at call start so a caller who connects and says nothing is prompted; Pipecat's clock starts only after the bot first stops speaking.",
        ["3000:idle"]),
    "mute/mute_until_first_bot_complete_released_by_error": (
        "go-voice's mute stage never sees LLM/TTS ErrorFrames (they flow downstream of it); a failed greeting is released by turn.mute max (30s) instead of at once.",
        ["0:muted"]),
    "dtmf/dtmf_pending_digits_flush_on_end_of_stream": (
        "go-voice drops a pending digit run when the session closes (the context dies with it); Pipecat flushes it as a transcription on EndFrame/CancelFrame.",
        ["interrupt"]),
    "text/short_sentences_not_merged_by_reference": (MINLEN, ["Hi. How are you doing?"]),
    "text/ellipsis_and_interrobang": (MINLEN, ["Wait... what happened here?", "Really?! Yes it did."]),
    "text/closing_quote_stays_with_sentence": (
        "A terminator that ends a streamed delta splits at once in go-voice (never stall a reply on lookahead), so a closing quote followed by lowercase continuation splits when the delta boundary lands there; the same text in one delta stays whole (core/sentence_test.go).",
        ['He said "stop now!"', "and left quickly.", "Done then."]),
    "text/cjk_sentences": (MINLEN, ["你好，今天天气很好。我们出去玩吧！"]),
    "text/long_unpunctuated_run_is_not_capped": (
        "go-voice caps a unit at max_unit_length (150 runes) at the next whitespace so first audio is never held for an unpunctuated run; Pipecat buffers until a terminator or flush.",
        ["word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word",
         "word word word word word word word word word word word word word word word word word word word word word word word word word word word word word"]),
    "text/numbered_list_lines": (
        "go-voice treats newlines and list numbers as boundaries and never splits inside '2.'; Pipecat leaves list-number fragments ('\\n1.') as separate sentences.",
        ["Steps follow.", "1. First item here", "2. Second item here"]),
    "assistant/interrupt_inside_a_word_first_half": (
        "go-voice keeps a word only if finished or at least half spoken at the barge-in (core.HeardText); Pipecat's TTS text frames deliver a word when its audio starts, so a word interrupted in its first half is kept.",
        ["content:Hello there,", "interrupted:true"]),
    "summary/unresolved_tool_call_stops_cut_before_it": (
        "Both stop the cut before an unresolved tool call; go-voice then keeps that whole turn verbatim (the user message too), Pipecat cuts right before the tool-call message.",
        ["summarize:4", "last_index:4"]),
}

AREAS = [
    ("turn", turn_scenarios, lambda sc: _turn(sc["params"]["start"], sc["params"]["stop_ms"], sc["steps"], sc["params"]["end_ms"])),
    ("idle", idle_scenarios, lambda sc: _idle(sc["params"]["timeout_ms"], sc["steps"], sc["params"]["end_ms"])),
    ("mute", mute_scenarios, lambda sc: _mute(sc["params"]["strategy"], sc["steps"])),
    ("dtmf", dtmf_scenarios, lambda sc: _dtmf(sc["params"]["timeout_ms"], sc["params"]["tail_ms"], sc["steps"])),
    ("text", text_scenarios, lambda sc: _text([s["chunk"] for s in sc["steps"]])),
    ("assistant", assistant_scenarios, lambda sc: _assistant(sc["units"], sc["interrupt_ms"])),
    ("summary", summary_scenarios, lambda sc: _summary(sc["messages"], sc["params"]["keep_turns"] * 2)),
    ("constants", constants_scenarios, lambda sc: sc["pipecat_out"]),
]


def main():
    os.makedirs(OUT, exist_ok=True)
    for area, make, run in AREAS:
        scs = []
        for sc in make():
            sc = dict(sc)
            sc["pipecat"] = run(sc)
            sc.pop("pipecat_out", None)
            d = DIFFS.get(f"{area}/{sc['id']}")
            if d:
                sc["intentional_difference"] = {"reason": d[0], "go": d[1]}
            scs.append(sc)
        doc = {"reference": {"repository": "pipecat-impl", "revision": REVISION, "generator": "tools/compat/parity/gen_goldens.py"},
               "area": area, "scenarios": scs}
        with open(os.path.join(OUT, f"{area}.json"), "w") as f:
            json.dump(doc, f, indent=1, sort_keys=True, ensure_ascii=False)
            f.write("\n")
        print(f"{area}: {len(scs)} scenarios")


if __name__ == "__main__":
    main()
