// reconcile_mcp_tools_test.go
// Tests for the MCPToolsProvider Reconcilable implementation.

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// ─── LoadConfig ────────────────────────────────────────────────────────────────

func TestMCPToolsProvider_LoadConfig_Default(t *testing.T) {
	root := t.TempDir()
	p := &MCPToolsProvider{}
	raw, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	pol, ok := raw.(*mcpToolPolicy)
	if !ok {
		t.Fatalf("LoadConfig returned %T, want *mcpToolPolicy", raw)
	}
	if len(pol.Allowlist) != 0 {
		t.Errorf("Allowlist = %v, want empty", pol.Allowlist)
	}
	if len(pol.Denylist) != 0 {
		t.Errorf("Denylist = %v, want empty", pol.Denylist)
	}
}

func TestMCPToolsProvider_LoadConfig_WithPolicy(t *testing.T) {
	root := t.TempDir()
	policyDir := filepath.Join(root, ".cog", "config", "mcp-tools")
	if err := os.MkdirAll(policyDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	policyContent := "allowlist:\n  - tool_a\n  - tool_c\ndenylist:\n  - tool_b\n"
	if err := os.WriteFile(filepath.Join(policyDir, "policy.yaml"), []byte(policyContent), 0644); err != nil {
		t.Fatalf("write policy: %v", err)
	}

	p := &MCPToolsProvider{}
	raw, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	pol := raw.(*mcpToolPolicy)
	if len(pol.Allowlist) != 2 {
		t.Fatalf("Allowlist len = %d, want 2", len(pol.Allowlist))
	}
	if pol.Allowlist[0] != "tool_a" || pol.Allowlist[1] != "tool_c" {
		t.Errorf("Allowlist = %v, want [tool_a tool_c]", pol.Allowlist)
	}
	if len(pol.Denylist) != 1 || pol.Denylist[0] != "tool_b" {
		t.Errorf("Denylist = %v, want [tool_b]", pol.Denylist)
	}
}

// ─── ComputePlan ───────────────────────────────────────────────────────────────

func TestMCPToolsProvider_ComputePlan(t *testing.T) {
	policy := &mcpToolPolicy{} // allow all
	tools := []discoveredTool{{Name: "tool_a"}, {Name: "tool_b"}}

	p := &MCPToolsProvider{}
	plan, err := p.ComputePlan(policy, tools, nil)
	if err != nil {
		t.Fatalf("ComputePlan failed: %v", err)
	}
	if plan.Summary.Creates != 2 {
		t.Errorf("Summary.Creates = %d, want 2", plan.Summary.Creates)
	}
	if plan.Summary.Deletes != 0 {
		t.Errorf("Summary.Deletes = %d, want 0", plan.Summary.Deletes)
	}
}

func TestMCPToolsProvider_ComputePlan_WithDenylist(t *testing.T) {
	policy := &mcpToolPolicy{Denylist: []string{"tool_b"}}
	tools := []discoveredTool{{Name: "tool_a"}, {Name: "tool_b"}}

	p := &MCPToolsProvider{}
	plan, err := p.ComputePlan(policy, tools, nil)
	if err != nil {
		t.Fatalf("ComputePlan failed: %v", err)
	}
	if plan.Summary.Creates != 1 {
		t.Errorf("Summary.Creates = %d, want 1", plan.Summary.Creates)
	}
	if plan.Summary.Deletes != 1 {
		t.Errorf("Summary.Deletes = %d, want 1", plan.Summary.Deletes)
	}

	// Verify the specific actions.
	for _, a := range plan.Actions {
		switch a.Name {
		case "tool_a":
			if a.Action != ActionCreate {
				t.Errorf("tool_a action = %s, want create", a.Action)
			}
		case "tool_b":
			if a.Action != ActionDelete {
				t.Errorf("tool_b action = %s, want delete", a.Action)
			}
		}
	}
}

// ─── BuildState ────────────────────────────────────────────────────────────────

func TestMCPToolsProvider_BuildState(t *testing.T) {
	policy := &mcpToolPolicy{Denylist: []string{"tool_c"}}
	tools := []discoveredTool{
		{Name: "tool_a", Description: "Alpha"},
		{Name: "tool_b", Description: "Beta"},
		{Name: "tool_c", Description: "Gamma"},
	}

	p := &MCPToolsProvider{}
	state, err := p.BuildState(policy, tools, nil)
	if err != nil {
		t.Fatalf("BuildState failed: %v", err)
	}
	if state.Version != 1 {
		t.Errorf("Version = %d, want 1", state.Version)
	}
	if state.ResourceType != "mcp-tools" {
		t.Errorf("ResourceType = %q, want mcp-tools", state.ResourceType)
	}
	// tool_c denied, so only 2 resources.
	if len(state.Resources) != 2 {
		t.Fatalf("Resources len = %d, want 2", len(state.Resources))
	}
	for _, r := range state.Resources {
		if r.Name == "tool_c" {
			t.Error("tool_c should be filtered out by denylist")
		}
	}
}

// ─── Health ────────────────────────────────────────────────────────────────────

func TestMCPToolsProvider_Health(t *testing.T) {
	t.Run("no OPENCLAW_URL", func(t *testing.T) {
		t.Setenv("OPENCLAW_URL", "")
		p := &MCPToolsProvider{}
		h := p.Health()
		if h.Health != HealthSuspended {
			t.Errorf("Health = %s, want Suspended", h.Health)
		}
	})

	t.Run("with state file", func(t *testing.T) {
		root := t.TempDir()
		stateDir := filepath.Join(root, ".cog", "config", "mcp-tools")
		if err := os.MkdirAll(stateDir, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, ".state.json"), []byte("{}"), 0644); err != nil {
			t.Fatalf("write state: %v", err)
		}

		t.Setenv("OPENCLAW_URL", "http://localhost:9999")
		p := &MCPToolsProvider{root: root}
		h := p.Health()
		if h.Health != HealthHealthy {
			t.Errorf("Health = %s, want Healthy", h.Health)
		}
	})
}
