# Independent review: bugs, bloat, YAGNI

Loop until clean: independent reviewers (read-only) report findings per area,
the coordinator verifies and fixes them (tests for bug fixes), then a fresh
reviewer re-checks. Stop when a re-review finds no real bugs.

| Area | Scope |
|---|---|
| core | core/ (pipeline, session, contexts, transcript, tools, audio) |
| server | server/, cmd/ (API, storage, credentials, agents, registry, call path) |
| edge | providers/google, providers/xai, vad/, transport/ |
| clients | ui/src, examples/, e2e/, scripts/, Makefile |

Rules: bugs first (with a failing test where practical), then deletions and
simplifications that keep behaviour. No new features. `make test`, UI tests
and the e2e must stay green.
