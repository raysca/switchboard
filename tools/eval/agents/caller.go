package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"govoice/providers/xai"
)

const (
	maxCallerLine    = 300 // runes of one persona line
	maxHistoryLines  = 24  // transcript lines shown to the persona model
	maxHistoryRunes  = 400 // runes of one transcript line shown to it
	callerTimeout    = 60 * time.Second
	callerOffline    = "offline"
	callerXAI        = "xai"
	defaultCallerURL = "http://127.0.0.1:11434/v1/responses" // Ollama, development only
	defaultCallerLLM = "govoice-test"
)

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

// AskFunc is one plain, tool-less completion: system and user prompt in, text out.
type AskFunc func(ctx context.Context, system, user string) (string, error)

// PersonaCaller plays p with one completion per turn. It ends on the model's lone DONE or after
// p.MaxTurns lines. The model sees the persona, the goal and the transcript only (never the agent's
// configuration) and has no tools; agent text reaches it, so a bounded single completion is the
// whole exposure.
func PersonaCaller(p Persona, agentName string, ask AskFunc) NextFunc {
	limit := min(max(p.MaxTurns, 1), maxPersonaTurn)
	system := fmt.Sprintf("You are a person making a phone call to %q. You are: %s. What you want: %s.\n"+
		"Reply with ONE short sentence, exactly what you would say next. No quotes, no stage directions, no lists. "+
		"Answer the questions you are asked. When your goal is met or the other side has said goodbye, reply with exactly DONE.",
		agentName, p.Who, p.Goal)
	turns := 0
	return func(ctx context.Context, transcript []Line) (string, bool, error) {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		if turns >= limit {
			return "", true, nil
		}
		turns++
		out, err := ask(ctx, system, personaPrompt(transcript))
		if err != nil {
			return "", false, fmt.Errorf("persona model: %w", err)
		}
		line := firstLine(out)
		if line == "" {
			return "", false, errors.New("persona model: empty reply")
		}
		if strings.EqualFold(strings.Trim(line, " .!?*`'\""), "DONE") {
			return "", true, nil
		}
		return truncRunes(line, maxCallerLine), false, nil
	}
}

// personaPrompt renders the latest transcript lines, each bounded.
func personaPrompt(t []Line) string {
	if len(t) == 0 {
		return "The call has just connected. Say your first line."
	}
	t = t[max(len(t)-maxHistoryLines, 0):]
	var b strings.Builder
	for _, l := range t {
		who := "Other side"
		if l.Who == "caller" {
			who = "You"
		}
		fmt.Fprintf(&b, "%s: %s\n", who, truncRunes(l.Text, maxHistoryRunes))
	}
	b.WriteString("What do you say next?")
	return b.String()
}

// firstLine is the first non-empty line of s, without surrounding quotes.
func firstLine(s string) string {
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.Trim(strings.TrimSpace(l), "\"'`"); l != "" {
			return l
		}
	}
	return ""
}

func truncRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// callerNeedsLive reports whether the persona model is paid: xAI always, and the offline (Ollama)
// kind only when its URL is a loopback host. An unparsable URL counts as remote.
func callerNeedsLive(llm, rawURL string) bool {
	if llm != callerOffline {
		return true
	}
	if rawURL == "" {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return true
	}
	switch strings.ToLower(u.Hostname()) {
	case "127.0.0.1", "localhost", "::1":
		return false
	}
	return true
}

// newAsk builds the persona model client for c. The returned close func releases it. The xAI key
// comes from the environment and is sent only to the pinned xAI endpoint.
func newAsk(c config) (AskFunc, func(), error) {
	cfg := xai.Config{Endpoint: c.CallerURL, Model: c.CallerModel, APIKey: "offline", Timeout: callerTimeout}
	if c.CallerLLM == callerXAI {
		if cfg.APIKey = os.Getenv("XAI_API_KEY"); cfg.APIKey == "" {
			return nil, nil, errors.New("-caller-llm xai needs XAI_API_KEY in the environment")
		}
		cfg.Endpoint, cfg.Model = "", c.CallerModel // pinned endpoint; model default is the pinned one
	} else {
		if cfg.Endpoint == "" {
			cfg.Endpoint = defaultCallerURL
		}
		if cfg.Model == "" {
			cfg.Model = defaultCallerLLM
		}
	}
	// A redirect is never followed: the key and the prompt go only where the operator pointed them. The
	// 3xx then reads as a non-200 answer, an error.
	cfg.HTTPClient = &http.Client{Timeout: callerTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	p, err := xai.NewProcessor(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("persona model: %w", err)
	}
	return p.SummarizeText, func() { _ = p.Close() }, nil
}
