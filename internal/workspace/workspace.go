// Package workspace resolves the active CogOS workspace root.
//
// Resolution precedence:
//  1. COG_ROOT environment variable (explicit)
//  2. COG_WORKSPACE env var (lookup in global config)
//  3. Local git repo detection (if inside a workspace)
//  4. current-workspace from ~/.cog/config
//
// This package is dependency-injected: the main package wires global-config
// loading and git-root detection at init time. This avoids pulling the full
// config/git machinery into this package while still allowing providers to
// import it cleanly.
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ConfigProvider is the minimal view of the global config that
// ResolveWorkspace needs. The main package adapts its *GlobalConfig to this
// interface in an init() hook.
type ConfigProvider interface {
	// CurrentWorkspace returns the name of the globally-selected workspace,
	// or "" if none is set.
	CurrentWorkspace() string
	// WorkspacePath returns the filesystem path for the workspace with the
	// given name, and a bool indicating whether it exists in the config.
	WorkspacePath(name string) (string, bool)
}

// Dependency injection points. The main package must set these in an init()
// hook before ResolveWorkspace is called. If either is nil, the corresponding
// resolution tier is skipped gracefully.
var (
	// LoadConfig loads the global config (~/.cog/config).
	LoadConfig func() (ConfigProvider, error)
	// GitRoot returns the path of the enclosing git repository.
	GitRoot func() (string, error)
)

// workspaceCache caches the result of workspace resolution.
// ResolveWorkspace() is called 25+ times per process; caching avoids
// redundant config reads and git operations.
var workspaceCache struct {
	once   sync.Once
	root   string
	source string
	err    error
}

// ResolveWorkspace determines the workspace root based on precedence:
//  1. COG_ROOT environment variable (explicit)
//  2. COG_WORKSPACE env var (lookup in global config)
//  3. Local git repo detection (if inside a workspace)
//  4. current-workspace from ~/.cog/config
//
// Returns (workspaceRoot, source, error) where source describes how it was
// resolved.
//
// Results are cached for the lifetime of the process since resolution is
// deterministic within a single invocation.
func ResolveWorkspace() (string, string, error) {
	workspaceCache.once.Do(func() {
		workspaceCache.root, workspaceCache.source, workspaceCache.err = resolveWorkspaceUncached()
	})
	return workspaceCache.root, workspaceCache.source, workspaceCache.err
}

// IsRealWorkspace checks if dir has a .cog/ directory that looks like a real
// CogOS workspace (has config/ or mem/), not just a bare .cog/ with .state/ only
// (which submodules sometimes have).
func IsRealWorkspace(dir string) bool {
	cogDir := filepath.Join(dir, ".cog")
	info, err := os.Stat(cogDir)
	if err != nil || !info.IsDir() {
		return false
	}
	// A real workspace has config/ subdirectory (not just mem/ or .state/)
	if info, err := os.Stat(filepath.Join(cogDir, "config")); err == nil && info.IsDir() {
		return true
	}
	return false
}

// resolveWorkspaceUncached implements the actual workspace resolution logic.
// Called once per process via ResolveWorkspace().
func resolveWorkspaceUncached() (string, string, error) {
	// 1. Explicit COG_ROOT (set by wrapper for --root flag)
	if root := os.Getenv("COG_ROOT"); root != "" {
		cogDir := filepath.Join(root, ".cog")
		if info, err := os.Stat(cogDir); err == nil && info.IsDir() {
			return root, "explicit", nil
		}
		return "", "", fmt.Errorf("COG_ROOT=%s is not a valid workspace (no .cog/ directory)", root)
	}

	// 2. COG_WORKSPACE env var (lookup by name in global config)
	// Graceful degradation: fall through to tier 3 if config fails or workspace not found
	if wsName := os.Getenv("COG_WORKSPACE"); wsName != "" && LoadConfig != nil {
		config, err := LoadConfig()
		if err == nil { // Only proceed if config loaded successfully
			if path, ok := config.WorkspacePath(wsName); ok {
				cogDir := filepath.Join(path, ".cog")
				if _, err := os.Stat(cogDir); err != nil {
					return "", "", fmt.Errorf("workspace '%s' at %s is invalid (no .cog/ directory)", wsName, path)
				}
				return path, "env", nil
			}
		}
		// Fall through to tier 3 (local git detection) silently
	}

	// 3. Local git detection (if inside a workspace)
	// Walk up through git roots — a submodule might have a bare .cog/ directory
	// (with only .state/) but the real workspace is the parent repo with config/ and mem/.
	if GitRoot != nil {
		if root, err := GitRoot(); err == nil {
			if IsRealWorkspace(root) {
				return root, "local", nil
			}
			// If git root has a .cog/ but it's not a real workspace (e.g. submodule),
			// check the parent directory's git root (the superproject).
			parentDir := filepath.Dir(root)
			if parentDir != root { // not filesystem root
				// Walk up looking for a real workspace
				for dir := parentDir; dir != "/" && dir != "."; dir = filepath.Dir(dir) {
					if IsRealWorkspace(dir) {
						return dir, "local", nil
					}
				}
			}
		}
	}

	// 4. Fall back to global current-workspace
	if LoadConfig == nil {
		return "", "", fmt.Errorf("no workspace found (run 'cog workspace add' or cd into a workspace)")
	}
	config, err := LoadConfig()
	if err != nil {
		return "", "", fmt.Errorf("failed to load global config: %w", err)
	}

	if name := config.CurrentWorkspace(); name != "" {
		if path, ok := config.WorkspacePath(name); ok {
			cogDir := filepath.Join(path, ".cog")
			if _, err := os.Stat(cogDir); err != nil {
				return "", "", fmt.Errorf("workspace '%s' at %s is invalid (no .cog/ directory)", name, path)
			}
			return path, "global", nil
		}
		// Current workspace is set but doesn't exist in config
		return "", "", fmt.Errorf("current workspace '%s' not found in config", name)
	}

	return "", "", fmt.Errorf("no workspace found (run 'cog workspace add' or cd into a workspace)")
}
