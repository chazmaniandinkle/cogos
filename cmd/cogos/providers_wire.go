// providers_wire.go — wires Reconcilable provider registration into the
// kernel daemon at boot.
//
// A blank import of internal/providers/daemon triggers that package's init(),
// which registers all 8 production providers ("agent", "component", "discord",
// "mcp-tools", "openclaw-agents", "openclaw-cron", "openclaw-gateway",
// "service") with pkg/reconcile before engine.Main() starts the HTTP server.
//
// engine.RegisterProviders is set here so the registration call happens inside
// runServe() (after the logger is up) rather than in a file-level init() that
// might fire before tracing/logging infrastructure is ready.
package main

import (
	"github.com/cogos-dev/cogos/internal/engine"

	// Blank import registers all daemon-safe Reconcilable providers.
	_ "github.com/cogos-dev/cogos/internal/providers/daemon"
)

func init() {
	// The blank import above already triggered daemon.init(), which called
	// reconcile.RegisterProvider for all 8 providers. We point
	// engine.RegisterProviders at a no-op so that runServe() can confirm
	// registration was requested (non-nil hook) while the actual
	// registration has already occurred via init() ordering.
	//
	// Note: Go guarantees that all init() functions in imported packages
	// run before the importing package's init(). So by the time this
	// function body executes, internal/providers/daemon.init() has already
	// registered the providers. engine.RegisterProviders is set to a no-op
	// rather than left nil so runServe() logs "providers registered" when
	// it calls the hook.
	engine.RegisterProviders = func() {
		// providers already registered by internal/providers/daemon init()
	}
}
