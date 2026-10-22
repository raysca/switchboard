package server

import (
	"context"
	"io"
	"sync"

	"govoice/core"
	"govoice/providers/google"
	"govoice/server/agent"
)

// silenceProvider is the text-only speech provider (docs/decisions/silence-provider.md): a flow that
// names it for both speech stages takes typed turns (RTVI send-text) and answers in text, for clients
// that listen and speak in the browser themselves. It needs no secret.
const silenceProvider = agent.SilenceProvider

// registerSilence registers silence.stt and silence.tts. Like registerEcho the stages are the real
// Google processors (retries, breaker, usage, tracing, unit splitting) on tiny in-process backends,
// with breakers of their own so a silence outage never touches echo's. The model is chosen separately.
func registerSilence(r *Registry) {
	r.Register("silence.stt", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		sttCfg := googleSTTConfig(cfg, spec, r.silenceSTTBreaker)
		sttCfg.Provider = silenceProvider
		sttCfg.StreamOpener = silenceSTTOpener{}
		return google.NewSTTProcessor(sttCfg)
	})

	r.Register("silence.tts", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		ttsCfg := googleTTSConfig(cfg, spec, r.silenceTTSBreaker)
		ttsCfg.Provider = silenceProvider
		ttsCfg.Client = silenceTTSClient{}
		return googleTTSProcessor(cfg, ttsCfg)
	})
}

// silenceSTTOpener opens streams that never transcribe: typed turns bypass speech recognition, and
// audio a client sends anyway is ignored.
type silenceSTTOpener struct{}

func (silenceSTTOpener) OpenStream(ctx context.Context, _ google.STTConfig) (google.STTStream, error) {
	ctx, cancel := context.WithCancel(ctx)
	return &silenceSTTStream{done: ctx.Done(), cancel: cancel, sent: make(chan struct{})}, nil
}

type silenceSTTStream struct {
	// done closes when the stream's context ends or Close is called; the context itself is not kept (AGENTS.md).
	done     <-chan struct{}
	cancel   context.CancelFunc
	sent     chan struct{}
	sendOnce sync.Once
}

func (*silenceSTTStream) Send([]byte) error { return nil }

func (s *silenceSTTStream) CloseSend() error {
	s.sendOnce.Do(func() { close(s.sent) })
	return nil
}

func (s *silenceSTTStream) Close() error {
	_ = s.CloseSend()
	s.cancel()
	return nil
}

// Recv yields no result: it ends with io.EOF once the audio ended, or context.Canceled when closed.
func (s *silenceSTTStream) Recv() ([]*google.STTResult, error) {
	select {
	case <-s.done:
		return nil, context.Canceled
	case <-s.sent:
		select { // a closed stream answers nothing, even if the audio had ended too
		case <-s.done:
			return nil, context.Canceled
		default:
			return nil, io.EOF
		}
	}
}

// silenceTTSClient synthesizes one 20 ms frame of silence per unit, with no delay: the turn's speaking
// events open and close at once and a reply costs no audio time.
type silenceTTSClient struct{}

func (silenceTTSClient) SynthesizeSpeech(ctx context.Context, _, _ string, sampleRate int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return silenceFrame(sampleRate), nil
}

func (silenceTTSClient) StreamSynthesize(ctx context.Context, pieces []string, _ string, sampleRate int, onAudio func([]byte) error) error {
	for range pieces {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := onAudio(silenceFrame(sampleRate)); err != nil {
			return err
		}
	}
	return nil
}

// silenceFrame is 20 ms of PCM16LE mono silence.
func silenceFrame(sampleRate int) []byte { return make([]byte, sampleRate/50*2) }
