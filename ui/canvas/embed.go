// Package canvas embeds the CogOS canvas view and exposes it as a
// net/http handler.
//
// The canvas view is a single static HTML file with no build step. It provides
// an infinite-canvas spatial interface with draggable nodes, real-time chat,
// and CogDoc visualization. It is served at GET /canvas by the v3 kernel daemon.
package canvas

import (
	_ "embed"
	"net/http"
)

//go:embed canvas.html
var html []byte

// Bytes returns the raw canvas HTML.
func Bytes() []byte { return html }

// Handler returns an http.Handler that serves the canvas HTML at any path.
// Callers are responsible for mounting it at the desired route (typically GET /canvas).
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(html)
	})
}
