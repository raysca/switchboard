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
		"artifacts repositories create switchboard", "switchboard-runtime", "switchboard-deployer",
		"sqladmin.googleapis.com", "sql instances create switchboard-db", "sql databases create switchboard", "roles/cloudsql.client",
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
		if strings.Contains(line, "add-iam-policy-binding") && strings.Contains(line, "switchboard-deployer") &&
			strings.Contains(line, "secrets") && !strings.Contains(line, "SWITCHBOARD_MANAGEMENT_API_KEY") {
			t.Errorf("the deployer must not read other secrets: %s", line)
		}
	}
}

func TestBootstrapRefusesMissingOrMalformedInput(t *testing.T) {
	// GITHUB_REPO is not in this list: when unset, the bootstrap derives it from gh or the origin remote.
	for _, drop := range []string{"PROJECT_ID", "XAI_API_KEY", "ELEVENLABS_API_KEY", "GOOGLE_API_KEY", "GOOGLE_SA_KEY_FILE"} {
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

// The database password and its connection URL never reach the output, and only the runtime account may read the DSN secret.
func TestBootstrapKeepsTheDatabaseSecretToTheRuntimeAccount(t *testing.T) {
	out, err := runBootstrap(t, bootstrapEnv(t))
	if err != nil {
		t.Fatalf("dry run failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "postgres://") {
		t.Fatalf("the dry run printed a connection URL:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "add-iam-policy-binding SWITCHBOARD_DATABASE_DSN") && !strings.Contains(line, "serviceAccount:switchboard-runtime@") {
			t.Errorf("only the runtime account may read the database secret: %s", line)
		}
	}
	if !strings.Contains(out, "--deletion-protection") {
		t.Error("the Cloud SQL instance must be created with deletion protection")
	}
}

func TestBootstrapMountsOnlyTheOptionalProvidersThatAreSet(t *testing.T) {
	out, err := runBootstrap(t, append(bootstrapEnv(t), "OPENAI_API_KEY=SENTINEL-OPENAI"))
	if err != nil {
		t.Fatalf("dry run failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "SENTINEL-OPENAI") {
		t.Fatalf("the dry run printed a provider key:\n%s", out)
	}
	for _, want := range []string{"create secret OPENAI_API_KEY", "secrets add-iam-policy-binding OPENAI_API_KEY --member=serviceAccount:switchboard-runtime@demo-proj", "GCP_PROVIDER_SECRETS=OPENAI_API_KEY"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q", want)
		}
	}
	for _, other := range []string{"DEEPGRAM_API_KEY", "CARTESIA_API_KEY", "NVIDIA_API_KEY"} {
		if strings.Contains(out, other) {
			t.Errorf("%s was not set and must not be in the plan", other)
		}
	}
}
