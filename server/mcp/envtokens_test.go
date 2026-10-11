package mcp

import (
	"strings"
	"testing"
)

func TestValidAuthEnv(t *testing.T) {
	good := []string{"MCP_TOKEN_CRM", "MCP_TOKEN_A", "MCP_TOKEN_" + strings.Repeat("X", 48)}
	bad := []string{"", "TOKEN_SECRET", "GOVOICE_MANAGEMENT_API_KEY", "XAI_API_KEY", "mcp_token_crm",
		"MCP_TOKEN_", "MCP_TOKEN_A B", "MCP_TOKEN_a", "MCP_TOKEN_" + strings.Repeat("X", 49), "MCP_TOKEN_A\n", "XMCP_TOKEN_A"}
	for _, n := range good {
		if !ValidAuthEnv(n) {
			t.Errorf("%q rejected", n)
		}
	}
	for _, n := range bad {
		if ValidAuthEnv(n) {
			t.Errorf("%q accepted", n)
		}
	}
}

func TestEnvTokensOnlyServesPrefixedSingleTokens(t *testing.T) {
	env := map[string]string{"MCP_TOKEN_CRM": " tok ", "TOKEN_SECRET": "must-not-leak", "MCP_TOKEN_BAD": "a b"}
	src := EnvTokens(func(k string) string { return env[k] })
	if v, ok := src.Token("MCP_TOKEN_CRM"); !ok || v != "tok" {
		t.Fatalf("got %q %v", v, ok)
	}
	for _, n := range []string{"TOKEN_SECRET", "MCP_TOKEN_BAD", "MCP_TOKEN_MISSING"} {
		if _, ok := src.Token(n); ok {
			t.Errorf("%s served", n)
		}
	}
}
