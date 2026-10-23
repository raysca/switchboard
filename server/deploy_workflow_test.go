package server

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDeployWorkflowShape(t *testing.T) {
	raw, err := os.ReadFile("../.github/workflows/deploy.yml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	var wf struct {
		On          map[string]any    `yaml:"on"`
		Permissions map[string]string `yaml:"permissions"`
		Concurrency struct {
			Group            string `yaml:"group"`
			CancelInProgress bool   `yaml:"cancel-in-progress"`
		} `yaml:"concurrency"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	if wf.Permissions["id-token"] != "write" || wf.Permissions["contents"] != "read" || len(wf.Permissions) != 2 {
		t.Errorf("permissions = %v (WIF needs id-token: write; nothing else)", wf.Permissions)
	}
	for _, trigger := range []string{"workflow_run", "workflow_dispatch"} {
		if _, ok := wf.On[trigger]; !ok {
			t.Errorf("missing trigger %s", trigger)
		}
	}
	if _, ok := wf.On["push"]; ok {
		t.Error("deploy must follow CI (workflow_run), not race it on push")
	}
	if wf.Concurrency.Group != "deploy" || wf.Concurrency.CancelInProgress {
		t.Errorf("concurrency = %+v: a deploy must never be cancelled half way", wf.Concurrency)
	}
	for _, needle := range []string{
		"google-github-actions/auth@", "workload_identity_provider: ${{ vars.GCP_WIF_PROVIDER }}",
		"github.event.workflow_run.conclusion == 'success'", "github.event.workflow_run.head_sha",
		"envsubst", "deploy/gcp/switchboard.service.yaml", "PROVIDER_ENV", "OPENAI_API_KEY|DEEPGRAM_API_KEY|CARTESIA_API_KEY|NVIDIA_API_KEY", "gcloud run services replace", "candidate", "update-traffic", "smoke-cloudrun.sh",
		// A fork PR whose branch is also called main must never deploy: only our own pushes to main, or a dispatch from main.
		"github.event.workflow_run.event == 'push'", "github.event.workflow_run.head_repository.full_name == github.repository",
		"github.ref == 'refs/heads/main'", `^[0-9]+$`,
	} {
		if !strings.Contains(text, needle) {
			t.Errorf("deploy.yml lacks %q", needle)
		}
	}
	for _, banned := range []string{"credentials_json", "GCP_SA_KEY", "secrets.XAI", "secrets.ELEVEN", "secrets.GOOGLE"} {
		if strings.Contains(text, banned) {
			t.Errorf("deploy.yml must not use %q: no stored GCP key, no provider key in GitHub", banned)
		}
	}
	// The smoke test runs before the candidate is promoted.
	if strings.Index(text, "smoke-cloudrun.sh") > strings.Index(text, "update-traffic") {
		t.Error("the candidate must be smoke-tested before it is promoted")
	}
}

func TestSmokeScriptWaitsForTheInstallBeforeCheckingItsMCPServer(t *testing.T) {
	raw, _ := os.ReadFile("../deploy/smoke-cloudrun.sh")
	text := string(raw)
	if strings.Index(text, "auth_env_set") < strings.Index(text, `"Smile Dental front desk"`) {
		t.Error("the MCP server check must come after the wait for the background install (it is discovered by the same install)")
	}
	if strings.Contains(text, "| grep -q") && strings.Contains(text, "curl -fsS \"${base}/\" |") {
		t.Error("curl | grep -q can SIGPIPE under pipefail: capture the body first")
	}
}

func TestSmokeScript(t *testing.T) {
	if out, err := exec.Command("bash", "-n", "../deploy/smoke-cloudrun.sh").CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
	raw, err := os.ReadFile("../deploy/smoke-cloudrun.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, needle := range []string{"/health", "client_ip", ".data.secure", "/api/v1/providers", "elevenlabs", "xai", "google",
		"/api/v1/mcp-servers", "auth_env_set", "/api/v1/agents", "/metrics", "too_many_attempts"} {
		if !strings.Contains(text, needle) {
			t.Errorf("smoke script lacks a check for %q", needle)
		}
	}
	if strings.Contains(text, "set -x") {
		t.Error("set -x would print the management key")
	}
}
