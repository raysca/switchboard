package google

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCredentials_VertexADC_MissingFile(t *testing.T) {
	cfg := CredentialConfig{
		Mode:            AuthModeVertexADC,
		CredentialsFile: "/non/existent/path/creds.json",
	}
	_, err := ResolveCredentials(cfg)
	if err == nil {
		t.Fatal("expected error for non-existent credential file, got nil")
	}
	if !os.IsNotExist(err) && err.Error() == "" {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestCredentials_VertexADC_FilePermissions(t *testing.T) {
	tempDir := t.TempDir()
	validJSON := `{
		"type": "service_account",
		"project_id": "test-proj",
		"client_email": "test@test-proj.iam.gserviceaccount.com"
	}`

	insecureFile := filepath.Join(tempDir, "insecure_key.json")
	if err := os.WriteFile(insecureFile, []byte(validJSON), 0644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(insecureFile, 0644)

	secureFile := filepath.Join(tempDir, "secure_key.json")
	if err := os.WriteFile(secureFile, []byte(validJSON), 0600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(secureFile, 0600)

	// 1. In production profile, 0644 must be rejected
	cfgProdInsecure := CredentialConfig{
		Mode:              AuthModeVertexADC,
		CredentialsFile:   insecureFile,
		ProductionProfile: true,
	}
	_, err := ResolveCredentials(cfgProdInsecure)
	if err == nil {
		t.Fatal("expected production rejection for 0644 credential file, got nil")
	}

	// 2. In production profile with insecure override, 0644 is accepted
	cfgOverride := CredentialConfig{
		Mode:                   AuthModeVertexADC,
		CredentialsFile:        insecureFile,
		ProductionProfile:      true,
		AllowInsecureFilePerms: true,
	}
	credsOverride, err := ResolveCredentials(cfgOverride)
	if err != nil {
		t.Fatalf("expected override to allow 0644, got: %v", err)
	}
	if credsOverride.ProjectID != "test-proj" {
		t.Fatalf("unexpected project ID: %s", credsOverride.ProjectID)
	}

	// 3. In production profile, 0600 must be accepted
	cfgProdSecure := CredentialConfig{
		Mode:              AuthModeVertexADC,
		CredentialsFile:   secureFile,
		ProductionProfile: true,
	}
	credsSecure, err := ResolveCredentials(cfgProdSecure)
	if err != nil {
		t.Fatalf("expected 0600 to succeed in production, got: %v", err)
	}
	if credsSecure.ProjectID != "test-proj" {
		t.Fatalf("unexpected project ID: %s", credsSecure.ProjectID)
	}
}

func TestCredentials_VertexADC_StructuralValidation(t *testing.T) {
	tempDir := t.TempDir()

	// Malformed JSON
	badJSONFile := filepath.Join(tempDir, "bad.json")
	_ = os.WriteFile(badJSONFile, []byte("{not json"), 0600)
	_, err := ResolveCredentials(CredentialConfig{
		Mode:            AuthModeVertexADC,
		CredentialsFile: badJSONFile,
	})
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}

	// Missing type field
	noTypeFile := filepath.Join(tempDir, "no_type.json")
	_ = os.WriteFile(noTypeFile, []byte(`{"project_id": "p"}`), 0600)
	_, err = ResolveCredentials(CredentialConfig{
		Mode:            AuthModeVertexADC,
		CredentialsFile: noTypeFile,
	})
	if err == nil {
		t.Fatal("expected error for missing 'type' field, got nil")
	}

	// Unsupported type
	wrongTypeFile := filepath.Join(tempDir, "wrong_type.json")
	_ = os.WriteFile(wrongTypeFile, []byte(`{"type": "api_key", "project_id": "p"}`), 0600)
	_, err = ResolveCredentials(CredentialConfig{
		Mode:            AuthModeVertexADC,
		CredentialsFile: wrongTypeFile,
	})
	if err == nil {
		t.Fatal("expected error for unsupported key type, got nil")
	}
}

func TestCredentials_GeminiAPIKey(t *testing.T) {
	// Direct key
	cfg := CredentialConfig{
		Mode:   AuthModeGeminiAPIKey,
		APIKey: "ai-secret-key-123",
	}
	creds, err := ResolveCredentials(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds.APIKey != "ai-secret-key-123" {
		t.Fatalf("unexpected API key: %s", creds.APIKey)
	}

	// Env var key
	t.Setenv("GOOGLE_API_KEY", "env-secret-456")
	cfgEnv := CredentialConfig{
		Mode: AuthModeGeminiAPIKey,
	}
	credsEnv, err := ResolveCredentials(cfgEnv)
	if err != nil {
		t.Fatalf("unexpected error resolving from env: %v", err)
	}
	if credsEnv.APIKey != "env-secret-456" {
		t.Fatalf("unexpected API key from env: %s", credsEnv.APIKey)
	}

	// Missing key
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	_, err = ResolveCredentials(CredentialConfig{Mode: AuthModeGeminiAPIKey})
	if err == nil {
		t.Fatal("expected error when no API key is provided, got nil")
	}
}
