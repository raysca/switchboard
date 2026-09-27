// Package call assembles the standard POC call pipeline:
// VAD -> FakeSTT -> FakeLLM -> FakeTTS, fed by a real-time audio stream.
package call

import (
	"path/filepath"
	"time"

	"govoice/loadgen"
	"govoice/vad"
	"govoice/voice"
)

type Runtime struct {
	M    *voice.Manager
	Exec *vad.Executor
	Host *vad.HostMetrics
	Lib  *loadgen.Library
	Pool *loadgen.Pool
}

func New(cfg vad.Config, feeders int) (*Runtime, error) {
	ex, err := vad.New(cfg)
	if err != nil {
		return nil, err
	}
	lib, err := loadgen.Load(filepath.Join(vad.Root(), "assets", "speech"), 0.0005)
	if err != nil {
		return nil, err
	}
	return &Runtime{M: voice.NewManager(), Exec: ex, Host: &vad.HostMetrics{}, Lib: lib, Pool: loadgen.NewPool(feeders)}, nil
}

func (r *Runtime) Close() {
	r.Pool.Stop()
	for _, s := range r.M.List() {
		s.Stop()
	}
	r.Exec.Close()
	r.M.Close()
}

type Opts struct {
	Fakes       bool          // add STT/LLM/TTS
	FakeLatency time.Duration // base provider latency
	Feed        bool          // attach a real-time audio stream
	Duty        float64
	Seed        uint64
	Gate        float32 // VAD energy gate RMS (0 = off)
	Budget      voice.Budget
}

// Call is a running session plus its handles.
type Call struct {
	*loadgen.Stream
	VAD   *vad.Processor
	Fakes []*voice.Fake
}

func (r *Runtime) NewCall(o Opts) (*Call, voice.StartTimings, error) {
	c := &Call{Stream: &loadgen.Stream{}}
	pc := vad.DefaultProcessorConfig()
	pc.EnergyGate = o.Gate
	pc.PassAudio = o.Fakes
	s, tm, err := r.M.Create(voice.Spec{Budget: o.Budget, Build: func(s *voice.Session) ([]voice.Processor, error) {
		c.VAD = vad.NewProcessor(r.Exec, pc, r.Host)
		ps := []voice.Processor{c.VAD}
		if o.Fakes {
			lat := o.FakeLatency
			c.Fakes = []*voice.Fake{
				{Name: "stt", Kind: voice.FakeSTT, Latency: lat},
				{Name: "llm", Kind: voice.FakeLLM, Latency: 2 * lat},
				{Name: "tts", Kind: voice.FakeTTS, Latency: lat},
			}
			for _, f := range c.Fakes {
				ps = append(ps, f)
			}
		}
		return ps, nil
	}})
	if err != nil {
		return nil, tm, err
	}
	c.S = s
	st := c.Stream
	s.OnOutput = func(f voice.Frame) {
		switch v := f.(type) {
		case *voice.SpeechStartedFrame:
			st.Mu.Lock()
			st.Detections = append(st.Detections, v.EmittedAt)
			st.Mu.Unlock()
		case *voice.AudioFrame:
			if v.Output {
				st.Mu.Lock()
				st.TTSFrames++
				st.Mu.Unlock()
			}
		}
	}
	if o.Feed {
		st.TL = loadgen.NewTimeline(r.Lib, o.Seed, o.Duty)
		// Random phase within one chunk so sessions don't tick in lockstep, and
		// join the conversation at a random point of its speech/silence cycle so
		// sessions created together don't all start speaking together.
		var k0 uint64
		if o.Duty > 0 && o.Duty < 1 {
			cycle := uint64(100 / o.Duty) // ~avg clip (100 chunks) / duty
			k0 = (o.Seed * 2654435761) % cycle
		}
		st.Start = time.Now().Add(-time.Duration(k0)*loadgen.ChunkDur - time.Duration(o.Seed*7919%32000)*time.Microsecond)
		st.SkipTo(k0)
		r.Pool.Add(st)
	}
	return c, tm, nil
}

// Fault applies a named fault to the call. Returns false if unknown.
func (c *Call) Fault(mode, processor string) bool {
	if mode == "vadabuse" {
		c.VAD.SetAbuse()
		return true
	}
	m, ok := voice.ParseMode(mode)
	if !ok || len(c.Fakes) == 0 {
		return false
	}
	target := c.Fakes[0]
	for _, f := range c.Fakes {
		if f.Name == processor {
			target = f
		}
	}
	target.SetMode(m)
	return true
}
