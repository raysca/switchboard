package main

import (
	"slices"
	"testing"

	"govoice/server/catalog"
)

// Drift guard: for every vendor in credSpecs and every service the catalog declares for it, the
// variables livecheck hands the server are exactly the catalog's required ones (first credential
// kind plus the service's needs), i.e. what the server reports missing in an empty environment.
func TestCredSpecsMatchCatalog(t *testing.T) {
	res := catalog.NewResolver(providers, func(string) string { return "" })
	for vendor, vars := range credSpecs {
		p, ok := providers.Provider(vendor)
		if !ok {
			t.Errorf("%s: not in the catalog", vendor)
			continue
		}
		for svc := range p.Services {
			want := res.Status(vendor, svc).MissingEnv
			got := slices.Sorted(slices.Values(vars(svc)))
			if !slices.Equal(got, want) {
				t.Errorf("%s %s: livecheck uses %v, the catalog requires %v", vendor, svc, got, want)
			}
		}
	}
}

// The child gets the profile's variables (defaults filling gaps) and nothing else.
func TestProviderEnvIsLimitedToTheProfile(t *testing.T) {
	p := Profile{Requires: []string{"NVIDIA_API_KEY"}, Optional: []string{"NVIDIA_LLM_MODEL"}, Defaults: map[string]string{"NVIDIA_TTS_FUNCTION_ID": "def"}}
	got := providerEnv(p, envOf(map[string]string{"NVIDIA_API_KEY": "k", "XAI_API_KEY": "x", "NVIDIA_TTS_FUNCTION_ID": ""}))
	if want := []string{"NVIDIA_API_KEY=k", "NVIDIA_TTS_FUNCTION_ID=def"}; !slices.Equal(got, want) {
		t.Fatalf("providerEnv = %v, want %v", got, want)
	}
}
