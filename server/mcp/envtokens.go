package mcp

import (
	"context"
	"regexp"
	"strings"
	"time"

	"govoice/server/secrets"
)

// authEnvRe confines MCP bearer tokens to a dedicated prefix so a management
// caller can never point auth_env at an unrelated secret (provider keys, the
// management API key).
var authEnvRe = regexp.MustCompile(`^MCP_TOKEN_[A-Z0-9_]{1,48}$`) // the environment backend of EnvTokens

// ValidAuthEnv reports whether name is a legal secret name: an MCP_TOKEN_* or HTTP_TOKEN_* variable, or the lower-case
// name of a stored or directory secret. Which of them resolves is the secret chain's business.
func ValidAuthEnv(name string) bool { return secrets.ValidName(name) }

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

type resolverTokens struct{ r secrets.Resolver }

// ResolverTokens serves tokens from a secrets chain. TokenSource has no context, so each lookup gets a short deadline
// of its own: a stored secret is one indexed read.
func ResolverTokens(r secrets.Resolver) TokenSource { return resolverTokens{r} }

func (t resolverTokens) Token(name string) (string, bool) {
	if !ValidAuthEnv(name) {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return t.r.Resolve(ctx, name)
}
