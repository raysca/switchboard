// govoice-turn-eval prints false-cut/late-cut rates and enforces the committed
// thresholds. Default: offline strategies with FAKE oracles (free, CI).
// Real model (costs money): GOVOICE_TURN_EVAL_REAL=1 govoice-turn-eval -oracle-cmd ./my-llm.sh
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"govoice/tools/eval/turnquality"
)

func main() {
	oracleCmd := flag.String("oracle-cmd", "", "program (space-separated argv) that answers one utterance with a real model; needs GOVOICE_TURN_EVAL_REAL=1")
	name := flag.String("name", "llm-completion-real", "strategy name for the real run (threshold key)")
	flag.Parse()
	rs, err := turnquality.RunDefault()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if *oracleCmd != "" {
		if os.Getenv("GOVOICE_TURN_EVAL_REAL") != "1" {
			fmt.Fprintln(os.Stderr, "-oracle-cmd calls a real model and costs money: set GOVOICE_TURN_EVAL_REAL=1 to confirm")
			os.Exit(2)
		}
		us, _ := turnquality.Sample()
		rs = append(rs, turnquality.CompletionRates(*name, us, turnquality.CommandOracle(strings.Fields(*oracleCmd), 60*time.Second)))
	}
	th, err := turnquality.DefaultThresholds()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fails := th.Check(rs)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{"results": rs, "failures": fails})
	if len(fails) > 0 {
		os.Exit(1)
	}
}
