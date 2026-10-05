package main

import (
	"encoding/json"
	"os"

	"govoice/tools/eval"
)

func main() {
	report, err := eval.RunAll()
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	if encodeErr := enc.Encode(report); encodeErr != nil {
		os.Exit(2)
	}
	if err != nil || eval.ExitCode(report) != 0 {
		os.Exit(1)
	}
}
