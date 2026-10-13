// Package adminui serves the admin UI that is built into the binary. The build output goes in
// ./static (make ui-embed, or the Dockerfile's UI stage); a plain `go build` embeds only the
// placeholder, and the handler then says the UI is not built.
package adminui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
)

//go:embed all:static
var embedded embed.FS

// Embedded is the build output directory.
func Embedded() fs.FS {
	sub, err := fs.Sub(embedded, "static")
	if err != nil {
		panic(err) // "static" is embedded above; a bad path is a build bug
	}
	return sub
}

// Built reports whether fsys holds a built UI.
func Built(fsys fs.FS) bool {
	_, err := fs.Stat(fsys, "index.html")
	return err == nil
}

// hostRe is what a Host header may be to appear in the policy: a DNS name or IP with an optional port.
var hostRe = regexp.MustCompile(`^[A-Za-z0-9.\-\[\]:]+$`)

func csp(host string) string {
	connect := "'self'"
	if hostRe.MatchString(host) {
		connect += " wss://" + host + " ws://" + host // the voice socket; ws: only matters for local rehearsal
	}
	return "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
		"media-src 'self' blob:; font-src 'self'; connect-src " + connect + "; worker-src 'self' blob:; " +
		"frame-ancestors 'none'; base-uri 'none'; form-action 'self'"
}

// reserved are the server's own first path segments; the UI never answers them.
var reserved = map[string]bool{"api": true, "ws": true, "health": true, "metrics": true, "ready": true, "debug": true}

func isFile(fsys fs.FS, name string) bool {
	info, err := fs.Stat(fsys, name)
	return err == nil && !info.IsDir()
}

// Handler serves fsys: files as they are, an unknown path without a file extension as index.html
// (client-side routes), and an unknown path with an extension as 404 (a stale deploy must show).
func Handler(fsys fs.FS) http.Handler {
	files := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		first, _, _ := strings.Cut(name, "/")
		if reserved[first] || (first == "assets" && !isFile(fsys, name)) { // server paths and stale hashed assets are real 404s
			http.NotFound(w, r)
			return
		}
		if !Built(fsys) {
			http.Error(w, "the admin UI is not built into this binary (make ui-embed)", http.StatusNotFound)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", csp(r.Host))
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if name == "" || name == "index.html" {
			h.Set("Cache-Control", "no-cache")
			r.URL.Path = "/"
			files.ServeHTTP(w, r)
			return
		}
		if isFile(fsys, name) {
			if strings.HasPrefix(name, "assets/") { // content-hashed by the build
				h.Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				h.Set("Cache-Control", "no-cache")
			}
			files.ServeHTTP(w, r)
			return
		}
		if path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}
		h.Set("Cache-Control", "no-cache")
		r.URL.Path = "/"
		files.ServeHTTP(w, r)
	})
}
