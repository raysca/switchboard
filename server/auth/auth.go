// Package auth enforces the management credential boundary.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

type Management struct {
	digest  [sha256.Size]byte
	enabled bool
}

func NewManagement(secret string) (*Management, error) {
	m := &Management{}
	if secret == "" {
		return m, nil
	}
	if len(secret) < 32 {
		return nil, errors.New("management API key must be at least 32 bytes")
	}
	m.digest = sha256.Sum256([]byte(secret))
	m.enabled = true
	return m, nil
}

func (m *Management) Enabled() bool { return m != nil && m.enabled }

func (m *Management) Authenticate(r *http.Request) bool {
	if !m.Enabled() {
		return false
	}
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	candidate := strings.TrimPrefix(h, "Bearer ")
	if candidate == "" || strings.ContainsAny(candidate, " \t\r\n") {
		return false
	}
	digest := sha256.Sum256([]byte(candidate))
	return subtle.ConstantTimeCompare(m.digest[:], digest[:]) == 1
}
