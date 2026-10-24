package secrets_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"govoice/server/secrets"
	"govoice/server/storage"
)

const sentinel = "sentinel-secret-value-9f3"

func do(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func handler(t *testing.T, spec, dir string) (http.Handler, secrets.Store) {
	t.Helper()
	st := newStore(t, spec)
	env := map[string]string{"MCP_TOKEN_CRM": "env-value", "OPENAI_API_KEY": "must-not-list"}
	return secrets.Handler(secrets.Sources{
		Env:     secrets.Env{Getenv: func(k string) string { return env[k] }},
		Dir:     secrets.Dir{Path: dir},
		Store:   st,
		Environ: func() []string { return []string{"MCP_TOKEN_CRM=env-value", "OPENAI_API_KEY=must-not-list"} },
	}), st
}

func TestSecretsAPIWritesButNeverReadsValues(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "from-file"), []byte("file-value"), 0o600)
	h, st := handler(t, "k1:"+key(1), dir)

	if w := do(h, "PUT", "/api/v1/secrets/crm-api", `{"value":"`+sentinel+`"}`); w.Code != 200 || strings.Contains(w.Body.String(), sentinel) {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	if v, ok := st.Resolve(t.Context(), "crm-api"); !ok || v != sentinel {
		t.Fatal("stored value does not resolve")
	}
	w := do(h, "GET", "/api/v1/secrets", "")
	body := w.Body.String()
	for _, leak := range []string{sentinel, "env-value", "file-value", "must-not-list", "OPENAI_API_KEY"} {
		if strings.Contains(body, leak) {
			t.Fatalf("list leaks %q: %s", leak, body)
		}
	}
	for _, want := range []string{`"MCP_TOKEN_CRM"`, `"environment"`, `"from-file"`, `"directory"`, `"crm-api"`, `"store"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("list lacks %s: %s", want, body)
		}
	}
	// the directory wins over the store in the chain, so a store write under that name is refused
	if w := do(h, "PUT", "/api/v1/secrets/from-file", `{"value":"x-value"}`); w.Code != 409 || !strings.Contains(w.Body.String(), "secret_managed_externally") {
		t.Fatalf("shadowing: %d %s", w.Code, w.Body)
	}
	for _, c := range []struct{ path, body string }{
		{"/api/v1/secrets/Bad_Name", `{"value":"v"}`},
		{"/api/v1/secrets/MCP_TOKEN_X", `{"value":"v"}`},
		{"/api/v1/secrets/ok-name", `{"value":""}`},
		{"/api/v1/secrets/ok-name", `{"value":"has space"}`},
		{"/api/v1/secrets/ok-name", `{"nope":1}`},
	} {
		if w := do(h, "PUT", c.path, c.body); w.Code != 400 {
			t.Fatalf("%s %s: %d %s", c.path, c.body, w.Code, w.Body)
		}
	}
	if w := do(h, "GET", "/api/v1/secrets/crm-api", ""); w.Code != 405 {
		t.Fatalf("a secret must not be readable by name: %d", w.Code)
	}
	if w := do(h, "DELETE", "/api/v1/secrets/crm-api", ""); w.Code != 204 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if w := do(h, "DELETE", "/api/v1/secrets/crm-api", ""); w.Code != 404 {
		t.Fatalf("second delete: %d", w.Code)
	}
}

func TestSecretInUseCannotBeDeleted(t *testing.T) {
	db, err := storage.Open(t.Context(), storage.Config{Dialect: storage.SQLite, DSN: filepath.Join(t.TempDir(), "s.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	kr, _ := secrets.ParseKeyring("k1:" + key(1))
	st := secrets.Store{Repo: db.Secrets(), Keys: kr}
	h := secrets.Handler(secrets.Sources{Store: st, Environ: func() []string { return nil }})
	if w := do(h, "PUT", "/api/v1/secrets/crm-api", `{"value":"v-value"}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if err := db.MCP().CreateServer(t.Context(), storage.MCPServer{ID: "01a16a7b-0000-7000-8000-000000000001", Name: "CRM", Endpoint: "https://crm.example.com/mcp", Transport: "streamable_http", AuthEnv: "crm-api", Enabled: true, ValidationState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	w := do(h, "DELETE", "/api/v1/secrets/crm-api", "")
	if w.Code != 409 || !strings.Contains(w.Body.String(), "secret_in_use") || !strings.Contains(w.Body.String(), "01a16a7b-0000-7000-8000-000000000001") {
		t.Fatalf("delete in use: %d %s", w.Code, w.Body)
	}
	if _, ok := st.Resolve(t.Context(), "crm-api"); !ok {
		t.Fatal("a refused delete removed the secret")
	}
}

func TestSecretsAPIWithoutMasterKeyStillListsExternalSecrets(t *testing.T) {
	h, _ := handler(t, "", "")
	if w := do(h, "PUT", "/api/v1/secrets/crm-api", `{"value":"v-value"}`); w.Code != 503 || !strings.Contains(w.Body.String(), "secrets_store_disabled") {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	if w := do(h, "GET", "/api/v1/secrets", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "MCP_TOKEN_CRM") {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
}

func TestStoredSecretChangesAreAuditedWithoutTheValue(t *testing.T) {
	h, st := handler(t, "k1:"+key(1), "")
	do2 := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Request-ID", "req-"+method)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := do2("PUT", "/api/v1/secrets/crm-api", `{"value":"`+sentinel+`"}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w := do2("PUT", "/api/v1/secrets/crm-api", `{"value":"`+sentinel+`-2"}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w := do2("PUT", "/api/v1/secrets/Bad_Name", `{"value":"v"}`); w.Code != 400 { // a refused write leaves no entry
		t.Fatal(w.Code)
	}
	if w := do2("DELETE", "/api/v1/secrets/crm-api", ""); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if w := do2("DELETE", "/api/v1/secrets/crm-api", ""); w.Code != 404 { // nothing deleted, nothing recorded
		t.Fatal(w.Code)
	}
	w := do2("GET", "/api/v1/secrets", "")
	var env struct {
		Data struct {
			RecentChanges []struct {
				Action    string `json:"action"`
				Name      string `json:"name"`
				RequestID string `json:"request_id"`
			} `json:"recent_changes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	got := env.Data.RecentChanges
	if len(got) != 3 || got[0].Action != "delete" || got[1].Action != "replace" || got[2].Action != "create" || got[0].Name != "crm-api" || got[0].RequestID == "" {
		t.Fatalf("changes = %+v", got)
	}
	if strings.Contains(w.Body.String(), sentinel) {
		t.Fatalf("the audit trail carries a value: %s", w.Body)
	}
	// only a write marked as a request's is audited: re-sealing under a new key is housekeeping, not a change someone made
	if err := st.Put(t.Context(), "quiet", "value-quiet"); err != nil {
		t.Fatal(err)
	}
	rotated := secrets.Store{Repo: st.Repo}
	rotated.Keys, _ = secrets.ParseKeyring("k2:" + key(2) + ",k1:" + key(1))
	if _, err := rotated.Reseal(t.Context()); err != nil {
		t.Fatal(err)
	}
	if changes, _ := st.Repo.Changes(t.Context(), 100); len(changes) != 3 {
		t.Fatalf("housekeeping writes were audited: %+v", changes)
	}
}
