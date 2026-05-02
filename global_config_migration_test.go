// global_config_migration_test.go
// Tests for the one-time migration of ~/.cog/config → ~/.cog/node/global.yaml.
//
// All tests use t.TempDir() as a fake HOME so the real ~/.cog/ is never touched.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupFakeHome creates a temporary directory, sets $HOME to it, and returns
// (fakeHome, oldPath, newPath). The test's t.Cleanup / t.TempDir handles teardown.
func setupFakeHome(t *testing.T) (fakeHome, oldPath, newPath string) {
	t.Helper()
	fakeHome = t.TempDir()
	t.Setenv("HOME", fakeHome)
	oldPath = filepath.Join(fakeHome, ".cog", "config")
	newPath = filepath.Join(fakeHome, ".cog", "node", "global.yaml")
	return
}

// writeFile creates parent dirs and writes content to path.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

const sampleGlobalConfig = `version: "1.0"
current-workspace: myws
workspaces:
  myws:
    path: /home/user/myws
    name: myws
`

// TestMigrateGlobalConfig_OldExists_NewAbsent verifies that when only the old
// file exists the migration moves it to the new path with content preserved and
// the old path removed.
func TestMigrateGlobalConfig_OldExists_NewAbsent(t *testing.T) {
	_, oldPath, newPath := setupFakeHome(t)

	writeFile(t, oldPath, sampleGlobalConfig)

	if err := migrateGlobalConfig(); err != nil {
		t.Fatalf("migrateGlobalConfig() returned error: %v", err)
	}

	// New path must exist with original content.
	got, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("new path not readable after migration: %v", err)
	}
	if string(got) != sampleGlobalConfig {
		t.Errorf("content mismatch\ngot:  %q\nwant: %q", string(got), sampleGlobalConfig)
	}

	// Old path must be gone.
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("old path still exists after migration: %s", oldPath)
	}
}

// TestMigrateGlobalConfig_Idempotent verifies that running migration a second
// time (new exists, old gone) is a no-op and returns nil.
func TestMigrateGlobalConfig_Idempotent(t *testing.T) {
	_, oldPath, newPath := setupFakeHome(t)

	writeFile(t, newPath, sampleGlobalConfig)

	// Old path does not exist (already migrated state).
	if err := migrateGlobalConfig(); err != nil {
		t.Fatalf("second run of migrateGlobalConfig() returned error: %v", err)
	}

	// New path must still contain the original content.
	got, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("new path not readable: %v", err)
	}
	if string(got) != sampleGlobalConfig {
		t.Errorf("new path content was modified by second run")
	}

	// Old path must still not exist.
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("old path appeared unexpectedly: %s", oldPath)
	}
}

// TestMigrateGlobalConfig_BothExist verifies that when both files exist the
// migration keeps the new file intact and returns nil (no error, no overwrite).
func TestMigrateGlobalConfig_BothExist(t *testing.T) {
	_, oldPath, newPath := setupFakeHome(t)

	const newContent = `version: "1.0"
current-workspace: newws
workspaces:
  newws:
    path: /home/user/newws
`
	writeFile(t, oldPath, sampleGlobalConfig)
	writeFile(t, newPath, newContent)

	if err := migrateGlobalConfig(); err != nil {
		t.Fatalf("migrateGlobalConfig() returned error when both files exist: %v", err)
	}

	// New path must retain its own content (not overwritten by old).
	got, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("new path not readable: %v", err)
	}
	if string(got) != newContent {
		t.Errorf("new path content was overwritten; got %q want %q", string(got), newContent)
	}

	// Old path is still present (we don't delete it in the both-exist case).
	if _, err := os.Stat(oldPath); err != nil {
		t.Errorf("old path unexpectedly absent: %v", err)
	}
}

// TestMigrateGlobalConfig_NeitherExists verifies that a fresh install (no
// files at either path) is a no-op and returns nil.
func TestMigrateGlobalConfig_NeitherExists(t *testing.T) {
	setupFakeHome(t) // sets $HOME only; no files written

	if err := migrateGlobalConfig(); err != nil {
		t.Fatalf("migrateGlobalConfig() returned error on fresh install: %v", err)
	}
}

// TestMigrateGlobalConfig_MoveFails verifies that when os.Rename fails (e.g.
// permission error) the function returns an error rather than panicking, and
// loadGlobalConfig falls back to the old path so the kernel still starts.
func TestMigrateGlobalConfig_MoveFails(t *testing.T) {
	_, oldPath, newPath := setupFakeHome(t)

	writeFile(t, oldPath, sampleGlobalConfig)

	// Make the destination directory read-only so Rename into it fails.
	nodeDir := filepath.Dir(newPath)
	if err := os.MkdirAll(nodeDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Chmod(nodeDir, 0555); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(nodeDir, 0755) }) // restore so TempDir cleanup works

	err := migrateGlobalConfig()
	if err == nil {
		t.Fatal("expected migrateGlobalConfig to return an error when Rename fails, got nil")
	}

	// loadGlobalConfig must still work by falling back to the old path.
	cfg, loadErr := loadGlobalConfig()
	if loadErr != nil {
		t.Fatalf("loadGlobalConfig() failed after migration error: %v", loadErr)
	}
	if cfg.CurrentWorkspace != "myws" {
		t.Errorf("expected current-workspace=myws from fallback, got %q", cfg.CurrentWorkspace)
	}
}

// TestLoadGlobalConfig_NewPath verifies that loadGlobalConfig reads from the
// new path when no old file exists (happy-path after migration).
func TestLoadGlobalConfig_NewPath(t *testing.T) {
	_, _, newPath := setupFakeHome(t)

	writeFile(t, newPath, sampleGlobalConfig)

	cfg, err := loadGlobalConfig()
	if err != nil {
		t.Fatalf("loadGlobalConfig(): %v", err)
	}
	if cfg.CurrentWorkspace != "myws" {
		t.Errorf("CurrentWorkspace = %q; want myws", cfg.CurrentWorkspace)
	}
	if _, ok := cfg.Workspaces["myws"]; !ok {
		t.Error("Workspaces[myws] not present")
	}
}

// TestGlobalConfigPath_NewLocation verifies that globalConfigPath() returns a
// path under ~/.cog/node/ (not ~/.cog/config).
func TestGlobalConfigPath_NewLocation(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	got := globalConfigPath()
	want := filepath.Join(fakeHome, ".cog", "node", "global.yaml")

	if got != want {
		t.Errorf("globalConfigPath() = %q; want %q", got, want)
	}
	if strings.Contains(got, filepath.Join(".cog", "config")) {
		t.Errorf("globalConfigPath() still points into .cog/config: %q", got)
	}
}
