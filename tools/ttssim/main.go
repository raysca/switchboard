package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
)

type fixture struct {
	Results []struct {
		Provider  string `json:"provider"`
		TTSTiming struct {
			Units []struct {
				Chars  int     `json:"chars"`
				WallMS float64 `json:"wall_ms"`
				Audio  float64 `json:"audio_ms"`
			} `json:"units"`
		} `json:"tts_timing"`
	} `json:"results"`
}

// limits: Google is pinned at 250 in providers/google/tts.go (longer units are split into extra requests).
// xAI: no limit pinned in this repo; 600 is a practical cap (a longer unit only delays first audio).
var limits = map[string]int{"google": 250, "xai": 600}

var strategies = []Strategy{
	{Name: "current (min10)", Min: 10},
	{Name: "min40", Min: 40}, {Name: "min60", Min: 60}, {Name: "min80", Min: 80}, {Name: "min120", Min: 120},
	{Name: "slack f0 m300", Min: 10, Slack: true, MarginMS: 300},
	{Name: "slack f20 m300", Min: 10, FirstClause: 20, Slack: true, MarginMS: 300},
	{Name: "slack f30 m300", Min: 10, FirstClause: 30, Slack: true, MarginMS: 300},
	{Name: "slack f40 m300", Min: 10, FirstClause: 40, Slack: true, MarginMS: 300},
	{Name: "slack f30 m0", Min: 10, FirstClause: 30, Slack: true, MarginMS: 0},
	{Name: "slack f30 m600", Min: 10, FirstClause: 30, Slack: true, MarginMS: 600},
	{Name: "slack f30 m1000", Min: 10, FirstClause: 30, Slack: true, MarginMS: 1000},
	{Name: "slack f30 m600 cap120", Min: 10, FirstClause: 30, Slack: true, MarginMS: 600, CapChars: 120},
	{Name: "slack f30 m600 cap200", Min: 10, FirstClause: 30, Slack: true, MarginMS: 600, CapChars: 200},
}

func main() {
	fix := flag.String("fixture", "tools/ttssim/testdata/tts_timing.json", "provtest results with tts_timing")
	n := flag.Int("n", 300, "replies per cell")
	ttft := flag.Float64("ttft", 600, "LLM first-token delay ms")
	cpsF := flag.String("cps", "40,80,150", "LLM chars/sec values, pooled")
	ovs := flag.String("overheads", "300,800,1000", "assumed fixed per-request overhead ms (sensitivity; the 3 short units cannot identify it)")
	flag.Parse()
	if *n < 1 {
		*n = 1
	}
	b, err := os.ReadFile(*fix)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var f fixture
	if err := json.Unmarshal(b, &f); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, r := range f.Results {
		var ch, wall, au float64
		for _, u := range r.TTSTiming.Units {
			ch += float64(u.Chars)
			wall += u.WallMS
			au += u.Audio
		}
		if ch == 0 {
			continue
		}
		lim := limits[r.Provider]
		if lim == 0 {
			lim = 250 // unknown provider: conservative, and never 0 (send divides by it)
		}
		k := float64(len(r.TTSTiming.Units))
		meanCh, meanWall := ch/k, wall/k
		fmt.Printf("## %s: measured %d short units, mean %.0f chars -> %.0f ms wall, %.1f ms audio/char (limit %d chars)\n\n",
			r.Provider, len(r.TTSTiming.Units), meanCh, meanWall, au/ch, lim)
		for _, o := range strings.Split(*ovs, ",") {
			var ov float64
			fmt.Sscan(o, &ov)
			if ov >= meanWall {
				continue
			}
			p := Provider{Name: r.Provider, OverheadMS: ov, Rate: meanCh / ((meanWall - ov) / 1000), AudioPerCh: au / ch / 1000, MaxChars: lim}
			fmt.Printf("### %s overhead %.0f ms + chars/%.0f cps (calibrated: %.0f chars = %.0f ms)\n\n", p.Name, ov, p.Rate, meanCh, meanWall)
			table(p, *n, *ttft, *cpsF)
		}
	}
}

func table(p Provider, n int, ttft float64, cpsF string) {
	var cpss []float64
	for _, c := range strings.Split(cpsF, ",") {
		var v float64
		fmt.Sscan(c, &v)
		cpss = append(cpss, v)
	}
	fmt.Println("| strategy | first audio mean/p95 s | gap-free replies | gaps/reply | gap s/reply | requests/reply | chars/reply |")
	fmt.Println("|---|---|---|---|---|---|---|")
	for _, st := range strategies {
		var fa []float64
		var gaps, gapS, req, chars float64
		free, tot := 0, 0
		for _, cps := range cpss {
			r := rand.New(rand.NewSource(1))
			for i := 0; i < n; i++ {
				res := Run(Reply(r), LLM{CPS: cps, TTFTMS: ttft, DeltaMS: 60}, p, st)
				tot++
				fa = append(fa, res.FirstAudio)
				gaps += float64(res.Gaps)
				gapS += res.GapS
				req += float64(res.Requests)
				chars += float64(res.Chars)
				if res.Gaps == 0 {
					free++
				}
			}
		}
		sort.Float64s(fa)
		t := float64(tot)
		fmt.Printf("| %s | %.2f / %.2f | %.0f%% | %.2f | %.2f | %.2f | %.0f |\n", st.Name, mean(fa), fa[int(.95*t)], 100*float64(free)/t, gaps/t, gapS/t, req/t, chars/t)
	}
	fmt.Println()
}

func mean(a []float64) float64 {
	s := 0.0
	for _, v := range a {
		s += v
	}
	return s / float64(len(a))
}
