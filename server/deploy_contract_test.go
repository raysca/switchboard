package server

import (
	"os"
	"regexp"
	"strings"
	"testing"
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
	body, err := os.ReadFile("../deploy/smoke.sh")
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
