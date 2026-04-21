// serve_agents_test.go — HTTP handler tests for the plural /v1/agents
// surface, the ServeAgentController adapter, and the dashboard-route
// compatibility guard.
//
// All tests are short, race-clean, and use httptest.ResponseRecorder so
// they can run silently in parallel with the rest of the engine suite.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cogos-dev/cogos/internal/engine"
	"github.com/cogos-dev/cogos/internal/linkfeed"
)

// newTestServeAgent returns a ServeAgent with enough fields populated
// that Status()/getMemoryForAPI()/etc. produce sensible values, but
// WITHOUT actually starting the Ollama-driven runLoop. Tests assert
// against the projection not the live loop.
func newTestServeAgent(t *testing.T) *ServeAgent {
	t.Helper()
	root := t.TempDir()
	// Write a minimal identity config so identityName() resolves.
	configDir := filepath.Join(root, ".cog", "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir identity config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "identity.yaml"),
		[]byte("default_identity: cog\n"), 0o644); err != nil {
		t.Fatalf("write identity.yaml: %v", err)
	}
	harness := NewAgentHarness(AgentHarnessConfig{
		OllamaURL: "http://localhost:11434",
		Model:     "gemma4:e4b",
	})
	sa := &ServeAgent{
		root:                 root,
		interval:             3 * time.Minute,
		harness:              harness,
		stopCh:               make(chan struct{}),
		wakeCh:               make(chan struct{}, 1),
		cycleMemory:          newAgentCycleMemory(maxRollingMemory),
		lastRegistrySnapshot: make(map[string]int64),
		startedAt:            time.Now().Add(-5 * time.Minute),
		lastRun:              time.Now().Add(-1 * time.Minute),
		cycleCount:           7,
		lastAction:           "execute",
		lastUrgency:          0.6,
		lastReason:           "new link arrived",
		lastDurMs:            1200,
	}
	return sa
}

// newTestServer wraps a ServeAgent in a serveServer so the HTTP handlers
// have s.agent wired.
func newTestServer(sa *ServeAgent) *serveServer {
	return &serveServer{
		agent: sa,
	}
}

// --- Controller-level tests --------------------------------------------

func TestServeAgentController_ListAgents(t *testing.T) {
	t.Parallel()
	sa := newTestServeAgent(t)
	ctrl := NewServeAgentController(sa)
	list, err := ctrl.ListAgents(context.Background(), false)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("len=%d; want 1", len(list))
	}
	got := list[0]
	if got.AgentID != engine.DefaultAgentID {
		t.Errorf("AgentID=%q; want %q", got.AgentID, engine.DefaultAgentID)
	}
	if got.CycleCount != 7 {
		t.Errorf("CycleCount=%d; want 7", got.CycleCount)
	}
	if got.LastAction != "execute" {
		t.Errorf("LastAction=%q; want execute", got.LastAction)
	}
	if got.Model != "gemma4:e4b" {
		t.Errorf("Model=%q; want gemma4:e4b", got.Model)
	}
	if got.Identity != "cog" {
		t.Errorf("Identity=%q; want cog", got.Identity)
	}
}

func TestServeAgentController_ListAgents_NilAgent(t *testing.T) {
	t.Parallel()
	ctrl := NewServeAgentController(nil)
	if _, err := ctrl.ListAgents(context.Background(), false); err == nil {
		t.Fatal("want ErrAgentUnavailable, got nil")
	}
}

func TestServeAgentController_GetAgent_NotFound(t *testing.T) {
	t.Parallel()
	sa := newTestServeAgent(t)
	ctrl := NewServeAgentController(sa)
	_, err := ctrl.GetAgent(context.Background(), "ghost", false, 0)
	if err != engine.ErrAgentNotFound {
		t.Fatalf("err=%v; want ErrAgentNotFound", err)
	}
}

func TestServeAgentController_GetAgent_Summary(t *testing.T) {
	t.Parallel()
	sa := newTestServeAgent(t)
	ctrl := NewServeAgentController(sa)
	snap, err := ctrl.GetAgent(context.Background(), engine.DefaultAgentID, false, 0)
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if snap.Summary.AgentID != engine.DefaultAgentID {
		t.Errorf("AgentID=%q; want %q", snap.Summary.AgentID, engine.DefaultAgentID)
	}
	if snap.IdentityRef == "" {
		t.Errorf("IdentityRef empty; want cog:agents/identities/identity_cog.md")
	}
	if snap.Traces != nil {
		t.Errorf("Traces=%v; want nil when include_trace=false", snap.Traces)
	}
}

func TestServeAgentController_GetAgent_WithTraces(t *testing.T) {
	t.Parallel()
	sa := newTestServeAgent(t)
	// Write three cycle traces to disk so the controller can read them.
	traceDir := filepath.Join(sa.root, ".cog", ".state", "agent")
	if err := os.MkdirAll(traceDir, 0o755); err != nil {
		t.Fatalf("mkdir trace dir: %v", err)
	}
	traces := []cycleTrace{
		{Cycle: 1, Timestamp: time.Now().Add(-3 * time.Minute), DurationMs: 100, Action: "observe", Urgency: 0.1, Reason: "initial", Observation: "obs-1", Result: "res-1"},
		{Cycle: 2, Timestamp: time.Now().Add(-2 * time.Minute), DurationMs: 200, Action: "execute", Urgency: 0.5, Reason: "new work", Observation: "obs-2", Result: "res-2"},
		{Cycle: 3, Timestamp: time.Now().Add(-1 * time.Minute), DurationMs: 150, Action: "sleep", Urgency: 0.0, Reason: "idle", Observation: "obs-3", Result: ""},
	}
	data, _ := json.MarshalIndent(traces, "", "  ")
	if err := os.WriteFile(filepath.Join(traceDir, "cycle-traces.json"), data, 0o644); err != nil {
		t.Fatalf("write traces: %v", err)
	}

	ctrl := NewServeAgentController(sa)
	snap, err := ctrl.GetAgent(context.Background(), engine.DefaultAgentID, true, 2)
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if len(snap.Traces) != 2 {
		t.Fatalf("Traces len=%d; want 2", len(snap.Traces))
	}
	// Should be the last two (tail clamp).
	if snap.Traces[1].Cycle != 3 {
		t.Errorf("Traces[1].Cycle=%d; want 3", snap.Traces[1].Cycle)
	}
	if snap.LastObservation == "" {
		t.Errorf("LastObservation empty; want populated")
	}
}

func TestServeAgentController_TriggerAgent_NilAgent(t *testing.T) {
	t.Parallel()
	ctrl := NewServeAgentController(nil)
	_, err := ctrl.TriggerAgent(context.Background(), engine.DefaultAgentID, "", false)
	if err != engine.ErrAgentUnavailable {
		t.Fatalf("err=%v; want ErrAgentUnavailable", err)
	}
}

func TestServeAgentController_TriggerAgent_AlreadyRunning(t *testing.T) {
	t.Parallel()
	sa := newTestServeAgent(t)
	// Simulate an in-progress cycle.
	atomic.StoreInt32(&sa.running, 1)
	defer atomic.StoreInt32(&sa.running, 0)

	ctrl := NewServeAgentController(sa)
	result, err := ctrl.TriggerAgent(context.Background(), engine.DefaultAgentID, "", false)
	if err != nil {
		t.Fatalf("TriggerAgent: %v", err)
	}
	if result.Triggered {
		t.Errorf("Triggered=true; want false (already_running)")
	}
	if result.Message != "already_running" {
		t.Errorf("Message=%q; want already_running", result.Message)
	}
}

// --- HTTP handler tests ------------------------------------------------

func TestHandleListAgents_Roundtrip(t *testing.T) {
	t.Parallel()
	sa := newTestServeAgent(t)
	s := newTestServer(sa)

	req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	w := httptest.NewRecorder()
	s.handleListAgents(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d; want 200", w.Code)
	}
	var resp engine.ListAgentsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, w.Body.String())
	}
	if resp.Count != 1 {
		t.Fatalf("Count=%d; want 1", resp.Count)
	}
	if resp.Agents[0].AgentID != engine.DefaultAgentID {
		t.Errorf("AgentID=%q; want %q", resp.Agents[0].AgentID, engine.DefaultAgentID)
	}
}

func TestHandleListAgents_NilAgent_503(t *testing.T) {
	t.Parallel()
	s := newTestServer(nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	w := httptest.NewRecorder()
	s.handleListAgents(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d; want 503", w.Code)
	}
}

func TestHandleGetAgent_Primary(t *testing.T) {
	t.Parallel()
	sa := newTestServeAgent(t)
	s := newTestServer(sa)

	req := httptest.NewRequest(http.MethodGet, "/v1/agents/primary", nil)
	w := httptest.NewRecorder()
	s.handleGetAgent(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d; body=%s", w.Code, w.Body.String())
	}
	var snap engine.AgentSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Summary.AgentID != engine.DefaultAgentID {
		t.Errorf("AgentID=%q; want %q", snap.Summary.AgentID, engine.DefaultAgentID)
	}
}

func TestHandleGetAgent_NotFound(t *testing.T) {
	t.Parallel()
	sa := newTestServeAgent(t)
	s := newTestServer(sa)

	req := httptest.NewRequest(http.MethodGet, "/v1/agents/ghost", nil)
	w := httptest.NewRecorder()
	s.handleGetAgent(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d; want 404", w.Code)
	}
}

func TestHandleGetAgent_InvalidID(t *testing.T) {
	t.Parallel()
	sa := newTestServeAgent(t)
	s := newTestServer(sa)

	req := httptest.NewRequest(http.MethodGet, "/v1/agents/BAD_ID", nil)
	w := httptest.NewRecorder()
	s.handleGetAgent(w, req)

	// Uppercase is rejected by the regex.
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d; want 400", w.Code)
	}
}

func TestHandleGetAgent_Traces(t *testing.T) {
	t.Parallel()
	sa := newTestServeAgent(t)
	// Drop two traces on disk.
	traceDir := filepath.Join(sa.root, ".cog", ".state", "agent")
	if err := os.MkdirAll(traceDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	traces := []cycleTrace{
		{Cycle: 1, Timestamp: time.Now(), Action: "observe", Urgency: 0.1},
		{Cycle: 2, Timestamp: time.Now(), Action: "execute", Urgency: 0.5},
	}
	data, _ := json.Marshal(traces)
	if err := os.WriteFile(filepath.Join(traceDir, "cycle-traces.json"), data, 0o644); err != nil {
		t.Fatalf("write traces: %v", err)
	}

	s := newTestServer(sa)
	req := httptest.NewRequest(http.MethodGet, "/v1/agents/primary/traces", nil)
	w := httptest.NewRecorder()
	s.handleGetAgent(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d; body=%s", w.Code, w.Body.String())
	}
	var got []cycleTrace
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len=%d; want 2", len(got))
	}
}

func TestHandleAgentTick_FireAndForget(t *testing.T) {
	t.Parallel()
	sa := newTestServeAgent(t)
	// Replace Ollama URL with a stub server so safeCycle doesn't hang on
	// the real network. The goroutine's failure path is acceptable here —
	// we assert on the trigger-receipt response, not the cycle outcome.
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(stub.Close)
	sa.harness.ollamaURL = stub.URL

	s := newTestServer(sa)
	req := httptest.NewRequest(http.MethodPost, "/v1/agents/primary/tick", nil)
	w := httptest.NewRecorder()
	s.handleAgentTick(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d; body=%s", w.Code, w.Body.String())
	}
	var result engine.AgentTriggerResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !result.Triggered {
		t.Errorf("Triggered=false; want true")
	}
	if result.Message != "triggered" {
		t.Errorf("Message=%q; want triggered", result.Message)
	}
	// Wait briefly for the async cycle goroutine to drain so the test
	// doesn't leak; this exercise doesn't care what the goroutine did.
	time.Sleep(50 * time.Millisecond)
}

func TestHandleAgentTick_AlreadyRunning_Returns409(t *testing.T) {
	t.Parallel()
	sa := newTestServeAgent(t)
	atomic.StoreInt32(&sa.running, 1)
	defer atomic.StoreInt32(&sa.running, 0)

	s := newTestServer(sa)
	req := httptest.NewRequest(http.MethodPost, "/v1/agents/primary/tick", nil)
	w := httptest.NewRecorder()
	s.handleAgentTick(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d; want 409", w.Code)
	}
	var result engine.AgentTriggerResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Triggered {
		t.Errorf("Triggered=true; want false")
	}
	if result.Message != "already_running" {
		t.Errorf("Message=%q; want already_running", result.Message)
	}
}

func TestHandleAgentTick_NoAgent_Returns503(t *testing.T) {
	t.Parallel()
	s := newTestServer(nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/agents/primary/tick", nil)
	w := httptest.NewRecorder()
	s.handleAgentTick(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d; want 503", w.Code)
	}
}

// --- Dashboard compatibility regression guard ---------------------------

// TestAgentRoutesPreservedForDashboard asserts that the legacy singular
// routes still return the shapes the embedded dashboard.html consumes.
// The dashboard issues:
//   GET  /v1/agent/status   → AgentStatusResponse (`alive`, `running`, `cycle_count`, `last_cycle`, `last_action`, `last_urgency`, `interval`, `model`, `activity`, `memory`, `proposals`, `inbox`)
//   GET  /v1/agent/traces   → []cycleTrace (each with `cycle`, `action`, `urgency`, `reason`, `target`, `observation`, `result`, `duration_ms`, `timestamp`)
//   POST /v1/agent/trigger  → {"status":"triggered"} | {"status":"already_running"} (409) | {"error":"agent not running"} (503)
//
// If anyone ever breaks these shapes this test fails loudly, and the
// dashboard at internal/engine/web/dashboard.html:1549/1573/1785 would
// break in the same way.
func TestAgentRoutesPreservedForDashboard(t *testing.T) {
	t.Parallel()
	sa := newTestServeAgent(t)
	// Drop a trace on disk so /v1/agent/traces has something to return.
	traceDir := filepath.Join(sa.root, ".cog", ".state", "agent")
	if err := os.MkdirAll(traceDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	trace := cycleTrace{
		Cycle:       9,
		Timestamp:   time.Now(),
		DurationMs:  321,
		Action:      "observe",
		Urgency:     0.3,
		Reason:      "dashboard",
		Target:      "",
		Observation: "watch",
		Result:      "",
	}
	data, _ := json.Marshal([]cycleTrace{trace})
	if err := os.WriteFile(filepath.Join(traceDir, "cycle-traces.json"), data, 0o644); err != nil {
		t.Fatalf("write traces: %v", err)
	}

	s := newTestServer(sa)

	// --- /v1/agent/status -------------------------------------------------
	req := httptest.NewRequest(http.MethodGet, "/v1/agent/status", nil)
	w := httptest.NewRecorder()
	s.handleAgentStatus(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/v1/agent/status: status=%d; want 200", w.Code)
	}
	// Decode into the root-package shape to guarantee the same field set.
	var status AgentStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatalf("/v1/agent/status decode: %v", err)
	}
	// Dashboard.html:1551-1557 reads these directly.
	if !status.Alive {
		t.Errorf("Alive=false; want true")
	}
	if status.CycleCount != 7 {
		t.Errorf("CycleCount=%d; want 7", status.CycleCount)
	}
	if status.LastAction != "execute" {
		t.Errorf("LastAction=%q; want execute", status.LastAction)
	}
	if status.Model != "gemma4:e4b" {
		t.Errorf("Model=%q; want gemma4:e4b", status.Model)
	}
	if status.Interval != "3m0s" {
		t.Errorf("Interval=%q; want 3m0s", status.Interval)
	}

	// Guard the exact JSON field names the dashboard's renderAgent* helpers use.
	needBodyFields := []string{
		`"alive":`, `"running":`, `"cycle_count":`, `"last_cycle":`,
		`"last_action":`, `"last_urgency":`, `"last_reason":`,
		`"last_duration_ms":`, `"interval":`, `"model":`,
	}
	body := w.Body.String()
	for _, f := range needBodyFields {
		if !strings.Contains(body, f) {
			t.Errorf("/v1/agent/status body missing %q\nfull body: %s", f, body)
		}
	}

	// --- /v1/agent/traces --------------------------------------------------
	req = httptest.NewRequest(http.MethodGet, "/v1/agent/traces", nil)
	w = httptest.NewRecorder()
	s.handleAgentTraces(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/v1/agent/traces: status=%d; want 200", w.Code)
	}
	// Dashboard.html:1577 reads `t.cycle + ':' + t.action`; renderAgentTraces uses
	// t.urgency, t.duration_ms, t.timestamp, t.reason, t.target, t.result, t.observation.
	var traces []cycleTrace
	if err := json.Unmarshal(w.Body.Bytes(), &traces); err != nil {
		t.Fatalf("/v1/agent/traces decode: %v", err)
	}
	if len(traces) != 1 {
		t.Fatalf("traces len=%d; want 1", len(traces))
	}
	if traces[0].Cycle != 9 || traces[0].Action != "observe" {
		t.Errorf("trace mismatch: %+v", traces[0])
	}
	tracesBody := w.Body.String()
	for _, f := range []string{
		`"cycle":9`, `"action":"observe"`, `"urgency":0.3`,
		`"reason":"dashboard"`, `"duration_ms":321`, `"observation":"watch"`,
	} {
		if !strings.Contains(tracesBody, f) {
			t.Errorf("/v1/agent/traces body missing %q\nfull body: %s", f, tracesBody)
		}
	}

	// --- POST /v1/agent/trigger (success path: 200 {"status":"triggered"}) ---
	// Point the harness at a stub so safeCycle doesn't hang on real Ollama.
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(stub.Close)
	sa.harness.ollamaURL = stub.URL

	req = httptest.NewRequest(http.MethodPost, "/v1/agent/trigger", nil)
	w = httptest.NewRecorder()
	s.handleAgentTrigger(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/v1/agent/trigger status=%d; want 200", w.Code)
	}
	var triggerResp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &triggerResp); err != nil {
		t.Fatalf("/v1/agent/trigger decode: %v", err)
	}
	if triggerResp["status"] != "triggered" {
		t.Errorf("/v1/agent/trigger status=%q; want triggered (dashboard.html:1785 sends POST and expects this success shape)", triggerResp["status"])
	}
	time.Sleep(50 * time.Millisecond) // let the async cycle goroutine drain

	// --- POST /v1/agent/trigger (conflict path: 409 {"status":"already_running"}) ---
	atomic.StoreInt32(&sa.running, 1)
	req = httptest.NewRequest(http.MethodPost, "/v1/agent/trigger", nil)
	w = httptest.NewRecorder()
	s.handleAgentTrigger(w, req)
	atomic.StoreInt32(&sa.running, 0)
	if w.Code != http.StatusConflict {
		t.Fatalf("/v1/agent/trigger conflict status=%d; want 409", w.Code)
	}
	var conflict map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &conflict); err != nil {
		t.Fatalf("/v1/agent/trigger conflict decode: %v", err)
	}
	if conflict["status"] != "already_running" {
		t.Errorf("conflict status=%q; want already_running", conflict["status"])
	}

	// --- POST /v1/agent/trigger (no agent: 503 {"error":"agent not running"}) ---
	nilSrv := newTestServer(nil)
	req = httptest.NewRequest(http.MethodPost, "/v1/agent/trigger", nil)
	w = httptest.NewRecorder()
	nilSrv.handleAgentTrigger(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("/v1/agent/trigger no-agent status=%d; want 503", w.Code)
	}
	var unavail map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &unavail)
	if unavail["error"] != "agent not running" {
		t.Errorf("no-agent error=%q; want 'agent not running'", unavail["error"])
	}

	// --- /v1/agent/status with no agent returns the Alive:false shape ---
	req = httptest.NewRequest(http.MethodGet, "/v1/agent/status", nil)
	w = httptest.NewRecorder()
	nilSrv.handleAgentStatus(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/v1/agent/status no-agent status=%d; want 200", w.Code)
	}
	var noAgent AgentStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &noAgent); err != nil {
		t.Fatalf("no-agent status decode: %v", err)
	}
	if noAgent.Alive {
		t.Errorf("no-agent Alive=true; want false")
	}
	if noAgent.Model != "none" {
		t.Errorf("no-agent Model=%q; want none", noAgent.Model)
	}
}

// TestAgentRoutes_PluralAndSingularAgree cross-checks that the plural
// route's summary field values match the singular route's top-level
// fields, so callers migrating from one to the other see the same data.
func TestAgentRoutes_PluralAndSingularAgree(t *testing.T) {
	t.Parallel()
	sa := newTestServeAgent(t)
	s := newTestServer(sa)

	// GET /v1/agent/status
	r1 := httptest.NewRequest(http.MethodGet, "/v1/agent/status", nil)
	w1 := httptest.NewRecorder()
	s.handleAgentStatus(w1, r1)
	var single AgentStatusResponse
	_ = json.Unmarshal(w1.Body.Bytes(), &single)

	// GET /v1/agents/primary
	r2 := httptest.NewRequest(http.MethodGet, "/v1/agents/primary", nil)
	w2 := httptest.NewRecorder()
	s.handleGetAgent(w2, r2)
	var snap engine.AgentSnapshot
	_ = json.Unmarshal(w2.Body.Bytes(), &snap)

	if snap.Summary.CycleCount != single.CycleCount {
		t.Errorf("plural CycleCount=%d; singular=%d (must match)", snap.Summary.CycleCount, single.CycleCount)
	}
	if snap.Summary.LastAction != single.LastAction {
		t.Errorf("plural LastAction=%q; singular=%q", snap.Summary.LastAction, single.LastAction)
	}
	if snap.Summary.Model != single.Model {
		t.Errorf("plural Model=%q; singular=%q", snap.Summary.Model, single.Model)
	}
	if snap.Summary.Interval != single.Interval {
		t.Errorf("plural Interval=%q; singular=%q", snap.Summary.Interval, single.Interval)
	}
}

// TestExtractAgentIDAndSuffix covers the URL-parsing helper used by the
// handlers directly.
func TestExtractAgentIDAndSuffix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path     string
		wantID   string
		wantSub  string
	}{
		{"/v1/agents/primary", "primary", ""},
		{"/v1/agents/primary/tick", "primary", "tick"},
		{"/v1/agents/primary/traces", "primary", "traces"},
		{"/v1/agents/", "", ""},
		{"/v1/agents", "", ""},
	}
	for _, tc := range cases {
		id, sub := extractAgentIDAndSuffix(tc.path)
		if id != tc.wantID || sub != tc.wantSub {
			t.Errorf("extractAgentIDAndSuffix(%q) = (%q, %q); want (%q, %q)",
				tc.path, id, sub, tc.wantID, tc.wantSub)
		}
	}
}

// TestAgentRoutes_MuxWiring checks that the actual ServeMux wiring
// dispatches to the correct handlers for both singular and plural routes.
// This catches regressions from accidentally deleting a HandleFunc call
// or mis-ordering route registrations.
func TestAgentRoutes_MuxWiring(t *testing.T) {
	t.Parallel()
	sa := newTestServeAgent(t)
	s := newTestServer(sa)

	mux := http.NewServeMux()
	// Exactly the route set registered in serve.go.
	mux.HandleFunc("GET /v1/agent/status", s.handleAgentStatus)
	mux.HandleFunc("POST /v1/agent/trigger", s.handleAgentTrigger)
	mux.HandleFunc("GET /v1/agent/traces", s.handleAgentTraces)
	mux.HandleFunc("GET /v1/agents", s.handleListAgents)
	mux.HandleFunc("GET /v1/agents/", s.handleGetAgent)
	mux.HandleFunc("POST /v1/agents/", s.handleAgentTick)

	testCases := []struct {
		method   string
		path     string
		wantCode int
	}{
		{"GET", "/v1/agent/status", 200},
		{"GET", "/v1/agent/traces", 200},
		{"GET", "/v1/agents", 200},
		{"GET", "/v1/agents/primary", 200},
		{"GET", "/v1/agents/ghost", 404},
	}
	for _, tc := range testCases {
		tc := tc
		t.Run(tc.method+"_"+strings.ReplaceAll(tc.path, "/", "_"), func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != tc.wantCode {
				t.Errorf("%s %s: status=%d; want %d (body=%s)", tc.method, tc.path, w.Code, tc.wantCode, w.Body.String())
			}
		})
	}
}

// TestInboxSummaryMapping makes sure the linkfeed-type → engine-type
// adapter doesn't drop fields. This is the only mapping with non-trivial
// nested slices.
func TestInboxSummaryMapping(t *testing.T) {
	t.Parallel()
	in := &linkfeed.AgentInboxSummary{
		RawCount:      1,
		EnrichedCount: 2,
		FailedCount:   3,
		TotalCount:    6,
		LastPull:      "2026-04-21T10:00:00Z",
		LastPullAgo:   "5m",
		NextPullIn:    "25m",
		RecentEnrichments: []linkfeed.RecentEnrichmentItem{
			{Title: "one", Connections: 1, Ago: "2m"},
			{Title: "two", Connections: 3, Ago: "4m"},
		},
	}
	out := inboxFromStatus(in)
	if out.RawCount != 1 || out.EnrichedCount != 2 || out.TotalCount != 6 {
		t.Errorf("counts mismatch: %+v", out)
	}
	if len(out.RecentEnrichments) != 2 {
		t.Fatalf("RecentEnrichments len=%d; want 2", len(out.RecentEnrichments))
	}
	if out.RecentEnrichments[1].Title != "two" {
		t.Errorf("RecentEnrichments[1].Title=%q; want two", out.RecentEnrichments[1].Title)
	}
	if inboxFromStatus(nil) != nil {
		t.Errorf("inboxFromStatus(nil) should return nil")
	}
}

