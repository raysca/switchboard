package server

import (
	"os"
	"reflect"
	"regexp"
	"slices"
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

func TestDeploymentDocNamesContainerAsDistribution(t *testing.T) {
	body, err := os.ReadFile("../docs/deployment.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "The container image is the V1 distribution.") {
		t.Fatal("deployment guide must name the container as the V1 distribution")
	}
	if !strings.Contains(text, "The systemd unit is a reference file and is not required to run V1.") {
		t.Fatal("deployment guide must say the systemd unit is not required")
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
	if dc, ok := ci["deadcode"]; !ok || !strings.Contains(runs("deadcode"), "make deadcode") || !dc.ContinueOnError {
		t.Error("ci job deadcode must run make deadcode and stay advisory (continue-on-error)")
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

func TestImageShipsTheCloudRunConfigAndBuildsTheUI(t *testing.T) {
	body, err := os.ReadFile("../deploy/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"AS ui", "npm ci", "npm run build", "server/adminui/static", "/etc/govoice/cloudrun.yaml"} {
		if !strings.Contains(string(body), needle) {
			t.Fatalf("Dockerfile missing %q", needle)
		}
	}
}

func TestDentalMCPImageIsSmallAndNonRoot(t *testing.T) {
	body, err := os.ReadFile("../deploy/Dockerfile.dental-mcp")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, needle := range []string{"CGO_ENABLED=0", "./cmd/dental-mcp", "USER 65532", "ENTRYPOINT"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("Dockerfile.dental-mcp missing %q", needle)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "FROM ") && !strings.Contains(line, "@sha256:") {
			t.Fatalf("base image must be pinned by digest: %q", line)
		}
	}
	if strings.Contains(text, "DENTAL_MCP_TOKEN=") {
		t.Fatal("the token must never be baked into the image")
	}
}

func TestRehearsalConfigOnlyDiffersWhereItMust(t *testing.T) {
	load := func(path string) map[string]any {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := yaml.Unmarshal(raw, &m); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		return m
	}
	real, rehearsal := load("../config/cloudrun.yaml"), load("../deploy/rehearsal/server.yaml")
	if rehearsal["profile"] != "development" {
		t.Fatalf("the rehearsal runs the development profile (loopback MCP), got %v", rehearsal["profile"])
	}
	mcp, _ := rehearsal["mcp"].(map[string]any)
	if mcp["allow_local_http"] != true {
		t.Fatalf("mcp = %v", mcp)
	}
	for _, k := range []string{"profile", "tls", "mcp"} { // the three declared differences
		delete(real, k)
		delete(rehearsal, k)
	}
	if !reflect.DeepEqual(real, rehearsal) {
		t.Fatalf("deploy/rehearsal/server.yaml drifted from config/cloudrun.yaml beyond profile, tls and mcp:\nreal:      %v\nrehearsal: %v", real, rehearsal)
	}
	// And it loads as a valid development config.
	t.Setenv("GOVOICE_DATABASE_DSN", t.TempDir()+"/g.db")
	t.Setenv("GOVOICE_MANAGEMENT_API_KEY", strings.Repeat("m", 40))
	if _, err := LoadConfig("../deploy/rehearsal/server.yaml"); err != nil {
		t.Fatalf("rehearsal config does not load: %v", err)
	}
}

// A module added to go.work must be copied into both images before `go work sync`, or the image
// does not build; and it must be in the Makefile's MODULES, or `make verify` never tests it.
func TestEveryWorkspaceModuleIsInTheImagesAndTheGate(t *testing.T) {
	work, err := os.ReadFile("../go.work")
	if err != nil {
		t.Fatal(err)
	}
	var modules []string
	for _, m := range regexp.MustCompile(`(?m)^\s+\./(\S+)\s*$`).FindAllStringSubmatch(string(work), -1) {
		modules = append(modules, m[1])
	}
	if len(modules) < 5 {
		t.Fatalf("parsed go.work modules %v", modules)
	}
	read := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	mk := read("../Makefile")
	line := regexp.MustCompile(`(?m)^MODULES\s*:=\s*(.*)$`).FindStringSubmatch(mk)
	if line == nil {
		t.Fatal("Makefile has no MODULES line")
	}
	gate := strings.Fields(line[1])
	for _, m := range modules {
		if !slices.Contains(gate, m) {
			t.Errorf("go.work module %s is missing from Makefile MODULES (make verify would not test it)", m)
		}
		for _, df := range []string{"../deploy/Dockerfile", "../deploy/Dockerfile.dental-mcp"} {
			if !strings.Contains(read(df), "COPY "+m+"/go.mod") {
				t.Errorf("%s does not COPY %s/go.mod before go work sync", df, m)
			}
		}
	}
}
