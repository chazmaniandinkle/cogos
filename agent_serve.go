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
	"sync/atomic"
	"time"

	"github.com/cogos-dev/cogos/internal/engine"
	"github.com/cogos-dev/cogos/internal/linkfeed"
	"github.com/cogos-dev/cogos/trace"
	"github.com/fsnotify/fsnotify"
	"github.com/google/uuid"
)

const (
	agentIntervalMin     = 3 * time.Minute  // fast cycles now that num_ctx=8192 makes assess ~7s
	agentIntervalMax     = 30 * time.Minute // relax to this after consecutive sleeps
	agentBusID           = "bus_agent_harness"
)

// AgentStatusResponse is the JSON payload for GET /v1/agent/status.
type AgentStatusResponse struct {
	Alive       bool    `json:"alive"`
	Running     bool    `json:"running"`
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
	Activity  *AgentActivitySummary  `json:"activity,omitempty"`
	Memory    []AgentMemoryEntry     `json:"memory,omitempty"`
	Proposals []AgentProposalEntry   `json:"proposals,omitempty"`
	Inbox     *linkfeed.AgentInboxSummary `json:"inbox,omitempty"`
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
	wakeCh   chan struct{} // event-driven wake signal (buffered 1)
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

	// Run awareness — prevents overlapping cycles
	running int32 // atomic: 1 if a cycle is in progress, 0 otherwise
}

// Status returns the current agent loop status for the API.
func (sa *ServeAgent) Status() AgentStatusResponse {
	sa.mu.RLock()
	defer sa.mu.RUnlock()

	uptime := time.Since(sa.startedAt)
	resp := AgentStatusResponse{
		Alive:       true,
		Running:     atomic.LoadInt32(&sa.running) == 1,
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

	// Enrich with activity, memory, proposals, and inbox (best-effort, outside lock)
	sa.mu.RUnlock()
	resp.Activity = sa.getActivityForAPI()
	resp.Memory = sa.getMemoryForAPI()
	resp.Proposals = sa.getProposalsForAPI()
	resp.Inbox = linkfeed.BuildInboxSummaryForAPI(sa.root)
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

		bid := entry.BusID
		isSystem := apiBusExclude[bid]

		if delta > 0 && !isSystem {
			summary.TotalEventDelta += delta
			if delta > summary.HottestDelta {
				summary.HottestDelta = delta
				summary.HottestBus = bid
				if len(summary.HottestBus) > 40 {
					summary.HottestBus = summary.HottestBus[:37] + "..."
				}
			}
		}

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
		wakeCh:               make(chan struct{}, 1), // buffered 1 so non-blocking send works
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

	// Watch proposals directory for file changes → wake agent
	go sa.watchProposals()

	// Watch inbox/links/ for new links → wake agent
	go sa.watchInboxLinks()

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

// watchProposals uses fsnotify to watch the proposals directory and wake the
// agent when new proposals are created or modified. Debounces at 500ms.
func (sa *ServeAgent) watchProposals() {
	dir := filepath.Join(sa.root, proposalsDir)
	os.MkdirAll(dir, 0o755)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("[agent] proposals watcher failed: %v", err)
		return
	}
	defer watcher.Close()

	if err := watcher.Add(dir); err != nil {
		log.Printf("[agent] cannot watch proposals dir: %v", err)
		return
	}

	log.Printf("[agent] watching proposals directory: %s", dir)

	var debounceTimer *time.Timer
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Create|fsnotify.Write) != 0 {
				// Debounce: 500ms
				if debounceTimer != nil {
					debounceTimer.Stop()
				}
				debounceTimer = time.AfterFunc(500*time.Millisecond, func() {
					log.Printf("[agent] proposals changed, waking agent")
					sa.Wake()
				})
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("[agent] proposals watcher error: %v", err)
		case <-sa.stopCh:
			return
		}
	}
}

// Wake signals the agent to run a cycle immediately instead of waiting for the
// timer. Non-blocking: if a wake is already pending it's a no-op.
// Enforces a 30-second cooldown to prevent thrashing when the agent's own
// proposals trigger bus events that re-wake it immediately.
func (sa *ServeAgent) Wake() {
	sa.mu.RLock()
	lastRun := sa.lastRun
	sa.mu.RUnlock()
	if !lastRun.IsZero() && time.Since(lastRun) < 30*time.Second {
		return // cooldown — too soon after last cycle
	}
	select {
	case sa.wakeCh <- struct{}{}:
	default: // already signaled
	}
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
	var action string

	// Run initial cycle after a short delay (let the kernel fully initialize)
	select {
	case <-time.After(60 * time.Second):
		action = sa.safeCycle(ctx)
		consecutiveSleeps = sa.updateSleepCount(action, consecutiveSleeps)
	case <-sa.wakeCh:
		log.Printf("[agent] woke by event (during init delay)")
		action = sa.safeCycle(ctx)
		consecutiveSleeps = sa.updateSleepCount(action, consecutiveSleeps)
	case <-sa.stopCh:
		return
	}

	for {
		// Self-chaining: if last cycle was "execute" and inbox still has work,
		// skip the sleep and immediately re-cycle. Cap at 5 consecutive chains
		// to prevent runaway loops.
		chainCount := 0
		const maxChains = 5
		for action != "sleep" && action != "error" && action != "skip" && chainCount < maxChains {
			inbox := linkfeed.ScanInbox(sa.root)
			if inbox.RawCount == 0 {
				break
			}
			chainCount++
			log.Printf("[agent] self-chain %d/%d: %d raw inbox items remaining, continuing immediately", chainCount, maxChains, inbox.RawCount)
			// Brief pause to let GPU cool and prevent tight-looping
			select {
			case <-time.After(10 * time.Second):
			case <-sa.stopCh:
				return
			}
			action = sa.safeCycle(ctx)
			consecutiveSleeps = sa.updateSleepCount(action, consecutiveSleeps)
		}
		if chainCount > 0 {
			log.Printf("[agent] self-chain complete: %d cycles chained", chainCount)
		}

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
			action = sa.safeCycle(ctx)
			consecutiveSleeps = sa.updateSleepCount(action, consecutiveSleeps)
		case <-sa.wakeCh:
			log.Printf("[agent] woke by event")
			action = sa.safeCycle(ctx)
			consecutiveSleeps = sa.updateSleepCount(action, consecutiveSleeps)
		case <-sa.stopCh:
			return
		}
	}
}

// safeCycle wraps runCycle with panic recovery and run-overlap guard.
func (sa *ServeAgent) safeCycle(ctx context.Context) (action string) {
	// Prevent overlapping cycles
	if !atomic.CompareAndSwapInt32(&sa.running, 0, 1) {
		log.Printf("[agent] cycle skipped: already running")
		return "skip"
	}
	defer atomic.StoreInt32(&sa.running, 0)

	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] PANIC recovered in cycle: %v", r)
			action = "error"
		}
	}()
	return sa.runCycle(ctx)
}

// updateSleepCount returns the new consecutive sleep counter.
// Errors count as sleeps — a failed cycle didn't do useful work,
// so the interval should keep doubling, not reset to minimum.
func (sa *ServeAgent) updateSleepCount(action string, current int) int {
	if action == "sleep" || action == "error" {
		return current + 1
	}
	return 0
}

// publishUserTurnReply is the seam used by ensureUserTurnReply to actually
// emit a dashboard response. Tests override this to capture calls without
// standing up a live bus manager; production points it at the bus-publishing
// implementation in agent_bus_inlet.go.
var publishUserTurnReply = publishDashboardResponse

// ensureUserTurnReply guarantees that *something* gets published to the
// dashboard response bus when a cycle consumed pending user message(s).
//
// This closes the BLOCKER from PR #7 review: the inlet drains the queue
// eagerly, so if the cycle errors during assessment, hits a sleep, or
// produces an empty execute result, the user turn would be silently lost
// without this guarantee. The chosen design (b-from-the-review): always
// publish; pick the most informative payload available; never drop.
//
// Skipped when:
//   - pendingMsgs is empty (nothing to reply to).
//   - The respond tool already published this turn (tracked atomically via
//     respondInvokedSince) — re-publishing would double-reply.
//
// Otherwise picks payload by priority:
//  1. cycle error  → "(cycle failed: <reason>)" so the user knows why
//  2. executeResult non-empty → the model's prose narration of the action
//     (the original auto-fallback path; preserves existing UX)
//  3. else → "(no reply: <action> — <reason>)" so a sleep/empty cycle
//     still acknowledges the user instead of vanishing
//
// Publication is best-effort and non-blocking (goroutine); failures only log.
func ensureUserTurnReply(
	pendingMsgs []pendingUserMsg,
	respondSnap uint64,
	cycleErr error,
	assessment *Assessment,
	executeResult string,
) {
	if len(pendingMsgs) == 0 {
		return
	}
	if respondInvokedSince(respondSnap) {
		log.Printf("[dashboard-inlet] auto-fallback suppressed: respond tool already published this turn")
		return
	}

	var pubText, reasoning string
	switch {
	case cycleErr != nil:
		pubText = fmt.Sprintf("(cycle failed: %s)", cycleErr.Error())
		reasoning = "auto-fallback: cycle errored before reply"
	case strings.TrimSpace(executeResult) != "":
		pubText = strings.TrimSpace(executeResult)
		reasoning = "auto-fallback: model did not invoke respond tool"
	default:
		action, reason := "unknown", ""
		if assessment != nil {
			action = assessment.Action
			reason = assessment.Reason
		}
		if reason != "" {
			pubText = fmt.Sprintf("(no reply: %s — %s)", action, reason)
		} else {
			pubText = fmt.Sprintf("(no reply: %s)", action)
		}
		reasoning = "auto-fallback: cycle produced no reply"
	}

	if pubText == "" {
		return
	}
	sessionID := firstUserMessageSessionID(pendingMsgs)

	go func(text, reason, sid string) {
		if _, err := publishUserTurnReply(text, reason, sid); err != nil {
			log.Printf("[dashboard-inlet] auto-fallback publish failed: %v", err)
			return
		}
		log.Printf("[dashboard-inlet] auto-fallback published agent response (%d chars, session=%q) on bus_dashboard_response", len(text), sid)
	}(pubText, reasoning, sessionID)
}

// runCycle executes a single observe-assess-execute pass.
// Returns the assessment action string for adaptive interval logic.
func (sa *ServeAgent) runCycle(ctx context.Context) string {
	start := time.Now()
	sa.mu.Lock()
	sa.cycleCount++
	cycle := sa.cycleCount
	sa.mu.Unlock()

	// Mint a fresh cycle-trace ID for this iteration so all downstream events
	// (assessment + tool dispatches inside Execute) share the same cycle_id.
	cycleID := uuid.NewString()
	ctx = WithCycleID(ctx, cycleID)

	log.Printf("[agent] cycle %d: starting (trace=%s)", cycle, cycleID)

	// Drain any pending dashboard user messages the bus inlet has queued.
	// Greedy semantics (v1): drained messages are consumed by this cycle even
	// if the model doesn't call `respond`. The reply guarantee is upheld by
	// the auto-fallback below, which always emits *something* whenever
	// pendingMsgs is non-empty — even on cycle errors, sleep-action, or
	// timeouts — so a consumed user turn never produces silence.
	pendingMsgs := drainPendingUserMessages()
	if len(pendingMsgs) > 0 {
		log.Printf("[agent] cycle %s: observing %d pending user message(s)", cycleID, len(pendingMsgs))
	}

	// Stamp ctx with the originating session_id (first pending message) so
	// the respond tool can tag its reply with the correct session and Mod³
	// can route it back to the originating client instead of broadcasting.
	// Multi-session interleaving in a single cycle is rare; if it happens,
	// the first session wins for tool-tagging and the auto-fallback will
	// publish per-session replies (see ensureUserTurnReply below).
	turnSessionID := firstUserMessageSessionID(pendingMsgs)
	if turnSessionID != "" {
		ctx = WithSessionID(ctx, turnSessionID)
	}

	// Cheap checks gate: skip model call if nothing changed. Pending user
	// messages always bypass the gate — a user is waiting for a reply.
	if len(pendingMsgs) == 0 {
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

			// Skips don't write to rolling memory — only real assessments do.
			// This prevents error/skip noise from diluting the compressed narrative.

			return "sleep"
		}
	}

	// Build the baseline prose observation (git/coherence/activity/etc).
	// This is still used as the decomposition context (sa.lastObservation).
	proseObservation := sa.gatherObservation()

	// Fetch the foveated-context manifest via HTTP loopback. The anchor query
	// is drawn from the first pending user message when present; otherwise
	// a generic "current workspace state" prompt so the manifest still falls
	// back to salience-only scoring and returns something useful.
	//
	// This is the substrate seam: the manifest already knows content hashes,
	// tiers, and salience per source — we surface all of it as typed records
	// so the Assess phase sees "what matters" as structured data rather than
	// having to infer it from prose.
	anchorPrompt := firstUserMessageText(pendingMsgs)
	if anchorPrompt == "" {
		anchorPrompt = "current workspace state"
	}
	manifest, mfErr := fetchFoveatedManifest(ctx, anchorPrompt)
	if mfErr != nil {
		// Non-fatal — the cycle still runs on the prose baseline + user msgs.
		log.Printf("[agent] cycle %s: foveated manifest fetch failed: %v (falling back to prose-only observation)", cycleID, mfErr)
	}

	// Build typed observation records. Pending user messages carry salience
	// 1.0 (always highest); foveated blocks inherit their max source salience;
	// the prose baseline is elided from the JSON envelope and attached after
	// it as a low-salience `workspace_summary` section so the model gets both
	// the substrate signal (top) and the narrative context (bottom).
	records := buildObservationRecords(
		pendingMsgs,
		manifest,
		"",                      // gitSummary — already in prose baseline
		"",                      // coherenceSummary — already in prose baseline
		"",                      // busSummary — already in prose baseline
	)

	anchor := ""
	goal := ""
	if manifest != nil {
		anchor = manifest.Anchor
		goal = manifest.Goal
	}
	recordsJSON := renderObservationRecords(records, anchor, goal)

	// Compose the final observation: typed records on top (substrate-grade
	// signal, JSON), prose baseline below (legacy narrative). The JSON header
	// is labeled so the model can anchor on it; deletion of the old
	// `=== PRIORITY` prepend is intentional — salience is now carried by
	// record.salience, not by English.
	var obsBuf strings.Builder
	obsBuf.WriteString("=== Observation Records (typed, salience-ordered) ===\n")
	obsBuf.WriteString(recordsJSON)
	obsBuf.WriteString("\n\n")
	obsBuf.WriteString("=== Workspace Summary (prose, low-salience) ===\n")
	obsBuf.WriteString(proseObservation)
	observation := obsBuf.String()

	// System prompt: concise, no thinking tags (Gemma E4B doesn't need them).
	// JSON mode is enforced by the harness via response_format.
	systemPrompt := fmt.Sprintf(`You are the CogOS kernel agent on a local node. Workspace: %s

Respond ONLY with a JSON object. No markdown, no explanation, no thinking.

{"action": "<sleep|observe|propose|execute|escalate>", "reason": "<brief reason>", "urgency": <0.0-1.0>, "target": "<URI or path or empty>"}

Actions:
- sleep: nothing needs attention, rest until next cycle
- observe: gather more info using tools (memory_search, memory_read, workspace_status, coherence_check)
- propose: write a proposal using the propose tool — for suggesting new plans or changes
- execute: do approved work using your tools — enrich links, pull link feed, process inbox items. Choose this when you have an approved plan or known tasks to complete.
- escalate: something needs human or cloud-model attention

You are an observer and advisor. Your proposals are staged in a directory — they do not modify anything. The user reviews them at their convenience. You cannot interrupt anyone. Act confidently.

Rules:
- You may ONLY return one of these actions: sleep, observe, propose, execute, escalate. No other values.
- PRIORITY: If the inbox has raw items (Raw items > 0), choose "execute" and process them with enrich_link. This is your primary job right now.
- Only choose "propose" if you have a genuinely NEW idea. Do not re-propose plans about things you've already proposed.
- Look at your Recent Cycle Memory. If the last 3+ entries show the same action, you MUST pick a DIFFERENT one.
- After reading a proposal's content, acknowledge it with acknowledge_proposal so you don't re-process it.
- When nothing needs attention, sleep. Sleeping is good.

You also have a wait tool available in the execute phase. Use it when an observation warrants no proposal, no bus event, and no further investigation — nothing needs doing right now. Prefer wait over fabricating plans or rationalizing in prose. It ends the current cycle cleanly.`, sa.root)

	// Run assessment phase (JSON mode)
	assessment, err := sa.harness.Assess(ctx, systemPrompt, observation)

	// Hard loop breaker: if the model chose the same action 3+ times,
	// override to a different action. E4B can reason about the rule
	// but can't reliably follow it, so we enforce it in code.
	if err == nil && sa.cycleMemory != nil {
		recent := sa.cycleMemory.recent(3)
		if len(recent) >= 3 {
			allSame := recent[0].Action == recent[1].Action && recent[1].Action == recent[2].Action
			if allSame && assessment.Action == recent[0].Action {
				old := assessment.Action
				switch old {
				case "observe":
					if sa.lastProposalCount > 0 {
						assessment.Action = "execute"
						assessment.Reason = fmt.Sprintf("Hard loop break: %s×3 → execute (approved work exists)", old)
					} else {
						assessment.Action = "sleep"
						assessment.Reason = fmt.Sprintf("Hard loop break: %s×3 → sleep", old)
					}
				case "propose":
					assessment.Action = "execute"
					assessment.Reason = fmt.Sprintf("Hard loop break: %s×3 → execute (stop proposing, start doing)", old)
				case "execute":
					// Don't break execute chains if there's still inbox work
					inbox := linkfeed.ScanInbox(sa.root)
					if inbox.RawCount > 0 {
						// Let it keep executing — the self-chain will handle it
						log.Printf("[agent] cycle %d: hard loop break suppressed: execute×3 but %d raw inbox items remain", cycle, inbox.RawCount)
					} else {
						assessment.Action = "sleep"
						assessment.Reason = fmt.Sprintf("Hard loop break: %s×3 → sleep (rest after work)", old)
					}
				default:
					assessment.Action = "sleep"
					assessment.Reason = fmt.Sprintf("Hard loop break: %s×3 → sleep", old)
				}
				log.Printf("[agent] cycle %d: hard loop break: %s → %s", cycle, old, assessment.Action)
			}
		}
	}

	// Work nudge (runs LAST — overrides both LLM choice and hard loop breaker):
	// If the inbox has raw items and no recent execute, force execute.
	// This is the highest priority override because real work exists.
	if err == nil && sa.cycleMemory != nil && assessment.Action != "execute" {
		inbox := linkfeed.ScanInbox(sa.root)
		if inbox.RawCount > 0 {
			recent := sa.cycleMemory.recent(2)
			noRecentExecute := true
			for _, r := range recent {
				if r.Action == "execute" {
					noRecentExecute = false
					break
				}
			}
			if noRecentExecute && len(recent) >= 2 {
				old := assessment.Action
				assessment.Action = "execute"
				assessment.Reason = fmt.Sprintf("Work nudge: %d raw inbox items waiting, overriding %s → execute", inbox.RawCount, old)
				log.Printf("[agent] cycle %d: work nudge: %s → execute (%d raw inbox items)", cycle, old, inbox.RawCount)
			}
		}
	}

	// Emit a cycle.assessment trace event as soon as the (possibly overridden)
	// assessment is final. Mapping note: Assessment.Urgency → CycleEvent
	// Confidence, Assessment.Reason → CycleEvent Rationale. Best-effort.
	if err == nil && assessment != nil {
		if ev, bErr := trace.NewAssessment(
			engine.TraceIdentity(),
			cycleID,
			assessment.Action,
			assessment.Urgency,
			assessment.Reason,
		); bErr == nil {
			emitCycleEvent(ev)
		}
	}

	// Snapshot the respond-tool invocation count before entering Execute so
	// the auto-fallback publisher below can tell whether the model already
	// called respond this turn. If it did, we must NOT also publish
	// executeResult — that would double-reply on the dashboard bus.
	respondSnap := respondInvokeSnapshot()

	var executeResult string
	if err == nil && assessment.Action != "sleep" {
		// Execute phase gets a different prompt that encourages tool chaining
		executePrompt := fmt.Sprintf(`You are the CogOS kernel agent executing an action. Workspace: %s

You decided: %s (reason: %s, target: %s)

Now execute this using your tools. You may call MULTIPLE tools in sequence:
- Call read_proposal to read proposals, then call acknowledge_proposal to mark them processed
- Call propose to write new proposals in response to what you read
- Call memory_search to find docs, then call memory_read to read them
- Call coherence_check, then propose a fix if needed
- Call list_inbox to see raw inbox filenames, then call enrich_link with each filename to process them
- Call pull_link_feed to pull new links from Discord
- Call wait when nothing needs doing — no proposal, no bus event, no further investigation. Prefer wait over fabricating plans or rationalizing in prose. It ends the current cycle cleanly.

Do not just describe what you would do — actually call the tools. When you are finished acting, respond with a brief summary of what you did.`, sa.root, assessment.Action, assessment.Reason, assessment.Target)

		task := fmt.Sprintf("Execute: %s\nTarget: %s\nReason: %s",
			assessment.Action, assessment.Target, assessment.Reason)
		// Limit execute phase to 60 seconds to prevent GPU exhaustion
		execCtx, execCancel := context.WithTimeout(ctx, 60*time.Second)
		executeResult, _ = sa.harness.Execute(execCtx, executePrompt, task)
		execCancel()
	}
	duration := time.Since(start)

	// Reply guarantee for consumed user turns. If pendingMsgs was non-empty,
	// we owe the dashboard a reply — even on cycle errors, sleep-action, or
	// timeouts. Run this BEFORE the err short-circuit below; otherwise an
	// assessment error drains the user turn and silently drops the reply
	// (the original BLOCKER from PR #7 review).
	//
	// Skip when respond already landed this turn (snapshot delta > 0):
	// publishing again would double-reply on the dashboard bus.
	ensureUserTurnReply(pendingMsgs, respondSnap, err, assessment, executeResult)

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

	// Coherence check (truncate output — full drift reports can be 600KB+)
	if coh, err := runQuietCommand(sa.root, "./scripts/cog", "coherence", "check"); err == nil {
		if strings.Contains(coh, "coherent") {
			sb.WriteString("Coherence: OK\n")
			cachedCoherenceOK = true
		} else {
			// Just note the drift; don't dump the full report into observation
			lines := strings.Split(strings.TrimSpace(coh), "\n")
			sb.WriteString(fmt.Sprintf("Coherence: DRIFT detected (%d lines in report). Use coherence_check tool for details.\n", len(lines)))
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

	// Inject link feed status
	sb.WriteString(sa.gatherLinkFeedStatus())

	// Inject inbox summary
	sb.WriteString(sa.gatherInboxSummary())

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

		bid := entry.BusID
		isSystem := systemBuses[bid]

		// Only count non-system bus events in the activity delta
		if delta > 0 && !isSystem {
			totalDelta += delta
			if delta > hottestDelta {
				hottestDelta = delta
				hottestBus = bid
			}
		}

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
	var acknowledged, approved, rejected int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		contentStr := string(content)
		status := extractFMField(contentStr, "status")
		switch status {
		case "pending":
			title := extractFMField(contentStr, "title")
			pType := extractFMField(contentStr, "type")
			pending = append(pending, fmt.Sprintf("  [%s] %s", pType, title))
		case "acknowledged":
			acknowledged++
		case "approved":
			approved++
		case "rejected":
			rejected++
		}
	}

	if len(pending) == 0 && acknowledged == 0 && approved == 0 && rejected == 0 {
		return ""
	}

	var sb strings.Builder
	if len(pending) > 0 {
		sb.WriteString(fmt.Sprintf("\n=== Pending Proposals (%d) ===\n%s\n", len(pending), strings.Join(pending, "\n")))
	}
	if acknowledged > 0 || approved > 0 || rejected > 0 {
		sb.WriteString(fmt.Sprintf("Previously processed: %d acknowledged, %d approved, %d rejected\n", acknowledged, approved, rejected))
	}

	return sb.String()
}

// gatherLinkFeedStatus returns the link feed timing status for observation injection.
func (sa *ServeAgent) gatherLinkFeedStatus() string {
	_, err := linkfeed.ReadDiscordAuth(sa.root)
	if err != nil {
		return "\n=== Link Feed ===\nNot configured (missing auth)\n"
	}

	ago, err := linkfeed.LinkFeedLastPull(sa.root)
	if err != nil {
		return "\n=== Link Feed ===\nLast pull: never\n"
	}

	var sb strings.Builder
	sb.WriteString("\n=== Link Feed ===\n")
	if ago > linkfeed.LinkFeedCheckInterval {
		sb.WriteString(fmt.Sprintf("Last pull: %s ago (overdue)\n", formatAgo(ago)))
	} else {
		remaining := linkfeed.LinkFeedCheckInterval - ago
		sb.WriteString(fmt.Sprintf("Last pull: %s ago (next in %s)\n", formatAgo(ago), formatAgo(remaining)))
	}
	return sb.String()
}

// gatherInboxSummary returns the inbox status for observation injection.
func (sa *ServeAgent) gatherInboxSummary() string {
	inbox := linkfeed.ScanInbox(sa.root)
	if inbox.TotalCount == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n=== Inbox ===\n")
	if inbox.RawCount > 0 {
		sb.WriteString(fmt.Sprintf("Raw items: %d new links awaiting enrichment\n", inbox.RawCount))
		for _, f := range inbox.NewestRaw {
			sb.WriteString(fmt.Sprintf("  - %s\n", f))
		}
	}
	sb.WriteString(fmt.Sprintf("Enriched: %d | Failed: %d | Total: %d\n", inbox.EnrichedCount, inbox.FailedCount, inbox.TotalCount))
	return sb.String()
}

// watchInboxLinks uses fsnotify to watch the inbox/links/ directory and wake
// the agent when new links arrive. Runs alongside watchProposals.
func (sa *ServeAgent) watchInboxLinks() {
	dir := filepath.Join(sa.root, linkfeed.InboxLinksRelPath)
	os.MkdirAll(dir, 0o755)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("[agent] inbox watcher failed: %v", err)
		return
	}
	defer watcher.Close()

	if err := watcher.Add(dir); err != nil {
		log.Printf("[agent] cannot watch inbox dir: %v", err)
		return
	}

	log.Printf("[agent] watching inbox directory: %s", dir)

	var debounceTimer *time.Timer
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Create|fsnotify.Write) != 0 {
				if debounceTimer != nil {
					debounceTimer.Stop()
				}
				debounceTimer = time.AfterFunc(2*time.Second, func() {
					log.Printf("[agent] inbox changed, waking agent")
					sa.Wake()
				})
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("[agent] inbox watcher error: %v", err)
		case <-sa.stopCh:
			return
		}
	}
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
	if atomic.LoadInt32(&s.agent.running) == 1 {
		w.WriteHeader(409)
		json.NewEncoder(w).Encode(map[string]string{"status": "already_running"})
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
