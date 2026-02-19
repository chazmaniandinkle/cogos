// cogfield_components.go — ComponentAdapter for CogField graph visualization.
//
// Reads the component index (.cog/.state/components/_index.json) and produces
// infrastructure nodes and service edges for the cognitive field.
//
// Implements BlockAdapter from cogfield_adapters.go.

package main

import (
	"fmt"
	"path/filepath"
)

// ComponentAdapter produces component entities for the cognitive field.
type ComponentAdapter struct{}

func (a *ComponentAdapter) ID() string { return "component" }

func (a *ComponentAdapter) NodeConfig() AdapterNodeConfig {
	return AdapterNodeConfig{
		BlockTypes: map[string]BlockTypeConfig{
			"component": {EntityType: "component", Shape: "hexagon", Color: "#78716c", Label: "Component"},
			"node":      {EntityType: "node", Shape: "square", Color: "#a8a29e", Label: "Node"},
		},
		DefaultSector: "infrastructure",
		ChainThread:   "explicit",
	}
}

// SummaryNodes reads the component index and produces one CogFieldNode per component.
func (a *ComponentAdapter) SummaryNodes(root string) ([]CogFieldNode, []CogFieldEdge) {
	idx, err := loadComponentIndex(root)
	if err != nil {
		return nil, nil // No index yet — graceful degradation
	}

	var nodes []CogFieldNode
	var edges []CogFieldEdge

	for path, blobHash := range idx.Components {
		blob, err := loadComponentBlob(root, path)
		if err != nil {
			continue
		}

		// Build tags from capabilities + language
		var tags []string
		if blob.Language != "" {
			tags = append(tags, blob.Language)
		}
		if blob.BuildSystem != "" {
			tags = append(tags, blob.BuildSystem)
		}
		tags = append(tags, blob.Capabilities...)
		if blob.Declared != nil {
			tags = append(tags, blob.Declared.Tags...)
		}

		// Strength based on kind and required
		strength := 3.0
		if blob.Declared != nil && blob.Declared.Required {
			strength = 5.0
		}
		if blob.Kind == "in-tree" {
			strength = 4.0 // slightly higher than submodules
		}

		node := CogFieldNode{
			ID:         "component:" + path,
			Label:      filepath.Base(path),
			EntityType: "component",
			Sector:     "infrastructure",
			Tags:       tags,
			Created:    blob.IndexedAt,
			Modified:   blob.IndexedAt,
			Strength:   strength,
			Meta: map[string]any{
				"tree_hash":    blob.TreeHash,
				"blob_hash":    blobHash,
				"kind":         blob.Kind,
				"language":     blob.Language,
				"build_system": blob.BuildSystem,
				"path":         path,
				"dirty":        blob.Dirty,
			},
		}

		// Add role to meta if declared
		if blob.Declared != nil {
			node.Meta["role"] = blob.Declared.Role
			node.Meta["required"] = blob.Declared.Required

			// Service edges
			for _, svc := range blob.Declared.Services {
				edges = append(edges, CogFieldEdge{
					Source:   "component:" + path,
					Target:   fmt.Sprintf("service:%s", svc.Name),
					Relation: "provides",
					Weight:   1.0,
					Thread:   "explicit",
				})
			}
		}

		nodes = append(nodes, node)
	}

	return nodes, edges
}

// ExpandNode returns an error — component nodes do not support expansion.
func (a *ComponentAdapter) ExpandNode(root, nodeID string) ([]CogFieldNode, []CogFieldEdge, error) {
	return nil, nil, fmt.Errorf("component nodes do not support expansion")
}
