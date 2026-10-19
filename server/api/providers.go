package api

import (
	"net/http"

	"govoice/server/catalog"
)

// ProvidersHandler serves the read-only provider listing. It reports whether
// credentials are detected, never their values.
type ProvidersHandler struct {
	Catalog  *catalog.Catalog
	Resolver *catalog.Resolver
	Supports func(provider, service string) bool
}

type serviceView struct {
	Available  bool            `json:"available"`
	Reason     string          `json:"reason,omitempty"`
	MissingEnv []string        `json:"missing_env,omitempty"`
	Models     []catalog.Entry `json:"models,omitempty"`
	Voices     []catalog.Entry `json:"voices,omitempty"`
	Languages  []catalog.Entry `json:"languages,omitempty"`
	Custom     bool            `json:"custom"`
}

type providerView struct {
	ID           string                 `json:"id"`
	DisplayName  string                 `json:"display_name"`
	Capabilities []string               `json:"capabilities"`
	Detected     bool                   `json:"detected"`
	Env          []string               `json:"env"`
	Services     map[string]serviceView `json:"services"`
}

// envOrEmpty keeps "env" a JSON array (never null) for providers with no variables.
func envOrEmpty(env []string) []string {
	if env == nil {
		return []string{}
	}
	return env
}

func (h ProvidersHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := RequestID(w, r)
	if r.Method != http.MethodGet {
		flowMethodNotAllowed(w, requestID, http.MethodGet)
		return
	}
	ps := h.Catalog.Providers()
	out := make([]providerView, 0, len(ps))
	for _, p := range ps {
		v := providerView{
			ID: p.ID, DisplayName: p.Name, Capabilities: []string{},
			Detected: h.Resolver.Detected(p.ID), Env: envOrEmpty(h.Resolver.Env(p.ID)),
			Services: make(map[string]serviceView, len(p.Services)),
		}
		for _, name := range []string{catalog.ServiceSTT, catalog.ServiceLLM, catalog.ServiceTTS} {
			svc, ok := p.Services[name]
			if !ok {
				continue
			}
			sv := serviceView{Models: svc.Models, Voices: svc.Voices, Languages: svc.Languages, Custom: svc.AllowsCustom()}
			st := h.Resolver.Status(p.ID, name)
			switch {
			case !h.Supports(p.ID, name):
				sv.Reason = "not_in_build"
			case !st.Detected:
				sv.Reason, sv.MissingEnv = "missing_env", st.MissingEnv
			default:
				sv.Available = true
				v.Capabilities = append(v.Capabilities, name)
			}
			v.Services[name] = sv
		}
		out = append(out, v)
	}
	WriteJSON(w, http.StatusOK, SuccessResponse[[]providerView]{Data: out})
}
