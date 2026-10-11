package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"govoice/server/catalog"
)

// TestProvidersRawJSONShape pins the wire shape the UI parses: it decodes into
// map[string]any so Go struct tags cannot hide a casing or null mismatch.
func TestProvidersRawJSONShape(t *testing.T) {
	cat, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	// Only Google is configured; every other provider is undetected.
	res := catalog.NewResolver(cat, func(k string) string {
		if k == "GOOGLE_API_KEY" || k == "GEMINI_API_KEY" {
			return "x"
		}
		return ""
	})
	h := ProvidersHandler{Catalog: cat, Resolver: res, Supports: func(string, string) bool { return true }}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/providers", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var body struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data) == 0 {
		t.Fatal("no providers")
	}
	sawEntry, sawDefault := false, false
	for _, p := range body.Data {
		if _, ok := p["env"].([]any); !ok {
			t.Errorf("provider %v: env is %T, want array", p["id"], p["env"])
		}
		if _, ok := p["capabilities"].([]any); !ok {
			t.Errorf("provider %v: capabilities is %T, want array", p["id"], p["capabilities"])
		}
		svcs, ok := p["services"].(map[string]any)
		if !ok {
			t.Fatalf("provider %v: services is %T", p["id"], p["services"])
		}
		for name, raw := range svcs {
			if name != "stt" && name != "llm" && name != "tts" {
				t.Errorf("unexpected service key %q", name)
			}
			svc := raw.(map[string]any)
			if _, ok := svc["custom"].(bool); !ok {
				t.Errorf("%v/%s: custom is %T, want bool", p["id"], name, svc["custom"])
			}
			for _, list := range []string{"models", "voices", "languages"} {
				entries, _ := svc[list].([]any)
				for _, e := range entries {
					m := e.(map[string]any)
					if _, ok := m["id"].(string); !ok {
						t.Errorf("%v/%s/%s: entry %v lacks lowercase id", p["id"], name, list, m)
					}
					if _, ok := m["label"].(string); !ok {
						t.Errorf("%v/%s/%s: entry %v lacks lowercase label", p["id"], name, list, m)
					}
					sawEntry = true
					if d, ok := m["default"]; ok {
						if d != true {
							t.Errorf("default must be true when present, got %v", d)
						}
						sawDefault = true
					}
				}
			}
		}
	}
	if !sawEntry || !sawDefault {
		t.Fatalf("catalog produced no entries (%v) or defaults (%v)", sawEntry, sawDefault)
	}
}
