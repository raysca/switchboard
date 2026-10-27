package server

import (
	"encoding/json"
	"os"
	"os/exec"
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
// cover verify, the tagged tests, Postgres and the image smoke. Linux only: the server ships as the
// container image, so there is no macOS build job.
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
	for name, want := range map[string]string{"test": "make verify", "postgres": "make test-postgres", "image-smoke": "deploy/smoke.sh"} {
		if !strings.Contains(runs(name), want) {
			t.Errorf("ci job %s must run %q", name, want)
		}
		if ci[name].ContinueOnError {
			t.Errorf("ci job %s must be blocking", name)
		}
	}
	if !strings.HasPrefix(ci["test"].RunsOn, "ubuntu") {
		t.Errorf("test runner = %q", ci["test"].RunsOn)
	}
	for name := range ci {
		if strings.Contains(ci[name].RunsOn, "macos") {
			t.Errorf("ci job %s runs on %q: the server ships as the Linux container image only", name, ci[name].RunsOn)
		}
	}
	if strings.Contains(runs("image-smoke"), "deploy/Dockerfile.dental-mcp") {
		t.Error("the image smoke must build the moved demo Dockerfiles under demos/, not the deleted deploy/ one")
	}
	for _, want := range []string{"demos/dental/deploy/cloudrun/Dockerfile.mcp", "demos/atlas/deploy/cloudrun/Dockerfile.mcp", "go build -o /tmp/seed ./demos/seed"} {
		if !strings.Contains(runs("image-smoke"), want) {
			t.Errorf("the image-smoke job must build the moved demo asset %q", want)
		}
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
	for _, needle := range []string{"AS ui", "npm ci", "npm run build", "server/adminui/static", "/etc/govoice/cloudrun.yaml",
		"/etc/govoice/cloudflare-demo.yaml", "/etc/govoice/cloudflare-demo-postgres.yaml"} {
		if !strings.Contains(string(body), needle) {
			t.Fatalf("Dockerfile missing %q", needle)
		}
	}
}

// The demo MCP images moved out of deploy/ into demos/<name>/deploy/{cloudrun,cloudflare}. They are
// small, non-root, pinned, static and build the moved demo binary; the bearer token is never baked in.
func TestDemoMCPImagesAreSmallAndNonRoot(t *testing.T) {
	for _, demo := range []struct{ name, token string }{{"dental", "DENTAL_MCP_TOKEN"}, {"atlas", "ATLAS_MCP_TOKEN"}} {
		for _, target := range []string{"cloudrun", "cloudflare"} {
			path := "../demos/" + demo.name + "/deploy/" + target + "/Dockerfile.mcp"
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			text := string(body)
			for _, needle := range []string{"CGO_ENABLED=0", "./demos/" + demo.name + "/cmd/" + demo.name + "-mcp", "USER 65532", "ENTRYPOINT"} {
				if !strings.Contains(text, needle) {
					t.Errorf("%s missing %q", path, needle)
				}
			}
			for _, line := range strings.Split(text, "\n") {
				if strings.HasPrefix(line, "FROM ") && !strings.Contains(line, "@sha256:") {
					t.Errorf("%s: base image must be pinned by digest: %q", path, line)
				}
			}
			if strings.Contains(text, demo.token+"=") {
				t.Errorf("%s: the token must never be baked into the image", path)
			}
		}
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
		for _, df := range []string{
			"../deploy/Dockerfile",
			"../demos/dental/deploy/cloudrun/Dockerfile.mcp", "../demos/dental/deploy/cloudflare/Dockerfile.mcp",
			"../demos/atlas/deploy/cloudrun/Dockerfile.mcp", "../demos/atlas/deploy/cloudflare/Dockerfile.mcp",
		} {
			if !strings.Contains(read(df), "COPY "+m+"/go.mod") {
				t.Errorf("%s does not COPY %s/go.mod before go work sync", df, m)
			}
		}
	}
}

// demoManifestSample is what deploy.sh substitutes into the demo Cloud Run manifests.
func demoManifestSample() map[string]string {
	return map[string]string{
		"REGION": "us-central1", "PROJECT_ID": "demo-proj",
		"IMAGE":           "us-central1-docker.pkg.dev/demo-proj/switchboard/dental-mcp:abc123",
		"DEMO_RUNTIME_SA": "demo-mcp-runtime@demo-proj.iam.gserviceaccount.com",
	}
}

// The per-demo Cloud Run assets (demos/<name>/deploy/cloudrun) deploy an in-memory fixture with one
// instance at most, a public HTTPS endpoint and a bearer token from Secret Manager. Atlas binds every
// interface (the binary defaults to loopback).
func TestDemoCloudRunAssets(t *testing.T) {
	for _, demo := range []struct{ name, token string }{{"dental", "DENTAL_MCP_TOKEN"}, {"atlas", "ATLAS_MCP_TOKEN"}} {
		dir := "../demos/" + demo.name + "/deploy/cloudrun/"
		raw, err := os.ReadFile(dir + "cloudrun.yaml")
		if err != nil {
			t.Fatalf("%s: %v", dir+"cloudrun.yaml", err)
		}
		if !strings.Contains(string(raw), "name: "+demo.name+"-mcp") {
			t.Errorf("%s must deploy the service %s-mcp", dir+"cloudrun.yaml", demo.name)
		}
		m := parseManifest(t, dir+"cloudrun.yaml", string(raw), demoManifestSample())
		spec, c := m.Spec.Template.Spec, m.Spec.Template.Spec.Containers[0]
		a := m.Spec.Template.Metadata.Annotations
		if a["autoscaling.knative.dev/minScale"] != "0" || a["autoscaling.knative.dev/maxScale"] != "1" {
			t.Errorf("%s: in-memory data needs at most one instance and scale to zero: %v", demo.name, a)
		}
		if spec.ServiceAccountName != "demo-mcp-runtime@demo-proj.iam.gserviceaccount.com" {
			t.Errorf("%s: the demo service must run on the dedicated demo account: %q", demo.name, spec.ServiceAccountName)
		}
		if len(c.Ports) != 1 || c.Ports[0].ContainerPort != 8081 || c.StartupProbe.HTTPGet.Path != "/health" || c.StartupProbe.HTTPGet.Port != 8081 {
			t.Errorf("%s: port/probe: %+v %+v", demo.name, c.Ports, c.StartupProbe)
		}
		secrets := secretEnv(t, m)
		if len(secrets) != 1 || secrets[demo.token] != demo.token {
			t.Errorf("%s: the demo token must be the only Secret Manager reference: %v", demo.name, secrets)
		}
		env := map[string]string{}
		for _, e := range c.Env {
			env[e.Name] = e.Value
		}
		if demo.name == "atlas" {
			// The binary binds 127.0.0.1 unless told otherwise, so a container would never be healthy.
			if env["PORT"] != "8081" || env["ATLAS_MCP_ADDR"] != "0.0.0.0:8081" {
				t.Errorf("atlas must bind every interface on the container port: PORT=%q ATLAS_MCP_ADDR=%q", env["PORT"], env["ATLAS_MCP_ADDR"])
			}
		}

		script := dir + "deploy.sh"
		body, err := os.ReadFile(script)
		if err != nil {
			t.Fatalf("%s: %v", script, err)
		}
		if out, err := exec.Command("bash", "-n", script).CombinedOutput(); err != nil {
			t.Fatalf("%s is not valid bash: %v\n%s", script, err, out)
		}
		text := string(body)
		for _, needle := range []string{"--platform linux/amd64", "gcloud run services replace", "roles/run.invoker", "/health", "MCP_URL="} {
			if !strings.Contains(text, needle) {
				t.Errorf("%s missing %q", script, needle)
			}
		}
		for _, banned := range []string{"gcloud secrets versions access", "DENTAL_MCP_TOKEN=", "ATLAS_MCP_TOKEN="} {
			if strings.Contains(text, banned) {
				t.Errorf("%s must not read or embed the demo token (%q): it is mounted from Secret Manager", script, banned)
			}
		}
		if demo.name == "atlas" && !strings.Contains(text, "HTTP_BASE=") {
			t.Errorf("%s must print the HTTP-tool base URL for the seed step", script)
		}
	}
}

// The per-demo Cloudflare assets (demos/<name>/deploy/cloudflare): a Worker + container that proxies
// only /mcp, /api/* and /health to the demo MCP process, whose bearer token is a Wrangler secret.
func TestDemoCloudflareAssets(t *testing.T) {
	type wrangler struct {
		Main       string `json:"main"`
		Containers []struct {
			ClassName string `json:"class_name"`
			Images    struct {
				Base struct {
					Dockerfile   string `json:"dockerfile"`
					BuildContext string `json:"build_context"`
				} `json:"base"`
			} `json:"images"`
		} `json:"containers"`
		DurableObjects struct {
			Bindings []struct {
				Name      string `json:"name"`
				ClassName string `json:"class_name"`
			} `json:"bindings"`
		} `json:"durable_objects"`
	}
	for _, demo := range []struct{ name, token string }{{"dental", "DENTAL_MCP_TOKEN"}, {"atlas", "ATLAS_MCP_TOKEN"}} {
		dir := "../demos/" + demo.name + "/deploy/cloudflare/"
		raw, err := os.ReadFile(dir + "wrangler.jsonc")
		if err != nil {
			t.Fatalf("%s: %v", dir+"wrangler.jsonc", err)
		}
		var w wrangler
		if err := json.Unmarshal(raw, &w); err != nil {
			t.Fatalf("%s is not valid JSON: %v", dir+"wrangler.jsonc", err)
		}
		if w.Main != "src/index.js" || len(w.Containers) != 1 {
			t.Fatalf("%s: main=%q containers=%d", demo.name, w.Main, len(w.Containers))
		}
		img := w.Containers[0].Images.Base
		if img.Dockerfile != "Dockerfile.mcp" || img.BuildContext != "../../../.." {
			t.Errorf("%s: the container must build Dockerfile.mcp from the repository root, got %q %q", demo.name, img.Dockerfile, img.BuildContext)
		}
		if len(w.DurableObjects.Bindings) != 1 || w.DurableObjects.Bindings[0].Name != "MCP" ||
			w.DurableObjects.Bindings[0].ClassName != w.Containers[0].ClassName {
			t.Errorf("%s: the Worker must reach its container through the MCP Durable Object binding: %+v %+v",
				demo.name, w.Containers[0], w.DurableObjects.Bindings)
		}

		worker, err := os.ReadFile(dir + "src/index.js")
		if err != nil {
			t.Fatalf("%s: %v", dir+"src/index.js", err)
		}
		js := string(worker)
		for _, needle := range []string{`"/mcp"`, `"/health"`, `"/api/"`, "getTcpPort", demo.token} {
			if !strings.Contains(js, needle) {
				t.Errorf("%s/src/index.js missing %q", demo.name, needle)
			}
		}
		if strings.Contains(js, "TOKEN =") || strings.Contains(js, demo.token+" =") {
			t.Errorf("%s/src/index.js must read the token from the Wrangler secret, not embed it", demo.name)
		}
		if !strings.Contains(js, `instance: "standard-1"`) {
			t.Errorf("%s/src/index.js must start a supported Cloudflare container instance (standard-1)", demo.name)
		}
		if demo.name == "atlas" {
			if !strings.Contains(js, "ATLAS_MCP_ADDR") || !strings.Contains(js, "0.0.0.0:${PORT}") {
				t.Errorf("atlas Worker must set ATLAS_MCP_ADDR=0.0.0.0:$PORT for its container")
			}
		}
	}
}

// The server Worker gained the persistent secrets key and the optional external Postgres profile:
// the SQLite file is ephemeral, so a Cloudflare deployment that runs demos needs both.
func TestCloudflareServerDeploySupportsPersistentDemos(t *testing.T) {
	worker, err := os.ReadFile("../deploy/cloudflare/src/index.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(worker)
	for _, needle := range []string{"SWITCHBOARD_SECRETS_KEY", "GOVOICE_DATABASE_DSN", "cloudflare-demo-postgres.yaml", "postgres"} {
		if !strings.Contains(js, needle) {
			t.Errorf("deploy/cloudflare/src/index.js must support %q", needle)
		}
	}
	// The external Postgres profile is shipped in the image and differs from the SQLite one only in storage.
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
	sqlite, postgres := load("../config/cloudflare-demo.yaml"), load("../deploy/cloudflare/cloudflare-demo-postgres.yaml")
	if postgres["profile"] != "production" {
		t.Fatalf("the Cloudflare Postgres profile must be the production profile: %v", postgres["profile"])
	}
	if st, _ := postgres["storage"].(map[string]any); st["dialect"] != "postgres" {
		t.Fatalf("the Cloudflare Postgres profile must select postgres: %v", postgres["storage"])
	}
	if st, _ := sqlite["storage"].(map[string]any); st["dialect"] != "sqlite" {
		t.Fatalf("the shipped Cloudflare profile must stay the ephemeral SQLite one: %v", sqlite["storage"])
	}
	delete(sqlite, "storage")
	delete(postgres, "storage")
	if !reflect.DeepEqual(sqlite, postgres) {
		t.Fatalf("deploy/cloudflare/cloudflare-demo-postgres.yaml drifted from config/cloudflare-demo.yaml beyond storage:\n%v\n%v", sqlite, postgres)
	}
	df, err := os.ReadFile("../deploy/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(df), "cloudflare-demo-postgres.yaml") {
		t.Error("the server image must ship the Cloudflare Postgres profile")
	}
}

// The two-pipeline deployment model is documented: clean-room server, optional demo deployment, seed
// with host + management key, the secrets-store prerequisite, the empty SQLite start and teardown.
func TestDeploymentDocsDescribeTheTwoPipelines(t *testing.T) {
	doc, err := os.ReadFile("../docs/deployment.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ToLower(string(doc))
	for _, needle := range []string{
		"clean-room", "two pipelines", ".github/workflows/deploy-demo.yml", "demos/seed",
		"switchboard_secrets_key", "--server", "--key-env",
		"starts empty", "teardown", "external postgres",
	} {
		if !strings.Contains(text, needle) {
			t.Errorf("docs/deployment.md must name %q (two-pipeline model, seed, persistence, teardown)", needle)
		}
	}
	cf, err := os.ReadFile("../docs/deployment-cloudflare-plan.md")
	if err != nil {
		t.Fatal(err)
	}
	cft := strings.ToLower(string(cf))
	for _, needle := range []string{"demos/dental/deploy/cloudflare", "demos/atlas/deploy/cloudflare", "external postgres", "switchboard_secrets_key", "deploy-demo.yml"} {
		if !strings.Contains(cft, needle) {
			t.Errorf("docs/deployment-cloudflare-plan.md must name %q (Cloudflare demo is deployable; demos need Postgres)", needle)
		}
	}
	if strings.Contains(cft, "Deploying the Smile Dental MCP or porting it to TypeScript/Hono.") {
		t.Error("the Cloudflare plan must replace the old \"MCP out of scope\" section: the demo MCP is deployable now")
	}
}
