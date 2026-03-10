// reconcile_mcp_tools.go — Reconciliation provider for MCP external tools.
//
// Discovers available tools from the OpenClaw gateway via the bridge's
// /tools/invoke endpoint (tool: "tools_list") and reconciles them against
// a local policy config (allowlist/denylist).
//
// This provider populates the tool registry that ultimately feeds
// MCPServer.externalTools so that tools/list returns the full merged set.
//
// Config source: .cog/config/mcp-tools/policy.yaml (optional — default: allow all)
// State target:  .cog/config/mcp-tools/.state.json

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

func init() {
	RegisterProvider("mcp-tools", &MCPToolsProvider{})
}

// MCPToolsProvider implements Reconcilable for MCP external tool management.
type MCPToolsProvider struct {
	root string
}

func (p *MCPToolsProvider) Type() string { return "mcp-tools" }

// --- Policy config types ---

// mcpToolPolicy describes which tools are allowed or denied.
// If both allowlist and denylist are empty, all discovered tools are allowed.
type mcpToolPolicy struct {
	Allowlist []string `yaml:"allowlist,omitempty"` // only these tools (empty = allow all)
	Denylist  []string `yaml:"denylist,omitempty"`  // exclude these tools
}

// isAllowed returns true if the given tool name passes the policy filter.
func (pol *mcpToolPolicy) isAllowed(name string) bool {
	// Check denylist first.
	for _, denied := range pol.Denylist {
		if denied == name {
			return false
		}
	}
	// If allowlist is empty, everything not denied is allowed.
	if len(pol.Allowlist) == 0 {
		return true
	}
	for _, allowed := range pol.Allowlist {
		if allowed == name {
			return true
		}
	}
	return false
}

// --- Live tool type ---

// discoveredTool is a tool found via the gateway.
type discoveredTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// --- Reconcilable implementation ---

// LoadConfig reads tool policy from .cog/config/mcp-tools/policy.yaml.
// If the file does not exist, returns a default "allow all" policy.
func (p *MCPToolsProvider) LoadConfig(root string) (any, error) {
	p.root = root
	policyPath := filepath.Join(root, ".cog", "config", "mcp-tools", "policy.yaml")
	data, err := os.ReadFile(policyPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Default: allow all tools.
			return &mcpToolPolicy{}, nil
		}
		return nil, fmt.Errorf("mcp-tools: read policy: %w", err)
	}

	var policy mcpToolPolicy
	if err := yaml.Unmarshal(data, &policy); err != nil {
		return nil, fmt.Errorf("mcp-tools: parse policy: %w", err)
	}
	return &policy, nil
}

// FetchLive discovers available tools from the OpenClaw gateway via the bridge.
// Uses ExecuteTool("tools_list", {}) to retrieve the tool manifest.
// If the bridge is unavailable (no OPENCLAW_URL), returns an empty list.
func (p *MCPToolsProvider) FetchLive(ctx context.Context, config any) (any, error) {
	openclawURL := os.Getenv("OPENCLAW_URL")
	openclawToken := os.Getenv("OPENCLAW_TOKEN")
	if openclawURL == "" {
		// Bridge not configured — return empty tool list.
		return []discoveredTool{}, nil
	}

	bridge := NewOpenClawBridge(openclawURL, openclawToken, "")
	result, err := bridge.ExecuteTool(ctx, "tools_list", map[string]interface{}{})
	if err != nil {
		return nil, fmt.Errorf("mcp-tools: fetch tools from gateway: %w", err)
	}

	// Parse the result text as a JSON array of tool objects.
	if result.IsError || len(result.Content) == 0 {
		errText := "unknown error"
		if len(result.Content) > 0 {
			errText = result.Content[0].Text
		}
		return nil, fmt.Errorf("mcp-tools: gateway returned error: %s", errText)
	}

	var tools []discoveredTool
	if err := json.Unmarshal([]byte(result.Content[0].Text), &tools); err != nil {
		// Gateway may return a different shape — try as array of strings.
		var names []string
		if err2 := json.Unmarshal([]byte(result.Content[0].Text), &names); err2 == nil {
			for _, name := range names {
				tools = append(tools, discoveredTool{Name: name})
			}
		} else {
			// Wrap the original tool objects in a map check.
			var toolMap map[string]any
			if err3 := json.Unmarshal([]byte(result.Content[0].Text), &toolMap); err3 == nil {
				for name := range toolMap {
					tools = append(tools, discoveredTool{Name: name})
				}
			} else {
				return nil, fmt.Errorf("mcp-tools: parse tools response: %w (raw: %.200s)", err, result.Content[0].Text)
			}
		}
	}

	return tools, nil
}

// ComputePlan diffs the policy config against live tools and produces actions.
func (p *MCPToolsProvider) ComputePlan(config any, live any, state *ReconcileState) (*ReconcilePlan, error) {
	policy := config.(*mcpToolPolicy)
	tools := live.([]discoveredTool)

	plan := &ReconcilePlan{
		ResourceType: "mcp-tools",
		GeneratedAt:  nowISO(),
		ConfigPath:   ".cog/config/mcp-tools/policy.yaml",
	}

	// Build index of previous state for drift detection.
	stateIdx := ReconcileResourceIndex(state)

	// Sort tools for deterministic output.
	sort.Slice(tools, func(i, j int) bool {
		return tools[i].Name < tools[j].Name
	})

	for _, tool := range tools {
		addr := "mcp-tool." + tool.Name

		if !policy.isAllowed(tool.Name) {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionDelete,
				ResourceType: "mcp-tool",
				Name:         tool.Name,
				Details: map[string]any{
					"reason": "tool denied by policy",
				},
			})
			plan.Summary.Deletes++
			continue
		}

		// Check if this tool is new or changed.
		prev, existed := stateIdx[addr]
		if !existed {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionCreate,
				ResourceType: "mcp-tool",
				Name:         tool.Name,
				Details: map[string]any{
					"reason":      "new tool discovered",
					"description": tool.Description,
				},
			})
			plan.Summary.Creates++
		} else {
			// Check for description drift.
			prevDesc, _ := prev.Attributes["description"].(string)
			if prevDesc != tool.Description {
				plan.Actions = append(plan.Actions, ReconcileAction{
					Action:       ActionUpdate,
					ResourceType: "mcp-tool",
					Name:         tool.Name,
					Details: map[string]any{
						"reason":          "tool description changed",
						"old_description": prevDesc,
						"new_description": tool.Description,
					},
				})
				plan.Summary.Updates++
			} else {
				plan.Actions = append(plan.Actions, ReconcileAction{
					Action:       ActionSkip,
					ResourceType: "mcp-tool",
					Name:         tool.Name,
					Details: map[string]any{
						"reason": "in sync",
					},
				})
				plan.Summary.Skipped++
			}
		}
	}

	// Check for tools in state that are no longer live (removed from gateway).
	if state != nil {
		liveSet := make(map[string]bool, len(tools))
		for _, t := range tools {
			liveSet[t.Name] = true
		}
		for _, res := range state.Resources {
			if _, still := liveSet[res.Name]; !still {
				plan.Actions = append(plan.Actions, ReconcileAction{
					Action:       ActionDelete,
					ResourceType: "mcp-tool",
					Name:         res.Name,
					Details: map[string]any{
						"reason": "tool no longer available from gateway",
					},
				})
				plan.Summary.Deletes++
			}
		}
	}

	return plan, nil
}

// ApplyPlan logs what would be applied. Phase 1: report-only.
// Future phases will populate a shared tool registry for MCPServer.externalTools.
func (p *MCPToolsProvider) ApplyPlan(ctx context.Context, plan *ReconcilePlan) ([]ReconcileResult, error) {
	var results []ReconcileResult
	for _, action := range plan.Actions {
		if action.Action == ActionSkip {
			continue
		}
		reason, _ := action.Details["reason"].(string)
		fmt.Printf("  [mcp-tools] %s %s: %s\n", action.Action, action.Name, reason)
		results = append(results, ReconcileResult{
			Phase:  "mcp-tools",
			Action: string(action.Action),
			Name:   action.Name,
			Status: ApplySkipped,
			Error:  "phase 1: report-only mode",
		})
	}
	return results, nil
}

// BuildState constructs reconcile state from discovered tools.
func (p *MCPToolsProvider) BuildState(config any, live any, existing *ReconcileState) (*ReconcileState, error) {
	policy := config.(*mcpToolPolicy)
	tools := live.([]discoveredTool)

	state := &ReconcileState{
		Version:      1,
		ResourceType: "mcp-tools",
		GeneratedAt:  nowISO(),
	}

	if existing != nil {
		state.Lineage = existing.Lineage
		state.Serial = existing.Serial + 1
	} else {
		state.Lineage = "mcp-tools-" + nowISO()
	}

	now := time.Now().Format(time.RFC3339)
	for _, tool := range tools {
		if !policy.isAllowed(tool.Name) {
			continue
		}
		state.Resources = append(state.Resources, ReconcileResource{
			Address:       "mcp-tool." + tool.Name,
			Type:          "mcp-tool",
			Mode:          ModeManaged,
			ExternalID:    tool.Name,
			Name:          tool.Name,
			LastRefreshed: now,
			Attributes: map[string]any{
				"description": tool.Description,
			},
		})
	}

	// Sort by address for deterministic output.
	sort.Slice(state.Resources, func(i, j int) bool {
		return state.Resources[i].Address < state.Resources[j].Address
	})

	return state, nil
}

// Health returns the current status of the MCP tools subsystem.
func (p *MCPToolsProvider) Health() ResourceStatus {
	openclawURL := os.Getenv("OPENCLAW_URL")
	if openclawURL == "" {
		return ResourceStatus{
			Sync:      SyncStatusUnknown,
			Health:    HealthSuspended,
			Operation: OperationIdle,
			Message:   "OPENCLAW_URL not set — bridge not available",
		}
	}

	// Check if state file exists (indicates at least one successful reconciliation).
	root := p.root
	if root == "" {
		var err error
		root, _, err = ResolveWorkspace()
		if err != nil {
			return ResourceStatus{
				Sync: SyncStatusUnknown, Health: HealthMissing, Operation: OperationIdle,
				Message: "workspace not found",
			}
		}
	}

	statePath := filepath.Join(root, ".cog", "config", "mcp-tools", ".state.json")
	if _, err := os.Stat(statePath); err != nil {
		return ResourceStatus{
			Sync:      SyncStatusUnknown,
			Health:    HealthProgressing,
			Operation: OperationIdle,
			Message:   "no state file — tools not yet discovered",
		}
	}

	return NewResourceStatus(SyncStatusSynced, HealthHealthy)
}
