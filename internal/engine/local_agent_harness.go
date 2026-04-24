package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var defaultLocalHarnessToolScope = []string{
	"cog_resolve_uri",
	"cog_search_memory",
	"cog_read_cogdoc",
	"cog_query_field",
	"cog_check_coherence",
	"cog_get_state",
	"cog_get_trust",
	"cog_get_nucleus",
	"cog_get_index",
	"cog_assemble_context",
	"cog_emit_event",
}

const (
	localHarnessHistoryLimit   = 24
	localHarnessCycleTimeout   = 90 * time.Second
	localHarnessAssessMaxToks  = 256
	localHarnessExecuteMaxToks = 1024
)

const localHarnessAssessPrompt = `You are the resident local CogOS maintenance agent.
Operate only through local inference and the kernel's local tools.
Decide whether a maintenance pass is warranted right now.
Return only one compact JSON object with these keys:
{"action":"sleep|observe|consolidate|repair|propose|escalate","reason":"short string","urgency":0..1,"target":"short string","task":"short concrete next step"}
Prefer "sleep" unless the observation names a concrete task worth doing now.`

const localHarnessExecutePrompt = `You are the resident local CogOS maintenance agent.
Stay local-only. Use the provided kernel tools when they materially improve the answer.
Prefer inspection and diagnosis over mutation. Finish with a concise plain-text result.`

const localHarnessDispatchPrompt = `You are the resident local CogOS harness.
Stay local-only. Use only the provided kernel tools. Be concise and finish with a direct answer.`

type localHarnessAssessment struct {
	Action  string  `json:"action"`
	Reason  string  `json:"reason"`
	Urgency float64 `json:"urgency"`
	Target  string  `json:"target"`
	Task    string  `json:"task"`
}

type localHarnessCycleRecord struct {
	Cycle       int64
	Timestamp   time.Time
	Duration    time.Duration
	Action      string
	Urgency     float64
	Reason      string
	Target      string
	Observation string
	Result      string
	Model       string
}

type localHarnessCycleOutcome struct {
	record   localHarnessCycleRecord
	timedOut bool
}

type LocalHarnessController struct {
	cfg             *Config
	nucleus         *Nucleus
	process         *Process
	toolRegistry    *KernelToolRegistry
	dispatchTools   *KernelToolRegistry
	backgroundTools *KernelToolRegistry

	agentID  string
	started  time.Time
	interval time.Duration

	runCtx context.Context

	running atomic.Bool
	stopped atomic.Bool

	cycleSeq   atomic.Int64
	triggerSeq atomic.Int64

	startOnce sync.Once

	mu              sync.RWMutex
	lastObservation string
	lastModel       string
	lastCycle       *localHarnessCycleRecord
	history         []localHarnessCycleRecord
}

func NewLocalHarnessController(cfg *Config, nucleus *Nucleus, process *Process, mcpSrv *MCPServer) (*LocalHarnessController, error) {
	if mcpSrv == nil {
		return nil, fmt.Errorf("local harness requires MCP server wiring")
	}
	registry := NewKernelToolRegistry(mcpSrv)
	dispatchTools, err := registry.Scoped(defaultLocalHarnessToolScope)
	if err != nil {
		return nil, err
	}

	interval := time.Minute
	if cfg != nil && cfg.HeartbeatInterval > 0 {
		interval = time.Duration(cfg.HeartbeatInterval) * time.Second
	}

	return &LocalHarnessController{
		cfg:             cfg,
		nucleus:         nucleus,
		process:         process,
		toolRegistry:    registry,
		dispatchTools:   dispatchTools,
		backgroundTools: dispatchTools,
		agentID:         DefaultAgentID,
		started:         time.Now().UTC(),
		interval:        interval,
	}, nil
}

func (c *LocalHarnessController) Start(ctx context.Context) {
	c.startOnce.Do(func() {
		c.runCtx = ctx
		c.stopped.Store(false)
		c.tryStartCycle("startup", 0, nil)
		go c.runTicker(ctx)
	})
}

func (c *LocalHarnessController) runTicker(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.stopped.Store(true)
			return
		case <-ticker.C:
			c.tryStartCycle("ticker", 0, nil)
		}
	}
}

func (c *LocalHarnessController) tryStartCycle(reason string, triggerSeq int64, waiter chan<- localHarnessCycleOutcome) bool {
	if c.stopped.Load() {
		return false
	}
	if !c.running.CompareAndSwap(false, true) {
		return false
	}
	parent := c.runCtx
	if parent == nil {
		parent = context.Background()
	}
	go c.runCycle(parent, reason, triggerSeq, waiter)
	return true
}

func (c *LocalHarnessController) runCycle(parent context.Context, reason string, triggerSeq int64, waiter chan<- localHarnessCycleOutcome) {
	defer c.running.Store(false)

	ctx, cancel := context.WithTimeout(parent, localHarnessCycleTimeout)
	defer cancel()

	start := time.Now().UTC()
	record := localHarnessCycleRecord{
		Cycle:     c.cycleSeq.Add(1),
		Timestamp: start,
		Action:    "sleep",
		Reason:    "idle",
	}
	record.Observation = c.buildObservation(reason)

	outcome := localHarnessCycleOutcome{record: record}

	target, err := detectLocalLLMTarget(ctx, "")
	if err != nil {
		outcome.record.Action = "error"
		outcome.record.Reason = err.Error()
		c.finishCycle(outcome.record)
		if waiter != nil {
			waiter <- outcome
		}
		return
	}

	model, _, note := resolveDispatchLocalModel(target.Models, c.localModelHint(), DispatchModelE4B)
	if model == "" {
		outcome.record.Action = "error"
		outcome.record.Reason = note
		outcome.record.Model = c.localModelHint()
		c.finishCycle(outcome.record)
		if waiter != nil {
			waiter <- outcome
		}
		return
	}
	outcome.record.Model = model

	provider := buildLocalProvider(target, model)
	assessment, err := c.assessCycle(ctx, provider, outcome.record.Observation)
	if err != nil {
		outcome.record.Action = "error"
		outcome.record.Reason = err.Error()
		c.finishCycle(outcome.record)
		if waiter != nil {
			waiter <- outcome
		}
		return
	}

	outcome.record.Action = assessment.Action
	outcome.record.Reason = assessment.Reason
	outcome.record.Urgency = clampUrgency(assessment.Urgency)
	outcome.record.Target = assessment.Target
	if note != "" {
		if outcome.record.Reason == "" {
			outcome.record.Reason = note
		} else {
			outcome.record.Reason = outcome.record.Reason + "; " + note
		}
	}

	if assessment.Action != "sleep" {
		result, err := c.executeCycleTask(ctx, provider, assessment, outcome.record.Observation, c.backgroundTools)
		if err != nil {
			outcome.record.Action = "error"
			outcome.record.Reason = err.Error()
		} else {
			outcome.record.Result = result
		}
	}

	if ctx.Err() == context.DeadlineExceeded {
		outcome.timedOut = true
		if outcome.record.Action == "" || outcome.record.Action == "sleep" {
			outcome.record.Action = "error"
		}
		if outcome.record.Reason == "" {
			outcome.record.Reason = "cycle timeout"
		}
	}

	c.finishCycle(outcome.record)
	if waiter != nil {
		waiter <- outcome
	}
}

func (c *LocalHarnessController) finishCycle(record localHarnessCycleRecord) {
	record.Duration = time.Since(record.Timestamp)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.lastObservation = record.Observation
	c.lastModel = record.Model
	c.lastCycle = &record
	c.history = append(c.history, record)
	if len(c.history) > localHarnessHistoryLimit {
		c.history = append([]localHarnessCycleRecord(nil), c.history[len(c.history)-localHarnessHistoryLimit:]...)
	}
}

func (c *LocalHarnessController) assessCycle(ctx context.Context, provider Provider, observation string) (*localHarnessAssessment, error) {
	temp := 0.0
	resp, err := provider.Complete(ctx, &CompletionRequest{
		SystemPrompt: localHarnessAssessPrompt,
		Messages: []ProviderMessage{
			{Role: "user", Content: observation},
		},
		MaxTokens:   localHarnessAssessMaxToks,
		Temperature: &temp,
		Metadata: RequestMetadata{
			RequestID:   fmt.Sprintf("local-harness-assess-%d", time.Now().UnixNano()),
			PreferLocal: true,
			Source:      "local-harness",
		},
	})
	if err != nil {
		return nil, err
	}

	var assessment localHarnessAssessment
	if err := decodeJSONPayload(resp.Content, &assessment); err != nil {
		return nil, fmt.Errorf("parse assessment: %w", err)
	}
	if strings.TrimSpace(assessment.Action) == "" {
		assessment.Action = "sleep"
	}
	assessment.Action = strings.ToLower(strings.TrimSpace(assessment.Action))
	assessment.Reason = strings.TrimSpace(assessment.Reason)
	assessment.Target = strings.TrimSpace(assessment.Target)
	assessment.Task = strings.TrimSpace(assessment.Task)
	assessment.Urgency = clampUrgency(assessment.Urgency)
	return &assessment, nil
}

func (c *LocalHarnessController) executeCycleTask(ctx context.Context, provider Provider, assessment *localHarnessAssessment, observation string, registry *KernelToolRegistry) (string, error) {
	temp := 0.1
	task := c.buildExecutionTask(assessment, observation)
	req := &CompletionRequest{
		SystemPrompt: localHarnessExecutePrompt,
		Messages: []ProviderMessage{
			{Role: "user", Content: task},
		},
		Tools:       registry.Definitions(),
		ToolChoice:  "auto",
		MaxTokens:   localHarnessExecuteMaxToks,
		Temperature: &temp,
		Metadata: RequestMetadata{
			RequestID:   fmt.Sprintf("local-harness-exec-%d", time.Now().UnixNano()),
			PreferLocal: true,
			Source:      "local-harness",
		},
	}
	resp, clientCalls, transcript, err := c.completeWithToolLoop(ctx, provider, req, registry)
	if err != nil {
		return "", err
	}
	if len(clientCalls) > 0 {
		slog.Warn("local harness produced unsupported client tool calls", "count", len(clientCalls))
	}
	content := strings.TrimSpace(resp.Content)
	if content == "" && len(transcript) > 0 {
		content = summarizeToolTranscript(transcript)
	}
	return content, nil
}

func (c *LocalHarnessController) buildExecutionTask(assessment *localHarnessAssessment, observation string) string {
	var b strings.Builder
	b.WriteString("Observation:\n")
	b.WriteString(observation)
	b.WriteString("\n\nRequested action: ")
	b.WriteString(assessment.Action)
	if assessment.Target != "" {
		b.WriteString("\nTarget: ")
		b.WriteString(assessment.Target)
	}
	if assessment.Reason != "" {
		b.WriteString("\nWhy: ")
		b.WriteString(assessment.Reason)
	}
	if assessment.Task != "" {
		b.WriteString("\nNext step: ")
		b.WriteString(assessment.Task)
	}
	return b.String()
}

func (c *LocalHarnessController) buildObservation(triggerReason string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "time=%s\n", time.Now().UTC().Format(time.RFC3339))
	if triggerReason != "" {
		fmt.Fprintf(&b, "trigger=%s\n", triggerReason)
	}
	if c.cfg != nil {
		fmt.Fprintf(&b, "workspace=%s\n", filepath.Base(c.cfg.WorkspaceRoot))
	}
	if c.nucleus != nil && c.nucleus.Name != "" {
		fmt.Fprintf(&b, "identity=%s\n", c.nucleus.Name)
	}
	if c.process != nil {
		fmt.Fprintf(&b, "process_state=%s\n", c.process.State().String())
		fovea := c.process.Field().Fovea(5)
		if len(fovea) > 0 {
			b.WriteString("field_top:\n")
			for _, item := range fovea {
				fmt.Fprintf(&b, "- %s score=%.3f\n", item.Path, item.Score)
			}
		}
	}

	c.mu.RLock()
	last := c.lastCycle
	c.mu.RUnlock()
	if last != nil {
		fmt.Fprintf(&b, "last_cycle=%s action=%s urgency=%.2f reason=%s\n",
			last.Timestamp.Format(time.RFC3339), last.Action, last.Urgency, last.Reason)
	}
	return b.String()
}

func (c *LocalHarnessController) localModelHint() string {
	if c.cfg != nil && strings.TrimSpace(c.cfg.LocalModel) != "" {
		return strings.TrimSpace(c.cfg.LocalModel)
	}
	return defaultOllamaModel
}

func (c *LocalHarnessController) summary() AgentSummary {
	c.mu.RLock()
	defer c.mu.RUnlock()

	s := AgentSummary{
		AgentID:   c.agentID,
		Alive:     !c.stopped.Load(),
		Running:   c.running.Load(),
		UptimeSec: int64(time.Since(c.started).Seconds()),
		Model:     c.lastModel,
		Interval:  c.interval.String(),
	}
	if c.nucleus != nil {
		s.Identity = c.nucleus.Name
	}
	if c.lastCycle != nil {
		s.CycleCount = c.lastCycle.Cycle
		s.LastAction = c.lastCycle.Action
		s.LastCycle = c.lastCycle.Timestamp.Format(time.RFC3339)
		s.LastUrgency = c.lastCycle.Urgency
		s.LastReason = c.lastCycle.Reason
		s.LastDurMs = c.lastCycle.Duration.Milliseconds()
	}
	if s.Model == "" {
		s.Model = c.localModelHint()
	}
	return s
}

func (c *LocalHarnessController) ListAgents(_ context.Context, _ bool) ([]AgentSummary, error) {
	if c.stopped.Load() {
		return nil, ErrAgentUnavailable
	}
	return []AgentSummary{c.summary()}, nil
}

func (c *LocalHarnessController) GetAgent(_ context.Context, id string, includeTrace bool, traceLimit int) (*AgentSnapshot, error) {
	if id != c.agentID {
		return nil, ErrAgentNotFound
	}
	if c.stopped.Load() {
		return nil, ErrAgentUnavailable
	}

	snap := &AgentSnapshot{
		Summary: c.summary(),
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	snap.LastObservation = c.lastObservation
	if c.nucleus != nil {
		snap.IdentityRef = c.nucleus.Name
	}
	snap.Memory = make([]AgentMemoryEntry, 0, len(c.history))
	for i := len(c.history) - 1; i >= 0; i-- {
		rec := c.history[i]
		snap.Memory = append(snap.Memory, AgentMemoryEntry{
			Cycle:    rec.Cycle,
			Action:   rec.Action,
			Urgency:  rec.Urgency,
			Sentence: summarizeMemoryEntry(rec),
			Ago:      sinceString(rec.Timestamp),
		})
	}
	if includeTrace {
		start := 0
		if traceLimit > 0 && len(c.history) > traceLimit {
			start = len(c.history) - traceLimit
		}
		for _, rec := range c.history[start:] {
			snap.Traces = append(snap.Traces, AgentCycleTrace{
				Cycle:       rec.Cycle,
				Timestamp:   rec.Timestamp.Format(time.RFC3339),
				DurationMs:  rec.Duration.Milliseconds(),
				Action:      rec.Action,
				Urgency:     rec.Urgency,
				Reason:      rec.Reason,
				Target:      rec.Target,
				Observation: rec.Observation,
				Result:      rec.Result,
			})
		}
	}
	return snap, nil
}

func (c *LocalHarnessController) TriggerAgent(ctx context.Context, id string, reason string, wait bool) (*AgentTriggerResult, error) {
	if id != c.agentID {
		return nil, ErrAgentNotFound
	}
	if c.stopped.Load() {
		return nil, ErrAgentUnavailable
	}

	seq := c.triggerSeq.Add(1)
	if !wait {
		if !c.tryStartCycle(reason, seq, nil) {
			return &AgentTriggerResult{
				Triggered:  false,
				AgentID:    id,
				TriggerSeq: seq,
				Message:    "already_running",
			}, nil
		}
		return &AgentTriggerResult{
			Triggered:  true,
			AgentID:    id,
			TriggerSeq: seq,
			Message:    "triggered",
		}, nil
	}

	waiter := make(chan localHarnessCycleOutcome, 1)
	if !c.tryStartCycle(reason, seq, waiter) {
		return &AgentTriggerResult{
			Triggered:  false,
			AgentID:    id,
			TriggerSeq: seq,
			Message:    "already_running",
		}, nil
	}

	select {
	case outcome := <-waiter:
		return &AgentTriggerResult{
			Triggered:  true,
			AgentID:    id,
			CycleID:    fmt.Sprintf("%s-%d", c.agentID, outcome.record.Cycle),
			TriggerSeq: seq,
			Message:    "completed",
			Action:     outcome.record.Action,
			Urgency:    outcome.record.Urgency,
			Reason:     outcome.record.Reason,
			DurationMs: outcome.record.Duration.Milliseconds(),
			TimedOut:   outcome.timedOut,
		}, nil
	case <-ctx.Done():
		return &AgentTriggerResult{
			Triggered:  true,
			AgentID:    id,
			TriggerSeq: seq,
			Message:    "triggered",
			TimedOut:   true,
		}, nil
	}
}

func (c *LocalHarnessController) DispatchToHarness(ctx context.Context, req DispatchRequest) (*DispatchBatchResult, error) {
	if c.stopped.Load() {
		return nil, ErrAgentUnavailable
	}
	if req.AgentID != "" && req.AgentID != c.agentID {
		return nil, ErrAgentNotFound
	}
	if err := req.Normalize(); err != nil {
		return nil, err
	}

	target, err := detectLocalLLMTarget(ctx, "")
	if err != nil {
		return nil, err
	}
	model, routeUsed, note := resolveDispatchLocalModel(target.Models, c.localModelHint(), req.Model)
	if model == "" {
		return nil, errors.New(note)
	}

	registry := c.dispatchTools
	if len(req.Tools) > 0 {
		registry, err = c.dispatchTools.Scoped(req.Tools)
		if err != nil {
			return nil, err
		}
	}

	provider := buildLocalProvider(target, model)
	batch := &DispatchBatchResult{
		Results: make([]DispatchResult, req.N),
	}
	if note != "" {
		batch.Notes = append(batch.Notes, note)
	}

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < req.N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			batch.Results[idx] = c.dispatchSlot(ctx, provider, registry, model, routeUsed, req, idx, note)
		}(i)
	}
	wg.Wait()
	batch.TotalDurationSec = time.Since(start).Seconds()
	return batch, nil
}

func (c *LocalHarnessController) dispatchSlot(parent context.Context, provider Provider, registry *KernelToolRegistry, model string, routeUsed DispatchModel, req DispatchRequest, idx int, note string) DispatchResult {
	res := DispatchResult{
		Index:     idx,
		ModelUsed: routeUsed,
	}
	slotCtx, cancel := context.WithTimeout(parent, time.Duration(req.TimeoutSeconds)*time.Second)
	defer cancel()

	systemPrompt := strings.TrimSpace(req.SystemPrompt)
	if systemPrompt == "" {
		systemPrompt = localHarnessDispatchPrompt
	}
	counting := &countingProvider{Provider: provider}

	temp := 0.1
	compReq := &CompletionRequest{
		SystemPrompt: systemPrompt,
		Messages: []ProviderMessage{
			{Role: "user", Content: strings.TrimSpace(req.Task)},
		},
		Tools:         registry.Definitions(),
		ToolChoice:    "auto",
		MaxTokens:     localHarnessExecuteMaxToks,
		Temperature:   &temp,
		ModelOverride: model,
		Metadata: RequestMetadata{
			RequestID:   fmt.Sprintf("local-harness-dispatch-%d-%d", time.Now().UnixNano(), idx),
			PreferLocal: true,
			Source:      "local-harness-dispatch",
		},
	}

	start := time.Now()
	resp, clientCalls, transcript, err := c.completeWithToolLoop(slotCtx, counting, compReq, registry)
	res.DurationSec = time.Since(start).Seconds()
	res.Turns = counting.CompleteCalls()
	for _, tc := range transcript {
		entry := DispatchToolCallSummary{
			Name:         tc.Name,
			ArgsDigest:   truncateDigest(tc.Arguments),
			ResultDigest: truncateDigest(tc.Result),
		}
		if tc.Rejected {
			entry.Error = tc.RejectReason
		}
		res.ToolCalls = append(res.ToolCalls, entry)
	}
	if len(clientCalls) > 0 {
		res.Error = fmt.Sprintf("unsupported client tool calls returned: %d", len(clientCalls))
	}
	if err != nil {
		if slotCtx.Err() == context.DeadlineExceeded {
			res.Error = "timeout"
		} else {
			res.Error = err.Error()
		}
		return res
	}
	res.Success = true
	res.Content = strings.TrimSpace(resp.Content)
	if res.Content == "" && len(transcript) > 0 {
		res.Content = summarizeToolTranscript(transcript)
	}
	if note != "" && res.Error == "" {
		res.Error = note
	}
	return res
}

func (c *LocalHarnessController) completeWithToolLoop(ctx context.Context, provider Provider, req *CompletionRequest, registry *KernelToolRegistry) (*CompletionResponse, []ToolCall, []ToolCallRecord, error) {
	resp, err := provider.Complete(ctx, req)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(resp.ToolCalls) == 0 {
		return resp, nil, nil, nil
	}
	return RunToolLoopWithTranscript(ctx, provider, req, resp, registry)
}

type countingProvider struct {
	Provider
	completeCalls atomic.Int64
}

func (p *countingProvider) Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	p.completeCalls.Add(1)
	return p.Provider.Complete(ctx, req)
}

func (p *countingProvider) CompleteCalls() int {
	return int(p.completeCalls.Load())
}

func clampUrgency(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func decodeJSONPayload(raw string, out any) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("empty response")
	}
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start >= 0 && end >= start {
		raw = raw[start : end+1]
	}
	return json.Unmarshal([]byte(raw), out)
}

func summarizeMemoryEntry(rec localHarnessCycleRecord) string {
	switch {
	case rec.Result != "":
		return truncateDigest(rec.Result)
	case rec.Reason != "":
		return truncateDigest(rec.Reason)
	default:
		return rec.Action
	}
}

func summarizeToolTranscript(records []ToolCallRecord) string {
	if len(records) == 0 {
		return ""
	}
	last := records[len(records)-1]
	if last.Result != "" {
		return truncateDigest(last.Result)
	}
	return last.Name
}

func truncateDigest(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 200 {
		return s
	}
	return s[:200] + "..."
}

func sinceString(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return time.Since(ts).Round(time.Second).String()
}
