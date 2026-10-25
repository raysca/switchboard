package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"govoice/server/api"
	"govoice/server/catalog"
	"govoice/server/eval"
)

// evalPath reports whether p is /api/v1/agents/{id}/eval or below it.
func evalPath(p string) bool {
	rest, ok := strings.CutPrefix(p, "/api/v1/agents/")
	if !ok {
		return false
	}
	id, sub, _ := strings.Cut(rest, "/")
	return id != "" && (sub == "eval" || strings.HasPrefix(sub, "eval/"))
}

// evalPolicy is what the eval API asks of the server: the live switch of this process (GOVOICE_LIVE=1, read from the
// server's own environment so a test can set it without touching the process's) and whether a provider can judge.
func (s *Server) evalPolicy() api.EvalPolicy {
	return api.EvalPolicy{
		LiveRequired:      s.evalLiveRequired,
		JudgeLiveRequired: s.judgeLiveRequired,
		JudgeProvider: func(provider string) error {
			p, ok := s.catalog.Provider(provider)
			if !ok {
				return fmt.Errorf("unknown provider %q", provider)
			}
			if p.Services[catalog.ServiceLLM] == nil {
				return errors.New("provider " + provider + " has no llm service")
			}
			return nil
		},
	}
}

// The one place that decides what is free. GOVOICE_LIVE must be exactly "1". The offline provider is free only
// while providers.offline.llm_endpoint is on this host: a remote endpoint is somebody's paid model, and the CLI
// treats a non-loopback caller URL the same way. The API (at enqueue and at judge save) and the worker (at run)
// all ask these, so they cannot drift.
func (s *Server) evalLive() bool { return s.env("GOVOICE_LIVE") == "1" }

func (s *Server) offlineFree() bool {
	u, err := url.Parse(s.cfg.Providers.Offline.LLMEndpoint)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return h == "localhost"
}

// evalLiveRequired: running this (rewritten) config reaches a paid model and this process has no GOVOICE_LIVE=1.
func (s *Server) evalLiveRequired(config json.RawMessage) bool {
	return eval.NeedsLive(config, s.offlineFree()) && !s.evalLive()
}

// judgeLiveRequired: a judge on this provider costs credit and this process has no GOVOICE_LIVE=1.
func (s *Server) judgeLiveRequired(provider string) bool {
	free := provider == "echo" || (provider == "offline" && s.offlineFree())
	return !free && !s.evalLive()
}
