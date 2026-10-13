package server

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// bootstrapEnv is a complete, valid input (the key files and values are sentinels the output must never contain).
func bootstrapEnv(t *testing.T) []string {
	t.Helper()
	sa := t.TempDir() + "/sa.json"
	if err := os.WriteFile(sa, []byte(`{"type":"service_account","private_key":"SENTINEL-PRIVATE-KEY"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"PATH=" + os.Getenv("PATH"), "DRY_RUN=1", "PROJECT_ID=demo-proj", "GITHUB_REPO=acme/go-voice",
		"XAI_API_KEY=SENTINEL-XAI", "ELEVENLABS_API_KEY=SENTINEL-ELEVEN", "GOOGLE_API_KEY=SENTINEL-GOOGLE", "GOOGLE_SA_KEY_FILE=" + sa}
}

func runBootstrap(t *testing.T, env []string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", "../deploy/gcp/bootstrap.sh")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestBootstrapDryRunPrintsThePlanAndNoSecret(t *testing.T) {
	out, err := runBootstrap(t, bootstrapEnv(t))
	if err != nil {
		t.Fatalf("dry run failed: %v\n%s", err, out)
	}
	for _, s := range []string{"SENTINEL-XAI", "SENTINEL-ELEVEN", "SENTINEL-GOOGLE", "SENTINEL-PRIVATE-KEY"} {
		if strings.Contains(out, s) {
			t.Fatalf("the dry run printed a secret value (%s):\n%s", s, out)
		}
	}
	for _, want := range []string{
		"run.googleapis.com", "artifactregistry.googleapis.com", "secretmanager.googleapis.com", "iamcredentials.googleapis.com",
		"artifacts repositories create govoice", "govoice-runtime", "govoice-deployer",
		"workload-identity-pools create github", "attribute.repository", "assertion.repository=='acme/go-voice'", "assertion.ref=='refs/heads/main'",
		"roles/run.admin", "roles/iam.workloadIdentityUser",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q", want)
		}
	}
	for _, name := range gcpSecretNames {
		if !strings.Contains(out, "secret "+name) {
			t.Errorf("plan does not create secret %s", name)
		}
	}
	// The public MCP service gets its own account, which may read exactly one secret.
	for _, want := range []string{"dental-mcp-runtime", "secrets add-iam-policy-binding DENTAL_MCP_TOKEN --member=serviceAccount:dental-mcp-runtime@demo-proj", "billingbudgets.googleapis.com"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q", want)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "add-iam-policy-binding") && strings.Contains(line, "dental-mcp-runtime@") &&
			strings.Contains(line, "secrets") && !strings.Contains(line, "DENTAL_MCP_TOKEN") {
			t.Errorf("dental-mcp-runtime may read only DENTAL_MCP_TOKEN: %s", line)
		}
	}
	// The deployer may read exactly one secret (the management key, for the smoke test), never a provider key.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "add-iam-policy-binding") && strings.Contains(line, "govoice-deployer") &&
			strings.Contains(line, "secrets") && !strings.Contains(line, "GOVOICE_MANAGEMENT_API_KEY") {
			t.Errorf("the deployer must not read other secrets: %s", line)
		}
	}
}

func TestBootstrapRefusesMissingOrMalformedInput(t *testing.T) {
	for _, drop := range []string{"PROJECT_ID", "GITHUB_REPO", "XAI_API_KEY", "ELEVENLABS_API_KEY", "GOOGLE_API_KEY", "GOOGLE_SA_KEY_FILE"} {
		env := append(bootstrapEnv(t), drop+"=") // the later entry wins: this one variable is empty
		out, err := runBootstrap(t, env)
		if err == nil || !strings.Contains(out, drop+" is required") {
			t.Errorf("without %s: err=%v out=%s", drop, err, out)
		}
	}
	if out, err := runBootstrap(t, append(bootstrapEnv(t), "GITHUB_REPO=not-a-repo")); err == nil || !strings.Contains(out, "owner/name") {
		t.Errorf("a malformed GITHUB_REPO must be refused: %v %s", err, out)
	}
	if out, err := runBootstrap(t, append(bootstrapEnv(t), "GOOGLE_SA_KEY_FILE=/nonexistent/sa.json")); err == nil || !strings.Contains(out, "readable file") {
		t.Errorf("an unreadable key file must be refused: %v %s", err, out)
	}
}
