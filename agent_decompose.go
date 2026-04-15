// agent_decompose.go — Wire decomposition into the agent loop.
//
// After each cycle, the assessment is decomposed into Tier 0 (one sentence).
// The last N Tier 0 sentences are injected into the next observation as
// rolling compressed memory. This is the first self-feeding wire in the
// CogOS Hypercycle: the agent observes its own compressed history.
//
// The decomposition uses the same AgentHarness that runs the assessment,
// calling GenerateJSON with Tier 0 prompts. If decomposition fails (Ollama
// busy, timeout, etc.), the cycle continues without it — best effort.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// maxRollingMemory is how many Tier 0 sentences to keep in the rolling buffer.
	maxRollingMemory = 10

	// decompTimeoutSec is the maximum time to spend on per-cycle decomposition.
	// Must be short enough not to delay the next cycle.
	decompTimeoutSec = 30
)

// cycleMemoryEntry is one cycle's compressed memory.
type cycleMemoryEntry struct {
	Cycle     int64     `json:"cycle"`
	Action    string    `json:"action"`
	Urgency   float64   `json:"urgency"`
	Sentence  string    `json:"sentence"`   // Tier 0
	Timestamp time.Time `json:"timestamp"`
}

// agentCycleMemory maintains a rolling buffer of decomposed cycle summaries.
type agentCycleMemory struct {
	mu      sync.RWMutex
	entries []cycleMemoryEntry
	maxSize int
}

func newAgentCycleMemory(maxSize int) *agentCycleMemory {
	return &agentCycleMemory{
		entries: make([]cycleMemoryEntry, 0, maxSize),
		maxSize: maxSize,
	}
}

// append adds a new entry, evicting the oldest if at capacity.
func (m *agentCycleMemory) append(entry cycleMemoryEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.entries) >= m.maxSize {
		m.entries = m.entries[1:]
	}
	m.entries = append(m.entries, entry)
}

// recent returns the last n entries (or fewer if not enough).
func (m *agentCycleMemory) recent(n int) []cycleMemoryEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if n > len(m.entries) {
		n = len(m.entries)
	}
	result := make([]cycleMemoryEntry, n)
	copy(result, m.entries[len(m.entries)-n:])
	return result
}

// len returns the current buffer size.
func (m *agentCycleMemory) len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.entries)
}

// formatForObservation renders the rolling memory as a compact string
// suitable for injection into gatherObservation().
func (m *agentCycleMemory) formatForObservation() string {
	entries := m.recent(maxRollingMemory)
	if len(entries) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n=== Recent Cycle Memory (compressed) ===\n")
	for _, e := range entries {
		ago := time.Since(e.Timestamp).Round(time.Minute)
		sb.WriteString(fmt.Sprintf("[%s ago] %s (u=%.1f): %s\n",
			formatAgo(ago), e.Action, e.Urgency, e.Sentence))
	}
	return sb.String()
}

// formatAgo produces a human-readable duration like "5m" or "2h".
func formatAgo(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

// decomposeAndStore runs Tier 0 decomposition on a cycle's assessment
// and stores the result in the rolling memory buffer.
// Called asynchronously after each cycle completes — best effort.
func (sa *ServeAgent) decomposeAndStore(ctx context.Context, cycle int64, assessment *Assessment) {
	// Build input text from the assessment
	input := fmt.Sprintf("Agent cycle %d assessment:\nAction: %s\nUrgency: %.1f\nReason: %s\nTarget: %s",
		cycle, assessment.Action, assessment.Urgency, assessment.Reason, assessment.Target)

	// Add workspace context for richer decomposition
	if obs := sa.lastObservation; obs != "" {
		// Truncate observation to avoid overwhelming Tier 0
		if len(obs) > 500 {
			obs = obs[:500] + "..."
		}
		input += "\n\nWorkspace state at time of assessment:\n" + obs
	}

	// Run Tier 0 decomposition with a tight timeout
	decompCtx, cancel := context.WithTimeout(ctx, decompTimeoutSec*time.Second)
	defer cancel()

	content, err := sa.harness.GenerateJSON(decompCtx, tier0SystemPrompt(), tierUserPrompt(input))
	if err != nil {
		log.Printf("[agent] cycle %d: decompose failed (best-effort): %v", cycle, err)
		// Fall back to a mechanical summary — still useful as memory
		sa.cycleMemory.append(cycleMemoryEntry{
			Cycle:     cycle,
			Action:    assessment.Action,
			Urgency:   assessment.Urgency,
			Sentence:  fmt.Sprintf("Cycle %d: %s (urgency %.1f) — %s", cycle, assessment.Action, assessment.Urgency, assessment.Reason),
			Timestamp: time.Now(),
		})
		return
	}

	var tier0 Tier0Result
	if err := json.Unmarshal([]byte(content), &tier0); err != nil {
		log.Printf("[agent] cycle %d: decompose parse failed: %v", cycle, err)
		// Fall back to reason string
		sa.cycleMemory.append(cycleMemoryEntry{
			Cycle:     cycle,
			Action:    assessment.Action,
			Urgency:   assessment.Urgency,
			Sentence:  assessment.Reason,
			Timestamp: time.Now(),
		})
		return
	}

	sa.cycleMemory.append(cycleMemoryEntry{
		Cycle:     cycle,
		Action:    assessment.Action,
		Urgency:   assessment.Urgency,
		Sentence:  tier0.Summary,
		Timestamp: time.Now(),
	})

	log.Printf("[agent] cycle %d: decomposed → %q", cycle, tier0.Summary)

	// Emit decomposition event to the bus
	sa.emitEvent("agent.decompose", map[string]interface{}{
		"cycle":    cycle,
		"action":   assessment.Action,
		"sentence": tier0.Summary,
	})

	// Persist to disk so memory survives kernel restarts
	sa.persistCycleMemory()
}

// persistCycleMemory writes the rolling buffer to disk as JSON.
func (sa *ServeAgent) persistCycleMemory() {
	memDir := filepath.Join(sa.root, ".cog", ".state", "agent")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		return
	}
	memFile := filepath.Join(memDir, "cycle-memory.json")

	entries := sa.cycleMemory.recent(maxRollingMemory)
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(memFile, data, 0o644)
}

// loadCycleMemory restores the rolling buffer from disk on startup.
func (sa *ServeAgent) loadCycleMemory() {
	memFile := filepath.Join(sa.root, ".cog", ".state", "agent", "cycle-memory.json")
	data, err := os.ReadFile(memFile)
	if err != nil {
		return // No prior memory — fresh start
	}

	var entries []cycleMemoryEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		log.Printf("[agent] failed to load cycle memory: %v", err)
		return
	}

	for _, e := range entries {
		sa.cycleMemory.append(e)
	}
	log.Printf("[agent] loaded %d cycle memory entries from disk", len(entries))
}
