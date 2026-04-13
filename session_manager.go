// session_manager.go — CogOS session lifecycle management.
//
// SessionManager replaces the simple claudeSessionStore map[string]string,
// tracking CogOS-managed conversations across Claude CLI session rotations.
// One CogOS conversation may span multiple Claude CLI sessions — when context
// pressure builds, we rotate the Claude session while preserving working memory.

package main

import (
	"crypto/rand"
	"fmt"
	"sync"
	"time"
)

// SessionManager manages CogOS session state across Claude CLI rotations.
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*SessionState // keyed by OpenClaw thread/session ID
	config   SessionManagerConfig
}

// NewSessionManager creates a SessionManager with the given config.
func NewSessionManager(config SessionManagerConfig) *SessionManager {
	return &SessionManager{
		sessions: make(map[string]*SessionState),
		config:   config,
	}
}

// generateID produces a random hex ID suitable for CogOS session identifiers.
func generateID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// Resolve looks up or creates a SessionState for the given thread ID.
// If creating, generates a new CogOS session ID and sets timestamps.
func (sm *SessionManager) Resolve(threadID string) *SessionState {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if s, ok := sm.sessions[threadID]; ok {
		return s
	}

	now := time.Now()
	s := &SessionState{
		ID:           generateID(),
		ThreadID:     threadID,
		CreatedAt:    now,
		LastActiveAt: now,
		History:      []SessionRotation{},
	}
	sm.sessions[threadID] = s
	return s
}

// ShouldRotate checks rotation policy against the current session state.
// Returns whether rotation is needed and the reason string.
func (sm *SessionManager) ShouldRotate(state *SessionState, threadView *ThreadView) (bool, string) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	// Nothing to rotate if no Claude session is active.
	if state.ClaudeSessionID == "" {
		return false, ""
	}

	if sm.config.MaxTurnsBeforeRotation > 0 && state.TurnCount >= sm.config.MaxTurnsBeforeRotation {
		return true, "pressure:turns"
	}

	if sm.config.MaxTokensBeforeRotation > 0 && state.TotalTokensSent >= sm.config.MaxTokensBeforeRotation {
		return true, "pressure:tokens"
	}

	if sm.config.IdleTimeout > 0 && time.Since(state.LastActiveAt) > sm.config.IdleTimeout {
		return true, "idle"
	}

	return false, ""
}

// Rotate retires the current Claude session, preserving working memory for continuity.
func (sm *SessionManager) Rotate(state *SessionState, reason string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := time.Now()

	if state.ClaudeSessionID != "" {
		rotation := SessionRotation{
			ClaudeSessionID: state.ClaudeSessionID,
			StartedAt:       state.LastActiveAt, // approximate start from last activity
			EndedAt:         now,
			Reason:          reason,
			TurnCount:       state.TurnCount,
		}
		// Use CreatedAt as start if this is the first rotation and History is empty.
		if len(state.History) == 0 {
			rotation.StartedAt = state.CreatedAt
		}
		state.History = append(state.History, rotation)
	}

	state.ClaudeSessionID = ""
	state.TurnCount = 0
	state.TotalTokensSent = 0
	state.LastActiveAt = now
	// WorkingMemory is deliberately preserved — that's the continuity layer.
}

// RecordClaudeSession stores the Claude CLI session ID for the given thread.
// Called when the stream handler receives a session_info/session_start event.
func (sm *SessionManager) RecordClaudeSession(threadID, claudeSessionID string) {
	// Resolve ensures the session exists.
	state := sm.Resolve(threadID)

	sm.mu.Lock()
	defer sm.mu.Unlock()

	state.ClaudeSessionID = claudeSessionID
	state.LastActiveAt = time.Now()
}

// RecordTurn increments turn count and token totals after a successful response.
func (sm *SessionManager) RecordTurn(threadID string, tokensSent int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	state, ok := sm.sessions[threadID]
	if !ok {
		return
	}

	state.TurnCount++
	state.TotalTokensSent += tokensSent
	state.LastActiveAt = time.Now()
}

// UpdateWorkingMemory updates the working memory for a session.
// Called when we extract insights from Claude's response.
func (sm *SessionManager) UpdateWorkingMemory(threadID string, wm *WorkingMemory) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	state, ok := sm.sessions[threadID]
	if !ok {
		return
	}

	state.WorkingMemory = wm
}

// GetSession returns a read-only copy of the session state, or nil if not found.
// For observability and debugging.
func (sm *SessionManager) GetSession(threadID string) *SessionState {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	state, ok := sm.sessions[threadID]
	if !ok {
		return nil
	}
	return state
}
