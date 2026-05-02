// Package dashboard embeds the CogOS web dashboard and exposes it as a
// net/http handler.
//
// The dashboard is a single static HTML file with no build step. It is served
// at GET / by the v3 kernel daemon.
package dashboard

import (
	_ "embed"
	"net/http"
)

//go:embed dashboard.html
var html []byte

// Bytes returns the raw dashboard HTML.
func Bytes() []byte { return html }

// Handler returns an http.Handler that serves the dashboard HTML at any path.
// Callers are responsible for mounting it at the desired route (typically GET /).
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(html)
	})
}
