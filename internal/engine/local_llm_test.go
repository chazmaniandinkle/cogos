package engine

import (
	"path/filepath"
	"testing"
)

// TestResolveLocalProviderTimeoutNilConfig: nil cfg yields 0 (caller falls
// back to localProviderDefaultTimeoutSec). Important for unit-test paths and
// the brief window during construction before cfg is wired.
func TestResolveLocalProviderTimeoutNilConfig(t *testing.T) {
	t.Parallel()
	if got := resolveLocalProviderTimeout(nil); got != 0 {
		t.Errorf("nil cfg: got %d; want 0", got)
	}
}

// TestResolveLocalProviderTimeoutMissingFiles: a Config rooted at a workspace
// without providers.yaml returns 0 (loadProvidersConfig errors are swallowed
// — the dispatch path then falls back to the default timeout).
func TestResolveLocalProviderTimeoutMissingFiles(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg, err := LoadConfig(root, 0)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := resolveLocalProviderTimeout(cfg); got != 0 {
		t.Errorf("missing providers.yaml: got %d; want 0", got)
	}
}

// TestResolveLocalProviderTimeoutPrefersOllama: when an "ollama" provider is
// configured, its Timeout wins regardless of other entries' values. This is
// the common-case operator setup.
func TestResolveLocalProviderTimeoutPrefersOllama(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	writeTestFile(t, filepath.Join(root, ".cog", "config", "providers.yaml"), `providers:
  ollama:
    type: ollama
    endpoint: http://localhost:11434
    model: gemma4:e4b
    timeout: 600
  claude-code:
    type: claude-code
    model: sonnet
    timeout: 30
routing:
  default: ollama
  fallback_chain: [ollama]
`)
	cfg, err := LoadConfig(root, 0)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := resolveLocalProviderTimeout(cfg); got != 600 {
		t.Errorf("ollama timeout: got %d; want 600", got)
	}
}

// TestResolveLocalProviderTimeoutLocalYAMLOverlay: providers.local.yaml's
// timeout overrides providers.yaml via the existing deep-merge, so operator
// node-specific overrides take effect without copying the entire defaults
// file. This is the path the bug originally hid: hardcoded 120s shadowed
// whatever operators configured here.
func TestResolveLocalProviderTimeoutLocalYAMLOverlay(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	writeTestFile(t, filepath.Join(root, ".cog", "config", "providers.yaml"), `providers:
  ollama:
    type: ollama
    endpoint: http://localhost:11434
    model: gemma4:e4b
    timeout: 60
routing:
  default: ollama
  fallback_chain: [ollama]
`)
	writeTestFile(t, filepath.Join(root, ".cog", "config", "providers.local.yaml"), `providers:
  ollama:
    timeout: 480
`)
	cfg, err := LoadConfig(root, 0)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := resolveLocalProviderTimeout(cfg); got != 480 {
		t.Errorf("local-yaml overlay: got %d; want 480", got)
	}
}

// TestResolveLocalProviderTimeoutFallsBackToOtherLocal: if no "ollama" entry
// exists (operator removed it; runs MLX or LM Studio only) the resolver falls
// back to any other localhost-pointed provider's timeout.
func TestResolveLocalProviderTimeoutFallsBackToOtherLocal(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	writeTestFile(t, filepath.Join(root, ".cog", "config", "providers.yaml"), `providers:
  lmstudio-mlx:
    type: openai
    endpoint: http://localhost:1234
    model: gemma-mlx
    timeout: 240
  claude-code:
    type: claude-code
    model: sonnet
    timeout: 30
routing:
  default: claude-code
  fallback_chain: [claude-code, lmstudio-mlx]
`)
	cfg, err := LoadConfig(root, 0)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := resolveLocalProviderTimeout(cfg); got != 240 {
		t.Errorf("local fallback: got %d; want 240", got)
	}
}

// TestResolveLocalProviderTimeoutSkipsRemoteProviders: a remote provider's
// timeout must not be picked up as the local-tier timeout. Otherwise a
// 30-second claude-code timeout would shadow whatever Ollama needs.
func TestResolveLocalProviderTimeoutSkipsRemoteProviders(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	writeTestFile(t, filepath.Join(root, ".cog", "config", "providers.yaml"), `providers:
  claude-code:
    type: claude-code
    model: sonnet
    timeout: 30
routing:
  default: claude-code
  fallback_chain: [claude-code]
`)
	cfg, err := LoadConfig(root, 0)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := resolveLocalProviderTimeout(cfg); got != 0 {
		t.Errorf("only-remote providers: got %d; want 0 (signal default)", got)
	}
}

// TestResolveLocalProviderTimeoutSkipsDisabledOllama: an explicitly disabled
// "ollama" entry must not contribute its timeout. Ensures the IsEnabled gate
// matches BuildRouter's behavior — operators expect "enabled: false" to
// remove a provider entirely from consideration.
func TestResolveLocalProviderTimeoutSkipsDisabledOllama(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	writeTestFile(t, filepath.Join(root, ".cog", "config", "providers.yaml"), `providers:
  ollama:
    type: ollama
    enabled: false
    endpoint: http://localhost:11434
    model: gemma4:e4b
    timeout: 60
  lmstudio-mlx:
    type: openai
    endpoint: http://localhost:1234
    model: gemma-mlx
    timeout: 240
routing:
  default: lmstudio-mlx
  fallback_chain: [lmstudio-mlx]
`)
	cfg, err := LoadConfig(root, 0)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := resolveLocalProviderTimeout(cfg); got != 240 {
		t.Errorf("disabled ollama should be skipped: got %d; want 240 (lmstudio fallback)", got)
	}
}

// TestBuildLocalProviderUsesProvidedTimeout: pass-through verification —
// if buildLocalProvider's caller supplies a timeout, that's what the
// constructed OllamaProvider's HTTP client sees.
func TestBuildLocalProviderUsesProvidedTimeout(t *testing.T) {
	t.Parallel()
	target := LocalLLMTarget{
		BaseURL: "http://localhost:11434",
		Backend: LocalLLMBackendOllama,
	}
	p := buildLocalProvider(target, "gemma4:e4b", 480)
	op, ok := p.(*OllamaProvider)
	if !ok {
		t.Fatalf("expected *OllamaProvider, got %T", p)
	}
	if got := int(op.timeout.Seconds()); got != 480 {
		t.Errorf("ollama HTTP timeout: got %ds; want 480s", got)
	}
}

// TestBuildLocalProviderDefaultsTo300: when caller passes 0 (no config
// override available) buildLocalProvider must apply the 300s default — that's
// the literal value the original 120s bug was raised against. Covers both
// negative (treated as zero) and zero inputs.
func TestBuildLocalProviderDefaultsTo300(t *testing.T) {
	t.Parallel()
	target := LocalLLMTarget{
		BaseURL: "http://localhost:11434",
		Backend: LocalLLMBackendOllama,
	}
	for _, in := range []int{0, -1} {
		p := buildLocalProvider(target, "gemma4:e4b", in)
		op := p.(*OllamaProvider)
		if got := int(op.timeout.Seconds()); got != localProviderDefaultTimeoutSec {
			t.Errorf("input %d: got %ds; want %ds", in, got, localProviderDefaultTimeoutSec)
		}
	}
}
