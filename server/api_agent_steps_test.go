package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"govoice/server/agent"
	"govoice/server/api"
	"govoice/server/storage"
)

const agentMgmtKey = "management-test-key-32-bytes-long-and-separate"

func agentAPIServer(t *testing.T) *Server {
	t.Helper()
	s := apiTestServer(t)
	store, err := storage.Open(t.Context(), storage.Config{Dialect: storage.SQLite, DSN: filepath.Join(t.TempDir(), "agents.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s.setAgentManagementAPI(api.NewAgentsHandler(agent.NewService(store, s.availability)))
	return s
}

func agentStages(stt bool) string {
	stages := `"llm":{"provider":"google","model":"m"},"tts":{"provider":"google","voice":"v"}`
	if stt {
		stages = `"stt":{"provider":"google","model":"s"},` + stages
	}
	return stages
}

func agentBody(name, routeTo string) string {
	return `{"name":"` + name + `","config":{"start":"front","steps":[` +
		`{"id":"front","name":"Front desk","config":{` + agentStages(true) + `}},` +
		`{"id":"sales","name":"Sales","config":{` + agentStages(false) + `}}],` +
		`"routes":[{"id":"e1","from":"front","to":"` + routeTo + `","condition":"wants to buy"}]}}`
}

type agentResp struct {
	Data struct {
		ID      string `json:"id"`
		Version struct {
			Number int             `json:"number"`
			Config json.RawMessage `json:"config"`
		} `json:"current_version"`
		Readiness struct {
			State string `json:"state"`
		} `json:"readiness"`
	} `json:"data"`
}

func TestAgentsAPIAcceptsAgentAndStillAcceptsFlat(t *testing.T) {
	s := agentAPIServer(t)

	w := apiRequest(s, http.MethodPost, "/api/v1/flows", agentMgmtKey, agentBody("two step", "sales"))
	if w.Code != http.StatusCreated {
		t.Fatalf("graph create: %d %s", w.Code, w.Body)
	}
	var graph agentResp
	_ = json.Unmarshal(w.Body.Bytes(), &graph)
	if graph.Data.Readiness.State != agent.ReadinessReady || !strings.Contains(string(graph.Data.Version.Config), `"steps"`) {
		t.Fatalf("graph flow = %+v", graph.Data)
	}

	flat := `{"name":"flat","config":{` + agentStages(true) + `}}`
	w = apiRequest(s, http.MethodPost, "/api/v1/flows", agentMgmtKey, flat)
	if w.Code != http.StatusCreated {
		t.Fatalf("flat create: %d %s", w.Code, w.Body)
	}
	var f agentResp
	_ = json.Unmarshal(w.Body.Bytes(), &f)
	if strings.Contains(string(f.Data.Version.Config), `"steps"`) {
		t.Fatalf("a flat config must be stored as sent: %s", f.Data.Version.Config)
	}
}

func TestAgentsAPIRejectsInvalidAgent(t *testing.T) {
	s := agentAPIServer(t)
	w := apiRequest(s, http.MethodPost, "/api/v1/flows", agentMgmtKey, agentBody("bad", "nowhere"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("route to a missing step: %d %s", w.Code, w.Body)
	}
}

func TestAgentsAPIAgentUpdatePublishesNewVersion(t *testing.T) {
	s := agentAPIServer(t)
	w := apiRequest(s, http.MethodPost, "/api/v1/flows", agentMgmtKey, `{"name":"a","config":{`+agentStages(true)+`}}`)
	var created agentResp
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	body := strings.TrimSuffix(strings.TrimPrefix(agentBody("a", "sales"), `{"name":"a",`), `}`)
	w = apiRequest(s, http.MethodPatch, "/api/v1/flows/"+created.Data.ID, agentMgmtKey, "{"+body+"}")
	if w.Code != http.StatusOK {
		t.Fatalf("graph update: %d %s", w.Code, w.Body)
	}
	var updated agentResp
	_ = json.Unmarshal(w.Body.Bytes(), &updated)
	if updated.Data.Version.Number != 2 || !strings.Contains(string(updated.Data.Version.Config), `"steps"`) {
		t.Fatalf("updated = %+v", updated.Data)
	}
}
