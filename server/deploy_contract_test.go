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
