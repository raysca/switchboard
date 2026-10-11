package server

import (
	"govoice/server/agent"
	"govoice/server/catalog"
)

// providerAvailability answers "can this build and environment run provider/service".
type providerAvailability struct {
	cat      *catalog.Catalog
	res      *catalog.Resolver
	supports func(provider, service string) bool
}

func (a providerAvailability) Check(provider, service string) agent.Unavailable {
	p, ok := a.cat.Provider(provider)
	if !ok {
		return agent.Unavailable{Reason: "unknown_provider"}
	}
	if _, ok := p.Services[service]; !ok || !a.supports(provider, service) {
		return agent.Unavailable{Reason: "unsupported_provider"}
	}
	if st := a.res.Status(provider, service); !st.Detected {
		return agent.Unavailable{Reason: "missing_credential", MissingEnv: st.MissingEnv}
	}
	return agent.Unavailable{}
}
