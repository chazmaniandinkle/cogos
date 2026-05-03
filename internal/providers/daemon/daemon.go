// Package daemon registers all Reconcilable providers that the kernel daemon
// (cmd/cogos) needs to surface in proprioception health blocks.
//
// Background: The 8 production providers (agent, component, discord,
// mcp-tools, openclaw-agents, openclaw-cron, openclaw-gateway, service) were
// originally defined in the workspace-root package main of the cog CLI.
// Because package main cannot be imported, the daemon binary (built from
// cmd/cogos) could not reach those registrations and pkg/reconcile.ListProviders()
// always returned empty.
//
// This package provides daemon-safe provider structs whose Health()
// implementations replicate the workspace-root logic using only importable
// packages (os, filepath, pkg/reconcile).
// The non-health methods (LoadConfig, FetchLive, ComputePlan, ApplyPlan,
// BuildState) return errors — the daemon only exercises Health() through the
// proprioception block.
//
// Workspace context is injected at boot via SetWorkspaceRoot(), called by
// engine.SetProvidersWorkspace after LoadConfig resolves cfg.WorkspaceRoot.
// This avoids the workspace.ResolveWorkspace() dependency-injection seams
// (LoadConfig/GitRoot func vars) that are only wired in the cog CLI's main
// package, not in cmd/cogos.
//
// The component provider is already fully extracted to
// internal/providers/component and is wired here via blank import.
// The pin provider (internal/providers/pin) is fully extracted and registered
// here directly — its Health() delegates to the extracted package.
// The other seven are implemented as minimal structs below.
//
// cmd/cogos/providers_wire.go imports this package (triggering init()) and
// wires both engine.RegisterProviders and engine.SetProvidersWorkspace so
// the full seam is operational before the HTTP server starts serving requests.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/cogos-dev/cogos/internal/providers/pin"
	"github.com/cogos-dev/cogos/pkg/reconcile"

	// Trigger internal/providers/component's init() which registers "component".
	_ "github.com/cogos-dev/cogos/internal/providers/component"
)

// workspaceRoot is set at daemon boot by SetWorkspaceRoot (called from
// engine.SetProvidersWorkspace after LoadConfig resolves the workspace path).
// It is read by resolveRoot() on every Health() probe. Zero value means
// "not yet wired" — callers get a clear error rather than a silent bad result.
var (
	workspaceRootMu sync.RWMutex
	workspaceRoot   string
)

// SetWorkspaceRoot injects the resolved workspace path into this package so
// that all daemon-side provider Health() implementations can resolve their
// filesystem checks without depending on workspace.ResolveWorkspace(), whose
// dependency-injection seams (LoadConfig/GitRoot) are not wired in the
// cmd/cogos binary.
//
// Must be called before any provider Health() invocation — engine.runServe()
// calls it via engine.SetProvidersWorkspace immediately after LoadConfig
// resolves cfg.WorkspaceRoot.
func SetWorkspaceRoot(root string) {
	workspaceRootMu.Lock()
	defer workspaceRootMu.Unlock()
	workspaceRoot = root
}

// pinProvider is the daemon-side wrapper around the fully-extracted pin provider.
// It delegates Health() directly to pin.New(), wiring the workspace root at
// probe time via resolveRoot(). The non-health methods are provided by
// the embedded stubMethods (daemon only needs Health() for the proprioception block).
type pinProvider struct {
	stubMethods
	mu   sync.Mutex
	impl *pin.Provider
}

func (p *pinProvider) Type() string { return "pin" }

func (p *pinProvider) Health() reconcile.ResourceStatus {
	root, bad := resolveRoot()
	if bad != nil {
		return *bad
	}
	p.mu.Lock()
	if p.impl == nil {
		p.impl = pin.New(nil)
	}
	impl := p.impl
	p.mu.Unlock()

	// Wire the workspace root into the pin provider so its Health() can
	// inspect .cog/pins/. LoadConfig is idempotent; call it cheaply here so
	// the provider's internal root field is always current.
	if _, err := impl.LoadConfig(root); err != nil {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthDegraded,
			Operation: reconcile.OperationIdle,
			Message:   fmt.Sprintf("pin LoadConfig: %v", err),
		}
	}
	return impl.Health()
}

func init() {
	reconcile.RegisterProvider("agent", &agentProvider{})
	reconcile.RegisterProvider("discord", &discordProvider{})
	reconcile.RegisterProvider("eval", &evalProvider{})
	reconcile.RegisterProvider("mcp-tools", &mcpToolsProvider{})
	reconcile.RegisterProvider("openclaw-agents", &openclawAgentsProvider{})
	reconcile.RegisterProvider("openclaw-cron", &openclawCronProvider{})
	reconcile.RegisterProvider("openclaw-gateway", &openclawGatewayProvider{})
	reconcile.RegisterProvider("pin", &pinProvider{stubMethods: stubMethods{name: "pin"}})
	reconcile.RegisterProvider("service", &serviceProvider{})
}

// resolveRoot returns the workspace root or an error status.
// It reads the package-level workspaceRoot set by SetWorkspaceRoot, bypassing
// workspace.ResolveWorkspace() whose DI seams are not wired in cmd/cogos.
func resolveRoot() (string, *reconcile.ResourceStatus) {
	workspaceRootMu.RLock()
	root := workspaceRoot
	workspaceRootMu.RUnlock()
	if root == "" {
		s := reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthMissing,
			Operation: reconcile.OperationIdle,
			Message:   "workspace not yet configured",
		}
		return "", &s
	}
	return root, nil
}

// stubMethods satisfies the non-Health parts of reconcile.Reconcilable.
// All operations return "daemon: operation not available" — the daemon only
// calls Health() through the proprioception block.
type stubMethods struct{ name string }

func (s *stubMethods) LoadConfig(_ string) (any, error) {
	return nil, fmt.Errorf("daemon: LoadConfig not available for %s provider", s.name)
}
func (s *stubMethods) FetchLive(_ context.Context, _ any) (any, error) {
	return nil, fmt.Errorf("daemon: FetchLive not available for %s provider", s.name)
}
func (s *stubMethods) ComputePlan(_ any, _ any, _ *reconcile.State) (*reconcile.Plan, error) {
	return nil, fmt.Errorf("daemon: ComputePlan not available for %s provider", s.name)
}
func (s *stubMethods) ApplyPlan(_ context.Context, _ *reconcile.Plan) ([]reconcile.Result, error) {
	return nil, fmt.Errorf("daemon: ApplyPlan not available for %s provider", s.name)
}
func (s *stubMethods) BuildState(_ any, _ any, _ *reconcile.State) (*reconcile.State, error) {
	return nil, fmt.Errorf("daemon: BuildState not available for %s provider", s.name)
}

// ─── agent ────────────────────────────────────────────────────────────────────

type agentProvider struct{ stubMethods }

func (p *agentProvider) Type() string { return "agent" }

func (p *agentProvider) Health() reconcile.ResourceStatus {
	root, bad := resolveRoot()
	if bad != nil {
		return *bad
	}
	agentsDir := filepath.Join(root, ".cog", "bin", "agents")
	info, err := os.Stat(agentsDir)
	if err != nil {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthMissing,
			Operation: reconcile.OperationIdle,
			Message:   fmt.Sprintf("agents directory missing: %v", err),
		}
	}
	if !info.IsDir() {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthDegraded,
			Operation: reconcile.OperationIdle,
			Message:   "agents path exists but is not a directory",
		}
	}
	registryPath := filepath.Join(agentsDir, "registry.yaml")
	if _, err := os.Stat(registryPath); err != nil {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthDegraded,
			Operation: reconcile.OperationIdle,
			Message:   "registry.yaml missing",
		}
	}
	return reconcile.ResourceStatus{
		Sync:      reconcile.SyncStatusUnknown,
		Health:    reconcile.HealthHealthy,
		Operation: reconcile.OperationIdle,
		Message:   fmt.Sprintf("agents directory readable (%s)", agentsDir),
	}
}

// ─── discord ──────────────────────────────────────────────────────────────────

type discordProvider struct{ stubMethods }

func (p *discordProvider) Type() string { return "discord" }

func (p *discordProvider) Health() reconcile.ResourceStatus {
	// Token presence mirrors the workspace-root DiscordProvider.Health() check.
	if os.Getenv("DISCORD_BOT_TOKEN") == "" {
		// Check .cog/config/discord/config.hcl for token field.
		root, bad := resolveRoot()
		if bad != nil {
			return *bad
		}
		hclPath := filepath.Join(root, ".cog", "config", "discord", "config.hcl")
		if _, err := os.Stat(hclPath); err != nil {
			return reconcile.ResourceStatus{
				Sync:      reconcile.SyncStatusUnknown,
				Health:    reconcile.HealthMissing,
				Operation: reconcile.OperationIdle,
				Message:   "no bot token configured",
			}
		}
	}
	return reconcile.NewResourceStatus(reconcile.SyncStatusUnknown, reconcile.HealthHealthy)
}

// ─── mcp-tools ────────────────────────────────────────────────────────────────

type mcpToolsProvider struct{ stubMethods }

func (p *mcpToolsProvider) Type() string { return "mcp-tools" }

func (p *mcpToolsProvider) Health() reconcile.ResourceStatus {
	if os.Getenv("OPENCLAW_URL") == "" {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthSuspended,
			Operation: reconcile.OperationIdle,
			Message:   "OPENCLAW_URL not set — bridge not available",
		}
	}
	root, bad := resolveRoot()
	if bad != nil {
		return *bad
	}
	statePath := filepath.Join(root, ".cog", "config", "mcp-tools", ".state.json")
	if _, err := os.Stat(statePath); err != nil {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthProgressing,
			Operation: reconcile.OperationIdle,
			Message:   "no state file — tools not yet discovered",
		}
	}
	return reconcile.NewResourceStatus(reconcile.SyncStatusSynced, reconcile.HealthHealthy)
}

// ─── openclaw-agents ─────────────────────────────────────────────────────────

type openclawAgentsProvider struct{ stubMethods }

func (p *openclawAgentsProvider) Type() string { return "openclaw-agents" }

func (p *openclawAgentsProvider) Health() reconcile.ResourceStatus {
	home, _ := os.UserHomeDir()
	configPath := filepath.Join(home, ".openclaw", "openclaw.json")
	if _, err := os.Stat(configPath); err != nil {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthMissing,
			Operation: reconcile.OperationIdle,
			Message:   "openclaw.json not found",
		}
	}
	return reconcile.NewResourceStatus(reconcile.SyncStatusUnknown, reconcile.HealthHealthy)
}

// ─── openclaw-cron ───────────────────────────────────────────────────────────

type openclawCronProvider struct{ stubMethods }

func (p *openclawCronProvider) Type() string { return "openclaw-cron" }

func (p *openclawCronProvider) Health() reconcile.ResourceStatus {
	home, _ := os.UserHomeDir()
	cronPath := filepath.Join(home, ".openclaw", "cron", "jobs.json")
	if _, err := os.Stat(cronPath); err != nil {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthMissing,
			Operation: reconcile.OperationIdle,
			Message:   "jobs.json not found (will be created on first apply)",
		}
	}
	return reconcile.NewResourceStatus(reconcile.SyncStatusUnknown, reconcile.HealthHealthy)
}

// ─── openclaw-gateway ────────────────────────────────────────────────────────

type openclawGatewayProvider struct{ stubMethods }

func (p *openclawGatewayProvider) Type() string { return "openclaw-gateway" }

func (p *openclawGatewayProvider) Health() reconcile.ResourceStatus {
	home, _ := os.UserHomeDir()
	configPath := filepath.Join(home, ".openclaw", "openclaw.json")
	if _, err := os.Stat(configPath); err != nil {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthMissing,
			Operation: reconcile.OperationIdle,
			Message:   "openclaw.json not found",
		}
	}
	return reconcile.NewResourceStatus(reconcile.SyncStatusUnknown, reconcile.HealthHealthy)
}

// ─── eval ─────────────────────────────────────────────────────────────────────

// evalProvider surfaces eval harness health for the daemon's proprioception block.
//
// Health() reads two state files from .cog/state/ that the eval harness writes:
//   - eval-baselines.json        — which experiments have a pinned baseline
//   - eval-dispatch-triggers.json — pending on-demand run requests
//
// Full plan/apply lives in the workspace-root CLI binary (eval_wiring.go);
// the daemon only needs Health() to contribute to the foveated context block.
type evalProvider struct{ stubMethods }

func (p *evalProvider) Type() string { return "eval" }

func (p *evalProvider) Health() reconcile.ResourceStatus {
	root, bad := resolveRoot()
	if bad != nil {
		return *bad
	}

	stateDir := filepath.Join(root, ".cog", "state")

	// Read baseline pins — presence indicates experiments are being tracked.
	pinsPath := filepath.Join(stateDir, "eval-baselines.json")
	pinnedCount := 0
	if data, err := os.ReadFile(pinsPath); err == nil {
		var pins map[string]string
		if json.Unmarshal(data, &pins) == nil {
			pinnedCount = len(pins)
		}
	}

	// Read pending dispatch triggers — non-empty means a run was requested but
	// the reconcile cycle hasn't consumed it yet.
	triggersPath := filepath.Join(stateDir, "eval-dispatch-triggers.json")
	pendingCount := 0
	if data, err := os.ReadFile(triggersPath); err == nil {
		var triggers map[string]bool
		if json.Unmarshal(data, &triggers) == nil {
			pendingCount = len(triggers)
		}
	}

	// Check for the experiments directory — its presence signals eval is configured.
	experimentsDir := filepath.Join(root, ".cog", "mem", "semantic", "architecture", "tournament", "experiments")
	_, expDirErr := os.Stat(experimentsDir)

	switch {
	case expDirErr != nil:
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthMissing,
			Operation: reconcile.OperationIdle,
			Message:   "no tournament experiments directory — eval not configured",
		}
	case pendingCount > 0:
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusOutOfSync,
			Health:    reconcile.HealthProgressing,
			Operation: reconcile.OperationSyncing,
			Message:   fmt.Sprintf("%d pending trigger(s), %d pinned baseline(s)", pendingCount, pinnedCount),
		}
	default:
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthHealthy,
			Operation: reconcile.OperationIdle,
			Message:   fmt.Sprintf("%d pinned baseline(s); full plan/apply via cog CLI", pinnedCount),
		}
	}
}

// ─── service ─────────────────────────────────────────────────────────────────

// serviceProvider checks for service CRD yaml files under
// .cog/config/services/. Full Docker container-status checks require the
// workspace-root CLI (cog plan service) — the daemon reports structural
// presence only.
type serviceProvider struct{ stubMethods }

func (p *serviceProvider) Type() string { return "service" }

func (p *serviceProvider) Health() reconcile.ResourceStatus {
	root, bad := resolveRoot()
	if bad != nil {
		return *bad
	}
	servicesDir := filepath.Join(root, ".cog", "config", "services")
	entries, err := os.ReadDir(servicesDir)
	if err != nil {
		// No services directory — treat as healthy (no services declared).
		return reconcile.NewResourceStatus(reconcile.SyncStatusSynced, reconcile.HealthHealthy)
	}
	count := 0
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".yaml" {
			count++
		}
	}
	if count == 0 {
		return reconcile.NewResourceStatus(reconcile.SyncStatusSynced, reconcile.HealthHealthy)
	}
	return reconcile.ResourceStatus{
		Sync:      reconcile.SyncStatusUnknown,
		Health:    reconcile.HealthHealthy,
		Operation: reconcile.OperationIdle,
		Message:   fmt.Sprintf("%d service CRD(s) declared; runtime status requires CLI", count),
	}
}
