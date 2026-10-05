# Output Pacing and Flush Boundary

Task 8 hardens the existing WebSocket `PacedWriter`; there is no second pacing engine. The transport retains protobuf encoding and socket writes. Core owns provider-neutral speaking transition types and the small concurrency-safe `SpeakingTracker`.

## Queue and pacing

Enqueue copies the frame and sample bytes, derives a single normalized duration when the producer omits one, and stores that duration with the queue entry. Admission checks frame-count, byte, and media-duration budgets before mutation. Overflow returns both `ErrQueueBufferFull` and the V1-compatible `ErrSlowClient`; retained accounting and a matching pending final marker are unchanged, so a retry can still become turn-final. Non-positive budget values are normalized to bounded defaults without changing the public constructor. Queue locks are not held during pacing waits, encoding, socket writes, or callbacks.

The default queue remains 4,800 bytes / 100 ms at 24-kHz PCM16 mono and is additionally bounded to 64 frames, including empty frames. The queue stores at most 64 pending whole-turn final markers. A marker-limit failure is explicit and fail-closed.

## Whole-turn finality and speaking

`OutputAudioFrame.IsFinal` continues to mean “last audio chunk of this synthesis unit.” `TurnFinal` means “last output for the whole assistant turn.” When LLM completion is known before the last synthesis unit is emitted, Google, xAI, and the deterministic fake copy `SynthesisUnitFrame.IsFinal` onto the last audio chunk; xAI also emits an empty final frame when a final unit contains no audio. When a punctuation-aligned unit was already streamed, `AssistantContextProcessor` emits an ordered `OutputTurnFinalFrame`. The pacer reconciles that marker whether it arrives before admission, while the unit is queued, selected for pacing, in a transport write, or just after successful completion. The completed-unit ledger and marker ledger are bounded, and duplicate or stale markers cannot produce another stop or leak pending state.

Speaking starts only after a successful non-empty socket write. It does not start on enqueue, `TTSStarted`, empty output, encoding failure, stale output, or a canceled write. It stops once on the paced whole-turn drain, flush/interruption, fatal write failure, or close. Unit-final frames do not stop speaking, so multi-unit turns do not flicker. `SpeakingStateFrame` is provider-neutral and maps directly to RTVI `bot-started-speaking` / `bot-stopped-speaking`. These events describe server output state, not client playback acknowledgement.

## Flush barrier

A flush removes queued interruptible output through its target generation and aborts targeted work that is still selected or pacing. Admission into the transport write is atomic with the final generation/drop/closed check. Once a write is transport-bound, the default real-WebSocket path waits for it to return and acknowledges only after quiescence; it does not cancel coder/websocket's write context because that closes the connection. An explicitly configured recoverable writer may opt into cancellation. Repeated and overlapping flushes retain independent callbacks for their requested generation. A late physical write return is fenced before metrics, speaking state, or write completion. A stale or no-op flush cannot stop newer-generation speaking or reset its retained pacing clock.

Explicitly uninterruptible output is retained in place, even when its generation is older than the active fence. New-generation output appends after retained media and is paced normally. Run cancellation and close clear queued accounting and balance speaking state exactly once. An unresponsive transport-bound writer reaches the bounded `ErrSlowClient` path and the connection closes without a false successful flush acknowledgement.

The server guarantee ends at bytes already accepted by the socket. The pinned unmodified browser client still cannot clear buffered playback; `docs/compatibility/playback-clear-decision.json` therefore remains unresolved and is not a pass.
