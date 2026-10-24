package server

import (
	"net/http"
	"strings"
	"testing"
)

// A secret written through the API (the encrypted store) authenticates an MCP server like an MCP_TOKEN_ variable
// does, and the value never appears in a management response.
func TestMCPServerAuthenticatesWithAStoredSecret(t *testing.T) {
	r := newMCPCallRig(t)
	if w := apiRequest(r.s, http.MethodPut, "/api/v1/secrets/crm-api", e2eMgmtKey, `{"value":"tok-1"}`); w.Code != 200 {
		t.Fatalf("put secret: %d %s", w.Code, w.Body)
	}
	id, hashes := r.server("stored", r.ts.URL+"/ok", "crm-api")
	if len(hashes) == 0 {
		t.Fatal("discovery with a stored secret found no tools")
	}
	view := apiRequest(r.s, http.MethodGet, "/api/v1/mcp-servers/"+id, e2eMgmtKey, "")
	if !strings.Contains(view.Body.String(), `"auth_env_set":true`) || strings.Contains(view.Body.String(), "tok-1") {
		t.Fatalf("server view: %s", view.Body)
	}
	if list := apiRequest(r.s, http.MethodGet, "/api/v1/secrets", e2eMgmtKey, ""); strings.Contains(list.Body.String(), "tok-1") {
		t.Fatalf("secret list leaks the value: %s", list.Body)
	}
	// a name that resolves nowhere cannot authenticate
	if _, none := r.server("unset", r.ts.URL+"/ok", "no-such-secret"); len(none) != 0 {
		t.Fatal("an unresolved secret still discovered tools")
	}
}
