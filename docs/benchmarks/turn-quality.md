# Turn quality gate

`make eval-turn` (`govoice-turn-eval`) replays 138 utterances from Pipecat's
`evals/turn-completion` dataset (`tools/eval/turnquality/data/`, provenance in
`PROVENANCE.md`) and reports, per strategy:

- **false cut**: share of *incomplete* utterances where the strategy ended the turn (bot talks over the user);
- **late cut**: share of *complete* utterances where the strategy held the turn (bot answers late or not at all).

Thresholds live in `tools/eval/turnquality/data/thresholds.json`; `go test ./tools/eval/turnquality`
(and the command) fail when a calibrated bound is exceeded.

## What is and is not measured (read this)

The dataset is transcripts with an expected `complete` / `incomplete` label. It has **no audio and no pause timing**.

| Strategy | What runs | What the numbers mean |
|---|---|---|
| `speech-timeout` | the real `SpeechTimeout` detector, 800 ms of silence after each text | Structural: it ends every turn, so false cut is 100% and late cut 0%. A baseline, not a quality score. |
| `turn-analyzer` (smart-turn) | skipped | Needs audio. Quality of the model is not evaluated here (its report was removed in the docs cleanup; see git history). |
| `llm-completion-perfect` | real marker protocol (`AssistantContextProcessor`), fake oracle that reads the label | Must be 0/0. Validates plumbing: marker stripping, hold, split deltas (judged on the text that leaves the stage, so a leaked marker or a spoken held reply is an error). |
| `llm-completion-heuristic` | same, fake keyword oracle | Exercises non-zero errors so the gate can regress. Says nothing about any real LLM. |
| `llm-completion-real` | same, your model via `-oracle-cmd` | The only number that measures model quality. Thresholds there are `calibrated: false` placeholders until someone runs it and commits real values. |

No real model has been run for this gate; no real LLM quality claim is made.

## Running against a real LLM (opt-in, costs money, not run in CI)

Write a program that reads one JSON line on stdin (`{"system_prompt": "...", "user": "..."}`),
calls your model with that system prompt (the default `core.CompletionInstructions`)
and the user text as the latest user message, and prints the raw reply (marker first).

```
GOVOICE_TURN_EVAL_REAL=1 go run ./tools/eval/cmd/govoice-turn-eval -oracle-cmd ./my-llm.sh
```

138 model calls per run. After a representative run, set the `llm-completion-real`
bounds a little above the measured rates and flip `calibrated` to `true`.
Context is not replayed (single user message, no prior assistant turn), unlike
Pipecat's own harness; results are therefore indicative only.
