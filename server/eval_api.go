package server

import (
	"errors"
	"fmt"
	"strings"

	"govoice/server/api"
	"govoice/server/catalog"
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
		LiveEnabled: func() bool { return s.env("GOVOICE_LIVE") == "1" },
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
