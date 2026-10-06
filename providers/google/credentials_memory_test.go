package google

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strings"
	"testing"
)

// Fake key material: never valid, never used against the network.
const fakeSAJSON = `{"type":"service_account","project_id":"proj-from-json","private_key":"-----BEGIN PRIVATE KEY-----\nFAKE\n-----END PRIVATE KEY-----\n","client_email":"svc@proj-from-json.iam.gserviceaccount.com"}`

func TestCredentialMaterialFromSecret(t *testing.T) {
	m, err := CredentialMaterialFromSecret([]byte("  AIzaFakeKey123\n"))
	if err != nil || m.APIKey != "AIzaFakeKey123" || m.ServiceAccountJSON != nil {
		t.Fatalf("api key: %+v %v", m, err)
	}

	m, err = CredentialMaterialFromSecret([]byte(fakeSAJSON))
	if err != nil || m.APIKey != "" || string(m.ServiceAccountJSON) != fakeSAJSON || m.ProjectID != "proj-from-json" {
		t.Fatalf("service account: %+v %v", m, err)
	}

	for name, secret := range map[string]string{
		"empty":         "",
		"blank":         " \n\t",
		"multi-token":   "secret-part-one secret-part-two",
		"multi-line":    "secret-line-one\nsecret-line-two",
		"bad json":      `{"type":"service_account", secret-garbage`,
		"wrong type":    `{"type":"authorized_user","private_key":"secret-pk","client_email":"a@b"}`,
		"missing key":   `{"type":"service_account","client_email":"a@b"}`,
		"missing email": `{"type":"service_account","private_key":"secret-pk"}`,
	} {
		_, err := CredentialMaterialFromSecret([]byte(secret))
		if err == nil {
			t.Fatalf("%s: expected error", name)
		}
		if strings.Contains(err.Error(), "secret-") {
			t.Fatalf("%s: error leaks secret contents: %v", name, err)
		}
	}
}

func TestClientAuthOptions_Precedence(t *testing.T) {
	typ := func(json []byte, file, key string) string {
		opts := clientAuthOptions(json, file, key)
		if len(opts) != 1 {
			return fmt.Sprint(len(opts))
		}
		return fmt.Sprintf("%T", opts[0])
	}
	jsonT := typ([]byte(fakeSAJSON), "/f.json", "k")
	fileT := typ(nil, "/f.json", "k")
	keyT := typ(nil, "", "k")
	if jsonT == fileT || jsonT == keyT || fileT == keyT {
		t.Fatalf("expected distinct option types, got json=%s file=%s key=%s", jsonT, fileT, keyT)
	}
	if got := typ(nil, "", ""); got != "0" {
		t.Fatalf("expected no auth options for ADC, got %s", got)
	}
}

func TestSTTProjectID_FromCredentialsJSON(t *testing.T) {
	if got := sttProjectID(STTConfig{CredentialsJSON: []byte(fakeSAJSON)}); got != "proj-from-json" {
		t.Fatalf("got %q", got)
	}
	if got := sttProjectID(STTConfig{ProjectID: "explicit", CredentialsJSON: []byte(fakeSAJSON)}); got != "explicit" {
		t.Fatalf("got %q", got)
	}
	if got := sttProjectID(STTConfig{}); got != "_" {
		t.Fatalf("got %q", got)
	}
}

func TestSTTTTSClients_AcceptCredentialsJSON(t *testing.T) {
	// Client construction is lazy (no dial/token fetch), so this stays offline.
	ctx := context.Background()
	saJSON := generatedSAJSON(t)
	stt, err := NewRealGoogleSTTOpener(ctx, STTConfig{CredentialsJSON: saJSON})
	if err != nil {
		t.Fatalf("stt: %v", err)
	}
	_ = stt.Close()
	tts, err := NewRealGoogleTTSClient(ctx, TTSConfig{CredentialsJSON: saJSON})
	if err != nil {
		t.Fatalf("tts: %v", err)
	}
	_ = tts.Close()
}

func TestResolveCredentials_InMemoryServiceAccount(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	creds, err := ResolveCredentials(CredentialConfig{
		Mode:              AuthModeVertexADC,
		CredentialsJSON:   []byte(fakeSAJSON),
		ProductionProfile: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds.ProjectID != "proj-from-json" || creds.Location != "us-central1" || creds.CredentialsFile != "" || string(creds.CredentialsJSON) != fakeSAJSON {
		t.Fatalf("unexpected creds: %+v", creds)
	}

	if _, err := ResolveCredentials(CredentialConfig{Mode: AuthModeVertexADC, CredentialsJSON: []byte(`{"type":"external_account"}`)}); err == nil {
		t.Fatal("expected non-service-account JSON to be rejected")
	}

	client, err := NewRealGoogleLLMClient(context.Background(), creds)
	if err != nil {
		t.Fatalf("llm client: %v", err)
	}
	if client.client.ClientConfig().Credentials == nil || client.client.ClientConfig().Project != "proj-from-json" {
		t.Fatal("expected in-memory credentials on the genai client")
	}
}

// generatedSAJSON returns service account JSON with a throwaway, parseable RSA key.
func generatedSAJSON(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(map[string]string{
		"type":         "service_account",
		"project_id":   "proj-from-json",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email": "svc@proj-from-json.iam.gserviceaccount.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCredentialMaterialRejectsEnvReference(t *testing.T) {
	if _, err := CredentialMaterialFromSecret([]byte("$TOKEN_SECRET")); err == nil {
		t.Fatal("a stored secret naming an env var must be rejected")
	}
}
