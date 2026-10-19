package server

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"govoice/server/api"
	"govoice/server/catalog"
)

// probeInterval is the least time between two checks of one provider.
const probeInterval = 5 * time.Second

// probeGate keeps a provider to one check at a time and at least probeInterval apart, so the button
// cannot be used to hammer a provider or to trip its circuit breaker faster than traffic would.
type probeGate struct {
	mu   sync.Mutex
	last map[string]time.Time // start of the latest check; one entry per catalog provider
	busy map[string]bool
}

func (g *probeGate) try(provider string) (retryAfter time.Duration, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.busy[provider] {
		return probeInterval, false
	}
	if at, seen := g.last[provider]; seen {
		if wait := probeInterval - time.Since(at); wait > 0 {
			return wait, false
		}
	}
	if g.last == nil {
		g.last, g.busy = map[string]time.Time{}, map[string]bool{}
	}
	g.last[provider], g.busy[provider] = time.Now(), true
	return 0, true
}

func (g *probeGate) done(provider string) {
	g.mu.Lock()
	delete(g.busy, provider)
	g.mu.Unlock()
}

type providerTestDTO struct {
	Provider  string        `json:"provider"`
	CheckedAt time.Time     `json:"checked_at"`
	Results   []ProbeResult `json:"results"`
}

// handleProviderTest runs one minimal request against each service the provider offers and has a credential for.
func (s *Server) handleProviderTest(w http.ResponseWriter, r *http.Request) {
	requestID := api.RequestID(w, r)
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		api.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not supported", requestID)
		return
	}
	id := r.PathValue("id")
	p, found := s.catalog.Provider(id)
	if !found {
		api.WriteError(w, http.StatusNotFound, "not_found", "provider not found", requestID)
		return
	}
	if !s.resolver.Detected(id) {
		api.WriteError(w, http.StatusConflict, "provider_not_detected", "no credential for this provider is set in the server environment", requestID)
		return
	}
	var stages []string
	for _, name := range []string{catalog.ServiceSTT, catalog.ServiceLLM, catalog.ServiceTTS} {
		if p.Services[name] != nil && s.registry.Supports(id, name) && s.resolver.Status(id, name).Detected {
			stages = append(stages, name)
		}
	}
	if len(stages) == 0 {
		api.WriteError(w, http.StatusConflict, "provider_not_detected", "no service of this provider is usable on this server", requestID)
		return
	}
	if wait, ok := s.probes.try(id); !ok {
		secs := int(wait/time.Second) + 1
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		api.WriteError(w, http.StatusTooManyRequests, "probe_too_soon", "this provider was checked a moment ago; try again in "+strconv.Itoa(secs)+" s", requestID)
		return
	}
	defer s.probes.done(id)
	results := make([]ProbeResult, len(stages))
	var wg sync.WaitGroup
	for i, stage := range stages {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if rec := recover(); rec != nil { // a provider bug must not take the daemon down
					results[i] = ProbeResult{Stage: stage, Status: "error", Detail: "internal: the check failed unexpectedly", Circuit: s.metrics.circuitState(id, stage)}
				}
			}()
			results[i] = s.probeStage(r.Context(), id, stage)
		}()
	}
	wg.Wait()
	api.WriteJSON(w, http.StatusOK, api.SuccessResponse[providerTestDTO]{Data: providerTestDTO{Provider: id, CheckedAt: time.Now().UTC(), Results: results}})
}
