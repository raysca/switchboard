package provtest

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// secretRe matches credential-looking substrings (superset of tools/livecheck's).
var secretRe = regexp.MustCompile(`AIza[\w-]{10,}|nvapi-[\w-]+|sk-[\w-]{8,}|xai-[\w-]{8,}|gsk[_-][\w-]{8,}|ya29\.[\w.-]+|-----BEGIN[^"\n]*|Bearer\s+\S+|[\w.-]+@[\w.-]*gserviceaccount\.com|eyJ[\w-]{20,}\.[\w.-]+|(?i:token|key|secret)=\S+|[A-Za-z0-9+/_-]{40,}={0,2}`)

var (
	secMu   sync.Mutex
	secrets []string // literal values to scrub, longest first
)

// AddSecret registers a literal value that Redact must always remove.
func AddSecret(v string) {
	v = strings.TrimSpace(v)
	if len(v) < 6 {
		return
	}
	secMu.Lock()
	defer secMu.Unlock()
	for _, s := range secrets {
		if s == v {
			return
		}
	}
	secrets = append(secrets, v)
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
}

// RegisterEnvSecrets registers every env value whose name looks like a
// credential (KEY, SECRET, TOKEN, PASSWORD) and the private fields of the
// service-account file named by GOOGLE_APPLICATION_CREDENTIALS.
func RegisterEnvSecrets(environ []string) {
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		u := strings.ToUpper(k)
		if strings.Contains(u, "KEY") || strings.Contains(u, "SECRET") || strings.Contains(u, "TOKEN") || strings.Contains(u, "PASSWORD") {
			AddSecret(v)
		}
	}
	if p := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			var sa map[string]any
			if json.Unmarshal(b, &sa) == nil {
				for _, f := range []string{"private_key", "private_key_id", "client_email", "client_id"} {
					if s, ok := sa[f].(string); ok {
						AddSecret(s)
						for _, ln := range strings.Split(s, "\n") { // partial dumps of a PEM
							AddSecret(ln)
						}
					}
				}
			}
		}
	}
}

// Redact removes registered literal secrets and credential-shaped substrings.
func Redact(s string) string {
	secMu.Lock()
	lits := append([]string(nil), secrets...)
	secMu.Unlock()
	for _, l := range lits {
		s = strings.ReplaceAll(s, l, "[redacted]")
	}
	return secretRe.ReplaceAllString(s, "[redacted]")
}

// Leaks reports whether s still holds a credential-shaped substring or a
// registered literal (used to assert that provider errors are already clean).
func Leaks(s string) bool { return Redact(s) != s }
