// component_provider.go — Reconciliation provider for workspace components.
// Part of ADR-060 Phase 1: report-only drift detection.
//
// Implements Reconcilable to detect drift between the declared component
// registry (.cog/conf/components.cog.md) and live on-disk state (indexed blobs).
// Phase 1 is report-only — ApplyPlan prints what it would do but does not
// execute destructive actions.

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// ComponentProvider implements Reconcilable for workspace component management.
type ComponentProvider struct {
	root string
}

func init() {
	RegisterProvider("component", &ComponentProvider{})
}

func (c *ComponentProvider) Type() string { return "component" }

// LoadConfig loads the component registry from .cog/conf/components.cog.md.
func (c *ComponentProvider) LoadConfig(root string) (any, error) {
	c.root = root
	return loadComponentRegistry(root)
}

// FetchLive runs the component indexer and loads individual blobs for comparison.
// Returns map[string]*ComponentBlob keyed by component path.
func (c *ComponentProvider) FetchLive(ctx context.Context, config any) (any, error) {
	root := c.root
	if root == "" {
		return nil, fmt.Errorf("component provider: root not set (call LoadConfig first)")
	}

	// Run the indexer to refresh all blobs and the Merkle index.
	idx, err := indexComponents(root)
	if err != nil {
		return nil, fmt.Errorf("component index: %w", err)
	}

	// Load each blob for detailed comparison.
	blobs := make(map[string]*ComponentBlob, len(idx.Components))
	for path := range idx.Components {
		blob, err := loadComponentBlob(root, path)
		if err != nil {
			// Non-fatal: log and skip this component.
			fmt.Fprintf(os.Stderr, "warning: could not load blob for %s: %v\n", path, err)
			continue
		}
		blobs[path] = blob
	}

	return blobs, nil
}

// ComputePlan compares declared config (registry) against live state (blobs)
// and produces a reconciliation plan.
func (c *ComponentProvider) ComputePlan(config any, live any, state *ReconcileState) (*ReconcilePlan, error) {
	reg := config.(*ComponentRegistry)
	blobs := live.(map[string]*ComponentBlob)

	plan := &ReconcilePlan{
		ResourceType: "component",
		GeneratedAt:  nowISO(),
		ConfigPath:   ".cog/conf/components.cog.md",
	}

	// Track which blobs are accounted for by the registry.
	seen := make(map[string]bool)

	// Sort declared paths for deterministic output.
	declaredPaths := make([]string, 0, len(reg.Components))
	for path := range reg.Components {
		declaredPaths = append(declaredPaths, path)
	}
	sort.Strings(declaredPaths)

	for _, path := range declaredPaths {
		decl := reg.Components[path]
		seen[path] = true

		blob, exists := blobs[path]
		if !exists {
			// Declared but not on disk — no blob was produced.
			action := ReconcileAction{
				Action:       ActionCreate,
				ResourceType: "component",
				Name:         path,
				Details: map[string]any{
					"reason":   "declared component not found on disk",
					"kind":     decl.Kind,
					"required": decl.Required,
				},
			}
			plan.Actions = append(plan.Actions, action)
			plan.Summary.Creates++

			if decl.Required {
				plan.Warnings = append(plan.Warnings,
					fmt.Sprintf("required component %q is missing", path))
			}
			continue
		}

		// Blob exists — check for drift.
		drifted := false
		reasons := []string{}

		// Check kind mismatch.
		if decl.Kind != "" && blob.Kind != decl.Kind {
			drifted = true
			reasons = append(reasons, fmt.Sprintf("kind: declared=%s live=%s", decl.Kind, blob.Kind))
		}

		// Check tree hash against previous state (if we have one).
		if state != nil {
			stateIdx := ReconcileResourceIndex(state)
			addr := "component." + encodePath(path)
			if prev, ok := stateIdx[addr]; ok {
				if prev.ExternalID != "" && blob.TreeHash != prev.ExternalID {
					drifted = true
					reasons = append(reasons, fmt.Sprintf("tree_hash changed: %s -> %s",
						truncHash(prev.ExternalID), truncHash(blob.TreeHash)))
				}
			}
		}

		// Check dirty status.
		if blob.Dirty {
			drifted = true
			reasons = append(reasons, "working tree is dirty")
		}

		if drifted {
			action := ReconcileAction{
				Action:       ActionUpdate,
				ResourceType: "component",
				Name:         path,
				Details: map[string]any{
					"reason":    fmt.Sprintf("drift detected: %s", joinReasons(reasons)),
					"tree_hash": blob.TreeHash,
					"dirty":     blob.Dirty,
				},
			}
			plan.Actions = append(plan.Actions, action)
			plan.Summary.Updates++
		} else {
			action := ReconcileAction{
				Action:       ActionSkip,
				ResourceType: "component",
				Name:         path,
				Details: map[string]any{
					"reason":    "in sync",
					"tree_hash": blob.TreeHash,
				},
			}
			plan.Actions = append(plan.Actions, action)
			plan.Summary.Skipped++
		}
	}

	// Check for auto-discovered components not in the registry.
	var undeclaredPaths []string
	for path := range blobs {
		if !seen[path] {
			undeclaredPaths = append(undeclaredPaths, path)
		}
	}
	sort.Strings(undeclaredPaths)

	for _, path := range undeclaredPaths {
		// Add as warnings — prune_unregistered defaults to false.
		if reg.Reconciler.PruneUnregistered {
			action := ReconcileAction{
				Action:       ActionDelete,
				ResourceType: "component",
				Name:         path,
				Details: map[string]any{
					"reason": "not in registry and prune_unregistered is true",
				},
			}
			plan.Actions = append(plan.Actions, action)
			plan.Summary.Deletes++
		} else {
			plan.Warnings = append(plan.Warnings,
				fmt.Sprintf("component %q found on disk but not declared in registry", path))
		}
	}

	return plan, nil
}

// ApplyPlan is Phase 1: report-only. Prints what would be done but does not
// execute any destructive actions.
func (c *ComponentProvider) ApplyPlan(ctx context.Context, plan *ReconcilePlan) ([]ReconcileResult, error) {
	var results []ReconcileResult
	for _, action := range plan.Actions {
		if action.Action == ActionSkip {
			continue
		}
		reason, _ := action.Details["reason"].(string)
		fmt.Printf("  [dry-run] %s %s: %s\n", action.Action, action.Name, reason)
		results = append(results, ReconcileResult{
			Phase:  "component",
			Action: string(action.Action),
			Name:   action.Name,
			Status: ApplySkipped,
			Error:  "phase 1: report-only mode",
		})
	}
	return results, nil
}

// BuildState constructs reconcile state from live blobs.
func (c *ComponentProvider) BuildState(config any, live any, existing *ReconcileState) (*ReconcileState, error) {
	blobs := live.(map[string]*ComponentBlob)

	state := &ReconcileState{
		Version:      1,
		ResourceType: "component",
		GeneratedAt:  nowISO(),
	}

	if existing != nil {
		state.Lineage = existing.Lineage
		state.Serial = existing.Serial + 1
	} else {
		state.Lineage = "component-" + nowISO()
	}

	for path, blob := range blobs {
		resource := ReconcileResource{
			Address:       "component." + encodePath(path),
			Type:          blob.Kind,
			Mode:          ModeManaged,
			ExternalID:    blob.TreeHash,
			Name:          path,
			LastRefreshed: blob.IndexedAt,
			Attributes: map[string]any{
				"commit_hash":  blob.CommitHash,
				"language":     blob.Language,
				"build_system": blob.BuildSystem,
				"dirty":        blob.Dirty,
				"blob_hash":    blob.BlobHash,
			},
		}
		state.Resources = append(state.Resources, resource)
	}

	// Sort by address for deterministic output.
	sort.Slice(state.Resources, func(i, j int) bool {
		return state.Resources[i].Address < state.Resources[j].Address
	})

	return state, nil
}

// Health returns the current three-axis status of the component subsystem.
func (c *ComponentProvider) Health() ResourceStatus {
	root := c.root
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

	reg, err := loadComponentRegistry(root)
	if err != nil {
		return ResourceStatus{
			Sync: SyncStatusUnknown, Health: HealthDegraded, Operation: OperationIdle,
			Message: "component registry not found",
		}
	}

	// Check required components exist on disk.
	missing := 0
	for path, decl := range reg.Components {
		if decl.Required {
			absPath := filepath.Join(root, path)
			if _, err := os.Stat(absPath); err != nil {
				missing++
			}
		}
	}

	if missing > 0 {
		return ResourceStatus{
			Sync: SyncStatusOutOfSync, Health: HealthDegraded, Operation: OperationIdle,
			Message: fmt.Sprintf("%d required components missing", missing),
		}
	}

	return NewResourceStatus(SyncStatusSynced, HealthHealthy)
}

// --- helpers ----------------------------------------------------------------

// truncHash shortens a hash for display, keeping the first 12 characters.
func truncHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// joinReasons joins multiple drift reasons into a semicolon-separated string.
func joinReasons(reasons []string) string {
	if len(reasons) == 0 {
		return "unknown"
	}
	result := reasons[0]
	for _, r := range reasons[1:] {
		result += "; " + r
	}
	return result
}
