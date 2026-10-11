package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"govoice/core"
)

func mapEnv(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

type provView struct {
	ID       string   `json:"id"`
	Detected bool     `json:"detected"`
	Env      []string `json:"env"`
	Services map[string]struct {
		Available  bool     `json:"available"`
		Reason     string   `json:"reason"`
		MissingEnv []string `json:"missing_env"`
		Voices     []struct {
			ID      string `json:"id"`
			Default bool   `json:"default"`
		} `json:"voices"`
	} `json:"services"`
}

func listProviders(t *testing.T, s *Server) ([]provView, []byte) {
	t.Helper()
	w := apiRequest(s, http.MethodGet, "/api/v1/providers", "management-test-key-32-bytes-long-and-separate", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	var out struct {
		Data []provView `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Data, w.Body.Bytes()
}

func provByID(t *testing.T, ps []provView, id string) provView {
	t.Helper()
	for _, p := range ps {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("provider %s not listed", id)
	return provView{}
}

// installProviders wires the listing the way EnableManagement does.
func installProviders(s *Server) {
	stub := func(*Config, StageSpec) (core.Processor, error) { return nil, nil }
	s.registry.Register("xai.stt", stub)
	s.registry.Register("xai.tts", stub)
	s.registry.Register("xai.llm", stub)
	s.SetProviderManagementAPI(s.providersRoutes())
}

func TestProvidersNoKeys(t *testing.T) {
	s := apiTestServer(t)
	s.env = mapEnv(nil)
	installProviders(s)
	ps, _ := listProviders(t, s)
	if len(ps) == 0 {
		t.Fatal("no providers")
	}
	for _, p := range ps {
		if p.Detected || len(p.Env) == 0 {
			t.Fatalf("%s detected=%v env=%v", p.ID, p.Detected, p.Env)
		}
		for name, sv := range p.Services {
			if sv.Available || (sv.Reason != "missing_env" && sv.Reason != "not_in_build") {
				t.Fatalf("%s.%s = %+v", p.ID, name, sv)
			}
			if sv.Reason == "missing_env" && len(sv.MissingEnv) == 0 {
				t.Fatalf("%s.%s missing_env empty", p.ID, name)
			}
		}
	}
}

func TestProvidersXAI(t *testing.T) {
	s := apiTestServer(t)
	s.env = mapEnv(map[string]string{"XAI_API_KEY": "sekrit-xai-value"})
	installProviders(s)
	ps, raw := listProviders(t, s)
	x := provByID(t, ps, "xai")
	if !x.Detected || !x.Services["llm"].Available || !x.Services["stt"].Available || !x.Services["tts"].Available {
		t.Fatalf("xai = %+v", x)
	}
	var def string
	for _, v := range x.Services["tts"].Voices {
		if v.Default {
			def = v.ID
		}
	}
	if def != "eve" {
		t.Fatalf("default voice = %q", def)
	}
	if bytes.Contains(raw, []byte("sekrit-xai-value")) {
		t.Fatal("response leaks the environment value")
	}
	if g := provByID(t, ps, "google"); g.Detected {
		t.Fatal("undetected provider must still be listed with detected:false")
	}
}

func TestProvidersNotInBuild(t *testing.T) {
	s := apiTestServer(t)
	s.env = mapEnv(map[string]string{"XAI_API_KEY": "k"})
	installProviders(s)
	delete(s.registry.factories, factoryKey("xai", "stt"))
	x := provByID(t, func() []provView { p, _ := listProviders(t, s); return p }(), "xai")
	if sv := x.Services["stt"]; sv.Available || sv.Reason != "not_in_build" {
		t.Fatalf("stt = %+v", sv)
	}
	if !x.Services["llm"].Available {
		t.Fatal("llm must stay available")
	}
}

func TestAvailabilityCheck(t *testing.T) {
	s := apiTestServer(t)
	installProviders(s)
	s.env = mapEnv(nil)
	if u := s.availability.Check("xai", "tts"); u.Reason != "missing_credential" || len(u.MissingEnv) == 0 {
		t.Fatalf("got %+v", u)
	}
	s.env = mapEnv(map[string]string{"XAI_API_KEY": "k"})
	if u := s.availability.Check("xai", "tts"); u.Reason != "" {
		t.Fatalf("got %+v", u)
	}
	if u := s.availability.Check("nope", "llm"); u.Reason != "unknown_provider" {
		t.Fatalf("got %+v", u)
	}
	if u := s.availability.Check("deepgram", "llm"); u.Reason != "unsupported_provider" {
		t.Fatalf("got %+v", u)
	}
}
