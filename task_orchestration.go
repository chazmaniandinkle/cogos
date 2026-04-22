package main

import (
	"fmt"
	"sort"
	"strings"
)

// === TASK ORCHESTRATION FUNCTIONS ===
//
// Library helpers for task dependency graph construction, cycle detection,
// and topological sorting. Retained after the removal of the `cog run`,
// `cog tasks`, and `cog cache` subcommands because `buildTaskGraph`,
// `detectCycles`, and `topoSort` are exercised by unit benchmarks and
// kernel-guarantee tests.

// buildTaskGraph constructs a dependency graph from task definitions.
// Edge semantics: task -> dependency (task depends on dependency).
func buildTaskGraph(tasks map[string]Task) (*TaskGraph, error) {
	graph := &TaskGraph{
		Nodes:    make(map[string]*Task),
		Edges:    make(map[string][]string),
		InDegree: make(map[string]int),
	}

	// Add all tasks as nodes
	for name, task := range tasks {
		t := task // Create copy
		graph.Nodes[name] = &t
		graph.InDegree[name] = 0
	}

	// Build edges from dependencies.
	// In-degree: number of tasks that depend on this task.
	for name, task := range tasks {
		graph.Edges[name] = make([]string, 0)
		for _, dep := range task.DependsOn {
			// Skip Turborepo-specific syntax like ^build
			if strings.HasPrefix(dep, "^") {
				continue // Skip upstream dependency markers for now
			}
			// Skip dependencies that don't exist (might be in other workspaces)
			if _, exists := graph.Nodes[dep]; !exists {
				continue // Lenient validation - skip missing deps
			}
			graph.Edges[name] = append(graph.Edges[name], dep)
			graph.InDegree[name]++
		}
	}

	return graph, nil
}

// detectCycles uses DFS to detect cycles in the task graph.
func detectCycles(graph *TaskGraph) error {
	// Track visit state: 0 = unvisited, 1 = visiting, 2 = visited
	state := make(map[string]int)
	path := []string{}

	var visit func(string) error
	visit = func(node string) error {
		if state[node] == 1 {
			// Currently visiting - found a cycle
			cycleStart := 0
			for i, n := range path {
				if n == node {
					cycleStart = i
					break
				}
			}
			cycle := append(path[cycleStart:], node)
			return fmt.Errorf("cycle detected: %s", strings.Join(cycle, " -> "))
		}
		if state[node] == 2 {
			// Already visited
			return nil
		}

		state[node] = 1
		path = append(path, node)

		for _, dep := range graph.Edges[node] {
			if err := visit(dep); err != nil {
				return err
			}
		}

		state[node] = 2
		path = path[:len(path)-1]
		return nil
	}

	for node := range graph.Nodes {
		if state[node] == 0 {
			if err := visit(node); err != nil {
				return err
			}
		}
	}

	return nil
}

// topoSort performs topological sort using Kahn's algorithm.
// Returns levels where tasks in the same level can run in parallel.
func topoSort(graph *TaskGraph) ([][]string, error) {
	inDegree := make(map[string]int)
	for name, deg := range graph.InDegree {
		inDegree[name] = deg
	}

	// Build reverse edges (node -> dependents)
	reverseEdges := make(map[string][]string)
	for node, deps := range graph.Edges {
		for _, dep := range deps {
			reverseEdges[dep] = append(reverseEdges[dep], node)
		}
	}

	levels := [][]string{}
	processed := 0

	for processed < len(graph.Nodes) {
		currentLevel := []string{}
		for node := range graph.Nodes {
			if inDegree[node] == 0 {
				currentLevel = append(currentLevel, node)
			}
		}

		if len(currentLevel) == 0 {
			return nil, fmt.Errorf("cycle detected in task graph")
		}

		sort.Strings(currentLevel)
		levels = append(levels, currentLevel)

		for _, node := range currentLevel {
			inDegree[node] = -1 // Mark as processed
			processed++

			for _, dependent := range reverseEdges[node] {
				inDegree[dependent]--
			}
		}
	}

	return levels, nil
}
