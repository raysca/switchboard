package auth

import (
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewManagement_Validation(t *testing.T) {
	// Empty secret allowed -> disabled
	m, err := NewManagement("")
	if err != nil {
		t.Fatalf("expected nil error for empty secret, got %v", err)
	}
	if m.Enabled() {
		t.Fatal("expected management auth to be disabled for empty secret")
	}

	// Secret shorter than 32 bytes rejected
	shortSecret := "short-secret-less-than-32-bytes"
	if len(shortSecret) >= 32 {
		t.Fatalf("test setup error: shortSecret is %d bytes", len(shortSecret))
	}
	m, err = NewManagement(shortSecret)
	if err == nil {
		t.Fatal("expected error for secret under 32 bytes, got nil")
	}
	if m != nil {
		t.Fatal("expected nil Management on error")
	}

	// Secret with exactly 32 bytes accepted
	secret32 := strings.Repeat("a", 32)
	m, err = NewManagement(secret32)
	if err != nil {
		t.Fatalf("unexpected error for 32-byte secret: %v", err)
	}
	if !m.Enabled() {
		t.Fatal("expected management auth to be enabled for 32-byte secret")
	}

	// Longer secret accepted
	secret64 := strings.Repeat("b", 64)
	m, err = NewManagement(secret64)
	if err != nil {
		t.Fatalf("unexpected error for 64-byte secret: %v", err)
	}
	if !m.Enabled() {
		t.Fatal("expected management auth to be enabled for 64-byte secret")
	}
}

func TestManagement_Authenticate(t *testing.T) {
	secret := "01234567890123456789012345678901" // 32 bytes
	m, err := NewManagement(secret)
	if err != nil {
		t.Fatalf("NewManagement: %v", err)
	}

	tests := []struct {
		name       string
		authHeader string
		want       bool
	}{
		{
			name:       "valid token",
			authHeader: "Bearer " + secret,
			want:       true,
		},
		{
			name:       "missing auth header",
			authHeader: "",
			want:       false,
		},
		{
			name:       "wrong scheme Basic",
			authHeader: "Basic " + secret,
			want:       false,
		},
		{
			name:       "wrong scheme bearer lowercase",
			authHeader: "bearer " + secret,
			want:       false,
		},
		{
			name:       "Bearer prefix without token",
			authHeader: "Bearer ",
			want:       false,
		},
		{
			name:       "Bearer prefix missing space",
			authHeader: "Bearer" + secret,
			want:       false,
		},
		{
			name:       "token with space suffix or prefix",
			authHeader: "Bearer " + secret + " extra",
			want:       false,
		},
		{
			name:       "token with internal tab",
			authHeader: "Bearer " + secret[:10] + "\t" + secret[10:],
			want:       false,
		},
		{
			name:       "invalid token different characters",
			authHeader: "Bearer " + strings.Repeat("x", 32),
			want:       false,
		},
		{
			name:       "token prefix match only",
			authHeader: "Bearer " + secret[:16],
			want:       false,
		},
		{
			name:       "token with extra characters",
			authHeader: "Bearer " + secret + "extra",
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			got := m.Authenticate(req)
			if got != tt.want {
				t.Fatalf("Authenticate(%q) = %v, want %v", tt.authHeader, got, tt.want)
			}
		})
	}
}

func TestManagement_DisabledRejectsAll(t *testing.T) {
	m, err := NewManagement("")
	if err != nil {
		t.Fatalf("NewManagement: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 32))
	if m.Authenticate(req) {
		t.Fatal("expected disabled management to reject authentication")
	}

	var nilM *Management
	if nilM.Enabled() {
		t.Fatal("nil Management must report Enabled() == false")
	}
	if nilM.Authenticate(req) {
		t.Fatal("nil Management must reject Authenticate")
	}
}

func TestManagement_ConstantTimeHashComparison(t *testing.T) {
	secret := "master-secret-key-that-is-at-least-32-bytes-long"
	m, err := NewManagement(secret)
	if err != nil {
		t.Fatalf("NewManagement: %v", err)
	}

	expectedDigest := sha256.Sum256([]byte(secret))
	if m.digest != expectedDigest {
		t.Fatalf("stored digest mismatch: got %x, want %x", m.digest, expectedDigest)
	}
}
