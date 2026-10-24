# Example agent eval cases

Cases for the agents `make dev` seeds. `make eval-agents` runs them on the offline model (Ollama); `make eval-agents-live` on each agent's own model (paid). Format and flags: `docs/operations/agent-evals.md`.

The dental case lives in `../cases-dental`, apart, because it needs the Smile Dental demo installed and `make dental-mcp` running, and a 3B model fails it (0 of 15): `make eval-agents CASES=tools/eval/agents/cases-dental`.

- `say` and `never_say` scan the agent's lines joined with `\n`, so a pattern can anchor on a line break.
- `max_turns` is the stored turn count: it includes the greeting a step speaks after a handoff.
- Echo replies repeat the caller's words, so a `say` pattern that matches the caller's own line proves nothing on echo.
- The model answers freely: assert outcomes (path, tools, key words), not exact wording, and use `-runs N -min-pass K` for a flaky case.
