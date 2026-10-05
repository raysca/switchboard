# Task 9 implementation report

Base: `b61c515c2fd47da665bcc3ad0b094b3b4fd1b278`

## RED / GREEN evidence

- Core RED: `cd core && ... go test -run 'Test(Finalizer|Processor|UserContextV2)' ./...` failed because the finalizer/processor, raw-candidate and explicit-advance frames, committed marker, and V2 context gate did not exist.
- Google RED: `cd providers/google && go test -run 'TestGoogleSTTEmitsRawCandidates' ./...` failed because `STTConfig.Now` and raw-candidate output did not exist.
- Tagged xAI RED: `cd providers/xai && go test -tags xaispeech -run 'TestXAISTTEmitsRawCandidates' ./...` failed for the same missing adapter boundary.
- Stability RED: the focused invalid-stability test failed before finite/range validation existed.
- Duplicate-completion RED: the focused test showed a repeated completion extended the grace deadline; GREEN makes the first completion timestamp authoritative.
- GREEN focused and full-suite evidence is recorded after final verification below.

## Implementation and provider semantics

- `core/transcript.Finalizer` accepts copied, bounded, valid candidate text and caller-supplied timestamps. Stable/repeated interims emit once. A provider final is held for turn completion; an utterance boundary can start grace directly. A better candidate before commitment replaces the prior candidate. Timeout commits the best available candidate, while interruption/no-candidate timeout retires without context output.
- Active plus retired turn identities are bounded per generation. Capacity returns `ErrRetiredTurnLimit` without evicting retirement evidence. A generation advance clears bounded state while fencing every older input. Same-generation distinct turns remain distinct even with identical text.
- `TranscriptCandidateFrame` is raw; only the finalizer emits `FinalTranscriptionFrame{Committed:true}`. The processor forwards committed frames as ordinary data and never reconsumes them, preventing recursive/double commit.
- Google Speech V2 generated-code comments define `IsFinal` as final for the corresponding audio portion and define `Stability` only for interim results (`0` means unset). The adapter maps those fields without text dedupe.
- xAI fixtures and adapter fields keep `is_final` (provider-final chunk) separate from `speech_final` (utterance boundary); `transcript.done` closes the stream and uses its own nonempty text, falling back to the last partial. Duplicate raw candidates are safe because common retirement owns dedupe.
- Provider receipt clocks are injectable. Policy code has no `time.Now`, timer, goroutine, provider call, or application callback. No callback is invoked while finalizer state is locked.

## Compatibility and assumptions

- V2 `UserContextProcessor` assembly requires the committed marker and uses the commit timestamp. The default processor configuration retains historical direct-final acceptance and a zero-timestamp fallback for explicit V1 compatibility.
- The server/demo V1 VAD path derives the historical `turn-<generation>-<sequence>` identity. The production processor now owns one session-lifecycle deadline driver, so server/demo assembly keeps nonzero grace/timeout defaults.
- Google final results can correspond to sequential audio portions. The adapter consumes all response results and assembles offset-ordered finals with bounded segment state; stream rotation supplies an absolute offset base and text overlap reconciles replay.
- Task 9 and Gate B remain open pending independent review and the Phase 2 tester.

## Final verification

- Focused core race repetitions passed: `cd core && ... go test -race -count=50 ./transcript`; after the review fix, `-count=25` passed again.
- Focused Google race repetitions passed 10 times for rotation dedupe-through-common-policy, raw semantic mapping, and the two-turn integration with one UserMessage per turn.
- Focused tagged xAI race repetitions passed 10 times for raw semantic mapping, `is_final` versus `speech_final`, and the full provider pipeline; the final integration asserts exactly one UserMessage despite the `speech_final` plus `transcript.done` duplicate candidate.
- Fresh full affected suites passed with `SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.sdk CGO_LDFLAGS=-Wl,-w`: core `go test -race -count=1 ./... && go vet ./...`; Google `go test -race -count=1 ./... && go vet ./...`; xAI `go test -race -tags xaispeech -count=1 ./... && go vet -tags xaispeech ./...`; server `go test -race -count=1 ./... && go vet ./...`.
- Root `go test -tags xaispeech ./...` passed, including the tagged full-xAI demo build. `go test -race -count=1 ./transport/rtvi ./transport/ws`, `gofmt`, and `git diff --check` passed.
- Author self-review found and fixed two Important validation issues with fresh RED/GREEN coverage: rejected candidate text could advance policy time, and malformed marked V2 finals could fall through to the legacy wall clock. Invalid candidates are now transactionally rejected, and V2 context requires nonempty identity/text plus an explicit timestamp. No subagent review was run because the task explicitly prohibited agents; Task 9 therefore remains open for independent review.

## Correction round 1 (base `8c9aa4c`)

Correction commit: `4e7f2792d3bbadcff84aab31834221df4cd3608b` (`fix(core): complete transcript finalization`).

### RED evidence and root cause

- The first focused correction run, `go test ./core/transcript ./core ./providers/google`, failed in `core/transcript`: grace-zero did not retire after accepting a terminal candidate, the timeout test still coupled policy progress to source timestamps, and reordering expiration exposed that generation change reset the global order fence. This was retained as the concrete RED checkpoint reported to the parent.
- The independent findings shared one root cause: receipt ordering, provider/media timestamps, terminal strength, provider stream identity, and deadline ownership were implicit or conflated. The correction makes each explicit rather than adding provider-local dedupe.
- Assumption/ruling: the policy clock is monotonic for the entire processor/session and never resets at a generation boundary. Source timestamps may legitimately regress across asynchronous producers and are preserved only for emitted metadata. A grace-zero terminal candidate is accepted first and retired synchronously in the same serialized call.
- Assumption/ruling: xAI `is_final` is a provider-final segment; only `speech_final`/`transcript.done` is utterance-final. Nonempty `transcript.done.text` is the authoritative correction. Google result-end offsets are converted to stream-global media offsets using the rotation overlap base; compatible same-offset suffixes are overlap, otherwise same-offset text is a correction.

### GREEN behavior

- Finalizer input is ordered with injected monotonic receipt time. It expires due state before accepting late Observe/Complete input, never extends the earliest terminal deadline, and applies interim < provider-final < utterance-final text precedence. Source timestamps cannot regress policy state.
- `Processor.Attach` starts one supervised deadline driver per session. It owns at most one active timer, wakes on state changes, re-enters the serialized pipeline, stops with session context, and invokes no emitter/provider callback while the Finalizer lock is held. Production server/demos retain the nonzero 200 ms grace and 2 s timeout defaults.
- Google consumes every result in each `StreamingRecognizeResponse`, carries protobuf `result_end_offset`, composes interim text with the committed prefix, joins nonoverlapping finals, and reconciles offset/text overlap. Google and xAI active streams carry immutable `(turn,generation)` ownership; later speech starts detach rather than relabel a draining stream.
- UserContext keeps a bounded fail-closed committed identity ledger. Replay of one marked `(TurnID,Generation)` commit is dropped, while identical text in a distinct turn is accepted.
- Server integration covers delayed-final, interim-only, and empty-result deadlines; provider integrations continue to assert exactly one UserMessage per provider turn. The documented client playback-clear probe limitation is unchanged. Task 9 and Gate B remain open.

### Verification (MacOS 26 SDK override)

All commands below used `SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.sdk CGO_LDFLAGS=-Wl,-w` where linking was required.

- PASS: `go test -race -count=100 ./core/transcript`
- PASS: `go test -race ./core/...`
- PASS: `go test -race -count=3 ./providers/google`; PASS: `go test -race ./providers/google/...`; PASS: `go vet ./providers/google/...`
- PASS: `go test -race -count=3 -tags xaispeech ./providers/xai`; PASS: `go test -race -tags xaispeech ./providers/xai/...`; PASS: `go vet -tags xaispeech ./providers/xai/...`
- PASS: `go test -race ./server/...`; PASS: `go vet ./server/...`
- PASS: `go test -tags xaispeech ./...`; PASS: `go build -tags xaispeech ./...`
- PASS transport sanity: `go test -race ./transport/...`
- PASS Google one-UserMessage integration: `go test -race ./providers/google -run TestGooglePipeline_MultiTurnRoundTripWithDeterministicVAD -count=1`
- PASS xAI one-UserMessage integration: `go test -race -tags xaispeech ./providers/xai -run TestFullXAI -count=1`

## Correction round 2 (base `4e7f279`)

Correction commit: `f26d2dd5989e5fe048ec20ce890ae808f7056b69` (`fix(core): harden transcript ownership and assembly`).

### Preserved RED and assumptions

- The preserved overlay command failed exactly as reviewed: two core failures, five Google behavior failures plus the Google ownership data race, and the xAI typed-completion failure. The overlay probes were copied into tracked `task9_review_probe_test.go` files and failed again without the overlay before production changes.
- Speech start/typed start owns the processor's pending identity. A raw candidate may carry its own immutable identity into policy but cannot change which V1 `UserStoppedSpeakingFrame` completes.
- A higher-generation transition is committed only after policy-time validation; stale lower-generation input remains a no-op even if it carries an old timestamp. Capacity and validation failures do not clear accepted lower-generation state.
- Google text overlap is evidence only when result media intervals overlap. Sequential, nonoverlapping audio portions are appended faithfully even when their words repeat. Each stream tracks its prior final offset, and rotation's base uses current-turn bytes minus the actual bridged overlap bytes.
- Google keeps at most 128 turn assemblies and 256 segments per assembly. A newer generation reclaims fenced older assemblies. Same-generation active overflow, segment overflow, and oversized composed text return `ErrTranscriptAssemblyCapacity`; no valid-looking partial transcript is emitted.
- xAI typed completion sends audio-done only to the matching immutable owner and permits a 500 ms bounded drain. Typed interruption cancels that owner. A new turn cancels a completed drain before starting its receiver; an unfinished stream may drain with its original identity. Receiver `Session.Go` admission errors are returned rather than hidden.

### GREEN and verification

All linked commands used `SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.sdk CGO_LDFLAGS=-Wl,-w` where linking was required.

- PASS preserved overlay: `go test -race -tags xaispeech -overlay=/tmp/go-voice-task9-review.7mTI81/overlay.json -run '^TestReview' -count=1 ./core/transcript ./providers/google ./providers/xai`
- PASS promoted reviewer regressions: `go test -race -tags xaispeech -run '^TestReview' -count=1 ./core/transcript ./providers/google ./providers/xai`
- PASS focused repetitions: `go test -race -count=100 ./core/transcript`; Google reviewer/capacity focus `-count=10`; tagged xAI ownership/lifecycle focus `-count=10`.
- PASS full race/vet: `go test -race ./core/... && go vet ./core/...`; corresponding Google, tagged xAI, and server package trees passed race and vet.
- PASS root tagged build/test: `go test -tags xaispeech ./...` and `go build -tags xaispeech ./...`.
- PASS transport sanity: `go test -race ./transport/...`.
- PASS one-UserMessage integrations: Google `TestGooglePipeline_MultiTurnRoundTripWithDeterministicVAD`; tagged xAI `TestFullXAI`.
- The playback-clear probe limitation remains unchanged. Live-provider tests were not run. Task 9 and Gate B remain open for independent re-review and the Phase 2 tester.

## Correction round 3 (base `f26d2dd`)

Correction commit message: `fix(core): close transcript integration gaps`.

### Preserved RED and assumptions

- The exact round-3 overlay reproduced all six findings under the race detector before production changes: an expired turn could be recreated and committed twice; the real Google barge-in pipeline left the transcript processor at generation 1; a completed xAI stream was canceled when turn B started; Google overflow left a partial prefix eligible to commit; consecutive interim portions emitted an incomplete snapshot; and automatic rotation sent 1,920 bytes instead of 1,280 bytes after two 640-byte chunks. The probes were promoted into tracked, non-vacuous regressions before the fixes.
- Expiration is authoritative retirement. `Observe` and `Complete` may return outputs made due by that input, but must recheck retirement before allocating or mutating the addressed identity. `Interrupt` already treats retired input as a no-op after expiration.
- An `InterruptionFrame` is the V1 control boundary at which the transcript processor adopts the session's already-advanced generation while retaining the speech-start-owned turn. This does not authorize raw candidates to change active V1 ownership.
- xAI permits at most eight prior immutable owners to drain alongside the active stream. A completed A remains eligible for a late final after B begins, until done, matching interruption, its 500 ms drain deadline, session close, or bounded oldest-owner eviction.
- Google assembly exhaustion is terminal for the affected identity. The provider emits a new correlated `TranscriptInvalidationFrame` before a separate observable error; the finalizer retires that turn without committing partial text, while unrelated turns continue. A legacy sentinel `ErrorFrame` without identity fails all currently pending turns closed and is deliberately ordered with data so it cannot overtake earlier candidates.
- A Google `StreamingRecognizeResponse` is one candidate snapshot: all nonempty consecutive portions are assembled before one candidate is emitted. Rotation overlap contains only already-sent audio; the chunk that triggers rotation is sent once after replay.

### GREEN behavior and contract delta

- Late `Observe`/`Complete` input cannot resurrect an identity retired by expiration, so the original due result is the only possible commit.
- The actual `Session` + Google STT + transcript finalizer + `UserContextProcessor` + `AssistantContextProcessor` barge-in ordering now commits the generation-2 user transcript.
- xAI V1 and typed paths retain bounded draining owners and accept A's late `transcript.done` after B starts, without relabeling it or exhausting the 16-task session budget.
- Google emits exactly one complete whole-response candidate, invalidates the correlated turn on bounded assembly failure, and rotates using only prior-sent replay audio.
- Public contract addition: `core.TranscriptInvalidationFrame{Turn, Gen, Err}` / `FrameTranscriptInvalidation` is the provider-neutral correlated terminal-failure signal. `core.ErrTranscriptInvalidated` identifies the uncorrelated compatibility fallback. Neither frame is a transcript candidate or committed output.

### Verification (MacOS 26 SDK override)

All linked commands used `SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.sdk CGO_LDFLAGS=-Wl,-w` where linking was required.

- PASS exact round-3 overlay: `go test -race -tags xaispeech -overlay=/tmp/go-voice-task9-round3.JG3RdK/overlay.json -run '^TestRound3' -count=1 ./core/transcript ./providers/google ./providers/xai`.
- PASS preserved round-2 overlay: `go test -race -tags xaispeech -overlay=/tmp/go-voice-task9-review.7mTI81/overlay.json -run '^TestReview' -count=1 ./core/transcript ./providers/google ./providers/xai`.
- PASS tracked core repetitions: `go test -race -count=100 ./core/transcript`; PASS full core: `go test -race ./core/...` and `go vet ./core/...`.
- PASS Google full race/vet: `go test -race ./providers/google` and `go vet ./providers/google/...`.
- PASS tagged xAI full race/vet: `go test -race -tags xaispeech ./providers/xai` and `go vet -tags xaispeech ./providers/xai/...`.
- PASS server full race/vet: `go test -race ./server/...` and `go vet ./server/...`.
- PASS root tagged test/build: `go test -tags xaispeech ./...` and `go build -tags xaispeech ./...`.
- PASS transport sanity: `go test -race ./transport/...`.
- PASS one-UserMessage integrations: Google `TestGooglePipeline_MultiTurnRoundTripWithDeterministicVAD`; tagged xAI `TestFullXAI`.
- PASS promoted regression repetitions: `go test -race -tags xaispeech -run '^TestRound3' -count=10 ./core/transcript ./providers/google ./providers/xai`; PASS tracked round-2 regressions: the corresponding `-run '^TestReview' -count=1` command.
- PASS `gofmt` and `git diff --check`. No live-provider test was run. The playback-clear probe limitation is unchanged. Task 9 and Gate B remain open for independent re-review and the Phase 2 tester.

## Correction round 4 (base `7c5b5a2`)

Correction commit message: `fix(providers): harden transcript transition safety`.

### RED evidence and decisions

- The exact round-4 overlay command (`go test -race -tags xaispeech -overlay=/tmp/go-voice-task9-round4.rcAbaG/overlay.json -run '^TestRound4' -count=3 -timeout=15s ./providers/google ./providers/xai`) failed all six reported probes on each repetition. Both overlay files were promoted to tracked tests and failed again without the overlay before production edits.
- Google capacity invalidation is terminal data ordered after earlier candidates. Admission waits for the bounded frame-processing interval; if the data queue cannot drain, the concrete session fails closed. A separate observable error retains `ErrTranscriptInvalidated`, turn, and generation in the same data lane and uses the same bounded admission rule. Correlated errors retire only their turn, while legacy uncorrelated sentinel errors keep the fail-all fallback. An invalidation admission failure does not send an out-of-order substitute error. The strengthened one-slot queue test was RED when the error frame was dropped, then GREEN with candidate → invalidation → error ordering.
- A Google provider final settles the text prefix, not an entire evolving turn snapshot. An interim can extend or correct only the mutable tail that follows that prefix. Utterance-final text remains stronger. Google composes rotated interim snapshots from final and interim media intervals, so replay overlap removes duplicate words while nonoverlapping repeated speech remains intact.
- xAI adopts the session's advanced generation at the interruption control boundary for future streams, without relabeling the old stream. Provider sends are serialized by a dedicated wire mutex, separate from ownership and lifecycle locks; cancellation can therefore interrupt a blocked context-aware buffered send. The receiver cancels and closes exactly once before removing stream ownership on every exit, including `transcript.done`; session-close cancellation cannot make the socket unreachable. A socket returned by an in-flight open after Close is closed immediately.

### Verification

- PASS round-4 overlay and tracked regressions under race, count 3 and count 20 respectively. The added close-during-open test was RED before its narrow fix and GREEN for 10 race repetitions. Core correlated-error test was RED at the missing scoped fields before implementation.
- PASS preserved round-2 and round-3 overlays under race; PASS `go test -race -count=100 ./core/transcript`.
- PASS full core, Google, tagged xAI, and server race suites and vet; PASS root tagged test/build; PASS transport RTVI/WebSocket race suites; PASS Google and xAI one-UserMessage integrations.
- No live-provider test was run. The playback-clear probe remains unresolved. Task 9 and Gate B remain open for independent review and the Phase 2 tester.

## Correction round 5 (base `6835637`)

Correction commit message: `fix(providers): preserve transcript completeness`.

### RED evidence and decisions

- The exact round-5 overlay failed both providers on all three race repetitions. Google committed `go again` for a 0–1 s final `go` followed by a 1–2 s interim `go again`, losing the first spoken `go`. xAI silently committed `first chunk only` after its second buffered audio send failed during `transcript.created` flush, with no session error or provider error count. The probes were promoted to tracked tests before production edits; those failed for the same missing transcript/error behaviors. The Google tracked test additionally covered a final and interim in one response.
- Google offsets describe portions of a particular stream's audio. The next interim begins at that stream's last final offset, including after a response boundary and within a mixed response. A rotated stream still uses its own `offsetBase` and final offset; interval overlap continues to drive replay dedupe. Text alone is insufficient evidence for dedupe when media intervals are consecutive.
- A failed xAI buffered send is terminal for that stream's immutable turn. The receiver first admits a correlated `TranscriptInvalidationFrame`, then a correlated sentinel `ErrorFrame` in data order, and returns; its existing deferred stop cancels and closes the socket exactly once before ownership release. If terminal admission fails, the session's bounded admission path fails closed. A send canceled by intentional stream shutdown exits without reporting a new provider fault. The error is mapped and counted like an ordinary audio-send failure.

### GREEN behavior and verification

- The tracked xAI integration test first establishes a real pending interim, then fails the second buffered chunk, observes the correlated provider error, sends a late `transcript.done`, and crosses a serialized turn-completion barrier. It asserts no committed transcript, one provider error, and exactly one socket close.
- PASS exact round-5 overlay with `-race -count=3`; PASS tracked Round5 focused race with `-count=100`; PASS preserved Round2/3/4 overlays; PASS tracked Review/Round3/Round4/Round5 race with `-count=3`.
- PASS `go test -race -count=100 ./core/transcript`; full core, Google, tagged xAI, and server race suites; corresponding vet checks; root tagged test/build; transport RTVI/WebSocket race; Google and xAI provider integrations; Google and tagged xAI focused race repetitions (`-count=20`). All linked commands used `SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.sdk CGO_LDFLAGS=-Wl,-w` where linking was required.
- No live-provider test was run and no playback-clear or Gate B claim is made. Task 9 remains unchecked, and Gate B remains open for independent review and the Phase 2 tester.
