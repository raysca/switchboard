//go:build live

package provtest

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"govoice/examples/envload"
)

// go test -tags live ./tools/provtest/ with GOVOICE_LIVE=1 and examples/.env.
// PROVTEST_PROVIDERS=google,nvidia PROVTEST_STAGES=stt,tts,llm PROVTEST_MAX_CALLS=60 narrow it.
// Spends real provider credit; providers without credentials are skipped.
func TestLiveProviders(t *testing.T) {
	if os.Getenv("GOVOICE_LIVE") != "1" {
		t.Skip("set GOVOICE_LIVE=1 to spend real provider credit")
	}
	_ = os.Chdir("../..")
	_ = envload.Load()
	RegisterEnvSecrets(os.Environ())
	clips, err := LoadClips()
	if err != nil {
		t.Fatal(err)
	}
	max := 60
	if v, err := strconv.Atoi(os.Getenv("PROVTEST_MAX_CALLS")); err == nil {
		max = v
	}
	o := Options{Clips: clips, Budget: NewBudget(max)}
	var ids []string
	var stages []Stage
	for _, s := range strings.Split(os.Getenv("PROVTEST_PROVIDERS"), ",") {
		if s != "" {
			ids = append(ids, s)
		}
	}
	for _, s := range strings.Split(os.Getenv("PROVTEST_STAGES"), ",") {
		if s != "" {
			stages = append(stages, Stage(s))
		}
	}
	for _, p := range Filter(Table, ids, stages) {
		t.Run(p.ID+"/"+string(p.Stage), func(t *testing.T) {
			tg, why := Live(p, os.Getenv)
			if why != "" {
				t.Skip(why)
			}
			res := RunTarget(tg, o)
			for _, c := range res.Checks {
				if c.Status == Fail {
					t.Errorf("%s: %s", c.Name, c.Detail)
				}
			}
		})
	}
}
