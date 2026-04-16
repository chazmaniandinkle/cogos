// agent_serve.go — Homeostatic agent loop for cog serve.
//
// Runs the native Go agent harness on a 30-minute ticker inside the kernel
// process. Each cycle: gathers workspace observations, calls E4B for
// assessment, and executes actions through kernel-native tools.
//
// Integration: Created in cmdServeForeground() alongside the reconciler.
//   agent := NewServeAgent(root)
//   agent.SetBus(busManager)
//   agent.Start()
//   defer agent.Stop()
//
// The reconciler and agent loop are complementary:
//   - Reconciler: declarative state convergence (every 5 min)
//   - Agent: observation-driven assessment and action (every 30 min)

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	agentIntervalMin     = 5 * time.Minute  // start fast
	agentIntervalMax     = 30 * time.Minute // relax to this after consecutive sleeps
	agentBusID           = "bus_agent_harness"
)

// AgentStatusResponse is the JSON payload for GET /v1/agent/status.
type AgentStatusResponse struct {
	Alive       bool    `json:"alive"`
	Uptime      string  `json:"uptime"`
	UptimeSec   int64   `json:"uptime_sec"`
	CycleCount  int64   `json:"cycle_count"`
	LastCycle   string  `json:"last_cycle,omitempty"` // RFC3339
	LastAction  string  `json:"last_action,omitempty"`
	LastUrgency float64 `json:"last_urgency"`
	LastReason  string  `json:"last_reason,omitempty"`
	LastDurMs   int64   `json:"last_duration_ms"`
	Interval    string  `json:"interval"`
	Model       string  `json:"model"`

	// Activity awareness (enriched)
	Activity *AgentActivitySummary  `json:"activity,omitempty"`
	Memory   []AgentMemoryEntry     `json:"memory,omitempty"`
	Proposals []AgentProposalEntry  `json:"proposals,omitempty"`
}

// AgentActivitySummary is the system activity snapshot from the bus registry.
type AgentActivitySummary struct {
	UserPresence     string `json:"user_presence"`      // "active", "recent", "idle", "unknown"
	UserLastEventAgo string `json:"user_last_event_ago"` // "3m", "2h"
	ClaudeCodeActive int    `json:"claude_code_active"`
	ClaudeCodeEvents int64  `json:"claude_code_events"`
	TotalEventDelta  int64  `json:"total_event_delta"`
	HottestBus       string `json:"hottest_bus,omitempty"`
	HottestDelta     int64  `json:"hottest_delta"`
}

// AgentMemoryEntry is a rolling memory item for the API.
type AgentMemoryEntry struct {
	Cycle    int64   `json:"cycle"`
	Action   string  `json:"action"`
	Urgency  float64 `json:"urgency"`
	Sentence string  `json:"sentence"`
	Ago      string  `json:"ago"` // "5m", "2h"
}

// AgentProposalEntry is a pending proposal for the API.
type AgentProposalEntry struct {
	File    string `json:"file"`
	Title   string `json:"title"`
	Type    string `json:"type"`
	Urgency string `json:"urgency"`
	Created string `json:"created"`
}

// ServeAgent runs the homeostatic agent loop inside cog serve.
type ServeAgent struct {
	root     string
	interval time.Duration
	harness  *AgentHarness
	bus      *busSessionManager
	stopCh   chan struct{}
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	// Metrics (read via Status())
	mu          sync.RWMutex
	startedAt   time.Time
	lastRun     time.Time
	cycleCount  int64
	lastAction  string
	lastUrgency float64
	lastReason  string
	lastDurMs   int64

	// Decomposition-fed rolling memory (the first hypercycle wire)
	cycleMemory     *agentCycleMemory
	lastObservation string // cached for decomposition context

	// Activity tracking for cheap checks gate and delta computation
	lastRegistrySnapshot map[string]int64 // busID → LastEventSeq at last cycle
	lastGitFileCount     int              // modified file count at last cycle
	lastCoherenceOK      bool             // coherence status at last cycle
	lastProposalCount    int              // pending proposal count at last cycle
}

// Status returns the current agent loop status for the API.
func (sa *ServeAgent) Status() AgentStatusResponse {
	sa.mu.RLock()
	defer sa.mu.RUnlock()

	uptime := time.Since(sa.startedAt)
	resp := AgentStatusResponse{
		Alive:       true,
		Uptime:      agentFormatDuration(uptime),
		UptimeSec:   int64(uptime.Seconds()),
		CycleCount:  sa.cycleCount,
		LastAction:  sa.lastAction,
		LastUrgency: sa.lastUrgency,
		LastReason:  sa.lastReason,
		LastDurMs:   sa.lastDurMs,
		Interval:    sa.interval.String(),
		Model:       sa.harness.model,
	}
	if !sa.lastRun.IsZero() {
		resp.LastCycle = sa.lastRun.Format(time.RFC3339)
	}

	// Enrich with activity, memory, and proposals (best-effort, outside lock)
	sa.mu.RUnlock()
	resp.Activity = sa.getActivityForAPI()
	resp.Memory = sa.getMemoryForAPI()
	resp.Proposals = sa.getProposalsForAPI()
	sa.mu.RLock() // re-acquire for deferred unlock

	return resp
}

// getActivityForAPI computes the activity summary for the status API.
func (sa *ServeAgent) getActivityForAPI() *AgentActivitySummary {
	if sa.bus == nil {
		return nil
	}
	registry := sa.bus.loadRegistry()
	if len(registry) == 0 {
		return nil
	}

	now := time.Now()
	summary := &AgentActivitySummary{UserPresence: "unknown"}

	apiBusExclude := map[string]bool{
		"bus_chat_system_capabilities": true,
		"bus_chat_http":                true,
		"bus_agent_harness":            true,
		"bus_index":                    true,
	}
	var userEventTime time.Time

	for _, entry := range registry {
		seq := int64(entry.LastEventSeq)
		prevSeq := sa.lastRegistrySnapshot[entry.BusID]
		delta := seq - prevSeq
		if delta < 0 {
			delta = 0
		}

		if delta > 0 {
			summary.TotalEventDelta += delta
			if delta > summary.HottestDelta {
				summary.HottestDelta = delta
				summary.HottestBus = entry.BusID
				if len(summary.HottestBus) > 40 {
					summary.HottestBus = summary.HottestBus[:37] + "..."
				}
			}
		}

		bid := entry.BusID
		isSystem := apiBusExclude[bid]

		if !isSystem && entry.LastEventAt != "" {
			if t, err := time.Parse(time.RFC3339Nano, entry.LastEventAt); err == nil {
				if t.After(userEventTime) {
					userEventTime = t
				}
			}
		}

		if strings.HasPrefix(bid, "claude-code-") && delta > 0 {
			summary.ClaudeCodeActive++
			summary.ClaudeCodeEvents += delta
		}
	}

	if !userEventTime.IsZero() {
		ago := now.Sub(userEventTime)
		summary.UserLastEventAgo = formatAgo(ago)
		if ago < 5*time.Minute {
			summary.UserPresence = "active"
		} else if ago < 30*time.Minute {
			summary.UserPresence = "recent"
		} else {
			summary.UserPresence = "idle"
		}
	}

	return summary
}

// getMemoryForAPI returns the rolling memory entries for the status API.
func (sa *ServeAgent) getMemoryForAPI() []AgentMemoryEntry {
	if sa.cycleMemory == nil {
		return nil
	}
	entries := sa.cycleMemory.recent(maxRollingMemory)
	if len(entries) == 0 {
		return nil
	}
	now := time.Now()
	result := make([]AgentMemoryEntry, len(entries))
	for i, e := range entries {
		result[i] = AgentMemoryEntry{
			Cycle:    e.Cycle,
			Action:   e.Action,
			Urgency:  e.Urgency,
			Sentence: e.Sentence,
			Ago:      formatAgo(now.Sub(e.Timestamp)),
		}
	}
	return result
}

// getProposalsForAPI returns pending proposals for the status API.
func (sa *ServeAgent) getProposalsForAPI() []AgentProposalEntry {
	dir := filepath.Join(sa.root, proposalsDir)
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var result []AgentProposalEntry
	for _, e := range dirEntries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		contentStr := string(content)
		if !strings.Contains(contentStr, "status: pending") {
			continue
		}
		result = append(result, AgentProposalEntry{
			File:    e.Name(),
			Title:   extractFMField(contentStr, "title"),
			Type:    extractFMField(contentStr, "type"),
			Urgency: extractFMField(contentStr, "urgency"),
			Created: extractFMField(contentStr, "created"),
		})
	}
	return result
}

// agentFormatDuration returns a human-readable duration like "4h 23m".
func agentFormatDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

// NewServeAgent creates an agent loop for the given workspace.
// Starts at agentIntervalMin (5m) and relaxes toward agentIntervalMax (30m)
// when the model reports consecutive "sleep" assessments.
func NewServeAgent(root string) *ServeAgent {
	interval := agentIntervalMin

	// Allow override via env var (disables adaptive interval)
	if v := os.Getenv("COG_AGENT_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			interval = d
		}
	}

	// Build harness pointing at Ollama
	ollamaURL := os.Getenv("OLLAMA_HOST")
	if ollamaURL == "" {
		ollamaURL = "http://localhost:11434"
	}
	ollamaURL = strings.TrimRight(ollamaURL, "/")

	model := os.Getenv("COG_AGENT_MODEL")
	if model == "" {
		model = "gemma4:e4b"
	}

	harness := NewAgentHarness(AgentHarnessConfig{
		OllamaURL: ollamaURL,
		Model:     model,
	})
	RegisterCoreTools(harness, root)

	sa := &ServeAgent{
		root:                 root,
		interval:             interval,
		harness:              harness,
		stopCh:               make(chan struct{}),
		cycleMemory:          newAgentCycleMemory(maxRollingMemory),
		lastRegistrySnapshot: make(map[string]int64),
	}
	sa.loadCycleMemory()
	return sa
}

// SetBus attaches a bus session manager for emitting agent events.
func (sa *ServeAgent) SetBus(mgr *busSessionManager) {
	sa.bus = mgr
}

// Start launches the agent loop in a goroutine.
func (sa *ServeAgent) Start() error {
	log.Printf("[agent] starting homeostatic loop (interval=%s, model=%s)", sa.interval, sa.harness.model)

	sa.mu.Lock()
	sa.startedAt = time.Now()
	sa.mu.Unlock()

	// Ensure the agent bus exists and is registered so events appear
	// in /v1/bus/list and cross-bus queries.
	if sa.bus != nil {
		sa.ensureBus()
	}

	ctx, cancel := context.WithCancel(context.Background())
	sa.cancel = cancel

	sa.wg.Add(1)
	go sa.runLoop(ctx)

	return nil
}

// ensureBus creates the bus directory, events file, and registry entry
// for the agent harness bus if they don't already exist.
func (sa *ServeAgent) ensureBus() {
	busDir := filepath.Join(sa.bus.busesDir(), agentBusID)
	if err := os.MkdirAll(busDir, 0755); err != nil {
		log.Printf("[agent] failed to create bus dir: %v", err)
		return
	}
	eventsFile := filepath.Join(busDir, "events.jsonl")
	if _, err := os.Stat(eventsFile); os.IsNotExist(err) {
		f, err := os.Create(eventsFile)
		if err != nil {
			log.Printf("[agent] failed to create events file: %v", err)
			return
		}
		f.Close()
	}
	if err := sa.bus.registerBus(agentBusID, "kernel:agent", "kernel:agent"); err != nil {
		log.Printf("[agent] failed to register bus: %v", err)
	}
}

// Stop signals the loop to stop and waits for completion.
func (sa *ServeAgent) Stop() {
	sa.cancel()
	close(sa.stopCh)
	sa.wg.Wait()
	sa.mu.RLock()
	count := sa.cycleCount
	sa.mu.RUnlock()
	log.Printf("[agent] stopped after %d cycles", count)
}

// runLoop is the main ticker loop with adaptive interval.
// Starts at agentIntervalMin, doubles toward agentIntervalMax on consecutive
// "sleep" assessments, resets to agentIntervalMin on any non-sleep action.
//
// The loop is resilient: panics in runCycle are recovered and logged,
// and the loop continues after a backoff delay.
func (sa *ServeAgent) runLoop(ctx context.Context) {
	defer sa.wg.Done()

	consecutiveSleeps := 0

	// Run initial cycle after a short delay (let the kernel fully initialize)
	select {
	case <-time.After(60 * time.Second):
		action := sa.safeCycle(ctx)
		consecutiveSleeps = sa.updateSleepCount(action, consecutiveSleeps)
	case <-sa.stopCh:
		return
	}

	for {
		// Adaptive interval: double on each consecutive sleep, cap at max
		interval := sa.interval
		for i := 0; i < consecutiveSleeps && interval < agentIntervalMax; i++ {
			interval *= 2
		}
		if interval > agentIntervalMax {
			interval = agentIntervalMax
		}

		log.Printf("[agent] next cycle in %s (consecutive sleeps: %d)", interval, consecutiveSleeps)

		select {
		case <-time.After(interval):
			action := sa.safeCycle(ctx)
			consecutiveSleeps = sa.updateSleepCount(action, consecutiveSleeps)
		case <-sa.stopCh:
			return
		}
	}
}

// safeCycle wraps runCycle with panic recovery so the loop survives crashes.
func (sa *ServeAgent) safeCycle(ctx context.Context) (action string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] PANIC recovered in cycle: %v", r)
			action = "error"
		}
	}()
	return sa.runCycle(ctx)
}

// updateSleepCount returns the new consecutive sleep counter.
func (sa *ServeAgent) updateSleepCount(action string, current int) int {
	if action == "sleep" {
		return current + 1
	}
	return 0
}

// runCycle executes a single observe-assess-execute pass.
// Returns the assessment action string for adaptive interval logic.
func (sa *ServeAgent) runCycle(ctx context.Context) string {
	start := time.Now()
	sa.mu.Lock()
	sa.cycleCount++
	cycle := sa.cycleCount
	sa.mu.Unlock()

	log.Printf("[agent] cycle %d: starting", cycle)

	// Cheap checks gate: skip model call if nothing changed
	if skip, reason := sa.shouldSkipModel(); skip {
		log.Printf("[agent] cycle %d: skipped (%s)", cycle, reason)

		sa.mu.Lock()
		sa.lastRun = time.Now()
		sa.lastAction = "skip"
		sa.lastUrgency = 0
		sa.lastReason = reason
		sa.lastDurMs = 0
		sa.mu.Unlock()

		sa.emitEvent("agent.skip", map[string]interface{}{
			"cycle":  cycle,
			"reason": reason,
		})

		// Feed skip into rolling memory so the agent knows it was skipped
		go sa.decomposeAndStore(ctx, cycle, &Assessment{
			Action:  "sleep",
			Reason:  "Cycle skipped: " + reason,
			Urgency: 0,
		})

		return "sleep"
	}

	// Build observation from workspace state
	observation := sa.gatherObservation()

	// System prompt: concise, no thinking tags (Gemma E4B doesn't need them).
	// JSON mode is enforced by the harness via response_format.
	systemPrompt := fmt.Sprintf(`You are the CogOS kernel agent on a local node. Workspace: %s

Respond ONLY with a JSON object. No markdown, no explanation, no thinking.

{"action": "<sleep|observe|propose|escalate>", "reason": "<brief reason>", "urgency": <0.0-1.0>, "target": "<URI or path or empty>"}

Actions:
- sleep: nothing needs attention, rest until next cycle
- observe: gather more info using tools (memory_search, memory_read, workspace_status, coherence_check)
- propose: write a proposal using the propose tool — this is your primary way of acting. Proposals are safe, lightweight, and never disrupt the user. Use them freely.
- escalate: something needs human or cloud-model attention

You are an observer and advisor. Your proposals are staged in a directory — they do not modify anything. The user reviews them at their convenience. You cannot interrupt anyone. Act confidently.

Rules:
- You may ONLY return one of these actions: sleep, observe, propose, escalate. No other values.
- Look at your Recent Cycle Memory. If the last 3+ entries show the same action, you MUST pick a DIFFERENT one. For example: if you see observe,observe,observe then pick propose or sleep. If you see propose,propose,propose then pick sleep or observe.
- If you have already read the pending proposals (check your memory — did a recent cycle mention reading them?), do NOT read them again. Instead propose your response or sleep.
- When nothing needs attention, sleep. Sleeping is good — it saves compute and the adaptive interval will wake you when something changes.`, sa.root)

	// Run assessment phase (JSON mode)
	assessment, err := sa.harness.Assess(ctx, systemPrompt, observation)
	var executeResult string
	if err == nil && assessment.Action != "sleep" {
		// Execute phase gets a different prompt that encourages tool chaining
		executePrompt := fmt.Sprintf(`You are the CogOS kernel agent executing an action. Workspace: %s

You decided: %s (reason: %s, target: %s)

Now execute this using your tools. You may call MULTIPLE tools in sequence:
- Call read_proposal to read proposals, then call propose to respond
- Call memory_search to find docs, then call memory_read to read them
- Call coherence_check, then propose a fix if needed

Do not just describe what you would do — actually call the tools. When you are finished acting, respond with a brief summary of what you did.`, sa.root, assessment.Action, assessment.Reason, assessment.Target)

		task := fmt.Sprintf("Execute: %s\nTarget: %s\nReason: %s",
			assessment.Action, assessment.Target, assessment.Reason)
		executeResult, _ = sa.harness.Execute(ctx, executePrompt, task)
	}
	duration := time.Since(start)

	if err != nil {
		log.Printf("[agent] cycle %d: error: %v (%s)", cycle, err, duration.Round(time.Millisecond))
		sa.emitEvent("agent.error", map[string]interface{}{
			"cycle": cycle,
			"error": err.Error(),
		})
		return "error"
	}

	// Update status fields for the API
	sa.mu.Lock()
	sa.lastRun = time.Now()
	sa.cycleCount = cycle
	sa.lastAction = assessment.Action
	sa.lastUrgency = assessment.Urgency
	sa.lastReason = assessment.Reason
	sa.lastDurMs = duration.Milliseconds()
	sa.mu.Unlock()

	log.Printf("[agent] cycle %d: action=%s urgency=%.1f reason=%q (%s)",
		cycle, assessment.Action, assessment.Urgency, assessment.Reason, duration.Round(time.Millisecond))

	if executeResult != "" {
		log.Printf("[agent] cycle %d: execute result: %s", cycle, agentTruncate(executeResult, 500))
	}

	sa.emitEvent("agent.cycle", map[string]interface{}{
		"cycle":       cycle,
		"action":      assessment.Action,
		"reason":      assessment.Reason,
		"urgency":     assessment.Urgency,
		"target":      assessment.Target,
		"duration_ms": duration.Milliseconds(),
		"executed":    executeResult != "",
		"result":      agentTruncate(executeResult, 2000),
	})

	// Store full cycle trace to disk for dashboard display
	sa.storeCycleTrace(cycle, assessment, observation, executeResult, duration)

	if assessment.Action == "escalate" {
		log.Printf("[agent] cycle %d: escalation requested — %s (target: %s)",
			cycle, assessment.Reason, assessment.Target)
		sa.emitEvent("agent.escalation", map[string]interface{}{
			"cycle":  cycle,
			"reason": assessment.Reason,
			"target": assessment.Target,
		})
	}

	// Decompose the assessment into Tier 0 and feed into rolling memory.
	// Runs asynchronously so it doesn't delay the next cycle.
	// This is the first self-feeding wire in the CogOS Hypercycle:
	// PRODUCE → DECOMPOSE → ABSORB (via gatherObservation on next cycle).
	go sa.decomposeAndStore(ctx, cycle, assessment)

	return assessment.Action
}

// gatherObservation builds a compact observation string from workspace state.
// Also updates the observation cache for the cheap checks gate.
func (sa *ServeAgent) gatherObservation() string {
	var sb strings.Builder
	var cachedGitCount int
	var cachedCoherenceOK bool
	var cachedProposalCount int

	sb.WriteString("=== Workspace Observation ===\n")
	sb.WriteString(fmt.Sprintf("Time: %s\n", time.Now().Format(time.RFC3339)))
	sb.WriteString(fmt.Sprintf("Workspace: %s\n\n", sa.root))

	// Git status (quick)
	if status, err := runQuietCommand(sa.root, "git", "status", "--porcelain"); err == nil {
		lines := strings.Split(strings.TrimSpace(status), "\n")
		if len(lines) > 0 && lines[0] != "" {
			cachedGitCount = len(lines)
			sb.WriteString(fmt.Sprintf("Git: %d modified files\n", cachedGitCount))
		} else {
			sb.WriteString("Git: clean\n")
		}
	}

	// Recent memory activity
	if recent, err := runQuietCommand(sa.root, "./scripts/cog", "memory", "search", "--recent", "1h"); err == nil && recent != "" {
		lines := strings.Split(strings.TrimSpace(recent), "\n")
		sb.WriteString(fmt.Sprintf("Memory: %d recent docs\n", len(lines)))
	}

	// Coherence check
	if coh, err := runQuietCommand(sa.root, "./scripts/cog", "coherence", "check"); err == nil {
		if strings.Contains(coh, "coherent") {
			sb.WriteString("Coherence: OK\n")
			cachedCoherenceOK = true
		} else {
			sb.WriteString(fmt.Sprintf("Coherence: DRIFT — %s\n", strings.TrimSpace(coh)))
			cachedCoherenceOK = false
		}
	}

	// Kernel uptime
	sa.mu.RLock()
	currentCycle := sa.cycleCount
	sa.mu.RUnlock()
	sb.WriteString(fmt.Sprintf("Agent cycle: %d\n", currentCycle+1))
	if !sa.lastRun.IsZero() {
		sb.WriteString(fmt.Sprintf("Last cycle: %s ago\n", time.Since(sa.lastRun).Round(time.Second)))
	}

	// Inject system activity summary from bus registry
	if activity := sa.gatherActivitySummary(); activity != "" {
		sb.WriteString(activity)
	}

	// Inject rolling compressed memory from previous cycles (the hypercycle wire)
	if sa.cycleMemory != nil {
		if mem := sa.cycleMemory.formatForObservation(); mem != "" {
			sb.WriteString(mem)
		}
	}

	// Inject pending proposals so the agent sees what it's already proposed
	if proposals := sa.gatherPendingProposals(); proposals != "" {
		sb.WriteString(proposals)
		// Count proposals for cache
		cachedProposalCount = strings.Count(proposals, "[")
	}

	// Update observation cache for the cheap checks gate
	sa.updateObservationCache(cachedGitCount, cachedCoherenceOK, cachedProposalCount)

	obs := sb.String()
	sa.lastObservation = obs // cache for decomposition context
	return obs
}

// gatherActivitySummary reads the bus registry and computes an activity summary.
// Cost: ~1-2ms (single file read of registry.json).
func (sa *ServeAgent) gatherActivitySummary() string {
	if sa.bus == nil {
		return ""
	}

	registry := sa.bus.loadRegistry()
	if len(registry) == 0 {
		return ""
	}

	now := time.Now()

	// System buses that fire from the kernel itself — NOT user activity
	systemBuses := map[string]bool{
		"bus_chat_system_capabilities": true,
		"bus_chat_http":                true,
		"bus_agent_harness":            true,
		"bus_index":                    true,
	}

	var (
		claudeCodeActive   int
		claudeCodeEvents   int64
		chatActive         int
		mcpActive          int
		voiceActive        int
		totalDelta         int64
		hottestBus         string
		hottestDelta       int64
		userEventTime      time.Time // only from user-initiated buses
		newSnapshot        = make(map[string]int64, len(registry))
	)

	for _, entry := range registry {
		seq := int64(entry.LastEventSeq)
		newSnapshot[entry.BusID] = seq

		// Compute delta from last cycle
		prevSeq, hasPrev := sa.lastRegistrySnapshot[entry.BusID]
		delta := int64(0)
		if hasPrev {
			delta = seq - prevSeq
		}

		if delta > 0 {
			totalDelta += delta
			if delta > hottestDelta {
				hottestDelta = delta
				hottestBus = entry.BusID
			}
		}

		bid := entry.BusID
		isSystem := systemBuses[bid]

		// Track user presence only from user-initiated buses (not kernel system buses)
		if !isSystem && entry.LastEventAt != "" {
			if t, err := time.Parse(time.RFC3339Nano, entry.LastEventAt); err == nil {
				if t.After(userEventTime) {
					userEventTime = t
				}
			}
		}

		// Classify by prefix
		switch {
		case strings.HasPrefix(bid, "claude-code-"):
			if delta > 0 {
				claudeCodeActive++
				claudeCodeEvents += delta
			}
		case strings.HasPrefix(bid, "bus_chat_") && !isSystem:
			if delta > 0 {
				chatActive++
			}
		case strings.HasPrefix(bid, "bus_mcp_"):
			if delta > 0 {
				mcpActive++
			}
		case strings.Contains(bid, "voice"):
			if delta > 0 {
				voiceActive++
			}
		}
	}

	// Update snapshot for next cycle's delta
	sa.lastRegistrySnapshot = newSnapshot

	// Build summary
	var sb strings.Builder
	sb.WriteString("\n=== System Activity (since last cycle) ===\n")

	// User presence
	if !userEventTime.IsZero() {
		ago := now.Sub(userEventTime).Round(time.Second)
		if ago < 5*time.Minute {
			sb.WriteString(fmt.Sprintf("User presence: active (last event %s ago)\n", ago))
		} else if ago < 30*time.Minute {
			sb.WriteString(fmt.Sprintf("User presence: recent (last event %s ago)\n", formatAgo(ago)))
		} else {
			sb.WriteString(fmt.Sprintf("User presence: idle (last event %s ago)\n", formatAgo(ago)))
		}
	} else {
		sb.WriteString("User presence: unknown\n")
	}

	// Session counts
	if claudeCodeActive > 0 {
		sb.WriteString(fmt.Sprintf("Claude Code sessions: %d active (%d new events)\n", claudeCodeActive, claudeCodeEvents))
	} else {
		sb.WriteString("Claude Code sessions: 0 active\n")
	}

	if chatActive > 0 {
		sb.WriteString(fmt.Sprintf("Chat buses: %d with activity\n", chatActive))
	}
	if mcpActive > 0 {
		sb.WriteString(fmt.Sprintf("MCP sessions: %d active\n", mcpActive))
	}
	if voiceActive > 0 {
		sb.WriteString(fmt.Sprintf("Voice sessions: %d active\n", voiceActive))
	}

	// Overall activity
	sb.WriteString(fmt.Sprintf("Total event delta: %d events since last cycle\n", totalDelta))
	if hottestBus != "" && hottestDelta > 0 {
		// Truncate long bus IDs for readability
		displayBus := hottestBus
		if len(displayBus) > 40 {
			displayBus = displayBus[:37] + "..."
		}
		sb.WriteString(fmt.Sprintf("Hottest bus: %s (%d events)\n", displayBus, hottestDelta))
	}

	return sb.String()
}

// shouldSkipModel returns true if nothing has changed since the last cycle,
// meaning we can skip the expensive E4B call and just sleep.
// This is the "cheap checks first" gate from the OpenClaw community pattern.
func (sa *ServeAgent) shouldSkipModel() (skip bool, reason string) {
	// Never skip the first few cycles — let the model establish a baseline
	sa.mu.RLock()
	cycles := sa.cycleCount
	sa.mu.RUnlock()
	if cycles < 3 {
		return false, ""
	}

	// Check 1: Rolling memory — was last action already "sleep"?
	lastEntries := sa.cycleMemory.recent(1)
	if len(lastEntries) == 0 || lastEntries[0].Action != "sleep" {
		return false, ""
	}

	// Check 2: Git status changed?
	gitCount := 0
	if status, err := runQuietCommand(sa.root, "git", "status", "--porcelain"); err == nil {
		lines := strings.Split(strings.TrimSpace(status), "\n")
		if len(lines) > 0 && lines[0] != "" {
			gitCount = len(lines)
		}
	}
	if gitCount != sa.lastGitFileCount {
		return false, "git status changed"
	}

	// Check 3: Bus events since last cycle?
	if sa.bus != nil {
		registry := sa.bus.loadRegistry()
		for _, entry := range registry {
			prevSeq, hasPrev := sa.lastRegistrySnapshot[entry.BusID]
			if hasPrev && int64(entry.LastEventSeq) > prevSeq {
				// Ignore our own agent harness bus — our own events shouldn't wake us
				if entry.BusID == agentBusID {
					continue
				}
				return false, fmt.Sprintf("bus activity: %s", entry.BusID)
			}
		}
	}

	// Check 4: Coherence drift?
	if !sa.lastCoherenceOK {
		return false, "coherence drift"
	}

	// Check 5: New proposals to review?
	proposalDir := filepath.Join(sa.root, proposalsDir)
	if entries, err := os.ReadDir(proposalDir); err == nil {
		count := 0
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
				count++
			}
		}
		if count != sa.lastProposalCount {
			return false, "proposal count changed"
		}
	}

	// All checks passed — nothing changed
	return true, "no changes since last cycle"
}

// updateObservationCache stores current state for the next cycle's delta checks.
func (sa *ServeAgent) updateObservationCache(gitCount int, coherenceOK bool, proposalCount int) {
	sa.lastGitFileCount = gitCount
	sa.lastCoherenceOK = coherenceOK
	sa.lastProposalCount = proposalCount
}

// emitEvent sends an event to the CogBus (best-effort).
func (sa *ServeAgent) emitEvent(eventType string, payload map[string]interface{}) {
	if sa.bus == nil {
		return
	}
	if _, err := sa.bus.appendBusEvent(agentBusID, eventType, "kernel:agent", payload); err != nil {
		log.Printf("[agent] bus event emit error: %v", err)
	}
}

// gatherPendingProposals returns a summary of pending proposals for observation injection.
func (sa *ServeAgent) gatherPendingProposals() string {
	dir := filepath.Join(sa.root, proposalsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}

	var pending []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		contentStr := string(content)
		if !strings.Contains(contentStr, "status: pending") {
			continue
		}
		title := extractFMField(contentStr, "title")
		pType := extractFMField(contentStr, "type")
		pending = append(pending, fmt.Sprintf("  [%s] %s", pType, title))
	}

	if len(pending) == 0 {
		return ""
	}

	return fmt.Sprintf("\n=== Pending Proposals (%d) ===\n%s\n", len(pending), strings.Join(pending, "\n"))
}

// handleAgentStatus serves GET /v1/agent/status.
func (s *serveServer) handleAgentStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.agent == nil {
		json.NewEncoder(w).Encode(AgentStatusResponse{Alive: false, Model: "none"})
		return
	}
	json.NewEncoder(w).Encode(s.agent.Status())
}

// handleAgentTraces serves GET /v1/agent/traces — returns recent cycle traces.
func (s *serveServer) handleAgentTraces(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.agent == nil {
		json.NewEncoder(w).Encode([]cycleTrace{})
		return
	}
	traceFile := filepath.Join(s.agent.root, ".cog", ".state", "agent", "cycle-traces.json")
	data, err := os.ReadFile(traceFile)
	if err != nil {
		json.NewEncoder(w).Encode([]cycleTrace{})
		return
	}
	w.Write(data)
}

// handleAgentTrigger serves POST /v1/agent/trigger — manually triggers one cycle.
func (s *serveServer) handleAgentTrigger(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.agent == nil {
		w.WriteHeader(503)
		json.NewEncoder(w).Encode(map[string]string{"error": "agent not running"})
		return
	}
	go s.agent.safeCycle(context.Background())
	json.NewEncoder(w).Encode(map[string]string{"status": "triggered"})
}

// runQuietCommand runs a command and returns stdout, suppressing stderr.
func runQuietCommand(dir string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(context.Background(), name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}
