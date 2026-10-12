package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"govoice/server/api"
	"govoice/server/flow"
	"govoice/server/storage"
)

const flowMgmtKey = "management-test-key-32-bytes-long-and-separate"

func flowAPIServer(t *testing.T) *Server {
	t.Helper()
	s := apiTestServer(t)
	store, err := storage.Open(t.Context(), storage.Config{Dialect: storage.SQLite, DSN: filepath.Join(t.TempDir(), "agents.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s.SetFlowManagementAPI(api.NewFlowsHandler(flow.NewService(store, s.availability)))
	return s
}

func flowStages(stt bool) string {
	stages := `"llm":{"provider":"google","model":"m"},"tts":{"provider":"google","voice":"v"}`
	if stt {
		stages = `"stt":{"provider":"google","model":"s"},` + stages
	}
	return stages
}

func flowBody(name, routeTo string) string {
	return `{"name":"` + name + `","config":{"entry":"front","nodes":[` +
		`{"id":"front","name":"Front desk","config":{` + flowStages(true) + `}},` +
		`{"id":"sales","name":"Sales","config":{` + flowStages(false) + `}}],` +
		`"edges":[{"id":"e1","from":"front","to":"` + routeTo + `","condition":"wants to buy"}]}}`
}

type flowResp struct {
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

func TestFlowsAPIAcceptsFlowAndStillAcceptsFlat(t *testing.T) {
	s := flowAPIServer(t)

	w := apiRequest(s, http.MethodPost, "/api/v1/agents", flowMgmtKey, flowBody("two node", "sales"))
	if w.Code != http.StatusCreated {
		t.Fatalf("graph create: %d %s", w.Code, w.Body)
	}
	var graph flowResp
	_ = json.Unmarshal(w.Body.Bytes(), &graph)
	if graph.Data.Readiness.State != flow.ReadinessReady || !strings.Contains(string(graph.Data.Version.Config), `"nodes"`) {
		t.Fatalf("graph agent = %+v", graph.Data)
	}

	flat := `{"name":"flat","config":{` + flowStages(true) + `}}`
	w = apiRequest(s, http.MethodPost, "/api/v1/agents", flowMgmtKey, flat)
	if w.Code != http.StatusCreated {
		t.Fatalf("flat create: %d %s", w.Code, w.Body)
	}
	var f flowResp
	_ = json.Unmarshal(w.Body.Bytes(), &f)
	if strings.Contains(string(f.Data.Version.Config), `"nodes"`) {
		t.Fatalf("a flat config must be stored as sent: %s", f.Data.Version.Config)
	}
}

func TestFlowsAPIRejectsInvalidFlow(t *testing.T) {
	s := flowAPIServer(t)
	w := apiRequest(s, http.MethodPost, "/api/v1/agents", flowMgmtKey, flowBody("bad", "nowhere"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("edge to a missing node: %d %s", w.Code, w.Body)
	}
}

func TestFlowsAPIFlowUpdatePublishesNewVersion(t *testing.T) {
	s := flowAPIServer(t)
	w := apiRequest(s, http.MethodPost, "/api/v1/agents", flowMgmtKey, `{"name":"a","config":{`+flowStages(true)+`}}`)
	var created flowResp
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	body := strings.TrimSuffix(strings.TrimPrefix(flowBody("a", "sales"), `{"name":"a",`), `}`)
	w = apiRequest(s, http.MethodPatch, "/api/v1/agents/"+created.Data.ID, flowMgmtKey, "{"+body+"}")
	if w.Code != http.StatusOK {
		t.Fatalf("graph update: %d %s", w.Code, w.Body)
	}
	var updated flowResp
	_ = json.Unmarshal(w.Body.Bytes(), &updated)
	if updated.Data.Version.Number != 2 || !strings.Contains(string(updated.Data.Version.Config), `"nodes"`) {
		t.Fatalf("updated = %+v", updated.Data)
	}
}
