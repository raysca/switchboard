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
Core spans carry only what the loop emits, and core does not filter attribute
keys itself; the server's trace adapter filters keys before export.

Trace attributes are copied and recursively validate only finite scalar and
bounded `custom.*` numeric/boolean metadata. Custom metadata never admits
strings, even under innocent-looking keys; selected text must use the explicit
bounded text/redaction policy. The trace policy is an admission boundary, not a
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

The server enforces configurable data retention and privacy controls that
distinguish operational metadata from conversational content:

- **Metadata retention (`MetadataRetentionDays` / `MetadataRetention`):**
  Defines the lifetime of session, turn, span, event, and tool call records.
  When expired, ended sessions and their associated trace trees are safely
  pruned.
- **Content retention (`ContentRetentionDays` / `ContentRetention`):**
  Defines the lifetime of conversation content (STT transcripts, LLM assistant
  responses). Tool argument and result content is never stored.
  When content retention expires prior to metadata retention, the content purge
  sets `transcript` and `assistant_text` to null while
  preserving session, turn, span, event timestamps, latencies, and structural
  metadata for operational debugging.
- **Who called:** a session stores `caller`, the label the integration chose
  (the `principal` of its voice token, at most 128 characters), and `end_reason`,
  a closed set of words (see `docs/api/v1.md`). Both are metadata and follow
  metadata retention. No IP address, user agent or other network detail is
  stored; the principal must not carry personal data (a phone number or
  e-mail), because it is shown in the console.
- **Deletion invariants:**
  Active and draining sessions (`status IN ('active', 'draining')`), current
  and historical flow versions, and MCP discoveries are never deleted by
  retention jobs.
- **Batch bounded execution:**
  Purges execute in bounded batches (e.g. 500 records) to avoid database
  contention or long locks on both SQLite and PostgreSQL.

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
  its keys but keeps the marker and key count.
- **Applied at:** stored turn history (`transcript`, `assistant_text`; admin
  API reads from it), recording sidecar transcript turns, error text in the
  session timeline and the "session failed" log (provider errors can echo
  request text), and OTel `transcript`/`prompt`/`text` attributes when
  `otel.capture_content` is on (today no span carries them: per-frame events
  are not exported). Debug events and stored traces are metadata only.
  Tool calls are stored by name, status and timing only (`tool_calls`;
  the summary columns stay empty and the API does not return them); their
  arguments and results are never stored, traced or exported to OTel.
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
