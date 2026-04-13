package main

// index_drift.go — Drift detection between workspace index snapshots.
//
// Phase 4: Loads a stored baseline index, computes structural diff against
// a fresh scan, and produces a DriftReport with file/symbol/dependency changes.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// =============================================================================
// BASELINE LOADING
// =============================================================================

// loadBaseline reads a previously saved WorkspaceIndex from a JSON file.
func loadBaseline(path string) (*WorkspaceIndex, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading baseline: %w", err)
	}
	var idx WorkspaceIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("parsing baseline: %w", err)
	}
	return &idx, nil
}

// =============================================================================
// DRIFT COMPUTATION
// =============================================================================

// computeDrift produces a DriftReport by comparing two workspace indexes.
func computeDrift(old, new *WorkspaceIndex) *DriftReport {
	report := &DriftReport{
		OldCommit:      old.GitCommit,
		NewCommit:      new.GitCommit,
		Timestamp:      new.Timestamp,
		AddedFiles:     []FileRecord{},
		RemovedFiles:   []FileRecord{},
		ModifiedFiles:  []FileDiff{},
		AddedSymbols:   []Symbol{},
		RemovedSymbols: []Symbol{},
		MovedSymbols:   []SymbolMove{},
		DepChanges:     []DepChange{},
		ComponentDelta: []string{},
	}

	// Build file lookup maps
	oldFiles := make(map[string]*FileRecord)
	for i := range old.Files {
		oldFiles[old.Files[i].Path] = &old.Files[i]
	}
	newFiles := make(map[string]*FileRecord)
	for i := range new.Files {
		newFiles[new.Files[i].Path] = &new.Files[i]
	}

	// File diffs: added, removed, modified
	for path, newFile := range newFiles {
		if oldFile, exists := oldFiles[path]; !exists {
			report.AddedFiles = append(report.AddedFiles, *newFile)
		} else if oldFile.Hash != newFile.Hash {
			report.ModifiedFiles = append(report.ModifiedFiles, FileDiff{
				Path:       path,
				OldHash:    oldFile.Hash,
				NewHash:    newFile.Hash,
				OldSymbols: oldFile.SymbolCount,
				NewSymbols: newFile.SymbolCount,
			})
		}
	}
	for path, oldFile := range oldFiles {
		if _, exists := newFiles[path]; !exists {
			report.RemovedFiles = append(report.RemovedFiles, *oldFile)
		}
	}

	// Sort file diffs for deterministic output
	sort.Slice(report.AddedFiles, func(i, j int) bool { return report.AddedFiles[i].Path < report.AddedFiles[j].Path })
	sort.Slice(report.RemovedFiles, func(i, j int) bool { return report.RemovedFiles[i].Path < report.RemovedFiles[j].Path })
	sort.Slice(report.ModifiedFiles, func(i, j int) bool { return report.ModifiedFiles[i].Path < report.ModifiedFiles[j].Path })

	// Symbol diffs (key includes Scope to distinguish same-name methods on different types)
	symbolKey := func(s Symbol) string {
		return s.File + ":" + s.Scope + ":" + s.Name + ":" + s.Kind
	}
	oldSyms := make(map[string]Symbol)
	for _, s := range old.Symbols {
		oldSyms[symbolKey(s)] = s
	}
	newSyms := make(map[string]Symbol)
	for _, s := range new.Symbols {
		newSyms[symbolKey(s)] = s
	}

	var rawAdded, rawRemoved []Symbol
	for key, s := range newSyms {
		if _, exists := oldSyms[key]; !exists {
			rawAdded = append(rawAdded, s)
		}
	}
	for key, s := range oldSyms {
		if _, exists := newSyms[key]; !exists {
			rawRemoved = append(rawRemoved, s)
		}
	}

	// Sort for deterministic output and move detection
	sort.Slice(rawRemoved, func(i, j int) bool { return symbolKey(rawRemoved[i]) < symbolKey(rawRemoved[j]) })
	sort.Slice(rawAdded, func(i, j int) bool { return symbolKey(rawAdded[i]) < symbolKey(rawAdded[j]) })

	// Detect moves (fuzzy match between removed and added)
	report.MovedSymbols = detectMoves(rawRemoved, rawAdded)

	// Filter out moved symbols from added/removed
	movedOld := make(map[string]bool)
	movedNew := make(map[string]bool)
	for _, m := range report.MovedSymbols {
		movedOld[m.OldFile+":"+m.OldName+":"+m.Kind] = true
		movedNew[m.NewFile+":"+m.NewName+":"+m.Kind] = true
	}
	for _, s := range rawAdded {
		key := s.File + ":" + s.Name + ":" + s.Kind
		if !movedNew[key] {
			report.AddedSymbols = append(report.AddedSymbols, s)
		}
	}
	for _, s := range rawRemoved {
		key := s.File + ":" + s.Name + ":" + s.Kind
		if !movedOld[key] {
			report.RemovedSymbols = append(report.RemovedSymbols, s)
		}
	}

	// Dependency changes
	report.DepChanges = computeDepChanges(old.Dependencies, new.Dependencies)

	// Component delta (new components)
	oldComps := make(map[string]bool)
	for comp := range old.ComponentMap {
		oldComps[comp] = true
	}
	for comp := range new.ComponentMap {
		if !oldComps[comp] {
			report.ComponentDelta = append(report.ComponentDelta, comp)
		}
	}
	sort.Strings(report.ComponentDelta)

	// Generate summary
	report.Summary = generateDriftSummary(report)

	return report
}

// =============================================================================
// DEPENDENCY CHANGES
// =============================================================================

// computeDepChanges finds added/removed imports per file.
func computeDepChanges(oldDeps, newDeps map[string][]string) []DepChange {
	var changes []DepChange

	allFiles := make(map[string]bool)
	for f := range oldDeps {
		allFiles[f] = true
	}
	for f := range newDeps {
		allFiles[f] = true
	}

	for file := range allFiles {
		oldSet := toSet(oldDeps[file])
		newSet := toSet(newDeps[file])

		var added, removed []string
		for dep := range newSet {
			if !oldSet[dep] {
				added = append(added, dep)
			}
		}
		for dep := range oldSet {
			if !newSet[dep] {
				removed = append(removed, dep)
			}
		}

		if len(added) > 0 || len(removed) > 0 {
			sort.Strings(added)
			sort.Strings(removed)
			changes = append(changes, DepChange{
				File:    file,
				Added:   added,
				Removed: removed,
			})
		}
	}

	sort.Slice(changes, func(i, j int) bool {
		return changes[i].File < changes[j].File
	})
	return changes
}

func toSet(items []string) map[string]bool {
	s := make(map[string]bool, len(items))
	for _, item := range items {
		s[item] = true
	}
	return s
}

// =============================================================================
// SYMBOL MOVE DETECTION
// =============================================================================

// detectMoves finds symbols that were likely renamed or moved between files.
// Uses name similarity (LCS ratio) with same-kind constraint.
func detectMoves(removed, added []Symbol) []SymbolMove {
	if len(removed) == 0 || len(added) == 0 {
		return nil
	}

	// Only consider code symbols (not headings, keys)
	codeKinds := map[string]bool{
		"function": true, "method": true, "class": true,
		"interface": true, "type": true, "variable": true,
	}

	var moves []SymbolMove
	usedAdded := make(map[int]bool)

	for _, rem := range removed {
		if !codeKinds[rem.Kind] {
			continue
		}
		bestScore := 0.0
		bestIdx := -1

		for j, add := range added {
			if usedAdded[j] || add.Kind != rem.Kind || add.Language != rem.Language {
				continue
			}

			score := nameSimilarity(rem.Name, add.Name)

			// Boost score if files are in the same directory
			if sameDir(rem.File, add.File) {
				score = score*0.8 + 0.2
			}

			if score > bestScore {
				bestScore = score
				bestIdx = j
			}
		}

		if bestScore >= 0.6 && bestIdx >= 0 {
			usedAdded[bestIdx] = true
			moves = append(moves, SymbolMove{
				OldName: rem.Name,
				NewName: added[bestIdx].Name,
				OldFile: rem.File,
				NewFile: added[bestIdx].File,
				Kind:    rem.Kind,
				Score:   bestScore,
			})
		}
	}

	return moves
}

// nameSimilarity computes similarity between two names (0-1) using LCS ratio.
func nameSimilarity(a, b string) float64 {
	if a == b {
		return 1.0
	}
	if len(a) == 0 || len(b) == 0 {
		return 0.0
	}
	lcs := lcsLength(strings.ToLower(a), strings.ToLower(b))
	return float64(lcs) / float64(max(len(a), len(b)))
}

// lcsLength computes the longest common subsequence length (space-optimized).
func lcsLength(a, b string) int {
	m, n := len(a), len(b)
	prev := make([]int, n+1)
	curr := make([]int, n+1)
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if a[i-1] == b[j-1] {
				curr[j] = prev[j-1] + 1
			} else {
				curr[j] = max(prev[j], curr[j-1])
			}
		}
		prev, curr = curr, prev
		clear(curr)
	}
	return prev[n]
}

func sameDir(a, b string) bool {
	aDir := ""
	if idx := strings.LastIndex(a, "/"); idx >= 0 {
		aDir = a[:idx]
	}
	bDir := ""
	if idx := strings.LastIndex(b, "/"); idx >= 0 {
		bDir = b[:idx]
	}
	return aDir == bDir
}

// =============================================================================
// SUMMARY GENERATION
// =============================================================================

// generateDriftSummary creates a human-readable summary of structural changes.
func generateDriftSummary(report *DriftReport) string {
	var parts []string
	if len(report.AddedFiles) > 0 {
		parts = append(parts, fmt.Sprintf("%d files added", len(report.AddedFiles)))
	}
	if len(report.RemovedFiles) > 0 {
		parts = append(parts, fmt.Sprintf("%d files removed", len(report.RemovedFiles)))
	}
	if len(report.ModifiedFiles) > 0 {
		parts = append(parts, fmt.Sprintf("%d files modified", len(report.ModifiedFiles)))
	}
	if len(report.AddedSymbols) > 0 {
		parts = append(parts, fmt.Sprintf("%d symbols added", len(report.AddedSymbols)))
	}
	if len(report.RemovedSymbols) > 0 {
		parts = append(parts, fmt.Sprintf("%d symbols removed", len(report.RemovedSymbols)))
	}
	if len(report.MovedSymbols) > 0 {
		parts = append(parts, fmt.Sprintf("%d symbols moved/renamed", len(report.MovedSymbols)))
	}
	if len(report.DepChanges) > 0 {
		parts = append(parts, fmt.Sprintf("%d files with import changes", len(report.DepChanges)))
	}
	if len(report.ComponentDelta) > 0 {
		parts = append(parts, fmt.Sprintf("new components: %s", strings.Join(report.ComponentDelta, ", ")))
	}
	if len(parts) == 0 {
		return "No structural drift detected."
	}
	return strings.Join(parts, "; ") + "."
}
