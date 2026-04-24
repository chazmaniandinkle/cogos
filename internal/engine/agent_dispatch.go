// agent_dispatch.go — Phase 2 transport: task-parameterized dispatch into the
// kernel-interior agent harness, with concurrency, structured return, per-call
// scope narrowing, and pluggable model routing.
//
// The cog_dispatch_to_harness MCP tool surfaces this transport. It is the
// foveal -> peripheral handoff: a big external Claude session can offload a
// piece of cognitive work (validation, rewriting, modality matching) onto the
// always-resident Gemma E4B (or LM Studio 26B) without burning Anthropic
// tokens.
//
// This file owns the *contract* — the request and result types and the
// AgentController extension. The concrete dispatcher lives in the root
// package (agent_dispatch.go in main) so it can reach the *AgentHarness
// instance the kernel runs.
//
// Identity propagation note: per the Phase 2 plan, identity claims travel
// through DispatchRequest.Identity as opaque OIDC-shaped fields (iss/sub/aud
// + claims map). Today the controller adapter copies these through to the
// harness as cycle-trace metadata; full CRD-based identity binding waits for
// the Wave 6b migration. The field is wired now so that integration is
// additive — no schema break later.
package engine

import "context"

// DispatchModel selects the inference backend. "e4b" routes to the always-
// resident Ollama (localhost:11434), "26b" routes to LM Studio at the
// configured remote (192.168.10.191:1234) over the OpenAI-compatible API.
// Empty string is normalized to "e4b" by the dispatcher.
type DispatchModel string

const (
	DispatchModelE4B DispatchModel = "e4b"
	DispatchModel26B DispatchModel = "26b"
)

// DispatchIdentity carries OIDC-shaped identity claims propagated from the
// caller. All fields are optional — when absent, the dispatcher records
// "anonymous" in the trace and lets the harness operate under its default
// envelope. Once Wave 6b lands, these claims will be checked against the
// Identity CRD reconciler before tool selection; today they are observability
// metadata only.
type DispatchIdentity struct {
	Iss    string                 `json:"iss,omitempty"`    // issuer (e.g. "anthropic.claude-code")
	Sub    string                 `json:"sub,omitempty"`    // subject (e.g. session id, user handle)
	Aud    string                 `json:"aud,omitempty"`    // audience (e.g. "cogos.kernel")
	Claims map[string]interface{} `json:"claims,omitempty"` // free-form claim bag
}

// DispatchRequest is the normalized input one DispatchToHarness call accepts.
// Validation and clamping happen in the controller adapter; engine callers
// can pass through unchecked since the controller re-normalizes.
type DispatchRequest struct {
	// AgentID names the harness instance. Defaults to "primary" when empty.
	AgentID string

	// Task is the user-role prompt sent into the harness's Execute loop.
	// Required and non-empty.
	Task string

	// Tools is the optional allowlist for this dispatch. nil or empty means
	// "use the harness's default tool registry". Names that don't match any
	// registered tool surface as an error in the result, not silent drop.
	Tools []string

	// Model selects the inference backend. Unknown values default to e4b.
	Model DispatchModel

	// TimeoutSeconds is the per-dispatch wall-clock budget. Clamped to
	// [1,120] by the controller. Default 30s.
	TimeoutSeconds int

	// N controls the parallel fan-out. Clamped to [1,4]. Default 1. Each
	// dispatch in the batch gets its own Index, its own context, its own
	// deadline; failures in one do not abort siblings.
	N int

	// SystemPrompt overrides the harness's default system prompt for this
	// dispatch. Empty string keeps the harness default. Used by the output-
	// alignment layer to swap in role-specific prompts (validator, rewriter,
	// modality-matcher) without persistent config changes.
	SystemPrompt string

	// Thinking optionally overrides the harness's "think" flag. nil keeps
	// the harness default (false today, JSON-mode-friendly). Pass &true to
	// let the model emit a reasoning trace before answering.
	Thinking *bool

	// Identity propagates OIDC-shaped caller claims for trace metadata.
	// Optional; see DispatchIdentity for forward-compat notes.
	Identity DispatchIdentity
}

// DispatchToolCallSummary is the digest of one tool invocation made during a
// dispatch's Execute loop. Args and result are summarized to keep the result
// shape small (full transcripts are still recoverable via the kernel's
// ledger using the dispatch's cycle id).
type DispatchToolCallSummary struct {
	Name         string `json:"name"`
	ArgsDigest   string `json:"args_digest,omitempty"`   // first 200 chars of the JSON args
	ResultDigest string `json:"result_digest,omitempty"` // first 200 chars of the JSON result
	Error        string `json:"error,omitempty"`
}

// DispatchResult is one slot in the batch — the outcome of a single dispatch
// invocation. Index is its position in the batch (0..N-1).
type DispatchResult struct {
	Index       int                       `json:"index"`
	Success     bool                      `json:"success"`
	Content     string                    `json:"content,omitempty"`
	ToolCalls   []DispatchToolCallSummary `json:"tool_calls,omitempty"`
	Error       string                    `json:"error,omitempty"`
	DurationSec float64                   `json:"duration_sec"`
	Turns       int                       `json:"turns"`
	// ModelUsed reports the backend that actually served this slot. May
	// differ from DispatchRequest.Model when "26b" degraded to e4b due to
	// LM Studio being unreachable — Error then carries the warning.
	ModelUsed DispatchModel `json:"model_used,omitempty"`
}

// DispatchBatchResult is the envelope returned to the caller. Results is
// always len(N), filled in dispatch index order, even when some slots
// failed. TotalDurationSec is wall-clock from dispatch start to last slot
// finishing.
type DispatchBatchResult struct {
	Results          []DispatchResult `json:"results"`
	TotalDurationSec float64          `json:"total_duration_sec"`
	// Notes carries batch-level diagnostic strings (e.g. "26b unreachable,
	// degraded to e4b for slots 0,2"). Per-slot warnings live in the
	// individual Error fields.
	Notes []string `json:"notes,omitempty"`
}

// AgentDispatcher is the AgentController extension that surfaces the Phase 2
// transport. The interface lives separate from AgentController so older
// implementations can satisfy the latter without growing the dispatch
// surface. The MCP layer type-asserts to detect availability.
type AgentDispatcher interface {
	// DispatchToHarness runs the request as a fan-out batch and returns
	// once every slot has either completed, errored, or timed out. The
	// returned batch is always non-nil when the error is nil.
	DispatchToHarness(ctx context.Context, req DispatchRequest) (*DispatchBatchResult, error)
}
