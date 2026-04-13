// lifecycle.go — Kernel lifecycle types and session tracking.
//
// LifecycleManager tracks inference sessions independently of the context
// engine's SessionManager. The context engine manages Claude CLI session
// rotation and context compression; the lifecycle manager tracks when to
// inject identity context (first turn only) and when to run post-inference
// hooks (working memory updates, sealing).

package main

import (
	"sync"
	"time"
)

// ─── Lifecycle Events ───────────────────────────────────────────────────────────

// HookEvent represents a kernel lifecycle event.
type HookEvent int

const (
	EventSessionStart  HookEvent = iota // Session created (first turn)
	EventPreInference                   // Before each inference call
	EventPostInference                  // After each inference call
	EventSessionEnd                     // Session terminated
)

// String returns the event name for logging.
func (e HookEvent) String() string {
	switch e {
	case EventSessionStart:
		return "session_start"
	case EventPreInference:
		return "pre_inference"
	case EventPostInference:
		return "post_inference"
	case EventSessionEnd:
		return "session_end"
	default:
		return "unknown"
	}
}

// ─── Lifecycle Session ──────────────────────────────────────────────────────────

// LifecycleSession tracks the kernel-owned lifecycle of an inference session.
// Distinct from SessionState (context engine) which manages Claude CLI rotation.
// LifecycleSession controls when identity context is injected (first turn)
// and when working memory is updated (every turn).
type LifecycleSession struct {
	ID              string    // Session ID (matches context engine key)
	FirstTurn       bool      // true until first turn completes
	TurnCount       int       // Completed inference turns
	ClaudeSessionID string    // Claude CLI session ID for --resume
	StartedAt       time.Time // Session creation time
	LastActivityAt  time.Time // Last completed turn
	AgentName       string    // From UCP Identity (e.g. "cog", "whirl")
	Origin          string    // Request origin: "http", "cli", "discord"
}

// ─── Lifecycle Manager ──────────────────────────────────────────────────────────

// LifecycleManager tracks active inference sessions for hook dispatch.
// Thread-safe for concurrent HTTP handlers.
type LifecycleManager struct {
	mu       sync.RWMutex
	sessions map[string]*LifecycleSession
}

// NewLifecycleManager creates an empty lifecycle manager.
func NewLifecycleManager() *LifecycleManager {
	return &LifecycleManager{
		sessions: make(map[string]*LifecycleSession),
	}
}

// GetOrCreate returns an existing session or creates a new one.
// The second return value is true when a new session was created (first turn).
func (lm *LifecycleManager) GetOrCreate(sessionID, origin, agentName string) (*LifecycleSession, bool) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	if s, ok := lm.sessions[sessionID]; ok {
		return s, false
	}

	now := time.Now()
	s := &LifecycleSession{
		ID:             sessionID,
		FirstTurn:      true,
		TurnCount:      0,
		StartedAt:      now,
		LastActivityAt: now,
		AgentName:      agentName,
		Origin:         origin,
	}
	lm.sessions[sessionID] = s
	return s, true
}

// Get returns a session by ID, or nil if not found.
func (lm *LifecycleManager) Get(sessionID string) *LifecycleSession {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	return lm.sessions[sessionID]
}

// RecordTurn updates session state after a completed inference turn.
// Clears FirstTurn so subsequent turns skip identity context injection.
func (lm *LifecycleManager) RecordTurn(sessionID, claudeSessionID string) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	s, ok := lm.sessions[sessionID]
	if !ok {
		return
	}

	s.TurnCount++
	s.LastActivityAt = time.Now()
	s.FirstTurn = false

	if claudeSessionID != "" {
		s.ClaudeSessionID = claudeSessionID
	}
}

// End marks a session as ended and removes it from the active set.
func (lm *LifecycleManager) End(sessionID string) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	delete(lm.sessions, sessionID)
}

// ActiveCount returns the number of active sessions.
func (lm *LifecycleManager) ActiveCount() int {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	return len(lm.sessions)
}
