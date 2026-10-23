package main

import "context"

// NextFunc is the simulated caller: given the transcript so far it returns the next line to type,
// or done when the call should end. A script and (later) a persona model both fit it.
type NextFunc func(ctx context.Context, transcript []Line) (line string, done bool, err error)

// ScriptCaller types the fixed lines in order, then reports done.
func ScriptCaller(lines []string) NextFunc {
	i := 0
	return func(ctx context.Context, _ []Line) (string, bool, error) {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		if i >= len(lines) {
			return "", true, nil
		}
		i++
		return lines[i-1], false, nil
	}
}
