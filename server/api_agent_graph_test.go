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

const graphMgmtKey = "management-test-key-32-bytes-long-and-separate"

func graphAPIServer(t *testing.T) *Server {
	t.Helper()
	s := apiTestServer(t)
	store, err := storage.Open(t.Context(), storage.Config{Dialect: storage.SQLite, DSN: filepath.Join(t.TempDir(), "agents.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s.SetAgentManagementAPI(api.NewAgentsHandler(agent.NewService(store, s.availability)))
	return s
}

func graphStages(stt bool) string {
	stages := `"llm":{"provider":"google","model":"m"},"tts":{"provider":"google","voice":"v"}`
	if stt {
		stages = `"stt":{"provider":"google","model":"s"},` + stages
	}
	return stages
}

func graphBody(name, edgeTo string) string {
	return `{"name":"` + name + `","config":{"entry":"front","nodes":[` +
		`{"id":"front","name":"Front desk","config":{` + graphStages(true) + `}},` +
		`{"id":"sales","name":"Sales","config":{` + graphStages(false) + `}}],` +
		`"edges":[{"id":"e1","from":"front","to":"` + edgeTo + `","condition":"wants to buy"}]}}`
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

func TestAgentsAPIAcceptsGraphAndStillAcceptsFlat(t *testing.T) {
	s := graphAPIServer(t)

	w := apiRequest(s, http.MethodPost, "/api/v1/agents", graphMgmtKey, graphBody("two node", "sales"))
	if w.Code != http.StatusCreated {
		t.Fatalf("graph create: %d %s", w.Code, w.Body)
	}
	var graph agentResp
	_ = json.Unmarshal(w.Body.Bytes(), &graph)
	if graph.Data.Readiness.State != agent.ReadinessReady || !strings.Contains(string(graph.Data.Version.Config), `"nodes"`) {
		t.Fatalf("graph agent = %+v", graph.Data)
	}

	flat := `{"name":"flat","config":{` + graphStages(true) + `}}`
	w = apiRequest(s, http.MethodPost, "/api/v1/agents", graphMgmtKey, flat)
	if w.Code != http.StatusCreated {
		t.Fatalf("flat create: %d %s", w.Code, w.Body)
	}
	var f agentResp
	_ = json.Unmarshal(w.Body.Bytes(), &f)
	if strings.Contains(string(f.Data.Version.Config), `"nodes"`) {
		t.Fatalf("a flat config must be stored as sent: %s", f.Data.Version.Config)
	}
}

func TestAgentsAPIRejectsInvalidGraph(t *testing.T) {
	s := graphAPIServer(t)
	w := apiRequest(s, http.MethodPost, "/api/v1/agents", graphMgmtKey, graphBody("bad", "nowhere"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("edge to a missing node: %d %s", w.Code, w.Body)
	}
}

func TestAgentsAPIGraphUpdatePublishesNewVersion(t *testing.T) {
	s := graphAPIServer(t)
	w := apiRequest(s, http.MethodPost, "/api/v1/agents", graphMgmtKey, `{"name":"a","config":{`+graphStages(true)+`}}`)
	var created agentResp
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	body := strings.TrimSuffix(strings.TrimPrefix(graphBody("a", "sales"), `{"name":"a",`), `}`)
	w = apiRequest(s, http.MethodPatch, "/api/v1/agents/"+created.Data.ID, graphMgmtKey, "{"+body+"}")
	if w.Code != http.StatusOK {
		t.Fatalf("graph update: %d %s", w.Code, w.Body)
	}
	var updated agentResp
	_ = json.Unmarshal(w.Body.Bytes(), &updated)
	if updated.Data.Version.Number != 2 || !strings.Contains(string(updated.Data.Version.Config), `"nodes"`) {
		t.Fatalf("updated = %+v", updated.Data)
	}
}
