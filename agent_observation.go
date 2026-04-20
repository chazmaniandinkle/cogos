// agent_observation.go — Typed observation records for the metabolic cycle.
//
// This file replaces the ad-hoc string-concat observation shape with a
// schema-bound record stream. The runCycle drains pending user messages and
// fetches the foveated-context manifest, converts both into ObservationRecord
// values, and serializes the set into a JSON array that the Assess phase sees.
//
// Why: the old shape used a hardcoded `=== PRIORITY` prose-prepend to force
// Gemma E4B to prioritize user messages. That was a prose-patch on a
// structural problem — the observation should already carry salience as data
// so the model doesn't need to be browbeaten into reading it. Typed records
// with user_message salience=1.0 put the priority signal where it belongs:
// in the substrate, not in English.
//
// Salience contract:
//   - user_message: 1.0 — always highest.
//   - knowledge_block: inherits max source.Salience from the foveated manifest.
//   - bus_event / git_state / coherence_state: fixed bands (0.3..0.5).

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// ObservationRecord is one unit of observable state fed to the Assess phase.
//
// Kinds:
//   - user_message    — pending dashboard chat input (always highest salience)
//   - knowledge_block — a foveated-manifest block (tier 0..3)
//   - bus_event       — a recent non-system bus event worth observing
//   - git_state       — summary of working-tree state
//   - coherence_state — drift/OK summary
type ObservationRecord struct {
	Kind      string    `json:"kind"`
	Source    string    `json:"source"`
	Digest    string    `json:"digest,omitempty"`
	Salience  float64   `json:"salience"`
	Tier      string    `json:"tier,omitempty"`
	Tokens    int       `json:"tokens,omitempty"`
	Preview   string    `json:"preview,omitempty"`
	Content   string    `json:"content,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// foveatedManifestRequest mirrors the JSON body handleFoveatedContext decodes.
// Kept as a local type so this file does not import the engine package (the
// main package compiles against engine's public surface only).
type foveatedManifestRequest struct {
	Prompt    string                `json:"prompt"`
	Query     string                `json:"query,omitempty"`
	Iris      foveatedManifestIris  `json:"iris"`
	Profile   string                `json:"profile,omitempty"`
	SessionID string                `json:"session_id,omitempty"`
}

type foveatedManifestIris struct {
	Size int `json:"size"`
	Used int `json:"used"`
}

// foveatedManifestResponse mirrors engine.foveatedResponse for decode.
type foveatedManifestResponse struct {
	Context         string                  `json:"context"`
	Tokens          int                     `json:"tokens"`
	Anchor          string                  `json:"anchor"`
	Goal            string                  `json:"goal"`
	IrisPressure    float64                 `json:"iris_pressure"`
	CoherenceScore  float64                 `json:"coherence_score"`
	TierBreakdown   map[string]int          `json:"tier_breakdown"`
	EffectiveBudget int                     `json:"effective_budget"`
	Blocks          []foveatedManifestBlock `json:"blocks"`
}

type foveatedManifestBlock struct {
	Tier      string                        `json:"tier"`
	Name      string                        `json:"name"`
	Hash      string                        `json:"hash"`
	Tokens    int                           `json:"tokens"`
	Stability int                           `json:"stability"`
	Preview   string                        `json:"preview,omitempty"`
	Sources   []foveatedManifestBlockSource `json:"sources,omitempty"`
}

type foveatedManifestBlockSource struct {
	URI      string  `json:"uri"`
	Title    string  `json:"title,omitempty"`
	Path     string  `json:"path,omitempty"`
	Salience float64 `json:"salience,omitempty"`
	Reason   string  `json:"reason,omitempty"`
	Summary  string  `json:"summary,omitempty"`
}

// kernelLoopbackURL returns the base URL used for in-process HTTP calls to
// the kernel's own /v1 surface. We're running inside the kernel daemon, so
// localhost with the configured port is always correct.
func kernelLoopbackURL() string {
	if v := os.Getenv("COG_KERNEL_PORT"); v != "" {
		return "http://localhost:" + v
	}
	return fmt.Sprintf("http://localhost:%d", defaultServePort)
}

// fetchFoveatedManifest calls the kernel's own /v1/context/foveated endpoint
// via HTTP loopback. This is in-process (same daemon, same listener), so the
// hop is cheap and avoids re-plumbing an engine.Process into the agent.
//
// prompt may be empty; when empty, the manifest falls back to salience-only
// scoring for knowledge selection.
//
// Returns (nil, err) on network / decode / non-2xx. Callers should treat the
// manifest as best-effort — a failure here does not invalidate the cycle.
func fetchFoveatedManifest(ctx context.Context, prompt string) (*foveatedManifestResponse, error) {
	// Conservative iris signal — the agent cycle runs at 8K ctx (see harness
	// Options), with generous headroom for the assessment turn itself. The
	// manifest endpoint clamps its budget from this, so pressure≈used/size.
	req := foveatedManifestRequest{
		Prompt:    prompt,
		Iris:      foveatedManifestIris{Size: 8192, Used: 2048},
		Profile:   "agent_harness",
		SessionID: "agent_harness",
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal foveated request: %w", err)
	}

	// Short timeout — the manifest is an enrichment, not a critical path.
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, kernelLoopbackURL()+"/v1/context/foveated", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("foveated loopback: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("foveated loopback: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	var out foveatedManifestResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode foveated response: %w", err)
	}
	return &out, nil
}

// buildObservationRecords converts the three inputs (pending user messages,
// foveated manifest blocks, base prose observation) into a single typed record
// stream, ordered by salience descending.
//
// The caller owns deciding when each input is available; the function treats
// nil/empty inputs as "skip that input cleanly" and does no I/O itself.
func buildObservationRecords(
	pending []pendingUserMsg,
	manifest *foveatedManifestResponse,
	gitSummary string,
	coherenceSummary string,
	busSummary string,
) []ObservationRecord {
	now := time.Now().UTC()
	var records []ObservationRecord

	// 1. User messages — salience 1.0 (always top).
	for _, m := range pending {
		ts := m.Ts
		if ts.IsZero() {
			ts = now
		}
		records = append(records, ObservationRecord{
			Kind:      "user_message",
			Source:    "mod3_dashboard",
			Salience:  1.0,
			Content:   m.Text,
			Timestamp: ts,
		})
	}

	// 2. Knowledge blocks from the foveated manifest. Each block inherits the
	//    max source.Salience — this is the substrate signal we want surfaced.
	if manifest != nil {
		for _, blk := range manifest.Blocks {
			salience := 0.0
			for _, src := range blk.Sources {
				if src.Salience > salience {
					salience = src.Salience
				}
			}
			// Block without sources (e.g. node health, events) — give it a
			// middling default so it still sorts below user messages but
			// above the raw prose state.
			if len(blk.Sources) == 0 {
				salience = 0.4
			}
			records = append(records, ObservationRecord{
				Kind:      "knowledge_block",
				Source:    "foveated_manifest",
				Digest:    blk.Hash,
				Salience:  salience,
				Tier:      blk.Tier,
				Tokens:    blk.Tokens,
				Preview:   blk.Preview,
				Timestamp: now,
			})
		}
	}

	// 3. Baseline prose state — git / coherence / bus activity. These are
	//    single-record summaries rather than per-file expansion; the point
	//    is that the model sees them at a known low salience and stops
	//    mistaking them for the primary signal.
	if s := strings.TrimSpace(gitSummary); s != "" {
		records = append(records, ObservationRecord{
			Kind:      "git_state",
			Source:    "workspace_state",
			Salience:  0.3,
			Preview:   s,
			Timestamp: now,
		})
	}
	if s := strings.TrimSpace(coherenceSummary); s != "" {
		records = append(records, ObservationRecord{
			Kind:      "coherence_state",
			Source:    "workspace_state",
			Salience:  0.35,
			Preview:   s,
			Timestamp: now,
		})
	}
	if s := strings.TrimSpace(busSummary); s != "" {
		records = append(records, ObservationRecord{
			Kind:      "bus_event",
			Source:    "bus_registry",
			Salience:  0.5,
			Preview:   s,
			Timestamp: now,
		})
	}

	return records
}

// renderObservationRecords serializes a record stream to the JSON array shape
// the Assess phase consumes. The format is:
//
//	{"records": [...], "record_count": N, "generated_at": "..."}
//
// Wrapping in an object (rather than a bare array) leaves room for future
// metadata — anchor, goal, iris pressure — without breaking the model's
// expectations. The envelope is stable; internal fields can grow.
//
// If marshaling fails (shouldn't happen with the fixed struct shape), returns
// a best-effort error string so the cycle continues rather than aborting.
func renderObservationRecords(records []ObservationRecord, anchor, goal string) string {
	env := struct {
		Records     []ObservationRecord `json:"records"`
		RecordCount int                 `json:"record_count"`
		GeneratedAt time.Time           `json:"generated_at"`
		Anchor      string              `json:"anchor,omitempty"`
		Goal        string              `json:"goal,omitempty"`
	}{
		Records:     records,
		RecordCount: len(records),
		GeneratedAt: time.Now().UTC(),
		Anchor:      anchor,
		Goal:        goal,
	}
	buf, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"records": [], "error": %q}`, err.Error())
	}
	return string(buf)
}

// firstUserMessageText returns the first pending user message's text (for
// anchor-query extraction), or an empty string if none are pending. Multiple
// pending messages keep using the first as the anchor on the assumption that
// back-to-back turns are topically related; the rest still appear as records.
func firstUserMessageText(pending []pendingUserMsg) string {
	for _, m := range pending {
		if s := strings.TrimSpace(m.Text); s != "" {
			return s
		}
	}
	return ""
}

// firstUserMessageSessionID returns the first pending user message's
// session_id, or "" if none. Used to tag the cycle's reply so Mod³ can
// route it to the originating client instead of broadcasting.
func firstUserMessageSessionID(pending []pendingUserMsg) string {
	for _, m := range pending {
		if m.SessionID != "" {
			return m.SessionID
		}
	}
	return ""
}

// uniqueUserMessageSessionIDs returns the distinct session_ids observed across
// the pending queue, in first-seen order. Messages with an empty session_id
// collapse into a single "" entry that publishers interpret as a broadcast.
//
// Used by the cycle's reply paths (respond tool + auto-fallback) to fan out a
// single agent response to every originating session when drainPendingUserMessages
// returned messages from multiple tabs/clients. Without this fan-out, a cycle
// that consumed N messages from N different sessions would only reply on one
// session (whichever was first), leaving the others silently waiting.
//
// Returns nil when pending is empty so callers can distinguish "no reply owed"
// from "broadcast reply owed".
func uniqueUserMessageSessionIDs(pending []pendingUserMsg) []string {
	if len(pending) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(pending))
	out := make([]string, 0, len(pending))
	for _, m := range pending {
		if seen[m.SessionID] {
			continue
		}
		seen[m.SessionID] = true
		out = append(out, m.SessionID)
	}
	return out
}
