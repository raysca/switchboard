package provtest

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/netpool"
	"govoice/tools/internal/wav"
)

func dryOpts(t *testing.T) Options {
	clips, err := LoadClips()
	if err != nil {
		t.Fatal(err)
	}
	return Options{Clips: clips, Budget: NewBudget(100), Fast: true, Idle: 50 * time.Millisecond, Timeout: 10 * time.Second}
}

func check(res Result, name string) *Check {
	for i := range res.Checks {
		if res.Checks[i].Name == name {
			return &res.Checks[i]
		}
	}
	return nil
}

func TestWER(t *testing.T) {
	for _, c := range []struct {
		ref, hyp string
		want     float64
	}{
		{"What is two plus two?", "what is 2 + 2", 0},
		{"What is the capital of France?", "What is the capital of France", 0},
		{"a b c d", "a x c", 0.5},
		{"a b", "", 1},
		{"What's two-plus-two?", "what is two plus two", 0},
		{"It’s 'Paris'.", "it is paris", 0},
		{"Two plus two equals four", "2+2 equals 4", 0},
		{"THE CAPITAL", "the capital", 0},
	} {
		if got := WER(c.ref, c.hyp); got != c.want {
			t.Errorf("WER(%q,%q)=%v want %v", c.ref, c.hyp, got, c.want)
		}
	}
}

func TestClipFixtures(t *testing.T) {
	clips, err := LoadClips()
	if err != nil {
		t.Fatal(err)
	}
	if len(clips) != 4 {
		t.Fatalf("%d clips, want 4", len(clips))
	}
	var size int64
	files, _ := filepath.Glob("testdata/clips/*")
	for _, f := range files {
		st, _ := os.Stat(f)
		size += st.Size()
	}
	if size > 150*1024 {
		t.Fatalf("fixtures are %d bytes (> 150 KB)", size)
	}
	for _, c := range clips {
		if secs := float64(len(c.PCM)) / 2 / clipRate; secs < 1 || secs > 12 || wav.RMS(c.PCM) < 300 {
			t.Errorf("clip %s: %.1fs rms %.0f", c.ID, secs, wav.RMS(c.PCM))
		}
	}
	if len(Words(clips[2].Text)) != 12 {
		t.Errorf("clip 3 is %d words, want 12", len(Words(clips[2].Text)))
	}
}

// Every stage's suite passes against well-behaved fakes.
func TestDryConformant(t *testing.T) {
	o := dryOpts(t)
	for _, st := range AllStages {
		t.Run(string(st), func(t *testing.T) {
			res := RunTarget(FakeTarget(st, "fake", o.Clips, FaultNone), o)
			for _, c := range res.Checks {
				if c.Status != Pass {
					t.Errorf("%s: %s %s", c.Name, c.Status, c.Detail)
				}
			}
			if len(res.Checks) < 8 {
				t.Errorf("only %d checks ran", len(res.Checks))
			}
		})
	}
}

// The checks have teeth: each deliberately broken fake fails the matching check.
func TestDryBrokenFakesFail(t *testing.T) {
	for _, c := range []struct {
		stage Stage
		fault Fault
		check string
	}{
		{TTS, FaultNoFinal, "final_flags"},
		{TTS, FaultNoInterrupt, "interrupt"},
		{TTS, FaultLeak, "close_leaks"},
		{STT, FaultDropSecond, "resumed_speech"},
		{STT, FaultDropSecond, "wer"},
		{LLM, FaultNoUsage, "usage"},
		{LLM, FaultNoInterrupt, "cancel"},
		{TTS, FaultWrongRate, "format"},
		{TTS, FaultSilent, "non_silent"},
		{TTS, FaultRIFF, "no_riff"},
		{STT, FaultNoSTTFinal, "wer"},
		{STT, FaultNotCumul, "cumulative"},
		{LLM, FaultBadToolArgs, "tool_round_trip"},
		{TTS, FaultAuthServer, "bad_credential"},
		{STT, FaultAuthServer, "bad_credential"},
		{LLM, FaultAuthServer, "bad_credential"},
		{TTS, FaultEchoSecret, "bad_credential"},
		{LLM, FaultEchoSecret, "bad_model"},
	} {
		t.Run(string(c.stage)+"/"+string(c.fault)+"/"+c.check, func(t *testing.T) {
			o := dryOpts(t)
			if c.fault == FaultNoSTTFinal {
				o.Timeout = time.Second // the final wait is bounded by it
			}
			res := RunTarget(FakeTarget(c.stage, "broken", o.Clips, c.fault), o)
			got := check(res, c.check)
			if got == nil || got.Status != Fail {
				t.Fatalf("%s did not fail: %+v", c.check, got)
			}
			if res.Status != Fail {
				t.Fatal("result should fail")
			}
			for _, ch := range res.Checks {
				if strings.Contains(ch.Detail, fakeBadSecret) {
					t.Errorf("%s detail echoes the credential: %s", ch.Name, ch.Detail)
				}
			}
		})
	}
}

func TestBudgetStopsRun(t *testing.T) {
	o := dryOpts(t)
	o.Budget = NewBudget(2)
	res := RunTarget(FakeTarget(TTS, "fake", o.Clips, FaultNone), o)
	skipped := false
	for _, c := range res.Checks {
		skipped = skipped || (c.Status == Skip && strings.Contains(c.Detail, "budget"))
	}
	if !skipped || o.Budget.Used() > 2 {
		t.Fatalf("budget not enforced: used=%d checks=%+v", o.Budget.Used(), res.Checks)
	}
}

// A run the budget cut short is not a pass, and once the budget is spent no
// further target is built or called.
func TestBudgetAbortsRemainingTargets(t *testing.T) {
	o := dryOpts(t)
	o.Budget = NewBudget(4) // TTS needs 5: synth 3, interrupt 1, bad_credential 1
	built := 0
	rep := Run(Filter(Table, []string{"google", "nvidia"}, []Stage{TTS}), func(p Provider) (Target, string) {
		tg := FakeTarget(p.Stage, p.ID, o.Clips, FaultNone)
		b := tg.Build
		tg.Build = func(v Variant) (core.Processor, error) { built++; return b(v) }
		return tg, ""
	}, o, 1, "dry", io.Discard)
	if rep.Calls != 4 || len(rep.Results) != 2 {
		t.Fatalf("calls=%d results=%d", rep.Calls, len(rep.Results))
	}
	if r := rep.Results[0]; r.Status != Skip || !strings.Contains(r.Reason, "budget") {
		t.Errorf("first: %s %q, want an incomplete skip", r.Status, r.Reason)
	}
	if r := rep.Results[1]; r.Status != Skip || !strings.Contains(r.Reason, "budget") || len(r.Checks) != 0 {
		t.Errorf("second: %s %q %d checks, want skipped before building", r.Status, r.Reason, len(r.Checks))
	}
	builtFirst := built
	if builtFirst == 0 || builtFirst > 3 {
		t.Errorf("built %d processors", builtFirst)
	}
}

// A target that ran nothing (no clips) is a skip, not a pass.
func TestEmptyRunIsNotAPass(t *testing.T) {
	o := dryOpts(t)
	o.Clips = nil
	if res := RunTarget(FakeTarget(STT, "fake", nil, FaultNone), o); res.Status != Skip {
		t.Fatalf("status %s, want skip", res.Status)
	}
}

// Clips are decoded to PCM before use, and the committed ADPCM costs little:
// speech re-encoded off the codec grid (gain 0.9; the decoded clips themselves
// re-encode losslessly) keeps > 30 dB SNR, the order of codec noise providers hear.
func TestClipCodecSNR(t *testing.T) {
	clips, err := LoadClips()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range clips {
		in := make([]byte, len(c.PCM))
		for i := 0; i+1 < len(in); i += 2 {
			v := int16(float64(int16(uint16(c.PCM[i])|uint16(c.PCM[i+1])<<8)) * 0.9)
			in[i], in[i+1] = byte(v), byte(uint16(v)>>8)
		}
		got, _, err := wav.ReadWAV(wav.EncodeIMAWAV(in, clipRate))
		if err != nil || len(got) != len(in) {
			t.Fatalf("%s: %v", c.ID, err)
		}
		var sig, noise float64
		for i := 0; i+1 < len(got); i += 2 {
			a, b := float64(int16(uint16(in[i])|uint16(in[i+1])<<8)), float64(int16(uint16(got[i])|uint16(got[i+1])<<8))
			sig += a * a
			noise += (a - b) * (a - b)
		}
		snr := 10 * log10(sig/noise)
		t.Logf("%s: %.1f dB", c.ID, snr)
		if snr < 30 {
			t.Errorf("%s: SNR %.1f dB", c.ID, snr)
		}
	}
}

func TestMissingEnvSkips(t *testing.T) {
	rep := Run(Filter(Table, []string{"deepgram", "cartesia"}, nil), func(p Provider) (Target, string) {
		return Live(p, func(string) string { return "" })
	}, Options{Budget: NewBudget(1)}, 1, "live", io.Discard)
	if len(rep.Results) == 0 {
		t.Fatal("no rows")
	}
	for _, r := range rep.Results {
		if r.Status != Skip || r.Reason == "" {
			t.Errorf("%s.%s: %s %q, want skip with a reason", r.Provider, r.Stage, r.Status, r.Reason)
		}
	}
	if rep.Calls != 0 {
		t.Fatal("skipped providers must not spend calls")
	}
}

func TestLiveResolveBuildsThroughServerRegistry(t *testing.T) {
	env := func(k string) string {
		return map[string]string{"NVIDIA_API_KEY": "nvapi-0123456789abcdef", "OPENAI_API_KEY": "sk-test-0123456789"}[k]
	}
	for _, p := range Filter(Table, []string{"nvidia", "openai"}, nil) {
		tg, why := Live(p, env)
		if why != "" {
			t.Fatalf("%s.%s: %s", p.ID, p.Stage, why)
		}
		proc, err := tg.Build(Variant{})
		if err != nil {
			t.Fatalf("%s.%s build: %v", p.ID, p.Stage, err)
		}
		if c, ok := proc.(interface{ Close() error }); ok {
			_ = c.Close()
		}
		if _, err := tg.Build(Variant{BadCredential: true}); err != nil && strings.Contains(err.Error(), tg.BadSecret) {
			t.Errorf("%s.%s: build error echoes the credential", p.ID, p.Stage)
		}
	}
}

// Results never carry a secret: literal env values and token shapes are scrubbed.
func TestResultsHoldNoSecrets(t *testing.T) {
	t.Setenv("PROVTEST_FAKE_API_KEY", "zz-canary-secret-value-123")
	RegisterEnvSecrets(os.Environ())
	rep := Report{Mode: "dry", At: time.Now(), Results: []Result{{Provider: "x", Stage: TTS, Status: Fail, Checks: []Check{
		{Name: "bad_credential", Status: Fail, Detail: Redact("err: zz-canary-secret-value-123 Bearer abc.def AIzaSyABCDEFGHIJKLMNOP nvapi-xyzxyzxyz")}}}}}
	for name, out := range map[string]string{"json": string(rep.JSON()), "markdown": rep.Markdown()} {
		if HasSecret(out) || Leaks(strings.ReplaceAll(out, "[redacted]", "")) && secretRe.MatchString(out) {
			t.Errorf("%s output leaks: %s", name, out)
		}
	}
	var back Report
	if json.Unmarshal(rep.JSON(), &back) != nil {
		t.Fatal("results.json must stay valid JSON after redaction")
	}
}

// Committed results (if any) contain no registered secret value.
func TestCommittedFilesHoldNoSecrets(t *testing.T) {
	root := "../.."
	if b, err := os.ReadFile(filepath.Join(root, "examples/.env")); err == nil {
		for _, ln := range strings.Split(string(b), "\n") {
			if k, v, ok := strings.Cut(ln, "="); ok && !strings.HasPrefix(k, "#") {
				RegisterEnvSecrets([]string{strings.TrimSpace(k) + "=" + strings.Trim(strings.TrimSpace(v), `"'`)})
			}
		}
	}
	RegisterEnvSecrets(os.Environ())
	files, _ := filepath.Glob("testdata/clips/*.json")
	files = append(files, filepath.Join(root, "docs/benchmarks/provider-matrix.md"), filepath.Join(root, "provtest-results.json"))
	for _, f := range files {
		if b, err := os.ReadFile(f); err == nil && HasSecret(string(b)) {
			t.Errorf("%s contains a secret value", f)
		}
	}
}

// The budget caps on MEASURED provider requests, not only on planned units: a provider that
// makes more requests than planned (per-sentence TTS, reopened STT streams) stops the run.
func TestBudgetStopsOnActualRequests(t *testing.T) {
	b := NewBudget(5)
	if err := b.Spend(1); err != nil || b.Actual() != 0 {
		t.Fatalf("fresh budget: %v actual=%d", err, b.Actual())
	}
	for i := 0; i < 5; i++ {
		netpool.CountRequest("budgettest", "tts") // 5 real requests for 1 planned unit
	}
	if b.Actual() != 5 || !b.Exhausted() {
		t.Errorf("actual=%d exhausted=%v", b.Actual(), b.Exhausted())
	}
	if err := b.Spend(1); !errors.Is(err, ErrBudget) || !strings.Contains(err.Error(), "actual") {
		t.Errorf("Spend after 5 actual requests = %v", err)
	}
	if NewBudget(0).Exhausted() {
		t.Error("max 0 means unlimited")
	}
}

// TTS rows carry tts_timing from the existing synth requests; the matrix shows it; old files without it merge.
func TestTTSTiming(t *testing.T) {
	o := dryOpts(t)
	res := RunTarget(FakeTarget(TTS, "fake", o.Clips, FaultNone), o)
	tm := res.TTSTiming
	if tm == nil || len(tm.Units) != len(ttsSentences) || tm.Chars != totalChars() {
		t.Fatalf("tts_timing = %+v", tm)
	}
	if tm.TTFAMS <= 0 || tm.WallMS < tm.TTFAMS || tm.CharsPerSec <= 0 {
		t.Errorf("implausible timing %+v", tm)
	}
	if want := float64(totalChars()) * 60; tm.AudioMS < want*0.99 || tm.AudioMS > want*1.01 { // fake: 60 ms per char
		t.Errorf("audio_ms %.0f, want ~%.0f", tm.AudioMS, want)
	}
	if m := Merge([]Result{res, res, res}); m.TTSTiming == nil {
		t.Error("Merge dropped tts_timing")
	}
	rep := Report{Mode: "dry", Results: []Result{res}}
	if !strings.Contains(rep.Markdown(), "chars/s") || !strings.Contains(string(rep.JSON()), `"tts_timing"`) {
		t.Error("matrix or JSON lacks timing")
	}
	// old results.json (no tts_timing) merges and renders
	old := filepath.Join(t.TempDir(), "old.json")
	os.WriteFile(old, []byte(`{"mode":"live","results":[{"provider":"google","stage":"tts","status":"pass","calls":3,"seconds":1}]}`), 0o600)
	m, err := MergeFiles([]string{old}, Table, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.Markdown(), "| - | - | - |") {
		t.Error("old row should render dashes")
	}
}
