# Agent evals

`tools/eval/agents` checks what an agent does on a call: a simulated caller talks to a stored agent (its current version, an older one, or a draft) and assertions score the call. It is one command, `go run ./tools/eval/agents`, that drives the public API like any integration. It adds nothing to the server.

Use it before and after you change a prompt, a step, a handoff or a tool selection, to see whether the call still goes where it should.

## What it is, and what it is not

It runs a call over the real `/ws` with text turns (RTVI `send-text`). Speech is always switched off for the run: every speech stage is rewritten to the text-only `silence` provider, so the harness tests prompts, steps, handoffs and tools, not the speech stack. For speech use `make live-check` and `make agent-matrix`.

**MCP tools run for real:** an eval call executes the agent's selected tools with whatever arguments the model sends. Point evals at agents whose tools hit test systems; `GOVOICE_LIVE` gates model cost only, not what tools do.

Every eval call is a normal draft call. It appears on the Calls page with caller `agent-eval` and `draft: true`, and the stored agent is never changed.

What a pass means depends on the model under test (`-providers`):

| Mode | Model | Cost | A pass proves |
|---|---|---|---|
| `echo` | the `echo` provider | free, no keys | Plumbing: the handoff exists from this step, the target step builds and answers, the MCP tool is attached and succeeds, `end_call` hangs up, and the harness reads all of it back. Nothing about prompts: the model never reads them, and it repeats the caller's words. |
| `offline` | Ollama `govoice-test` (`llama3.2:3b`) at 127.0.0.1:11434 | free | Behaviour of the prompts on a small local model: does it pick the handoff, call the tool, avoid forbidden phrases. A 3B model is weaker than the model you ship, so a failure may be the model, and a pass is a floor, not a guarantee. |
| `keep` | each step's own model | paid | Behaviour on the models the agent really uses. Needs `GOVOICE_LIVE=1`. |

The report prints the mode on its last line, so an echo pass is never mistaken for a behaviour pass. `offline` can be pointed at a paid OpenAI-compatible endpoint by setting the server's `providers.offline.llm_endpoint`; that is then your cost, and this tool cannot tell, so use `keep` and the gate when the model is paid.

There is no LLM judge. Assertions are structural (step path, tool names and statuses, end reason, turn count) plus regular expressions over the agent's words and a reply-time median. A judge could be added later as one more `expect` key.

## Case format

One or more cases per file (YAML documents separated by `---`), `*.yaml` or `*.yml`, in a directory (default `tools/eval/agents/cases`). Unknown keys are refused. An omitted `expect` key is not checked.

```yaml
name: dealership-buyer-reaches-sales      # unique across the directory
agent: Dealership desk                     # agent name (exact) or id
caller:                                    # exactly one of script | persona
  script:                                  # 1 to 20 lines, 1 to 500 characters each
    - "Hi, I'd like to buy a car. What are your prices?"
    - "What kinds of cars do you sell?"
  # persona:
  #   who: "Jane Doe, who has never been to Smile Dental"
  #   goal: "register and book a cleaning next week"
  #   max_turns: 8                         # 1 to 20, default 8
expect:
  path: [front, sales]                     # exact step path, step ids or names
  tools_called: [transfer_to_sales]        # each must appear; "name" means succeeded, or "name:failed", "name:cancelled"
  tools_not_called: [end_call]
  say: ["(?i)hatchback|suv"]               # each regexp must match the agent's replies, joined with a newline
  never_say: ["(?i)as an ai"]              # none may match
  end_reason: caller_hung_up               # the stored end reason of the call
  max_turns: 6                             # most stored turns (see Limits)
  median_reply_ms_max: 8000                # median of "typed line to first reply text"
```

Always checked, not configurable: no RTVI `error` message (other than the server closing the call with its own end reason) and no failed handoff.

Regular expressions are Go RE2 and are not made case-insensitive: write `(?i)` yourself. Tool names are the ones in the call's trace: `transfer_to_<step>`, `end_call`, and an MCP tool's own name. Only name and status are recorded; arguments and results never are.

### Callers

A **script** types its lines in order, one per turn, then hangs up (`caller_hung_up`), unless the agent hangs up first.

A **persona** is played by a model. Each turn the model gets the persona, the goal and the transcript so far (the latest 24 lines, 400 characters each), and answers with one short sentence (cut at 300 characters). The call ends when the model replies with a lone `DONE`, or after `max_turns` typed lines. A persona that reaches `max_turns` without `DONE` is not a failure by itself: only the assertions decide, and a case with only `say` passes. The caller model never sees the agent's configuration, has no tools, and makes one bounded completion per turn. It does read the agent's words, so treat an agent that tries to instruct the caller as a possible prompt injection, as you would any model reading untrusted text.

`-caller-llm offline` (default) uses Ollama `govoice-test` at 127.0.0.1:11434 (`-caller-url`, `-caller-model` change it). The Modelfile sets temperature 0 and seed 42, so the persona is as repeatable as a local model gets. `-caller-llm xai` uses xAI with `XAI_API_KEY` from the environment, only against the xAI endpoint (`-caller-url` is refused with it), and is paid.

## Running it

Against a server you run yourself (`make dev`), with the offline model:

```bash
set -a; . .govoice-dev/dev.env; set +a    # exports GOVOICE_MANAGEMENT_API_KEY; the targets never read or print it
make eval-agents                           # tools/eval/agents/cases on -providers offline
make eval-agents CASES=my/cases EVAL_ARGS="-runs 5 -min-pass 3 -max-turns 200"
```

The make variables are `EVAL_ADMIN` (default `http://127.0.0.1:9090`), `EVAL_PUBLIC` (default `127.0.0.1:8080`), `CASES` and `EVAL_ARGS`. Ollama must be up with `govoice-test` (`make offline-up OFFLINE_PROFILE=host-llm`, `make offline-models`). The default cases (`dev-assistant-answers`, `dealership-buyer-reaches-sales`) need the seeded agents of `make dev`. The dental case is in its own directory, `tools/eval/agents/cases-dental`, so a default run never needs the demo: run it with `make eval-agents CASES=tools/eval/agents/cases-dental`, after installing the Smile Dental demo ([dentistry.md](../demo/dentistry.md)) and starting its MCP server (`make dental-mcp`). A run lists every case whose agent is missing in one error.

The harness proves itself with no keys and no running server (about 40 s, builds and starts its own server on the echo provider, then must see every deliberately broken agent fail):

```bash
make eval-agents-echo
```

On each agent's own models, which spends API credit (the tool and the target both refuse without `GOVOICE_LIVE=1`):

```bash
GOVOICE_LIVE=1 make eval-agents-live       # MAX_TURNS caps caller turns, default 40
```

Or call the tool directly:

```bash
go run ./tools/eval/agents -admin http://127.0.0.1:9090 -public 127.0.0.1:8080 -providers offline -case dealership-buyer-reaches-sales -runs 3 -min-pass 2
```

| Flag | Default | Meaning |
|---|---|---|
| `-cases` | `tools/eval/agents/cases` | directory of case files |
| `-case` | all | comma-separated case names to run |
| `-providers` | `keep` | `keep`, `offline` or `echo` (the table above) |
| `-version N` | current | run published version N of each agent, as a draft |
| `-runs N` | 1 | runs per case |
| `-min-pass K` | 1 | runs that must pass for the case to pass |
| `-json path` | none | also write the report as JSON, file mode 0600 |
| `-turn-timeout` | 45s | most time one typed turn may take |
| `-quiet` | 1.5s | agent silence that ends a turn |
| `-max-turns N` | 40 | most caller turns in the whole run (cases x runs x lines, or `max_turns` for a persona), checked before the first call |
| `-caller-llm` | `offline` | persona model: `offline` or `xai` |
| `-caller-url`, `-caller-model` | Ollama, `govoice-test` | persona model endpoint (offline only) and name |
| `-admin`, `-public` | `http://127.0.0.1:9090`, `127.0.0.1:8080` | management API and public listener |
| `-selftest` | off | the harness self-test (`make eval-agents-echo`) |

The management key is read from `GOVOICE_MANAGEMENT_API_KEY` and `XAI_API_KEY` from the environment only; neither is printed or written. Exit code 0: every case passed. 1: a case failed. 2: setup problem (bad case file, unreachable server, a refused live gate, too many planned turns).

### The live gate

`GOVOICE_LIVE=1` gates model cost only. MCP tools run for real: an eval call executes the agent's selected tools with whatever arguments the model sends, so point evals at agents whose tools hit test systems.

A run refuses without `GOVOICE_LIVE=1`, before any call, when any step of a selected agent would still reach a paid model (`-providers keep` with a non-echo, non-offline model, or a fallback that is), or when a persona case uses a paid caller model (`-caller-llm xai`, or an offline caller URL whose host is not loopback). Set it only when you mean to spend. Cost is roughly one model request per agent reply plus one per persona turn.

## Reading a report

```
PASS dealership-buyer-reaches-sales (offline, 1/1 runs)
  run 1: session <id>, agent version <id>, config <sha256>
    path: front > sales
    tools: transfer_to_sales:succeeded
    end: caller_hung_up, 3 turns, median reply 2326 ms
    caller: Hi, I'd like to buy a car. What are your prices?
    agent: ...
agent-eval: 2 passed, 1 failed (offline)
```

Each failed assertion prints as `FAIL <key>: want ..., got ...` under its run. The session id opens in the Calls page; the agent version id and config hash pin the result to what ran. `errors` in the JSON are harness problems (a timeout, an API error), not verdicts on the agent. The JSON report (`govoice.agent-eval/v1`) has no timestamps, so two runs diff cleanly:

```bash
diff <(jq -S '.results[]|{case,passed,runs:[.runs[]|{path,tools,end_reason}]}' v3.json) <(jq -S '...same...' v4.json)
```

To compare versions, run the same cases with `-version 3` and `-version 4` and diff the two files. Transcripts are in the report (text report and JSON, lines cut at 500 characters), so keep the JSON file as private as the Calls page.

## Writing a good case

- Assert outcomes, not wording: the step path, the tools, a key word with `say`, a forbidden phrase with `never_say`. A model rarely says the same words twice.
- One behaviour per case. A case named for what should happen (`dealership-buyer-reaches-sales`) is easier to read than one that checks everything.
- Prefer `path` and `tools_called` for handoffs and tools; they are exact and do not depend on phrasing.
- Do not assert on words the caller itself says: on echo the agent repeats the caller's line, so such a `say` proves nothing there.
- Use a persona for open-ended dialogue (a booking) and a script when you want the same lines every time.
- Set `max_turns` in `expect` to catch a call that wanders; remember it counts handoff greetings (see Limits).

## Determinism, `-runs` and `-min-pass`

The offline model runs at temperature 0 with seed 42 when the agent sets no temperature; an agent's own `llm.temperature` is kept on purpose, because it is part of what is tested. A paid model is not deterministic. For a flaky case run it several times: `-runs 5 -min-pass 3` passes the case when at least 3 of the 5 runs pass, and the report shows every run. Choose `-min-pass` from measured pass rates, not to make a failing case go green. A case file does not hold `-min-pass`; the number is part of the command (for example in `EVAL_ARGS`).

## Measured on the offline model

Measured on `govoice-test` (`llama3.2:3b`, Ollama), `-providers offline`, 2026-10-24, three batches of `-runs 5`, same server:

| Case | Agent | Result (runs passed) | Note |
|---|---|---|---|
| `dev-assistant-answers` | Dev Assistant | 15 of 15 | |
| `dealership-buyer-reaches-sales` | Dealership desk | 15 of 15 | The model picked `transfer_to_sales` every time. |
| `dental-new-patient-books` | Smile Dental front desk | 0 of 15 | The persona held its role and said `DONE` after 8 lines. The front desk handed off to the new-patient step every time, but that step wrote `register_patient` and `find_slots` out as text instead of calling them, then called `book_appointment` with invented arguments (failed). A 3B model cannot do this five-tool booking; the case is kept in `cases-dental`, outside the default run, for a bigger model (`FROM` in a local Modelfile, `offline/README.md`) or `-providers keep`. |

The runs were identical from batch to batch (temperature 0, seed 42), so repeated batches add little confidence: they show the harness is repeatable, not that the model is reliable. `-min-pass` cannot rescue a 0 of 15 case, and there is no per-case `min_pass` key.

## CI

`make eval-agents-echo` runs in the `graph-e2e` job of `.github/workflows/ci.yml`, and in `make e2e-echo`. It needs `third_party/silero_vad.onnx`: without it the target prints SKIPPED and exits 0 locally, and fails when `CI` is set. `make eval-agents` and `make eval-agents-live` are not in CI. The tool's own unit tests run with `make test`.

## Limits

- Text turns only. Speech, STT, TTS, interruptions and audio latency are not exercised.
- A turn ends when the agent has been quiet for `-quiet` (1.5 s) with no reply open. A tool round or a model that pauses longer than that inside one reply is tracked (an open reply, a tool call in flight, or a transfer in progress keeps the turn open), but an unusually slow model may still need a larger `-quiet` or `-turn-timeout`.
- Reply time is measured from the typed line to the first reply text on the client. It is not voice-to-voice latency.
- `max_turns` is the stored turn count, and it includes the greeting a step speaks after a handoff.
- Echo repeats the caller's words and calls a tool only on magic phrases ("transfer to <step>", "end the call"), so it cannot say whether a prompt works.
- A small offline model sometimes writes a tool call as text instead of calling it. That is a real failure of the agent on that model, and the case reports it.
- Persona runs are as repeatable as the caller model: a persona can drift, repeat itself or never say `DONE`; `max_turns` bounds it.
- The eval changes only the speech and model stages of the draft. Prompts, sampling, greetings, tools, `end_call` and handoffs are the agent's own.
- No run history, no server endpoint, no console page, no publish gate: the cases are files you keep next to your agent exports.
