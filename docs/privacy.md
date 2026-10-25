# Privacy policy

## Runtime traces

Core tracing is metadata-only unless a caller explicitly selects
`TracePolicy{Content: TraceText}`. The runtime records lifecycle names,
timestamps, opaque session/turn/call correlation, finite counts, formats, and
safe status/category labels. It never records raw audio, credentials, raw
provider/executor errors, unrestricted text, or arbitrary attributes.

Text mode is bounded (4 KiB by default and 16 KiB maximum) and only admits the
selected `transcript`, `prompt`, and `text` fields. Tool arguments and results
are never traced: the core ToolLoop's execution span carries no argument or
result attribute, and the trace policy drops any `tool.arguments` or
`tool.result` key a caller supplies. A result payload is untrusted until the
loop has normalized it and committed it to the conversation context.
Core filters attribute keys itself: the trace policy admits only the content
fields selected above, the known metadata keys and finite scalar `custom.*`
keys, and drops everything else. Core spans go to the OpenTelemetry exporter
(when enabled), not through the server's trace adapter.

The server's trace adapter, which writes the stored (database) traces, keeps only an exact whitelist of finite scalar
metadata keys (`status`, `error.category`, `reason`, `from_step`, `to_step`,
`mcp.server_id`, `mcp.tool_name`, `mcp.duration_ms`); every other key is
dropped. The trace policy is an admission boundary, not a
retention policy: exporters and the Server remain responsible for access,
storage, and deletion controls.

## Failure isolation

Runtime trace callbacks run through an asynchronous bounded dispatcher. A slow,
failing, or panicking exporter cannot fail or hold the voice session. Normal
attributes/events may be dropped when the queue is full; when a span start was
accepted, its one terminal operation is retained so the exporter observes a
balanced span. Admission reserves terminal capacity before a start, so overload
fails closed by declining new starts rather than retaining unbounded terminal
work. Only canonical error categories are supplied to `Span.End`.

Provider STT streams and TTS synthesis requests, VAD inference jobs, and
session-backed WebSocket/RTVI connections use the same metadata policy. Their
first-final, first-audio, speech-decision, connect/disconnect, and completion
events contain correlation IDs and bounded numeric metadata only. Provider,
executor, and transport failure spans receive fixed categories such as
`provider_failure`, `vad_failure`, `transport_failure`, `protocol_failure`, and
`canceled`; raw error messages are never passed to the tracer. A missing or
dropped ordinary event does not expose content or prevent an accepted span's
terminal operation.

## Server data retention and privacy controls

The server enforces configurable data retention that distinguishes operational
metadata from conversational content:

- **Operational metadata:** session start and end times, turn order, latencies,
  provider names, models, safe error categories, and the span trees used for
  reliability monitoring and debugging.
- **Conversational content:** STT transcripts and assistant reply text. Tool
  arguments and results are never stored.

The two lifetimes are independent, so content can be kept shorter than metadata.

```yaml
retention:
  enabled: true                  # default false: automatic purging is an explicit opt-in
  interval: 1h                   # default 1h
  metadata_retention_days: 30    # default 30
  content_retention_days: 7      # default 7
  batch_size: 500                # default 500
```

`config/production.yaml` turns the job on with these values; `config/cloudrun.yaml`
keeps content for 1 day and metadata for 7. OpenTelemetry export is configured
separately: see [observability.md](observability.md) (OpenTelemetry export).

- **Who called:** a session stores `caller`, the label the integration chose
  (the `principal` of its voice token, at most 128 characters), and `end_reason`,
  a closed set of words (see `docs/api/v1.md`). Both are metadata and follow
  metadata retention. No IP address, user agent or other network detail is
  stored; the principal must not carry personal data (a phone number or
  e-mail), because it is shown in the console.

### What each run does

The job (`server/retention/job.go`) runs every `retention.interval` and makes three passes.

1. **Content purge.** For ended sessions whose `ended_at` is older than
   `content_retention_days`, `turns.transcript` and `turns.assistant_text` are
   set to `NULL`. `tool_calls` rows hold no argument or result content, so there
   is nothing to purge there. `trace_spans` and `trace_events` are not touched:
   the trace adapter stores a fixed whitelist of metadata attributes and never
   the content keys `transcript`, `prompt`, `text`, `tool.arguments` or
   `tool.result` (a server test pins this). Sessions, turn rows, ordinals,
   latencies, tool-call status, error categories and span trees are preserved.
2. **Metadata purge.** For ended sessions older than `metadata_retention_days`
   the job deletes, in this order, `trace_events`, `trace_spans` (after
   unlinking `parent_id`), `tool_calls`, `turns`, `session_mcp_tools` and the
   `sessions` row; just before that row goes it blanks the `reason` of eval
   results scored on the call. Standalone trace spans and events older than the cutoff are removed too.
3. **Eval purge** (when `eval_retention_days`, default 180, is above 0). Finished
   eval jobs, eval results and eval runs go together once they are older than
   `eval_retention_days`, so a run's detail stays complete for the run's whole life.
   A `reason` (a judge's words, a `never_say` match) can quote call content: it
   outlives `content_retention_days` and is blanked when the call's metadata is
   purged or the call is erased.

**Upgrade note.** A database written by a build between commits `5be193e` and
`12a7884` (2026-10-05/06) with a trace content mode configured may hold content
keys in stored traces. This build no longer sanitizes stored traces, so purge
them by hand: stop the server, back up the database, then run on SQLite or
Postgres (the column is `TEXT` in both; this resets only the matching rows to an
empty attribute set):

```sql
UPDATE trace_spans SET attributes_json = '{}' WHERE attributes_json LIKE '%"transcript":%' OR attributes_json LIKE '%"prompt":%' OR attributes_json LIKE '%"text":%' OR attributes_json LIKE '%"tool.arguments":%' OR attributes_json LIKE '%"tool.result":%';
UPDATE trace_events SET attributes_json = '{}' WHERE attributes_json LIKE '%"transcript":%' OR attributes_json LIKE '%"prompt":%' OR attributes_json LIKE '%"text":%' OR attributes_json LIKE '%"tool.arguments":%' OR attributes_json LIKE '%"tool.result":%';
```

Databases that never ran with a trace content mode need no action.

### Invariants

- Sessions with status `active` or `draining` are never deleted and their
  content is never purged, whatever their age.
- Agent versions (`agent_versions`) and MCP discoveries and tool schemas
  (`mcp_discoveries`, `mcp_tools`, `agent_mcp_tools`) are immutable (database
  triggers reject updates and deletes) and retention never touches them.
- Deletes follow foreign-key order inside transactions, which satisfies the
  `ON DELETE RESTRICT` constraints on SQLite and PostgreSQL.
- A call on hold is never blanked or deleted by a run, and the dry run does not count it
  ([holds and erasure](#holds-and-erasure)).

### Bounded execution

Each query handles at most `batch_size` sessions. Inside a batch the metadata
purge works in sub-batches of 25 sessions per transaction, committing between
them so SQLite's write lock is released and concurrent trace inserts are not
stalled. A large backlog drains over many short transactions rather than one
large one.

### Seeing what retention does

`GET /api/v1/retention` (management key) reports the policy in effect and the last run:

```json
{ "data": { "enabled": true, "interval_seconds": 3600,
  "content_retention_days": 7, "metadata_retention_days": 30, "eval_retention_days": 180,
  "batch_size": 500, "recording_retention_days": 3,
  "last_run": { "started_at": "2026-10-24T11:00:00Z", "duration_ms": 41, "turns_purged": 12,
                "sessions_deleted": 4, "turns_deleted": 9, "tool_calls_deleted": 2,
                "spans_deleted": 31, "events_deleted": 70,
                "eval_jobs_deleted": 0, "eval_results_deleted": 0, "eval_runs_deleted": 0 },
  "held_sessions": 1,
  "recent_actions": [ { "at": "2026-10-24T11:20:00Z", "action": "hold",
                        "subject": "0192aaaa-0000-7000-8000-000000000001", "detail": {}, "request_id": "..." } ] } }
```

`held_sessions` is how many calls are on hold; `recent_actions` is the last 20 holds, releases and erasures, newest first.

`last_run` is `null` until the first run since the server started, and carries `error` when the run failed. With
`?dry_run=true` the response adds `dry_run`: what a run started now would remove, counted without removing
anything (`content_sessions`, `content_turns`, `metadata_sessions`, `metadata_turns`, `metadata_tool_calls`,
`metadata_spans`, `metadata_events`). The numbers are the ones the run then reports; a call that ends in between
can move them by one session. With `retention.enabled` off the endpoint still shows the configured lifetimes, and a
dry run answers 409 `retention_disabled`.

Metrics: `voice_retention_enabled`, `voice_retention_runs_total{result}`, `voice_retention_turns_purged_total`,
`voice_retention_sessions_deleted_total`, `voice_retention_erasures_total{kind="call"|"caller"}`, `voice_retention_interval_seconds` and `voice_retention_last_success_age_seconds`
(the uptime until the first completed run). The `GovoiceRetentionStalled` alert fires when the job is on and has not
completed a run for three intervals. A configuration that keeps content longer than metadata, or recordings longer than content, is refused at
start-up with a message naming both values.

### Holds and erasure

All of this is behind the management key; the console's call page has the buttons for one call.

**Hold.** `PUT /api/v1/sessions/{id}/hold` (optional `{"reason": "..."}`, at most 200 bytes) keeps a call out of every
retention run, however old it gets; `DELETE /api/v1/sessions/{id}/hold` lifts it, and the next run treats the call as any
other. Both are idempotent and return the call. A hold may be placed on a call in progress. The call's recording is kept
by the recording janitor too, if the call has one. The reason is written by the operator, lives on the call, and goes
with it when the call is erased; it is never logged, put in a metric or put in the audit history. Only a change of the
flag is audited.

**Erase one call.** `DELETE /api/v1/sessions/{id}` removes an ended call that is not on hold: the call row, its turns
(transcript and assistant text), tool calls, spans, events, the tool pins captured for it, and its recording on this
server. An eval result scored on the call keeps its score and outcome but loses its `reason`, which can quote the call. The response counts what was removed by class (`turns`, `tool_calls`, `spans`, `events`, `recordings_removed`).
A call on hold is refused with 409 `session_held`, a call that has not ended with 409 `session_active`, an unknown id
with 404. The rows go first, in one transaction with the audit entry, and the recording files after it; if a file cannot
be removed the answer is 500 `recordings_not_removed` (the rows are already gone), and sending the same `DELETE` again
finishes the files (200 with `recordings_removed`, or 404 once nothing is left).

**Erase by caller.** `POST /api/v1/sessions/erase` with `{"caller": "<label>", "confirm_caller": "<the same label>",
"dry_run": false}` erases the ended, non-held calls whose caller label is exactly that. Typing the label twice is the
confirmation: a missing, blank, mismatched or over-128-character label is 400. At most 500 calls are erased per request;
the response has `erased`, `held` and `active` (matching calls skipped), `remaining` (still to erase), the same class
counts as above and `recordings_removed`, plus `recordings_failed`, the ids of erased calls whose files could not be
removed (erase each id again to retry). Repeat the request until `erased` is 0. With `dry_run` it only counts.

**Audit.** Every hold, release and erasure is recorded in `retention_audit` in the same transaction as the change: the
action, the time, the call id (for an erasure by caller, the first 16 hex characters of the SHA-256 of the label, so the
history never holds the label), counts, and the server-generated request id (a client `X-Request-ID` is ignored). It
holds no transcript text, recording path or reason. The history is shown in the console's Server page and in the status
above; it is not yet purged by the retention policy.

**What an erasure leaves.** Agent versions, tool definitions and MCP discoveries (they belong to the agent, not the
call); eval results scored on the call (everything but the reason), which age out under
`eval_retention_days` and are not protected by a hold on the call; the audit entry; metrics, which are labelled by agent and provider only; anything outside the database: backups
and snapshots, log files, an external trace backend, provider-side retention; and recordings that are not this call's:

- Recordings made before holds and erasure existed are named `<agent id>-<number>-<unix time>`, not by call. They cannot
  be tied to a call, so a hold does not protect them, an erasure does not remove them, and they age out under
  `recording.retention_days` as before. Newer recordings are named `<call id>.wav` and `<call id>.json`.
- `recording.dir` is local to one server instance. An erasure removes recordings only on the instance that handles the
  request, and `recordings_removed` says how many it found. With several instances, send the `DELETE` to each (an id
  with no rows left but files on that instance removes them), or share one directory. Cross-instance erasure is not
  built.

## Call recording

Off by default. A call is recorded only when **both** the server enables it
(`recording.enabled`, with `recording.dir` and `recording.retention_days > 0`)
**and** the agent opts in (`recording: {enabled, disclosure_confirmed}` in its
config). `enabled` without `disclosure_confirmed` makes the agent report
`recording_disclosure_unconfirmed` and refuse calls: the operator must confirm
callers are told they are recorded. The decision is made once at call start
from the agent version the call pins; turning recording off affects new calls,
not calls already in progress.

- **What is stored:** `<call id>.wav` (the stored call's id; a call with no stored record
  keeps the older `<agentid>-<sessionid>-<unix>.wav`) (stereo 16-bit PCM,
  L = caller, R = bot) and a `.json` sidecar (session, agent (field `agent_id`) and call ids, start/end,
  format, and the user/assistant transcript turns, each capped at 4 KiB; an
  assistant turn cut off by barge-in holds only the heard prefix and is marked
  `interrupted`). File names contain ids only, never names or phone numbers. The caller audio and
  transcript are personal data: treat the directory accordingly.
- **Bot track = what was actually sent.** It is fed from the paced writer's
  successful write completions, so audio flushed on barge-in is never recorded.
  Tracks are aligned to wall-clock with silence padding; audio arriving within
  60 ms of a track's end continues it without a gap, so network jitter never
  inserts clicks.
- **Retention:** a janitor deletes recordings older than
  `recording.retention_days` (hourly, by file mtime), except the recording of a call on hold
  (the database is asked per expired file; if it cannot answer the file is kept), and finalizes `.part` files
  left by a crashed process (only files whose header this package wrote). A call does not read as
  ended until its recording is written.
  Recordings are conversation content, so `retention_days` may not exceed
  `retention.content_retention_days` when database retention is on.
- **Storage hygiene:** use a dedicated directory. It is created/tightened to
  0700 and refused if it is a symlink; files are 0600, created exclusively
  (never through a planted link), written as `.part` and renamed on
  completion. The janitor only touches regular files named like recordings
  (`<id>.wav`, `<id>.json`, and their `.part`) and never follows symlinks.
- **Caps per call:** `recording.max_duration` (1h) and `recording.max_bytes`
  (256 MiB); the file is truncated and flagged `truncated`.
- **Failure isolation:** the recorder queue is bounded; a slow or failing disk
  drops and counts frames (logged at call end) and never stalls or fails the call.
- **Not covered:** S3-compatible storage is an interface (`record.Sink`) only;
  encryption at rest is the operator's responsibility.

## DTMF digits

Key presses (`dtmf.detect`/`dtmf.aggregate`) can be PINs or card numbers. The
aggregated `DTMF: …` text goes to the LLM unchanged (the model needs it), but
is masked at the sinks by default (see Sink redaction): stored turn history,
recording sidecar and OTel content become `DTMF: [REDACTED:dtmf 4]`. The
client's `user-transcription` still carries the digits. A recording's caller
track contains the tones themselves, which are decodable: disable recording
or do not enable DTMF for agents that collect payment data. `send_dtmf` digits
are tool arguments (content). The `dtmf.detected` debug event and logs never
carry digits.

## Context summarization

Agents may set `context.summarization.enabled` (default off). When the call history passes `trigger_tokens` (or three quarters of `max_messages`), the older turns (caller speech, assistant replies, tool results) are sent to the agent's primary LLM provider, with the agent's own credential and model, to be replaced by one short summary in the call's in-memory history. Fallback providers are never asked to summarize (on failover they receive the history, including any summary, like any turn). The summary is stored as a user-role message labelled as background context, never shown or recorded as caller speech, and the transcript is sent as delimited, untrusted data. The summary is derived content: it exists only in the call's history, is covered by the same content retention rules as the turns it replaces, is never added to traces or debug events (only outcome counts, a bounded failure reason and token estimates are exported), and a failed or cancelled summary leaves the history untouched.

## Sink redaction

One policy engine (`core/redact`) masks sensitive spans as `[REDACTED:kind]`
at **sinks only**; the live LLM context is never touched.

```yaml
privacy:
  redaction:
    enabled: false      # text rules below; default off
    rules: []           # default when enabled: pin, ssn, iban, card, digits
    dtmf: always        # always (default) | off; independent of enabled
```

- **Rules:** `pin` (3-8 digits or 3-8 spoken digits after PIN/CVV/CVC/
  passcode/OTP/one-time password, or after "my/the/your/a/this/that/access/
  door/gate/security/verification/confirmation/auth/one-time code"; "zip/area/
  error code" are not matched), `ssn` (NNN-NN-NNNN, plausible area), `iban`
  (mod-97 valid), `card` (13-19 digits with spaces/dashes/dots, Luhn valid),
  `digits` (9+ contiguous digits, or 9+ spoken/single digit tokens such as
  "four one one one ...", "oh four double one ..." or "4 1 1 1 ..."), and
  opt-in only by name `email` and `phone`. Before the text rules run,
  non-ASCII digits (Arabic-Indic, fullwidth, ...) are folded to ASCII and
  invisible characters (zero-width, bidi controls, soft hyphen) are dropped,
  so they cannot split a number; the stored text changes only in those
  characters. Redacting is idempotent. DTMF: every `DTMF:` marker run loses
  its keys but keeps the marker and key count. The key class includes the
  letters A-D, so the first A-D letter after a `DTMF:` marker is masked even in
  prose ("DTMF: Call me" becomes "DTMF: [REDACTED:dtmf 1]all me").
- **Applied at:** stored turn history (`transcript`, `assistant_text`; admin
  API reads from it), recording sidecar transcript turns, error text in the
  session timeline and the "session failed" log (provider errors can echo
  request text), and OTel `transcript`/`prompt`/`text` attributes when
  `otel.capture_content` is on (today no span carries them: per-frame events
  are not exported). Debug events and stored traces are metadata only.
  Tool calls are stored by name, status and timing only (`tool_calls`;
  the summary columns stay empty and the API does not return them); their
  arguments and results are never stored, traced or exported to OTel.
- **Idle lines:** the quiet-caller prompt (`prompt_mode: say`), the bot-stuck
  fallback and the goodbye are operator-configured text, spoken as assistant
  turns like a greeting. They are stored as turns (empty `transcript`, the line
  as `assistant_text`), appear in the recording sidecar and reach the client as
  bot text, and pass the same redaction. They are no caller turn and add no
  reply-time sample, but they do count in `turn_count` and the dashboard turn totals, so a silent
  caller's call is no longer at zero turns. A goodbye refused while a reply is active is skipped (the
  call still ends after the goodbye wait of silence).
- **Tool calls on the client wire:** `llm-function-call-started`, `-in-progress` and `-stopped` carry only the call's `tool_call_id` (and `cancelled`), Pipecat's default `function_call_report_level` `none`: no tool name, arguments or result reaches the client, whatever the redaction settings. The `tool_call_id` itself is chosen by the provider or model (OpenAI-compatible backends echo whatever the server returns); it reaches the RTVI wire and the stored span's `ToolCallID`, as in Pipecat.
- **NOT covered:** the live LLM context; anything the STT/LLM/TTS providers
  receive; the client's `user-transcription`; audio recordings (speech and DTMF
  tones remain); the summarizer input; content already stored before the
  policy was enabled. With only the default DTMF masking, an assistant reply
  that reads the keyed digits back ("you entered 1234") is stored as-is:
  enable the text rules (`pin`) or instruct the agent not to repeat them.
- **Limits (precision over recall):** rules are regular expressions, not
  understanding. Not caught: numbers spoken as tens/hundreds ("forty one
  eleven"), numbers broken up by other words, digits spoken in other
  languages, numbers with fewer than 9 digits (except after a PIN/code
  keyword), Luhn-invalid spaced card numbers. False positives: any 9+ digit
  number (order IDs, Unix timestamps, dotted version strings) and digit-word
  runs of 9+. Matching is linear time (RE2); about 0.5 s per MiB in the
  worst case, and turns are small.

## Offline mode

With the `offline` provider (see [providers.md](providers.md#offline-mode-privacy)) a call's audio, transcripts and replies are produced by models running on the same host: no audio, transcript or reply is sent to a third-party provider, as long as no stage names a cloud provider (a failover from `offline` to a cloud provider, which the agent editor allows, sends that call to the cloud provider). The step that needs the network is `make offline-models`, which downloads the model weights once. The running stack needs no network for calls; whether the Speaches model loader contacts the Hugging Face hub for metadata while a network exists was not verified, so for an air-gapped deployment set `HF_HUB_OFFLINE=1` on the `speaches` service after the models are downloaded (see [offline/README.md](../offline/README.md)).
