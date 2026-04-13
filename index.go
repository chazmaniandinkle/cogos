package main

// index.go — cogos index & cogos drift commands (Phase 0: Foundation)
//
// Adds workspace proprioception to the cogos kernel.
// Spec: docs/specs/cogos-index-spec.md

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// =============================================================================
// DATA MODEL
// =============================================================================

// Symbol represents an extracted code symbol (function, method, class, etc.)
type Symbol struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`                // function, method, class, interface, variable, type, const, export, heading
	File      string `json:"file"`                // relative to workspace root
	Line      int    `json:"line"`
	EndLine   int    `json:"end_line"`            // symbol extent for better diffing
	Language  string `json:"language"`
	Scope     string `json:"scope,omitempty"`     // parent class/struct/namespace
	Signature string `json:"signature,omitempty"` // reconstructed signature
	Exported  bool   `json:"exported"`
	DocString string `json:"doc,omitempty"`       // extracted documentation
}

// FileRecord represents a single indexed file.
type FileRecord struct {
	Path        string   `json:"path"`
	Language    string   `json:"language"`
	Size        int64    `json:"size"`
	Hash        string   `json:"hash"`                  // SHA256 first 16 chars
	ModTime     string   `json:"mod_time"`              // for incremental indexing
	SymbolCount int      `json:"symbol_count"`
	Imports     []string `json:"imports"`
	Exports     []string `json:"exports"`
	Package     string   `json:"package,omitempty"`     // Go package, JS module
}

// IndexStats contains summary statistics for a workspace index.
type IndexStats struct {
	Files        int            `json:"files"`
	Symbols      int            `json:"symbols"`
	Components   int            `json:"components"`
	Languages    map[string]int `json:"languages"`
	ImportEdges  int            `json:"import_edges"`
	CallEdges    int            `json:"call_edges"`
	DurationMs   int64          `json:"duration_ms"`
}

// WorkspaceIndex is the complete structural index of a workspace.
type WorkspaceIndex struct {
	Version      int                 `json:"version"`       // Schema version (1)
	Timestamp    string              `json:"timestamp"`
	Workspace    string              `json:"workspace"`
	GitCommit    string              `json:"git_commit"`    // ties index to commit
	Stats        IndexStats          `json:"stats"`
	Files        []FileRecord        `json:"files"`
	Symbols      []Symbol            `json:"symbols"`
	ComponentMap map[string][]string `json:"component_map"`
	Dependencies map[string][]string `json:"dependencies"`
	CallGraph    map[string][]string `json:"call_graph"`
}

// DriftReport represents the structural diff between two indexes.
type DriftReport struct {
	OldCommit      string        `json:"old_commit"`
	NewCommit      string        `json:"new_commit"`
	Timestamp      string        `json:"timestamp"`
	AddedFiles     []FileRecord  `json:"added_files"`
	RemovedFiles   []FileRecord  `json:"removed_files"`
	ModifiedFiles  []FileDiff    `json:"modified_files"`
	AddedSymbols   []Symbol      `json:"added_symbols"`
	RemovedSymbols []Symbol      `json:"removed_symbols"`
	MovedSymbols   []SymbolMove  `json:"moved_symbols"`
	DepChanges     []DepChange   `json:"dep_changes"`
	ComponentDelta []string      `json:"new_components"`
	Summary        string        `json:"summary"`
}

// FileDiff represents a modified file between two index snapshots.
type FileDiff struct {
	Path       string `json:"path"`
	OldHash    string `json:"old_hash"`
	NewHash    string `json:"new_hash"`
	OldSymbols int    `json:"old_symbols"`
	NewSymbols int    `json:"new_symbols"`
}

// SymbolMove represents a symbol that was renamed or moved between files.
type SymbolMove struct {
	OldName string  `json:"old_name"`
	NewName string  `json:"new_name"`
	OldFile string  `json:"old_file"`
	NewFile string  `json:"new_file"`
	Kind    string  `json:"kind"`
	Score   float64 `json:"score"` // similarity score (0-1)
}

// DepChange represents an import/dependency change for a file.
type DepChange struct {
	File    string   `json:"file"`
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
}

// =============================================================================
// COMMANDS
// =============================================================================

func cmdIndex(args []string) int {
	// Parse flags
	var (
		watch     bool
		jsonOnly  bool
		component string
		lang      string
		statsOnly bool
		showHelp  bool
	)

	i := 0
	for i < len(args) {
		switch args[i] {
		case "--watch", "-w":
			watch = true
			i++
		case "--json":
			jsonOnly = true
			i++
		case "--component":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: --component requires an argument\n")
				return 1
			}
			component = args[i+1]
			i += 2
		case "--lang":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: --lang requires an argument\n")
				return 1
			}
			lang = args[i+1]
			i += 2
		case "--stats":
			statsOnly = true
			i++
		case "--help", "-h":
			showHelp = true
			i++
		default:
			// Positional: workspace path (optional)
			i++
		}
	}

	if showHelp {
		printIndexHelp()
		return 0
	}

	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	scanOpts := ScanOptions{
		Root:      root,
		Component: component,
		Language:  lang,
	}

	cogRoot := filepath.Join(root, ".cog")
	indexDir := filepath.Join(cogRoot, "run", "index")
	docsDir := filepath.Join(root, "docs", "index")

	// Watch mode: continuous re-indexing
	if watch {
		var writers []IndexWriter
		writers = append(writers, &JSONIndexWriter{OutputDir: indexDir})
		if !jsonOnly {
			writers = append(writers, &MarkdownIndexWriter{OutputDir: docsDir})
		}
		var driftWriters []DriftWriter
		driftWriters = append(driftWriters, &JSONDriftWriter{OutputDir: indexDir})
		if !jsonOnly {
			driftWriters = append(driftWriters, &MarkdownDriftWriter{OutputDir: docsDir})
		}

		// Do initial scan
		idx, err := ScanWorkspace(scanOpts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error scanning workspace: %v\n", err)
			return 1
		}
		for _, w := range writers {
			w.WriteIndex(idx)
		}
		fmt.Fprintf(os.Stderr, "Initial index: %d files (%d symbols, %d call edges) in %dms\n",
			idx.Stats.Files, idx.Stats.Symbols, idx.Stats.CallEdges, idx.Stats.DurationMs)

		if err := watchWorkspace(root, scanOpts, writers, driftWriters); err != nil {
			fmt.Fprintf(os.Stderr, "Watch error: %v\n", err)
			return 1
		}
		return 0
	}

	// One-shot scan
	idx, err := ScanWorkspace(scanOpts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error scanning workspace: %v\n", err)
		return 1
	}

	// Stats-only mode
	if statsOnly {
		output, _ := json.MarshalIndent(idx.Stats, "", "  ")
		fmt.Println(string(output))
		return 0
	}

	// Write JSON output
	jsonWriter := &JSONIndexWriter{OutputDir: indexDir}
	if err := jsonWriter.WriteIndex(idx); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to write index file: %v\n", err)
	}

	// Write markdown output (unless --json)
	if !jsonOnly {
		mdWriter := &MarkdownIndexWriter{OutputDir: docsDir}
		if err := mdWriter.WriteIndex(idx); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to write markdown: %v\n", err)
		}
	}

	// Print to stdout
	output, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	fmt.Println(string(output))

	// Emit bus event
	emitIndexBusEvent(root, idx)

	// Summary to stderr
	fmt.Fprintf(os.Stderr, "Indexed %d files (%d symbols, %d call edges, %d components) in %dms\n",
		idx.Stats.Files, idx.Stats.Symbols, idx.Stats.CallEdges, idx.Stats.Components, idx.Stats.DurationMs)

	return 0
}

func cmdDrift(args []string) int {
	var (
		baseline string
		auto     bool
		since    string
		showHelp bool
	)

	i := 0
	for i < len(args) {
		switch args[i] {
		case "--baseline":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: --baseline requires an argument\n")
				return 1
			}
			baseline = args[i+1]
			i += 2
		case "--auto":
			auto = true
			i++
		case "--since":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: --since requires an argument\n")
				return 1
			}
			since = args[i+1]
			i += 2
		case "--help", "-h":
			showHelp = true
			i++
		default:
			i++
		}
	}

	if showHelp {
		printDriftHelp()
		return 0
	}

	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	cogRoot := filepath.Join(root, ".cog")
	indexDir := filepath.Join(cogRoot, "run", "index")

	// Determine baseline path
	baselinePath := baseline
	if auto || baselinePath == "" {
		baselinePath = filepath.Join(indexDir, "current.json")
	}

	// Load baseline
	old, err := loadBaseline(baselinePath)
	if err != nil {
		if auto {
			fmt.Fprintf(os.Stderr, "No baseline found at %s — run 'cogos index' first\n", baselinePath)
			return 1
		}
		fmt.Fprintf(os.Stderr, "Error loading baseline: %v\n", err)
		return 1
	}

	_ = since // Phase 5+: git-based drift with --since

	// Fresh scan
	new, err := ScanWorkspace(ScanOptions{Root: root})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error scanning workspace: %v\n", err)
		return 1
	}

	// Compute drift
	report := computeDrift(old, new)

	// Write outputs
	jsonDrift := &JSONDriftWriter{OutputDir: indexDir}
	if err := jsonDrift.WriteDrift(report); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to write drift JSON: %v\n", err)
	}

	docsDir := filepath.Join(root, "docs", "index")
	mdDrift := &MarkdownDriftWriter{OutputDir: docsDir}
	if err := mdDrift.WriteDrift(report); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to write drift markdown: %v\n", err)
	}

	// Print to stdout
	output, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	fmt.Println(string(output))

	// Emit bus event
	emitDriftBusEvent(root, report)

	// Summary to stderr
	fmt.Fprintf(os.Stderr, "%s\n", report.Summary)

	return 0
}

// =============================================================================
// BUS EVENTS
// =============================================================================

// emitIndexBusEvent publishes an index.complete event to the bus.
func emitIndexBusEvent(root string, idx *WorkspaceIndex) {
	bus := newBusSessionManager(root)
	payload := map[string]interface{}{
		"files":       idx.Stats.Files,
		"symbols":     idx.Stats.Symbols,
		"components":  idx.Stats.Components,
		"call_edges":  idx.Stats.CallEdges,
		"import_edges": idx.Stats.ImportEdges,
		"duration_ms": idx.Stats.DurationMs,
		"git_commit":  idx.GitCommit,
	}
	if _, err := bus.appendBusEvent("bus_index", "index.complete", "cogos-index", payload); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to emit index bus event: %v\n", err)
	}
}

// emitDriftBusEvent publishes an index.drift event to the bus.
func emitDriftBusEvent(root string, report *DriftReport) {
	bus := newBusSessionManager(root)
	payload := map[string]interface{}{
		"added_files":    len(report.AddedFiles),
		"removed_files":  len(report.RemovedFiles),
		"modified_files": len(report.ModifiedFiles),
		"added_symbols":  len(report.AddedSymbols),
		"removed_symbols": len(report.RemovedSymbols),
		"moved_symbols":  len(report.MovedSymbols),
		"dep_changes":    len(report.DepChanges),
		"new_components": len(report.ComponentDelta),
		"summary":        report.Summary,
	}
	if _, err := bus.appendBusEvent("bus_index", "index.drift", "cogos-drift", payload); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to emit drift bus event: %v\n", err)
	}
}

// =============================================================================
// HELP
// =============================================================================

func printIndexHelp() {
	fmt.Println(`Usage: cogos index [options] [workspace-path]

Structural workspace indexer — extracts symbols, builds call graphs,
tracks dependencies, and classifies components.

Options:
  --watch, -w        Continuous re-index on file changes
  --json             Machine-readable JSON output only
  --component NAME   Index specific component only
  --lang LANGUAGE    Index specific language only
  --stats            Summary statistics only
  --help, -h         Show this help

Output:
  .cog/run/index/current.json    Full index (JSON)
  docs/index/SUMMARY.md          Human-readable summary
  docs/index/class-map.md        Component registry
  docs/index/dependency-map.md   Import graph
  docs/index/call-graph.md       Function relationships`)
}

func printDriftHelp() {
	fmt.Println(`Usage: cogos drift [options]

Detect structural changes between workspace index snapshots.

Options:
  --baseline PATH    Compare against a specific index file
  --auto             Compare current vs last stored index
  --since COMMIT     Structural diff since git commit
  --help, -h         Show this help

Output:
  Drift report as JSON (stdout)
  .cog/run/index/drift-report.md (when --auto)`)
}
