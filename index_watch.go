package main

// index_watch.go — File watcher for continuous workspace indexing.
//
// Phase 6: Uses fsnotify with debounce for incremental re-indexing
// on file changes. Follows the BEP provider pattern (500ms debounce).

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

// watchWorkspace runs continuous indexing, re-scanning on file changes.
func watchWorkspace(root string, opts ScanOptions, writers []IndexWriter, driftWriters []DriftWriter) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("creating watcher: %w", err)
	}
	defer watcher.Close()

	// Add workspace directories to watch
	cfg := loadComponentConfig(root)
	if err := addWatchDirs(watcher, root, cfg); err != nil {
		return fmt.Errorf("adding watch dirs: %w", err)
	}

	// Signal handling for clean shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	fmt.Fprintf(os.Stderr, "Watching workspace for changes... (Ctrl-C to stop)\n")

	// Load current index as baseline for drift
	baselinePath := filepath.Join(root, ".cog", "run", "index", "current.json")
	baseline, _ := loadBaseline(baselinePath)

	debounce := time.NewTimer(0)
	if !debounce.Stop() {
		<-debounce.C
	}
	pending := make(map[string]bool)

	for {
		select {
		case <-sigCh:
			fmt.Fprintf(os.Stderr, "\nShutting down watcher...\n")
			return nil

		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			relPath, err := filepath.Rel(root, event.Name)
			if err != nil {
				continue
			}
			if shouldIgnoreWatch(relPath) {
				continue
			}
			pending[relPath] = true
			if !debounce.Stop() {
				select {
				case <-debounce.C:
				default:
				}
			}
			debounce.Reset(500 * time.Millisecond)

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			fmt.Fprintf(os.Stderr, "Watch error: %v\n", err)

		case <-debounce.C:
			if len(pending) == 0 {
				continue
			}

			nChanged := len(pending)
			clear(pending)

			fmt.Fprintf(os.Stderr, "\n[%s] %d files changed, re-indexing...\n",
				time.Now().Format("15:04:05"), nChanged)

			// Re-scan
			idx, err := ScanWorkspace(opts)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error re-indexing: %v\n", err)
				continue
			}

			// Write outputs
			for _, w := range writers {
				if err := w.WriteIndex(idx); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
				}
			}

			// Emit bus event
			emitIndexBusEvent(root, idx)

			// Compute and report drift
			if baseline != nil {
				report := computeDrift(baseline, idx)
				if report.Summary != "No structural drift detected." {
					fmt.Fprintf(os.Stderr, "Drift: %s\n", report.Summary)
					for _, w := range driftWriters {
						if err := w.WriteDrift(report); err != nil {
							fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
						}
					}
					emitDriftBusEvent(root, report)
				}
			}

			baseline = idx
			fmt.Fprintf(os.Stderr, "Indexed %d files (%d symbols) in %dms\n",
				idx.Stats.Files, idx.Stats.Symbols, idx.Stats.DurationMs)

			// Add any new directories created during changes
			addWatchDirs(watcher, root, cfg)
		}
	}
}

// addWatchDirs adds workspace subdirectories to the watcher.
func addWatchDirs(watcher *fsnotify.Watcher, root string, cfg *ComponentConfig) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}

		name := d.Name()

		// Skip ignored directories
		if defaultIgnoreDirs[name] {
			return filepath.SkipDir
		}

		// Skip hidden dirs except .cog, .claude, .openclaw
		if strings.HasPrefix(name, ".") && name != ".cog" && name != ".claude" && name != ".openclaw" && path != root {
			return filepath.SkipDir
		}

		// Skip path prefixes
		relPath, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		for _, prefix := range cfg.IgnorePaths {
			if strings.HasPrefix(relPath+"/", prefix) {
				return filepath.SkipDir
			}
		}

		// Skip submodules
		gitFile := filepath.Join(path, ".git")
		if fi, serr := os.Stat(gitFile); serr == nil && !fi.IsDir() {
			return filepath.SkipDir
		}

		watcher.Add(path)
		return nil
	})
}

// shouldIgnoreWatch returns true for paths that shouldn't trigger re-indexing.
func shouldIgnoreWatch(relPath string) bool {
	// Skip index output files (prevents feedback loops)
	if strings.HasPrefix(relPath+"/", ".cog/run/index/") {
		return true
	}
	if strings.HasPrefix(relPath+"/", "docs/index/") {
		return true
	}
	// Skip common non-source changes
	name := filepath.Base(relPath)
	if defaultIgnoreFiles[name] {
		return true
	}
	return false
}
