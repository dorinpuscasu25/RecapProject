// Package web serves the developer console: a dependency-free single-page app for exercising the
// API in a browser before the mobile apps exist. It calls the same public endpoints a mobile app
// does and shows every request and response, so it doubles as living documentation.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed console
var files embed.FS

// Handler serves the console under prefix (for example "/console/"). The console only loads its own
// static files and talks to the API on the same origin.
func Handler(prefix string) http.Handler {
	sub, _ := fs.Sub(files, "console")
	fsrv := http.StripPrefix(prefix, http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; media-src 'self' http: https:; "+
			"connect-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if strings.HasSuffix(r.URL.Path, "/") || strings.HasSuffix(r.URL.Path, ".html") {
			h.Set("Cache-Control", "no-store")
		} else {
			h.Set("Cache-Control", "no-cache")
		}
		fsrv.ServeHTTP(w, r)
	})
}
