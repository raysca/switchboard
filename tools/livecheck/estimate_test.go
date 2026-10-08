package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func prof(tts string) Profile {
	var p Profile
	p.Stages.TTS.Provider = tts
	return p
}

// The calibration case: xAI S2/S4 (2 turns + a barge-in/resume each) made ~30-35 real requests
// together after being estimated at 15; the upper bound must now cover that.
func TestEstCallsCoversMeasuredXAI(t *testing.T) {
	s2 := estCalls(shape{turns: 2, extraSTT: 1}, prof("xai"))
	s4 := estCalls(shape{turns: 2, extraSTT: 1}, prof("xai"))
	if s2+s4 < 35 {
		t.Errorf("S2+S4 xai upper bound %d < measured 35", s2+s4)
	}
	if s2 != 6+2+10 {
		t.Errorf("S2 xai = %d, want stt 6 + llm 2 + tts 10", s2)
	}
}

func TestEstCallsTerms(t *testing.T) {
	for _, c := range []struct {
		sh   shape
		tts  string
		want int
	}{
		{shape{}, "xai", 1}, // call-start STT stream only
		{shape{turns: 1, noSTT: true}, "elevenlabs", 1 + 2},   // llm + 2 tts
		{shape{turns: 1, extraLLM: 1}, "google", 3 + 2 + 4},   // tool round adds an LLM request
		{shape{turns: 3, extraSTT: 1}, "cartesia", 8 + 3 + 6}, // stt 1+2*3+1
	} {
		if got := estCalls(c.sh, prof(c.tts)); got != c.want {
			t.Errorf("%+v %s = %d want %d", c.sh, c.tts, got, c.want)
		}
	}
	f := prof("xai")
	f.Fake = true
	if estCalls(shape{turns: 5}, f) != 0 {
		t.Error("fake profile must estimate 0")
	}
}

func TestSpend(t *testing.T) {
	s := newSpend(10, 3) // 3 clip syntheses already made
	if s.total(4) != 7 || s.over(4) || s.exhausted(4) {
		t.Error("7 of 10")
	}
	s.finish(5) // a stopped server made 5
	if s.total(0) != 8 || s.exhausted(1) || !s.exhausted(2) || s.over(2) || !s.over(3) {
		t.Errorf("total %d", s.total(0))
	}
	var nilS *spend
	if nilS.over(1000) || nilS.total(5) != 5 {
		t.Error("nil spend must not limit")
	}
	if newSpend(0, 0).over(1 << 20) {
		t.Error("max 0 means no limit")
	}
}

// abort on actual: the watcher fires as soon as the measured count passes the cap, once.
func TestWatchSpendAbortsOnActual(t *testing.T) {
	var reqs, fired atomic.Int64
	s := newSpend(5, 0)
	stop := watchSpend(s, func() (int, bool) { return int(reqs.Load()), true }, 5*time.Millisecond, func() { fired.Add(1) })
	defer stop()
	reqs.Store(5)
	time.Sleep(40 * time.Millisecond)
	if fired.Load() != 0 {
		t.Fatal("fired at exactly the cap")
	}
	reqs.Store(6)
	time.Sleep(60 * time.Millisecond)
	if fired.Load() != 1 {
		t.Fatalf("fired %d times, want 1", fired.Load())
	}
	// no cap: never fires
	var f2 atomic.Int64
	stop2 := watchSpend(newSpend(0, 0), func() (int, bool) { return 1 << 20, false }, time.Millisecond, func() { f2.Add(1) })
	time.Sleep(10 * time.Millisecond)
	stop2()
	if f2.Load() != 0 {
		t.Error("no cap must never abort")
	}
}

func TestReqDeltaAndTable(t *testing.T) {
	d := reqDelta(map[string]int{"xai/tts": 2, "xai/llm": 1}, map[string]int{"xai/tts": 7, "xai/llm": 1, "xai/stt": 3})
	if d["xai/tts"] != 5 || d["xai/stt"] != 3 || len(d) != 2 {
		t.Errorf("delta = %v", d)
	}
	if reqDelta(nil, nil) != nil {
		t.Error("empty delta must be nil")
	}
	rep := &Report{Scenarios: []string{"S1", "S2"}, Results: []*Result{
		{Profile: "xai", Scenario: "S1", Estimate: 20, Requests: 9, ReqBy: map[string]int{"xai/tts": 9}},
		{Profile: "xai", Scenario: "S1", Estimate: 20, Requests: 11, ReqBy: map[string]int{"xai/tts": 11}},
		{Profile: "fake", Scenario: "S1"},
	}}
	var b strings.Builder
	renderRequests(func(f string, a ...any) { b.WriteString(fmt.Sprintf(f, a...)) }, rep)
	out := b.String()
	for _, want := range []string{"| xai | S1 | 40 | 20 |", "| **xai** | **total** | **40** | **20** |", "`xai/tts` 20"} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "fake") {
		t.Error("fake profile must not appear")
	}
}

// The scrape regexp matches the exact line core/netpool.WriteMetrics emits.
func TestRequestsScrapeLine(t *testing.T) {
	m := reqRe.FindStringSubmatch(`voice_provider_requests_total{provider="xai",stage="tts"} 12`)
	if m == nil || m[1] != "xai" || m[2] != "tts" || m[3] != "12" {
		t.Errorf("match = %v", m)
	}
}

// A server child whose /metrics counter grows past the cap trips the watcher; once /metrics stops
// answering, the watcher fails closed (abort) instead of treating the spend as zero.
func TestWatchSpendAgainstMetricsEndpoint(t *testing.T) {
	var n, fired atomic.Int64
	var down atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if down.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, "voice_provider_requests_total{provider=\"xai\",stage=\"tts\"} %d\nvoice_provider_requests_total{provider=\"xai\",stage=\"llm\"} 1\n", n.Add(1))
	}))
	defer ts.Close()
	srv := &server{admin: ts.URL}
	s := newSpend(6, 2) // 2 clip syntheses: trips once tts passes 3 (2 + tts + 1 llm > 6)
	stop := watchSpend(s, srv.requestTotal, 5*time.Millisecond, func() { fired.Add(1) })
	defer stop()
	deadline := time.Now().Add(2 * time.Second)
	for fired.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if fired.Load() != 1 {
		t.Fatal("growing counter never tripped the cap")
	}
	if got, ok := srv.requestTotal(); !ok || got < 5 {
		t.Fatalf("total %d ok %v", got, ok)
	}

	down.Store(true)
	fired.Store(0)
	stop2 := watchSpend(newSpend(1000, 0), srv.requestTotal, 5*time.Millisecond, func() { fired.Add(1) })
	defer stop2()
	time.Sleep(60 * time.Millisecond)
	if fired.Load() != 1 {
		t.Fatal("unreadable counters must fail closed (abort)")
	}
	if last, ok := srv.requestTotal(); ok || last == 0 {
		t.Fatalf("unreadable scrape: ok %v last %d", ok, last)
	}
}

// Every recorded live actual must sit at or under the estimator (upper bound). Sources, with the
// smallest scenario mapping each doc allows (fewer scenarios = lower estimate = stricter check):
//   - xai-live.md "Live rerun": S2+S4 on xAI, about 30-35 requests (cached clips).
//   - xai-live.md agent matrix: S1,S2,S4,S5,S7 on xAI (+ reruns, not counted here), about 41 incl. 5 clip syntheses.
//   - turn-manager-live.md round 2: S1 and S4 on Google, 3 variants (+ S4 reruns, not counted), 18+13+17 = 48.
//   - docs/benchmarks/README.md smoke: Google S1 off, about 7.
var knownActuals = []struct {
	name   string
	tts    string
	runs   map[string]int // scenario id -> runs
	actual int
}{
	{"xai S2+S4 rerun", "xai", map[string]int{"S2": 1, "S4": 1}, 35},
	{"xai agent pass", "xai", map[string]int{"S1": 1, "S2": 1, "S4": 1, "S5": 1, "S7": 1}, 41 - 5},
	{"google round 2", "google", map[string]int{"S1": 3, "S4": 3}, 18 + 13 + 17},
	{"google S1 smoke", "google", map[string]int{"S1": 1}, 7},
	// K5 S9 long reply, one cell each (off, merge): xai 15+9, google 9+7 (merge cut at the cap)
	{"xai S9 off+merge", "xai", map[string]int{"S9": 2}, 15 + 9},
	{"google S9 off+merge", "google", map[string]int{"S9": 2}, 9 + 7},
}

func TestEstimateCoversKnownActuals(t *testing.T) {
	o := &opts{s1Turns: 2}
	for _, k := range knownActuals {
		est := 0
		for _, sc := range scenarios {
			est += k.runs[sc.id] * estCalls(sc.shape(o), prof(k.tts))
		}
		t.Logf("%-18s est %3d actual %3d", k.name, est, k.actual)
		if est < k.actual {
			t.Errorf("%s: estimate %d < known actual %d", k.name, est, k.actual)
		}
	}
}
