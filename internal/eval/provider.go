// Package eval provides the EvalProvider: a Reconcilable that manages the
// eval harness substrate — variant cogdoc loading, matrix expansion, trial
// dispatch, CogBlock emission, and scorecard computation — as part of the
// CogOS kernel's continuous reconciliation loop.
//
// Architectural placement: Phase C of the eval harness substrate plan
// (see cog://mem/semantic/architecture/eval-harness-substrate-plan.cog.md).
//
// EvalProvider implements pkg/reconcile.Reconcilable:
//
//	Declared state → experiment cogdocs at cog://mem/semantic/architecture/tournament/experiments/
//	                + baseline pins at .cog/state/eval-baselines.json
//	Live state     → completed TrialRecords read from bus_tournament channel
//	Plan           → pending runs, stale baselines, regression retries, new variant cells
//	Apply          → dispatch trials via AgentDispatcher; emit CogBlocks via BusEmitter
//
// This file intentionally ships as a DRAFT SKELETON. All Reconcilable method
// bodies return errors.New("TODO") or zero values; types are complete. The
// file is intended to compile (go build ./internal/eval/...) and serve as the
// structural contract before Phase C begins.
//
// Do NOT register this provider in pkg/reconcile/registry.go until Phase C
// is formally shipped. See constraint note at bottom of file.
package eval

import (
	"context"
	"errors"
	"time"

	"github.com/cogos-dev/cogos/pkg/reconcile"
)

// ---------------------------------------------------------------------------
// Dependency interfaces
// ---------------------------------------------------------------------------
//
// These narrow interfaces are what EvalProvider needs from other packages.
// Concrete implementations wire in from main or from the kernel boot path.
// Defining them here lets the package compile standalone without importing
// internal/engine or internal/agents directly — avoiding import cycles.

// AgentDispatcher is the subset of internal/engine.AgentDispatcher that
// EvalProvider calls. Matches AgentDispatcher defined in
// internal/engine/agent_dispatch.go (lines 138-143) exactly — wire the
// concrete LocalHarnessController (or future AgentProvider) here.
//
// TODO(Phase C): import internal/engine.AgentDispatcher directly once
// internal/agents/ is extracted as its own package. Until then, this
// interface is intentionally shape-compatible but locally declared to
// avoid a direct internal/engine import from internal/eval.
type AgentDispatcher interface {
	// DispatchToHarness executes a fan-out batch and returns once all slots
	// complete, error, or time out. See internal/engine.DispatchRequest for
	// the full field contract.
	DispatchToHarness(ctx context.Context, req DispatchRequest) (*DispatchBatchResult, error)
}

// BusEmitter is the subset of internal/bus that EvalProvider calls for
// CogBlock emission. Each trial record and each run summary becomes a
// CogBlock on bus_tournament via EmitCogBlock.
//
// TODO(Phase C): align with the concrete bus.Emitter type once internal/bus/
// is extracted. Shape is intentionally minimal.
type BusEmitter interface {
	// EmitCogBlock emits a serialized CogBlock to the named bus channel.
	// channelName is the raw bus channel id, e.g. "bus_tournament".
	// block is a JSON-serializable payload; the bus layer wraps it in the
	// ADR-084 pointer-envelope (digest → BlobStore, metadata in envelope).
	EmitCogBlock(ctx context.Context, channelName string, block any) error
}

// CogdocReader reads raw cogdoc files from the workspace by cog:// URI or
// absolute path. Used by LoadConfig to enumerate experiment cogdocs and by
// FetchLive's variant loader.
//
// TODO(Phase C): align with workspace.Reader or the resolved path from
// internal/engine.ResolveURI (uri.go). The mem:// projection resolves to
// .cog/mem/ — see uri.go line 51.
type CogdocReader interface {
	// ReadCogdoc reads a single .cog.md file, returning raw bytes.
	// path may be an absolute filesystem path or a cog:// URI.
	ReadCogdoc(path string) ([]byte, error)
	// GlobCogdocs returns all .cog.md file paths under the given cog:// URI
	// prefix or filesystem directory (non-recursive subdirs included).
	GlobCogdocs(prefix string) ([]string, error)
}

// ---------------------------------------------------------------------------
// Dispatch shims (shape-compatible with internal/engine, locally redeclared)
// ---------------------------------------------------------------------------
//
// These types are exact shape copies of types in internal/engine/agent_dispatch.go.
// They exist here so internal/eval compiles without importing internal/engine.
// On Phase C wiring, replace these with direct imports or a shared pkg/types.
//
// TODO(Phase C / internal/agents extraction): move DispatchRequest and siblings
// to a shared pkg/dispatch or pkg/agentapi package so both internal/engine and
// internal/eval can import without cycles.

// DispatchRequest is a shape copy of internal/engine.DispatchRequest
// (agent_dispatch.go lines 50-92).
type DispatchRequest struct {
	AgentID        string
	Task           string
	Tools          []string
	Model          string // matches DispatchModel string type
	TimeoutSeconds int
	N              int
	SystemPrompt   string
	Thinking       *bool
}

// DispatchResult is a shape copy of internal/engine.DispatchResult
// (agent_dispatch.go lines 105-119).
type DispatchResult struct {
	Index       int     `json:"index"`
	Success     bool    `json:"success"`
	Content     string  `json:"content,omitempty"`
	Error       string  `json:"error,omitempty"`
	DurationSec float64 `json:"duration_sec"`
	Turns       int     `json:"turns"`
	ModelUsed   string  `json:"model_used,omitempty"`
}

// DispatchBatchResult is a shape copy of internal/engine.DispatchBatchResult
// (agent_dispatch.go lines 125-132).
type DispatchBatchResult struct {
	Results          []DispatchResult `json:"results"`
	TotalDurationSec float64          `json:"total_duration_sec"`
	Notes            []string         `json:"notes,omitempty"`
}

// ---------------------------------------------------------------------------
// Domain types — ported from Python with Go idioms
// ---------------------------------------------------------------------------
//
// These are the Go ports of:
//   evals/harness/cases.py        — Rubric, Case
//   evals/tournament/variants.py  — Variant
//   evals/tournament/matrix.py    — Experiment, TrialSpec
//   evals/harness/scoring.py      — Verdict
//   evals/tournament/compare.py   — Scorecard, Delta
// plus TrialRecord from evals/reports/data.py and RunSummary.

// VariantClass identifies what a variant overrides.
type VariantClass string

const (
	VariantClassSystemPrompt    VariantClass = "system-prompt"
	VariantClassToolDescription VariantClass = "tool-description"
	VariantClassTask            VariantClass = "task"
	VariantClassExperiment      VariantClass = "experiment"
)

// Variant is a single prompt variant loaded from a .cog.md cogdoc.
// Ports evals/tournament/variants.py Variant dataclass.
type Variant struct {
	// ID is the variant identifier from cogdoc frontmatter or the stem of the file.
	ID string `json:"id"`
	// Class identifies whether this variant overrides the system prompt,
	// tool descriptions, or task configuration.
	Class VariantClass `json:"variant_class"`
	// Content is the variant payload:
	//   - system-prompt:    string (body under "## Variant content")
	//   - tool-description: map[string]any (overrides: dict from frontmatter)
	//   - task:             map[string]any (case: dict from frontmatter)
	Content any `json:"content"`
	// BaselineOf links this variant to its baseline counterpart (e.g. "sp-1-production").
	BaselineOf string `json:"baseline_of,omitempty"`
	// Ablation names the specific feature this variant removes.
	Ablation string `json:"ablation,omitempty"`
	// Tags are arbitrary labels for filtering (e.g. ["tournament", "anti-pattern"]).
	Tags []string `json:"tags,omitempty"`
	// SourcePath is the absolute filesystem path from which this variant was loaded.
	SourcePath string `json:"source_path,omitempty"`
}

// Rubric holds the scoring criteria for a single eval case.
// Ports evals/harness/cases.py Rubric dataclass.
//
// Extension point: Phase C ports this exactly. Weighted scoring and judge
// integration are post-Phase-C additions (see design memo Q8).
type Rubric struct {
	// ExpectedTools are tool names that MUST appear in the tool-call sequence.
	ExpectedTools []string `json:"expected_tools,omitempty"`
	// ExpectedToolsAnyOf requires at least ONE of these names to appear.
	ExpectedToolsAnyOf []string `json:"expected_tools_any_of,omitempty"`
	// ForbiddenTools are tool names that MUST NOT appear.
	ForbiddenTools []string `json:"forbidden_tools,omitempty"`
	// ContentContains are strings that must appear in the assistant's final content.
	ContentContains []string `json:"content_contains,omitempty"`
	// ContentMustNotContain are strings that must NOT appear in final content.
	ContentMustNotContain []string `json:"content_must_not_contain,omitempty"`
	// ContentContainsCI is the case-insensitive variant of ContentContains.
	// Added during Phase C port to close the task-3 gap from exp-001 runs.
	ContentContainsCI []string `json:"content_contains_ci,omitempty"`
	// ContentMustNotContainCI is the case-insensitive variant of ContentMustNotContain.
	ContentMustNotContainCI []string `json:"content_must_not_contain_ci,omitempty"`
	// FirstToolOneOf constrains the first tool call to one of these names.
	FirstToolOneOf []string `json:"first_tool_one_of,omitempty"`
}

// Case is a single eval scenario with a prompt and a scoring rubric.
// Ports evals/harness/cases.py Case dataclass.
type Case struct {
	// Name is the stable identifier for this case, matching the task variant ID.
	Name string `json:"name"`
	// Prompt is the user-turn text sent to the model.
	Prompt string `json:"prompt"`
	// Rubric holds the scoring constraints.
	Rubric Rubric `json:"rubric"`
	// SystemPrompt, if non-empty, overrides the default system prompt for this case.
	// Set from the trial's system-prompt variant before dispatch.
	SystemPrompt string `json:"system_prompt,omitempty"`
	// Tags are arbitrary labels inherited from the task variant.
	Tags []string `json:"tags,omitempty"`
	// MaxTokens is the per-trial token budget. Default 1024.
	MaxTokens int `json:"max_tokens"`
}

// Experiment is the parsed form of an experiment cogdoc.
// Ports evals/tournament/matrix.py Experiment dataclass.
//
// Cogdocs live at cog://mem/semantic/architecture/tournament/experiments/
// — resolved by uri.go projection "mem" → .cog/mem/ (uri.go line 51).
type Experiment struct {
	// ID is the stable identifier, e.g. "exp-001-anti-pattern-placement".
	ID string `json:"id"`
	// Title is the human-readable experiment title from frontmatter.
	Title string `json:"title"`
	// BaselineVariant is the composite key for the baseline cell,
	// e.g. "sp-1-production+td-1-current".
	BaselineVariant string `json:"baseline_variant"`
	// VariantAxes maps axis name → list of variant IDs,
	// e.g. {"system_prompt": ["sp-1-production", "sp-3-stripped"]}.
	VariantAxes map[string][]string `json:"variant_axes"`
	// TaskIDs lists the task variant IDs included in this experiment.
	TaskIDs []string `json:"task_ids"`
	// Target names the dispatch target, e.g. "laptop-lms".
	Target string `json:"target"`
	// Tags are arbitrary labels.
	Tags []string `json:"tags,omitempty"`
	// AutoReconcile, when true, allows the metabolic cycle to run this
	// experiment automatically. Defaults false (on-demand only).
	//
	// TODO(Phase C): wire this from frontmatter key "auto_reconcile: true".
	// When false, the experiment only runs via explicit cog_run_experiment MCP call.
	AutoReconcile bool `json:"auto_reconcile,omitempty"`
	// BaselinePinned is the run ID of the pinned baseline, if any.
	// Set externally via cog_pin_baseline MCP tool (see design memo Q10).
	BaselinePinned string `json:"baseline_pinned,omitempty"`
}

// TrialSpec is a single trial to execute: one variant configuration × one task.
// Ports evals/tournament/matrix.py TrialSpec dataclass.
type TrialSpec struct {
	// TrialID is the stable identifier, e.g.
	// "exp-001__sp-3-stripped+td-1-current__task-1-state-probe".
	TrialID string `json:"trial_id"`
	// ExperimentID links this trial to its parent experiment.
	ExperimentID string `json:"experiment_id"`
	// TaskVariant is the resolved task variant.
	TaskVariant Variant `json:"task_variant"`
	// VariantIDs maps axis → variant ID for non-task axes in this trial.
	VariantIDs map[string]string `json:"variant_ids"`
	// SystemPromptVariant is the resolved system-prompt variant, or empty ID if absent.
	SystemPromptVariant *Variant `json:"system_prompt_variant,omitempty"`
	// ToolDescriptionVariant is the resolved tool-description variant, or nil if Phase 1.
	ToolDescriptionVariant *Variant `json:"tool_description_variant,omitempty"`
	// Target names the dispatch target, inherited from the experiment.
	Target string `json:"target"`
}

// Verdict is the scoring result for a single trial.
// Ports evals/harness/scoring.py Verdict dataclass.
type Verdict struct {
	// Passed is true if all rubric constraints were satisfied.
	Passed bool `json:"passed"`
	// Failures lists each rubric constraint that was not met.
	Failures []string `json:"failures,omitempty"`
	// Notes are informational annotations (e.g. "tool_calls: [cog_read_cogdoc]").
	Notes []string `json:"notes,omitempty"`
}

// TrialRecord is the persisted record of a completed trial.
// Ports evals/reports/data.py TrialRecord dataclass.
// Emitted as a CogBlock on bus_tournament after each trial completes.
type TrialRecord struct {
	// TrialID is the stable identifier for this trial.
	TrialID string `json:"trial_id"`
	// ExperimentID links this record to its parent experiment.
	ExperimentID string `json:"experiment_id"`
	// VariantIDs is the axis → variant mapping for this trial.
	VariantIDs map[string]string `json:"variant_ids"`
	// TaskID is the task variant ID.
	TaskID string `json:"task_id"`
	// Target names the dispatch target.
	Target string `json:"target"`
	// Passed reports whether the trial satisfied its rubric.
	Passed bool `json:"passed"`
	// Failures lists rubric violations, empty on pass.
	Failures []string `json:"failures,omitempty"`
	// Notes are informational annotations from the scorer.
	Notes []string `json:"notes,omitempty"`
	// ToolCalls records each tool invocation made during the trial.
	ToolCalls []ToolCallRecord `json:"tool_calls,omitempty"`
	// Content is the final assistant text response.
	Content string `json:"content,omitempty"`
	// Reasoning is the model's reasoning trace, if available.
	Reasoning string `json:"reasoning,omitempty"`
	// DurationSec is the wall-clock time for this trial.
	DurationSec float64 `json:"duration_sec"`
	// Timestamp is the ISO-8601 start time of this trial.
	Timestamp string `json:"timestamp"`
	// Model is the inference backend used.
	Model string `json:"model,omitempty"`
	// TDWired indicates whether tool-description variant overrides were wired.
	// False in Phase 1 (TD axis not yet wired into dispatch).
	TDWired bool `json:"td_wired"`
	// CogBlockHash is the content-addressed hash of the CogBlock emitted for
	// this trial on bus_tournament. Empty if emission failed.
	CogBlockHash string `json:"cogblock_hash,omitempty"`
}

// ToolCallRecord is the digest of a single tool invocation within a trial.
type ToolCallRecord struct {
	Name      string `json:"name"`
	ArgsDigest string `json:"args_digest,omitempty"`
	ResultDigest string `json:"result_digest,omitempty"`
	Error     string `json:"error,omitempty"`
}

// RunSummary is the aggregate result of an experiment run.
type RunSummary struct {
	// ExperimentID identifies the experiment.
	ExperimentID string `json:"experiment_id"`
	// RunID is the stable identifier for this run, e.g.
	// "exp-001-anti-pattern-placement_run_20260426T010713Z".
	RunID string `json:"run_id"`
	// StartedAt and EndedAt are ISO-8601 timestamps.
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at"`
	// Total, Passed, Failed are trial counts.
	Total  int `json:"total"`
	Passed int `json:"passed"`
	Failed int `json:"failed"`
	// Target names the dispatch target used in this run.
	Target string `json:"target"`
	// Model is the inference backend used.
	Model string `json:"model,omitempty"`
}

// ScorecardCell is the pass/fail aggregate for a (variant_key, task_id) cell.
// nil means no data for this cell.
type ScorecardCell = *bool

// Scorecard is the aggregate pass/fail matrix for an experiment run.
// Ports evals/tournament/compare.py Scorecard dataclass.
type Scorecard struct {
	// ExperimentID identifies the experiment.
	ExperimentID string `json:"experiment_id"`
	// Cells maps (variant_key, task_id) → pass/fail aggregate.
	// variant_key is "sp-id / td-id"; task_id is the task variant id.
	// nil = no data for this cell.
	Cells map[[2]string]ScorecardCell `json:"cells"`
	// VariantKeys is the sorted list of variant keys (for deterministic output).
	VariantKeys []string `json:"variant_keys"`
	// TaskIDs is the sorted list of task IDs.
	TaskIDs []string `json:"task_ids"`
}

// Delta is the pass-rate difference between a variant and its baseline.
// Ports evals/tournament/compare.py Delta dataclass.
type Delta struct {
	// VariantKey and BaselineKey identify the compared variants.
	VariantKey  string `json:"variant_key"`
	BaselineKey string `json:"baseline_key"`
	// Delta is positive for improvement, negative for regression.
	// math.Inf(-1) when variant has no data.
	Delta float64 `json:"delta"`
	// VariantPassRate and BaselinePassRate are the aggregated pass rates.
	// nil when no data is available.
	VariantPassRate  *float64 `json:"variant_pass_rate,omitempty"`
	BaselinePassRate *float64 `json:"baseline_pass_rate,omitempty"`
	// TaskDeltas maps task_id → per-task delta (nil = missing data).
	TaskDeltas map[string]*float64 `json:"task_deltas,omitempty"`
}

// ---------------------------------------------------------------------------
// EvalConfig — the declared state loaded by LoadConfig
// ---------------------------------------------------------------------------

// EvalConfig is the declared configuration for the eval provider.
// Loaded from:
//   - Experiment cogdocs at cog://mem/semantic/architecture/tournament/experiments/
//     (resolved to .cog/mem/semantic/architecture/tournament/experiments/ by uri.go)
//   - Baseline pins from .cog/state/eval-baselines.json
//     (see design memo Q1 for the storage decision rationale)
type EvalConfig struct {
	// Experiments is the set of declared experiments, keyed by experiment ID.
	Experiments map[string]*Experiment `json:"experiments"`
	// BaselinePins maps experiment ID → pinned run ID.
	// Populated from .cog/state/eval-baselines.json (design memo Q1).
	BaselinePins map[string]string `json:"baseline_pins,omitempty"`
	// TournamentRoot is the resolved filesystem path of the tournament cogdoc
	// directory, e.g. /Users/.../cog/.cog/mem/semantic/architecture/tournament.
	// Populated by LoadConfig from workspace root + uri.go "mem" projection.
	TournamentRoot string `json:"tournament_root,omitempty"`
}

// ---------------------------------------------------------------------------
// EvalLiveState — the live state fetched by FetchLive
// ---------------------------------------------------------------------------

// EvalLiveState is the snapshot of completed trials fetched from bus_tournament.
// FetchLive reads recent CogBlock events from bus_tournament, deserializes
// TrialRecord payloads, and builds a per-experiment scorecard.
//
// TODO(Phase C — FetchLive): decide look-back window (all-time vs N-day).
// See design memo Q2 for the recommendation (all-time, re-materialized per
// reconcile cycle, with scorecard computed inline).
type EvalLiveState struct {
	// Trials is the flat list of all completed trial records fetched from the bus.
	Trials []TrialRecord `json:"trials"`
	// Scorecards maps experiment ID → computed scorecard over all fetched trials.
	Scorecards map[string]*Scorecard `json:"scorecards"`
	// FetchedAt is the ISO-8601 timestamp when this snapshot was taken.
	FetchedAt string `json:"fetched_at"`
}

// ---------------------------------------------------------------------------
// EvalPlanAction — the planned operations in ComputePlan
// ---------------------------------------------------------------------------

// EvalActionType identifies the kind of eval action planned.
type EvalActionType string

const (
	// EvalActionRun plans a new experiment run (no prior runs for this experiment).
	EvalActionRun EvalActionType = "run"
	// EvalActionRefreshBaseline plans a baseline refresh (pinned run is stale or missing).
	EvalActionRefreshBaseline EvalActionType = "refresh_baseline"
	// EvalActionRunIncremental plans running only new variant cells since the last run.
	EvalActionRunIncremental EvalActionType = "run_incremental"
	// EvalActionRetryRegression plans a retry of cells that regressed vs the baseline.
	EvalActionRetryRegression EvalActionType = "retry_regression"
	// EvalActionSkip plans no action (experiment is current and healthy).
	EvalActionSkip EvalActionType = "skip"
)

// EvalPlanDetail holds per-action detail for eval plan actions.
// Stored in reconcile.Action.Details as a map[string]any (JSON-serializable).
type EvalPlanDetail struct {
	// ExperimentID identifies which experiment this action targets.
	ExperimentID string `json:"experiment_id"`
	// EvalAction is the specific eval operation.
	EvalAction EvalActionType `json:"eval_action"`
	// TrialSpecs are the specific trials to run for incremental and retry actions.
	// Empty for full-run actions (expand_matrix is called at ApplyPlan time).
	TrialSpecs []TrialSpec `json:"trial_specs,omitempty"`
	// RegressionCells lists (variant_key, task_id) pairs that regressed.
	// Populated for EvalActionRetryRegression.
	RegressionCells [][2]string `json:"regression_cells,omitempty"`
	// StaleAfter is the ISO-8601 time after which the baseline is considered stale.
	StaleAfter string `json:"stale_after,omitempty"`
}

// ---------------------------------------------------------------------------
// EvalProviderState — persisted between reconcile cycles
// ---------------------------------------------------------------------------

// EvalProviderState is the eval-specific metadata persisted inside
// reconcile.State.Metadata["eval_state"]. It bridges cycles so ApplyPlan
// doesn't double-fire in-flight trials and circuit-breakers work.
//
// Stored as a JSON blob in reconcile.State.Metadata (see design memo Q9).
type EvalProviderState struct {
	// InFlightTrialIDs lists trial IDs currently being dispatched.
	// Checked by ComputePlan to avoid re-planning in-flight work.
	InFlightTrialIDs []string `json:"in_flight_trial_ids,omitempty"`
	// RecentFailureCounts maps experiment ID → consecutive failure count.
	// When > CircuitBreakerThreshold, ComputePlan skips that experiment.
	RecentFailureCounts map[string]int `json:"recent_failure_counts,omitempty"`
	// LastReconcileAt is the ISO-8601 time of the last completed reconcile.
	LastReconcileAt string `json:"last_reconcile_at,omitempty"`
	// CircuitBreakerThreshold is the failure count above which an experiment
	// is suspended until manually reset. Default 3.
	CircuitBreakerThreshold int `json:"circuit_breaker_threshold,omitempty"`
}

// ---------------------------------------------------------------------------
// EvalProvider — the Reconcilable
// ---------------------------------------------------------------------------

// EvalProvider implements pkg/reconcile.Reconcilable for the eval harness
// substrate. It is the Go kernel's owner of variant cogdoc loading, matrix
// expansion, trial dispatch, CogBlock emission, and scorecard computation.
//
// Dependency wiring follows the component_provider.go pattern (see
// internal/providers/component/component_provider.go lines 68-81): the main
// package or kernel boot path sets the exported function variables before the
// first reconcile cycle.
//
// NOTE: Do NOT register this provider in pkg/reconcile/registry.go until
// Phase C is formally shipped. The component_provider.go init() call
// (line 91) is the pattern to follow when registration is ready.
type EvalProvider struct {
	// root is the workspace root, set by LoadConfig.
	root string
	// dispatcher handles trial dispatch. Set by the kernel boot path or tests.
	dispatcher AgentDispatcher
	// emitter handles CogBlock emission to bus_tournament. Set by boot path.
	emitter BusEmitter
	// reader reads cogdoc files. Set by boot path.
	reader CogdocReader
	// lastHealth is the cached health status, updated on each reconcile cycle.
	lastHealth reconcile.ResourceStatus
}

// Dependency-injection seam variables. Set by the wiring layer (kernel boot
// or test setup) before any reconcile cycle begins. Pattern mirrors
// component_provider.go lines 68-81.
var (
	// NewEvalProvider constructs a wired EvalProvider. Set by wiring layer.
	// TODO(Phase C wiring): replace with direct constructor call from kernel boot.
	NewEvalProvider func(dispatcher AgentDispatcher, emitter BusEmitter, reader CogdocReader) *EvalProvider
	// NowISO returns the current UTC time in ISO-8601. Set by wiring layer
	// (same sentinel as component_provider.go).
	NowISO func() string
)

// Type returns the resource type identifier. Satisfies reconcile.Reconcilable.
func (e *EvalProvider) Type() string { return "eval" }

// LoadConfig loads the declared eval configuration from the workspace.
//
// What it reads:
//  1. All .cog.md files under the tournament experiments directory
//     (resolved from workspace root as
//     .cog/mem/semantic/architecture/tournament/experiments/ via uri.go
//     "mem" projection — see uri.go line 51).
//  2. Baseline pin state from .cog/state/eval-baselines.json
//     (JSON file with map[experiment_id]run_id — see design memo Q1).
//
// Returns *EvalConfig. Callers must type-assert: config.(*EvalConfig).
//
// TODO(Phase C): implement via CogdocReader.GlobCogdocs +
// CogdocReader.ReadCogdoc; parse YAML frontmatter to populate
// Experiment.VariantAxes, TaskIDs, AutoReconcile, etc.
// Mirror the Python logic in evals/tournament/variants.py load_variants()
// and evals/tournament/matrix.py load_experiment_from_cogdoc().
func (e *EvalProvider) LoadConfig(root string) (any, error) {
	e.root = root
	// TODO(Phase C): enumerate experiment cogdocs via CogdocReader.GlobCogdocs
	// with prefix "<root>/.cog/mem/semantic/architecture/tournament/experiments/".
	// Parse each .cog.md frontmatter into an Experiment struct.
	// Load baseline pins from "<root>/.cog/state/eval-baselines.json" if present.
	return nil, errors.New("TODO: LoadConfig not implemented — Phase C skeleton")
}

// FetchLive fetches the current live state from bus_tournament.
//
// What it reads:
//   - All CogBlock events on bus_tournament (channel carrying tournament.trial.v1
//     and tournament.experiment.v1 blocks, emitted by ApplyPlan via BusEmitter).
//   - Read window: all-time (re-materialized per reconcile cycle). The channel's
//     retention policy governs eviction independently.
//
// The scorecard for each experiment is computed inline from the fetched trials.
// Returns *EvalLiveState. Callers must type-assert: live.(*EvalLiveState).
//
// TODO(Phase C): implement via BusEmitter's read path (or a companion
// BusReader interface for reading vs emitting). Deserialize TrialRecord from
// each event payload. Call buildScorecard() per experiment.
// See evals/tournament/compare.py build_scorecard() for the aggregation logic.
func (e *EvalProvider) FetchLive(ctx context.Context, config any) (any, error) {
	// TODO(Phase C): read bus_tournament events, deserialize TrialRecords,
	// group by experiment_id, build Scorecard per experiment.
	return nil, errors.New("TODO: FetchLive not implemented — Phase C skeleton")
}

// ComputePlan compares declared experiments against completed trial data and
// produces a reconciliation plan.
//
// Planning rules (in priority order):
//  1. No runs for this experiment → EvalActionRun (full matrix expansion at apply time)
//  2. New variant axis cells since last run → EvalActionRunIncremental (only new cells)
//  3. Pinned baseline is stale (> 7 days old, or run_id missing) → EvalActionRefreshBaseline
//  4. Regression detected (pass rate drop > 10pp vs pinned baseline) → EvalActionRetryRegression
//  5. In-flight trials (EvalProviderState.InFlightTrialIDs non-empty) → EvalActionSkip (wait)
//  6. Circuit breaker tripped (RecentFailureCounts[id] > threshold) → EvalActionSkip (suspended)
//  7. AutoReconcile=false and no explicit trigger → EvalActionSkip (on-demand only)
//  8. Experiment is current and healthy → EvalActionSkip (in sync)
//
// Returns a reconcile.Plan with ResourceType "eval". Each reconcile.Action
// carries a Details map with EvalPlanDetail fields serialized as map[string]any.
//
// TODO(Phase C): implement the planning rules listed above. Parse EvalProviderState
// from state.Metadata["eval_state"] using encoding/json. Use the Python
// compare.py regression_check() logic (compare.py lines 174-199) as the
// regression detection reference.
func (e *EvalProvider) ComputePlan(config any, live any, state *reconcile.State) (*reconcile.Plan, error) {
	// TODO(Phase C): implement planning rules.
	// cfg := config.(*EvalConfig)
	// ls := live.(*EvalLiveState)
	// eps := parseEvalProviderState(state)
	// For each experiment in cfg.Experiments:
	//   determine action using priority rules above
	//   append reconcile.Action with EvalPlanDetail in Details
	return nil, errors.New("TODO: ComputePlan not implemented — Phase C skeleton")
}

// ApplyPlan executes planned eval actions.
//
// For each action in plan.Actions:
//   - EvalActionRun: expand full trial matrix, dispatch trials sequentially
//     (Ollama single-thread constraint — see feedback_ollama_single_thread_constraint.md),
//     emit TrialRecord CogBlocks via BusEmitter to bus_tournament after each trial.
//   - EvalActionRunIncremental: dispatch only the TrialSpecs in action.Details.
//   - EvalActionRefreshBaseline: re-run baseline cells, update baseline pin.
//   - EvalActionRetryRegression: re-run regressed cells from action.Details.
//   - EvalActionSkip: emit a reconcile.Result with ApplySkipped, no dispatch.
//
// Trial dispatch granularity: one trial at a time, returning to ComputePlan
// between each (fine-grain — see design memo Q4). This allows interruption and
// drift detection mid-run. ApplyPlan is called once per reconcile cycle; the
// per-cycle budget limits how many trials run before the next cycle checks state.
//
// Concurrency: ApplyPlan must acquire an eval-scoped mutex before dispatching
// to Ollama. The metabolic cycle ticker also uses Ollama (see
// feedback_ollama_single_thread_constraint.md). Recommended: eval reconciliation
// holds a per-cycle budget (e.g. 3 trials/cycle) and releases the mutex between
// trials so the metabolic ticker can run (see design memo Q5).
//
// TODO(Phase C): implement dispatch loop. Use e.dispatcher.DispatchToHarness()
// per trial. Call e.emitter.EmitCogBlock("bus_tournament", record) after each.
// Track in-flight trial IDs in EvalProviderState for the next cycle.
// Mirror the Python runner loop in evals/tournament/runner.py run_experiment()
// (runner.py lines 187-338).
func (e *EvalProvider) ApplyPlan(ctx context.Context, plan *reconcile.Plan) ([]reconcile.Result, error) {
	// TODO(Phase C): implement dispatch loop.
	return nil, errors.New("TODO: ApplyPlan not implemented — Phase C skeleton")
}

// BuildState constructs reconcile state from live trial data (for snapshot/import).
//
// What it stores in the state:
//   - One reconcile.Resource per experiment, carrying:
//     Address: "eval.<experiment_id>"
//     ExternalID: latest run ID (or "" if no runs yet)
//     Attributes: map with trial_count, pass_rate, last_run_at, baseline_pinned
//   - EvalProviderState in state.Metadata["eval_state"] (JSON-encoded).
//
// TODO(Phase C): implement by iterating live.(*EvalLiveState).Scorecards,
// building reconcile.Resource per experiment. Pattern mirrors
// component_provider.go BuildState() (component_provider.go lines 293-334).
func (e *EvalProvider) BuildState(config any, live any, existing *reconcile.State) (*reconcile.State, error) {
	// TODO(Phase C): implement state construction.
	// ls := live.(*EvalLiveState)
	// cfg := config.(*EvalConfig)
	return nil, errors.New("TODO: BuildState not implemented — Phase C skeleton")
}

// Health returns the current three-axis status of the eval subsystem.
//
// Health axes:
//   - Sync: OutOfSync if any AutoReconcile experiments have pending runs;
//     Synced if all declared experiments have recent runs or are on-demand only.
//   - Health: Degraded if any experiment has tripped its circuit breaker;
//     Progressing if any trials are in flight;
//     Healthy otherwise.
//   - Operation: Syncing when trials are in flight; Idle otherwise.
//
// Message carries a summary: "N pending, M in-flight, K suspended".
//
// TODO(Phase C): implement by reading e.lastHealth (updated at end of each
// ApplyPlan call) and optionally doing a lightweight cogdoc scan to check
// for newly declared experiments.
func (e *EvalProvider) Health() reconcile.ResourceStatus {
	// TODO(Phase C): implement health computation.
	// Return cached e.lastHealth if available; otherwise return Unknown.
	return reconcile.ResourceStatus{
		Sync:      reconcile.SyncStatusUnknown,
		Health:    reconcile.HealthMissing,
		Operation: reconcile.OperationIdle,
		Message:   "eval provider not yet wired (Phase C skeleton)",
	}
}

// ---------------------------------------------------------------------------
// Internal helpers (stubs — implement in Phase C)
// ---------------------------------------------------------------------------

// nowISO delegates to the wired NowISO function, falling back to a sentinel.
// Pattern mirrors component_provider.go nowISO() (lines 390-396).
func nowISO() string {
	if NowISO != nil {
		return NowISO()
	}
	return time.Now().UTC().Format(time.RFC3339)
}

// parseEvalProviderState deserializes EvalProviderState from reconcile.State.Metadata.
// Returns a zero-value EvalProviderState if the key is absent or unparseable.
//
// TODO(Phase C): implement using encoding/json.Unmarshal on
// state.Metadata["eval_state"]. The Metadata field is map[string]any, so
// re-serialize the value to JSON first (it may have been round-tripped through
// interface{}).
func parseEvalProviderState(_ *reconcile.State) EvalProviderState {
	return EvalProviderState{
		CircuitBreakerThreshold: 3,
	}
}

// ---------------------------------------------------------------------------
// Registration guard
// ---------------------------------------------------------------------------
//
// DO NOT uncomment the init() below until Phase C is formally shipped and
// all Reconcilable methods are implemented.
//
// When ready, add:
//   func init() {
//       reconcile.RegisterProvider("eval", &EvalProvider{})
//   }
//
// See component_provider.go line 91 for the registration pattern.
