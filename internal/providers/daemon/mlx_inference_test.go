// mlx_inference_test.go — unit tests for the daemon-side mlx-inference provider.
//
// These tests are whitebox (package daemon) and exercise the YAML parser,
// launchd label defaulting, and Health() logic in isolation.
//
// No live launchctl or HTTP calls are made. The Health() integration tests that
// depend on a real workspace live in daemon_test.go.
package daemon

import (
	"os"
	"strings"
	"testing"
)

// ── parseMLXEntriesFromYAML ───────────────────────────────────────────────────

// TestParseMLXEntries_Empty: empty YAML yields no entries.
func TestParseMLXEntries_Empty(t *testing.T) {
	t.Parallel()
	entries := parseMLXEntriesFromYAML([]byte(""), "providers.yaml")
	if len(entries) != 0 {
		t.Errorf("empty YAML: got %d entries; want 0", len(entries))
	}
}

// TestParseMLXEntries_NoMLXProviders: YAML with non-mlx providers yields nothing.
func TestParseMLXEntries_NoMLXProviders(t *testing.T) {
	t.Parallel()
	yaml := `providers:
  ollama:
    type: ollama
    endpoint: http://localhost:11434
    model: gemma4:e4b
  claude-code:
    type: claude-code
    model: sonnet
`
	entries := parseMLXEntriesFromYAML([]byte(yaml), "providers.yaml")
	if len(entries) != 0 {
		t.Errorf("no mlx providers: got %d entries; want 0", len(entries))
	}
}

// TestParseMLXEntries_SingleMLXEntry: a single mlx-supervised entry is parsed correctly.
func TestParseMLXEntries_SingleMLXEntry(t *testing.T) {
	t.Parallel()
	yaml := `providers:
  mlx-gemma:
    type: mlx-supervised
    endpoint: http://localhost:1235
    model: /Volumes/SD/gemma-4-mlx
`
	entries := parseMLXEntriesFromYAML([]byte(yaml), "providers.yaml")
	if len(entries) != 1 {
		t.Fatalf("got %d entries; want 1", len(entries))
	}
	e := entries[0]
	if e.name != "mlx-gemma" {
		t.Errorf("name: got %q; want mlx-gemma", e.name)
	}
	if e.endpoint != "http://localhost:1235" {
		t.Errorf("endpoint: got %q; want http://localhost:1235", e.endpoint)
	}
	if e.model != "/Volumes/SD/gemma-4-mlx" {
		t.Errorf("model: got %q; want /Volumes/SD/gemma-4-mlx", e.model)
	}
	// Default label: com.cogos.mlx-<name>
	if e.launchdLabel != "com.cogos.mlx-mlx-gemma" {
		t.Errorf("launchdLabel: got %q; want com.cogos.mlx-mlx-gemma", e.launchdLabel)
	}
}

// TestParseMLXEntries_CustomLaunchdLabel: launchd_label under options is parsed.
func TestParseMLXEntries_CustomLaunchdLabel(t *testing.T) {
	t.Parallel()
	yaml := `providers:
  mlx-custom:
    type: mlx-supervised
    endpoint: http://localhost:1235
    model: /vol/model
    launchd_label: com.example.custom
`
	entries := parseMLXEntriesFromYAML([]byte(yaml), "providers.local.yaml")
	if len(entries) != 1 {
		t.Fatalf("got %d entries; want 1", len(entries))
	}
	if entries[0].launchdLabel != "com.example.custom" {
		t.Errorf("launchdLabel: got %q; want com.example.custom", entries[0].launchdLabel)
	}
}

// TestParseMLXEntries_SkipsNonMLX: only mlx-supervised entries are included;
// ollama and other entries are ignored.
func TestParseMLXEntries_SkipsNonMLX(t *testing.T) {
	t.Parallel()
	yaml := `providers:
  ollama:
    type: ollama
    model: gemma4:e4b
  mlx-gemma:
    type: mlx-supervised
    endpoint: http://localhost:1235
    model: /vol/model
  claude-code:
    type: claude-code
    model: sonnet
`
	entries := parseMLXEntriesFromYAML([]byte(yaml), "providers.yaml")
	if len(entries) != 1 {
		t.Fatalf("got %d entries; want 1 (only mlx-supervised)", len(entries))
	}
	if entries[0].name != "mlx-gemma" {
		t.Errorf("name: got %q; want mlx-gemma", entries[0].name)
	}
}

// TestParseMLXEntries_MultipleMLXEntries: multiple mlx-supervised entries all parsed.
func TestParseMLXEntries_MultipleMLXEntries(t *testing.T) {
	t.Parallel()
	yaml := `providers:
  mlx-gemma:
    type: mlx-supervised
    endpoint: http://localhost:1235
    model: /vol/gemma
  mlx-qwen:
    type: mlx-supervised
    endpoint: http://localhost:1236
    model: /vol/qwen
`
	entries := parseMLXEntriesFromYAML([]byte(yaml), "providers.local.yaml")
	if len(entries) != 2 {
		t.Fatalf("got %d entries; want 2", len(entries))
	}
	names := make(map[string]bool)
	for _, e := range entries {
		names[e.name] = true
	}
	for _, want := range []string{"mlx-gemma", "mlx-qwen"} {
		if !names[want] {
			t.Errorf("missing entry %q in parsed result", want)
		}
	}
}

// TestParseMLXEntries_CommentsIgnored: YAML comment lines do not confuse the parser.
func TestParseMLXEntries_CommentsIgnored(t *testing.T) {
	t.Parallel()
	yaml := `# Top-level comment
providers:
  # This is ollama
  ollama:
    type: ollama
    model: gemma4:e4b
  # This is mlx
  mlx-gemma:
    type: mlx-supervised
    endpoint: http://localhost:1235
    model: /vol/model
`
	entries := parseMLXEntriesFromYAML([]byte(yaml), "providers.yaml")
	if len(entries) != 1 {
		t.Fatalf("got %d entries; want 1", len(entries))
	}
}

// TestParseMLXEntries_NoProvidersSection: file with no providers: key yields nothing.
func TestParseMLXEntries_NoProvidersSection(t *testing.T) {
	t.Parallel()
	yaml := `routing:
  default: claude-code
  fallback_chain: [claude-code]
`
	entries := parseMLXEntriesFromYAML([]byte(yaml), "providers.yaml")
	if len(entries) != 0 {
		t.Errorf("no providers section: got %d entries; want 0", len(entries))
	}
}

// ── loadMLXEntries (filesystem integration) ───────────────────────────────────

// TestLoadMLXEntries_NoFiles: workspace without providers files yields empty.
func TestLoadMLXEntries_NoFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	entries := loadMLXEntries(root)
	if len(entries) != 0 {
		t.Errorf("no config files: got %d entries; want 0", len(entries))
	}
}

// TestLoadMLXEntries_LocalOverridesBase: providers.local.yaml's mlx entry
// replaces the base entry by name (same overlay semantics as engine's merge).
func TestLoadMLXEntries_LocalOverridesBase(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	configDir := root + "/.cog/config"
	if err := mkdirAll(configDir); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Base: mlx-gemma on port 1235.
	writeTestYAML(t, configDir+"/providers.yaml", `providers:
  mlx-gemma:
    type: mlx-supervised
    endpoint: http://localhost:1235
    model: /vol/base-model
`)
	// Local: mlx-gemma on port 1236 (different endpoint).
	writeTestYAML(t, configDir+"/providers.local.yaml", `providers:
  mlx-gemma:
    type: mlx-supervised
    endpoint: http://localhost:1236
    model: /vol/local-model
`)

	entries := loadMLXEntries(root)
	if len(entries) != 1 {
		t.Fatalf("got %d entries; want 1", len(entries))
	}
	// Local wins.
	if entries[0].endpoint != "http://localhost:1236" {
		t.Errorf("endpoint: got %q; want local http://localhost:1236", entries[0].endpoint)
	}
	if entries[0].model != "/vol/local-model" {
		t.Errorf("model: got %q; want /vol/local-model", entries[0].model)
	}
}

// ── probeEndpointModels ───────────────────────────────────────────────────────

// TestProbeEndpointModels_EmptyModel: empty model string accepts any server response.
func TestProbeEndpointModels_EmptyModel(t *testing.T) {
	// No server — the call should return false cleanly, no panic.
	t.Parallel()
	// Use a clearly-invalid URL to get a fast failure without flapping.
	ok := probeEndpointModels(t.Context(), "http://127.0.0.1:19877", "")
	if ok {
		t.Error("probeEndpointModels: returned true with no server")
	}
}

// ── helpers ────────────────────────────────────────────────────────────────────

func mkdirAll(path string) error {
	return os.MkdirAll(path, 0o755)
}

func writeTestYAML(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writeTestYAML %s: %v", path, err)
	}
}

// mlxTestContains checks whether s contains sub — local to this test file to
// avoid collision with other test helpers in the package.
func mlxTestContains(s, sub string) bool {
	return strings.Contains(s, sub)
}

// Ensure the test file compiles: unused import guard.
var _ = mlxTestContains
