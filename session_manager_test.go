package main

import (
	"sync"
	"testing"
	"time"
)

func TestResolveCreatesNewSession(t *testing.T) {
	sm := NewSessionManager(DefaultSessionManagerConfig())
	state := sm.Resolve("thread-1")

	if state == nil {
		t.Fatal("expected non-nil session state")
	}
	if state.ID == "" {
		t.Error("expected session to have an ID")
	}
	if state.ThreadID != "thread-1" {
		t.Errorf("expected ThreadID=thread-1, got %s", state.ThreadID)
	}
	if state.ClaudeSessionID != "" {
		t.Error("expected empty ClaudeSessionID for new session")
	}
	if state.CreatedAt.IsZero() {
		t.Error("expected CreatedAt to be set")
	}
	if state.LastActiveAt.IsZero() {
		t.Error("expected LastActiveAt to be set")
	}
	if state.TurnCount != 0 {
		t.Errorf("expected TurnCount=0, got %d", state.TurnCount)
	}
}

func TestResolveReturnsExisting(t *testing.T) {
	sm := NewSessionManager(DefaultSessionManagerConfig())
	s1 := sm.Resolve("thread-1")
	s2 := sm.Resolve("thread-1")

	if s1 != s2 {
		t.Error("expected same pointer for same threadID")
	}
	if s1.ID != s2.ID {
		t.Error("expected same session ID")
	}
}

func TestRecordClaudeSession(t *testing.T) {
	sm := NewSessionManager(DefaultSessionManagerConfig())

	sm.RecordClaudeSession("thread-1", "claude-abc123")

	state := sm.GetSession("thread-1")
	if state == nil {
		t.Fatal("expected session to exist after RecordClaudeSession")
	}
	if state.ClaudeSessionID != "claude-abc123" {
		t.Errorf("expected ClaudeSessionID=claude-abc123, got %s", state.ClaudeSessionID)
	}
}

func TestRecordTurn(t *testing.T) {
	sm := NewSessionManager(DefaultSessionManagerConfig())
	sm.Resolve("thread-1")

	sm.RecordTurn("thread-1", 500)
	sm.RecordTurn("thread-1", 300)

	state := sm.GetSession("thread-1")
	if state.TurnCount != 2 {
		t.Errorf("expected TurnCount=2, got %d", state.TurnCount)
	}
	if state.TotalTokensSent != 800 {
		t.Errorf("expected TotalTokensSent=800, got %d", state.TotalTokensSent)
	}
}

func TestShouldRotateTurnsThreshold(t *testing.T) {
	cfg := SessionManagerConfig{
		MaxTurnsBeforeRotation:  5,
		MaxTokensBeforeRotation: 1_000_000,
		IdleTimeout:             time.Hour,
	}
	sm := NewSessionManager(cfg)
	sm.Resolve("thread-1")
	sm.RecordClaudeSession("thread-1", "claude-1")

	// Record 6 turns.
	for i := 0; i < 6; i++ {
		sm.RecordTurn("thread-1", 100)
	}

	state := sm.GetSession("thread-1")
	shouldRotate, reason := sm.ShouldRotate(state, nil)
	if !shouldRotate {
		t.Error("expected rotation due to turn count")
	}
	if reason != "pressure:turns" {
		t.Errorf("expected reason=pressure:turns, got %s", reason)
	}
}

func TestShouldRotateTokensThreshold(t *testing.T) {
	cfg := SessionManagerConfig{
		MaxTurnsBeforeRotation:  100,
		MaxTokensBeforeRotation: 1000,
		IdleTimeout:             time.Hour,
	}
	sm := NewSessionManager(cfg)
	sm.Resolve("thread-1")
	sm.RecordClaudeSession("thread-1", "claude-1")

	sm.RecordTurn("thread-1", 1500)

	state := sm.GetSession("thread-1")
	shouldRotate, reason := sm.ShouldRotate(state, nil)
	if !shouldRotate {
		t.Error("expected rotation due to token count")
	}
	if reason != "pressure:tokens" {
		t.Errorf("expected reason=pressure:tokens, got %s", reason)
	}
}

func TestShouldRotateIdleTimeout(t *testing.T) {
	cfg := SessionManagerConfig{
		MaxTurnsBeforeRotation:  100,
		MaxTokensBeforeRotation: 1_000_000,
		IdleTimeout:             time.Millisecond,
	}
	sm := NewSessionManager(cfg)
	sm.Resolve("thread-1")
	sm.RecordClaudeSession("thread-1", "claude-1")

	// Wait for idle timeout to expire.
	time.Sleep(5 * time.Millisecond)

	state := sm.GetSession("thread-1")
	shouldRotate, reason := sm.ShouldRotate(state, nil)
	if !shouldRotate {
		t.Error("expected rotation due to idle timeout")
	}
	if reason != "idle" {
		t.Errorf("expected reason=idle, got %s", reason)
	}
}

func TestShouldRotateNoClaudeSession(t *testing.T) {
	cfg := SessionManagerConfig{
		MaxTurnsBeforeRotation:  1,
		MaxTokensBeforeRotation: 1,
		IdleTimeout:             time.Millisecond,
	}
	sm := NewSessionManager(cfg)
	state := sm.Resolve("thread-1")

	// Even with aggressive thresholds, no rotation when no Claude session exists.
	time.Sleep(2 * time.Millisecond)

	shouldRotate, reason := sm.ShouldRotate(state, nil)
	if shouldRotate {
		t.Errorf("expected no rotation without ClaudeSessionID, got reason=%s", reason)
	}
}

func TestRotate(t *testing.T) {
	sm := NewSessionManager(DefaultSessionManagerConfig())
	sm.Resolve("thread-1")
	sm.RecordClaudeSession("thread-1", "claude-old")

	// Set working memory before rotation.
	wm := &WorkingMemory{
		ActiveTopics: []string{"physics", "math"},
		Summary:      "discussing eigenvalues",
		UpdatedAt:    time.Now(),
	}
	sm.UpdateWorkingMemory("thread-1", wm)

	// Record some turns.
	for i := 0; i < 10; i++ {
		sm.RecordTurn("thread-1", 100)
	}

	state := sm.GetSession("thread-1")
	sm.Rotate(state, "pressure:turns")

	// Verify ClaudeSessionID cleared.
	if state.ClaudeSessionID != "" {
		t.Error("expected ClaudeSessionID to be cleared after rotation")
	}

	// Verify history entry.
	if len(state.History) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(state.History))
	}
	h := state.History[0]
	if h.ClaudeSessionID != "claude-old" {
		t.Errorf("expected history ClaudeSessionID=claude-old, got %s", h.ClaudeSessionID)
	}
	if h.Reason != "pressure:turns" {
		t.Errorf("expected history reason=pressure:turns, got %s", h.Reason)
	}
	if h.TurnCount != 10 {
		t.Errorf("expected history TurnCount=10, got %d", h.TurnCount)
	}

	// Verify counters reset.
	if state.TurnCount != 0 {
		t.Errorf("expected TurnCount=0 after rotation, got %d", state.TurnCount)
	}
	if state.TotalTokensSent != 0 {
		t.Errorf("expected TotalTokensSent=0 after rotation, got %d", state.TotalTokensSent)
	}

	// Verify working memory preserved.
	if state.WorkingMemory == nil {
		t.Fatal("expected WorkingMemory to be preserved after rotation")
	}
	if state.WorkingMemory.Summary != "discussing eigenvalues" {
		t.Error("expected WorkingMemory.Summary to be preserved")
	}
	if len(state.WorkingMemory.ActiveTopics) != 2 {
		t.Error("expected WorkingMemory.ActiveTopics to be preserved")
	}
}

func TestConcurrentAccess(t *testing.T) {
	sm := NewSessionManager(DefaultSessionManagerConfig())
	var wg sync.WaitGroup

	// Spawn 10 goroutines hitting the same and different threads.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			threadID := "thread-concurrent"
			if n%3 == 0 {
				threadID = "thread-other"
			}
			sm.Resolve(threadID)
			sm.RecordClaudeSession(threadID, "claude-concurrent")
			sm.RecordTurn(threadID, 100)
			sm.GetSession(threadID)
			sm.UpdateWorkingMemory(threadID, &WorkingMemory{
				Summary:   "concurrent test",
				UpdatedAt: time.Now(),
			})
		}(i)
	}

	wg.Wait()

	// Basic sanity: sessions should exist and have positive turn counts.
	s1 := sm.GetSession("thread-concurrent")
	if s1 == nil {
		t.Fatal("expected thread-concurrent session to exist")
	}
	if s1.TurnCount <= 0 {
		t.Error("expected positive turn count for concurrent thread")
	}
}
