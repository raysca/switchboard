package server

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type workflowStep struct {
	Name string            `yaml:"name"`
	Run  string            `yaml:"run"`
	Uses string            `yaml:"uses"`
	If   string            `yaml:"if"`
	Env  map[string]string `yaml:"env"`
}

type workflowJob struct {
	RunsOn          string            `yaml:"runs-on"`
	ContinueOnError bool              `yaml:"continue-on-error"`
	Environment     string            `yaml:"environment"`
	Env             map[string]string `yaml:"env"`
	Secrets         string            `yaml:"secrets"`
	Uses            string            `yaml:"uses"`
	Steps           []workflowStep    `yaml:"steps"`
	With            map[string]any    `yaml:"with"`
}

type workflowFile struct {
	Name        string                 `yaml:"name"`
	On          map[string]any         `yaml:"on"`
	Permissions map[string]string      `yaml:"permissions"`
	Concurrency workflowConcurrency    `yaml:"concurrency"`
	Jobs        map[string]workflowJob `yaml:"jobs"`
}

type workflowConcurrency struct {
	Group            string `yaml:"group"`
	CancelInProgress bool   `yaml:"cancel-in-progress"`
}

func loadWorkflow(t *testing.T, path string) workflowFile {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var wf workflowFile
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("%s does not parse: %v", path, err)
	}
	if wf.Name == "" || len(wf.Jobs) == 0 {
		t.Fatalf("%s: missing name or jobs", path)
	}
	return wf
}

func runText(wf workflowFile) string {
	var all []string
	for _, job := range wf.Jobs {
		for _, s := range job.Steps {
			all = append(all, s.Run)
		}
	}
	return strings.Join(all, "\n")
}

func TestDeployWorkflowShape(t *testing.T) {
	wf := loadWorkflow(t, "../.github/workflows/deploy.yml")
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
	raw, err := os.ReadFile("../.github/workflows/deploy.yml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, needle := range []string{
		"google-github-actions/auth@", "workload_identity_provider: ${{ vars.GCP_WIF_PROVIDER }}",
		"github.event.workflow_run.conclusion == 'success'", "github.event.workflow_run.head_sha",
		"envsubst", "deploy/gcp/switchboard.service.yaml", "PROVIDER_ENV", "OPENAI_API_KEY|DEEPGRAM_API_KEY|CARTESIA_API_KEY|NVIDIA_API_KEY", "gcloud run services replace", "candidate", "update-traffic", "smoke-cloudrun.sh",
		// A fork PR whose branch is also called main must never deploy: only our own pushes to main, or a dispatch from main.
		"github.event.workflow_run.event == 'push'", "github.event.workflow_run.head_repository.full_name == github.repository",
		"github.ref == 'refs/heads/main'", `^[0-9]+$`,
		// The demo pipeline is a separate workflow; the server deployment names it but ships no demo.
		"deploy-demo.yml",
	} {
		if !strings.Contains(text, needle) {
			t.Errorf("deploy.yml lacks %q", needle)
		}
	}
	for _, banned := range []string{"credentials_json", "GCP_SA_KEY", "secrets.XAI", "secrets.ELEVEN", "secrets.GOOGLE",
		"deploy/Dockerfile.dental-mcp", "dental-mcp:", "dental-mcp.service.yaml", "DENTAL_MCP_URL", "MCP_TOKEN_DENTAL", "dental-mcp-runtime"} {
		if strings.Contains(text, banned) {
			t.Errorf("deploy.yml must not carry demo asset or stored key %q (the server pipeline is clean-room)", banned)
		}
	}
	// The smoke test runs before the candidate is promoted.
	if strings.Index(text, "smoke-cloudrun.sh") > strings.Index(text, "update-traffic") {
		t.Error("the candidate must be smoke-tested before it is promoted")
	}
}

func TestSmokeScriptMakesNoDemoExpectation(t *testing.T) {
	raw, err := os.ReadFile("../deploy/smoke-cloudrun.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, banned := range []string{"Smile Dental", "demo.auto_install", "mcp-servers", "auth_env_set", "DENTAL_MCP_URL", "DENTAL_MCP_TOKEN", "dentistry"} {
		if strings.Contains(text, banned) {
			t.Errorf("the server smoke test must not expect a demo (%q)", banned)
		}
	}
	if !strings.Contains(text, "empty") && !strings.Contains(text, "no demo") {
		t.Error("the smoke test should say the store may be empty (clean-room)")
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
		"/api/v1/agents", "/metrics", "too_many_attempts"} {
		if !strings.Contains(text, needle) {
			t.Errorf("smoke script lacks a check for %q", needle)
		}
	}
	if strings.Contains(text, "set -x") {
		t.Error("set -x would print the management key")
	}
}

// The optional demo pipeline: one reusable workflow with a dispatch entry, deploying one demo MCP
// service to Cloud Run or Cloudflare, health-checking it, and then seeding the demo with the seed
// client on the runner. No seed image, no Kubernetes Job, no generated token.
func TestDeployDemoWorkflowShape(t *testing.T) {
	wf := loadWorkflow(t, "../.github/workflows/deploy-demo.yml")
	call, ok := wf.On["workflow_call"].(map[string]any)
	if !ok {
		t.Fatalf("deploy-demo.yml must be reusable (workflow_call); on = %v", wf.On)
	}
	inputs, _ := call["inputs"].(map[string]any)
	for _, name := range []string{"demo", "target", "environment", "server_admin_url", "management_key_secret", "mcp_base_url", "auto_seed", "image_tag"} {
		if _, ok := inputs[name]; !ok {
			t.Errorf("deploy-demo.yml lacks input %q", name)
		}
	}
	if wf.Permissions["id-token"] != "write" {
		t.Error("the Cloud Run target authenticates with Workload Identity Federation: id-token: write")
	}
	if !strings.Contains(wf.Concurrency.Group, "deploy-demo") || wf.Concurrency.CancelInProgress {
		t.Errorf("concurrency = %+v: one demo seed at a time, never cancelled half way", wf.Concurrency)
	}
	job, ok := wf.Jobs["deploy"]
	if !ok {
		t.Fatalf("jobs = %v", wf.Jobs)
	}
	if job.Environment != "${{ inputs.environment }}" {
		t.Errorf("the job must target the input environment (per-demo token secrets): %q", job.Environment)
	}

	raw, err := os.ReadFile("../.github/workflows/deploy-demo.yml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	runs := runText(wf)
	for _, needle := range []string{
		"go run ./demos/seed", "--key-env GOVOICE_MANAGEMENT_API_KEY", "--demo", "--mcp", "--yes",
		"deploy/cloudrun/deploy.sh",                 // Cloud Run assets per demo
		"wrangler", "secret put", "${MCP_BASE_URL}", // Cloudflare assets and the required Worker URL
		"::add-mask::",                          // every secret is masked
		"gcloud secrets versions access latest", // Cloud Run management key + demo token source
		`--secret="${DEMO_TOKEN_SECRET}"`,       // Cloud Run demo token is fetched for the seed step
	} {
		if !strings.Contains(runs, needle) {
			t.Errorf("deploy-demo.yml runs nothing containing %q", needle)
		}
	}
	// Masking and printing rules: no secret value is printed, and the auto-seed-off command uses env indirection.
	if !strings.Contains(runs, `echo "::add-mask::${MANAGEMENT_KEY_VALUE}"`) {
		t.Error("the management key fetched at seed time must be masked before use")
	}
	for _, banned := range []string{"openssl rand", "demos/seed/Dockerfile", "kind: Job", "kubectl", "echo \"${MANAGEMENT_KEY}\"", "echo ${DENTAL_MCP_TOKEN}"} {
		if strings.Contains(text, banned) {
			t.Errorf("deploy-demo.yml must not contain %q (no generated token, no seed image, no printed secret)", banned)
		}
	}
	// The seed step is gated on auto_seed; a manual command (env indirection only) is printed otherwise.
	var seedSteps, manualSteps int
	for _, step := range job.Steps {
		if strings.Contains(step.Run, `go run ./demos/seed`) && strings.Contains(step.Run, `GOVOICE_MANAGEMENT_API_KEY="${MANAGEMENT_KEY_VALUE}"`) {
			seedSteps++
			if !strings.Contains(step.If, "inputs.auto_seed") {
				t.Errorf("the seed step must be gated on auto_seed: if = %q", step.If)
			}
		}
		if strings.Contains(step.Run, `GOVOICE_MANAGEMENT_API_KEY="<the management key>"`) {
			manualSteps++
			if !strings.Contains(step.If, "!inputs.auto_seed") {
				t.Errorf("the printed seed command must run when auto_seed is off: if = %q", step.If)
			}
		}
	}
	if seedSteps != 1 || manualSteps != 1 {
		t.Errorf("seed steps = %d, printed-command steps = %d (want 1 each)", seedSteps, manualSteps)
	}
	// Per-demo token secrets come from the environment, never generated here.
	for _, demo := range []string{"DENTAL_MCP_TOKEN", "ATLAS_MCP_TOKEN"} {
		if !strings.Contains(text, "secrets."+demo) {
			t.Errorf("deploy-demo.yml must read the %s GitHub environment secret (per-demo token)", demo)
		}
	}
}

func TestDeployDemoDispatchEntry(t *testing.T) {
	wf := loadWorkflow(t, "../.github/workflows/deploy-demo-dispatch.yml")
	if _, ok := wf.On["workflow_dispatch"]; !ok {
		t.Fatalf("the dispatch entry must be workflow_dispatch; on = %v", wf.On)
	}
	call, ok := wf.On["workflow_dispatch"].(map[string]any)
	if !ok {
		t.Fatalf("workflow_dispatch = %v", wf.On["workflow_dispatch"])
	}
	inputs, _ := call["inputs"].(map[string]any)
	for _, name := range []string{"demo", "target", "environment", "server_admin_url", "management_key_secret", "mcp_base_url", "auto_seed", "image_tag"} {
		if _, ok := inputs[name]; !ok {
			t.Errorf("the dispatch entry lacks input %q", name)
		}
	}
	job, ok := wf.Jobs["deploy"]
	if !ok {
		t.Fatalf("jobs = %v", wf.Jobs)
	}
	if job.Uses != "./.github/workflows/deploy-demo.yml" {
		t.Errorf("the dispatch entry must call the reusable workflow, uses = %q", job.Uses)
	}
	if job.Secrets != "inherit" {
		t.Errorf("the dispatch entry must pass the environment secrets (secrets: inherit), got %q", job.Secrets)
	}
	for _, name := range []string{"demo", "target", "environment", "server_admin_url", "management_key_secret", "mcp_base_url", "auto_seed", "image_tag"} {
		if _, ok := job.With[name]; !ok {
			t.Errorf("the dispatch entry does not forward input %q", name)
		}
	}
}
