package mcp

import (
	"regexp"
	"strings"
)

// authEnvRe confines MCP bearer tokens to a dedicated prefix so a management
// caller can never point auth_env at an unrelated secret (provider keys, the
// management API key).
var authEnvRe = regexp.MustCompile(`^MCP_TOKEN_[A-Z0-9_]{1,48}$`)

func ValidAuthEnv(name string) bool { return authEnvRe.MatchString(name) }

// TokenSource resolves an auth_env variable name to its bearer token.
type TokenSource interface {
	Token(name string) (string, bool)
}

type envTokens struct{ getenv func(string) string }

func EnvTokens(getenv func(string) string) TokenSource { return envTokens{getenv} }

func (e envTokens) Token(name string) (string, bool) {
	if !ValidAuthEnv(name) {
		return "", false // never read a variable outside the prefix, even if asked
	}
	v := strings.TrimSpace(e.getenv(name))
	if v == "" || strings.IndexFunc(v, func(c rune) bool { return c <= ' ' || c == 0x7f }) >= 0 {
		return "", false
	}
	return v, true
}
