package server

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"govoice/core/netpool"
)

// populatedScrape is a /metrics body with every conditional series present
// (otel, smart-turn workers, Nvidia breakers, provider errors, netpool).
func populatedScrape(t *testing.T) string {
	t.Helper()
	s := apiTestServer(t)
	s.metrics.OTelStats = func() (uint64, uint64) { return 0, 0 }
	s.metrics.SmartTurnWorkers = func() (int, int) { return 1, 1 }
	s.registry.nvidiaBreaker("llm|" + nvidiaNIMBaseURL)
	s.metrics.providerErrors.add("nvidia", "llm", 1)
	netpool.CountRequest("google", "llm")
	return scrape(t, s)
}

// Names that appear in the docs but are label values or prose, not series.
var notMetrics = map[string]bool{"voice_to_voice": true}

var metricName = regexp.MustCompile(`\bvoice_[a-z0-9_]+`)

func familyOf(name string) string {
	for _, suf := range []string{"_bucket", "_sum", "_count"} {
		if strings.HasSuffix(name, suf) {
			return strings.TrimSuffix(name, suf)
		}
	}
	return name
}

// Every voice_* metric the docs and the shipped alert rules name must be emitted by /metrics (M4-04),
// and every label matcher in an alert expression must match an emitted label value.
func TestDocsAndAlertRulesNameRealMetrics(t *testing.T) {
	scraped := populatedScrape(t)
	families := map[string]bool{}
	for _, line := range strings.Split(scraped, "\n") {
		if m := metricName.FindString(line); m != "" && !strings.HasPrefix(line, "#") {
			families[familyOf(m)] = true
		}
		if strings.HasPrefix(line, "# TYPE ") {
			families[strings.Fields(line)[2]] = true
		}
	}

	var bad []string
	check := func(file, text string) {
		for _, m := range metricName.FindAllString(text, -1) {
			if notMetrics[m] || families[familyOf(m)] {
				continue
			}
			bad = append(bad, file+": "+m)
		}
	}
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for _, f := range []string{"../docs/observability.md", "../docs/operations_runbook.md", "../docs/deployment.md"} {
		check(f, read(f))
	}

	rules := read("../deploy/alerts/govoice.rules.yml")
	var doc struct {
		Groups []struct {
			Rules []struct {
				Alert string `yaml:"alert"`
				Expr  string `yaml:"expr"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal([]byte(rules), &doc); err != nil {
		t.Fatalf("alert rules do not parse: %v", err)
	}
	selector := regexp.MustCompile(`(voice_[a-z0-9_]+)\{([^}]*)\}`)
	matcher := regexp.MustCompile(`(\w+)="([^"]*)"`)
	n := 0
	for _, g := range doc.Groups {
		for _, r := range g.Rules {
			n++
			if r.Alert == "" || r.Expr == "" {
				t.Errorf("rule without alert/expr: %+v", r)
			}
			check("alert "+r.Alert, r.Expr)
			for _, sel := range selector.FindAllStringSubmatch(r.Expr, -1) {
				for _, kv := range matcher.FindAllStringSubmatch(sel[2], -1) {
					want := kv[1] + `="` + kv[2] + `"`
					found := false
					for _, line := range strings.Split(scraped, "\n") {
						if strings.HasPrefix(line, sel[1]) && strings.Contains(line, want) {
							found = true
							break
						}
					}
					if !found {
						bad = append(bad, "alert "+r.Alert+": no series "+sel[1]+"{"+want+"}")
					}
				}
			}
		}
	}
	if n == 0 {
		t.Fatal("no alert rules found")
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		t.Fatalf("docs/alerts name metrics the server does not emit:\n%s", strings.Join(bad, "\n"))
	}
}
