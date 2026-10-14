# Linux CI failures: root causes and fixes (branch fix/linux-ci)

Commits (on top of c6e858a, not pushed):

- c585d77 fix(netpool): detect a server-closed spare behind bytes the server already sent
- 5e1a31c fix(smartturn): run the quantized turn model in ORT x64 precision mode
- 3816d2d test(dtmf): wait for the user message frame at the sink, not only the context commit

## A. core/netpool TestClosedByServerSpareIsDiscarded (Linux only)

**Root cause.** `alive()` checked whether a spare was still open with a 1-byte `recv(MSG_PEEK|MSG_DONTWAIT)`. A TLS 1.3 server sends its session ticket right after the handshake, before any request, and an h2 server also sends SETTINGS. So the spare's socket always holds unread bytes, and a peek returns those bytes instead of the FIN or RST queued behind them. The dead spare was handed to the transport, and the request then failed with "http2: client conn could not be established" or "broken pipe".

The bug only showed on Linux because of how each kernel handles the receive queue:

- **macOS:** drops the receive queue on RST, so the peek returned ECONNRESET. Measured on macOS: 30/30 `peek n=-1 err=connection reset by peer`.
- **Linux:** keeps the queued bytes. Measured with instrumentation on Linux arm64:
  - 27 out of 30 runs saw the RST (pass).
  - 3 out of 30 runs got `peek n=1, 91 bytes queued` (the ticket), followed by FIN (fail).

The bug was RED on both linux/arm64 and linux/amd64 at -count=30.

**Fix.** `alive` (alive_unix.go) now reads ahead without blocking until it gets EAGAIN, EOF or an error, so it sees the close behind the data. The read-ahead is bounded to 16 KiB; if the buffer fills, the spare is assumed open, which is the old behaviour. Spares are dialed over a small `replayConn`, which hands the bytes it read ahead back to the first TLS Reads. TLS and HTTP/2 therefore see an unchanged stream, and the transport still gets a `*tls.Conn`. Only spares are wrapped; ordinary dials are unchanged. The non-unix stub is unchanged.

**RED/GREEN.**

- The existing test is RED on Linux, both arches.
- I added `TestSpareClosedAfterServerSpokeIsDiscarded`. It waits 100 ms so the server has finished its handshake and sent its ticket and SETTINGS, then closes cleanly. This is the data-then-EOF case, made deterministic. It is RED 10/10 on macOS with the old code, so macOS now covers it too.
- GREEN: `go test -race -count=30 ./core/netpool` passes on macOS arm64, linux/arm64 and linux/amd64 (Docker, golang:1.27.1-bookworm).

Production has predial off, but the code path is now correct.

## B. smartturn TestRealModelSmoke / TestPoolMatchesInProcess (Linux x86_64 only)

**Root cause.** This is a genuine x64 inference defect, not numeric noise.

- `smart_turn_v3.2_cpu.onnx` is a u8s8 QDQ model (459 DequantizeLinear, 312 QuantizeLinear). ORT fuses the QDQ pairs into int8 kernels.
- On x64 CPUs without VNNI, ORT's MLAS u8s8 GEMM uses AVX2 `VPMADDUBSW`. That instruction saturates the int16 pair sums, which is the known ORT u8s8 saturation issue.
- GitHub's ubuntu-24.04 runners are AMD EPYC (Zen 3): AVX2, no VNNI.

Evidence. All runs below use the pinned linux-x64 ORT 1.29.0 (sha256-verified) and the same test binary under `qemu-x86_64`. Reference values are from python ORT on macOS.

| Platform | silence | noise | golden clip | Result |
|---|---|---|---|---|
| Reference | — | 0.3747 | 0.2012 | — |
| qemu `-cpu qemu64` (SSE only) | 0.9870 | 0.3747 | — | PASS |
| Docker/Rosetta amd64 (SSE4, no AVX2) | — | — | — | PASS |
| linux/aarch64 | — | — | — | PASS |
| macOS arm64 | — | — | — | PASS |
| qemu `-cpu Haswell` (AVX2) | 0.9875 | 0.9008 | 0.9660364 | FAIL: exactly the CI numbers |
| qemu `-cpu EPYC-Milan` (AVX2, no VNNI) | 0.9875 | 0.9008 | 0.9660364 | FAIL: identical |

Ruled out:

- **Features:** bit-exact on Linux, both arches (`TestFeaturesGolden*`: max abs diff 0).
- **Model file:** hash-pinned and checked on load.
- **Golden fixture path:** fine.

Session options compared under emulated AVX2 (golden clip result):

| Option | Golden clip | Outcome |
|---|---|---|
| graph optimisation level DISABLE_ALL | 0.272 | Wrong |
| graph optimisation level BASIC | 0.272 | Wrong |
| `session.disable_quant_qdq=1` | 0.208, noise 0.52 | Wrong |
| `session.x64quantprecision=1` | matches reference to the 4 logged decimals on every signal | Fixed |

`session.x64quantprecision=1` is ORT's switch for exactly this problem. It converts the s8 weights to u8 so ORT runs exact u8u8 kernels. Non-x64 builds ignore the key.

**Fix.** `smartturn.NewRunner` sets `session.x64quantprecision=1`. NewRunner is the only smart-turn session constructor and serves both in-process and turnworker. Silero VAD is a float model and is unaffected. The golden test stays strict at 2e-3 with no platform carve-out.

**RED/GREEN.**

- RED: the qemu Haswell and EPYC-Milan runs without the fix (CI numbers reproduced exactly).
- GREEN: the same CPUs with the fix pass. `go test -race ./smartturn` passes on macOS arm64 and on Docker linux/amd64.
- TestPoolMatchesInProcess was not run under qemu, because its child worker would not run under the emulated AVX2 CPU. It goes through the same NewRunner.

### Finding: does Linux/x64 smart-turn behave materially differently?

**Yes, before this fix, on any x64 host without VNNI** (AVX2-only, or AVX-512 without VNNI): AMD EPYC Rome/Milan, Intel Haswell to Skylake.

- 3 s of low-level noise scored p(complete)=0.90 instead of 0.37.
- The golden speech-like clip scored 0.97 instead of 0.20.

So the analyzer would declare the turn complete mid-utterance and the bot would talk over callers. The silence score of 0.9875 is not the defect: the reference also scores silence about 0.987 "complete".

Cloud Run does not guarantee a CPU generation, so production linux/amd64 could land on an affected host.

### Is it on by default in production?

**No.** The turn backend is only built when the server config `turn.stop` contains `turn-analyzer` (cmd/server/main.go:89, server/registry.go:658).

- server/config_defaults.go sets `Turn.Manager=true` and the analyzer workers/stop_secs/budget, but never sets `turn.stop`.
- config/production.yaml and config/cloudrun.yaml have no `turn:` section.
- So the default stop chain is speech-timeout only. Exposure is limited to deployments that opt in.

### Recommendation

- Ship the precision-mode fix. It is exact and costs one session option.
- Keep the golden test strict.
- Before anyone enables `turn-analyzer` in production, re-check the latency budget (150 ms) on the real Cloud Run CPU. The u8u8 kernels are somewhat slower than the saturating u8s8 path on AVX2. Emulated timings here are meaningless, so I did not measure x64 latency.

## dtmf TestPipelineDetectsAggregatesAndCommitsUserMessage (macOS CI flake)

**Cause.** This is an obvious test race. `UserContextProcessor` appends the turn to the context and only then pushes `UserMessageFrame` to the next pipeline stage. The test waited for the context commit and then read the sink immediately, so it could see `user messages 0`; the CI failure happened 0.01 s into the test.

**Fix (test only).** The test now also waits for the frame at the sink. It passes 30/30 with -race on macOS. It was not reproducible locally, so it has no local RED.

Seen once and not chased: `TestSendDTMFToolValidationAndDelegation` failed once on linux/amd64 under Rosetta emulation with -race. I did not capture the message. It then passed 10/10 targeted runs and 5/5 full-package runs; it is untouched by these changes.

## Other notes

- Under Docker Desktop's linux/arm64 VM, `TestTurnWorkerExitsCleanOnStdinClose` fails. ORT prints "cpuid_info warning: Unknown CPU vendor" to stderr there, and the test requires empty stderr. This is an emulation artifact, unrelated to these changes; CI has no linux/arm64 job.
- Verification:
  - `gofmt -l core smartturn` is clean.
  - `go vet` and `go test -race` for core/netpool, core/audio/dtmf and smartturn pass on macOS.
  - On linux/amd64, netpool and smartturn pass; dtmf passed on rerun.
- AGENTS.md asks for an independent adversarial review. I was told not to dispatch subagents, so that review is still outstanding.
