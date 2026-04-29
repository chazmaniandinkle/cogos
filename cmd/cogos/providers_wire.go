// providers_wire.go — wires Reconcilable provider registration, workspace
// context, and MCP tool extensions into the kernel daemon at boot.
//
// A named import of internal/providers/daemon triggers that package's init(),
// which registers all 9 production providers ("agent", "component", "discord",
// "eval", "mcp-tools", "openclaw-agents", "openclaw-cron", "openclaw-gateway",
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
//
// engine.RegisterMCPExtensions wires the four eval MCP tools
// (cog_run_experiment, cog_list_experiments, cog_get_experiment_status,
// cog_pin_baseline) onto the kernel's MCP server so they are accessible
// from both the cog CLI binary and the daemon.
package main

import (
	"github.com/cogos-dev/cogos/internal/engine"
	"github.com/cogos-dev/cogos/internal/eval"
	"github.com/cogos-dev/cogos/internal/providers/component"
	"github.com/cogos-dev/cogos/internal/providers/daemon"
)

// daemonEvalProvider is the daemon-side EvalProvider instance passed to the
// eval MCP tools. The daemon does not run plan/apply; it only exposes the
// four read/trigger tools whose state effects (trigger files, baseline pins)
// are read by the CLI's reconcile loop.
var daemonEvalProvider = eval.New(nil, nil)

func init() {
	// The named import of internal/providers/daemon above already triggered
	// daemon.init() (and component.init() via daemon's blank import), which
	// called reconcile.RegisterProvider for all 9 providers. engine.RegisterProviders
	// is set to a no-op rather than left nil so runServe() logs "providers
	// registered" when it calls the hook.
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
		// Prime the daemon EvalProvider root so its MCP tools can resolve
		// the workspace-relative state files (eval-dispatch-triggers.json,
		// eval-baselines.json). LoadConfig is idempotent — safe to call here.
		if workspaceRoot != "" {
			_, _ = daemonEvalProvider.LoadConfig(workspaceRoot)
		}
	}

	// Wire the four eval MCP tools onto the kernel's MCP server. The daemon
	// EvalProvider is minimal (no dispatcher/emitter) — the tools read/write
	// the sidecar state files that the CLI's reconcile loop consumes.
	// The root path is set lazily: cog_run_experiment calls LoadConfig, which
	// sets e.root before writeDispatchTrigger uses it. For a fully configured
	// workspace, the tools work end-to-end; for a fresh smoke workspace they
	// return a "not configured" error that still exercises the code path.
	engine.RegisterMCPExtensions = func(srv *engine.MCPServer) {
		eval.RegisterEvalTools(srv.Server(), daemonEvalProvider)
	}
}
