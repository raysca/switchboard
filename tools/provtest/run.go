package provtest

import (
	"fmt"
	"io"
	"time"
)

// Report is one whole run.
type Report struct {
	Mode     string    `json:"mode"` // "live" or "dry"
	At       time.Time `json:"at"`
	MaxCalls int       `json:"max_calls"`
	Calls    int       `json:"calls"`           // planned units (estimate)
	Actual   int       `json:"actual_requests"` // measured at the provider boundary
	Repeat   int       `json:"repeat"`
	Results  []Result  `json:"results"`
}

// Resolver turns a table row into a runnable target or a skip reason.
type Resolver func(Provider) (Target, string)

// Run executes the suite over table (already filtered) and returns the report.
// A provider that cannot run is recorded as skipped, never failed. When the
// call budget is spent the remaining targets are skipped.
func Run(table []Provider, resolve Resolver, o Options, repeat int, mode string, log io.Writer) Report {
	if repeat < 1 {
		repeat = 1
	}
	rep := Report{Mode: mode, At: time.Now().UTC(), Repeat: repeat}
	if o.Budget != nil {
		rep.MaxCalls = o.Budget.max
	}
	for _, p := range table {
		t, why := resolve(p)
		if why == "" && o.Budget.Exhausted() {
			why = "not run: " + ErrBudget.Error() // abort: no further provider is built or called
		}
		if why != "" {
			rep.Results = append(rep.Results, Result{Provider: p.ID, Stage: p.Stage, Status: Skip, Reason: why})
			fmt.Fprintf(log, "%-10s %-4s skip: %s\n", p.ID, p.Stage, why)
			continue
		}
		var runs []Result
		for i := 0; i < repeat; i++ {
			runs = append(runs, RunTarget(t, o))
		}
		res := Merge(runs)
		rep.Results = append(rep.Results, res)
		fails := 0
		for _, c := range res.Checks {
			if c.Status == Fail {
				fails++
			}
		}
		fmt.Fprintf(log, "%-10s %-4s %s (%d checks, %d failed, %d units (upper-bound estimate), %d actual requests, %.1fs)\n", p.ID, p.Stage, res.Status, len(res.Checks), fails, res.Calls, res.Actual, res.Seconds)
	}
	rep.Calls = o.Budget.Used()
	rep.Actual = o.Budget.Actual()
	fmt.Fprintf(log, "actual provider requests: %d (estimate %d, max-calls %d)\n", rep.Actual, rep.Calls, rep.MaxCalls)
	return rep
}
