package adminui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

var site = fstest.MapFS{
	"index.html":        {Data: []byte(`<!doctype html><title>Switchboard</title><script type="module" src="/assets/app-abc.js"></script>`)},
	"assets/app-abc.js": {Data: []byte(`console.log(1)`)},
	"favicon.svg":       {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
}

func get(h http.Handler, method, target string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.Host = "voice.run.app"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestServesIndexAndAssets(t *testing.T) {
	h := Handler(site)
	if w := get(h, "GET", "/"); w.Code != 200 || !strings.Contains(w.Body.String(), "Switchboard") || w.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("/: %d %q cache=%q", w.Code, w.Body.String(), w.Header().Get("Cache-Control"))
	}
	w := get(h, "GET", "/assets/app-abc.js")
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "javascript") || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("asset: %d type=%q cache=%q", w.Code, w.Header().Get("Content-Type"), w.Header().Get("Cache-Control"))
	}
}

func TestClientRoutesFallBackToIndexButMissingFilesDoNot(t *testing.T) {
	h := Handler(site)
	for _, p := range []string{"/flows", "/flows/abc/edit", "/calls/42"} {
		if w := get(h, "GET", p); w.Code != 200 || !strings.Contains(w.Body.String(), "Switchboard") {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	for _, p := range []string{"/assets/missing.js", "/robots.txt", "/x.map"} {
		if w := get(h, "GET", p); w.Code != 404 {
			t.Errorf("%s must be a real 404 (a stale deploy must show), got %d", p, w.Code)
		}
	}
}

func TestOnlyReadsAreServed(t *testing.T) {
	if w := get(Handler(site), "POST", "/"); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /: %d", w.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	w := get(Handler(site), "GET", "/")
	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "script-src 'self'", "connect-src 'self' wss://voice.run.app", "frame-ancestors 'none'", "base-uri 'none'", "worker-src 'self' blob:"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %q: %s", want, csp)
		}
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("headers: %v", w.Header())
	}
	// A hostile Host header cannot inject policy.
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "evil.example; script-src *"
	rec := httptest.NewRecorder()
	Handler(site).ServeHTTP(rec, r)
	if strings.Contains(rec.Header().Get("Content-Security-Policy"), "script-src *") {
		t.Fatalf("CSP built from an unvalidated Host: %s", rec.Header().Get("Content-Security-Policy"))
	}
}

func TestAnUnbuiltUIsaysSo(t *testing.T) {
	empty := fstest.MapFS{".gitkeep": {}}
	if Built(empty) {
		t.Fatal("a directory without index.html is not a built UI")
	}
	w := get(Handler(empty), "GET", "/")
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "not built") {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
}
