package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// === LifecycleManager Tests ===

func TestLifecycleManager_GetOrCreate_New(t *testing.T) {
	lm := NewLifecycleManager()
	s, isNew := lm.GetOrCreate("sess-1", "http", "cog")
	if !isNew {
		t.Fatal("expected new session")
	}
	if s.ID != "sess-1" {
		t.Fatalf("expected ID sess-1, got %s", s.ID)
	}
	if !s.FirstTurn {
		t.Fatal("expected FirstTurn=true for new session")
	}
	if s.TurnCount != 0 {
		t.Fatalf("expected TurnCount=0, got %d", s.TurnCount)
	}
	if s.AgentName != "cog" {
		t.Fatalf("expected AgentName=cog, got %s", s.AgentName)
	}
	if s.Origin != "http" {
		t.Fatalf("expected Origin=http, got %s", s.Origin)
	}
}

func TestLifecycleManager_GetOrCreate_Existing(t *testing.T) {
	lm := NewLifecycleManager()
	s1, _ := lm.GetOrCreate("sess-1", "http", "cog")
	s2, isNew := lm.GetOrCreate("sess-1", "http", "cog")
	if isNew {
		t.Fatal("expected existing session")
	}
	if s1 != s2 {
		t.Fatal("expected same pointer")
	}
}

func TestLifecycleManager_RecordTurn(t *testing.T) {
	lm := NewLifecycleManager()
	lm.GetOrCreate("sess-1", "http", "cog")

	lm.RecordTurn("sess-1", "claude-abc")

	s := lm.Get("sess-1")
	if s == nil {
		t.Fatal("session should exist")
	}
	if s.FirstTurn {
		t.Fatal("FirstTurn should be false after RecordTurn")
	}
	if s.TurnCount != 1 {
		t.Fatalf("expected TurnCount=1, got %d", s.TurnCount)
	}
	if s.ClaudeSessionID != "claude-abc" {
		t.Fatalf("expected ClaudeSessionID=claude-abc, got %s", s.ClaudeSessionID)
	}
}

func TestLifecycleManager_End(t *testing.T) {
	lm := NewLifecycleManager()
	lm.GetOrCreate("sess-1", "http", "cog")
	if lm.ActiveCount() != 1 {
		t.Fatal("expected 1 active session")
	}

	lm.End("sess-1")
	if lm.ActiveCount() != 0 {
		t.Fatal("expected 0 active sessions after End")
	}
	if lm.Get("sess-1") != nil {
		t.Fatal("session should be nil after End")
	}
}

// === Session Context Tests ===

func TestLoadSessionContext_FirstTurn(t *testing.T) {
	// Create a minimal workspace with identity files
	root := t.TempDir()
	identDir := filepath.Join(root, ".cog", "bin", "agents", "identities")
	os.MkdirAll(identDir, 0o755)

	// Write evocation seed
	os.WriteFile(filepath.Join(identDir, "evocation_seed.md"), []byte("Test evocation"), 0o644)

	// Write identity card
	os.WriteFile(filepath.Join(identDir, "identity_cog_interface.md"), []byte("---\ntype: identity\n---\n# Cog\nWorkspace guardian."), 0o644)

	session := &LifecycleSession{
		ID:        "test-sess",
		FirstTurn: true,
		AgentName: "cog",
	}

	ctx := LoadSessionContext(root, session)
	if ctx == "" {
		t.Fatal("expected non-empty context on first turn")
	}
	if !strings.Contains(ctx, "Test evocation") {
		t.Error("expected evocation in context")
	}
	if !strings.Contains(ctx, "session-context") {
		t.Error("expected session-context tag")
	}
}

func TestLoadSessionContext_SubsequentTurn(t *testing.T) {
	session := &LifecycleSession{
		ID:        "test-sess",
		FirstTurn: false,
		TurnCount: 3,
	}

	ctx := LoadSessionContext(t.TempDir(), session)
	if ctx != "" {
		t.Fatalf("expected empty context on non-first turn, got: %s", ctx)
	}
}

func TestLoadSessionContext_NilSession(t *testing.T) {
	ctx := LoadSessionContext(t.TempDir(), nil)
	if ctx != "" {
		t.Fatal("expected empty context for nil session")
	}
}

// === Working Memory Tests ===

func TestCreateWorkingMemory(t *testing.T) {
	root := t.TempDir()
	err := CreateWorkingMemory(root, "test-session")
	if err != nil {
		t.Fatalf("CreateWorkingMemory failed: %v", err)
	}

	path := workingMemoryPath(root, "test-session")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read created file: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "type: working-memory") {
		t.Error("missing frontmatter type")
	}
	if !strings.Contains(content, "session: test-session") {
		t.Error("missing session ID in frontmatter")
	}
	if !strings.Contains(content, "# Current Focus") {
		t.Error("missing Current Focus section")
	}
}

func TestCreateWorkingMemory_Idempotent(t *testing.T) {
	root := t.TempDir()
	CreateWorkingMemory(root, "test-session")

	// Modify the file
	path := workingMemoryPath(root, "test-session")
	os.WriteFile(path, []byte("modified"), 0o644)

	// Create again should not overwrite
	CreateWorkingMemory(root, "test-session")
	data, _ := os.ReadFile(path)
	if string(data) != "modified" {
		t.Error("CreateWorkingMemory should not overwrite existing file")
	}
}

func TestUpdateWorkingMemory(t *testing.T) {
	root := t.TempDir()
	CreateWorkingMemory(root, "test-session")

	response := `I decided to use Go for the implementation.
Let me check the file at apps/cogos/serve.go.
Is there a better approach for this?`

	err := UpdateWorkingMemory(root, "test-session", response)
	if err != nil {
		t.Fatalf("UpdateWorkingMemory failed: %v", err)
	}

	path := workingMemoryPath(root, "test-session")
	data, _ := os.ReadFile(path)
	content := string(data)

	if !strings.Contains(content, "turn: 1") {
		t.Error("expected turn count to increment to 1")
	}
}

func TestSealWorkingMemory(t *testing.T) {
	root := t.TempDir()
	CreateWorkingMemory(root, "test-session")

	err := SealWorkingMemory(root, "test-session")
	if err != nil {
		t.Fatalf("SealWorkingMemory failed: %v", err)
	}

	path := workingMemoryPath(root, "test-session")
	data, _ := os.ReadFile(path)
	content := string(data)

	if !strings.Contains(content, "sealed: true") {
		t.Error("expected sealed: true in frontmatter")
	}
	if !strings.Contains(content, "sealed_at:") {
		t.Error("expected sealed_at timestamp in frontmatter")
	}
}

func TestResolveWMSessionID(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", "_default"},
		{"unknown", "_default"},
		{"req-http-abc", "_default"},
		{"req-cli-xyz", "_default"},
		{"http:cog", "http:cog"},
		{"my-session", "my-session"},
	}

	for _, tt := range tests {
		got := resolveWMSessionID(tt.input)
		if got != tt.expected {
			t.Errorf("resolveWMSessionID(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}
