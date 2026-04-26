// providers_wire.go — wires Reconcilable provider registration and workspace
// context into the kernel daemon at boot.
//
// A named import of internal/providers/daemon triggers that package's init(),
// which registers production providers ("agent", "component", "discord",
// "mcp-tools", "openclaw-agents", "openclaw-cron", "openclaw-gateway",
// "service") with pkg/reconcile before engine.Main() starts the HTTP server.
//
// engine.RegisterProviders is set here so the registration call happens inside
// runServe() (after the logger is up) rather than in a file-level init() that
// might fire before tracing/logging infrastructure is ready.
//
// engine.SetProvidersWorkspace is set here so that after LoadConfig resolves
// cfg.WorkspaceRoot, the daemon-side providers receive the workspace path and
// can perform real filesystem Health() checks rather than reporting
// "workspace not yet configured".
package main

import (
	"github.com/cogos-dev/cogos/internal/engine"
	"github.com/cogos-dev/cogos/internal/providers/component"
	"github.com/cogos-dev/cogos/internal/providers/daemon"
)

func init() {
	// The named import of internal/providers/daemon above already triggered
	// daemon.init() (and component.init() via daemon's blank import), which
	// called reconcile.RegisterProvider for all production providers.
	// engine.RegisterProviders is set to a no-op rather than left nil so
	// runServe() logs "providers registered" when it calls the hook.
	engine.RegisterProviders = func() {
		// providers already registered by internal/providers/daemon init()
	}

	// Wire workspace context into daemon-side providers. runServe() calls this
	// after LoadConfig resolves cfg.WorkspaceRoot. Until then, workspaceRoot
	// is "" and Health() returns "workspace not yet configured" — acceptable
	// because no Health() is called until the autonomic ticker or foveated
	// handler fires.
	engine.SetProvidersWorkspace = func(workspaceRoot string) {
		daemon.SetWorkspaceRoot(workspaceRoot)
		component.SetWorkspaceRoot(workspaceRoot)
	}
}
