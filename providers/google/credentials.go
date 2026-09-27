package google

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// AuthMode represents the Google authentication mechanism.
type AuthMode string

const (
	AuthModeVertexADC    AuthMode = "vertex_adc"
	AuthModeGeminiAPIKey AuthMode = "gemini_api_key"
)

// CredentialConfig holds options for credential resolution and validation.
type CredentialConfig struct {
	Mode                   AuthMode // "vertex_adc" or "gemini_api_key"
	CredentialsFile        string   // Path to service account JSON (falls back to GOOGLE_APPLICATION_CREDENTIALS)
	APIKey                 string   // Direct API key or env var name (falls back to GOOGLE_API_KEY)
	ProductionProfile      bool     // If true, enforces strict 0600/0400 file permissions
	AllowInsecureFilePerms bool     // Override to permit group/world readable files in production
	ProjectID              string   // Optional explicit Google Cloud project ID
	Location               string   // Optional Google Cloud location (e.g. "us-central1")
}

// ResolvedCredentials contains validated configuration ready for client instantiation.
type ResolvedCredentials struct {
	Mode            AuthMode
	CredentialsFile string
	APIKey          string
	ProjectID       string
	Location        string
}

// ServiceAccountKey matches the basic structure of a Google service account JSON key.
type ServiceAccountKey struct {
	Type                    string `json:"type"`
	ProjectID               string `json:"project_id"`
	PrivateKeyID            string `json:"private_key_id"`
	PrivateKey              string `json:"private_key"`
	ClientEmail             string `json:"client_email"`
	ClientID                string `json:"client_id"`
	AuthURI                 string `json:"auth_uri"`
	TokenURI                string `json:"token_uri"`
	AuthProviderX509CertURL string `json:"auth_provider_x509_cert_url"`
	ClientX509CertURL       string `json:"client_x509_cert_url"`
}

// ResolveCredentials validates and returns the resolved credentials according to security policy.
func ResolveCredentials(cfg CredentialConfig) (*ResolvedCredentials, error) {
	switch cfg.Mode {
	case AuthModeVertexADC:
		filePath := cfg.CredentialsFile
		if filePath == "" {
			filePath = os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
		}
		if filePath == "" {
			return nil, fmt.Errorf("vertex_adc mode requires credentials file: GOOGLE_APPLICATION_CREDENTIALS is not set")
		}

		info, err := os.Stat(filePath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("credential file does not exist: %s", filePath)
			}
			return nil, fmt.Errorf("failed to inspect credential file %s: %w", filePath, err)
		}

		// Security check: in production profile, refuse group- or world-readable files
		perm := info.Mode().Perm()
		if cfg.ProductionProfile && !cfg.AllowInsecureFilePerms {
			if perm&0077 != 0 {
				return nil, fmt.Errorf("credential file %s has insecure permissions %04o: group- or world-readable files are prohibited in production profile", filePath, perm)
			}
		}

		// Structural validation: must parse as valid JSON service account or authorized user
		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("failed to read credential file %s: %w", filePath, err)
		}

		var rawMap map[string]any
		if err := json.Unmarshal(data, &rawMap); err != nil {
			return nil, fmt.Errorf("credential file %s contains invalid JSON: %w", filePath, err)
		}

		keyType, _ := rawMap["type"].(string)
		if keyType == "" {
			return nil, fmt.Errorf("credential file %s is structurally invalid: missing 'type' field", filePath)
		}
		if keyType != "service_account" && keyType != "authorized_user" && keyType != "external_account" {
			return nil, fmt.Errorf("credential file %s is unsupported type: %s", filePath, keyType)
		}

		projectID := cfg.ProjectID
		if projectID == "" {
			if pid, ok := rawMap["project_id"].(string); ok {
				projectID = pid
			}
		}

		location := cfg.Location
		if location == "" {
			location = "us-central1"
		}

		return &ResolvedCredentials{
			Mode:            AuthModeVertexADC,
			CredentialsFile: filePath,
			ProjectID:       projectID,
			Location:        location,
		}, nil

	case AuthModeGeminiAPIKey:
		key := cfg.APIKey
		if key == "" || strings.HasPrefix(key, "$") {
			envVar := "GOOGLE_API_KEY"
			if strings.HasPrefix(key, "$") {
				envVar = strings.TrimPrefix(key, "$")
			}
			key = os.Getenv(envVar)
		}
		if key == "" {
			// Also check GEMINI_API_KEY as fallback
			key = os.Getenv("GEMINI_API_KEY")
		}
		if key == "" {
			return nil, fmt.Errorf("gemini_api_key mode requires API key: neither configured key nor GOOGLE_API_KEY/GEMINI_API_KEY is set")
		}

		return &ResolvedCredentials{
			Mode:   AuthModeGeminiAPIKey,
			APIKey: key,
		}, nil

	default:
		return nil, fmt.Errorf("unsupported google auth mode: %q (must be %q or %q)", cfg.Mode, AuthModeVertexADC, AuthModeGeminiAPIKey)
	}
}
