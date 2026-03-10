// tool_bridge.go — Synchronous tool bridge for client-driven agent loops.
//
// When a client like BrowserOS sends a request with tools, the kernel spawns
// Claude CLI. If Claude calls an external tool (one the client wants to execute
// locally), the MCP bridge subprocess blocks and waits for the real result
// instead of returning a canned "delegated to client" response.
//
// Flow:
//
//	Request 1: Client → kernel → start CLI → Claude calls external tool
//	           → MCP bridge blocks on /v1/tool-bridge/pending
//	           → kernel streams tool_calls + [DONE] to client
//	           → CLI stays alive, output channel parked
//
//	Client executes tool locally
//
//	Request 2: Client → kernel (with role:"tool" messages)
//	           → kernel delivers results to blocked MCP bridge
//	           → CLI continues → kernel streams new output on Request 2's SSE
//	           → finish_reason: "stop" + [DONE] → session cleanup
//
// IPC: MCP bridge POST → localhost:{KERNEL_PORT}/v1/tool-bridge/pending (blocks)
// Kernel holds response open until client delivers result via follow-up request.
//
// Timing: Claude CLI invokes MCP tools eagerly (during streaming, before
// message_stop). MCP bridges may arrive before the harness registers calls.
// The waiter mechanism handles this: MCP bridges register as waiters, and
// RegisterCall wakes them when matching calls arrive.

package main

import (
	"log"
	"sync"
	"time"
)

// ToolBridge manages active tool bridge sessions. Each session corresponds to
// a single Claude CLI process that is suspended waiting for external tool results.
type ToolBridge struct {
	mu       sync.Mutex
	sessions map[string]*ToolBridgeSession
}

// ToolBridgeSession tracks a suspended CLI session and its pending tool calls.
type ToolBridgeSession struct {
	SessionID string
	CreatedAt time.Time

	// PendingCalls holds tool calls registered by the harness (via serve.go)
	// that haven't been claimed by an MCP bridge yet.
	PendingCalls []*ToolBridgeCall

	// CallsByID indexes all registered calls by tool_call_id. Unlike PendingCalls,
	// entries persist after WaitForPending dequeues them, so DeliverResult can
	// always find the call regardless of arrival order.
	CallsByID map[string]*ToolBridgeCall

	// Waiters holds MCP bridge requests that arrived before their call was
	// registered. When RegisterCall adds a matching call, the waiter is woken.
	Waiters []*toolBridgeWaiter

	// OutputCh is the parked streaming output channel from the harness.
	// When results are delivered, the resumed handler reads from this channel.
	OutputCh <-chan StreamChunkInference

	// InferReq holds the original inference request for context preservation
	// when resuming the streaming response.
	InferReq *InferenceRequest

	// Cancel kills the CLI process and cleans up the session.
	Cancel func()
}

// toolBridgeWaiter represents an MCP bridge waiting for a call to be registered.
type toolBridgeWaiter struct {
	ToolName string
	Ch       chan *ToolBridgeCall // Receives the matched call
}

// ToolBridgeCall represents a single pending external tool call.
type ToolBridgeCall struct {
	ToolCallID string // Claude's content_block.id (e.g., toolu_01ABC)
	Name       string // Original tool name (no MCP prefix)
	Arguments  string // JSON-encoded arguments

	// ResultCh receives the real result from the client.
	// The MCP bridge blocks on this channel.
	ResultCh chan ToolBridgeResult
}

// ToolBridgeResult is the real tool result from the client.
type ToolBridgeResult struct {
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

// NewToolBridge creates a new tool bridge and starts the reaper goroutine.
func NewToolBridge() *ToolBridge {
	tb := &ToolBridge{
		sessions: make(map[string]*ToolBridgeSession),
	}
	go tb.reaper()
	return tb
}

// EnsureSession creates an empty session if one doesn't already exist.
// Called at inference start when tools are present, so MCP bridges always
// have a session to register waiters against.
func (tb *ToolBridge) EnsureSession(sessionID string) {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	if _, ok := tb.sessions[sessionID]; ok {
		return
	}
	tb.sessions[sessionID] = &ToolBridgeSession{
		SessionID: sessionID,
		CreatedAt: time.Now(),
		CallsByID: make(map[string]*ToolBridgeCall),
	}
	log.Printf("[tool-bridge] Session pre-created: %s", sessionID)
}

// RegisterSession parks a CLI session's output channel for resumption.
// If the session was pre-created by EnsureSession, preserves eagerly-registered
// calls and waiters. Otherwise replaces any existing session.
func (tb *ToolBridge) RegisterSession(sessionID string, outputCh <-chan StreamChunkInference, inferReq *InferenceRequest, cancel func()) {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	if existing, ok := tb.sessions[sessionID]; ok {
		if existing.OutputCh == nil {
			// Upgrading a pre-created session — preserve calls and waiters
			existing.OutputCh = outputCh
			existing.InferReq = inferReq
			existing.Cancel = cancel
			log.Printf("[tool-bridge] Session parked: %s (pending=%d, indexed=%d, waiters=%d)",
				sessionID, len(existing.PendingCalls), len(existing.CallsByID), len(existing.Waiters))
			return
		}
		// Replacing a fully-registered session (re-suspension case)
		log.Printf("[tool-bridge] Replacing existing session %s", sessionID)
		if existing.Cancel != nil {
			existing.Cancel()
		}
	}

	tb.sessions[sessionID] = &ToolBridgeSession{
		SessionID: sessionID,
		CreatedAt: time.Now(),
		CallsByID: make(map[string]*ToolBridgeCall),
		OutputCh:  outputCh,
		InferReq:  inferReq,
		Cancel:    cancel,
	}
	log.Printf("[tool-bridge] Session registered: %s", sessionID)
}

// RegisterCall adds a pending tool call to a session.
// If an MCP bridge is already waiting for this tool name, delivers directly.
// Otherwise queues the call for a future WaitForPending.
// Deduplicates by tool_call_id — if the same ID is registered twice
// (e.g., from both tool_use event and Suspended Done), skip the duplicate.
func (tb *ToolBridge) RegisterCall(sessionID string, call *ToolBridgeCall) {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	sess, ok := tb.sessions[sessionID]
	if !ok {
		log.Printf("[tool-bridge] WARNING: RegisterCall for unknown session %s", sessionID)
		return
	}

	// Deduplicate: skip if this tool_call_id is already registered.
	// Without this, duplicate registrations create separate ResultCh channels,
	// causing DeliverResult and WaitForPending to use different channels → deadlock.
	if _, exists := sess.CallsByID[call.ToolCallID]; exists {
		return
	}

	// Index by ID for DeliverResult
	sess.CallsByID[call.ToolCallID] = call

	// Check if an MCP bridge is already waiting for this tool name
	for i, w := range sess.Waiters {
		if w.ToolName == call.Name {
			// Wake the waiter directly
			sess.Waiters = append(sess.Waiters[:i], sess.Waiters[i+1:]...)
			log.Printf("[tool-bridge] Call registered + waiter matched: session=%s tool=%s id=%s",
				sessionID, call.Name, call.ToolCallID)
			w.Ch <- call
			return
		}
	}

	// No waiter — queue for future WaitForPending
	sess.PendingCalls = append(sess.PendingCalls, call)
	log.Printf("[tool-bridge] Call registered: session=%s tool=%s id=%s (queue=%d)",
		sessionID, call.Name, call.ToolCallID, len(sess.PendingCalls))
}

// WaitForPending returns a pending call matching toolName, or registers a waiter
// and returns a channel that will receive the call when it's registered.
// The caller should select on the returned channel with a timeout.
func (tb *ToolBridge) WaitForPending(sessionID, toolName string) (*ToolBridgeCall, <-chan *ToolBridgeCall) {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	sess, ok := tb.sessions[sessionID]
	if !ok {
		return nil, nil
	}

	// Check if a matching call is already queued
	for i, call := range sess.PendingCalls {
		if call.Name == toolName {
			sess.PendingCalls = append(sess.PendingCalls[:i], sess.PendingCalls[i+1:]...)
			log.Printf("[tool-bridge] Pending call matched immediately: session=%s tool=%s id=%s",
				sessionID, call.Name, call.ToolCallID)
			return call, nil
		}
	}

	// No match yet — register as waiter. RegisterCall will wake us.
	waiter := &toolBridgeWaiter{
		ToolName: toolName,
		Ch:       make(chan *ToolBridgeCall, 1),
	}
	sess.Waiters = append(sess.Waiters, waiter)
	log.Printf("[tool-bridge] MCP bridge waiting: session=%s tool=%s (waiters=%d)",
		sessionID, toolName, len(sess.Waiters))
	return nil, waiter.Ch
}

// DeliverResult sends a tool result to a session by tool_call_id.
// Called by handleChatCompletions when the client sends role:"tool" messages.
// Uses CallsByID (which persists after WaitForPending dequeues) so delivery
// works regardless of whether the MCP bridge arrived first.
func (tb *ToolBridge) DeliverResult(sessionID, toolCallID string, result ToolBridgeResult) bool {
	tb.mu.Lock()
	sess, ok := tb.sessions[sessionID]
	if !ok {
		tb.mu.Unlock()
		log.Printf("[tool-bridge] DeliverResult: unknown session %s", sessionID)
		return false
	}

	call, ok := sess.CallsByID[toolCallID]
	if !ok {
		tb.mu.Unlock()
		log.Printf("[tool-bridge] DeliverResult: tool_call_id %s not found in session %s", toolCallID, sessionID)
		return false
	}
	tb.mu.Unlock()

	log.Printf("[tool-bridge] Delivering result: session=%s tool_call_id=%s (len=%d)", sessionID, toolCallID, len(result.Content))
	call.ResultCh <- result
	return true
}

// GetSession returns the session for a given ID, or nil.
func (tb *ToolBridge) GetSession(sessionID string) *ToolBridgeSession {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	return tb.sessions[sessionID]
}

// CleanupSession removes a session and cancels its CLI process.
// Closes any waiting MCP bridges with nil to unblock them.
func (tb *ToolBridge) CleanupSession(sessionID string) {
	tb.mu.Lock()
	sess, ok := tb.sessions[sessionID]
	if !ok {
		tb.mu.Unlock()
		return
	}
	delete(tb.sessions, sessionID)
	tb.mu.Unlock()

	// Unblock any waiting MCP bridges
	for _, w := range sess.Waiters {
		close(w.Ch)
	}

	if sess.Cancel != nil {
		sess.Cancel()
	}
	log.Printf("[tool-bridge] Session cleaned up: %s", sessionID)
}

// reaper kills orphaned sessions after 5 minutes.
func (tb *ToolBridge) reaper() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		tb.mu.Lock()
		now := time.Now()
		var expired []string
		for id, sess := range tb.sessions {
			if now.Sub(sess.CreatedAt) > 5*time.Minute {
				expired = append(expired, id)
			}
		}
		tb.mu.Unlock()

		for _, id := range expired {
			log.Printf("[tool-bridge] Reaping expired session: %s", id)
			tb.CleanupSession(id)
		}
	}
}
