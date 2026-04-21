package engine

import (
	"encoding/json"
	"testing"
	"time"
)

// seedToolCall appends a tool.call event directly to the session ledger via
// the same path production code uses. Returns the call ID for subsequent
// seedToolResult calls.
func seedToolCall(t *testing.T, root, sessionID, callID, toolName string, when time.Time, extra map[string]interface{}) {
	t.Helper()
	data := map[string]interface{}{
		"call_id":   callID,
		"tool_name": toolName,
		"source":    ToolSourceMCP,
		"ownership": ToolOwnershipKernel,
	}
	for k, v := range extra {
		data[k] = v
	}
	env := &EventEnvelope{
		HashedPayload: EventPayload{
			Type:      "tool.call",
			Timestamp: when.UTC().Format(time.RFC3339),
			SessionID: sessionID,
			Data:      data,
		},
		Metadata: EventMetadata{Source: "test"},
	}
	if err := AppendEvent(root, sessionID, env); err != nil {
		t.Fatalf("seed tool.call: %v", err)
	}
}

func seedToolResult(t *testing.T, root, sessionID, callID, toolName, status string, when time.Time, extra map[string]interface{}) {
	t.Helper()
	data := map[string]interface{}{
		"call_id":       callID,
		"tool_name":     toolName,
		"status":        status,
		"output_length": 0,
		"source":        ToolSourceMCP,
		"duration_ms":   0,
	}
	for k, v := range extra {
		data[k] = v
	}
	env := &EventEnvelope{
		HashedPayload: EventPayload{
			Type:      "tool.result",
			Timestamp: when.UTC().Format(time.RFC3339),
			SessionID: sessionID,
			Data:      data,
		},
		Metadata: EventMetadata{Source: "test"},
	}
	if err := AppendEvent(root, sessionID, env); err != nil {
		t.Fatalf("seed tool.result: %v", err)
	}
}

// ── Stitching ─────────────────────────────────────────────────────────────

func TestQueryPairsCallAndResult(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	now := time.Now().UTC().Add(-1 * time.Minute)
	seedToolCall(t, root, "s1", "call-a", "cog_read_cogdoc", now, nil)
	seedToolResult(t, root, "s1", "call-a", "cog_read_cogdoc", ToolStatusSuccess, now.Add(100*time.Millisecond), map[string]interface{}{
		"duration_ms":   100,
		"output_length": 42,
	})

	result, err := QueryToolCalls(root, ToolCallQuery{SessionID: "s1"})
	if err != nil {
		t.Fatalf("QueryToolCalls: %v", err)
	}
	if result.Count != 1 {
		t.Fatalf("count = %d; want 1", result.Count)
	}
	r := result.Calls[0]
	if r.CallID != "call-a" {
		t.Errorf("call_id = %q", r.CallID)
	}
	if r.Status != ToolStatusSuccess {
		t.Errorf("status = %q; want success", r.Status)
	}
	if r.CompletedAt == "" {
		t.Error("completed_at empty; want populated")
	}
	if r.DurationMs != 100 {
		t.Errorf("duration_ms = %d; want 100", r.DurationMs)
	}
	if r.OutputLength != 42 {
		t.Errorf("output_length = %d; want 42", r.OutputLength)
	}
}

func TestQueryPendingStatusForUnmatched(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	now := time.Now().UTC()
	seedToolCall(t, root, "s1", "call-b", "cog_read_cogdoc", now, nil)

	result, err := QueryToolCalls(root, ToolCallQuery{SessionID: "s1"})
	if err != nil {
		t.Fatalf("QueryToolCalls: %v", err)
	}
	if result.Count != 1 {
		t.Fatalf("count = %d; want 1", result.Count)
	}
	if result.Calls[0].Status != ToolStatusPending {
		t.Errorf("status = %q; want pending", result.Calls[0].Status)
	}
	if result.Calls[0].CompletedAt != "" {
		t.Errorf("completed_at = %q; want empty", result.Calls[0].CompletedAt)
	}
}

func TestQueryFilterByToolNameWildcard(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	now := time.Now().UTC()
	seedToolCall(t, root, "s1", "call-1", "cog_read_cogdoc", now, nil)
	seedToolCall(t, root, "s1", "call-2", "cog_read_tool_calls", now, nil)
	seedToolCall(t, root, "s1", "call-3", "cog_write_cogdoc", now, nil)

	result, err := QueryToolCalls(root, ToolCallQuery{SessionID: "s1", ToolName: "cog_read_*"})
	if err != nil {
		t.Fatalf("QueryToolCalls: %v", err)
	}
	if result.Count != 2 {
		t.Errorf("wildcard match count = %d; want 2", result.Count)
	}
	for _, r := range result.Calls {
		if r.ToolName != "cog_read_cogdoc" && r.ToolName != "cog_read_tool_calls" {
			t.Errorf("unexpected tool_name in result: %q", r.ToolName)
		}
	}
}

func TestQueryFilterByStatus(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	now := time.Now().UTC()
	seedToolCall(t, root, "s1", "c1", "cog_x", now, nil)
	seedToolResult(t, root, "s1", "c1", "cog_x", ToolStatusSuccess, now.Add(time.Millisecond), nil)
	seedToolCall(t, root, "s1", "c2", "cog_x", now, nil)
	seedToolResult(t, root, "s1", "c2", "cog_x", ToolStatusError, now.Add(time.Millisecond), map[string]interface{}{"reason": "boom"})
	seedToolCall(t, root, "s1", "c3", "cog_x", now, nil)
	seedToolResult(t, root, "s1", "c3", "cog_x", ToolStatusRejected, now.Add(time.Millisecond), map[string]interface{}{"reason": "schema"})

	// Filter by status=error → one row.
	result, err := QueryToolCalls(root, ToolCallQuery{SessionID: "s1", Status: ToolStatusError})
	if err != nil {
		t.Fatalf("QueryToolCalls: %v", err)
	}
	if result.Count != 1 {
		t.Fatalf("status=error count = %d; want 1", result.Count)
	}
	if result.Calls[0].CallID != "c2" {
		t.Errorf("filtered call_id = %q; want c2", result.Calls[0].CallID)
	}
	if result.Calls[0].Reason != "boom" {
		t.Errorf("reason = %q; want boom", result.Calls[0].Reason)
	}
}

func TestQueryFilterByCallID(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	now := time.Now().UTC()
	seedToolCall(t, root, "s1", "target", "cog_x", now, nil)
	seedToolCall(t, root, "s1", "other", "cog_x", now, nil)

	result, err := QueryToolCalls(root, ToolCallQuery{SessionID: "s1", CallID: "target"})
	if err != nil {
		t.Fatalf("QueryToolCalls: %v", err)
	}
	if result.Count != 1 {
		t.Fatalf("count = %d; want 1", result.Count)
	}
	if result.Calls[0].CallID != "target" {
		t.Errorf("call_id = %q; want target", result.Calls[0].CallID)
	}
}

func TestQueryIncludeArgsDefault(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	now := time.Now().UTC()
	extra := map[string]interface{}{"arguments": json.RawMessage(`{"uri":"cog://mem/x"}`)}
	seedToolCall(t, root, "s1", "c-args", "cog_read_cogdoc", now, extra)

	// Without include_args, arguments should be omitted.
	result, err := QueryToolCalls(root, ToolCallQuery{SessionID: "s1"})
	if err != nil {
		t.Fatalf("QueryToolCalls: %v", err)
	}
	if result.Calls[0].Arguments != nil {
		t.Errorf("Arguments present by default: %v", string(result.Calls[0].Arguments))
	}

	// With include_args, arguments should be present.
	result, err = QueryToolCalls(root, ToolCallQuery{SessionID: "s1", IncludeArgs: true})
	if err != nil {
		t.Fatalf("QueryToolCalls include_args: %v", err)
	}
	if len(result.Calls[0].Arguments) == 0 {
		t.Fatal("Arguments missing with include_args=true")
	}
	var parsed map[string]string
	if err := json.Unmarshal(result.Calls[0].Arguments, &parsed); err != nil {
		t.Fatalf("parse arguments: %v", err)
	}
	if parsed["uri"] != "cog://mem/x" {
		t.Errorf("parsed uri = %q", parsed["uri"])
	}
}

func TestToolCallsQueryTruncation(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	now := time.Now().UTC()
	for i := 0; i < 150; i++ {
		seedToolCall(t, root, "s1", "call-"+intToStr(i), "cog_x", now.Add(time.Duration(i)*time.Millisecond), nil)
	}
	result, err := QueryToolCalls(root, ToolCallQuery{SessionID: "s1", Limit: 100})
	if err != nil {
		t.Fatalf("QueryToolCalls: %v", err)
	}
	if result.Count != 100 {
		t.Errorf("count = %d; want 100", result.Count)
	}
	if !result.Truncated {
		t.Error("truncated = false; want true")
	}
}

func TestToolCallsQueryOrderAsc(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	base := time.Now().UTC().Add(-1 * time.Hour)
	seedToolCall(t, root, "s1", "c1", "cog_x", base, nil)
	seedToolCall(t, root, "s1", "c2", "cog_x", base.Add(30*time.Second), nil)
	seedToolCall(t, root, "s1", "c3", "cog_x", base.Add(60*time.Second), nil)

	result, err := QueryToolCalls(root, ToolCallQuery{SessionID: "s1", Order: "asc"})
	if err != nil {
		t.Fatalf("QueryToolCalls: %v", err)
	}
	if result.Count != 3 {
		t.Fatalf("count = %d; want 3", result.Count)
	}
	if result.Calls[0].CallID != "c1" || result.Calls[2].CallID != "c3" {
		t.Errorf("asc order wrong: first=%q last=%q", result.Calls[0].CallID, result.Calls[2].CallID)
	}
}

func TestQueryCrossSession(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	now := time.Now().UTC()
	seedToolCall(t, root, "sess-a", "a1", "cog_x", now, nil)
	seedToolCall(t, root, "sess-b", "b1", "cog_x", now.Add(10*time.Second), nil)

	// With no SessionID, both should be returned.
	result, err := QueryToolCalls(root, ToolCallQuery{})
	if err != nil {
		t.Fatalf("QueryToolCalls: %v", err)
	}
	if result.Count != 2 {
		t.Fatalf("cross-session count = %d; want 2", result.Count)
	}

	// With SessionID=sess-a, only a1.
	result, err = QueryToolCalls(root, ToolCallQuery{SessionID: "sess-a"})
	if err != nil {
		t.Fatalf("QueryToolCalls: %v", err)
	}
	if result.Count != 1 || result.Calls[0].CallID != "a1" {
		t.Errorf("session filter wrong: count=%d id=%q", result.Count, result.Calls[0].CallID)
	}
}

func TestQueryToolCallsSinceFilter(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	old := time.Now().UTC().Add(-2 * time.Hour)
	recent := time.Now().UTC().Add(-1 * time.Minute)
	seedToolCall(t, root, "s1", "old", "cog_x", old, nil)
	seedToolCall(t, root, "s1", "recent", "cog_x", recent, nil)

	result, err := QueryToolCalls(root, ToolCallQuery{
		SessionID: "s1",
		Since:     time.Now().UTC().Add(-10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("QueryToolCalls: %v", err)
	}
	if result.Count != 1 {
		t.Fatalf("count = %d; want 1", result.Count)
	}
	if result.Calls[0].CallID != "recent" {
		t.Errorf("call_id = %q; want recent", result.Calls[0].CallID)
	}
}

func TestToolNameMatchesWildcards(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, pattern string
		want          bool
	}{
		{"cog_read_cogdoc", "cog_read_*", true},
		{"cog_read_cogdoc", "*_cogdoc", true},
		{"cog_read_cogdoc", "*read*", true},
		{"cog_read_cogdoc", "cog_write_*", false},
		{"cog_read_cogdoc", "cog_read_cogdoc", true},
		{"cog_read_cogdoc", "cog_read_tool_calls", false},
		{"cog_read_cogdoc", "", true},
	}
	for _, c := range cases {
		got := toolNameMatches(c.name, c.pattern)
		if got != c.want {
			t.Errorf("toolNameMatches(%q, %q) = %v; want %v", c.name, c.pattern, got, c.want)
		}
	}
}

func TestQuerySourceCounts(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	now := time.Now().UTC()
	seedToolCall(t, root, "s1", "m1", "cog_x", now, map[string]interface{}{"source": ToolSourceMCP})
	seedToolCall(t, root, "s1", "m2", "cog_x", now, map[string]interface{}{"source": ToolSourceMCP})
	seedToolCall(t, root, "s1", "o1", "cog_x", now, map[string]interface{}{"source": ToolSourceOpenAI})

	result, err := QueryToolCalls(root, ToolCallQuery{SessionID: "s1"})
	if err != nil {
		t.Fatalf("QueryToolCalls: %v", err)
	}
	if result.SourcesChecked.MCP != 2 {
		t.Errorf("mcp count = %d; want 2", result.SourcesChecked.MCP)
	}
	if result.SourcesChecked.OpenAI != 1 {
		t.Errorf("openai count = %d; want 1", result.SourcesChecked.OpenAI)
	}
}

// intToStr is a tiny local helper so the test file doesn't pull strconv.
func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	sign := ""
	if n < 0 {
		sign = "-"
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return sign + string(digits)
}
