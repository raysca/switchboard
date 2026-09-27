package ws

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strings"
)

var (
	ErrDisallowedOrigin = errors.New("ws: origin not permitted by allowlist")
	ErrMissingOrigin    = errors.New("ws: request missing Origin header")
	ErrInsecureScheme   = errors.New("ws: connection must use wss/tls in production")
)

// SecurityConfig controls TLS termination, proxy trusts, and origin allowlists.
type SecurityConfig struct {
	AllowedOrigins     []string // Allowed browser origins (e.g. "https://example.com", or "*" for development)
	AllowMissingOrigin bool     // Allow clients with no Origin header (e.g. non-browser SDKs)
	TrustedProxies     []string // CIDR blocks or IP addresses of reverse proxies
	RequireTLS         bool     // If true, reject plain HTTP/WS upgrade
	TLSConfig          *tls.Config
}

// CheckOrigin verifies the request's Origin header against the configured allowlist.
func (cfg *SecurityConfig) CheckOrigin(r *http.Request) error {
	origin := r.Header.Get("Origin")
	if origin == "" {
		if cfg.AllowMissingOrigin {
			return nil
		}
		return ErrMissingOrigin
	}

	for _, allowed := range cfg.AllowedOrigins {
		if allowed == "*" || allowed == origin {
			return nil
		}
	}
	return ErrDisallowedOrigin
}

// ClientIP extracts the real client IP, trusting forwarded headers ONLY if RemoteAddr is a trusted proxy.
func (cfg *SecurityConfig) ClientIP(r *http.Request) string {
	remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteHost = r.RemoteAddr
	}

	if !cfg.isTrustedProxy(remoteHost) {
		return remoteHost
	}

	// Remote host is trusted proxy: check X-Forwarded-For
	xff := r.Header.Get("X-Forwarded-For")
	if xff != "" {
		parts := strings.Split(xff, ",")
		clientIP := strings.TrimSpace(parts[0])
		if clientIP != "" {
			return clientIP
		}
	}

	return remoteHost
}

// IsSecure returns true if the connection is directly TLS or forwarded by a trusted proxy over https.
func (cfg *SecurityConfig) IsSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}

	remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteHost = r.RemoteAddr
	}

	if cfg.isTrustedProxy(remoteHost) {
		proto := r.Header.Get("X-Forwarded-Proto")
		return strings.EqualFold(proto, "https")
	}

	return false
}

func (cfg *SecurityConfig) isTrustedProxy(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}

	for _, entry := range cfg.TrustedProxies {
		if strings.Contains(entry, "/") {
			_, cidr, err := net.ParseCIDR(entry)
			if err == nil && cidr.Contains(ip) {
				return true
			}
		} else {
			if entry == ipStr {
				return true
			}
		}
	}
	return false
}
