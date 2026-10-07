package dtmf

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"govoice/core"
)

func startSession(t *testing.T, procs ...core.Processor) *core.Session {
	t.Helper()
	pipe, err := core.NewPipeline(procs...)
	if err != nil {
		t.Fatal(err)
	}
	s, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func postPCM(t *testing.T, s *core.Session, b []byte, rate int) {
	t.Helper()
	step := rate / 50 * 2 // 20 ms
	for off := 0; off < len(b); off += step {
		c := b[off:min(off+step, len(b))]
		err := s.Post(&core.InputAudioFrame{Samples: c, Encoding: core.EncodingPCM16LE, SampleRate: rate, Channels: 1,
			Duration: time.Duration(len(c)/2) * time.Second / time.Duration(rate), Timestamp: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func finals(p *core.PassthroughProcessor) (out []string) {
	for _, f := range p.GetFrames() {
		if ft, ok := f.(*core.FinalTranscriptionFrame); ok {
			out = append(out, ft.Text)
		}
	}
	return
}

// Audio in -> detector -> InputDTMF + debug event -> aggregator -> committed
// user message in the conversation context and an UserMessageFrame for the LLM.
func TestPipelineDetectsAggregatesAndCommitsUserMessage(t *testing.T) {
	in, err := NewInputProcessor(DefaultDetectorConfig())
	if err != nil {
		t.Fatal(err)
	}
	ctxPool := core.NewConversationContext(core.ContextConfig{})
	sink := &core.PassthroughProcessor{}
	s := startSession(t, in, NewAggregator(AggregatorConfig{Timeout: time.Minute}),
		core.NewUserContextProcessor(ctxPool, core.UserContextConfig{RequireCommittedTranscript: true}), sink)
	var debugs chan core.DebugEvent = make(chan core.DebugEvent, 8)
	if _, _, err := s.Subscribe(8, func(e core.DebugEvent) {
		if e.Name == DebugDetected {
			debugs <- e
		}
	}); err != nil {
		t.Fatal(err)
	}
	pcm, _ := Generate("12#", DefaultGeneratorConfig(16000))
	postPCM(t, s, append(make([]byte, 3200), pcm...), 16000)

	eventually(t, "committed DTMF user message", func() bool {
		for _, m := range ctxPool.Messages() {
			if m.Role == core.RoleUser && m.Content == "DTMF: 12#" {
				return true
			}
		}
		return false
	})
	var digits string
	var msgs int
	for _, f := range sink.GetFrames() {
		switch v := f.(type) {
		case *core.InputDTMFFrame:
			digits += v.Digit
		case *core.UserMessageFrame:
			msgs++
		}
	}
	if digits != "12#" || msgs != 1 {
		t.Fatalf("digits %q user messages %d", digits, msgs)
	}
	for i := 0; i < 3; i++ {
		select {
		case e := <-debugs:
			if len(e.Attributes()) != 0 {
				t.Fatalf("debug event leaks attributes: %v", e.Attributes())
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d debug events", i)
		}
	}
}

func TestInputProcessorIgnoresOtherRates(t *testing.T) {
	in, _ := NewInputProcessor(DefaultDetectorConfig())
	sink := &core.PassthroughProcessor{}
	s := startSession(t, in, sink)
	pcm, _ := Generate("5", DefaultGeneratorConfig(24000))
	postPCM(t, s, pcm, 24000)
	eventually(t, "audio forwarded", func() bool { return sink.Count() > 5 })
	for _, f := range sink.GetFrames() {
		if f.Type() == core.FrameInputDTMF {
			t.Fatal("tone at an unexpected rate must not be analysed")
		}
	}
}

func TestAggregatorTimeoutFlushes(t *testing.T) {
	sink := &core.PassthroughProcessor{}
	s := startSession(t, NewAggregator(AggregatorConfig{Timeout: 40 * time.Millisecond}), sink)
	for _, d := range []string{"4", "2"} {
		if err := s.Post(&core.InputDTMFFrame{Digit: d}); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "timeout flush", func() bool { return len(finals(sink)) == 1 })
	if got := finals(sink)[0]; got != "DTMF: 42" {
		t.Fatalf("got %q", got)
	}
	// A later run is a separate message with its own turn.
	_ = s.Post(&core.InputDTMFFrame{Digit: "7"})
	eventually(t, "second flush", func() bool { return len(finals(sink)) == 2 })
	var turns = map[string]bool{}
	for _, f := range sink.GetFrames() {
		if ft, ok := f.(*core.FinalTranscriptionFrame); ok {
			turns[ft.Turn] = true
		}
	}
	if len(turns) != 2 {
		t.Fatalf("turns %v", turns)
	}
}

func TestAggregatorTerminatorFlushesImmediatelyAndInvalidPassesThrough(t *testing.T) {
	sink := &core.PassthroughProcessor{}
	s := startSession(t, NewAggregator(AggregatorConfig{Timeout: time.Minute}), sink)
	_ = s.Post(&core.InputDTMFFrame{Digit: "Z"}) // invalid: forwarded, never aggregated
	_ = s.Post(&core.InputDTMFFrame{Digit: "9"})
	_ = s.Post(&core.InputDTMFFrame{Digit: "#"})
	eventually(t, "terminator flush", func() bool { return len(finals(sink)) == 1 })
	if got := finals(sink)[0]; got != "DTMF: 9#" {
		t.Fatalf("got %q", got)
	}
}

func newOut(t *testing.T) (*OutputProcessor, *core.PassthroughProcessor, *core.Session) {
	t.Helper()
	out, err := NewOutputProcessor(OutputConfig{Tones: DefaultGeneratorConfig(24000)})
	if err != nil {
		t.Fatal(err)
	}
	sink := &core.PassthroughProcessor{}
	return out, sink, startSession(t, out, sink)
}

func audioBytes(p *core.PassthroughProcessor) (n int, gen uint64, ok bool) {
	ok = true
	for _, f := range p.GetFrames() {
		if a, isA := f.(*core.OutputAudioFrame); isA {
			n += len(a.Samples)
			gen = a.Gen
			ok = ok && a.NonSpeech && a.Turn == "" && a.SampleRate == 24000 && a.Channels == 1
		}
	}
	return
}

func TestOutputFrameBecomesPacedToneAudio(t *testing.T) {
	_, sink, s := newOut(t)
	if err := s.Post(&core.OutputDTMFFrame{Digits: "5#"}); err != nil {
		t.Fatal(err)
	}
	want, _ := Generate("5#", DefaultGeneratorConfig(24000))
	start := time.Now()
	eventually(t, "all tone audio", func() bool { n, _, _ := audioBytes(sink); return n == len(want) })
	n, gen, ok := audioBytes(sink)
	if !ok || n != len(want) || gen != s.CurrentGeneration() {
		t.Fatalf("bytes %d/%d gen %d ok %v", n, len(want), gen, ok)
	}
	// 320 ms of audio must take roughly that long (60 ms lead), not arrive in one burst.
	if el := time.Since(start); el < 200*time.Millisecond {
		t.Fatalf("tones released in %v, not paced", el)
	}
	for _, f := range sink.GetFrames() {
		if f.Type() == core.FrameOutputDTMF {
			t.Fatal("OutputDTMFFrame must be consumed, not forwarded")
		}
	}
}

func TestOutputStopsOnBargeIn(t *testing.T) {
	out, sink, s := newOut(t)
	if err := out.Send(0, "1234567890"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "first frames", func() bool { n, _, _ := audioBytes(sink); return n > 0 })
	s.IncrementGeneration()
	time.Sleep(100 * time.Millisecond)
	n1, _, _ := audioBytes(sink)
	time.Sleep(300 * time.Millisecond)
	n2, _, _ := audioBytes(sink)
	full, _ := Generate("1234567890", DefaultGeneratorConfig(24000))
	if n2 != n1 || n2 >= len(full)/2 {
		t.Fatalf("audio continued after interruption: %d -> %d of %d", n1, n2, len(full))
	}
}

func TestOutputStaleGenerationIsDropped(t *testing.T) {
	out, sink, s := newOut(t)
	s.IncrementGeneration()
	if err := out.Send(1, "5"); err != nil { // queued under the old generation
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if n, _, _ := audioBytes(sink); n != 0 {
		t.Fatalf("stale request played %d bytes", n)
	}
}

func TestSendDTMFToolValidationAndDelegation(t *testing.T) {
	out, sink, _ := newOut(t)
	var delegated string
	exec := Executor(out, core.ToolExecutorFunc(func(_ context.Context, c core.ToolCall) (core.ToolResult, error) {
		delegated = c.Name
		return core.ToolResult{CallID: c.ID, Status: core.ToolSucceeded, Result: json.RawMessage(`{}`)}, nil
	}))
	call := func(name, args string) core.ToolResult {
		r, err := exec.Execute(context.Background(), core.ToolCall{ID: "c1", Name: name, Arguments: json.RawMessage(args), Generation: 0})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	long := `{"digits":"` + string(make([]byte, 0)) + `123456789012345678901234567890123"}` // 33
	for name, args := range map[string]string{
		"empty": `{"digits":""}`, "missing": `{}`, "lowercase": `{"digits":"a"}`, "letter E": `{"digits":"1E"}`,
		"space": `{"digits":"1 2"}`, "too long": long, "wrong type": `{"digits":12}`, "not json": `nope`,
	} {
		if r := call(ToolName, args); r.Status != core.ToolFailed || r.ErrorCategory != "invalid_arguments" {
			t.Errorf("%s: %+v", name, r)
		}
	}
	if n, _, _ := audioBytes(sink); n != 0 {
		t.Fatal("rejected call produced audio")
	}
	// The call returns after playback, so a following reply cannot interleave.
	if r := call(ToolName, `{"digits":"0*#ABCD9"}`); r.Status != core.ToolSucceeded {
		t.Fatalf("valid call: %+v", r)
	}
	want, _ := Generate("0*#ABCD9", DefaultGeneratorConfig(24000))
	if n, _, _ := audioBytes(sink); n != len(want) {
		t.Fatalf("tool returned with %d of %d tone bytes released", n, len(want))
	}
	call("lookup", `{}`)
	if delegated != "lookup" {
		t.Fatal("other tools must reach the wrapped executor")
	}
	// Bounded backlog: a flood is refused, never queued without limit.
	var busy bool
	for i := 0; i < 3*maxPending && !busy; i++ {
		busy = out.Send(0, "1") == ErrBusy
	}
	if !busy {
		t.Fatal("pending sends are unbounded")
	}
	if r := call(ToolName, `{"digits":"1"}`); r.ErrorCategory != "busy" {
		t.Fatalf("full queue: %+v", r)
	}
}

func TestToolDefinitionSchemaMatchesValidation(t *testing.T) {
	d := ToolDefinition()
	var schema struct {
		Properties map[string]struct {
			Pattern   string `json:"pattern"`
			MaxLength int    `json:"maxLength"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(d.InputSchema, &schema); err != nil || d.Name != ToolName {
		t.Fatalf("bad definition: %v", err)
	}
	if p := schema.Properties["digits"]; p.MaxLength != MaxDigitsPerSend || p.Pattern != "^[0-9*#A-D]{1,32}$" {
		t.Fatalf("schema drifted from validation: %+v", p)
	}
}
