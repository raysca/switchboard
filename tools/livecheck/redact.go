package main

import (
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
)

// secretRe matches credential-looking text by shape. Literal values (env secrets, the
// service account) are handled by the redactor below, which catches what no pattern can.
var secretRe = regexp.MustCompile(`AIza[\w-]{10,}|nvapi-[\w-]+|ya29\.[\w.-]+|\bsk-[\w-]{10,}|\bxai-[\w-]{10,}|\bgsk[_-][\w-]{10,}|-----BEGIN[^"\n]*|Bearer\s+\S+|[\w.-]+@[\w.-]*gserviceaccount\.com|eyJ[\w-]{20,}\.[\w.-]+|(?i:token|key|secret|password)=\S+`)

// longTokenRe finds opaque 40+ character runs; only those mixing letters and digits are
// treated as secrets (so table rules, paths and prose survive; 36-character UUIDs never match).
var longTokenRe = regexp.MustCompile(`[A-Za-z0-9_=-]{40,}`)

func mixedAlnum(s string) bool {
	var l, d bool
	for _, c := range s {
		l = l || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
		d = d || (c >= '0' && c <= '9')
	}
	return l && d
}

// secretEnvRe names the environment variables whose values are secrets.
var secretEnvRe = regexp.MustCompile(`(?i)(key|secret|token|password|credential)`)

type redactor struct {
	mu       sync.RWMutex
	literals []string
}

// theRedactor is shared by every output path (stdout, stderr, files, errors).
var theRedactor = &redactor{}

// add registers literal secrets (ignored when short enough to be common text).
func (r *redactor) add(vals ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range vals {
		if v = strings.TrimSpace(v); len(v) >= 8 {
			r.literals = append(r.literals, v)
		}
	}
}

// addEnv registers the value of every secret-named variable in environ ("K=V" entries), and
// the secret-bearing fields of a Google service-account file named by GOOGLE_APPLICATION_CREDENTIALS.
func (r *redactor) addEnv(environ []string) {
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || v == "" || !secretEnvRe.MatchString(k) {
			continue
		}
		if k == "GOOGLE_APPLICATION_CREDENTIALS" { // a path: the file's secrets matter
			if b, err := os.ReadFile(v); err == nil {
				var sa map[string]any
				if json.Unmarshal(b, &sa) == nil {
					for _, f := range []string{"private_key", "private_key_id", "client_email", "client_id"} {
						if s, _ := sa[f].(string); s != "" {
							r.add(s)
						}
					}
				}
			}
			continue
		}
		r.add(v)
	}
}

func (r *redactor) String(s string) string {
	r.mu.RLock()
	lits := r.literals
	r.mu.RUnlock()
	for _, l := range lits {
		s = strings.ReplaceAll(s, l, "[redacted]")
	}
	s = secretRe.ReplaceAllString(s, "[redacted]")
	return longTokenRe.ReplaceAllStringFunc(s, func(m string) string {
		if mixedAlnum(m) {
			return "[redacted]"
		}
		return m
	})
}

// redact is err's text with credentials removed.
func redact(err error) string { return theRedactor.String(err.Error()) }

// redactWriter filters everything written through it, line by line.
type redactWriter struct {
	w   io.Writer
	r   *redactor
	mu  sync.Mutex
	buf []byte
}

func (rw *redactWriter) Write(p []byte) (int, error) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.buf = append(rw.buf, p...)
	for {
		i := strings.IndexByte(string(rw.buf), '\n')
		if i < 0 {
			break
		}
		line := rw.r.String(string(rw.buf[:i+1]))
		rw.buf = rw.buf[i+1:]
		if _, err := io.WriteString(rw.w, line); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Flush writes a trailing partial line.
func (rw *redactWriter) Flush() {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if len(rw.buf) > 0 {
		_, _ = io.WriteString(rw.w, rw.r.String(string(rw.buf)))
		rw.buf = nil
	}
}

var (
	stdout = &redactWriter{w: os.Stdout, r: theRedactor}
	stderr = &redactWriter{w: os.Stderr, r: theRedactor}
)
