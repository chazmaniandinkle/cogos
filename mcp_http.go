// MCP Streamable HTTP Transport (spec 2025-03-26)
//
// Implements the Streamable HTTP transport for the MCP protocol, allowing
// remote clients to connect to the kernel's MCP interface over HTTP.
//
// This replaces the deprecated HTTP+SSE transport from protocol version 2024-11-05.
// All responses use Content-Type: application/json (no SSE streaming needed since
// all tool calls are synchronous). GET returns 405 per spec (server-initiated
// messages not supported).
//
// Endpoints:
//   - POST /mcp   — Client → Server JSON-RPC messages
//   - GET  /mcp   — 405 (no server-initiated streaming)
//   - DELETE /mcp — Session termination
//
// Sessions are created on initialize and expire after 30 minutes of inactivity.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// mcpSession wraps an MCPServer instance with session metadata.
type mcpSession struct {
	server   *MCPServer
	lastUsed time.Time
}

// MCPSessionManager manages HTTP MCP sessions.
type MCPSessionManager struct {
	mu          sync.RWMutex
	sessions    map[string]*mcpSession
	workspaces  map[string]*workspaceContext // name → workspace (shared with serveServer)
	defaultRoot string                       // fallback root when no workspace param
	stopOnce    sync.Once
	stopCh      chan struct{}
}

// NewMCPSessionManager creates a session manager and starts the cleanup goroutine.
func NewMCPSessionManager(workspaces map[string]*workspaceContext, defaultRoot string) *MCPSessionManager {
	m := &MCPSessionManager{
		sessions:    make(map[string]*mcpSession),
		workspaces:  workspaces,
		defaultRoot: defaultRoot,
		stopCh:      make(chan struct{}),
	}
	go m.cleanupLoop()
	return m
}

// rootForRequest returns the workspace root for this request, using the
// workspace context injected by workspaceMiddleware, or falling back to defaultRoot.
func (m *MCPSessionManager) rootForRequest(r *http.Request) string {
	if ws := workspaceFromRequest(r); ws != nil {
		return ws.root
	}
	return m.defaultRoot
}

// busManagerForRequest returns the busSessionManager for this request's workspace,
// or nil if no workspace context or bus is available.
func (m *MCPSessionManager) busManagerForRequest(r *http.Request) *busSessionManager {
	if ws := workspaceFromRequest(r); ws != nil && ws.busChat != nil {
		return ws.busChat.manager
	}
	return nil
}

// ServeHTTP dispatches to POST/GET/DELETE handlers.
func (m *MCPSessionManager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		m.handlePost(w, r)
	case http.MethodGet:
		m.handleGet(w, r)
	case http.MethodDelete:
		m.handleDelete(w, r)
	case http.MethodOptions:
		// CORS preflight is handled by corsMiddleware, just return 204
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "POST, GET, DELETE, OPTIONS")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handlePost processes POST /mcp — client → server messages.
func (m *MCPSessionManager) handlePost(w http.ResponseWriter, r *http.Request) {
	// Validate Content-Type
	ct := r.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}

	// Validate Accept header includes both required types
	accept := r.Header.Get("Accept")
	if !strings.Contains(accept, "application/json") && !strings.Contains(accept, "*/*") {
		http.Error(w, "Accept header must include application/json", http.StatusNotAcceptable)
		return
	}

	// Read body
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB limit
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	// Detect batch (JSON array) vs single message (JSON object)
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) > 0 && trimmed[0] == '[' {
		m.handleBatchPost(w, r, body)
		return
	}

	// Single JSON-RPC message
	var req JSONRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONRPCError(w, nil, ParseError, "Parse error", err.Error())
		return
	}

	if req.JSONRPC != "2.0" {
		writeJSONRPCError(w, req.ID, InvalidRequest, "Invalid Request", "jsonrpc must be '2.0'")
		return
	}

	// Handle initialize — creates a new session
	if req.Method == "initialize" {
		m.handleInitializeHTTP(w, r, &req)
		return
	}

	// All other requests require a valid session
	sessionID := r.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		http.Error(w, "Missing Mcp-Session-Id header", http.StatusBadRequest)
		return
	}

	m.mu.Lock()
	session, ok := m.sessions[sessionID]
	if ok {
		session.lastUsed = time.Now()
	}
	m.mu.Unlock()

	if !ok {
		http.Error(w, "Session not found or expired", http.StatusNotFound)
		return
	}

	// Handle notifications (no ID = notification, return 202)
	if req.ID == nil {
		// Notification — process but don't send response
		session.server.HandleRequest(&req)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Handle request
	result, rpcErr := session.server.HandleRequest(&req)

	if rpcErr != nil {
		writeJSONRPCError(w, req.ID, rpcErr.Code, rpcErr.Message, rpcErr.Data)
		return
	}

	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  result,
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Mcp-Session-Id", sessionID)
	json.NewEncoder(w).Encode(resp)
}

// handleInitializeHTTP creates a new session and returns the initialize result.
func (m *MCPSessionManager) handleInitializeHTTP(w http.ResponseWriter, r *http.Request, req *JSONRPCRequest) {
	busManager := m.busManagerForRequest(r)

	// Create a new MCP server rooted at the resolved workspace
	server := NewMCPServerForHTTP(m.rootForRequest(r), busManager)

	// Process initialize request
	result, rpcErr := server.HandleRequest(req)
	if rpcErr != nil {
		writeJSONRPCError(w, req.ID, rpcErr.Code, rpcErr.Message, rpcErr.Data)
		return
	}

	// Generate session ID
	sessionID := uuid.New().String()

	// Create a bus for this MCP session
	if busManager != nil {
		busID, err := busManager.createMCPBus(sessionID, "mcp")
		if err != nil {
			log.Printf("[mcp-http] failed to create bus for session %s: %v", sessionID, err)
		} else {
			server.busID = busID
			// Emit session init event
			busManager.appendBusEvent(busID, BlockMCPSessionInit, "kernel:mcp", map[string]interface{}{
				"sessionId": sessionID,
			})
		}
	}

	// Store session
	m.mu.Lock()
	m.sessions[sessionID] = &mcpSession{
		server:   server,
		lastUsed: time.Now(),
	}
	m.mu.Unlock()

	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  result,
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Mcp-Session-Id", sessionID)
	json.NewEncoder(w).Encode(resp)
}

// handleBatchPost processes a JSON-RPC batch request (JSON array of messages).
// Per the MCP spec, clients MAY send batch arrays. If all messages are notifications
// (no id), return 202. Otherwise return a JSON array of responses for messages with ids.
func (m *MCPSessionManager) handleBatchPost(w http.ResponseWriter, r *http.Request, body []byte) {
	var batch []JSONRPCRequest
	if err := json.Unmarshal(body, &batch); err != nil {
		writeJSONRPCError(w, nil, ParseError, "Parse error", err.Error())
		return
	}

	if len(batch) == 0 {
		writeJSONRPCError(w, nil, InvalidRequest, "Invalid Request", "empty batch")
		return
	}

	// Check if any message in the batch is "initialize" to determine session handling.
	// If present, process it first to create the session for subsequent messages.
	initIdx := -1
	for i := range batch {
		if batch[i].Method == "initialize" {
			initIdx = i
			break
		}
	}

	var session *mcpSession
	var sessionID string
	var initResult interface{} // pre-computed initialize response

	if initIdx >= 0 {
		initReq := &batch[initIdx]
		busManager := m.busManagerForRequest(r)
		server := NewMCPServerForHTTP(m.rootForRequest(r), busManager)
		result, rpcErr := server.HandleRequest(initReq)
		if rpcErr != nil {
			writeJSONRPCError(w, initReq.ID, rpcErr.Code, rpcErr.Message, rpcErr.Data)
			return
		}
		initResult = result
		sessionID = uuid.New().String()

		// Create a bus for this MCP session
		if busManager != nil {
			busID, err := busManager.createMCPBus(sessionID, "mcp")
			if err != nil {
				log.Printf("[mcp-http] failed to create bus for session %s: %v", sessionID, err)
			} else {
				server.busID = busID
				busManager.appendBusEvent(busID, BlockMCPSessionInit, "kernel:mcp", map[string]interface{}{
					"sessionId": sessionID,
				})
			}
		}

		session = &mcpSession{
			server:   server,
			lastUsed: time.Now(),
		}
		m.mu.Lock()
		m.sessions[sessionID] = session
		m.mu.Unlock()
	} else {
		// Require session header
		sessionID = r.Header.Get("Mcp-Session-Id")
		if sessionID == "" {
			http.Error(w, "Missing Mcp-Session-Id header", http.StatusBadRequest)
			return
		}

		m.mu.Lock()
		var ok bool
		session, ok = m.sessions[sessionID]
		if ok {
			session.lastUsed = time.Now()
		}
		m.mu.Unlock()

		if !ok {
			http.Error(w, "Session not found or expired", http.StatusNotFound)
			return
		}
	}

	// Process each message and collect responses
	var responses []JSONRPCResponse
	hasResponses := false

	for i := range batch {
		req := &batch[i]

		if req.JSONRPC != "2.0" {
			if req.ID != nil {
				hasResponses = true
				responses = append(responses, JSONRPCResponse{
					JSONRPC: "2.0",
					ID:      req.ID,
					Error: &JSONRPCError{
						Code:    InvalidRequest,
						Message: "Invalid Request",
						Data:    "jsonrpc must be '2.0'",
					},
				})
			}
			continue
		}

		// For the initialize message, use the pre-computed result (avoid double dispatch)
		if i == initIdx {
			if req.ID != nil {
				hasResponses = true
				responses = append(responses, JSONRPCResponse{
					JSONRPC: "2.0",
					ID:      req.ID,
					Result:  initResult,
				})
			}
			continue
		}

		// Process through HandleRequest
		result, rpcErr := session.server.HandleRequest(req)

		if req.ID == nil {
			// Notification — no response needed
			continue
		}

		hasResponses = true
		if rpcErr != nil {
			responses = append(responses, JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error: &JSONRPCError{
					Code:    rpcErr.Code,
					Message: rpcErr.Message,
					Data:    rpcErr.Data,
				},
			})
		} else {
			responses = append(responses, JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  result,
			})
		}
	}

	// If batch contained only notifications, return 202 Accepted
	if !hasResponses {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Return array of responses
	w.Header().Set("Content-Type", "application/json")
	if sessionID != "" {
		w.Header().Set("Mcp-Session-Id", sessionID)
	}
	json.NewEncoder(w).Encode(responses)
}

// handleGet returns 405 per spec — server-initiated streaming not supported.
// The spec allows this: "The server MUST either return Content-Type: text/event-stream
// in response to this HTTP GET, or else return HTTP 405 Method Not Allowed."
func (m *MCPSessionManager) handleGet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "POST, DELETE, OPTIONS")
	http.Error(w, "Server does not support server-initiated streaming", http.StatusMethodNotAllowed)
}

// handleDelete processes DELETE /mcp — session termination.
func (m *MCPSessionManager) handleDelete(w http.ResponseWriter, r *http.Request) {
	sessionID := r.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		http.Error(w, "Missing Mcp-Session-Id header", http.StatusBadRequest)
		return
	}

	m.mu.Lock()
	session, ok := m.sessions[sessionID]
	if ok {
		delete(m.sessions, sessionID)
	}
	m.mu.Unlock()

	if !ok {
		http.Error(w, "Session not found", http.StatusNotFound)
		return
	}

	// Emit session end event on the bus
	if ok && session.server.busManager != nil && session.server.busID != "" {
		session.server.busManager.appendBusEvent(session.server.busID, BlockMCPSessionEnd, "kernel:mcp", map[string]interface{}{
			"sessionId": sessionID,
		})
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, `{"ok":true}`)
}

// writeJSONRPCError writes a JSON-RPC error response.
func writeJSONRPCError(w http.ResponseWriter, id interface{}, code int, message string, data interface{}) {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &JSONRPCError{
			Code:    code,
			Message: message,
			Data:    data,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK) // JSON-RPC errors use 200 status
	json.NewEncoder(w).Encode(resp)
}

// cleanupLoop removes expired sessions every 5 minutes.
func (m *MCPSessionManager) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.cleanupSessions()
		case <-m.stopCh:
			return
		}
	}
}

// cleanupSessions removes sessions idle for more than 30 minutes.
func (m *MCPSessionManager) cleanupSessions() {
	cutoff := time.Now().Add(-30 * time.Minute)

	m.mu.Lock()
	defer m.mu.Unlock()

	for id, session := range m.sessions {
		if session.lastUsed.Before(cutoff) {
			delete(m.sessions, id)
		}
	}
}

// SessionCount returns the number of active MCP sessions.
func (m *MCPSessionManager) SessionCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// Stop shuts down the cleanup goroutine.
func (m *MCPSessionManager) Stop() {
	m.stopOnce.Do(func() {
		close(m.stopCh)
	})
}
