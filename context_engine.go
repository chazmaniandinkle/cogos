// context_engine.go — CogOS Context Engine orchestrator.
//
// The ContextEngine is the central coordinator that replaces the manual message
// flattening and session lookup in serve_inference.go:83-320. It:
//
//  1. Parses the incoming OpenClaw thread into a normalized ThreadView
//  2. Resolves or creates a CogOS session (independent of OpenClaw's session)
//  3. Decides whether to resume or rotate the Claude CLI session
//  4. Builds a compressed, budget-constrained context window
//
// Call flow:
//
//	serve_inference.go:handleChatCompletions
//	  → contextEngine.Build(messages, sessionID, taaProfile, headers)
//	    → parser.Parse(messages, headers)           → ThreadView
//	    → sessionMgr.Resolve(threadID)              → SessionState
//	    → sessionMgr.ShouldRotate(state, view)      → bool, reason
//	    → compressor.Compress(view, state, profile)  → ContextWindow
//	  ← ContextWindow {Prompt, SystemPrompt, Strategy, ClaudeSession}

package main

import (
	"fmt"
	"log"
)

// ContextEngine orchestrates thread parsing, session management, and context
// compression. Stateless — all persistent state lives in SessionManager.
type ContextEngine struct {
	parser        *ThreadParser
	sessionMgr    *SessionManager
	compressor    *ContextCompressor
	workspaceRoot string
}

// NewContextEngine creates a context engine with default configuration.
func NewContextEngine(workspaceRoot string) *ContextEngine {
	return &ContextEngine{
		parser:        NewThreadParser(),
		sessionMgr:    NewSessionManager(DefaultSessionManagerConfig()),
		compressor:    NewContextCompressor(workspaceRoot),
		workspaceRoot: workspaceRoot,
	}
}

// Build takes raw OpenAI-format messages from OpenClaw and produces a
// ready-to-send ContextWindow for Claude CLI. This is the single entry
// point that replaces serve_inference.go:83-320.
func (e *ContextEngine) Build(
	messages []ChatMessage,
	sessionID string,
	taaProfile string,
	headers RequestHeaders,
) (*ContextWindow, error) {

	// 1. Parse thread into normalized view
	threadView, err := e.parser.Parse(messages, headers)
	if err != nil {
		return nil, fmt.Errorf("thread parser: %w", err)
	}
	if threadView.ThreadID == "" {
		threadView.ThreadID = sessionID
	}

	log.Printf("[context-engine] Parsed thread %s: %d messages, %d user turns, last=%q",
		threadView.ThreadID, len(threadView.Messages), threadView.TurnCount,
		truncateStr(threadView.LastUserMsg, 80))

	// 2. Resolve session state
	session := e.sessionMgr.Resolve(threadView.ThreadID)

	// 3. Check for explicit reset
	if headers.SessionReset {
		e.sessionMgr.Rotate(session, "explicit")
		log.Printf("[context-engine] Explicit session reset for %s", threadView.ThreadID)
	}

	// 4. Check rotation policy
	if shouldRotate, reason := e.sessionMgr.ShouldRotate(session, threadView); shouldRotate {
		e.sessionMgr.Rotate(session, reason)
		log.Printf("[context-engine] Rotating session %s: %s", threadView.ThreadID, reason)
	}

	// 5. Load TAA profile for budget allocation
	profile, err := LoadTAAProfile(e.workspaceRoot, taaProfile)
	if err != nil {
		log.Printf("[context-engine] TAA profile %q not found, using defaults: %v", taaProfile, err)
		profile = nil // compressor uses defaults
	}

	// 6. Build compressed context window
	ctxWindow, err := e.compressor.Compress(threadView, session, profile)
	if err != nil {
		return nil, fmt.Errorf("context compressor: %w", err)
	}

	log.Printf("[context-engine] Built context: strategy=%s tokens=%d claude_session=%s",
		ctxWindow.Strategy, ctxWindow.TotalTokens, truncateStr(ctxWindow.ClaudeSession, 8))

	return ctxWindow, nil
}

// truncateStr shortens a string for logging.
// Named to avoid collision with truncate() in fleet.go.
func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
