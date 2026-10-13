package server

import (
	"govoice/server/catalog"
	"govoice/server/flow"
)

// providerAvailability answers "can this build and environment run provider/service".
type providerAvailability struct {
	cat      *catalog.Catalog
	res      *catalog.Resolver
	supports func(provider, service string) bool
}

func (a providerAvailability) Check(provider, service string) flow.Unavailable {
	p, ok := a.cat.Provider(provider)
	if !ok {
		return flow.Unavailable{Reason: "unknown_provider"}
	}
	if _, ok := p.Services[service]; !ok || !a.supports(provider, service) {
		return flow.Unavailable{Reason: "unsupported_provider"}
	}
	if st := a.res.Status(provider, service); !st.Detected {
		return flow.Unavailable{Reason: "missing_credential", MissingEnv: st.MissingEnv}
	}
	return flow.Unavailable{}
}
