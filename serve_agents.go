// serve_agents.go — Plural /v1/agents HTTP surface + ServeAgentController
// adapter that satisfies engine.AgentController.
//
// This file is the entire root-package surface for the agent-state API.
// It is additive: the existing /v1/agent/{status,traces,trigger} routes
// remain in agent_serve.go and the dashboard keeps consuming them
// unchanged. The new plural routes project the same ServeAgent state
// onto engine-side value types so MCP + HTTP callers share one schema.
//
// Design references:
//   - cog://mem/semantic/surveys/2026-04-21-consolidation/agent-T-agent-state-design
//   - internal/engine/agent_controller.go (the interface this adapts)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cogos-dev/cogos/internal/engine"
	"github.com/cogos-dev/cogos/internal/linkfeed"
)

// ServeAgentController adapts *ServeAgent to the engine.AgentController
// interface. It is the only root-package piece of the agent-state API;
// everything else is engine-side and additive.
type ServeAgentController struct {
	agent *ServeAgent // may be nil when the kernel has no agent wired
	// Monotonic counter for trigger acknowledgments. Each successful
	// TriggerAgent increments it; response payloads echo the value so
	// callers can correlate async acks.
	triggerSeq int64
}

// NewServeAgentController returns a controller wired to the given
// ServeAgent. nil-safe: if agent is nil, all methods return
// engine.ErrAgentUnavailable.
func NewServeAgentController(agent *ServeAgent) *ServeAgentController {
	return &ServeAgentController{agent: agent}
}

// identityName returns the nucleus identity the kernel is currently
// animating, used as the Identity field on the AgentSummary. Empty when
// no SDK kernel or nucleus is loaded.
func (c *ServeAgentController) identityName() string {
	if c == nil || c.agent == nil {
		return ""
	}
	// ServeAgent doesn't have a direct nucleus reference today. The
	// kernel-wide identity is surfaced via the SDK's /state route, but
	// we don't want to take that path (it requires kernel boot).
	// Resolve from the root config file if present; fall back to env.
	if v := os.Getenv("COG_IDENTITY"); v != "" {
		return v
	}
	// Best-effort read of .cog/config/identity.yaml — a single "name:" line.
	configPath := filepath.Join(c.agent.root, ".cog", "config", "identity.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "default_identity:") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "default_identity:"))
		}
	}
	return ""
}

// summaryFromStatus projects the root-package AgentStatusResponse onto
// the engine-side AgentSummary shape. Every field maps 1:1 from state
// already populated by ServeAgent.Status().
func (c *ServeAgentController) summaryFromStatus(st AgentStatusResponse) engine.AgentSummary {
	return engine.AgentSummary{
		AgentID:     engine.DefaultAgentID,
		Identity:    c.identityName(),
		Alive:       st.Alive,
		Running:     st.Running,
		UptimeSec:   st.UptimeSec,
		CycleCount:  st.CycleCount,
		LastAction:  st.LastAction,
		LastCycle:   st.LastCycle,
		LastUrgency: st.LastUrgency,
		LastReason:  st.LastReason,
		LastDurMs:   st.LastDurMs,
		Model:       st.Model,
		Interval:    st.Interval,
	}
}

// activityFromStatus projects the enriched activity summary onto the
// engine type, or returns nil if no activity is available.
func activityFromStatus(a *AgentActivitySummary) *engine.AgentActivitySummary {
	if a == nil {
		return nil
	}
	return &engine.AgentActivitySummary{
		UserPresence:     a.UserPresence,
		UserLastEventAgo: a.UserLastEventAgo,
		ClaudeCodeActive: a.ClaudeCodeActive,
		ClaudeCodeEvents: a.ClaudeCodeEvents,
		TotalEventDelta:  a.TotalEventDelta,
		HottestBus:       a.HottestBus,
		HottestDelta:     a.HottestDelta,
	}
}

// memoryFromStatus maps the root-package rolling memory entries onto
// the engine type.
func memoryFromStatus(m []AgentMemoryEntry) []engine.AgentMemoryEntry {
	if len(m) == 0 {
		return nil
	}
	out := make([]engine.AgentMemoryEntry, len(m))
	for i, e := range m {
		out[i] = engine.AgentMemoryEntry{
			Cycle:    e.Cycle,
			Action:   e.Action,
			Urgency:  e.Urgency,
			Sentence: e.Sentence,
			Ago:      e.Ago,
		}
	}
	return out
}

// proposalsFromStatus maps pending proposal entries.
func proposalsFromStatus(p []AgentProposalEntry) []engine.AgentProposalEntry {
	if len(p) == 0 {
		return nil
	}
	out := make([]engine.AgentProposalEntry, len(p))
	for i, e := range p {
		out[i] = engine.AgentProposalEntry{
			File:    e.File,
			Title:   e.Title,
			Type:    e.Type,
			Urgency: e.Urgency,
			Created: e.Created,
		}
	}
	return out
}

// inboxFromStatus maps the linkfeed inbox summary onto the engine type.
func inboxFromStatus(in *linkfeed.AgentInboxSummary) *engine.AgentInboxSummary {
	if in == nil {
		return nil
	}
	recent := make([]engine.AgentInboxEnrichItem, len(in.RecentEnrichments))
	for i, r := range in.RecentEnrichments {
		recent[i] = engine.AgentInboxEnrichItem{
			Title:       r.Title,
			Connections: r.Connections,
			Ago:         r.Ago,
		}
	}
	return &engine.AgentInboxSummary{
		RawCount:          in.RawCount,
		EnrichedCount:     in.EnrichedCount,
		FailedCount:       in.FailedCount,
		TotalCount:        in.TotalCount,
		LastPull:          in.LastPull,
		LastPullAgo:       in.LastPullAgo,
		NextPullIn:        in.NextPullIn,
		RecentEnrichments: recent,
	}
}

// readRecentTraces loads the most recent N cycle traces from disk. Returns
// nil when the trace file is absent or unreadable. The traces are already
// persisted as a JSON array in .cog/.state/agent/cycle-traces.json (ring
// of 20) — we just slice the tail.
func (c *ServeAgentController) readRecentTraces(limit int) ([]engine.AgentCycleTrace, string) {
	if c == nil || c.agent == nil {
		return nil, ""
	}
	traceFile := filepath.Join(c.agent.root, ".cog", ".state", "agent", "cycle-traces.json")
	data, err := os.ReadFile(traceFile)
	if err != nil {
		return nil, ""
	}
	var raw []cycleTrace
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, ""
	}
	if len(raw) == 0 {
		return nil, ""
	}
	// Keep the last N in chronological order (oldest→newest). Callers
	// that want newest-first can reverse on the client side if needed.
	if limit > 0 && len(raw) > limit {
		raw = raw[len(raw)-limit:]
	}
	out := make([]engine.AgentCycleTrace, len(raw))
	for i, t := range raw {
		out[i] = engine.AgentCycleTrace{
			Cycle:       t.Cycle,
			Timestamp:   t.Timestamp.Format(time.RFC3339),
			DurationMs:  t.DurationMs,
			Action:      t.Action,
			Urgency:     t.Urgency,
			Reason:      t.Reason,
			Target:      t.Target,
			Observation: t.Observation,
			Result:      t.Result,
		}
	}
	// Use the newest trace's observation as a top-level hint for
	// last_observation.
	lastObs := raw[len(raw)-1].Observation
	return out, lastObs
}

// ListAgents implements engine.AgentController.
func (c *ServeAgentController) ListAgents(ctx context.Context, includeStopped bool) ([]engine.AgentSummary, error) {
	if c == nil || c.agent == nil {
		return nil, engine.ErrAgentUnavailable
	}
	st := c.agent.Status()
	return []engine.AgentSummary{c.summaryFromStatus(st)}, nil
}

// GetAgent implements engine.AgentController.
func (c *ServeAgentController) GetAgent(ctx context.Context, id string, includeTrace bool, traceLimit int) (*engine.AgentSnapshot, error) {
	if c == nil || c.agent == nil {
		return nil, engine.ErrAgentUnavailable
	}
	if id != engine.DefaultAgentID {
		return nil, engine.ErrAgentNotFound
	}
	st := c.agent.Status()
	snap := &engine.AgentSnapshot{
		Summary:   c.summaryFromStatus(st),
		Activity:  activityFromStatus(st.Activity),
		Memory:    memoryFromStatus(st.Memory),
		Proposals: proposalsFromStatus(st.Proposals),
		Inbox:     inboxFromStatus(st.Inbox),
	}
	if ident := snap.Summary.Identity; ident != "" {
		snap.IdentityRef = fmt.Sprintf("cog:agents/identities/identity_%s.md", strings.ToLower(ident))
	}
	if includeTrace {
		traces, lastObs := c.readRecentTraces(traceLimit)
		snap.Traces = traces
		snap.LastObservation = lastObs
	}
	return snap, nil
}

// TriggerAgent implements engine.AgentController.
func (c *ServeAgentController) TriggerAgent(ctx context.Context, id string, reason string, wait bool) (*engine.AgentTriggerResult, error) {
	if c == nil || c.agent == nil {
		return nil, engine.ErrAgentUnavailable
	}
	if id != engine.DefaultAgentID {
		return nil, engine.ErrAgentNotFound
	}

	// Overlap guard — mirror the behaviour of POST /v1/agent/trigger.
	if atomic.LoadInt32(&c.agent.running) == 1 {
		return &engine.AgentTriggerResult{
			Triggered:  false,
			AgentID:    id,
			TriggerSeq: atomic.LoadInt64(&c.triggerSeq),
			Message:    "already_running",
		}, nil
	}

	seq := atomic.AddInt64(&c.triggerSeq, 1)

	if !wait {
		// Fire-and-forget — kick the cycle in a goroutine and return
		// a trigger receipt. Mirror the existing handleAgentTrigger
		// behaviour so the dashboard-facing alias stays byte-compat.
		go c.agent.safeCycle(context.Background())
		if reason != "" {
			log.Printf("[agent-api] tick trigger_seq=%d reason=%q wait=false", seq, reason)
		}
		return &engine.AgentTriggerResult{
			Triggered:  true,
			AgentID:    id,
			TriggerSeq: seq,
			Message:    "triggered",
		}, nil
	}

	// wait=true — block until the cycle finishes, up to a 90s deadline.
	// Snapshot cycle_count before/after to detect completion; we don't
	// modify runCycle itself (out of scope).
	startCount := c.agent.Status().CycleCount
	done := make(chan string, 1)
	go func() {
		done <- c.agent.safeCycle(context.Background())
	}()

	deadline := 90 * time.Second
	timer := time.NewTimer(deadline)
	defer timer.Stop()

	select {
	case action := <-done:
		// Read post-cycle snapshot.
		end := c.agent.Status()
		return &engine.AgentTriggerResult{
			Triggered:  true,
			AgentID:    id,
			TriggerSeq: seq,
			Message:    "completed",
			Action:     action,
			Urgency:    end.LastUrgency,
			Reason:     end.LastReason,
			DurationMs: end.LastDurMs,
			TimedOut:   false,
		}, nil
	case <-timer.C:
		// Cycle still running; acknowledge the timeout but let the
		// goroutine continue. We report the trigger as accepted with
		// TimedOut=true so callers know to poll for the final state.
		_ = startCount // reserved for future cycle_id correlation
		return &engine.AgentTriggerResult{
			Triggered:  true,
			AgentID:    id,
			TriggerSeq: seq,
			Message:    "timed_out",
			TimedOut:   true,
		}, nil
	case <-ctx.Done():
		return &engine.AgentTriggerResult{
			Triggered:  true,
			AgentID:    id,
			TriggerSeq: seq,
			Message:    "canceled",
			TimedOut:   true,
		}, nil
	}
}

// --- HTTP handlers --------------------------------------------------------

// handleListAgents serves GET /v1/agents.
func (s *serveServer) handleListAgents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	ctrl := NewServeAgentController(s.agent)

	includeStopped := r.URL.Query().Get("include_stopped") == "true"
	resp, err := engine.QueryListAgents(r.Context(), ctrl, engine.ListAgentsRequest{IncludeStopped: includeStopped})
	if err != nil {
		writeAgentError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// handleGetAgent serves GET /v1/agents/{id} and GET /v1/agents/{id}/traces.
// The id is extracted from the path; the traces suffix flips include_trace
// on and defaults trace_limit to 20 to match the existing /v1/agent/traces
// alias shape.
func (s *serveServer) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	ctrl := NewServeAgentController(s.agent)

	id, suffix := extractAgentIDAndSuffix(r.URL.Path)
	q := r.URL.Query()
	req := engine.GetAgentRequest{
		AgentID:      id,
		IncludeTrace: q.Get("include_trace") == "true",
	}
	if v := q.Get("trace_limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeAgentError(w, &engine.AgentControllerError{Code: "invalid_input", Message: fmt.Sprintf("trace_limit must be an integer (got %q)", v)})
			return
		}
		req.TraceLimit = n
	}

	// /traces suffix → alias for the dashboard's `/v1/agent/traces` route.
	// Return the traces array directly (not wrapped in a snapshot envelope)
	// to preserve the legacy shape.
	if suffix == "traces" {
		s.handleAgentTracesByID(w, r, id, req.TraceLimit)
		return
	}

	snap, err := engine.QueryGetAgent(r.Context(), ctrl, req)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(snap)
}

// handleAgentTracesByID serves GET /v1/agents/{id}/traces. Returns the
// cycle-traces.json array directly (same shape as GET /v1/agent/traces)
// so existing consumers can swap the URL with no code change.
func (s *serveServer) handleAgentTracesByID(w http.ResponseWriter, r *http.Request, id string, limit int) {
	if err := engine.ValidateAgentID(id); err != nil {
		writeAgentError(w, err)
		return
	}
	if id != engine.DefaultAgentID {
		writeAgentError(w, engine.ErrAgentNotFound)
		return
	}
	if s.agent == nil {
		_, _ = w.Write([]byte("[]"))
		return
	}
	traceFile := filepath.Join(s.agent.root, ".cog", ".state", "agent", "cycle-traces.json")
	data, err := os.ReadFile(traceFile)
	if err != nil {
		_, _ = w.Write([]byte("[]"))
		return
	}
	if limit <= 0 || limit > 20 {
		// Preserve the full ring when the caller did not specify a clamp;
		// this mirrors the old /v1/agent/traces route (which also returned
		// the full file verbatim).
		_, _ = w.Write(data)
		return
	}
	var traces []cycleTrace
	if err := json.Unmarshal(data, &traces); err != nil {
		_, _ = w.Write(data)
		return
	}
	if len(traces) > limit {
		traces = traces[len(traces)-limit:]
	}
	_ = json.NewEncoder(w).Encode(traces)
}

// handleAgentTick serves POST /v1/agents/{id}/tick.
func (s *serveServer) handleAgentTick(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	ctrl := NewServeAgentController(s.agent)

	id, suffix := extractAgentIDAndSuffix(r.URL.Path)
	if suffix != "tick" {
		// Path like POST /v1/agents/primary (no /tick) — still treat as tick
		// to be lenient on trailing slashes.
		if suffix != "" {
			writeAgentError(w, &engine.AgentControllerError{Code: "invalid_input", Message: fmt.Sprintf("unknown POST subpath %q", suffix)})
			return
		}
	}

	q := r.URL.Query()
	req := engine.TriggerAgentRequest{
		AgentID: id,
		Reason:  q.Get("reason"),
		Wait:    q.Get("wait") == "true",
	}

	result, err := engine.QueryTriggerAgent(r.Context(), ctrl, req)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	// Return 409 when the agent was already running — preserves the
	// semantics of the legacy POST /v1/agent/trigger route.
	if !result.Triggered && result.Message == "already_running" {
		w.WriteHeader(http.StatusConflict)
	}
	_ = json.NewEncoder(w).Encode(result)
}

// extractAgentIDAndSuffix parses /v1/agents/{id}[/suffix] paths. Both
// the id and the optional suffix are returned; missing values are empty.
func extractAgentIDAndSuffix(path string) (id, suffix string) {
	// Expect: /v1/agents, /v1/agents/, /v1/agents/{id}, /v1/agents/{id}/{suffix}
	trimmed := strings.TrimPrefix(path, "/v1/agents")
	trimmed = strings.TrimPrefix(trimmed, "/")
	if trimmed == "" {
		return "", ""
	}
	parts := strings.SplitN(trimmed, "/", 2)
	id = parts[0]
	if len(parts) > 1 {
		suffix = strings.TrimSuffix(parts[1], "/")
	}
	return id, suffix
}

// writeAgentError maps engine errors onto HTTP status + a JSON error body.
func writeAgentError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	code := http.StatusInternalServerError
	msg := err.Error()
	if ace, ok := err.(*engine.AgentControllerError); ok && ace != nil {
		msg = ace.Message
		switch ace.Code {
		case "not_found":
			code = http.StatusNotFound
		case "unavailable":
			code = http.StatusServiceUnavailable
		case "invalid_input":
			code = http.StatusBadRequest
		}
	}
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
