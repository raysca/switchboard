package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"govoice/server/api"
)

func TestServerConfigTellsTheEditorTheSoundNamesAndQuietCallerDefaultsWithoutPaths(t *testing.T) {
	s := apiTestServer(t)
	key := "management-test-key-32-bytes-long-and-separate"
	get := func() (string, struct{ Data api.ServerConfig }) {
		w := apiRequest(s, http.MethodGet, "/api/v1/server-config", key, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		var out struct{ Data api.ServerConfig }
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return w.Body.String(), out
	}
	body, out := get()
	if out.Data.Audio.Backgrounds == nil || len(out.Data.Audio.Backgrounds) != 0 || !strings.Contains(body, `"backgrounds":[]`) {
		t.Fatalf("no sounds must be an empty list, not null: %s", body)
	}
	if out.Data.Idle.UserTimeoutMS != 15000 || out.Data.Idle.MaxPrompts != 2 || out.Data.Idle.PromptMode != "llm" {
		t.Fatalf("built-in quiet-caller defaults: %+v", out.Data.Idle)
	}
	s.cfg.Audio.Backgrounds = map[string]CueConfig{"office": {File: "/secret/dir/office.wav"}, "cafe": {File: "/secret/dir/cafe.wav"}}
	s.cfg.Audio.AllowFlowBackground, s.cfg.Audio.BackgroundDefault = true, "cafe"
	s.cfg.Idle.UserTimeout, s.cfg.Idle.MaxPrompts, s.cfg.Idle.PromptMode = 8*time.Second, 3, "say"
	body, out = get()
	if out.Data.Audio.BackgroundDefault != "cafe" {
		t.Fatalf("default sound: %q", out.Data.Audio.BackgroundDefault)
	}
	if got := strings.Join(out.Data.Audio.Backgrounds, ","); got != "cafe,office" || !out.Data.Audio.AllowFlowBackground {
		t.Fatalf("sounds %q allow=%v", got, out.Data.Audio.AllowFlowBackground)
	}
	if out.Data.Idle.UserTimeoutMS != 8000 || out.Data.Idle.MaxPrompts != 3 || out.Data.Idle.PromptMode != "say" {
		t.Fatalf("idle: %+v", out.Data.Idle)
	}
	if strings.Contains(body, "/secret") || strings.Contains(body, ".wav") {
		t.Fatalf("a file path left the server: %s", body)
	}
	s.cfg.Idle.UserTimeout = 0
	if _, out = get(); out.Data.Idle.UserTimeoutMS != 0 {
		t.Fatalf("off must read 0: %+v", out.Data.Idle)
	}
}
