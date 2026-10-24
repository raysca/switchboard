package mcp

import (
	"context"
	"strings"
	"time"

	"govoice/server/secrets"
)

// ValidAuthEnv reports whether name is a legal secret name for an MCP server: an MCP_TOKEN_* variable, or the
// lower-case name of a stored or directory secret. HTTP_TOKEN_* variables belong to HTTP tools and are never sent to
// an MCP endpoint. Which names resolve is the secret chain's business.
func ValidAuthEnv(name string) bool {
	return secrets.ValidName(name) && !strings.HasPrefix(name, "HTTP_TOKEN_")
}

// TokenSource resolves an auth_env secret name to its bearer token.
type TokenSource interface {
	Token(name string) (string, bool)
	// Has reports whether the name resolves without counting as a use of the secret (readiness and server views).
	Has(name string) bool
}

type resolverTokens struct{ r secrets.Resolver }

// ResolverTokens serves tokens from a secrets chain. TokenSource has no context, so each lookup gets a short deadline
// of its own: a stored secret is one indexed read.
func ResolverTokens(r secrets.Resolver) TokenSource { return resolverTokens{r} }

func (t resolverTokens) Token(name string) (string, bool) {
	return t.resolve(context.Background(), name)
}

func (t resolverTokens) Has(name string) bool {
	_, ok := t.resolve(secrets.WithoutUseStamp(context.Background()), name)
	return ok
}

func (t resolverTokens) resolve(ctx context.Context, name string) (string, bool) {
	if !ValidAuthEnv(name) {
		return "", false // never read a name outside the allowed forms, even if asked
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return t.r.Resolve(ctx, name)
}
