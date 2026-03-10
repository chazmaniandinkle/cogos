package main

// index_scanner.go — Workspace file scanner and component mapper for cogos index.
//
// Phase 1: Walks workspace, classifies files by language and component,
// computes hashes, and builds the file-level WorkspaceIndex.

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// =============================================================================
// IGNORE PATTERNS
// =============================================================================

// defaultIgnoreDirs are directories skipped during workspace scanning.
var defaultIgnoreDirs = map[string]bool{
	".git":            true,
	"node_modules":    true,
	"__pycache__":     true,
	".DS_Store":       true,
	"dist":            true,
	"vendor":          true,
	".venv":           true,
	"venv":            true,
	".next":           true,
	".nuxt":           true,
	".turbo":          true,
	".cache":          true,
	".knownPackages":  true,
	"platforms":       true,
	"licenses":        true,
}

// defaultIgnoreFiles are filenames skipped during workspace scanning.
var defaultIgnoreFiles = map[string]bool{
	"package-lock.json": true,
	".DS_Store":         true,
	"yarn.lock":         true,
	"pnpm-lock.yaml":   true,
	"bun.lockb":        true,
	"go.sum":           true,
}

// =============================================================================
// LANGUAGE DETECTION
// =============================================================================

// langExtMap maps file extensions to language names.
var langExtMap = map[string]string{
	".go":   "go",
	".py":   "python",
	".js":   "javascript",
	".jsx":  "javascript",
	".mjs":  "javascript",
	".cjs":  "javascript",
	".ts":   "typescript",
	".tsx":  "typescript",
	".sh":   "shell",
	".bash": "shell",
	".zsh":  "shell",
	".json": "json",
	".md":   "markdown",
	".yaml": "yaml",
	".yml":  "yaml",
	".toml": "toml",
	".cs":   "csharp",
}

// shebangs maps shebang fragments to language names.
var shebangs = map[string]string{
	"python": "python",
	"node":   "javascript",
	"bun":    "javascript",
	"bash":   "shell",
	"zsh":    "shell",
	"/sh":    "shell",
}

// detectLanguage determines the language of a file from its extension or shebang.
func detectLanguage(path string, content []byte) string {
	ext := filepath.Ext(path)
	if lang, ok := langExtMap[ext]; ok {
		return lang
	}

	// Check shebang (first line)
	if len(content) > 2 && content[0] == '#' && content[1] == '!' {
		firstLine := string(content[:min(256, len(content))])
		if nl := strings.IndexByte(firstLine, '\n'); nl > 0 {
			firstLine = firstLine[:nl]
		}
		for fragment, lang := range shebangs {
			if strings.Contains(firstLine, fragment) {
				return lang
			}
		}
	}

	return ""
}

// =============================================================================
// COMPONENT MAP
// =============================================================================

// ComponentConfig defines the component classification rules.
type ComponentConfig struct {
	Components map[string]ComponentRule `yaml:"components"`
	Ignore     []string                `yaml:"ignore"`
	IgnorePaths []string               `yaml:"ignore_paths"` // path prefixes to skip entirely
}

// ComponentRule defines path matching for a component.
type ComponentRule struct {
	Paths    []string `yaml:"paths"`
	Exclude  []string `yaml:"exclude,omitempty"`
	Children bool     `yaml:"children,omitempty"` // auto-discover sub-components
}

// defaultComponentConfig returns the built-in component map configuration.
func defaultComponentConfig() *ComponentConfig {
	return &ComponentConfig{
		Components: map[string]ComponentRule{
			"kernel": {
				Paths:   []string{".cog/"},
				Exclude: []string{".cog/mem/", ".cog/run/", ".cog/bus-", ".cog/.state/"},
			},
			"bus": {
				Paths: []string{".cog/bus_", ".cog/bus-", ".cog/.state/buses"},
			},
			"memory": {
				Paths: []string{".cog/mem/"},
			},
			"runtime": {
				Paths: []string{".cog/run/"},
			},
			"hooks": {
				Paths: []string{".cog/hooks/", "hooks/"},
			},
			"skills": {
				Paths:    []string{".claude/skills/", "skills/"},
				Children: true,
			},
			"docs": {
				Paths:    []string{"docs/"},
				Children: true,
			},
			"tooling": {
				Paths: []string{"build-tools/"},
			},
			"gateway": {
				Paths: []string{".openclaw/", "apps/moltbot-gateway/"},
			},
			"cogos": {
				Paths: []string{"apps/cogos/"},
			},
		},
		Ignore: []string{
			"node_modules/",
			".git/",
			"*.pyc",
			"__pycache__/",
			".DS_Store",
		},
		IgnorePaths: []string{
			"research/",
			"reference/",
			"external/",
			"archive/",
			"k8s/",
		},
	}
}

// loadComponentConfig loads from .cog/index-components.yaml or returns defaults.
func loadComponentConfig(root string) *ComponentConfig {
	path := filepath.Join(root, ".cog", "index-components.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return defaultComponentConfig()
	}

	var cfg ComponentConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return defaultComponentConfig()
	}
	return &cfg
}

// classifyComponent determines which component a file belongs to.
// Rules are evaluated in specificity order (longest prefix first).
func classifyComponent(relPath string, cfg *ComponentConfig) string {
	type match struct {
		name   string
		prefix string
	}

	var matches []match
	for name, rule := range cfg.Components {
		for _, prefix := range rule.Paths {
			if strings.HasPrefix(relPath, prefix) {
				// Check exclusions
				excluded := false
				for _, exc := range rule.Exclude {
					if strings.HasPrefix(relPath, exc) {
						excluded = true
						break
					}
				}
				if !excluded {
					matches = append(matches, match{name: name, prefix: prefix})
				}
			}
		}
	}

	if len(matches) == 0 {
		return "root"
	}

	// Most specific (longest prefix) wins
	sort.Slice(matches, func(i, j int) bool {
		return len(matches[i].prefix) > len(matches[j].prefix)
	})

	best := matches[0]

	// If children: true, use subdirectory as sub-component
	rule := cfg.Components[best.name]
	if rule.Children {
		suffix := strings.TrimPrefix(relPath, best.prefix)
		if idx := strings.Index(suffix, "/"); idx > 0 {
			return best.name + "/" + suffix[:idx]
		}
	}

	return best.name
}

// =============================================================================
// FILE SCANNER
// =============================================================================

// ScanOptions configures the workspace scanner.
type ScanOptions struct {
	Component string // filter to specific component
	Language  string // filter to specific language
	Root      string // workspace root directory
}

// ScanWorkspace walks the workspace and builds a file-level index.
func ScanWorkspace(opts ScanOptions) (*WorkspaceIndex, error) {
	root := opts.Root
	cfg := loadComponentConfig(root)
	registry := defaultRegistry()

	start := time.Now()

	idx := &WorkspaceIndex{
		Version:      1,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		Workspace:    root,
		Stats:        IndexStats{Languages: make(map[string]int)},
		Files:        []FileRecord{},
		Symbols:      []Symbol{},
		ComponentMap: make(map[string][]string),
		Dependencies: make(map[string][]string),
		CallGraph:    make(map[string][]string),
	}

	// Try to get current git commit
	idx.GitCommit = currentGitCommit(root)

	// Merge config ignore patterns into default ignore dirs
	ignoreDirs := make(map[string]bool)
	for k, v := range defaultIgnoreDirs {
		ignoreDirs[k] = v
	}
	for _, pattern := range cfg.Ignore {
		dir := strings.TrimSuffix(pattern, "/")
		ignoreDirs[dir] = true
	}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors, continue walking
		}

		name := info.Name()

		// Compute relative path for prefix-based ignore checks
		relPath, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}

		// Skip ignored directories
		if info.IsDir() {
			if ignoreDirs[name] {
				return filepath.SkipDir
			}
			// Skip path prefixes from config (e.g. "research/", "external/")
			for _, prefix := range cfg.IgnorePaths {
				if strings.HasPrefix(relPath+"/", prefix) {
					return filepath.SkipDir
				}
			}
			// Skip hidden directories except .cog, .claude, .openclaw
			if strings.HasPrefix(name, ".") && name != ".cog" && name != ".claude" && name != ".openclaw" {
				return filepath.SkipDir
			}
			// Skip git submodules (directories containing a .git file, not dir)
			gitFile := filepath.Join(path, ".git")
			if fi, err := os.Stat(gitFile); err == nil && !fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip ignored files
		if defaultIgnoreFiles[name] {
			return nil
		}

		// Skip hidden files without recognized extensions
		if strings.HasPrefix(name, ".") {
			if _, ok := langExtMap[filepath.Ext(name)]; !ok {
				return nil
			}
		}

		// Skip binary / large files
		if info.Size() > 1024*1024 { // 1MB limit
			return nil
		}

		// Read file content for language detection and hashing
		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		lang := detectLanguage(path, content)
		if lang == "" {
			return nil
		}

		// Apply language filter
		if opts.Language != "" && lang != opts.Language {
			return nil
		}

		// Classify component
		component := classifyComponent(relPath, cfg)

		// Apply component filter
		if opts.Component != "" && component != opts.Component && !strings.HasPrefix(component, opts.Component+"/") {
			return nil
		}

		// Compute hash
		hash := fmt.Sprintf("%x", sha256.Sum256(content))[:16]

		rec := FileRecord{
			Path:     relPath,
			Language: lang,
			Size:     info.Size(),
			Hash:     hash,
			ModTime:  info.ModTime().UTC().Format(time.RFC3339),
		}

		// Detect Go package name
		if lang == "go" {
			if pkg := extractGoPackage(content); pkg != "" {
				rec.Package = pkg
			}
		}

		// Parse symbols via tree-sitter / regex parsers
		if parser := registry.ForLanguage(lang); parser != nil {
			parsedRec, symbols, err := parser.Parse(relPath, content)
			if err == nil && parsedRec != nil {
				rec.SymbolCount = parsedRec.SymbolCount
				rec.Imports = parsedRec.Imports
				rec.Exports = parsedRec.Exports
				idx.Symbols = append(idx.Symbols, symbols...)
				// Build dependency map from imports
				if len(parsedRec.Imports) > 0 {
					idx.Dependencies[relPath] = parsedRec.Imports
				}
			}

			// Extract call graph (optional — only tree-sitter parsers support this)
			if cgp, ok := parser.(CallGraphParser); ok {
				if calls := cgp.ExtractCalls(relPath, content); len(calls) > 0 {
					for caller, callees := range calls {
						key := relPath + ":" + caller
						for _, callee := range callees {
							idx.CallGraph[key] = appendUnique(idx.CallGraph[key], callee)
						}
					}
				}
			}
		}

		idx.Files = append(idx.Files, rec)
		idx.Stats.Languages[lang]++

		// Add to component map
		idx.ComponentMap[component] = append(idx.ComponentMap[component], relPath)

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("walking workspace: %w", err)
	}

	// Sort files for deterministic output
	sort.Slice(idx.Files, func(i, j int) bool {
		return idx.Files[i].Path < idx.Files[j].Path
	})

	// Sort component map entries
	for comp := range idx.ComponentMap {
		sort.Strings(idx.ComponentMap[comp])
	}

	// Compute stats
	idx.Stats.Files = len(idx.Files)
	idx.Stats.Symbols = len(idx.Symbols)
	idx.Stats.Components = len(idx.ComponentMap)
	idx.Stats.DurationMs = time.Since(start).Milliseconds()
	for _, deps := range idx.Dependencies {
		idx.Stats.ImportEdges += len(deps)
	}
	for _, callees := range idx.CallGraph {
		idx.Stats.CallEdges += len(callees)
	}

	return idx, nil
}

// currentGitCommit returns the current HEAD commit hash, or empty string.
func currentGitCommit(root string) string {
	headPath := filepath.Join(root, ".git", "HEAD")
	data, err := os.ReadFile(headPath)
	if err != nil {
		return ""
	}

	head := strings.TrimSpace(string(data))

	// If HEAD is a ref, resolve it
	if strings.HasPrefix(head, "ref: ") {
		refPath := filepath.Join(root, ".git", strings.TrimPrefix(head, "ref: "))
		data, err = os.ReadFile(refPath)
		if err != nil {
			return ""
		}
		resolved := strings.TrimSpace(string(data))
		if len(resolved) >= 12 {
			return resolved[:12]
		}
		return resolved
	}

	// Detached HEAD
	if len(head) >= 12 {
		return head[:12]
	}
	return head
}

// extractGoPackage extracts the package name from Go source.
func extractGoPackage(content []byte) string {
	s := string(content)
	for _, line := range strings.SplitN(s, "\n", 20) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "package ") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				return parts[1]
			}
		}
	}
	return ""
}
