package server

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Every secret name the deployed services and the bootstrap script must agree on.
var gcpSecretNames = []string{"TOKEN_SECRET", "GOVOICE_MANAGEMENT_API_KEY", "XAI_API_KEY", "ELEVENLABS_API_KEY", "GOOGLE_API_KEY", "GOOGLE_SA_KEY", "DENTAL_MCP_TOKEN"}

// The placeholders the workflow's envsubst fills in; any other ${VAR} in a manifest is a typo.
var gcpSample = map[string]string{
	"REGION": "us-central1", "PROJECT_ID": "demo-proj", "IMAGE": "us-central1-docker.pkg.dev/demo-proj/govoice/govoice:abc123",
	"MIN_INSTANCES": "1", "DENTAL_MCP_URL": "https://dental-mcp-abc.a.run.app/mcp",
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
	text := string(raw)
	for _, m := range regexp.MustCompile(`\$\{([A-Z_]+)\}`).FindAllStringSubmatch(text, -1) {
		if _, ok := gcpSample[m[1]]; !ok {
			t.Fatalf("%s uses unknown placeholder ${%s}", path, m[1])
		}
	}
	var m manifest
	if err := yaml.Unmarshal([]byte(os.Expand(text, func(k string) string { return gcpSample[k] })), &m); err != nil {
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
	m := renderManifest(t, "../deploy/gcp/govoice.service.yaml")
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
	if spec.TimeoutSeconds != 3600 || spec.ContainerConcurrency != 20 || c.Resources.Limits["cpu"] != "2" || c.Resources.Limits["memory"] != "2Gi" {
		t.Errorf("sizing: timeout=%d concurrency=%d limits=%v", spec.TimeoutSeconds, spec.ContainerConcurrency, c.Resources.Limits)
	}
	if spec.ServiceAccountName != "govoice-runtime@demo-proj.iam.gserviceaccount.com" {
		t.Errorf("service account = %q", spec.ServiceAccountName)
	}
	if strings.Join(c.Args, " ") != "-- /opt/govoice/bin/server -config /etc/govoice/cloudrun.yaml" || len(c.Command) != 1 || c.Command[0] != "/usr/bin/tini" {
		t.Errorf("command/args = %v %v", c.Command, c.Args)
	}
	if c.StartupProbe.HTTPGet.Path != "/health" || c.StartupProbe.HTTPGet.Port != 8080 || c.Ports[0].ContainerPort != 8080 {
		t.Errorf("probe/port: %+v %+v", c.StartupProbe, c.Ports)
	}
	want := map[string]string{
		"TOKEN_SECRET": "TOKEN_SECRET", "GOVOICE_MANAGEMENT_API_KEY": "GOVOICE_MANAGEMENT_API_KEY",
		"XAI_API_KEY": "XAI_API_KEY", "ELEVENLABS_API_KEY": "ELEVENLABS_API_KEY", "GOOGLE_API_KEY": "GOOGLE_API_KEY",
		"MCP_TOKEN_DENTAL": "DENTAL_MCP_TOKEN",
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
	if len(spec.Volumes) != 1 || spec.Volumes[0].Secret.SecretName != "GOOGLE_SA_KEY" || c.VolumeMounts[0].MountPath != "/secrets/google" {
		t.Errorf("the Google key must be a mounted secret file: %+v %+v", spec.Volumes, c.VolumeMounts)
	}
	if env["GOVOICE_DATABASE_DSN"] != "/tmp/govoice.db" || env["DENTAL_MCP_URL"] != "https://dental-mcp-abc.a.run.app/mcp" {
		t.Errorf("env = %v", env)
	}
}

func TestDentalMCPServiceManifest(t *testing.T) {
	m := renderManifest(t, "../deploy/gcp/dental-mcp.service.yaml")
	a, spec := m.Spec.Template.Metadata.Annotations, m.Spec.Template.Spec
	c := spec.Containers[0]
	if a["autoscaling.knative.dev/minScale"] != "0" || a["autoscaling.knative.dev/maxScale"] != "1" {
		t.Errorf("scaling: %v (in-memory data: at most one instance, scale to zero)", a)
	}
	if got := secretEnv(t, m); got["DENTAL_MCP_TOKEN"] != "DENTAL_MCP_TOKEN" || len(got) != 1 {
		t.Errorf("secrets = %v", got)
	}
	if c.Ports[0].ContainerPort != 8081 || c.StartupProbe.HTTPGet.Path != "/health" || c.StartupProbe.HTTPGet.Port != 8081 {
		t.Errorf("port/probe: %+v %+v", c.Ports, c.StartupProbe)
	}
	// The public MCP service must not run as the account that can read every provider key.
	if spec.ServiceAccountName != "dental-mcp-runtime@demo-proj.iam.gserviceaccount.com" {
		t.Errorf("service account = %q", spec.ServiceAccountName)
	}
}

func TestManifestSecretsAreTheOnesTheBootstrapCreates(t *testing.T) {
	boot, err := os.ReadFile("../deploy/gcp/bootstrap.sh")
	if err != nil {
		t.Fatalf("read bootstrap.sh: %v", err)
	}
	for _, name := range gcpSecretNames {
		if !strings.Contains(string(boot), name) {
			t.Errorf("bootstrap.sh does not create secret %s", name)
		}
	}
}
