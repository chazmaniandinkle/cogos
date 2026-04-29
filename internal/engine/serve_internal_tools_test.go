// serve_internal_tools_test.go — coverage for cogos-dev/cogos#94: when a
// kernel-agent chat request lands and the provider emits a tool_use event
// for an MCP-internal tool (cog_*, mod3_*, ...), the kernel must execute
// the tool in-process, append a tool_result message, re-call the provider,
// and surface the final assistant response — not forward the call to a
// dashboard that has no executor for it.
package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptedToolUseProvider is a Provider whose Complete() returns a
// preconfigured sequence of CompletionResponses. Each call advances the
// cursor; the final scripted entry repeats on overflow so a runaway tool
// loop fails loudly rather than panicking in the test harness.
type scriptedToolUseProvider struct {
	name      string
	mu        sync.Mutex
	cursor    int
	scripted  []*CompletionResponse
	requests  []*CompletionRequest // captured for assertions
}

func newScriptedToolUseProvider(name string, scripted ...*CompletionResponse) *scriptedToolUseProvider {
	return &scriptedToolUseProvider{name: name, scripted: scripted}
}

func (p *scriptedToolUseProvider) Name() string  { return p.name }
func (p *scriptedToolUseProvider) Model() string { return "scripted" }
func (p *scriptedToolUseProvider) Available(_ context.Context) bool { return true }
func (p *scriptedToolUseProvider) Capabilities() ProviderCapabilities {
	return ProviderCapabilities{
		Capabilities:     []Capability{CapToolUse},
		MaxContextTokens: 64_000,
		MaxOutputTokens:  4096,
		IsLocal:          true,
	}
}
func (p *scriptedToolUseProvider) Ping(_ context.Context) (time.Duration, error) { return 0, nil }

func (p *scriptedToolUseProvider) Complete(_ context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Deep-copy the messages slice so later mutations by the chat handler
	// (appending tool_result) don't retroactively change what the test
	// observes for earlier turns.
	msgsCopy := make([]ProviderMessage, len(req.Messages))
	copy(msgsCopy, req.Messages)
	captured := *req
	captured.Messages = msgsCopy
	p.requests = append(p.requests, &captured)

	idx := p.cursor
	if idx >= len(p.scripted) {
		idx = len(p.scripted) - 1
	}
	p.cursor++
	resp := *p.scripted[idx]
	return &resp, nil
}

func (p *scriptedToolUseProvider) Stream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
	// Streaming path isn't exercised by this test; return a closed channel
	// so the surface stays compatible.
	ch := make(chan StreamChunk)
	close(ch)
	return ch, nil
}

// TestServerSideExecutionOfInternalCogTool is the primary regression test
// for #94. It scripts a provider that:
//
//  1. First turn: emits a tool_use event for cog_read_cogdoc against a
//     fixture cogdoc the kernel can resolve.
//  2. Second turn: emits a final assistant message that echoes a marker
//     string the test can pin against.
//
// The kernel must execute the tool itself (no client passthrough), append
// the tool_result to the conversation, re-call the provider, and return
// the final assistant message. Acceptance evidence:
//
//   - exactly two provider calls were made (tool_use → final).
//   - the second call's request contains a role=tool message whose content
//     contains the fixture's body (proving CallTool actually ran).
//   - the HTTP response carries the final assistant marker.
func TestServerSideExecutionOfInternalCogTool(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	if srv.mcpServer == nil {
		t.Fatal("test server has no mcpServer wired; #94 path cannot run")
	}
	if !srv.mcpServer.IsInternalTool("cog_read_cogdoc") {
		t.Fatal("mcpServer snapshot missing cog_read_cogdoc; the auto-injection path is broken")
	}

	// Stage a fixture CogDoc the model will "read". The marker string is
	// distinctive so we can prove it round-tripped through the tool result
	// into the conversation and out via the final assistant message.
	const fixtureMarker = "FIXTURE-MARKER-94-3F2A"
	fixtureRel := "semantic/insights/issue-94-fixture.md"
	fixtureAbs := filepath.Join(srv.cfg.WorkspaceRoot, ".cog", "mem", fixtureRel)
	if err := os.MkdirAll(filepath.Dir(fixtureAbs), 0o755); err != nil {
		t.Fatalf("mkdir fixture dir: %v", err)
	}
	fixtureBody := "---\ntitle: issue 94 fixture\n---\n\n" + fixtureMarker + "\n"
	if err := os.WriteFile(fixtureAbs, []byte(fixtureBody), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	args := map[string]any{"uri": "cog://mem/" + fixtureRel}
	argsRaw, _ := json.Marshal(args)

	scripted := []*CompletionResponse{
		{
			Content:    "",
			StopReason: "tool_use",
			ToolCalls: []ToolCall{{
				ID:        "call_1",
				Name:      "cog_read_cogdoc",
				Arguments: string(argsRaw),
			}},
			ProviderMeta: ProviderMeta{Provider: "scripted", Model: "scripted"},
		},
		{
			Content:    "I read the fixture and saw " + fixtureMarker + ".",
			StopReason: "end_turn",
			ProviderMeta: ProviderMeta{Provider: "scripted", Model: "scripted"},
		},
	}

	prov := newScriptedToolUseProvider("scripted", scripted...)
	router := NewSimpleRouter(RoutingConfig{Default: "scripted"})
	router.RegisterProvider(prov)
	srv.SetRouter(router)

	body := `{"model":"kernel-agent","messages":[{"role":"user","content":"please read the fixture"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleChat(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", w.Code, w.Body.String())
	}

	// 1. Two provider calls: turn-1 (tool_use) and turn-2 (final assistant).
	if got := len(prov.requests); got != 2 {
		t.Fatalf("provider was called %d time(s); want 2 (tool_use + final)", got)
	}

	// 2. The second request must include a role=tool message carrying the
	//    fixture's body — proving the kernel executed the tool itself
	//    rather than forwarding to a (nonexistent) client executor.
	turn2 := prov.requests[1]
	var toolMsg *ProviderMessage
	for i := range turn2.Messages {
		if turn2.Messages[i].Role == "tool" && turn2.Messages[i].ToolCallID == "call_1" {
			toolMsg = &turn2.Messages[i]
			break
		}
	}
	if toolMsg == nil {
		roles := make([]string, 0, len(turn2.Messages))
		for _, m := range turn2.Messages {
			roles = append(roles, m.Role)
		}
		t.Fatalf("turn-2 messages missing role=tool entry for call_1; roles=%v", roles)
	}
	if !strings.Contains(toolMsg.Content, fixtureMarker) {
		t.Errorf("tool_result content missing fixture marker %q; got %q",
			fixtureMarker, toolMsg.Content)
	}

	// 3. The HTTP response surfaces the final assistant message — the user
	//    sees a real reply instead of the silent turn-end #94 documents.
	var resp oaiChatResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message == nil {
		t.Fatalf("response missing assistant choice; raw=%s", w.Body.String())
	}
	final := extractContent(resp.Choices[0].Message.Content)
	if !strings.Contains(final, fixtureMarker) {
		t.Errorf("final assistant content missing fixture marker %q; got %q",
			fixtureMarker, final)
	}
	if resp.Choices[0].FinishReason == nil || *resp.Choices[0].FinishReason != "stop" {
		got := "<nil>"
		if resp.Choices[0].FinishReason != nil {
			got = *resp.Choices[0].FinishReason
		}
		t.Errorf("finish_reason = %q; want stop (tool_calls would mean we forwarded the cog_* call)", got)
	}
}

// TestSplitToolCallsByOwnership pins the partitioning helper #94 introduces
// against three cases: a kernel MCP tool (cog_read_cogdoc), a known
// client-only name (browser_click), and a nil MCPServer (chat path before
// the snapshot was wired — must fall through to client forwarding).
func TestSplitToolCallsByOwnership(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	if srv.mcpServer == nil {
		t.Fatal("test server has no mcpServer; partition test cannot run")
	}

	calls := []ToolCall{
		{ID: "1", Name: "cog_read_cogdoc", Arguments: "{}"},
		{ID: "2", Name: "browser_click", Arguments: "{}"},
	}
	internal, external := splitToolCallsByOwnership(calls, srv.mcpServer)
	if len(internal) != 1 || internal[0].Name != "cog_read_cogdoc" {
		t.Errorf("internal = %v; want [cog_read_cogdoc]", internal)
	}
	if len(external) != 1 || external[0].Name != "browser_click" {
		t.Errorf("external = %v; want [browser_click]", external)
	}

	// Nil MCPServer → everything is external (pre-snapshot fallback).
	internal2, external2 := splitToolCallsByOwnership(calls, nil)
	if len(internal2) != 0 {
		t.Errorf("nil MCPServer must produce no internal calls; got %v", internal2)
	}
	if len(external2) != 2 {
		t.Errorf("nil MCPServer must forward all calls externally; got %d", len(external2))
	}
}
