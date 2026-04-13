package main

import (
	"os"
	"path/filepath"
	"testing"
)

// =============================================================================
// LANGUAGE DETECTION
// =============================================================================

func TestDetectLanguage(t *testing.T) {
	tests := []struct {
		path    string
		content string
		want    string
	}{
		{"main.go", "", "go"},
		{"app.ts", "", "typescript"},
		{"index.js", "", "javascript"},
		{"script.py", "", "python"},
		{"run.sh", "", "shell"},
		{"config.yaml", "", "yaml"},
		{"data.json", "", "json"},
		{"README.md", "", "markdown"},
		{"unknown.xyz", "", ""},
		{"no-ext", "#!/usr/bin/env python3\nprint('hi')", "python"},
		{"no-ext", "#!/bin/bash\necho hi", "shell"},
		{"no-ext", "#!/usr/bin/env node\nconsole.log('hi')", "javascript"},
	}

	for _, tc := range tests {
		got := detectLanguage(tc.path, []byte(tc.content))
		if got != tc.want {
			t.Errorf("detectLanguage(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// =============================================================================
// COMPONENT CLASSIFICATION
// =============================================================================

func TestClassifyComponent(t *testing.T) {
	cfg := defaultComponentConfig()

	tests := []struct {
		relPath string
		want    string
	}{
		{".cog/serve.go", "kernel"},
		{".cog/bus_session.go", "bus"},
		{".cog/mem/working.cog.md", "memory"},
		{".cog/run/index/current.json", "runtime"},
		{".cog/hooks/pre-compact.py", "hooks"},
		{".claude/skills/bus-bridge/SKILL.md", "skills/bus-bridge"},
		{"docs/specs/indexer.md", "docs/specs"},
		{"apps/cogos/cog.go", "cogos"},
		{"build-tools/indexer/index.py", "tooling"},
		{".openclaw/extensions/cogos/index.ts", "gateway"},
		{"README.md", "root"},
		{"some-random-file.go", "root"},
	}

	for _, tc := range tests {
		got := classifyComponent(tc.relPath, cfg)
		if got != tc.want {
			t.Errorf("classifyComponent(%q) = %q, want %q", tc.relPath, got, tc.want)
		}
	}
}

func TestClassifyComponentExclusions(t *testing.T) {
	cfg := defaultComponentConfig()

	// .cog/mem/ should be "memory", not "kernel" (exclusion rule)
	got := classifyComponent(".cog/mem/semantic/insight.md", cfg)
	if got != "memory" {
		t.Errorf("classifyComponent(.cog/mem/...) = %q, want %q", got, "memory")
	}

	// .cog/run/ should be "runtime", not "kernel"
	got = classifyComponent(".cog/run/signals/field.json", cfg)
	if got != "runtime" {
		t.Errorf("classifyComponent(.cog/run/...) = %q, want %q", got, "runtime")
	}
}

// =============================================================================
// SCANNER
// =============================================================================

func TestScanWorkspaceSmall(t *testing.T) {
	// Create a temp workspace with a few files
	dir := t.TempDir()

	// Create a Go file
	goDir := filepath.Join(dir, "pkg")
	os.MkdirAll(goDir, 0755)
	os.WriteFile(filepath.Join(goDir, "main.go"), []byte(`package main

func Hello() string {
	return "hello"
}

func Goodbye() string {
	return "bye"
}
`), 0644)

	// Create a Python file
	os.WriteFile(filepath.Join(dir, "script.py"), []byte(`
def greet(name):
    return f"hello {name}"

class Greeter:
    def __init__(self):
        self.name = "world"
`), 0644)

	// Create an ignored directory
	os.MkdirAll(filepath.Join(dir, "node_modules", "pkg"), 0755)
	os.WriteFile(filepath.Join(dir, "node_modules", "pkg", "index.js"), []byte(`module.exports = {}`), 0644)

	idx, err := ScanWorkspace(ScanOptions{Root: dir})
	if err != nil {
		t.Fatalf("ScanWorkspace: %v", err)
	}

	if idx.Stats.Files != 2 {
		t.Errorf("Files = %d, want 2", idx.Stats.Files)
	}

	// Should have Go and Python
	if idx.Stats.Languages["go"] != 1 {
		t.Errorf("Go files = %d, want 1", idx.Stats.Languages["go"])
	}
	if idx.Stats.Languages["python"] != 1 {
		t.Errorf("Python files = %d, want 1", idx.Stats.Languages["python"])
	}

	// Should have symbols (exact count depends on parser)
	if idx.Stats.Symbols == 0 {
		t.Error("Symbols = 0, want > 0")
	}

	// node_modules should be ignored
	for _, f := range idx.Files {
		if filepath.Base(filepath.Dir(f.Path)) == "node_modules" {
			t.Errorf("node_modules file found in index: %s", f.Path)
		}
	}
}

func TestScanWorkspaceLanguageFilter(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main
func Main() {}
`), 0644)
	os.WriteFile(filepath.Join(dir, "app.py"), []byte(`def main(): pass
`), 0644)

	idx, err := ScanWorkspace(ScanOptions{Root: dir, Language: "go"})
	if err != nil {
		t.Fatalf("ScanWorkspace: %v", err)
	}

	if idx.Stats.Files != 1 {
		t.Errorf("Files = %d, want 1 (Go only)", idx.Stats.Files)
	}
	if idx.Files[0].Language != "go" {
		t.Errorf("Language = %q, want %q", idx.Files[0].Language, "go")
	}
}

// =============================================================================
// DRIFT DETECTION
// =============================================================================

func TestComputeDriftAddedFiles(t *testing.T) {
	old := &WorkspaceIndex{
		Files:        []FileRecord{{Path: "a.go", Hash: "abc", Language: "go"}},
		Symbols:      []Symbol{},
		Dependencies: map[string][]string{},
		ComponentMap: map[string][]string{"root": {"a.go"}},
	}
	new := &WorkspaceIndex{
		Files: []FileRecord{
			{Path: "a.go", Hash: "abc", Language: "go"},
			{Path: "b.go", Hash: "def", Language: "go"},
		},
		Symbols:      []Symbol{},
		Dependencies: map[string][]string{},
		ComponentMap: map[string][]string{"root": {"a.go", "b.go"}},
	}

	report := computeDrift(old, new)

	if len(report.AddedFiles) != 1 {
		t.Fatalf("AddedFiles = %d, want 1", len(report.AddedFiles))
	}
	if report.AddedFiles[0].Path != "b.go" {
		t.Errorf("AddedFile path = %q, want %q", report.AddedFiles[0].Path, "b.go")
	}
}

func TestComputeDriftRemovedFiles(t *testing.T) {
	old := &WorkspaceIndex{
		Files: []FileRecord{
			{Path: "a.go", Hash: "abc", Language: "go"},
			{Path: "b.go", Hash: "def", Language: "go"},
		},
		Symbols:      []Symbol{},
		Dependencies: map[string][]string{},
		ComponentMap: map[string][]string{},
	}
	new := &WorkspaceIndex{
		Files:        []FileRecord{{Path: "a.go", Hash: "abc", Language: "go"}},
		Symbols:      []Symbol{},
		Dependencies: map[string][]string{},
		ComponentMap: map[string][]string{},
	}

	report := computeDrift(old, new)

	if len(report.RemovedFiles) != 1 {
		t.Fatalf("RemovedFiles = %d, want 1", len(report.RemovedFiles))
	}
	if report.RemovedFiles[0].Path != "b.go" {
		t.Errorf("RemovedFile path = %q, want %q", report.RemovedFiles[0].Path, "b.go")
	}
}

func TestComputeDriftModifiedFiles(t *testing.T) {
	old := &WorkspaceIndex{
		Files:        []FileRecord{{Path: "a.go", Hash: "abc", Language: "go", SymbolCount: 3}},
		Symbols:      []Symbol{},
		Dependencies: map[string][]string{},
		ComponentMap: map[string][]string{},
	}
	new := &WorkspaceIndex{
		Files:        []FileRecord{{Path: "a.go", Hash: "xyz", Language: "go", SymbolCount: 5}},
		Symbols:      []Symbol{},
		Dependencies: map[string][]string{},
		ComponentMap: map[string][]string{},
	}

	report := computeDrift(old, new)

	if len(report.ModifiedFiles) != 1 {
		t.Fatalf("ModifiedFiles = %d, want 1", len(report.ModifiedFiles))
	}
	if report.ModifiedFiles[0].OldHash != "abc" || report.ModifiedFiles[0].NewHash != "xyz" {
		t.Errorf("ModifiedFile hashes = %q→%q, want abc→xyz",
			report.ModifiedFiles[0].OldHash, report.ModifiedFiles[0].NewHash)
	}
}

func TestComputeDriftSymbols(t *testing.T) {
	old := &WorkspaceIndex{
		Files: []FileRecord{{Path: "a.go", Hash: "abc"}},
		Symbols: []Symbol{
			{Name: "Foo", Kind: "function", File: "a.go", Line: 1},
			{Name: "Bar", Kind: "function", File: "a.go", Line: 10},
		},
		Dependencies: map[string][]string{},
		ComponentMap: map[string][]string{},
	}
	new := &WorkspaceIndex{
		Files: []FileRecord{{Path: "a.go", Hash: "def"}},
		Symbols: []Symbol{
			{Name: "Foo", Kind: "function", File: "a.go", Line: 1},
			{Name: "Baz", Kind: "function", File: "a.go", Line: 10},
		},
		Dependencies: map[string][]string{},
		ComponentMap: map[string][]string{},
	}

	report := computeDrift(old, new)

	// Bar→Baz is detected as a move (name similarity > 0.6, same kind)
	// so they show up as MovedSymbols, not Added/Removed
	totalChanges := len(report.RemovedSymbols) + len(report.AddedSymbols) + len(report.MovedSymbols)
	if totalChanges == 0 {
		t.Error("expected symbol changes (add/remove/move), got none")
	}
	if len(report.MovedSymbols) == 1 {
		if report.MovedSymbols[0].OldName != "Bar" || report.MovedSymbols[0].NewName != "Baz" {
			t.Errorf("move = %q→%q, want Bar→Baz",
				report.MovedSymbols[0].OldName, report.MovedSymbols[0].NewName)
		}
	}
}

func TestNameSimilarity(t *testing.T) {
	tests := []struct {
		a, b     string
		minScore float64
	}{
		{"Hello", "Hello", 1.0},
		{"HandleRequest", "HandleResponse", 0.6},
		{"Foo", "Bar", 0.0},
		{"processData", "processItems", 0.5},
	}

	for _, tc := range tests {
		score := nameSimilarity(tc.a, tc.b)
		if score < tc.minScore {
			t.Errorf("nameSimilarity(%q, %q) = %.2f, want >= %.2f", tc.a, tc.b, score, tc.minScore)
		}
	}
}

func TestDetectMoves(t *testing.T) {
	removed := []Symbol{
		{Name: "ProcessRequest", Kind: "function", File: "old.go", Line: 10},
	}
	added := []Symbol{
		{Name: "ProcessResponse", Kind: "function", File: "new.go", Line: 20},
		{Name: "CompletelyDifferent", Kind: "function", File: "other.go", Line: 5},
	}

	moves := detectMoves(removed, added)

	// ProcessRequest → ProcessResponse should match (high LCS similarity, same kind)
	if len(moves) != 1 {
		t.Fatalf("moves = %d, want 1", len(moves))
	}
	if moves[0].OldName != "ProcessRequest" || moves[0].NewName != "ProcessResponse" {
		t.Errorf("move = %q→%q, want ProcessRequest→ProcessResponse",
			moves[0].OldName, moves[0].NewName)
	}
}

func TestGenerateDriftSummary(t *testing.T) {
	report := &DriftReport{
		AddedFiles:    []FileRecord{{Path: "a.go"}, {Path: "b.go"}},
		RemovedFiles:  []FileRecord{{Path: "c.go"}},
		ModifiedFiles: []FileDiff{{Path: "d.go"}},
		AddedSymbols:  []Symbol{{Name: "Foo"}},
	}

	summary := generateDriftSummary(report)
	if summary == "" {
		t.Error("summary is empty")
	}
	// Should mention file counts
	if len(summary) < 10 {
		t.Errorf("summary too short: %q", summary)
	}
}

// =============================================================================
// PARSERS
// =============================================================================

func TestGoParserExtractsSymbols(t *testing.T) {
	registry := defaultRegistry()
	parser := registry.ForLanguage("go")
	if parser == nil {
		t.Fatal("no Go parser registered")
	}

	content := []byte(`package main

import "fmt"

type Server struct {
	port int
}

func NewServer(port int) *Server {
	return &Server{port: port}
}

func (s *Server) Start() error {
	fmt.Println("starting")
	return nil
}

const Version = "1.0"
`)

	rec, syms, err := parser.Parse("test.go", content)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if rec == nil {
		t.Fatal("FileRecord is nil")
	}

	// Should find: Server (type), NewServer (function), Start (method), Version (const)
	if len(syms) < 3 {
		t.Errorf("symbols = %d, want >= 3", len(syms))
	}

	// Check imports
	if len(rec.Imports) == 0 {
		t.Error("imports = 0, want >= 1 (fmt)")
	}
}

func TestPythonParserExtractsSymbols(t *testing.T) {
	registry := defaultRegistry()
	parser := registry.ForLanguage("python")
	if parser == nil {
		t.Fatal("no Python parser registered")
	}

	content := []byte(`import os
from pathlib import Path

class FileManager:
    def __init__(self, root):
        self.root = root

    def list_files(self):
        return os.listdir(self.root)

def helper():
    pass
`)

	rec, syms, err := parser.Parse("test.py", content)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if rec == nil {
		t.Fatal("FileRecord is nil")
	}

	// Should find: FileManager (class), __init__ (method), list_files (method), helper (function)
	if len(syms) < 3 {
		t.Errorf("symbols = %d, want >= 3", len(syms))
	}

	if len(rec.Imports) < 2 {
		t.Errorf("imports = %d, want >= 2", len(rec.Imports))
	}
}

func TestCallGraphExtraction(t *testing.T) {
	registry := defaultRegistry()
	parser := registry.ForLanguage("go")
	if parser == nil {
		t.Fatal("no Go parser registered")
	}

	cgp, ok := parser.(CallGraphParser)
	if !ok {
		t.Fatal("Go parser does not implement CallGraphParser")
	}

	content := []byte(`package main

import "fmt"

func Hello() {
	fmt.Println("hello")
	Goodbye()
}

func Goodbye() {
	fmt.Println("bye")
}
`)

	calls := cgp.ExtractCalls("test.go", content)

	// Hello should call fmt.Println and Goodbye
	helloCalls := calls["Hello"]
	if len(helloCalls) < 2 {
		t.Errorf("Hello calls = %d, want >= 2", len(helloCalls))
	}
}

// =============================================================================
// OUTPUT WRITERS
// =============================================================================

func TestJSONIndexWriter(t *testing.T) {
	dir := t.TempDir()
	writer := &JSONIndexWriter{OutputDir: dir}

	idx := &WorkspaceIndex{
		Version:      1,
		Timestamp:    "2026-03-09T00:00:00Z",
		Stats:        IndexStats{Files: 5, Languages: map[string]int{"go": 5}},
		Files:        []FileRecord{{Path: "main.go", Language: "go", Hash: "abc"}},
		Symbols:      []Symbol{},
		ComponentMap: map[string][]string{},
		Dependencies: map[string][]string{},
		CallGraph:    map[string][]string{},
	}

	if err := writer.WriteIndex(idx); err != nil {
		t.Fatalf("WriteIndex: %v", err)
	}

	outPath := filepath.Join(dir, "current.json")
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if len(data) == 0 {
		t.Error("output file is empty")
	}
}

func TestMarkdownIndexWriter(t *testing.T) {
	dir := t.TempDir()
	writer := &MarkdownIndexWriter{OutputDir: dir}

	idx := &WorkspaceIndex{
		Version:   1,
		Timestamp: "2026-03-09T00:00:00Z",
		Stats:     IndexStats{Files: 2, Symbols: 5, Components: 1, Languages: map[string]int{"go": 2}},
		Files: []FileRecord{
			{Path: "main.go", Language: "go", Hash: "abc", Size: 100, SymbolCount: 3},
			{Path: "util.go", Language: "go", Hash: "def", Size: 50, SymbolCount: 2},
		},
		Symbols: []Symbol{
			{Name: "Main", Kind: "function", File: "main.go", Line: 5},
		},
		ComponentMap: map[string][]string{"root": {"main.go", "util.go"}},
		Dependencies: map[string][]string{"main.go": {"fmt", "os"}},
		CallGraph:    map[string][]string{"main.go:Main": {"fmt.Println", "helper"}},
	}

	if err := writer.WriteIndex(idx); err != nil {
		t.Fatalf("WriteIndex: %v", err)
	}

	// Check all expected files exist
	for _, name := range []string{"SUMMARY.md", "class-map.md", "dependency-map.md", "call-graph.md"} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("expected output file %s not created", name)
		}
	}
}

func TestMarkdownDriftWriter(t *testing.T) {
	dir := t.TempDir()
	writer := &MarkdownDriftWriter{OutputDir: dir}

	report := &DriftReport{
		Timestamp:    "2026-03-09T00:00:00Z",
		AddedFiles:   []FileRecord{{Path: "new.go", Language: "go", Size: 100}},
		RemovedFiles: []FileRecord{},
		ModifiedFiles: []FileDiff{
			{Path: "changed.go", OldHash: "abc", NewHash: "def", OldSymbols: 3, NewSymbols: 5},
		},
		AddedSymbols:   []Symbol{{Name: "Foo", Kind: "function", File: "new.go", Line: 1}},
		RemovedSymbols: []Symbol{},
		MovedSymbols:   []SymbolMove{},
		DepChanges:     []DepChange{},
		Summary:        "1 file added, 1 modified",
	}

	if err := writer.WriteDrift(report); err != nil {
		t.Fatalf("WriteDrift: %v", err)
	}

	path := filepath.Join(dir, "drift-report.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	content := string(data)
	if len(content) < 50 {
		t.Errorf("drift report too short: %d bytes", len(content))
	}
}

// =============================================================================
// HELPERS
// =============================================================================

func TestTruncateSig(t *testing.T) {
	tests := []struct {
		input  string
		maxLen int
		want   string
	}{
		{"short", 10, "short"},
		{"this is a long signature", 10, "this is a ..."},
		{"exact", 5, "exact"},
	}

	for _, tc := range tests {
		got := truncateSig(tc.input, tc.maxLen)
		if got != tc.want {
			t.Errorf("truncateSig(%q, %d) = %q, want %q", tc.input, tc.maxLen, got, tc.want)
		}
	}
}

func TestExtractGoPackage(t *testing.T) {
	tests := []struct {
		content string
		want    string
	}{
		{"package main\n\nfunc Hello() {}", "main"},
		{"// comment\npackage util\n", "util"},
		{"no package here", ""},
	}

	for _, tc := range tests {
		got := extractGoPackage([]byte(tc.content))
		if got != tc.want {
			t.Errorf("extractGoPackage(%q) = %q, want %q", tc.content, got, tc.want)
		}
	}
}
