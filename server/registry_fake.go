//go:build fakeproviders

package server

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"time"

	"govoice/core"
)

// Test-only build (go build -tags fakeproviders): every google.* stage is a
// fake, so black-box tests (tools/eval/pipecat) need no network or keys.
// Text turns only: STT passes frames through. Never ship this binary.
func init() {
	registerFakeProviders = func(r *Registry) {
		slog.Warn("FAKE PROVIDERS: google.stt/llm/tts are fakes (built with -tags fakeproviders)")
		r.Register("google.stt", func(*Config, StageSpec) (core.Processor, error) {
			if os.Getenv("GOVOICE_FAKE_STT") == "audio" {
				// Load harness (tools/loadtest): any audio frame counts as one word
				// (Samples[0] indexes Words), so detected speech yields a transcript.
				words := make([]string, 256)
				for i := range words {
					words[i] = "hello"
				}
				return core.NewFakeSTTProcessor(core.FakeSTTConfig{Provider: "google", Words: words}), nil
			}
			return passSTT{}, nil
		})
		r.Register("google.llm", func(_ *Config, spec StageSpec) (core.Processor, error) {
			// Echo the user's text so scenarios prove their input reached the LLM.
			// GOVOICE_FAKE_LLM_DELAY (a Go duration) delays every chunk: only the
			// perf-gate self-test (make perf-gate-selftest) sets it, to prove a
			// latency regression fails the gate. Exists only in this build tag.
			delay, _ := time.ParseDuration(os.Getenv("GOVOICE_FAKE_LLM_DELAY"))
			cfg := core.FakeLLMConfig{ResponseChunks: []string{"You said: "}, EchoUser: true, ChunkDelay: delay, Executor: spec.ToolExecutor,
				ToolOnPhrase: map[string]string{}, AfterTool: "Connecting you."}
			// Graph agents: GOVOICE_FAKE_LLM_LABEL=1 prefixes each reply with the node's
			// system prompt in brackets, so a test can tell which agent answered; a user
			// message "transfer to <node name>" makes the fake call that node's transfer tool.
			if os.Getenv("GOVOICE_FAKE_LLM_LABEL") != "" {
				cfg.Label = "[" + spec.SystemPrompt + "] "
			}
			for _, d := range spec.Tools {
				if node, ok := strings.CutPrefix(d.Name, "transfer_to_"); ok {
					cfg.ToolOnPhrase["transfer to "+strings.ReplaceAll(node, "_", " ")] = d.Name
				}
			}
			return core.NewFakeLLMProcessor(cfg), nil
		})
		r.Register("google.tts", func(_ *Config, spec StageSpec) (core.Processor, error) {
			// A node whose voice is "brokenvoice" cannot be built: the e2e suite uses it to
			// make a handoff fail. Exists only in this build tag.
			if spec.Voice == "brokenvoice" {
				return nil, errors.New("fake tts: voice brokenvoice is unavailable")
			}
			return core.NewFakeTTSProcessor(core.FakeTTSConfig{FramesPerUnit: 2}), nil
		})
	}
}

// passSTT forwards frames untouched (core.PassthroughProcessor records every
// frame, which grows without bound on a live audio stream).
type passSTT struct{}

func (passSTT) ProcessFrame(_ context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
	return e.PushDirection(f, d)
}
