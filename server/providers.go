package server

import "govoice/server/catalog"

// unavailable says why a provider service cannot run; the zero value means it can.
type unavailable struct {
	Reason     string
	MissingEnv []string
}

// providerAvailability answers "can this build and environment run provider/service".
type providerAvailability struct {
	cat      *catalog.Catalog
	res      *catalog.Resolver
	supports func(provider, service string) bool
}

func (a providerAvailability) Check(provider, service string) unavailable {
	p, ok := a.cat.Provider(provider)
	if !ok {
		return unavailable{Reason: "unknown_provider"}
	}
	if _, ok := p.Services[service]; !ok || !a.supports(provider, service) {
		return unavailable{Reason: "unsupported_provider"}
	}
	if st := a.res.Status(provider, service); !st.Detected {
		return unavailable{Reason: "missing_credential", MissingEnv: st.MissingEnv}
	}
	return unavailable{}
}
