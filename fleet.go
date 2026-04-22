// Fleet Module - Agent fleet registry types
//
// The `cog fleet` CLI has been removed. What remains is the on-disk registry
// schema + loader used by `registry_indexer.go` (so constellation indexing
// still sees historical fleet runs) and the shared `truncate` helper used by
// the Discord bridge.

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// === FLEET REGISTRY LOCATION ===

const (
	fleetRegistryFile = ".cog/fleet/registry.json"
)

// === FLEET STATE TYPES ===

// FleetState represents the state of a fleet
type FleetState string

const (
	FleetPending   FleetState = "pending"
	FleetRunning   FleetState = "running"
	FleetCompleted FleetState = "completed"
	FleetFailed    FleetState = "failed"
)

// FleetEntry represents a single fleet in the registry
type FleetEntry struct {
	ID         string     `json:"id"`
	Config     string     `json:"config"`
	Task       string     `json:"task"`
	State      FleetState `json:"state"`
	AgentCount int        `json:"agent_count"`
	Completed  int        `json:"completed"`
	Failed     int        `json:"failed"`
	CreatedAt  string     `json:"created_at"`
	UpdatedAt  string     `json:"updated_at"`
	ResultsDir string     `json:"results_dir"`
	PID        int        `json:"pid,omitempty"`
}

// FleetRegistry holds all fleet entries
type FleetRegistry struct {
	Version string                `json:"version"`
	Fleets  map[string]FleetEntry `json:"fleets"`
}

// === REGISTRY LOADER ===

// loadFleetRegistry loads the fleet registry from disk. Returns an empty
// registry if the file does not exist.
func loadFleetRegistry(root string) (*FleetRegistry, error) {
	registryPath := filepath.Join(root, fleetRegistryFile)

	data, err := os.ReadFile(registryPath)
	if err != nil {
		return &FleetRegistry{
			Version: "1.0.0",
			Fleets:  make(map[string]FleetEntry),
		}, nil
	}

	var registry FleetRegistry
	if err := json.Unmarshal(data, &registry); err != nil {
		return nil, fmt.Errorf("failed to parse fleet registry: %w", err)
	}

	if registry.Fleets == nil {
		registry.Fleets = make(map[string]FleetEntry)
	}

	return &registry, nil
}

// === UTILITIES ===

// truncate shortens s to at most maxLen runes, appending "..." if truncated.
// Also used by event_discord_bridge.go.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
