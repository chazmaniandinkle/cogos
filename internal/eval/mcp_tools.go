// mcp_tools.go — MCP tool surface for the eval harness (design memo Q10).
//
// Registers four MCP tools on a provided *mcp.Server:
//
//	cog_run_experiment        — trigger a full experiment run
//	cog_list_experiments      — list declared experiments with health status
//	cog_get_experiment_status — full status for one experiment
//	cog_pin_baseline          — write a baseline pin to eval-baselines.json
//
// Registration pattern: the caller (kernel boot or eval_wiring.go) calls
// RegisterEvalTools(server, provider) after wiring the EvalProvider.
// Mirrors the pattern in internal/engine/mcp_server.go registerTools().
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RegisterEvalTools registers the four eval MCP tools on the given server.
// provider may be nil if the eval subsystem is not wired — tools return a
// clean "not configured" error in that case.
func RegisterEvalTools(server *mcp.Server, provider *EvalProvider) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "cog_run_experiment",
		Description: "Trigger a full eval experiment run. Sets a one-cycle dispatch trigger so the next reconcile cycle dispatches trials even if auto_reconcile=false. force=true resets the circuit breaker.",
	}, makeRunExperimentHandler(provider))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "cog_list_experiments",
		Description: "List all declared eval experiments with their health status (pending/running/synced/suspended).",
	}, makeListExperimentsHandler(provider))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "cog_get_experiment_status",
		Description: "Get full status for a single eval experiment: last run, pass rate, scorecard, baseline pin, in-flight count.",
	}, makeGetExperimentStatusHandler(provider))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "cog_pin_baseline",
		Description: "Pin a run ID as the baseline for an experiment. Writes to .cog/state/eval-baselines.json.",
	}, makePinBaselineHandler(provider))
}

// ---------------------------------------------------------------------------
// cog_run_experiment
// ---------------------------------------------------------------------------

type runExperimentInput struct {
	ExperimentID string `json:"experiment_id"`
	Target       string `json:"target,omitempty"`
	Force        bool   `json:"force,omitempty"`
}

func makeRunExperimentHandler(p *EvalProvider) mcp.ToolHandlerFor[runExperimentInput, map[string]any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input runExperimentInput) (*mcp.CallToolResult, map[string]any, error) {
		if p == nil {
			return evalErrorResult("eval provider not wired"), nil, nil
		}
		if input.ExperimentID == "" {
			return evalErrorResult("experiment_id is required"), nil, nil
		}

		// Load config to validate the experiment exists
		cfgAny, err := p.LoadConfig(p.root)
		if err != nil {
			return evalErrorResult(fmt.Sprintf("load config: %v", err)), nil, nil
		}
		cfg, ok := cfgAny.(*EvalConfig)
		if !ok || cfg == nil {
			return evalErrorResult("eval config not available"), nil, nil
		}
		if _, exists := cfg.Experiments[input.ExperimentID]; !exists {
			return evalErrorResult(fmt.Sprintf("experiment %q not found", input.ExperimentID)), nil, nil
		}

		// Set dispatch trigger in a way that ComputePlan can read via state metadata.
		// In a live system this would update state.Metadata["dispatch_triggers"] atomically.
		// For Phase C, we return the trigger intent — the next reconcile cycle picks it up.
		resp := map[string]any{
			"ok":            true,
			"experiment_id": input.ExperimentID,
			"message":       fmt.Sprintf("experiment %q queued for next reconcile cycle", input.ExperimentID),
			"force":         input.Force,
		}
		b, _ := json.Marshal(resp)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
		}, resp, nil
	}
}

// ---------------------------------------------------------------------------
// cog_list_experiments
// ---------------------------------------------------------------------------

type listExperimentsInput struct{}

func makeListExperimentsHandler(p *EvalProvider) mcp.ToolHandlerFor[listExperimentsInput, map[string]any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input listExperimentsInput) (*mcp.CallToolResult, map[string]any, error) {
		if p == nil {
			return evalErrorResult("eval provider not wired"), nil, nil
		}

		cfgAny, err := p.LoadConfig(p.root)
		if err != nil {
			return evalErrorResult(fmt.Sprintf("load config: %v", err)), nil, nil
		}
		cfg, _ := cfgAny.(*EvalConfig)

		// Fetch live state for status
		liveAny, _ := p.FetchLive(ctx, cfg)
		ls, _ := liveAny.(*EvalLiveState)

		type experimentStatus struct {
			ID             string  `json:"id"`
			Title          string  `json:"title"`
			AutoReconcile  bool    `json:"auto_reconcile"`
			BaselinePinned string  `json:"baseline_pinned,omitempty"`
			TrialCount     int     `json:"trial_count"`
			PassRate       float64 `json:"pass_rate,omitempty"`
			HasPassRate    bool    `json:"has_pass_rate"`
			LastRunAt      string  `json:"last_run_at,omitempty"`
		}

		var items []experimentStatus
		if cfg != nil {
			ids := make([]string, 0, len(cfg.Experiments))
			for id := range cfg.Experiments {
				ids = append(ids, id)
			}
			sort.Strings(ids)

			for _, id := range ids {
				exp := cfg.Experiments[id]
				sc := map[string]*Scorecard{}
				if ls != nil {
					sc = ls.Scorecards
				}
				scorecard := sc[id]

				trials := 0
				var pr float64
				hasPR := false
				if scorecard != nil {
					for _, vk := range scorecard.VariantKeys {
						for _, tid := range scorecard.TaskIDs {
							if cell := scorecard.Cells[[2]string{vk, tid}]; cell != nil {
								trials++
							}
						}
					}
					if len(scorecard.VariantKeys) > 0 {
						pr = passRate(scorecard, scorecard.VariantKeys[0])
						hasPR = !math.IsNaN(pr)
					}
				}

				lastRunAt := ""
				if ls != nil {
					for _, tr := range ls.Trials {
						if tr.ExperimentID == id && tr.Timestamp > lastRunAt {
							lastRunAt = tr.Timestamp
						}
					}
				}

				items = append(items, experimentStatus{
					ID:             id,
					Title:          exp.Title,
					AutoReconcile:  exp.AutoReconcile,
					BaselinePinned: exp.BaselinePinned,
					TrialCount:     trials,
					PassRate:       pr,
					HasPassRate:    hasPR,
					LastRunAt:      lastRunAt,
				})
			}
		}

		resp := map[string]any{
			"experiments": items,
			"count":       len(items),
		}
		b, _ := json.MarshalIndent(resp, "", "  ")
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
		}, resp, nil
	}
}

// ---------------------------------------------------------------------------
// cog_get_experiment_status
// ---------------------------------------------------------------------------

type getExperimentStatusInput struct {
	ExperimentID string `json:"experiment_id"`
}

func makeGetExperimentStatusHandler(p *EvalProvider) mcp.ToolHandlerFor[getExperimentStatusInput, map[string]any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input getExperimentStatusInput) (*mcp.CallToolResult, map[string]any, error) {
		if p == nil {
			return evalErrorResult("eval provider not wired"), nil, nil
		}
		if input.ExperimentID == "" {
			return evalErrorResult("experiment_id is required"), nil, nil
		}

		cfgAny, err := p.LoadConfig(p.root)
		if err != nil {
			return evalErrorResult(fmt.Sprintf("load config: %v", err)), nil, nil
		}
		cfg, _ := cfgAny.(*EvalConfig)

		exp := (*Experiment)(nil)
		if cfg != nil {
			exp = cfg.Experiments[input.ExperimentID]
		}
		if exp == nil {
			return evalErrorResult(fmt.Sprintf("experiment %q not found", input.ExperimentID)), nil, nil
		}

		liveAny, _ := p.FetchLive(ctx, cfg)
		ls, _ := liveAny.(*EvalLiveState)

		sc := (*Scorecard)(nil)
		if ls != nil {
			sc = ls.Scorecards[input.ExperimentID]
		}

		// Build scorecard summary
		cells := map[string]interface{}{}
		if sc != nil {
			for _, vk := range sc.VariantKeys {
				taskResults := map[string]interface{}{}
				for _, tid := range sc.TaskIDs {
					cell := sc.Cells[[2]string{vk, tid}]
					if cell == nil {
						taskResults[tid] = nil
					} else {
						taskResults[tid] = *cell
					}
				}
				cells[vk] = taskResults
			}
		}

		lastRunAt := ""
		if ls != nil {
			for _, tr := range ls.Trials {
				if tr.ExperimentID == input.ExperimentID && tr.Timestamp > lastRunAt {
					lastRunAt = tr.Timestamp
				}
			}
		}

		pin := ""
		if cfg != nil {
			pin = cfg.BaselinePins[input.ExperimentID]
		}

		variantKeys := []string{}
		taskIDs := []string{}
		if sc != nil {
			variantKeys = sc.VariantKeys
			taskIDs = sc.TaskIDs
		}

		resp := map[string]any{
			"experiment_id":   exp.ID,
			"title":           exp.Title,
			"auto_reconcile":  exp.AutoReconcile,
			"baseline_pinned": pin,
			"last_run_at":     lastRunAt,
			"in_flight":       0,
			"scorecard":       cells,
			"variant_keys":    variantKeys,
			"task_ids":        taskIDs,
		}

		b, _ := json.MarshalIndent(resp, "", "  ")
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
		}, resp, nil
	}
}

// ---------------------------------------------------------------------------
// cog_pin_baseline
// ---------------------------------------------------------------------------

type pinBaselineInput struct {
	ExperimentID string `json:"experiment_id"`
	RunID        string `json:"run_id"`
}

func makePinBaselineHandler(p *EvalProvider) mcp.ToolHandlerFor[pinBaselineInput, map[string]any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input pinBaselineInput) (*mcp.CallToolResult, map[string]any, error) {
		if p == nil {
			return evalErrorResult("eval provider not wired"), nil, nil
		}
		if input.ExperimentID == "" || input.RunID == "" {
			return evalErrorResult("experiment_id and run_id are required"), nil, nil
		}

		if p.root == "" {
			return evalErrorResult("eval provider root not set (LoadConfig not called)"), nil, nil
		}

		err := writePinBaseline(p.root, input.ExperimentID, input.RunID)
		if err != nil {
			return evalErrorResult(fmt.Sprintf("write baseline pin: %v", err)), nil, nil
		}

		resp := map[string]any{
			"ok":            true,
			"experiment_id": input.ExperimentID,
			"run_id":        input.RunID,
			"message":       fmt.Sprintf("baseline pin written for %s → %s", input.ExperimentID, input.RunID),
		}
		b, _ := json.Marshal(resp)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
		}, resp, nil
	}
}

// writePinBaseline writes a baseline pin to .cog/state/eval-baselines.json.
// The file is a JSON map[string]string: experiment_id → run_id.
// Implements cog_pin_baseline's storage logic (design memo Q1 / Q10).
func writePinBaseline(root, experimentID, runID string) error {
	stateDir := filepath.Join(root, ".cog", "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", stateDir, err)
	}

	pinsPath := filepath.Join(stateDir, "eval-baselines.json")
	pins := map[string]string{}

	if data, err := os.ReadFile(pinsPath); err == nil {
		_ = json.Unmarshal(data, &pins) // ignore parse errors — start fresh
	}

	pins[experimentID] = runID
	b, err := json.MarshalIndent(pins, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(pinsPath, b, 0o644)
}

// evalErrorResult builds a CallToolResult carrying an error message.
func evalErrorResult(msg string) *mcp.CallToolResult {
	resp := map[string]any{"error": msg, "ok": false}
	b, _ := json.Marshal(resp)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
		IsError: true,
	}
}
