package main

// index_output.go — Output writers for workspace index results.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// IndexWriter writes a workspace index in a specific format.
type IndexWriter interface {
	WriteIndex(idx *WorkspaceIndex) error
}

// DriftWriter writes a drift report in a specific format.
type DriftWriter interface {
	WriteDrift(report *DriftReport) error
}

// =============================================================================
// JSON WRITER
// =============================================================================

// JSONIndexWriter writes the workspace index as JSON.
type JSONIndexWriter struct {
	OutputDir string // e.g. ".cog/run/index"
}

func (w *JSONIndexWriter) WriteIndex(idx *WorkspaceIndex) error {
	if err := os.MkdirAll(w.OutputDir, 0755); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}

	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling index: %w", err)
	}

	path := filepath.Join(w.OutputDir, "current.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("writing index: %w", err)
	}

	return nil
}

// JSONDriftWriter writes the drift report as JSON.
type JSONDriftWriter struct {
	OutputDir string
}

func (w *JSONDriftWriter) WriteDrift(report *DriftReport) error {
	if err := os.MkdirAll(w.OutputDir, 0755); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}

	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling drift report: %w", err)
	}

	path := filepath.Join(w.OutputDir, "drift-report.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("writing drift report: %w", err)
	}

	return nil
}

// =============================================================================
// MARKDOWN INDEX WRITERS
// =============================================================================

// MarkdownIndexWriter writes human-readable markdown outputs.
type MarkdownIndexWriter struct {
	OutputDir string // e.g. "docs/index"
}

func (w *MarkdownIndexWriter) WriteIndex(idx *WorkspaceIndex) error {
	if err := os.MkdirAll(w.OutputDir, 0755); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}

	if err := w.writeSummary(idx); err != nil {
		return err
	}
	if err := w.writeClassMap(idx); err != nil {
		return err
	}
	if err := w.writeDependencyMap(idx); err != nil {
		return err
	}
	if err := w.writeCallGraph(idx); err != nil {
		return err
	}
	return nil
}

func (w *MarkdownIndexWriter) writeSummary(idx *WorkspaceIndex) error {
	var b strings.Builder
	b.WriteString("# Workspace Index — Summary\n\n")
	b.WriteString(fmt.Sprintf("*Generated: %s*\n\n", idx.Timestamp))

	// Stats table
	b.WriteString("## Stats\n\n")
	b.WriteString("| Metric | Count |\n|--------|-------|\n")
	b.WriteString(fmt.Sprintf("| Files indexed | %d |\n", idx.Stats.Files))
	b.WriteString(fmt.Sprintf("| Symbols found | %d |\n", idx.Stats.Symbols))
	b.WriteString(fmt.Sprintf("| Components | %d |\n", idx.Stats.Components))
	b.WriteString(fmt.Sprintf("| Import edges | %d |\n", idx.Stats.ImportEdges))
	b.WriteString(fmt.Sprintf("| Call edges | %d |\n", idx.Stats.CallEdges))
	b.WriteString(fmt.Sprintf("| Duration | %dms |\n", idx.Stats.DurationMs))
	if idx.GitCommit != "" {
		b.WriteString(fmt.Sprintf("| Git commit | %s |\n", idx.GitCommit))
	}
	b.WriteString("\n")

	// Languages table
	b.WriteString("## Languages\n\n")
	b.WriteString("| Language | Files |\n|----------|-------|\n")
	type langCount struct {
		lang  string
		count int
	}
	var langs []langCount
	for lang, count := range idx.Stats.Languages {
		langs = append(langs, langCount{lang, count})
	}
	sort.Slice(langs, func(i, j int) bool { return langs[i].count > langs[j].count })
	for _, lc := range langs {
		b.WriteString(fmt.Sprintf("| %s | %d |\n", lc.lang, lc.count))
	}
	b.WriteString("\n")

	// Components list
	b.WriteString("## Components\n\n")
	type compCount struct {
		comp  string
		count int
	}
	var comps []compCount
	for comp, files := range idx.ComponentMap {
		comps = append(comps, compCount{comp, len(files)})
	}
	sort.Slice(comps, func(i, j int) bool { return comps[i].count > comps[j].count })
	for _, cc := range comps {
		b.WriteString(fmt.Sprintf("- **%s/** — %d files\n", cc.comp, cc.count))
	}

	return os.WriteFile(filepath.Join(w.OutputDir, "SUMMARY.md"), []byte(b.String()), 0644)
}

func (w *MarkdownIndexWriter) writeClassMap(idx *WorkspaceIndex) error {
	var b strings.Builder
	b.WriteString("# Class Map — Workspace Component Registry\n\n")
	b.WriteString(fmt.Sprintf("*Generated: %s*\n", idx.Timestamp))
	b.WriteString(fmt.Sprintf("*Files indexed: %d | Symbols: %d*\n\n", idx.Stats.Files, idx.Stats.Symbols))

	var compNames []string
	for comp := range idx.ComponentMap {
		compNames = append(compNames, comp)
	}
	sort.Strings(compNames)

	// Build lookups
	fileByPath := make(map[string]*FileRecord)
	for i := range idx.Files {
		fileByPath[idx.Files[i].Path] = &idx.Files[i]
	}
	symsByFile := make(map[string][]Symbol)
	for _, sym := range idx.Symbols {
		symsByFile[sym.File] = append(symsByFile[sym.File], sym)
	}

	for _, comp := range compNames {
		files := idx.ComponentMap[comp]
		b.WriteString(fmt.Sprintf("## %s/\n\n", comp))

		for _, fpath := range files {
			fr := fileByPath[fpath]
			if fr == nil {
				continue
			}

			if fr.SymbolCount > 0 {
				b.WriteString(fmt.Sprintf("- **`%s`** (%s, %db, %d symbols)\n", fpath, fr.Language, fr.Size, fr.SymbolCount))
				// List code symbols (skip markdown headings, JSON/YAML keys)
				if syms, ok := symsByFile[fpath]; ok {
					for _, sym := range syms {
						if sym.Kind == "heading" || sym.Kind == "key" {
							continue
						}
						sigPart := ""
						if sym.Signature != "" {
							sigPart = " — `" + truncateSig(sym.Signature, 60) + "`"
						}
						scopePart := ""
						if sym.Scope != "" {
							scopePart = " (" + sym.Scope + ")"
						}
						b.WriteString(fmt.Sprintf("  - %s **%s**%s%s :%d\n",
							sym.Kind, sym.Name, scopePart, sigPart, sym.Line))
					}
				}
			} else {
				b.WriteString(fmt.Sprintf("- `%s` (%s, %db)\n", fpath, fr.Language, fr.Size))
			}
		}
		b.WriteString("\n")
	}

	return os.WriteFile(filepath.Join(w.OutputDir, "class-map.md"), []byte(b.String()), 0644)
}

func (w *MarkdownIndexWriter) writeDependencyMap(idx *WorkspaceIndex) error {
	if len(idx.Dependencies) == 0 {
		return nil
	}

	var b strings.Builder
	b.WriteString("# Dependency Map — Import Graph\n\n")
	b.WriteString(fmt.Sprintf("*Generated: %s*\n", idx.Timestamp))
	b.WriteString(fmt.Sprintf("*%d files with imports | %d total import edges*\n\n",
		len(idx.Dependencies), idx.Stats.ImportEdges))

	// Group by component
	fileByPath := make(map[string]*FileRecord)
	for i := range idx.Files {
		fileByPath[idx.Files[i].Path] = &idx.Files[i]
	}

	// Sort files
	var files []string
	for f := range idx.Dependencies {
		files = append(files, f)
	}
	sort.Strings(files)

	// Group by language
	byLang := make(map[string][]string)
	for _, f := range files {
		fr := fileByPath[f]
		if fr != nil {
			byLang[fr.Language] = append(byLang[fr.Language], f)
		}
	}

	var langs []string
	for l := range byLang {
		langs = append(langs, l)
	}
	sort.Strings(langs)

	for _, lang := range langs {
		b.WriteString(fmt.Sprintf("## %s\n\n", lang))
		langFiles := byLang[lang]
		for _, f := range langFiles {
			deps := idx.Dependencies[f]
			if len(deps) == 0 {
				continue
			}
			b.WriteString(fmt.Sprintf("### `%s`\n", f))
			sort.Strings(deps)
			for _, dep := range deps {
				b.WriteString(fmt.Sprintf("- %s\n", dep))
			}
			b.WriteString("\n")
		}
	}

	return os.WriteFile(filepath.Join(w.OutputDir, "dependency-map.md"), []byte(b.String()), 0644)
}

func (w *MarkdownIndexWriter) writeCallGraph(idx *WorkspaceIndex) error {
	if len(idx.CallGraph) == 0 {
		return nil
	}

	var b strings.Builder
	b.WriteString("# Call Graph — Function Relationships\n\n")
	b.WriteString(fmt.Sprintf("*Generated: %s*\n", idx.Timestamp))
	b.WriteString(fmt.Sprintf("*%d callers | %d total call edges*\n\n",
		len(idx.CallGraph), idx.Stats.CallEdges))

	// Sort callers
	var callers []string
	for caller := range idx.CallGraph {
		callers = append(callers, caller)
	}
	sort.Strings(callers)

	// Group by file (caller format is "file:FuncName")
	byFile := make(map[string][]string)
	for _, caller := range callers {
		file := caller
		if sepIdx := strings.LastIndex(caller, ":"); sepIdx > 0 {
			file = caller[:sepIdx]
		}
		byFile[file] = append(byFile[file], caller)
	}

	var files []string
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)

	for _, file := range files {
		b.WriteString(fmt.Sprintf("## `%s`\n\n", file))
		fileFuncs := byFile[file]
		for _, caller := range fileFuncs {
			callees := idx.CallGraph[caller]
			funcName := caller
			if sepIdx := strings.LastIndex(caller, ":"); sepIdx > 0 {
				funcName = caller[sepIdx+1:]
			}
			sort.Strings(callees)
			b.WriteString(fmt.Sprintf("- **%s** → %s\n", funcName, strings.Join(callees, ", ")))
		}
		b.WriteString("\n")
	}

	return os.WriteFile(filepath.Join(w.OutputDir, "call-graph.md"), []byte(b.String()), 0644)
}

// =============================================================================
// MARKDOWN DRIFT WRITER
// =============================================================================

// MarkdownDriftWriter writes drift report as markdown.
type MarkdownDriftWriter struct {
	OutputDir string
}

func (w *MarkdownDriftWriter) WriteDrift(report *DriftReport) error {
	if err := os.MkdirAll(w.OutputDir, 0755); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}

	var b strings.Builder
	b.WriteString("# Drift Report\n\n")
	b.WriteString(fmt.Sprintf("*Generated: %s*\n", report.Timestamp))
	if report.OldCommit != "" || report.NewCommit != "" {
		b.WriteString(fmt.Sprintf("*%s → %s*\n", report.OldCommit, report.NewCommit))
	}
	b.WriteString(fmt.Sprintf("\n**Summary:** %s\n\n", report.Summary))

	// Added files
	if len(report.AddedFiles) > 0 {
		b.WriteString(fmt.Sprintf("## Added Files (%d)\n\n", len(report.AddedFiles)))
		for _, f := range report.AddedFiles {
			b.WriteString(fmt.Sprintf("- `%s` (%s, %db)\n", f.Path, f.Language, f.Size))
		}
		b.WriteString("\n")
	}

	// Removed files
	if len(report.RemovedFiles) > 0 {
		b.WriteString(fmt.Sprintf("## Removed Files (%d)\n\n", len(report.RemovedFiles)))
		for _, f := range report.RemovedFiles {
			b.WriteString(fmt.Sprintf("- `%s` (%s, %db)\n", f.Path, f.Language, f.Size))
		}
		b.WriteString("\n")
	}

	// Modified files
	if len(report.ModifiedFiles) > 0 {
		b.WriteString(fmt.Sprintf("## Modified Files (%d)\n\n", len(report.ModifiedFiles)))
		b.WriteString("| File | Old Symbols | New Symbols |\n|------|------------|-------------|\n")
		for _, f := range report.ModifiedFiles {
			b.WriteString(fmt.Sprintf("| `%s` | %d | %d |\n", f.Path, f.OldSymbols, f.NewSymbols))
		}
		b.WriteString("\n")
	}

	// Added symbols
	if len(report.AddedSymbols) > 0 {
		b.WriteString(fmt.Sprintf("## Added Symbols (%d)\n\n", len(report.AddedSymbols)))
		for _, s := range report.AddedSymbols {
			b.WriteString(fmt.Sprintf("- %s **%s** in `%s:%d`\n", s.Kind, s.Name, s.File, s.Line))
		}
		b.WriteString("\n")
	}

	// Removed symbols
	if len(report.RemovedSymbols) > 0 {
		b.WriteString(fmt.Sprintf("## Removed Symbols (%d)\n\n", len(report.RemovedSymbols)))
		for _, s := range report.RemovedSymbols {
			b.WriteString(fmt.Sprintf("- %s **%s** in `%s:%d`\n", s.Kind, s.Name, s.File, s.Line))
		}
		b.WriteString("\n")
	}

	// Moved symbols
	if len(report.MovedSymbols) > 0 {
		b.WriteString(fmt.Sprintf("## Moved/Renamed Symbols (%d)\n\n", len(report.MovedSymbols)))
		for _, m := range report.MovedSymbols {
			b.WriteString(fmt.Sprintf("- %s **%s** (`%s`) → **%s** (`%s`) — %.0f%% match\n",
				m.Kind, m.OldName, m.OldFile, m.NewName, m.NewFile, m.Score*100))
		}
		b.WriteString("\n")
	}

	// Dependency changes
	if len(report.DepChanges) > 0 {
		b.WriteString(fmt.Sprintf("## Import Changes (%d files)\n\n", len(report.DepChanges)))
		for _, dc := range report.DepChanges {
			b.WriteString(fmt.Sprintf("### `%s`\n", dc.File))
			for _, a := range dc.Added {
				b.WriteString(fmt.Sprintf("- + %s\n", a))
			}
			for _, r := range dc.Removed {
				b.WriteString(fmt.Sprintf("- - %s\n", r))
			}
			b.WriteString("\n")
		}
	}

	// New components
	if len(report.ComponentDelta) > 0 {
		b.WriteString(fmt.Sprintf("## New Components (%d)\n\n", len(report.ComponentDelta)))
		for _, c := range report.ComponentDelta {
			b.WriteString(fmt.Sprintf("- **%s/**\n", c))
		}
		b.WriteString("\n")
	}

	return os.WriteFile(filepath.Join(w.OutputDir, "drift-report.md"), []byte(b.String()), 0644)
}

// =============================================================================
// HELPERS
// =============================================================================

func truncateSig(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
