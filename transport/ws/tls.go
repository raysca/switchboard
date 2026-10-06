package ws

import (
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

// ClientIP extracts the real client IP. Forwarded headers are trusted ONLY if RemoteAddr is a trusted proxy.
// When behind a trusted proxy, X-Forwarded-For is traversed from right to left, selecting the first untrusted IP
// to prevent client-injected header spoofing. If X-Real-IP is present from the trusted proxy, it is used as fallback.
func (cfg *SecurityConfig) ClientIP(r *http.Request) string {
	remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteHost = r.RemoteAddr
	}

	if !cfg.isTrustedProxy(remoteHost) {
		if net.ParseIP(remoteHost) == nil {
			return ""
		}
		return remoteHost
	}

	// Remote host is a trusted proxy: parse X-Forwarded-For right-to-left.
	// Unparseable entries are skipped so they cannot become rate-limiter keys.
	xff := r.Header.Get("X-Forwarded-For")
	if xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			ip := strings.TrimSpace(parts[i])
			if net.ParseIP(ip) == nil {
				continue
			}
			if !cfg.isTrustedProxy(ip) {
				return ip
			}
		}
	}

	if xrip := strings.TrimSpace(r.Header.Get("X-Real-IP")); net.ParseIP(xrip) != nil {
		return xrip
	}

	if net.ParseIP(remoteHost) == nil {
		return ""
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
