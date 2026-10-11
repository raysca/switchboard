package server

import (
	"testing"

	"govoice/providers/google"
	"govoice/server/catalog"
)

// Each seed provider's assembled secret must be accepted by the adapter's own parser.

func adapterResolver(t *testing.T, env, files map[string]string) *catalog.Resolver {
	t.Helper()
	cat, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	read := func(path string, _ int64) ([]byte, error) { return []byte(files[path]), nil }
	return catalog.NewResolverWithFS(cat, func(k string) string { return env[k] }, read)
}

func TestAdapterParsesGoogleSecret(t *testing.T) {
	const sa = `{"type":"service_account","project_id":"proj-from-json","private_key":"-----BEGIN PRIVATE KEY-----\nFAKE\n-----END PRIVATE KEY-----\n","client_email":"svc@proj-from-json.iam.gserviceaccount.com"}`
	r := adapterResolver(t, map[string]string{"GOOGLE_API_KEY": "gkey", "GOOGLE_APPLICATION_CREDENTIALS": "/sa.json"}, map[string]string{"/sa.json": "  " + sa + "\n"})

	b, err := r.Secret("google", "llm")
	if err != nil {
		t.Fatal(err)
	}
	if m, err := google.CredentialMaterialFromSecret(b); err != nil || m.APIKey != "gkey" {
		t.Fatalf("api key: %+v, %v", m, err)
	}
	for _, svc := range []string{"stt", "tts"} {
		b, err := r.Secret("google", svc)
		if err != nil {
			t.Fatal(err)
		}
		if m, err := google.CredentialMaterialFromSecret(b); err != nil || m.ProjectID != "proj-from-json" || len(m.ServiceAccountJSON) == 0 {
			t.Fatalf("%s service account: %+v, %v", svc, m, err)
		}
	}
}

func TestAdapterParsesOpenAISecret(t *testing.T) {
	for _, tc := range []struct{ name, base string }{{"key only", ""}, {"key and base url", "http://localhost:8080/v1"}} {
		t.Run(tc.name, func(t *testing.T) {
			r := adapterResolver(t, map[string]string{"OPENAI_API_KEY": "okey", "OPENAI_BASE_URL": tc.base}, nil)
			b, err := r.Secret("openai", "llm")
			if err != nil {
				t.Fatal(err)
			}
			key, base, err := openAICredential(b)
			if err != nil || key != "okey" || base != tc.base {
				t.Fatalf("got %q %q %v", key, base, err)
			}
		})
	}
}

func TestAdapterParsesNvidiaSecret(t *testing.T) {
	r := adapterResolver(t, map[string]string{"NVIDIA_API_KEY": "nkey", "NVIDIA_STT_FUNCTION_ID": "f1", "NVIDIA_TTS_FUNCTION_ID": "f2"}, nil)
	for _, svc := range []string{"llm", "stt", "tts"} {
		b, err := r.Secret("nvidia", svc)
		if err != nil {
			t.Fatal(err)
		}
		if svc == "llm" {
			if key, err := apiKey(b); err != nil || key != "nkey" {
				t.Fatalf("llm: %q, %v", key, err)
			}
			continue
		}
		c, err := nvidiaSTTCredential(b)
		if err != nil || c.APIKey != "nkey" {
			t.Fatalf("%s: %+v, %v", svc, c, err)
		}
		if c.FunctionID != "f1" || c.TTSFuncID != "f2" {
			t.Fatalf("%s function ids: %+v", svc, c)
		}
	}
}
