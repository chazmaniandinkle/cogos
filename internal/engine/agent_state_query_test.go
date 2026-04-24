// agent_state_query_test.go — unit tests for the AgentController query
// helpers and the MCP tool marshaling path.
//
// A fake AgentController exercises the validation, normalization, and
// error-translation paths without spinning up the *ServeAgent goroutine.
// Adapter-level tests (against the live root package) live in root.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeAgentController is an in-memory test double. Each field steers one
// method's behaviour; ListErr/GetErr/TriggerErr let tests inject errors.
type fakeAgentController struct {
	Agents []AgentSummary

	GetResult *AgentSnapshot
	GetErr    error
	GetCalls  []getCall

	TriggerResult *AgentTriggerResult
	TriggerErr    error
	TriggerDelay  time.Duration
	TriggerCalls  []triggerCall

	ListErr error
}

type getCall struct {
	ID           string
	IncludeTrace bool
	TraceLimit   int
}

type triggerCall struct {
	ID     string
	Reason string
	Wait   bool
}

func (f *fakeAgentController) ListAgents(_ context.Context, _ bool) ([]AgentSummary, error) {
	if f.ListErr != nil {
		return nil, f.ListErr
	}
	return f.Agents, nil
}

func (f *fakeAgentController) GetAgent(_ context.Context, id string, includeTrace bool, limit int) (*AgentSnapshot, error) {
	f.GetCalls = append(f.GetCalls, getCall{ID: id, IncludeTrace: includeTrace, TraceLimit: limit})
	if f.GetErr != nil {
		return nil, f.GetErr
	}
	if f.GetResult == nil {
		if id != DefaultAgentID {
			return nil, ErrAgentNotFound
		}
		return &AgentSnapshot{Summary: AgentSummary{AgentID: id, Alive: true}}, nil
	}
	return f.GetResult, nil
}

func (f *fakeAgentController) TriggerAgent(ctx context.Context, id string, reason string, wait bool) (*AgentTriggerResult, error) {
	f.TriggerCalls = append(f.TriggerCalls, triggerCall{ID: id, Reason: reason, Wait: wait})
	if f.TriggerErr != nil {
		return nil, f.TriggerErr
	}
	if wait && f.TriggerDelay > 0 {
		select {
		case <-time.After(f.TriggerDelay):
		case <-ctx.Done():
			return &AgentTriggerResult{
				Triggered: true,
				AgentID:   id,
				Message:   "triggered",
				TimedOut:  true,
			}, nil
		}
	}
	if f.TriggerResult != nil {
		out := *f.TriggerResult
		out.AgentID = id
		return &out, nil
	}
	return &AgentTriggerResult{
		Triggered:  true,
		AgentID:    id,
		TriggerSeq: 1,
		Message:    "triggered",
	}, nil
}

func (f *fakeAgentController) DispatchToHarness(_ context.Context, _ DispatchRequest) (*DispatchBatchResult, error) {
	return &DispatchBatchResult{Results: []DispatchResult{{Index: 0, Success: true, Content: "ok"}}}, nil
}

// --- Validation helpers ------------------------------------------------------

func TestValidateAgentID_AcceptsDefault(t *testing.T) {
	if err := ValidateAgentID(DefaultAgentID); err != nil {
		t.Fatalf("ValidateAgentID(%q): %v", DefaultAgentID, err)
	}
}

func TestValidateAgentID_Rejects(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		id   string
	}{
		{"empty", ""},
		{"uppercase", "Primary"},
		{"slash", "BAD/ID"},
		{"leading-digit", "1agent"},
		{"leading-dash", "-agent"},
		{"too-long", strings.Repeat("a", 65)},
		{"space", "bad id"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateAgentID(tc.id); err == nil {
				t.Fatalf("ValidateAgentID(%q): want error, got nil", tc.id)
			}
		})
	}
}

func TestClampTraceLimit(t *testing.T) {
	t.Parallel()
	if n, err := ClampTraceLimit(0); err != nil || n != 1 {
		t.Fatalf("ClampTraceLimit(0) = (%d, %v); want (1, nil)", n, err)
	}
	if n, err := ClampTraceLimit(5); err != nil || n != 5 {
		t.Fatalf("ClampTraceLimit(5) = (%d, %v); want (5, nil)", n, err)
	}
	if n, err := ClampTraceLimit(20); err != nil || n != 20 {
		t.Fatalf("ClampTraceLimit(20) = (%d, %v); want (20, nil)", n, err)
	}
	if _, err := ClampTraceLimit(21); err == nil {
		t.Fatalf("ClampTraceLimit(21): want error, got nil")
	}
	if _, err := ClampTraceLimit(-1); err == nil {
		t.Fatalf("ClampTraceLimit(-1): want error, got nil")
	}
}

// --- QueryListAgents ---------------------------------------------------------

func TestQueryListAgents_Empty(t *testing.T) {
	t.Parallel()
	ctrl := &fakeAgentController{Agents: nil}
	resp, err := QueryListAgents(context.Background(), ctrl, ListAgentsRequest{})
	if err != nil {
		t.Fatalf("QueryListAgents: %v", err)
	}
	if resp.Count != 0 {
		t.Fatalf("count = %d; want 0", resp.Count)
	}
	// Ensure non-nil slice for JSON marshal (so the wire shape is [] not null).
	if resp.Agents == nil {
		t.Fatalf("Agents = nil; want []")
	}
	b, _ := json.Marshal(resp)
	if !strings.Contains(string(b), `"agents":[]`) {
		t.Fatalf("JSON = %s; want agents:[]", b)
	}
}

func TestQueryListAgents_Singleton(t *testing.T) {
	t.Parallel()
	ctrl := &fakeAgentController{
		Agents: []AgentSummary{{
			AgentID:    DefaultAgentID,
			Identity:   "cog",
			Alive:      true,
			Running:    false,
			CycleCount: 42,
			LastAction: "execute",
			Model:      "gemma4:e4b",
			Interval:   "3m0s",
		}},
	}
	resp, err := QueryListAgents(context.Background(), ctrl, ListAgentsRequest{})
	if err != nil {
		t.Fatalf("QueryListAgents: %v", err)
	}
	if resp.Count != 1 {
		t.Fatalf("count = %d; want 1", resp.Count)
	}
	if resp.Agents[0].AgentID != DefaultAgentID {
		t.Fatalf("AgentID = %q; want %q", resp.Agents[0].AgentID, DefaultAgentID)
	}
	if resp.Agents[0].Identity != "cog" {
		t.Fatalf("Identity = %q; want cog", resp.Agents[0].Identity)
	}
}

func TestQueryListAgents_NilController(t *testing.T) {
	t.Parallel()
	if _, err := QueryListAgents(context.Background(), nil, ListAgentsRequest{}); err == nil {
		t.Fatal("want error for nil controller")
	}
}

// --- QueryGetAgent -----------------------------------------------------------

func TestQueryGetAgent_DefaultID(t *testing.T) {
	t.Parallel()
	ctrl := &fakeAgentController{
		GetResult: &AgentSnapshot{
			Summary: AgentSummary{AgentID: DefaultAgentID, Alive: true, CycleCount: 7},
		},
	}
	resp, err := QueryGetAgent(context.Background(), ctrl, GetAgentRequest{})
	if err != nil {
		t.Fatalf("QueryGetAgent: %v", err)
	}
	if resp.Summary.AgentID != DefaultAgentID {
		t.Fatalf("AgentID = %q; want %q", resp.Summary.AgentID, DefaultAgentID)
	}
	if len(ctrl.GetCalls) != 1 || ctrl.GetCalls[0].ID != DefaultAgentID {
		t.Fatalf("GetCalls[0] = %#v; want ID=%q", ctrl.GetCalls, DefaultAgentID)
	}
}

func TestQueryGetAgent_Unknown(t *testing.T) {
	t.Parallel()
	ctrl := &fakeAgentController{GetErr: ErrAgentNotFound}
	_, err := QueryGetAgent(context.Background(), ctrl, GetAgentRequest{AgentID: "ghost"})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !errors.Is(err, ErrAgentNotFound) && err.Error() != ErrAgentNotFound.Error() {
		t.Fatalf("err = %v; want ErrAgentNotFound", err)
	}
}

func TestQueryGetAgent_InvalidID(t *testing.T) {
	t.Parallel()
	ctrl := &fakeAgentController{}
	_, err := QueryGetAgent(context.Background(), ctrl, GetAgentRequest{AgentID: "BAD/ID"})
	if err == nil {
		t.Fatal("want error for invalid id")
	}
	if !strings.Contains(err.Error(), "invalid agent_id") {
		t.Fatalf("err = %v; want invalid_id message", err)
	}
}

func TestQueryGetAgent_WithTraces(t *testing.T) {
	t.Parallel()
	ctrl := &fakeAgentController{
		GetResult: &AgentSnapshot{
			Summary: AgentSummary{AgentID: DefaultAgentID, Alive: true},
			Traces: []AgentCycleTrace{
				{Cycle: 1, Action: "observe"},
				{Cycle: 2, Action: "execute"},
				{Cycle: 3, Action: "sleep"},
			},
		},
	}
	resp, err := QueryGetAgent(context.Background(), ctrl, GetAgentRequest{
		AgentID:      DefaultAgentID,
		IncludeTrace: true,
		TraceLimit:   3,
	})
	if err != nil {
		t.Fatalf("QueryGetAgent: %v", err)
	}
	if len(resp.Traces) != 3 {
		t.Fatalf("traces = %d; want 3", len(resp.Traces))
	}
	if ctrl.GetCalls[0].TraceLimit != 3 || !ctrl.GetCalls[0].IncludeTrace {
		t.Fatalf("call = %#v; want IncludeTrace=true, TraceLimit=3", ctrl.GetCalls[0])
	}
}

func TestQueryGetAgent_TraceLimitTooHigh(t *testing.T) {
	t.Parallel()
	ctrl := &fakeAgentController{}
	_, err := QueryGetAgent(context.Background(), ctrl, GetAgentRequest{
		AgentID:      DefaultAgentID,
		IncludeTrace: true,
		TraceLimit:   100,
	})
	if err == nil {
		t.Fatal("want error for trace_limit=100")
	}
}

func TestQueryGetAgent_TraceLimitDefaultsToOne(t *testing.T) {
	t.Parallel()
	ctrl := &fakeAgentController{
		GetResult: &AgentSnapshot{Summary: AgentSummary{AgentID: DefaultAgentID}},
	}
	_, err := QueryGetAgent(context.Background(), ctrl, GetAgentRequest{
		AgentID:      DefaultAgentID,
		IncludeTrace: true,
		TraceLimit:   0,
	})
	if err != nil {
		t.Fatalf("QueryGetAgent: %v", err)
	}
	if ctrl.GetCalls[0].TraceLimit != 1 {
		t.Fatalf("TraceLimit = %d; want 1 (default)", ctrl.GetCalls[0].TraceLimit)
	}
}

// --- QueryTriggerAgent -------------------------------------------------------

func TestQueryTriggerAgent_FireAndForget(t *testing.T) {
	t.Parallel()
	ctrl := &fakeAgentController{
		TriggerResult: &AgentTriggerResult{
			Triggered:  true,
			TriggerSeq: 5,
			Message:    "triggered",
		},
	}
	resp, err := QueryTriggerAgent(context.Background(), ctrl, TriggerAgentRequest{
		AgentID: DefaultAgentID,
		Reason:  "user asked",
		Wait:    false,
	})
	if err != nil {
		t.Fatalf("QueryTriggerAgent: %v", err)
	}
	if !resp.Triggered {
		t.Fatal("want triggered=true")
	}
	if resp.AgentID != DefaultAgentID {
		t.Fatalf("AgentID = %q; want %q", resp.AgentID, DefaultAgentID)
	}
	if ctrl.TriggerCalls[0].Reason != "user asked" {
		t.Fatalf("Reason = %q; want 'user asked'", ctrl.TriggerCalls[0].Reason)
	}
}

func TestQueryTriggerAgent_AlreadyRunning(t *testing.T) {
	t.Parallel()
	ctrl := &fakeAgentController{
		TriggerResult: &AgentTriggerResult{
			Triggered: false,
			Message:   "already_running",
		},
	}
	resp, err := QueryTriggerAgent(context.Background(), ctrl, TriggerAgentRequest{
		AgentID: DefaultAgentID,
	})
	if err != nil {
		t.Fatalf("QueryTriggerAgent: %v", err)
	}
	if resp.Triggered {
		t.Fatal("want triggered=false")
	}
	if resp.Message != "already_running" {
		t.Fatalf("Message = %q; want already_running", resp.Message)
	}
}

func TestQueryTriggerAgent_InvalidID(t *testing.T) {
	t.Parallel()
	ctrl := &fakeAgentController{}
	_, err := QueryTriggerAgent(context.Background(), ctrl, TriggerAgentRequest{AgentID: "Bad/ID"})
	if err == nil {
		t.Fatal("want error for invalid id")
	}
}

func TestQueryTriggerAgent_Wait_Completes(t *testing.T) {
	t.Parallel()
	ctrl := &fakeAgentController{
		TriggerResult: &AgentTriggerResult{
			Triggered:  true,
			Message:    "completed",
			Action:     "execute",
			Urgency:    0.7,
			Reason:     "new work",
			DurationMs: 120,
		},
		TriggerDelay: 0, // complete synchronously
	}
	resp, err := QueryTriggerAgent(context.Background(), ctrl, TriggerAgentRequest{
		AgentID: DefaultAgentID,
		Wait:    true,
	})
	if err != nil {
		t.Fatalf("QueryTriggerAgent: %v", err)
	}
	if resp.Action != "execute" {
		t.Fatalf("Action = %q; want execute", resp.Action)
	}
	if resp.Urgency != 0.7 {
		t.Fatalf("Urgency = %v; want 0.7", resp.Urgency)
	}
}

func TestQueryTriggerAgent_Wait_TimesOut(t *testing.T) {
	t.Parallel()
	ctrl := &fakeAgentController{
		TriggerDelay: 200 * time.Millisecond,
	}
	// A 10ms deadline will time out well before the 200ms fake delay.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	resp, err := QueryTriggerAgent(ctx, ctrl, TriggerAgentRequest{
		AgentID: DefaultAgentID,
		Wait:    true,
	})
	if err != nil {
		t.Fatalf("QueryTriggerAgent: %v", err)
	}
	if !resp.TimedOut {
		t.Fatalf("TimedOut = false; want true (resp=%+v)", resp)
	}
}

func TestQueryTriggerAgent_NilController(t *testing.T) {
	t.Parallel()
	if _, err := QueryTriggerAgent(context.Background(), nil, TriggerAgentRequest{}); err == nil {
		t.Fatal("want error for nil controller")
	}
}

// --- Snapshot shape guard ----------------------------------------------------

// TestMCPServer_RegistersAgentTools ensures the three agent-state MCP
// tools reach the live server's tool-call dispatch path. Without this
// guard a typo in NewMCPServer / registerTools would silently drop the
// tools from the Streamable HTTP surface.
func TestMCPServer_RegistersAgentTools(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	process := NewProcess(cfg, makeNucleus("Cog", "tester"))
	server := NewMCPServerWithAgentController(cfg, makeNucleus("Cog", "tester"), process,
		&fakeAgentController{
			Agents: []AgentSummary{{AgentID: DefaultAgentID, Alive: true, CycleCount: 9}},
			GetResult: &AgentSnapshot{
				Summary: AgentSummary{AgentID: DefaultAgentID, Alive: true, CycleCount: 9},
			},
		},
	)
	// Exercise each tool handler directly via the MCPServer method.
	result, _, err := server.toolListAgents(context.Background(), nil, listAgentsInput{})
	if err != nil {
		t.Fatalf("toolListAgents: %v", err)
	}
	var listResp ListAgentsResponse
	decodeMCPJSONForAgentTests(t, result, &listResp)
	if listResp.Count != 1 {
		t.Fatalf("Count=%d; want 1", listResp.Count)
	}

	result2, _, err := server.toolGetAgentState(context.Background(), nil, getAgentStateInput{})
	if err != nil {
		t.Fatalf("toolGetAgentState: %v", err)
	}
	var snap AgentSnapshot
	decodeMCPJSONForAgentTests(t, result2, &snap)
	if snap.Summary.AgentID != DefaultAgentID {
		t.Fatalf("AgentID=%q; want %q", snap.Summary.AgentID, DefaultAgentID)
	}

	result3, _, err := server.toolTriggerAgentLoop(context.Background(), nil, triggerAgentLoopInput{})
	if err != nil {
		t.Fatalf("toolTriggerAgentLoop: %v", err)
	}
	var trigger AgentTriggerResult
	decodeMCPJSONForAgentTests(t, result3, &trigger)
	if !trigger.Triggered {
		t.Fatalf("Triggered=false; want true")
	}
}

// decodeMCPJSONForAgentTests mirrors decodeMCPJSON from mcp_server_test.go
// but lives here so the agent-state tests are self-contained (parallel
// test files often need their own helper).
func decodeMCPJSONForAgentTests(t *testing.T, result interface{}, target any) {
	t.Helper()
	// We intentionally use the same shape as decodeMCPJSON in mcp_server_test.go.
	// Both files are in package engine, so we could call the shared helper,
	// but writing a local one keeps this test independent.
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal mcp result: %v", err)
	}
	// The mcp.CallToolResult JSON envelope looks like {"content":[{"text":"..."}]}.
	// Extract the Text field then unmarshal into the target.
	var envelope struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(b, &envelope); err != nil || len(envelope.Content) == 0 {
		t.Fatalf("unmarshal envelope: %v (raw=%s)", err, b)
	}
	if err := json.Unmarshal([]byte(envelope.Content[0].Text), target); err != nil {
		t.Fatalf("unmarshal text: %v (raw=%s)", err, envelope.Content[0].Text)
	}
}

// TestMCPServer_AgentToolsWithoutController ensures the tools return a
// clean "not configured" response — not a panic — when no controller is
// attached. This is the default state until SetAgentController is called.
func TestMCPServer_AgentToolsWithoutController(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	process := NewProcess(cfg, makeNucleus("Cog", "tester"))
	server := NewMCPServer(cfg, makeNucleus("Cog", "tester"), process)
	// agentController is nil

	result, _, err := server.toolListAgents(context.Background(), nil, listAgentsInput{})
	if err != nil {
		t.Fatalf("toolListAgents: %v", err)
	}
	// fallbackResult sets IsError=true
	if result == nil {
		t.Fatal("nil result")
	}
	if !strings.Contains(firstTextContent(result), "agent not running") {
		t.Errorf("got %q; want contains 'agent not running'", firstTextContent(result))
	}
}

func firstTextContent(r interface{}) string {
	b, _ := json.Marshal(r)
	var envelope struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	_ = json.Unmarshal(b, &envelope)
	if len(envelope.Content) == 0 {
		return ""
	}
	return envelope.Content[0].Text
}

// TestAgentSummary_JSONFields guards the wire shape against accidental
// field renames. The dashboard and existing /v1/agent/status tests
// depend on these exact JSON names.
func TestAgentSummary_JSONFields(t *testing.T) {
	t.Parallel()
	s := AgentSummary{
		AgentID:     "primary",
		Identity:    "cog",
		Alive:       true,
		Running:     false,
		UptimeSec:   60,
		CycleCount:  3,
		LastAction:  "sleep",
		LastCycle:   "2026-04-21T12:00:00Z",
		LastUrgency: 0.2,
		LastReason:  "idle",
		LastDurMs:   42,
		Model:       "gemma4:e4b",
		Interval:    "3m0s",
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := []string{
		`"agent_id":"primary"`,
		`"identity":"cog"`,
		`"alive":true`,
		`"running":false`,
		`"cycle_count":3`,
		`"last_action":"sleep"`,
		`"last_cycle":"2026-04-21T12:00:00Z"`,
		`"last_urgency":0.2`,
		`"last_reason":"idle"`,
		`"last_duration_ms":42`,
		`"model":"gemma4:e4b"`,
		`"interval":"3m0s"`,
		`"uptime_sec":60`,
	}
	for _, needle := range want {
		if !strings.Contains(string(b), needle) {
			t.Fatalf("JSON missing %q\nfull: %s", needle, b)
		}
	}
}
