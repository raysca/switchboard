package catalog

import (
	"reflect"
	"strings"
	"testing"
)

const goodXAI = `
id: xai
name: xAI
credentials:
  - id: key
    label: xAI API key
    fields:
      api_key: {env: XAI_API_KEY}
    assemble: token
services:
  llm:
    credential: [key]
    models:
      - {id: grok-4.20-0309-non-reasoning, label: Grok 4.20, default: true}
  tts:
    credential: [key]
    voices:
      - {id: eve, label: Eve, default: true}
      - {id: ara, label: Ara}
`

func TestPackagedCatalogLoads(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"google", "echo", "xai", "elevenlabs", "deepgram", "cartesia", "openai", "nvidia"} {
		if _, ok := c.Provider(id); !ok {
			t.Errorf("packaged provider %q missing", id)
		}
	}
	g, _ := c.Provider("google")
	if got := g.Services[ServiceLLM].Credential; len(got) != 2 || got[0] != "api_key" {
		t.Fatalf("google llm must prefer the api key, got %v", got)
	}
	if got := g.Services[ServiceSTT].Credential; len(got) != 1 || got[0] != "service_account" {
		t.Fatalf("google stt needs the service account, got %v", got)
	}
}

func TestParseGood(t *testing.T) {
	p, err := Parse("xai.yaml", []byte(goodXAI))
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := DefaultEntry(p.Services[ServiceTTS].Voices); !ok || d.ID != "eve" {
		t.Fatalf("default voice = %+v", d)
	}
	if !p.Services[ServiceTTS].AllowsCustom() {
		t.Fatal("custom entries are allowed unless the catalog says otherwise")
	}
}

func TestParseRejects(t *testing.T) {
	mutate := func(from, to string) string { return strings.Replace(goodXAI, from, to, 1) }
	cases := map[string]string{
		"unknown key":           mutate("name: xAI", "name: xAI\nbogus: 1"),
		"bad provider id":       mutate("id: xai", "id: XAI!"),
		"empty name":            mutate("name: xAI", "name: ''"),
		"unknown assembler":     mutate("assemble: token", "assemble: magic"),
		"bad env name":          mutate("XAI_API_KEY", "xai_api_key"),
		"unknown kind in svc":   mutate("credential: [key]\n    models", "credential: [nope]\n    models"),
		"unknown service":       mutate("  tts:", "  video:"),
		"two defaults":          mutate("{id: ara, label: Ara}", "{id: ara, label: Ara, default: true}"),
		"duplicate entry id":    mutate("{id: ara, label: Ara}", "{id: eve, label: Eve again}"),
		"empty entry id":        mutate("{id: ara, label: Ara}", "{id: '', label: Ara}"),
		"token with two fields": mutate("api_key: {env: XAI_API_KEY}", "api_key: {env: XAI_API_KEY}\n      other: {env: XAI_OTHER}"),
		"needs missing field":   mutate("credential: [key]\n    models", "credential: [key]\n    needs: [nope]\n    models"),
		"service without kind":  mutate("credential: [key]\n    models", "credential: []\n    models"),
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse("x.yaml", []byte(doc)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestParseCapsEntries(t *testing.T) {
	var b strings.Builder
	b.WriteString(strings.Replace(goodXAI, "      - {id: ara, label: Ara}\n", "", 1))
	for i := 0; i < maxEntries; i++ {
		b.WriteString("      - {id: v" + strings.Repeat("x", 1) + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676)) + ", label: V}\n")
	}
	if _, err := Parse("x.yaml", []byte(b.String())); err == nil {
		t.Fatalf("more than %d entries accepted", maxEntries)
	}
}

func TestEchoMirrorsGoogleCatalog(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	g, ok := c.Provider("google")
	e, ok2 := c.Provider("echo")
	if !ok || !ok2 {
		t.Fatalf("google=%v echo=%v", ok, ok2)
	}
	for _, name := range []string{ServiceSTT, ServiceLLM, ServiceTTS} {
		gs, es := g.Services[name], e.Services[name]
		if es == nil {
			t.Fatalf("echo has no %s service", name)
		}
		if !es.Keyless {
			t.Errorf("echo %s must be keyless", name)
		}
		if es.AllowsCustom() { // its lists are the whole catalog: the console offers no "Other…"
			t.Errorf("echo %s must not allow a custom model, voice or language", name)
		}
		if !reflect.DeepEqual(gs.Models, es.Models) || !reflect.DeepEqual(gs.Voices, es.Voices) || !reflect.DeepEqual(gs.Languages, es.Languages) {
			t.Errorf("echo %s differs from google: update server/catalog/data/echo.yaml", name)
		}
	}
	if e.Name != "Echo (simulated)" || len(e.Credentials) != 0 {
		t.Fatalf("echo name %q credentials %v", e.Name, e.Credentials)
	}
}

func TestKeylessServiceNeedsNoCredential(t *testing.T) {
	keyless := "id: sim\nname: Sim\nservices:\n  tts:\n    keyless: true\n    voices:\n      - {id: v, label: V}\n"
	if _, err := Parse("sim.yaml", []byte(keyless)); err != nil {
		t.Fatalf("keyless service rejected: %v", err)
	}
	neither := strings.Replace(keyless, "    keyless: true\n", "", 1)
	if _, err := Parse("sim.yaml", []byte(neither)); err == nil {
		t.Fatal("a service with neither credential nor keyless must be rejected")
	}
	both := strings.Replace(goodXAI, "credential: [key]\n    models", "credential: [key]\n    keyless: true\n    models", 1)
	if _, err := Parse("x.yaml", []byte(both)); err == nil || !strings.Contains(err.Error(), "keyless") {
		t.Fatalf("keyless plus credentials must be rejected, got %v", err)
	}
}
