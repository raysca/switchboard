package server

import (
	"errors"
	"slices"
	"time"

	"govoice/core"
	"govoice/core/audio/dtmf"
	"govoice/transport/ws"
)

// dtmfStages are a call's optional DTMF processors (see Config.DTMF); nil
// fields are disabled.
type dtmfStages struct {
	in  core.Processor // before VAD: tones in inbound audio -> InputDTMF
	agg core.Processor // before user context: key presses -> user message
	out core.Processor // before transport: OutputDTMF -> tone audio
}

// buildDTMF constructs the enabled stages. With SendTool it appends the
// send_dtmf tool to the primary LLM and every fallback of call (a local copy),
// routing it to the output stage.
func buildDTMF(cfg *Config, call *CallSpec) (dtmfStages, error) {
	var st dtmfStages
	if cfg == nil {
		return st, nil
	}
	if cfg.DTMF.Detect {
		p, err := dtmf.NewInputProcessor(dtmf.DefaultDetectorConfig())
		if err != nil {
			return st, err
		}
		st.in = p
	}
	if cfg.DTMF.Aggregate {
		ac := dtmf.DefaultAggregatorConfig()
		if cfg.DTMF.AggregateTimeout > 0 {
			ac.Timeout = cfg.DTMF.AggregateTimeout
		}
		st.agg = dtmf.NewAggregator(ac)
	}
	if cfg.DTMF.SendTool {
		rate := cfg.Providers.GoogleTTS.SampleRate // the pipeline's single TTS output rate
		if rate <= 0 {
			rate = 24000
		}
		out, err := dtmf.NewOutputProcessor(dtmf.OutputConfig{Tones: dtmf.DefaultGeneratorConfig(rate)})
		if err != nil {
			return st, err
		}
		st.out = out
		add := func(s *StageSpec) {
			s.Tools = append(slices.Clone(s.Tools), dtmf.ToolDefinition())
			s.ToolExecutor = dtmf.Executor(out, s.ToolExecutor)
		}
		add(&call.LLM)
		call.LLMFallbacks = slices.Clone(call.LLMFallbacks)
		for i := range call.LLMFallbacks {
			add(&call.LLMFallbacks[i])
		}
	}
	return st, nil
}

// onDTMF enters client-sent key presses (RTVI dtmf) as InputDTMF frames at the
// head of the call pipeline, exactly like detected tones.
func (s *Server) onDTMF(conn *ws.Connection, digits string) error {
	if !s.cfg.DTMF.Aggregate && !s.cfg.DTMF.Detect {
		return errors.New("dtmf input is disabled")
	}
	session := conn.Session()
	if session == nil {
		return errors.New("no active session")
	}
	for i := 0; i < len(digits); i++ {
		if err := session.Post(&core.InputDTMFFrame{Digit: digits[i : i+1], Timestamp: time.Now()}); err != nil {
			return err
		}
	}
	return nil
}
