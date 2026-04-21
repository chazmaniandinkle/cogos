package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newServeServerWithContextEngine creates a serveServer with workspace AND
// context engine populated, so /v1/context/build handler paths through cleanly.
func newServeServerWithContextEngine(t *testing.T) *serveServer {
	t.Helper()
	root := makeTempWorkspace(t)
	s := &serveServer{
		busBroker:     newBusEventBroker(),
		toolBridge:    NewToolBridge(),
		contextEngine: NewContextEngine(root),
		workspaces: map[string]*workspaceContext{
			"default": {root: root, name: "default"},
		},
		defaultWS: "default",
	}
	return s
}

// postContextBuild sends a POST to /v1/context/build and returns the recorder.
func postContextBuild(s *serveServer, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/context/build", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleContextBuild(w, req)
	return w
}

// --- Tests ---

func TestHandleContextBuild_MalformedJSON(t *testing.T) {
	s := newServeServerWithContextEngine(t)
	w := postContextBuild(s, `{not json}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
	}
	resp := decodeErrorResponse(t, w)
	if !strings.Contains(resp.Error.Message, "Invalid JSON") {
		t.Errorf("error message=%q, want contains 'Invalid JSON'", resp.Error.Message)
	}
	if resp.Error.Type != "invalid_request" {
		t.Errorf("error type=%q, want 'invalid_request'", resp.Error.Type)
	}
}

func TestHandleContextBuild_NoUserMessage(t *testing.T) {
	s := newServeServerWithContextEngine(t)
	// Messages array with only a system message — no user turn.
	body := `{"messages":[{"role":"system","content":"you are a helpful assistant"}]}`
	w := postContextBuild(s, body)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
	}
	resp := decodeErrorResponse(t, w)
	if !strings.Contains(resp.Error.Message, "No user message") {
		t.Errorf("error message=%q, want contains 'No user message'", resp.Error.Message)
	}
}

func TestHandleContextBuild_NoContextEngine(t *testing.T) {
	// Deliberately omit contextEngine — handler must 503.
	s := &serveServer{
		busBroker:  newBusEventBroker(),
		toolBridge: NewToolBridge(),
	}
	body := `{"messages":[{"role":"user","content":"hi"}]}`
	w := postContextBuild(s, body)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	resp := decodeErrorResponse(t, w)
	if !strings.Contains(resp.Error.Message, "Context engine not initialized") {
		t.Errorf("error message=%q, want contains 'Context engine not initialized'", resp.Error.Message)
	}
	if resp.Error.Type != "unavailable" {
		t.Errorf("error type=%q, want 'unavailable'", resp.Error.Type)
	}
}

func TestHandleContextBuild_MethodNotAllowed(t *testing.T) {
	s := newServeServerWithContextEngine(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/context/build", nil)
	w := httptest.NewRecorder()
	s.handleContextBuild(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleContextBuild_BasicRequest(t *testing.T) {
	s := newServeServerWithContextEngine(t)
	body := `{
		"model": "claude",
		"messages": [
			{"role": "system", "content": "you are a helpful assistant"},
			{"role": "user", "content": "hello there"}
		]
	}`
	w := postContextBuild(s, body)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type=%q, want application/json", ct)
	}

	var resp ContextBuildResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Core invariants
	if resp.SessionID == "" {
		t.Errorf("session_id is empty")
	}
	if resp.Prompt == "" && resp.SystemPrompt == "" {
		t.Errorf("both prompt and system_prompt are empty; at least one should be populated")
	}

	// Strategy should be one of the documented values
	switch resp.Strategy {
	case "", "resume", "fresh", "fallback":
		// ok
	default:
		t.Errorf("strategy=%q, want one of (empty, resume, fresh, fallback)", resp.Strategy)
	}
}

// TestHandleContextBuild_NilKernelWithUCPHeaders guards the A1 regression:
// when kernel is nil but contextEngine is populated, UCP headers must NOT
// produce a spurious 400. The handler skips UCP schema validation (which
// requires a workspace root) and serves the request via the flat-message
// path. TAA diagnostics downgrade to Enabled=true with empty tiers rather
// than crashing on a missing workspace root.
func TestHandleContextBuild_NilKernelWithUCPHeaders(t *testing.T) {
	s := newServeServerWithContextEngine(t) // fixture leaves s.kernel == nil
	body := `{"messages":[{"role":"user","content":"hello there"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/context/build", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-UCP-TAA", `{"version":"1.0","profile":"test","total_tokens":0,"tiers":{"tier1_identity":{"enabled":true,"budget":0},"tier2_temporal":{"enabled":true,"budget":0},"tier3_present":{"enabled":true,"budget":0},"tier4_semantic":{"enabled":true,"budget":0}}}`)
	req.Header.Set("X-TAA-Profile", "openclaw")
	w := httptest.NewRecorder()
	s.handleContextBuild(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 (nil-kernel + UCP headers must not 400)", w.Code, w.Body.String())
	}
	var resp ContextBuildResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if resp.TAA == nil {
		t.Fatalf("resp.TAA is nil; want non-nil (TAA was requested via X-TAA-Profile)")
	}
	if !resp.TAA.Enabled {
		t.Errorf("resp.TAA.Enabled=false, want true")
	}
}

func TestHandleContextBuild_ExplicitSessionID(t *testing.T) {
	s := newServeServerWithContextEngine(t)
	body := `{"messages":[{"role":"user","content":"question one"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/context/build", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-ID", "test-session-abc")
	w := httptest.NewRecorder()
	s.handleContextBuild(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp ContextBuildResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if resp.SessionID != "test-session-abc" {
		t.Errorf("session_id=%q, want 'test-session-abc'", resp.SessionID)
	}
}
