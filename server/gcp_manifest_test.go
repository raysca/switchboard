package server

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Every secret name the clean-room server's manifest and the bootstrap script must agree on. Demo
// tokens are NOT here: the server carries no demo secret; the bootstrap creates them for the demo
// deployment and the demo Cloud Run manifests mount them.
var gcpSecretNames = []string{"TOKEN_SECRET", "SWITCHBOARD_MANAGEMENT_API_KEY", "SWITCHBOARD_DATABASE_DSN", "SWITCHBOARD_SECRETS_KEY", "xai-api-key", "elevenlabs-api-key", "google-api-key", "GOOGLE_SA_KEY"}

// The per-demo bearer tokens the bootstrap creates as deployment secrets and the demo pipeline reads.
var gcpDemoTokenSecrets = []string{"DENTAL_MCP_TOKEN", "ATLAS_MCP_TOKEN"}

// The placeholders the workflow's envsubst fills in; any other ${VAR} in a manifest is a typo.
var gcpSample = map[string]string{
	"REGION": "us-central1", "PROJECT_ID": "demo-proj", "IMAGE": "us-central1-docker.pkg.dev/demo-proj/switchboard/switchboard:abc123",
	"MIN_INSTANCES": "1", "PROVIDER_ENV": "",
}

type manifest struct {
	Spec struct {
		Template struct {
			Metadata struct {
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"metadata"`
			Spec struct {
				ServiceAccountName   string `yaml:"serviceAccountName"`
				ContainerConcurrency int    `yaml:"containerConcurrency"`
				TimeoutSeconds       int    `yaml:"timeoutSeconds"`
				Containers           []struct {
					Command []string `yaml:"command"`
					Args    []string `yaml:"args"`
					Ports   []struct {
						ContainerPort int `yaml:"containerPort"`
					} `yaml:"ports"`
					Env []struct {
						Name      string `yaml:"name"`
						Value     string `yaml:"value"`
						ValueFrom *struct {
							SecretKeyRef struct {
								Name string `yaml:"name"`
								Key  string `yaml:"key"`
							} `yaml:"secretKeyRef"`
						} `yaml:"valueFrom"`
					} `yaml:"env"`
					Resources struct {
						Limits map[string]string `yaml:"limits"`
					} `yaml:"resources"`
					StartupProbe struct {
						HTTPGet struct {
							Path string `yaml:"path"`
							Port int    `yaml:"port"`
						} `yaml:"httpGet"`
					} `yaml:"startupProbe"`
					VolumeMounts []struct {
						Name      string `yaml:"name"`
						MountPath string `yaml:"mountPath"`
					} `yaml:"volumeMounts"`
				} `yaml:"containers"`
				Volumes []struct {
					Secret struct {
						SecretName string `yaml:"secretName"`
					} `yaml:"secret"`
				} `yaml:"volumes"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

func renderManifest(t *testing.T, path string) manifest {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return parseManifest(t, path, string(raw), gcpSample)
}

// parseManifest expands ${VAR} placeholders from sample (any unknown placeholder fails) and decodes
// the result with the Cloud Run service shape above.
func parseManifest(t *testing.T, path, text string, sample map[string]string) manifest {
	t.Helper()
	for _, m := range regexp.MustCompile(`\$\{([A-Z_]+)\}`).FindAllStringSubmatch(text, -1) {
		if _, ok := sample[m[1]]; !ok {
			t.Fatalf("%s uses unknown placeholder ${%s}", path, m[1])
		}
	}
	var m manifest
	if err := yaml.Unmarshal([]byte(os.Expand(text, func(k string) string { return sample[k] })), &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

// secretEnv maps env var -> Secret Manager secret name, requiring every reference to follow "latest".
func secretEnv(t *testing.T, m manifest) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, e := range m.Spec.Template.Spec.Containers[0].Env {
		if e.ValueFrom == nil {
			continue
		}
		if e.ValueFrom.SecretKeyRef.Key != "latest" {
			t.Errorf("env %s: secret %s must follow latest, got %q", e.Name, e.ValueFrom.SecretKeyRef.Name, e.ValueFrom.SecretKeyRef.Key)
		}
		out[e.Name] = e.ValueFrom.SecretKeyRef.Name
	}
	return out
}

func TestVoiceServiceManifest(t *testing.T) {
	m := renderManifest(t, "../deploy/gcp/switchboard.service.yaml")
	a, spec := m.Spec.Template.Metadata.Annotations, m.Spec.Template.Spec
	c := spec.Containers[0]
	for k, want := range map[string]string{
		"autoscaling.knative.dev/minScale": "1", "autoscaling.knative.dev/maxScale": "1",
		"run.googleapis.com/execution-environment": "gen2", "run.googleapis.com/cpu-throttling": "false",
	} {
		if a[k] != want {
			t.Errorf("annotation %s = %q, want %q", k, a[k], want)
		}
	}
	if _, ok := a["run.googleapis.com/cloudsql-instances"]; ok {
		t.Error("the SQLite profile mounts no Cloud SQL instance")
	}
	if spec.TimeoutSeconds != 3600 || spec.ContainerConcurrency != 20 || c.Resources.Limits["cpu"] != "2" || c.Resources.Limits["memory"] != "2Gi" {
		t.Errorf("sizing: timeout=%d concurrency=%d limits=%v", spec.TimeoutSeconds, spec.ContainerConcurrency, c.Resources.Limits)
	}
	if spec.ServiceAccountName != "switchboard-runtime@demo-proj.iam.gserviceaccount.com" {
		t.Errorf("service account = %q", spec.ServiceAccountName)
	}
	if strings.Join(c.Args, " ") != "-- /opt/govoice/bin/server -config /etc/govoice/cloudrun-sqlite.yaml" || len(c.Command) != 1 || c.Command[0] != "/usr/bin/tini" {
		t.Errorf("command/args = %v %v", c.Command, c.Args)
	}
	if c.StartupProbe.HTTPGet.Path != "/health" || c.StartupProbe.HTTPGet.Port != 8080 || c.Ports[0].ContainerPort != 8080 {
		t.Errorf("probe/port: %+v %+v", c.StartupProbe, c.Ports)
	}
	want := map[string]string{
		"TOKEN_SECRET": "TOKEN_SECRET", "GOVOICE_MANAGEMENT_API_KEY": "SWITCHBOARD_MANAGEMENT_API_KEY",
		"SWITCHBOARD_SECRETS_KEY": "SWITCHBOARD_SECRETS_KEY",
		"XAI_API_KEY":             "xai-api-key", "ELEVENLABS_API_KEY": "elevenlabs-api-key", "GOOGLE_API_KEY": "google-api-key",
	}
	got := secretEnv(t, m)
	for env, secret := range want {
		if got[env] != secret {
			t.Errorf("env %s must come from secret %s, got %q", env, secret, got[env])
		}
	}
	secretish := regexp.MustCompile(`KEY|SECRET|TOKEN|CREDENTIALS`)
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
		if secretish.MatchString(e.Name) && e.ValueFrom == nil && e.Name != "GOOGLE_APPLICATION_CREDENTIALS" {
			t.Errorf("env %s must come from Secret Manager, not a literal", e.Name)
		}
	}
	if env["GOOGLE_APPLICATION_CREDENTIALS"] != "/secrets/google/key.json" {
		t.Errorf("GOOGLE_APPLICATION_CREDENTIALS = %q", env["GOOGLE_APPLICATION_CREDENTIALS"])
	}
	if env["GOVOICE_DATABASE_DSN"] != "/tmp/switchboard.db" {
		t.Errorf("the SQLite database is a tmpfs file, not a secret: GOVOICE_DATABASE_DSN = %q", env["GOVOICE_DATABASE_DSN"])
	}
	if len(spec.Volumes) != 1 || spec.Volumes[0].Secret.SecretName != "GOOGLE_SA_KEY" || c.VolumeMounts[0].MountPath != "/secrets/google" {
		t.Errorf("the Google key must be a mounted secret file: %+v %+v", spec.Volumes, c.VolumeMounts)
	}
	// Clean-room: the server manifest carries no demo URL, demo token or demo service account.
	raw, err := os.ReadFile("../deploy/gcp/switchboard.service.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"DENTAL_MCP_URL", "ATLAS_DEMO_URL", "MCP_TOKEN_", "DENTAL_MCP_TOKEN", "dental-mcp-runtime", "demo.auto_install"} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("the server manifest must not carry demo asset %q", banned)
		}
	}
}

func TestManifestSecretsAreTheOnesTheBootstrapCreates(t *testing.T) {
	boot, err := os.ReadFile("../deploy/gcp/bootstrap.sh")
	if err != nil {
		t.Fatalf("read bootstrap.sh: %v", err)
	}
	for _, name := range append(append([]string{}, gcpSecretNames...), gcpDemoTokenSecrets...) {
		if !strings.Contains(string(boot), name) {
			t.Errorf("bootstrap.sh does not create secret %s", name)
		}
	}
	// The encrypted secret store needs the keyring format, not a bare random token.
	if !strings.Contains(string(boot), "put_secret SWITCHBOARD_SECRETS_KEY keyring") {
		t.Error("bootstrap.sh must create SWITCHBOARD_SECRETS_KEY in the keyring format (id:base64key)")
	}
}

// The demo deployment assets moved out of deploy/: the server deployment must not reference them.
func TestServerDeploymentShipsNoDemoAssets(t *testing.T) {
	for _, gone := range []string{"../deploy/Dockerfile.dental-mcp", "../deploy/gcp/dental-mcp.service.yaml"} {
		if _, err := os.Stat(gone); err == nil {
			t.Errorf("%s must move under demos/<name>/deploy", gone)
		}
	}
	for _, path := range []string{"../demos/dental/deploy/cloudrun/Dockerfile.mcp", "../demos/atlas/deploy/cloudrun/Dockerfile.mcp"} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("missing demo asset %s: %v", path, err)
		}
	}
}

// The workflow builds PROVIDER_ENV from GCP_PROVIDER_SECRETS and envsubst puts it into the env list: the
// result must stay valid YAML with one secret-backed entry per name, and an empty list must change nothing.
func TestProviderEnvInjectionKeepsTheManifestValid(t *testing.T) {
	raw, err := os.ReadFile("../deploy/gcp/switchboard.service.yaml")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"OPENAI_API_KEY", "DEEPGRAM_API_KEY"}
	var inject strings.Builder
	for _, n := range names { // the exact shape deploy.yml builds
		inject.WriteString("\n            - name: " + n + "\n              valueFrom: {secretKeyRef: {name: " + n + ", key: latest}}")
	}
	for _, withKeys := range []bool{false, true} {
		sample := map[string]string{}
		for k, v := range gcpSample {
			sample[k] = v
		}
		if withKeys {
			sample["PROVIDER_ENV"] = inject.String()
		}
		var m manifest
		if err := yaml.Unmarshal([]byte(os.Expand(string(raw), func(k string) string { return sample[k] })), &m); err != nil {
			t.Fatalf("withKeys=%v: %v", withKeys, err)
		}
		got := secretEnv(t, m)
		for _, n := range names {
			if (got[n] == n) != withKeys {
				t.Errorf("withKeys=%v: env %s = %q", withKeys, n, got[n])
			}
		}
		if got["TOKEN_SECRET"] != "TOKEN_SECRET" || got["SWITCHBOARD_SECRETS_KEY"] != "SWITCHBOARD_SECRETS_KEY" {
			t.Errorf("withKeys=%v: the fixed secrets were lost: %v", withKeys, got)
		}
	}
}
