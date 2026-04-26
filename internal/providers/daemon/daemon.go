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
// packages (os, filepath, pkg/reconcile, internal/workspace).
// The non-health methods (LoadConfig, FetchLive, ComputePlan, ApplyPlan,
// BuildState) return errors — the daemon only exercises Health() through the
// proprioception block.
//
// The component provider is already fully extracted to
// internal/providers/component and is wired here via blank import.
// The other seven are implemented as minimal structs below.
//
// cmd/cogos/providers_wire.go blank-imports this package so its init()
// runs at daemon boot, completing the registration before the HTTP server
// starts.
package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cogos-dev/cogos/internal/workspace"
	"github.com/cogos-dev/cogos/pkg/reconcile"

	// Trigger internal/providers/component's init() which registers "component".
	_ "github.com/cogos-dev/cogos/internal/providers/component"
)

func init() {
	reconcile.RegisterProvider("agent", &agentProvider{})
	reconcile.RegisterProvider("discord", &discordProvider{})
	reconcile.RegisterProvider("mcp-tools", &mcpToolsProvider{})
	reconcile.RegisterProvider("openclaw-agents", &openclawAgentsProvider{})
	reconcile.RegisterProvider("openclaw-cron", &openclawCronProvider{})
	reconcile.RegisterProvider("openclaw-gateway", &openclawGatewayProvider{})
	reconcile.RegisterProvider("service", &serviceProvider{})
}

// resolveRoot returns workspace root or an error status.
func resolveRoot() (string, *reconcile.ResourceStatus) {
	root, _, err := workspace.ResolveWorkspace()
	if err != nil {
		s := reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthMissing,
			Operation: reconcile.OperationIdle,
			Message:   "workspace not found",
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
