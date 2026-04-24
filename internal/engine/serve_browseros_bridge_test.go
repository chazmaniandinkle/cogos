// serve_browseros_bridge_test.go locks in the three-point fix to the
// BrowserOS tool bridge (Phase 24 audit). Each test covers one break
// point from the audit:
//
//  1. System-prompt merge — client system prompt survives the chat
//     handler even when context assembly falls back (no hardcoded
//     overwrite).
//  2. Tool list extensibility — client-owned tools (browser_*) reach
//     the Provider via CompletionRequest.Tools AND appear in
//     ExternalTools so downstream code can distinguish ownership.
//  3. ExternalTools + ToolCalls plumbing — a stubbed provider returning
//     tool_calls propagates through handleChat into the OpenAI response
//     with finish_reason="tool_calls" and a tool_calls array on the
//     assistant message.
package engine

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleChat_PreservesClientSystemPrompt(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	stub := NewStubProvider("stub", "ok")
	router := NewSimpleRouter(RoutingConfig{Default: "stub"})
	router.RegisterProvider(stub)
	srv.SetRouter(router)

	// BrowserOS-style: a system prompt arrives inside messages[]. Even if
	// AssembleContext succeeds (the common path), the ClientSystem text
	// must end up somewhere inside creq.SystemPrompt — not be silently
	// replaced by a kernel-only template.
	body := `{
		"model": "local",
		"messages": [
			{"role": "system", "content": "YOU-ARE-BROWSEROS-AGENT"},
			{"role": "user", "content": "hello"}
		],
		"stream": false
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleChat(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if stub.lastRequest == nil {
		t.Fatal("provider never received a request")
	}
	if !strings.Contains(stub.lastRequest.SystemPrompt, "YOU-ARE-BROWSEROS-AGENT") {
		t.Errorf("SystemPrompt missing client marker:\n%s", stub.lastRequest.SystemPrompt)
	}
	// The role=system message must be stripped from Messages so Anthropic
	// doesn't reject the upstream request.
	for _, m := range stub.lastRequest.Messages {
		if m.Role == "system" {
			t.Errorf("role=system survived into provider.Messages: %+v", m)
		}
	}
}

func TestMergeSystemPrompts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		nucleus string
		clients []string
		want    string
	}{
		{"everything empty", "", nil, ""},
		{"nucleus only", "core", nil, "core"},
		{"clients only", "", []string{"a", "b"}, "a\n\n---\n\nb"},
		{"mix, blanks dropped", "core", []string{"", "  ", "a"}, "core\n\n---\n\na"},
		{"trailing newlines trimmed", "core\n\n", []string{"a\n"}, "core\n\n---\n\na"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := mergeSystemPrompts(c.nucleus, c.clients)
			if got != c.want {
				t.Errorf("mergeSystemPrompts(%q, %v)\ngot  = %q\nwant = %q", c.nucleus, c.clients, got, c.want)
			}
		})
	}
}

func TestClassifyToolOwnership(t *testing.T) {
	t.Parallel()
	internal := []string{"bash", "Bash", "exec", "shell", "read", "write", "edit", "grep", "glob", "find"}
	external := []string{"browser_navigate", "browser_click", "custom_tool", "", "strata_invoke"}

	for _, n := range internal {
		if got := classifyToolOwnership(n); got != ToolOwnershipKernel {
			t.Errorf("classifyToolOwnership(%q) = %s; want kernel", n, got)
		}
	}
	for _, n := range external {
		if got := classifyToolOwnership(n); got != ToolOwnershipClient {
			t.Errorf("classifyToolOwnership(%q) = %s; want client", n, got)
		}
	}
}

func TestHandleChat_PartitionsToolsIntoExternal(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	stub := NewStubProvider("stub", "ok")
	router := NewSimpleRouter(RoutingConfig{Default: "stub"})
	router.RegisterProvider(stub)
	srv.SetRouter(router)

	// BrowserOS-style payload: a mix of internal (bash) and external
	// (browser_navigate, browser_click) tool definitions.
	body := `{
		"model": "local",
		"messages": [{"role": "user", "content": "open google"}],
		"stream": false,
		"tools": [
			{"type": "function", "function": {"name": "bash", "description": "run cmd", "parameters": {}}},
			{"type": "function", "function": {"name": "browser_navigate", "description": "nav", "parameters": {}}},
			{"type": "function", "function": {"name": "browser_click", "description": "click", "parameters": {}}}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleChat(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", w.Code, w.Body.String())
	}
	if stub.lastRequest == nil {
		t.Fatal("provider never received a request")
	}
	if len(stub.lastRequest.Tools) != 3 {
		t.Errorf("Tools = %d; want 3 (all tools forwarded)", len(stub.lastRequest.Tools))
	}
	if len(stub.lastRequest.ExternalTools) != 2 {
		t.Errorf("ExternalTools = %d; want 2 (browser_* only)", len(stub.lastRequest.ExternalTools))
	}
	seen := make(map[string]bool)
	for _, t := range stub.lastRequest.ExternalTools {
		seen[t.Name] = true
	}
	if !seen["browser_navigate"] || !seen["browser_click"] {
		t.Errorf("ExternalTools missing browser_*: %+v", stub.lastRequest.ExternalTools)
	}
	if seen["bash"] {
		t.Error("bash (internal) leaked into ExternalTools")
	}
}

func TestHandleChat_ReturnsToolCalls(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	// Provider returns a tool_use for a browser_* tool.
	stub := NewStubProvider("stub", "")
	stub.toolCalls = []ToolCall{
		{ID: "call_1", Name: "browser_navigate", Arguments: `{"url":"https://example.com"}`},
	}
	router := NewSimpleRouter(RoutingConfig{Default: "stub"})
	router.RegisterProvider(stub)
	srv.SetRouter(router)

	body := `{
		"model": "local",
		"messages": [{"role": "user", "content": "open example"}],
		"stream": false,
		"tools": [
			{"type": "function", "function": {"name": "browser_navigate", "description": "nav", "parameters": {}}}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleChat(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", w.Code, w.Body.String())
	}

	var resp oaiChatResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %d; want 1", len(resp.Choices))
	}
	if resp.Choices[0].FinishReason == nil || *resp.Choices[0].FinishReason != "tool_calls" {
		var got string
		if resp.Choices[0].FinishReason != nil {
			got = *resp.Choices[0].FinishReason
		}
		t.Errorf("finish_reason = %q; want tool_calls", got)
	}
	if resp.Choices[0].Message == nil || len(resp.Choices[0].Message.ToolCalls) == 0 {
		t.Fatalf("Message.ToolCalls empty; got %+v", resp.Choices[0].Message)
	}

	// Decode the raw tool_calls and verify the browser_navigate call made it.
	var calls []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(resp.Choices[0].Message.ToolCalls, &calls); err != nil {
		t.Fatalf("decode ToolCalls: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("decoded tool_calls = %d; want 1", len(calls))
	}
	if calls[0].Function.Name != "browser_navigate" {
		t.Errorf("tool name = %q; want browser_navigate", calls[0].Function.Name)
	}
	if !strings.Contains(calls[0].Function.Arguments, "example.com") {
		t.Errorf("tool arguments missing payload: %q", calls[0].Function.Arguments)
	}
}

func TestHandleChat_StreamingEmitsToolCallsDelta(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	stub := NewStubProvider("stub", "")
	stub.chunks = []string{}
	stub.toolCalls = []ToolCall{
		{ID: "call_s1", Name: "browser_click", Arguments: `{"ref":"x"}`},
	}
	router := NewSimpleRouter(RoutingConfig{Default: "stub"})
	router.RegisterProvider(stub)
	srv.SetRouter(router)

	body := `{
		"model": "local",
		"messages": [{"role": "user", "content": "click"}],
		"stream": true,
		"tools": [
			{"type": "function", "function": {"name": "browser_click", "description": "click", "parameters": {}}}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleChat(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}

	// Scan SSE events — we expect at least one chunk that carries a
	// tool_calls delta and a terminal chunk with finish_reason=tool_calls.
	var sawToolCall, sawFinish bool
	scanner := bufio.NewScanner(w.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		if strings.Contains(data, "tool_calls") && strings.Contains(data, "browser_click") {
			sawToolCall = true
		}
		if strings.Contains(data, `"finish_reason":"tool_calls"`) {
			sawFinish = true
		}
	}
	if !sawToolCall {
		t.Error("stream never emitted a tool_calls delta for browser_click")
	}
	if !sawFinish {
		t.Error("stream never emitted finish_reason=tool_calls")
	}
}
