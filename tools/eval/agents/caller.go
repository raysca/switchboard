package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"govoice/providers/xai"
	"govoice/server/eval"
)

const (
	callerTimeout    = 60 * time.Second
	callerOffline    = "offline"
	callerXAI        = "xai"
	defaultCallerURL = "http://127.0.0.1:11434/v1/responses" // Ollama, development only
	defaultCallerLLM = "govoice-test"
)

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
func newAsk(c config) (eval.AskFunc, func(), error) {
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
