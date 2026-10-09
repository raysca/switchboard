package server

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestNginxCertificatesAreMounted(t *testing.T) {
	nginx, err := os.ReadFile("../deploy/nginx.conf")
	if err != nil {
		t.Fatal(err)
	}
	compose, err := os.ReadFile("../deploy/docker-compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(compose), "./certs:/etc/ssl/certs:ro") {
		t.Fatal("compose must mount ./certs onto /etc/ssl/certs read-only")
	}
	re := regexp.MustCompile(`ssl_certificate(?:_key)?\s+(\S+);`)
	for _, match := range re.FindAllStringSubmatch(string(nginx), -1) {
		path := match[1]
		if !strings.HasPrefix(path, "/etc/ssl/certs/") {
			t.Fatalf("certificate path %s is outside the Compose mount /etc/ssl/certs", path)
		}
	}
}

func TestRunbookNamesContainerAsDistribution(t *testing.T) {
	body, err := os.ReadFile("../docs/operations_runbook.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "The container image is the V1 distribution.") {
		t.Fatal("runbook must name the container as the V1 distribution")
	}
	if !strings.Contains(text, "The systemd unit is a reference file and is not required to run V1.") {
		t.Fatal("runbook must say the systemd unit is not required")
	}
}

func TestSmokeScriptChecksProxyHealth(t *testing.T) {
	body, err := os.ReadFile("../deploy/smoke-google.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, needle := range []string{
		"docker compose -f deploy/docker-compose.yaml",
		"https://127.0.0.1/health",
		"docker kill -s TERM",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("smoke script missing %q", needle)
		}
	}
}

func TestDockerfileProductionHardening(t *testing.T) {
	dockerfile, err := os.ReadFile("../deploy/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	content := string(dockerfile)

	// Check non-root user setup
	if !strings.Contains(content, "USER govoice:govoice") {
		t.Fatal("Dockerfile must run as non-root USER govoice:govoice")
	}

	// Check tini init system
	if !strings.Contains(content, `ENTRYPOINT ["/usr/bin/tini", "--"]`) {
		t.Fatal("Dockerfile must use tini as entrypoint")
	}

	// Check compose capability dropping and read-only filesystem
	compose, err := os.ReadFile("../deploy/docker-compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	composeStr := string(compose)
	if !strings.Contains(composeStr, "read_only: true") {
		t.Fatal("docker-compose must enforce read_only: true")
	}
	if !strings.Contains(composeStr, "cap_drop:\n      - ALL") {
		t.Fatal("docker-compose must drop ALL capabilities")
	}
	if !strings.Contains(composeStr, "no-new-privileges:true") {
		t.Fatal("docker-compose must set no-new-privileges:true")
	}
}

// CI (M4-01): every workflow and the alert rules parse, and ci.yml gates on the Makefile targets that
// cover verify, the tagged tests, Postgres and the image smoke on Linux and macOS.
func TestCIWorkflowCoversTheSafetyNet(t *testing.T) {
	type step struct {
		Run  string `yaml:"run"`
		Uses string `yaml:"uses"`
	}
	type job struct {
		RunsOn          string `yaml:"runs-on"`
		ContinueOnError bool   `yaml:"continue-on-error"`
		Steps           []step `yaml:"steps"`
	}
	load := func(p string) map[string]job {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var wf struct {
			Name string         `yaml:"name"`
			Jobs map[string]job `yaml:"jobs"`
		}
		if err := yaml.Unmarshal(b, &wf); err != nil {
			t.Fatalf("%s does not parse: %v", p, err)
		}
		if wf.Name == "" || len(wf.Jobs) == 0 {
			t.Fatalf("%s: missing name or jobs", p)
		}
		return wf.Jobs
	}
	ci := load("../.github/workflows/ci.yml")
	runs := func(name string) string {
		var all []string
		for _, s := range ci[name].Steps {
			all = append(all, s.Run)
		}
		return strings.Join(all, "\n")
	}
	for name, want := range map[string]string{"linux": "make verify", "macos": "make verify", "postgres": "make test-postgres", "image-smoke": "deploy/smoke.sh"} {
		if !strings.Contains(runs(name), want) {
			t.Errorf("ci job %s must run %q", name, want)
		}
		if ci[name].ContinueOnError {
			t.Errorf("ci job %s must be blocking", name)
		}
	}
	if !strings.HasPrefix(ci["linux"].RunsOn, "ubuntu") || !strings.HasPrefix(ci["macos"].RunsOn, "macos") {
		t.Errorf("linux/macos runners = %q/%q", ci["linux"].RunsOn, ci["macos"].RunsOn)
	}
	if perf := load("../.github/workflows/perf-gate.yml"); !perf["perf-gate"].ContinueOnError {
		t.Error("perf-gate must stay advisory until a runner-class baseline is committed")
	}
}

// The probe listener (M4-02) must be wired consistently: production.yaml binds it, compose publishes
// it on loopback only, nginx's admin vhost proxies to it, and the management API stays on loopback.
func TestProbeListenerWiring(t *testing.T) {
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	cfg, compose, nginx := read("../config/production.yaml"), read("../deploy/docker-compose.yaml"), read("../deploy/nginx.conf")
	m := regexp.MustCompile(`(?m)^probe_listen_addr:\s*"[^"]*:(\d+)"`).FindStringSubmatch(cfg)
	if m == nil {
		t.Fatal("production.yaml must set probe_listen_addr")
	}
	port := m[1]
	if !strings.Contains(compose, `"127.0.0.1:`+port+":"+port+`"`) {
		t.Fatalf("compose must publish probe port %s on 127.0.0.1 only", port)
	}
	if regexp.MustCompile(`"(0\.0\.0\.0:)?\d+:9090"`).MatchString(compose) {
		t.Fatal("compose must not publish the management/admin port 9090")
	}
	if !strings.Contains(nginx, "govoice:"+port) || strings.Contains(nginx, "govoice:9090") {
		t.Fatalf("nginx admin vhost must proxy to the probe port %s, never to the admin listener", port)
	}
	if !regexp.MustCompile(`(?m)^admin_listen_addr:\s*"127\.0\.0\.1:`).MatchString(cfg) {
		t.Fatal("production admin_listen_addr must stay on loopback")
	}
}

// The shipped server must include xAI speech (launch scope) and the keyless smoke must probe from outside.
func TestProductionImageBuildsWithXAISpeechAndKeylessSmoke(t *testing.T) {
	df, _ := os.ReadFile("../deploy/Dockerfile")
	if !strings.Contains(string(df), "-tags xaispeech") {
		t.Fatal("Dockerfile must build the server with -tags xaispeech")
	}
	mk, _ := os.ReadFile("../Makefile")
	if !strings.Contains(string(mk), "GOTAGS ?= xaispeech") {
		t.Fatal("Makefile must default GOTAGS to xaispeech")
	}
	sm, err := os.ReadFile("../deploy/smoke.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"/ready", "/metrics", "-p 127.0.0.1:0:9092", "docker kill -s TERM"} {
		if !strings.Contains(string(sm), needle) {
			t.Fatalf("smoke.sh missing %q", needle)
		}
	}
	if strings.Contains(string(sm), "SMOKE_GOOGLE_SECRET_FILE") {
		t.Fatal("deploy/smoke.sh must stay keyless")
	}
}
