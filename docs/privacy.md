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

The job (`server/retention/job.go`) runs every `retention.interval` and makes two passes.

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
   `sessions` row. Standalone trace spans and events older than the cutoff are
   removed too.

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
- Flow versions (`agent_versions`) and MCP discoveries and tool schemas
  (`mcp_discoveries`, `mcp_tools`, `agent_mcp_tools`) are immutable (database
  triggers reject updates and deletes) and retention never touches them.
- Deletes follow foreign-key order inside transactions, which satisfies the
  `ON DELETE RESTRICT` constraints on SQLite and PostgreSQL.

### Bounded execution

Each query handles at most `batch_size` sessions. Inside a batch the metadata
purge works in sub-batches of 25 sessions per transaction, committing between
them so SQLite's write lock is released and concurrent trace inserts are not
stalled. A large backlog drains over many short transactions rather than one
large one.

## Call recording

Off by default. A call is recorded only when **both** the server enables it
(`recording.enabled`, with `recording.dir` and `recording.retention_days > 0`)
**and** the flow opts in (`recording: {enabled, disclosure_confirmed}` in its
config). `enabled` without `disclosure_confirmed` makes the flow report
`recording_disclosure_unconfirmed` and refuse calls: the operator must confirm
callers are told they are recorded. The decision is made once at call start
from the flow version the call pins; turning recording off affects new calls,
not calls already in progress.

- **What is stored:** `<flowid>-<sessionid>-<unix>.wav` (stereo 16-bit PCM,
  L = caller, R = bot) and a `.json` sidecar (session, flow (field `flow_id`) and call ids, start/end,
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
  `recording.retention_days` (hourly, by file mtime) and finalizes `.part` files
  left by a crashed process (only files whose header this package wrote).
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
or do not enable DTMF for flows that collect payment data. `send_dtmf` digits
are tool arguments (content). The `dtmf.detected` debug event and logs never
carry digits.

## Context summarization

Flows may set `context.summarization.enabled` (default off). When the call history passes `trigger_tokens` (or three quarters of `max_messages`), the older turns (caller speech, assistant replies, tool results) are sent to the flow's primary LLM provider, with the flow's own credential and model, to be replaced by one short summary in the call's in-memory history. Fallback providers are never asked to summarize (on failover they receive the history, including any summary, like any turn). The summary is stored as a user-role message labelled as background context, never shown or recorded as caller speech, and the transcript is sent as delimited, untrusted data. The summary is derived content: it exists only in the call's history, is covered by the same content retention rules as the turns it replaces, is never added to traces or debug events (only outcome counts, a bounded failure reason and token estimates are exported), and a failed or cancelled summary leaves the history untouched.

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
  enable the text rules (`pin`) or instruct the flow not to repeat them.
- **Limits (precision over recall):** rules are regular expressions, not
  understanding. Not caught: numbers spoken as tens/hundreds ("forty one
  eleven"), numbers broken up by other words, digits spoken in other
  languages, numbers with fewer than 9 digits (except after a PIN/code
  keyword), Luhn-invalid spaced card numbers. False positives: any 9+ digit
  number (order IDs, Unix timestamps, dotted version strings) and digit-word
  runs of 9+. Matching is linear time (RE2); about 0.5 s per MiB in the
  worst case, and turns are small.

## Offline mode

With the `offline` provider (see [providers.md](providers.md#offline-mode-privacy)) a call's audio, transcripts and replies are produced by models running on the same host: no audio, transcript or reply is sent to a third-party provider, as long as no stage names a cloud provider (a failover from `offline` to a cloud provider, which the flow editor allows, sends that call to the cloud provider). The step that needs the network is `make offline-models`, which downloads the model weights once. The running stack needs no network for calls; whether the Speaches model loader contacts the Hugging Face hub for metadata while a network exists was not verified, so for an air-gapped deployment set `HF_HUB_OFFLINE=1` on the `speaches` service after the models are downloaded (see [offline/README.md](../offline/README.md)).
