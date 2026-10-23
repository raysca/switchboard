# Decision: pipecat.evals as a black-box check of the Go server (P0-3 spike)

Status: spike works for scripted, text-modality scenarios. Not adopted for audio or internal-state checks.

## What was built

`make eval-pipecat` (`tools/eval/pipecat/run.sh`):

1. Builds `cmd/server` (no tag) and runs it with the simulated `echo` provider (`providers.echo`:
   `mimic: instant`, `stt: audio`; see `docs/providers.md`). The agent's stages are `echo.stt|llm|tts`:
   the real Google processors against simulated backends; the LLM replies "You said: <user text>".
   (This was a `-tags fakeproviders` build that replaced `google.*`; the tag no longer exists.)
   Go builds need the repo's `SDKROOT`/`CGO_LDFLAGS`, so run it through make.
2. Refuses to start if a port is taken, then runs that binary on 18680/19690 (override with
   `EVAL_PUBLIC_PORT`/`EVAL_ADMIN_PORT`) with a throwaway store in `.govoice-eval-pipecat/` (git-ignored,
   kept for logs), sets fake Google variables in the server's environment, creates an agent over the admin API, mints one single-use voice token per
   scenario file. The server is killed on exit, pass or fail.
3. Runs `tools/eval/pipecat/run_eval.py` (stock `pipecat.evals run` plus a framing shim) against
   `ws://.../ws?token=...`. The token is masked in the output.

## What the scenarios prove (and what they do not)

Scenarios in `tools/eval/pipecat/scenarios/`, script-driven, no judge, no provider calls, about 1 s each:

- `text_turn`: a `send-text` turn produces `bot-llm-started` and a reply containing the user's own text.
- `two_turns`: each turn's reply contains that turn's text and not the previous turn's (per-turn routing).
- `latency_budget`: reply within 3000 ms. With fakes this only catches hangs and gross stalls; it says
  nothing about real latency.

Because the fake LLM echoes input, a pass means the user text crossed client -> RTVI -> pipeline -> LLM -> RTVI
-> client for each turn. It does not exercise STT, VAD, audio, TTS audio, or any real provider.
A wrong expectation fails the target (checked by changing `text_turn` to expect "Berlin": 60 s timeout, exit non-zero).

Skips with exit 0 and a message when `uv`, `go`, `curl`, the pipecat checkout, `third_party/silero_vad.onnx`
is missing, or `pipecat.evals` cannot be imported. A broken `run_eval.py` fails rather than skips.

## Why a build tag

The server binary had no fake-provider mode: Playwright e2e (`e2e/`) uses real Google and has nothing to reuse;
the Go "e2e" (`server/e2e/scenarios_test.go`) builds `core` pipelines in-process. The first cut swapped
`server/registry_noxaispeech.go` with `go build -overlay`; that replacement file was never compiled by
vet/CI/gopls, duplicated an unrelated symbol, and could not combine with `-tags xaispeech`. The tagged file is
type-checked by `go vet -tags fakeproviders ./server/...` and can be reused by a fake-provider Playwright
or load-test (P5-4) stack. (Historical: the tag no longer exists; the always-built `echo` provider replaced
it and the Playwright and load-test stacks use that.)

## What had to be shimmed

| Gap | Cause | Status |
|---|---|---|
| Framing | Go accepts and sends only binary protobuf `Frame` (RTVI JSON in `message.data`); the eval client speaks bare RTVI JSON text. Text frames are ignored by `transport/ws`. | Shim in `run_eval.py` wraps/unwraps at `EvalClientTransport` and its serializer. Upstream eval client should accept a serializer. |
| `bot-llm-started` | The server never sent it; the harness drops LLM text until it sees one. | Fixed in the server: `transport/rtvi` now brackets each reply with `bot-llm-started`/`bot-llm-stopped` and each TTS unit with `bot-tts-started`/`bot-tts-stopped`. Shim removed. |

The shim lives in the wrapper, not in `pipecat-impl/` (a git-ignored local checkout).

## What it can assert

RTVI server messages in text modality: `user_transcription`, `llm_started`/`llm_response` (text,
`text_contains`/`text_excludes`), `bot_started_speaking`/`bot_stopped_speaking`, ordering, `within_ms` from send
to reply, multi-turn scripts. Judge/persona simulations need an LLM (default `ollama/gemma4:12b`); not run.

## What it cannot assert (keep these as Go tests)

- Internal state: turn manager transitions, generations/fencing, interruption bookkeeping, context contents,
  transcript stabilization, tool-loop rounds, breaker/retry state, metrics, traces, stored session rows.
- Provider-side behavior: what was sent to STT/LLM/TTS, retries, deadlines, fallback.
- Anything the server does not surface over RTVI: no `vad-*` or marker messages, so `llm_marker` and `vad_*`
  expectations cannot pass today. `bot-tts-text`, `bot-interrupted` and the `llm-function-call-*` events are sent
  (see `docs/pipecat-compatibility.md`), but the function-call events carry only the call id and a cancelled
  flag, no name, arguments or result, so a `function_call` expectation on name or arguments cannot pass either,
  and the unmodified client ignores `bot-interrupted`.
- Audio paths: audio modality needs a user TTS and a bot-audio STT in the harness plus real VAD/STT on the server;
  the fake STT ignores audio. Not attempted.
- Admission, auth, token replay, rate limits: the harness sees only a closed socket.
- One token per connection: `eval run` reuses its URL, so one invocation per scenario file (the script does this).

## Setup

```sh
brew install uv                       # or see docs.astral.sh/uv
# pipecat checkout at ./pipecat-impl (git-ignored here) or PIPECAT_DIR=/path/to/pipecat
make eval-pipecat
```

`uv run --extra cli` builds the venv on first run (about 80 packages, no torch/onnx for text scenarios).
Logs: `.govoice-eval-pipecat/{server.log,<scenario>.eval.log}`.

## Open items for P0-3 acceptance ("nightly CI")

- Not yet in CI. CI needs the pipecat checkout (`PIPECAT_DIR`) pinned to a commit; it is not vendored. In CI a
  skip must count as a failure, or the job passes vacuously when deps are missing.
- Expand beyond text only after the missing RTVI messages exist; until then Go tests stay the source of truth.
