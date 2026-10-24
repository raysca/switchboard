package server

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// The retention status is served behind the management key, reports the policy in force, and a run shows in the
// metrics: runs by result, what was removed, and the age of the last success.
func TestRetentionStatusEndpointAndMetrics(t *testing.T) {
	s := apiTestServer(t)
	key := "management-test-key-32-bytes-long-and-separate"
	t.Setenv("GOVOICE_DATABASE_DSN", filepath.Join(t.TempDir(), "retention.db"))
	s.cfg.Storage.Dialect = "sqlite"
	s.cfg.Retention.Enabled = true
	s.cfg.Retention.ContentRetentionDays = 7
	s.cfg.Retention.MetadataRetentionDays = 30

	if code := apiRequest(s, http.MethodGet, "/api/v1/retention", key, "").Code; code != http.StatusServiceUnavailable {
		t.Fatalf("before management is enabled: %d", code)
	}
	if err := s.EnableManagement(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.closeManagement)

	if code := apiRequest(s, http.MethodGet, "/api/v1/retention", "", "").Code; code != http.StatusUnauthorized {
		t.Fatalf("without the key: %d", code)
	}
	w := apiRequest(s, http.MethodGet, "/api/v1/retention?dry_run=true", key, "")
	for _, want := range []string{`"enabled":true`, `"content_retention_days":7`, `"metadata_retention_days":30`, `"last_run":null`, `"dry_run":{`, `"metadata_sessions":`, `"content_turns":`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("status lacks %s: %d %s", want, w.Code, w.Body)
		}
	}

	if _, err := s.mgmt.Load().retention.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if w := apiRequest(s, http.MethodGet, "/api/v1/retention", key, ""); strings.Contains(w.Body.String(), `"last_run":null`) {
		t.Fatalf("a run is not reported: %s", w.Body)
	}
	metrics := scrape(t, s)
	for _, want := range []string{"voice_retention_enabled 1", `voice_retention_runs_total{result="ok"} 1`, `voice_retention_runs_total{result="failed"} 0`, "voice_retention_last_success_age_seconds 0"} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("metrics lack %q:\n%s", want, grep(metrics, "voice_retention"))
		}
	}
}

func TestRetentionStatusWhenTheJobIsOff(t *testing.T) {
	s := apiTestServer(t)
	key := "management-test-key-32-bytes-long-and-separate"
	t.Setenv("GOVOICE_DATABASE_DSN", filepath.Join(t.TempDir(), "retention-off.db"))
	s.cfg.Storage.Dialect = "sqlite"
	s.cfg.Retention.Enabled = false
	if err := s.EnableManagement(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.closeManagement)
	if w := apiRequest(s, http.MethodGet, "/api/v1/retention", key, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("off: %d %s", w.Code, w.Body)
	}
	if w := apiRequest(s, http.MethodGet, "/api/v1/retention?dry_run=true", key, ""); w.Code != http.StatusConflict {
		t.Fatalf("off dry run: %d %s", w.Code, w.Body)
	}
	if m := scrape(t, s); !strings.Contains(m, "voice_retention_enabled 0") {
		t.Fatal("the metric says the job runs")
	}
}

func grep(text, needle string) string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, needle) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
