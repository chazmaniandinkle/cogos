// mcp_server.go — MCP Streamable HTTP server for CogOS v3
//
// Embeds the MCP server into the existing HTTP server at /mcp.
// Registers 11 MCP tools and 3 MCP resources. Four former tools
// (resolve_uri, get_trust, get_nucleus, get_index) are no longer
// registered as MCP tools but their implementations remain — used
// by the internal tool loop (tool_loop.go).
//
// Resources (read-only addressable data):
//   - cogos://state   — kernel process state
//   - cogos://nucleus — identity context
//   - cogos://field   — attentional field (top-20)
//
// Transport: Streamable HTTP (MCP spec 2025-03-26)
// Endpoint: POST/GET /mcp
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"
)

// MCPServer wraps the MCP server and its dependencies.
type MCPServer struct {
	server          *mcp.Server
	handler         http.Handler
	cfg             *Config
	nucleus         *Nucleus
	process         *Process
	cogdocSvc       *CogDocService
	agentController AgentController // optional; nil when the kernel has no live agent
}

// NewMCPServer creates and configures the MCP server with all stage-1 tools.
// The returned server has no AgentController attached. Call SetAgentController
// to enable cog_list_agents / cog_get_agent_state / cog_trigger_agent_loop.
func NewMCPServer(cfg *Config, nucleus *Nucleus, process *Process) *MCPServer {
	return NewMCPServerWithAgentController(cfg, nucleus, process, nil)
}

// NewMCPServerWithAgentController creates the MCP server and attaches an
// AgentController for the agent-state tools. The controller may be nil;
// the tools remain registered and return a "not configured" response in
// that case so clients get a consistent error shape.
func NewMCPServerWithAgentController(cfg *Config, nucleus *Nucleus, process *Process, ctrl AgentController) *MCPServer {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "cogos-v3",
		Version: BuildTime,
	}, nil)

	m := &MCPServer{
		server:          server,
		cfg:             cfg,
		nucleus:         nucleus,
		process:         process,
		cogdocSvc:       NewCogDocService(cfg, process),
		agentController: ctrl,
	}

	m.registerTools()
	m.registerResources()

	m.handler = mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server { return server },
		nil,
	)

	return m
}

// SetAgentController wires a live AgentController into an already-built
// MCPServer. Safe to call after construction; the tool registration is
// unchanged because the tools resolve the current controller on each call.
func (m *MCPServer) SetAgentController(ctrl AgentController) {
	m.agentController = ctrl
}

// Handler returns the http.Handler for mounting at /mcp.
func (m *MCPServer) Handler() http.Handler {
	return m.handler
}

// registerTools registers MCP tools.
// Design: tools are actions with side effects or non-trivial computation.
// Read-only state queries will migrate to MCP Resources in Phase 2.
//
// Every handler is wrapped with withToolObserver so an invocation emits a
// paired tool.call + tool.result event to the hash-chained ledger. This
// closes Agent F gap #6 and activates the gate.go:94 recognizer that has
// been waiting for a producer.
func (m *MCPServer) registerTools() {
	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_search_memory",
		Description: "Full-text and semantic search over the CogDoc memory corpus. Returns ranked results with salience scores. Fallback: ./scripts/cog memory search \"query\"",
	}, withToolObserver(m, "cog_search_memory", m.toolSearchMemory))

	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_read_cogdoc",
		Description: "Read a CogDoc by URI or path. Resolves cog: URIs automatically. Returns full content with parsed frontmatter and optional section extraction via #fragment. Fallback: ./scripts/cog memory read <path>",
	}, withToolObserver(m, "cog_read_cogdoc", m.toolReadCogdoc))

	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_write_cogdoc",
		Description: "Write or update a CogDoc at the specified memory path. Creates the file with proper frontmatter if it doesn't exist. Fallback: ./scripts/cog memory write <path> \"Title\"",
	}, withToolObserver(m, "cog_write_cogdoc", m.toolWriteCogdoc))

	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_patch_frontmatter",
		Description: "Merge description, tags, or type patches into a CogDoc frontmatter block.",
	}, withToolObserver(m, "cog_patch_frontmatter", m.toolPatchFrontmatter))

	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_check_coherence",
		Description: "Run coherence validation against the workspace. Checks URI resolution, frontmatter validity, and reference integrity. Fallback: ./scripts/cog coherence check",
	}, withToolObserver(m, "cog_check_coherence", m.toolCheckCoherence))

	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_get_state",
		Description: "Get kernel state: process status, uptime, trust, node health (sibling services), field size, and heartbeat info. Includes identity and coherence metadata. Fallback: curl http://localhost:6931/health",
	}, withToolObserver(m, "cog_get_state", m.toolGetState))

	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_query_field",
		Description: "Query the attentional field — the salience-scored map of all tracked CogDocs. Returns top-N items, optionally filtered by sector. Shows what the kernel considers most relevant right now.",
	}, withToolObserver(m, "cog_query_field", m.toolQueryField))

	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_assemble_context",
		Description: "Build a context package for a given token budget with an explicit focus topic. Use this for intentional context assembly (subtasks, specific investigations). The automatic foveated-context hook handles ambient context on every prompt.",
	}, withToolObserver(m, "cog_assemble_context", m.toolAssembleContext))

	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_emit_event",
		Description: "Emit a typed event to the workspace ledger. Events: attention.boost (uri + weight), session.marker (label), insight.captured (summary), decision.made (decision + rationale). Fallback: events are JSONL in .cog/ledger/",
	}, withToolObserver(m, "cog_emit_event", m.toolEmitEvent))

	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_read_ledger",
		Description: "Read the hash-chained event ledger. Filter by session_id, event_type (exact or 'prefix.*' wildcard), after_seq (requires session_id), since_timestamp (RFC3339), or limit (default 100, max 1000). Set verify_chain=true to recompute hashes and validate prior_hash links. Fallback: cat .cog/ledger/<session_id>/events.jsonl",
	}, m.toolReadLedger)

	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_ingest",
		Description: "Ingest external material into CogOS knowledge. Deterministic decomposition — no LLM calls. Supports URLs, conversations, documents. Applies membrane policy (accept/quarantine/defer/discard).",
	}, withToolObserver(m, "cog_ingest", m.toolIngest))

	// Tool-call observability — reads the paired tool.call/tool.result events
	// the wrapper above emits. Self-reflective: these two tools also go
	// through withToolObserver and end up in their own query results.
	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_read_tool_calls",
		Description: "Query recent tool invocations and their outcomes. Returns call+result pairs from the ledger, filterable by tool_name, status (pending/success/error/rejected/timeout), source, ownership, call_id, or time window. Default limit 100, max 500. Arguments and output are opt-in via include_args/include_output. Fallback: grep '\"type\":\"tool\\.' .cog/ledger/<sid>/events.jsonl",
	}, withToolObserver(m, "cog_read_tool_calls", m.toolReadToolCalls))

	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_tail_tool_calls",
		Description: "Tail tool-call events live. Replays recent tool.call / tool.result events (up to max_events, default 50), applying the same filters as cog_read_tool_calls. When Agent N's event bus lands, this will stream new events live; until then it returns a snapshot of the latest matching rows. Fallback: tail -f .cog/ledger/<sid>/events.jsonl | grep '\"type\":\"tool\\.'",
	}, withToolObserver(m, "cog_tail_tool_calls", m.toolTailToolCalls))

	// Agent state / loop control — closes Agent F gap #8 per Agent T's design.
	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_list_agents",
		Description: "Enumerate active agent harness instances inside the kernel. Each entry summarises identity, state, and recent activity. Today returns one element (\"primary\") reflecting the ServeAgent singleton; forward-compatible for future multi-agent deployment. Fallback: curl http://localhost:6931/v1/agents",
	}, m.toolListAgents)

	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_get_agent_state",
		Description: "Full state snapshot of one agent instance — status summary, activity awareness, rolling cycle memory, pending proposals, inbox queue, and optionally the most recent cycle traces. Matches the shape of GET /v1/agents/{id}. Fallback: curl http://localhost:6931/v1/agents/primary",
	}, m.toolGetAgentState)

	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_trigger_agent_loop",
		Description: "Manually invoke one homeostatic cycle of the specified agent, outside the regular ticker. Equivalent to POST /v1/agents/{id}/tick. Returns immediately with a trigger receipt; cycle runs async unless wait=true. Refuses if a cycle is already in flight (overlap guard). Fallback: curl -X POST http://localhost:6931/v1/agents/primary/tick",
	}, m.toolTriggerAgentLoop)

	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_tail_kernel_log",
		Description: "Read recent entries from the kernel's own diagnostic log (slog JSON at .cog/run/kernel.log.jsonl). Returns newest-first, optionally filtered by level, substring, and time range. This is the OPERATOR/DEBUG surface — for hash-chained event history use cog_read_ledger (when available); for client metabolites (turn metrics, attention, proprioceptive) use cog_search_traces. Fallback: tail -n 100 .cog/run/kernel.log.jsonl | jq -c .",
	}, m.toolTailKernelLog)

	mcp.AddTool(m.server, &mcp.Tool{
		Name: "cog_read_conversation",
		Description: "Read conversation turns (prompt + response pairs) from a session's chat history. " +
			"Each turn is a complete user-to-assistant exchange; kernel tool calls are inlined when include_tools=true. " +
			"Backed by the turn.completed ledger event + per-session sidecar (.cog/run/turns/<sid>.jsonl). " +
			"Use after_turn / before_turn for pagination. Default: current process session, 20 turns, ascending. " +
			"Fallback (kernel unavailable): jq -c . .cog/run/turns/<sid>.jsonl",
	}, m.toolReadConversation)

	// Config mutation API (Agent O design — closes Agent F gaps #5 + #19).
	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_read_config",
		Description: "Read the kernel config (.cog/config/kernel.yaml). Returns the effective resolved config (defaults + file overrides). Optional include_raw_yaml returns the raw file bytes; include_defaults also returns the hardcoded defaults for diffing. kernel.yaml only — sibling configs (providers.yaml, secrets.yaml) are out of scope.",
	}, m.toolReadConfig)

	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_write_config",
		Description: "Merge a patch into the kernel config (.cog/config/kernel.yaml) using RFC 7396 JSON merge-patch semantics: fields omitted from the patch are left unchanged; explicit null removes a field and restores the default on next boot. Validated before persisting — returns violations without writing on failure. Atomic write + rotating .bak-<timestamp> backups (keeps 10). Takes effect on next daemon restart (requires_restart: true in response). Fallback: edit .cog/config/kernel.yaml and run `./scripts/cog restart`. No authentication — the kernel assumes a trusted local caller.",
	}, m.toolWriteConfig)

	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_rollback_config",
		Description: "Restore kernel.yaml from a prior .bak-<timestamp> backup. Pass list_only=true to enumerate available backups without restoring. If backup is empty, the most recent backup is used. Atomic restore; response carries updated backup list.",
	}, m.toolRollbackConfig)

	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_search_traces",
		Description: "Search kernel trace JSONL streams in .cog/run/ (turn_metrics, attention, proprioceptive, internal_requests). Filter by source, session_id, level, case-insensitive substring, and time range (since/until accept RFC3339 or duration like 5m/1h). Returns unified chronological results with per-source scan diagnostics. Fallback: ls .cog/run/*.jsonl && jq -c . .cog/run/<name>.jsonl | head",
	}, m.toolSearchTraces)
}

// registerResources registers MCP Resources — read-only addressable data.
// Unlike tools (actions with side effects), resources expose live kernel state
// that clients can read without triggering mutations.
func (m *MCPServer) registerResources() {
	m.server.AddResource(&mcp.Resource{
		URI:         "cogos://state",
		Name:        "Kernel State",
		Description: "Process state, uptime, trust, field size, and node health",
		MIMEType:    "application/json",
	}, m.resourceState)

	m.server.AddResource(&mcp.Resource{
		URI:         "cogos://nucleus",
		Name:        "Identity",
		Description: "Kernel identity context — name, role, summary",
		MIMEType:    "application/json",
	}, m.resourceNucleus)

	m.server.AddResource(&mcp.Resource{
		URI:         "cogos://field",
		Name:        "Attentional Field",
		Description: "Top-20 salience-scored CogDocs with cog:// URIs",
		MIMEType:    "application/json",
	}, m.resourceField)

	m.server.AddResource(&mcp.Resource{
		URI:         "cogos://config",
		Name:        "Kernel Config",
		Description: "Effective kernel configuration (kernel.yaml resolved against defaults)",
		MIMEType:    "application/json",
	}, m.resourceConfig)
}

// ── Resource Handlers ───────────────────────────────────────────────────────

func (m *MCPServer) resourceState(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	if m.process == nil {
		return nil, fmt.Errorf("process not initialized")
	}

	queue := ReadIngestionQueueState(m.cfg.WorkspaceRoot)
	trust := m.process.TrustSnapshot()
	lastHeartbeat := ""
	if !trust.LastHeartbeatAt.IsZero() {
		lastHeartbeat = trust.LastHeartbeatAt.Format(time.RFC3339)
	}

	identity := ""
	if m.nucleus != nil {
		identity = m.nucleus.Name
	}

	result := map[string]any{
		"state":             m.process.State().String(),
		"identity":          identity,
		"session_id":        m.process.SessionID(),
		"node_id":           m.process.NodeID,
		"uptime_seconds":    int(time.Since(m.process.StartedAt()).Seconds()),
		"field_size":        m.process.Field().Len(),
		"trust_score":       trust.LocalScore,
		"fingerprint":       m.process.Fingerprint(),
		"last_heartbeat":    lastHeartbeat,
		"coherence_state":   trust.CoherenceFingerprint,
		"quarantined_count": queue.Quarantined,
		"deferred_count":    queue.Deferred,
	}

	if nh := m.process.NodeHealth(); nh != nil {
		if summary := nh.Summary(); len(summary) > 0 {
			result["node"] = summary
		}
	}

	b, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshal state: %w", err)
	}
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{{
			URI:      req.Params.URI,
			MIMEType: "application/json",
			Text:     string(b),
		}},
	}, nil
}

func (m *MCPServer) resourceNucleus(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	if m.nucleus == nil {
		return nil, fmt.Errorf("nucleus not loaded")
	}

	result := map[string]any{
		"name":      m.nucleus.Name,
		"role":      m.nucleus.Role,
		"summary":   m.nucleus.Summary(),
		"workspace": m.cfg.WorkspaceRoot,
		"port":      m.cfg.Port,
		"build":     BuildTime,
	}

	b, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshal nucleus: %w", err)
	}
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{{
			URI:      req.Params.URI,
			MIMEType: "application/json",
			Text:     string(b),
		}},
	}, nil
}

func (m *MCPServer) resourceField(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	if m.process == nil || m.process.field == nil {
		return nil, fmt.Errorf("attentional field not initialized")
	}

	const limit = 20
	scores := m.process.field.AllScores()

	type entry struct {
		URI      string  `json:"uri"`
		Salience float64 `json:"salience"`
	}
	var entries []entry
	for absPath, score := range scores {
		uri := FieldKeyToURI(m.cfg.WorkspaceRoot, absPath)
		entries = append(entries, entry{URI: uri, Salience: score})
	}
	// Sort by salience descending.
	for i := 0; i < len(entries); i++ {
		for j := i + 1; j < len(entries); j++ {
			if entries[j].Salience > entries[i].Salience {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}
	if len(entries) > limit {
		entries = entries[:limit]
	}

	result := map[string]any{
		"count":   len(entries),
		"entries": entries,
	}

	b, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshal field: %w", err)
	}
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{{
			URI:      req.Params.URI,
			MIMEType: "application/json",
			Text:     string(b),
		}},
	}, nil
}

// ── Tool Inputs ──────────────────────────────────────────────────────────────

// resolveURIInput — no longer an MCP tool; used by the internal tool loop (tool_loop.go).
type resolveURIInput struct {
	URI string `json:"uri" jsonschema:"A cog: URI to resolve. Examples: cog:mem/semantic/architecture/x or cog://cog-workspace/adr/059"`
}

type queryFieldInput struct {
	Sector string `json:"sector,omitempty" jsonschema:"Filter by memory sector (semantic/episodic/procedural/reflective). Empty for all."`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum number of results (default 20)"`
}

type assembleContextInput struct {
	Budget int    `json:"budget" jsonschema:"Token budget for the assembled context"`
	Focus  string `json:"focus,omitempty" jsonschema:"Optional focus topic to bias context selection"`
}

type checkCoherenceInput struct {
	Scope string `json:"scope,omitempty" jsonschema:"Scope of coherence check: structural (default)/navigational/canonical"`
}

type getStateInput struct {
	Verbose bool `json:"verbose,omitempty" jsonschema:"Include detailed field and process info"`
}

// getTrustInput — no longer an MCP tool; used by the internal tool loop (tool_loop.go).
type getTrustInput struct{}

type searchMemoryInput struct {
	Query  string `json:"query" jsonschema:"Search query string"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum results (default 10)"`
	Sector string `json:"sector,omitempty" jsonschema:"Filter by memory sector"`
}

// getNucleusInput — no longer an MCP tool; used by the internal tool loop (tool_loop.go).
// Logic retained for C1 migration to MCP Resource.
type getNucleusInput struct {
	IncludeConfig bool `json:"include_config,omitempty" jsonschema:"Include workspace configuration details"`
}

type readCogdocInput struct {
	URI     string `json:"uri" jsonschema:"A cog: URI pointing to the CogDoc"`
	Section string `json:"section,omitempty" jsonschema:"Optional section name to extract (from #fragment)"`
}

type cogdocFrontmatterPatch struct {
	Description string   `json:"description,omitempty" jsonschema:"One-line summary for the CogDoc" yaml:"description,omitempty"`
	Tags        []string `json:"tags,omitempty" jsonschema:"Classification tags" yaml:"tags,omitempty"`
	Type        string   `json:"type,omitempty" jsonschema:"CogDoc type" yaml:"type,omitempty"`
}

type patchFrontmatterInput struct {
	URI     string                 `json:"uri" jsonschema:"A cog: URI pointing to the CogDoc"`
	Patches cogdocFrontmatterPatch `json:"patches" jsonschema:"Frontmatter fields to merge into the CogDoc"`
}

type writeCogdocInput struct {
	Path    string   `json:"path" jsonschema:"Memory-relative path (e.g. semantic/insights/topic.md)"`
	Title   string   `json:"title" jsonschema:"Document title for frontmatter"`
	Content string   `json:"content" jsonschema:"Markdown content to write"`
	Tags    []string `json:"tags,omitempty" jsonschema:"Optional tags for classification"`
	Status  string   `json:"status,omitempty" jsonschema:"Document status (active/raw/enriched/integrated)"`
	DocType string   `json:"type,omitempty" jsonschema:"Document type (insight/link/conversation/architecture/guide)"`
}

type readCogdocResult struct {
	URI              string            `json:"uri"`
	Path             string            `json:"path"`
	Fragment         string            `json:"fragment,omitempty"`
	Frontmatter      cogdocFrontmatter `json:"frontmatter,omitempty"`
	Content          string            `json:"content"`
	SchemaIssues     []string          `json:"schema_issues,omitempty"`
	PatchFrontmatter map[string]any    `json:"patch_frontmatter,omitempty"`
	SchemaHint       string            `json:"schema_hint,omitempty"`
}

type emitEventInput struct {
	Type    string         `json:"type" jsonschema:"Event type: attention.boost, session.marker, insight.captured, decision.made"`
	Payload map[string]any `json:"payload,omitempty" jsonschema:"Event payload. attention.boost: {uri, weight}. session.marker: {label}. insight.captured: {summary, tags}. decision.made: {decision, rationale}."`
}

type readLedgerInput struct {
	SessionID      string `json:"session_id,omitempty" jsonschema:"Filter to a single session; empty reads across all non-genesis sessions"`
	EventType      string `json:"event_type,omitempty" jsonschema:"Exact event type, or a prefix wildcard like 'attention.*'"`
	AfterSeq       int64  `json:"after_seq,omitempty" jsonschema:"Return events with seq greater than this. Requires session_id (seq is not monotonic across sessions)."`
	SinceTimestamp string `json:"since_timestamp,omitempty" jsonschema:"RFC3339 timestamp; return events with timestamp >= this"`
	Limit          int    `json:"limit,omitempty" jsonschema:"Maximum events to return. Default 100, capped at 1000."`
	VerifyChain    bool   `json:"verify_chain,omitempty" jsonschema:"Recompute hashes and validate prior_hash links on returned events. Off by default (chain walk is O(N))."`
}

// getIndexInput — no longer an MCP tool; used by the internal tool loop (tool_loop.go).
type getIndexInput struct {
	Sector string `json:"sector,omitempty" jsonschema:"Filter by memory sector"`
}

type ingestInput struct {
	Source   string            `json:"source" jsonschema:"Data source: discord, chatgpt, claude, gemini, url, file"`
	Format   string            `json:"format" jsonschema:"Input format: url, conversation, message, document"`
	Data     string            `json:"data" jsonschema:"Raw material to ingest (URL, text, JSON)"`
	Metadata map[string]string `json:"metadata,omitempty" jsonschema:"Optional context (discord_message_id, channel, etc.)"`
}

type tailKernelLogInput struct {
	Limit     int    `json:"limit,omitempty" jsonschema:"Maximum entries to return (default 100, max 1000)"`
	Level     string `json:"level,omitempty" jsonschema:"Filter by exact level (case-insensitive): debug|info|warn|error"`
	Substring string `json:"substring,omitempty" jsonschema:"Case-insensitive substring filter applied to the raw JSON line. Max 1024 chars."`
	Since     string `json:"since,omitempty" jsonschema:"Lower time bound. RFC3339 OR duration like '5m', '2h', '24h'."`
	Until     string `json:"until,omitempty" jsonschema:"Upper time bound. RFC3339 OR duration."`
}

type listAgentsInput struct {
	IncludeStopped bool `json:"include_stopped,omitempty" jsonschema:"Include agents that have stopped (default false). Reserved for future multi-agent pool managers."`
}

type getAgentStateInput struct {
	AgentID      string `json:"agent_id,omitempty" jsonschema:"Which agent to inspect. Default \"primary\" (the ServeAgent singleton)."`
	IncludeTrace bool   `json:"include_trace,omitempty" jsonschema:"Attach up to trace_limit most-recent full cycle traces (observation + result)."`
	TraceLimit   int    `json:"trace_limit,omitempty" jsonschema:"If include_trace, how many recent traces to include. Range [1, 20]. Default 1."`
}

type triggerAgentLoopInput struct {
	AgentID string `json:"agent_id,omitempty" jsonschema:"Which agent to trigger. Default \"primary\"."`
	Reason  string `json:"reason,omitempty" jsonschema:"Free-text tag stored on a synthetic agent.wake event for audit (optional)."`
	Wait    bool   `json:"wait,omitempty" jsonschema:"If true, block until the cycle completes (up to 90s) and return the outcome. Default false (fire-and-forget)."`
}

// readToolCallsInput mirrors ToolCallQuery for the MCP surface. Time bounds
// accept RFC3339 strings; relative shorthand ("5m", "1h", "24h") is supported.
type readToolCallsInput struct {
	SessionID     string `json:"session_id,omitempty" jsonschema:"Filter to a session; empty = all sessions"`
	ToolName      string `json:"tool_name,omitempty" jsonschema:"Exact match or wildcard (e.g. cog_read_*)"`
	Status        string `json:"status,omitempty" jsonschema:"pending | success | error | rejected | timeout"`
	Source        string `json:"source,omitempty" jsonschema:"mcp | openai-chat | anthropic-messages | kernel-loop"`
	Ownership     string `json:"ownership,omitempty" jsonschema:"kernel | client"`
	CallID        string `json:"call_id,omitempty" jsonschema:"Exact single-call lookup"`
	Since         string `json:"since,omitempty" jsonschema:"Lower bound — RFC3339 timestamp or relative duration (e.g. 5m, 1h)"`
	Until         string `json:"until,omitempty" jsonschema:"Upper bound — RFC3339 timestamp"`
	Limit         int    `json:"limit,omitempty" jsonschema:"Default 100, max 500"`
	Order         string `json:"order,omitempty" jsonschema:"desc (default) | asc"`
	IncludeArgs   bool   `json:"include_args,omitempty" jsonschema:"Include arguments payload (default false — PII control)"`
	IncludeOutput bool   `json:"include_output,omitempty" jsonschema:"Include output summary (default false — PII control)"`
}

// tailToolCallsInput is a snapshot-mode version of the live tail. Inherits
// all of readToolCallsInput's filters; defaults include_args/include_output
// to true (callers tailing are actively observing) and caps the returned set
// with max_events + max_duration.
type tailToolCallsInput struct {
	SessionID   string `json:"session_id,omitempty" jsonschema:"Filter to a session; empty = all sessions"`
	ToolName    string `json:"tool_name,omitempty" jsonschema:"Exact match or wildcard"`
	Status      string `json:"status,omitempty" jsonschema:"pending | success | error | rejected | timeout"`
	Source      string `json:"source,omitempty" jsonschema:"Source taxonomy filter"`
	Ownership   string `json:"ownership,omitempty" jsonschema:"kernel | client"`
	CallID      string `json:"call_id,omitempty" jsonschema:"Exact single-call lookup"`
	Since       string `json:"since,omitempty" jsonschema:"Lower bound — RFC3339 or relative"`
	MaxEvents   int    `json:"max_events,omitempty" jsonschema:"Stop after N matching events (default 50, max 500)"`
	MaxDuration string `json:"max_duration,omitempty" jsonschema:"Hard cap on wall-clock (default 60s, max 10m)"`
}

// readConversationInput drives cog_read_conversation — see Agent R §5.2.
type readConversationInput struct {
	SessionID    string `json:"session_id,omitempty" jsonschema:"Session to read. Empty = current process session."`
	AfterTurn    int    `json:"after_turn,omitempty" jsonschema:"Pagination: return turns with turn_index > this."`
	BeforeTurn   int    `json:"before_turn,omitempty" jsonschema:"Reverse pagination: turn_index < this."`
	Since        string `json:"since,omitempty" jsonschema:"RFC3339 lower bound on turn timestamp."`
	Limit        int    `json:"limit,omitempty" jsonschema:"Max turns (default 20, max 200)."`
	IncludeFull  *bool  `json:"include_full,omitempty" jsonschema:"Hydrate prompt/response from the sidecar (default true)."`
	IncludeTools *bool  `json:"include_tools,omitempty" jsonschema:"Include kernel tool-call transcript (default true)."`
	Order        string `json:"order,omitempty" jsonschema:"asc (default, natural reading order) or desc."`
}

type searchTracesInput struct {
	Source    string `json:"source,omitempty" jsonschema:"Trace source filter. One of: turn_metrics, attention, proprioceptive, internal_requests, all (default)."`
	Level     string `json:"level,omitempty" jsonschema:"Level-like filter (exact match, case-insensitive). For proprioceptive source this matches the event field."`
	SessionID string `json:"session_id,omitempty" jsonschema:"Filter to rows whose session_id matches. Only meaningful for sources that carry a session_id (turn_metrics, internal_requests)."`
	Substring string `json:"substring,omitempty" jsonschema:"Case-insensitive substring match against the raw JSONL line. Capped at 1024 characters."`
	Since     string `json:"since,omitempty" jsonschema:"Lower time bound. RFC3339 timestamp or Go duration (e.g. 5m, 1h, 24h for 'since N ago')."`
	Until     string `json:"until,omitempty" jsonschema:"Upper time bound. RFC3339 timestamp or Go duration."`
	Limit     int    `json:"limit,omitempty" jsonschema:"Maximum results (default 100, max 1000)."`
	Order     string `json:"order,omitempty" jsonschema:"'desc' (default, newest first) or 'asc'."`
}

// ── Tool Implementations ─────────────────────────────────────────────────────

// toolResolveURI — no longer registered as an MCP tool; used by the internal tool loop (tool_loop.go).
func (m *MCPServer) toolResolveURI(ctx context.Context, req *mcp.CallToolRequest, input resolveURIInput) (*mcp.CallToolResult, any, error) {
	// Try v2 registry first (multi-scheme)
	if URIRegistry != nil {
		content, err := URIRegistry.Resolve(ctx, input.URI)
		if err == nil {
			result := map[string]any{
				"uri":      input.URI,
				"resolved": true,
				"metadata": content.Metadata,
			}
			if path, ok := content.Metadata["path"]; ok {
				result["path"] = path
				if _, statErr := os.Stat(path.(string)); statErr == nil {
					result["exists"] = true
				} else {
					result["exists"] = false
				}
			}
			return marshalResult(result)
		}
	}

	// Fallback to legacy resolver
	res, err := ResolveURI(m.cfg.WorkspaceRoot, input.URI)
	if err != nil {
		return marshalResult(map[string]any{
			"uri":      input.URI,
			"resolved": false,
			"error":    err.Error(),
		})
	}
	_, statErr := os.Stat(res.Path)
	return marshalResult(map[string]any{
		"uri":      input.URI,
		"resolved": true,
		"path":     res.Path,
		"fragment": res.Fragment,
		"exists":   statErr == nil,
	})
}

func (m *MCPServer) toolQueryField(ctx context.Context, req *mcp.CallToolRequest, input queryFieldInput) (*mcp.CallToolResult, any, error) {
	if m.process == nil || m.process.field == nil {
		return textResult("attentional field not initialized")
	}

	limit := input.Limit
	if limit <= 0 {
		limit = 20
	}

	scores := m.process.field.AllScores()

	type entry struct {
		URI      string  `json:"uri"`
		Salience float64 `json:"salience"`
	}
	var entries []entry
	for absPath, score := range scores {
		if input.Sector != "" && !strings.Contains(absPath, input.Sector) {
			continue
		}
		// Project field key (abs path) to canonical URI.
		uri := FieldKeyToURI(m.cfg.WorkspaceRoot, absPath)
		entries = append(entries, entry{URI: uri, Salience: score})
	}
	// Sort by salience descending
	for i := 0; i < len(entries); i++ {
		for j := i + 1; j < len(entries); j++ {
			if entries[j].Salience > entries[i].Salience {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}
	if len(entries) > limit {
		entries = entries[:limit]
	}
	return marshalResult(map[string]any{
		"count":   len(entries),
		"entries": entries,
	})
}

func (m *MCPServer) toolAssembleContext(ctx context.Context, req *mcp.CallToolRequest, input assembleContextInput) (*mcp.CallToolResult, any, error) {
	if m.process == nil {
		return textResult("process not initialized")
	}

	budget := input.Budget
	if budget <= 0 {
		budget = 50000
	}

	// Use the existing context assembly pipeline
	assembled, err := m.process.AssembleContext(input.Focus, nil, budget, WithManifestMode(true))
	if err != nil {
		return textResult(fmt.Sprintf("context assembly failed: %v", err))
	}

	return marshalResult(assembled)
}

func (m *MCPServer) toolCheckCoherence(ctx context.Context, req *mcp.CallToolRequest, input checkCoherenceInput) (*mcp.CallToolResult, any, error) {
	report, err := CheckCoherenceMCP(m.cfg, m.nucleus)
	if err != nil {
		return fallbackResult(fmt.Sprintf("coherence check failed: %v", err),
			"./scripts/cog coherence check")
	}
	return marshalResult(report)
}

func (m *MCPServer) toolGetState(ctx context.Context, req *mcp.CallToolRequest, input getStateInput) (*mcp.CallToolResult, any, error) {
	if m.process == nil {
		return fallbackResult("process not initialized", "curl http://localhost:6931/health")
	}
	queue := ReadIngestionQueueState(m.cfg.WorkspaceRoot)
	trust := m.process.TrustSnapshot()
	lastHeartbeat := ""
	if !trust.LastHeartbeatAt.IsZero() {
		lastHeartbeat = trust.LastHeartbeatAt.Format(time.RFC3339)
	}

	// Identity (nucleus)
	identity := ""
	if m.nucleus != nil {
		identity = m.nucleus.Name
	}

	result := map[string]any{
		"state":             m.process.State().String(),
		"identity":          identity,
		"session_id":        m.process.SessionID(),
		"node_id":           m.process.NodeID,
		"uptime_seconds":    int(time.Since(m.process.StartedAt()).Seconds()),
		"field_size":        m.process.Field().Len(),
		"trust_score":       trust.LocalScore,
		"fingerprint":       m.process.Fingerprint(),
		"last_heartbeat":    lastHeartbeat,
		"coherence_state":   trust.CoherenceFingerprint,
		"quarantined_count": queue.Quarantined,
		"deferred_count":    queue.Deferred,
	}

	// Node health — sibling services probed on heartbeat.
	if nh := m.process.NodeHealth(); nh != nil {
		if summary := nh.Summary(); len(summary) > 0 {
			result["node"] = summary
		}
	}

	if input.Verbose {
		result["workspace"] = m.cfg.WorkspaceRoot
		result["started_at"] = m.process.StartedAt().Format(time.RFC3339)
		result["last_heartbeat_hash"] = trust.LastHeartbeatHash
		result["last_quarantine"] = queue.LastQuarantineRFC3339
	}
	return marshalResult(result)
}

// toolGetTrust — no longer registered as an MCP tool; used by the internal tool loop (tool_loop.go).
func (m *MCPServer) toolGetTrust(ctx context.Context, req *mcp.CallToolRequest, input getTrustInput) (*mcp.CallToolResult, any, error) {
	if m.process == nil {
		return textResult("process not initialized")
	}
	trust := m.process.TrustSnapshot()
	lastHeartbeat := ""
	if !trust.LastHeartbeatAt.IsZero() {
		lastHeartbeat = trust.LastHeartbeatAt.Format(time.RFC3339)
	}
	return marshalResult(map[string]any{
		"node_id":         m.process.NodeID,
		"trust_score":     trust.LocalScore,
		"fingerprint":     m.process.Fingerprint(),
		"last_heartbeat":  lastHeartbeat,
		"coherence_state": trust.CoherenceFingerprint,
	})
}

func (m *MCPServer) toolSearchMemory(ctx context.Context, req *mcp.CallToolRequest, input searchMemoryInput) (*mcp.CallToolResult, any, error) {
	limit := input.Limit
	if limit <= 0 {
		limit = 10
	}

	results, err := SearchMemory(m.cfg.WorkspaceRoot, input.Query, limit, input.Sector)
	if err != nil {
		return fallbackResult(fmt.Sprintf("search failed: %v", err),
			fmt.Sprintf("./scripts/cog memory search %q", input.Query))
	}
	return marshalResult(results)
}

// toolGetNucleus — no longer registered as an MCP tool; used by the internal tool loop (tool_loop.go).
// Logic retained for C1 migration to MCP Resource.
func (m *MCPServer) toolGetNucleus(ctx context.Context, req *mcp.CallToolRequest, input getNucleusInput) (*mcp.CallToolResult, any, error) {
	if m.nucleus == nil {
		return textResult("nucleus not loaded")
	}
	return marshalResult(map[string]any{
		"name":      m.nucleus.Name,
		"role":      m.nucleus.Role,
		"summary":   m.nucleus.Summary(),
		"workspace": m.cfg.WorkspaceRoot,
		"port":      m.cfg.Port,
		"build":     BuildTime,
	})
}

func (m *MCPServer) toolReadCogdoc(ctx context.Context, req *mcp.CallToolRequest, input readCogdocInput) (*mcp.CallToolResult, any, error) {
	uri := input.URI
	if input.Section != "" && !strings.Contains(uri, "#") {
		uri += "#" + input.Section
	}

	res, err := ResolveURI(m.cfg.WorkspaceRoot, uri)
	if err != nil {
		return textResult(fmt.Sprintf("resolve failed: %v", err))
	}

	data, err := os.ReadFile(res.Path)
	if err != nil {
		return textResult(fmt.Sprintf("read failed: %v", err))
	}

	content := string(data)
	fm, _ := parseCogdocFrontmatter(content)
	issues := missingSchemaIssues(content)
	patchTemplate := patchTemplateForIssues(issues)
	result := readCogdocResult{
		URI:              uri,
		Path:             res.Path,
		Frontmatter:      fm,
		Content:          content,
		SchemaIssues:     issues,
		PatchFrontmatter: patchTemplate,
	}
	if hasSchemaIssue(issues, "missing_description") {
		result.SchemaHint = fmt.Sprintf("This CogDoc is missing a description field. If you can summarize it in one sentence, include it in your next response as: COGDOC_PATCH: %s | description: your summary here", uri)
	}

	// If fragment specified, extract section
	if res.Fragment != "" {
		section := extractSection(content, res.Fragment)
		if section != "" {
			result.Fragment = res.Fragment
			result.Content = section
			return marshalResult(result)
		}
	}

	return marshalResult(result)
}

func (m *MCPServer) toolPatchFrontmatter(ctx context.Context, req *mcp.CallToolRequest, input patchFrontmatterInput) (*mcp.CallToolResult, any, error) {
	if input.URI == "" {
		return textResult("uri is required")
	}
	if input.Patches.empty() {
		return textResult("at least one frontmatter patch is required")
	}

	result, err := m.cogdocSvc.PatchAndSync(input.URI, input.Patches)
	if err != nil {
		return textResult(fmt.Sprintf("patch failed: %v", err))
	}

	// Read back the patched frontmatter for the response.
	var fm cogdocFrontmatter
	if data, readErr := os.ReadFile(result.Path); readErr == nil {
		fm, _ = parseCogdocFrontmatter(string(data))
	}

	return marshalResult(map[string]any{
		"updated":     true,
		"uri":         result.URI,
		"path":        result.Path,
		"frontmatter": fm,
	})
}

func (m *MCPServer) toolWriteCogdoc(ctx context.Context, req *mcp.CallToolRequest, input writeCogdocInput) (*mcp.CallToolResult, any, error) {
	if input.Path == "" || input.Content == "" {
		return textResult("path and content are required")
	}

	opts := CogDocWriteOpts{
		Title:   input.Title,
		Content: input.Content,
		Tags:    input.Tags,
		Status:  input.Status,
		DocType: input.DocType,
	}

	result, err := m.cogdocSvc.WriteAndSync(input.Path, opts)
	if err != nil {
		return textResult(fmt.Sprintf("write failed: %v", err))
	}

	return marshalResult(map[string]any{
		"written": true,
		"path":    result.Path,
		"uri":     result.URI,
	})
}

// CogDocWriteOpts holds options for writing a CogDoc via the internal API.
type CogDocWriteOpts struct {
	Title    string
	Content  string
	Tags     []string
	Status   string            // default "active"
	DocType  string            // e.g. "link", "conversation", "insight"
	Source   string            // e.g. "discord", "chatgpt"
	URL      string            // optional URL field
	SourceID string            // dedup key
	Extra    map[string]string // additional frontmatter fields
}

// detectSector extracts the memory sector from a memory-relative path.
// e.g. "semantic/insights/foo.md" -> "semantic"
func detectSector(path string) string {
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 0 {
		return "semantic"
	}
	switch parts[0] {
	case "semantic", "episodic", "procedural", "reflective":
		return parts[0]
	default:
		return "semantic"
	}
}

// slugFromPath generates a slug-based id from a memory-relative path.
// e.g. "semantic/insights/my-topic.cog.md" -> "my-topic"
func slugFromPath(path string) string {
	base := filepath.Base(path)
	// Strip known extensions
	base = strings.TrimSuffix(base, ".cog.md")
	base = strings.TrimSuffix(base, ".md")
	// Slugify: lowercase, replace non-alnum with hyphens, collapse
	slug := strings.ToLower(base)
	re := regexp.MustCompile(`[^a-z0-9]+`)
	slug = re.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-")
	return slug
}

// WriteCogDoc writes a CogDoc to the memory filesystem with proper frontmatter.
// This is the internal API used by the ingestion pipeline.
func WriteCogDoc(workspaceRoot string, path string, opts CogDocWriteOpts) (string, error) {
	fullPath := filepath.Join(workspaceRoot, ".cog", "mem", path)

	// Ensure directory exists
	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("mkdir failed: %w", err)
	}

	sector := detectSector(path)
	docID := slugFromPath(path)
	now := time.Now().UTC().Format(time.RFC3339)

	status := opts.Status
	if status == "" {
		status = "active"
	}

	// Build YAML frontmatter
	var sb strings.Builder
	sb.WriteString("---\n")
	if docID != "" {
		sb.WriteString(fmt.Sprintf("id: %s\n", docID))
	}
	sb.WriteString(fmt.Sprintf("title: %q\n", opts.Title))
	sb.WriteString(fmt.Sprintf("created: %q\n", now))
	sb.WriteString(fmt.Sprintf("memory_sector: %s\n", sector))
	sb.WriteString(fmt.Sprintf("status: %s\n", status))

	if opts.DocType != "" {
		sb.WriteString(fmt.Sprintf("type: %s\n", opts.DocType))
	}
	if opts.Source != "" {
		sb.WriteString(fmt.Sprintf("source: %s\n", opts.Source))
	}
	if opts.URL != "" {
		sb.WriteString(fmt.Sprintf("url: %q\n", opts.URL))
	}
	if opts.SourceID != "" {
		sb.WriteString(fmt.Sprintf("source_id: %q\n", opts.SourceID))
	}

	if len(opts.Tags) > 0 {
		sb.WriteString("tags:\n")
		for _, tag := range opts.Tags {
			sb.WriteString(fmt.Sprintf("  - %s\n", tag))
		}
	}

	// Write any extra frontmatter fields
	for k, v := range opts.Extra {
		sb.WriteString(fmt.Sprintf("%s: %q\n", k, v))
	}

	sb.WriteString("---\n\n")
	sb.WriteString(opts.Content)

	if err := os.WriteFile(fullPath, []byte(sb.String()), 0644); err != nil {
		return "", fmt.Errorf("write failed: %w", err)
	}

	uri := "cog:mem/" + path
	return uri, nil
}

func (m *MCPServer) toolEmitEvent(ctx context.Context, req *mcp.CallToolRequest, input emitEventInput) (*mcp.CallToolResult, any, error) {
	if input.Type == "" {
		return textResult("event type is required. Valid types: attention.boost, session.marker, insight.captured, decision.made")
	}

	event := map[string]any{
		"type": input.Type,
	}
	if input.Payload != nil {
		event["payload"] = input.Payload
	}

	// Handle attention.boost: resolve URI to field key, then boost.
	if input.Type == "attention.boost" && m.process != nil {
		if uri, ok := input.Payload["uri"].(string); ok && uri != "" {
			fieldKey := ResolveToFieldKey(m.cfg.WorkspaceRoot, uri)
			weight := 1.0
			if w, ok := input.Payload["weight"].(float64); ok && w > 0 {
				weight = w
			}
			m.process.Field().Boost(fieldKey, weight)
			event["field_boosted"] = true
			event["resolved_key"] = fieldKey
		}
	}

	if err := EmitLedgerEvent(m.cfg, event); err != nil {
		return fallbackResult(fmt.Sprintf("emit failed: %v", err), "echo '{\"type\":\"...\"}' >> .cog/ledger/events.jsonl")
	}

	return marshalResult(map[string]any{
		"emitted": true,
		"type":    input.Type,
	})
}

func (m *MCPServer) toolReadLedger(ctx context.Context, req *mcp.CallToolRequest, input readLedgerInput) (*mcp.CallToolResult, any, error) {
	q := LedgerQuery{
		SessionID:      input.SessionID,
		EventType:      input.EventType,
		AfterSeq:       input.AfterSeq,
		SinceTimestamp: input.SinceTimestamp,
		Limit:          input.Limit,
		VerifyChain:    input.VerifyChain,
	}
	result, err := QueryLedger(m.cfg.WorkspaceRoot, q)
	if err != nil {
		return fallbackResult(fmt.Sprintf("read ledger failed: %v", err),
			"ls .cog/ledger/ && cat .cog/ledger/<session_id>/events.jsonl")
	}
	return marshalResult(result)
}

// toolGetIndex — no longer registered as an MCP tool; used by the internal tool loop (tool_loop.go).
func (m *MCPServer) toolGetIndex(ctx context.Context, req *mcp.CallToolRequest, input getIndexInput) (*mcp.CallToolResult, any, error) {
	index, err := BuildMemoryIndex(m.cfg.WorkspaceRoot, input.Sector)
	if err != nil {
		return textResult(fmt.Sprintf("index build failed: %v", err))
	}
	return marshalResult(index)
}

func (m *MCPServer) toolIngest(ctx context.Context, req *mcp.CallToolRequest, input ingestInput) (*mcp.CallToolResult, any, error) {
	if input.Source == "" || input.Format == "" || input.Data == "" {
		return textResult("source, format, and data are required")
	}

	// Build the pipeline fresh (stateless except for workspace root).
	pipeline := NewIngestPipeline(m.cfg.WorkspaceRoot)
	pipeline.Register(NewURLDecomposer(m.cfg.WorkspaceRoot))

	// Build the IngestRequest from input.
	ingestReq := &IngestRequest{
		Source:   IngestSource(input.Source),
		Format:   IngestFormat(input.Format),
		Data:     input.Data,
		Metadata: input.Metadata,
	}

	// Derive a source ID for dedup. For URLs, it's the URL itself.
	// For other formats, use data as the key (or metadata source_id if provided).
	sourceID := input.Data
	if id, ok := input.Metadata["source_id"]; ok && id != "" {
		sourceID = id
	}

	// Check for duplicates.
	if pipeline.CheckDuplicate(sourceID) {
		return marshalResult(map[string]any{
			"ingested":  false,
			"reason":    "duplicate",
			"source_id": sourceID,
		})
	}

	// Run decomposition.
	result, err := pipeline.Ingest(ctx, ingestReq)
	if err != nil {
		return textResult(fmt.Sprintf("ingest failed: %v", err))
	}

	// Ensure source ID is set on the result.
	if result.SourceID == "" {
		result.SourceID = sourceID
	}
	block := NormalizeIngestBlock(ingestReq, result)
	block.WorkspaceID = filepath.Base(m.cfg.WorkspaceRoot)
	if m.nucleus != nil {
		block.TargetIdentity = m.nucleus.Name
	}
	if m.process != nil {
		block.SessionID = m.process.SessionID()
		block.SourceIdentity = m.process.NodeID
		m.process.RecordBlock(block)
	}

	// Determine inbox subdirectory based on content type.
	var subdir string
	switch {
	case result.ContentType == ContentArticle || result.ContentType == ContentPaper ||
		result.ContentType == ContentRepo || result.ContentType == ContentVideo ||
		result.ContentType == ContentTool || result.URL != "":
		subdir = "links"
	case input.Format == string(FormatConversation) || input.Format == string(FormatMessage):
		subdir = "conversations"
	default:
		subdir = "documents"
	}

	// Generate filename: {source}-{date}-{slug}.cog.md
	date := time.Now().UTC().Format("2006-01-02")
	slug := slugify(result.Title)
	if slug == "" {
		slug = "untitled"
	}
	filename := fmt.Sprintf("%s-%s-%s.cog.md", input.Source, date, slug)
	memPath := filepath.Join("semantic", "inbox", subdir, filename)

	// Write the CogDoc.
	opts := CogDocWriteOpts{
		Title:    result.Title,
		Content:  buildIngestContent(result),
		Tags:     result.Tags,
		Status:   string(StatusRaw),
		DocType:  string(result.ContentType),
		Source:   string(result.Source),
		URL:      result.URL,
		SourceID: result.SourceID,
	}

	decision := DefaultMembranePolicy{}.Evaluate(block)
	memPath, opts, shouldWrite := ApplyMembraneDecision(memPath, opts, decision)
	if !shouldWrite {
		slog.Info("ingest: discarded by membrane policy", "reason", decision.Reason)
		return marshalResult(map[string]any{
			"ingested":  false,
			"decision":  string(decision.Decision),
			"reason":    decision.Reason,
			"source_id": result.SourceID,
		})
	}
	if decision.Decision == Quarantine {
		slog.Warn("ingest: quarantined by membrane policy", "reason", decision.QuarantineReason, "path", memPath)
	}
	if decision.Decision == Defer {
		slog.Info("ingest: deferred by membrane policy", "reason", decision.Reason, "path", memPath)
	}

	writeResult, err := m.cogdocSvc.WriteAndSync(memPath, opts)
	if err != nil {
		return textResult(fmt.Sprintf("write cogdoc failed: %v", err))
	}

	return marshalResult(map[string]any{
		"ingested":     true,
		"decision":     string(decision.Decision),
		"reason":       decision.Reason,
		"path":         memPath,
		"uri":          writeResult.URI,
		"title":        result.Title,
		"content_type": string(result.ContentType),
	})
}

// toolTailKernelLog reads the kernel slog JSONL sink at
// <workspace>/.cog/run/kernel.log.jsonl and returns the most recent entries
// that match the filters in input. Mirror of GET /v1/kernel-log; shares the
// same QueryKernelLog backend. See Agent U's kernel-slog-api design for
// rationale (this is the surface half; log_capture.go is the capture half).
func (m *MCPServer) toolTailKernelLog(ctx context.Context, req *mcp.CallToolRequest, input tailKernelLogInput) (*mcp.CallToolResult, any, error) {
	q, err := BuildKernelLogQueryFromValues(
		intToStr(input.Limit),
		input.Level,
		input.Substring,
		input.Since,
		input.Until,
		time.Now(),
	)
	if err != nil {
		return textResult(fmt.Sprintf("invalid kernel-log query: %v", err))
	}

	path := kernelLogPathFor(m.cfg)
	result, err := QueryKernelLog(path, q)
	if err != nil {
		return fallbackResult(
			fmt.Sprintf("kernel log query failed: %v", err),
			fmt.Sprintf("tail -n 100 %s | jq -c .", path),
		)
	}
	return marshalResult(result)
}

// intToStr renders an int as a string for BuildKernelLogQueryFromValues.
// Zero is returned as "" so callers get the default limit rather than 0.
func intToStr(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d", n)
}

// --- Agent state / loop control tools -------------------------------------

// toolListAgents implements cog_list_agents — returns the set of active
// agents. Today always a one-element list ("primary"). See agent-T-agent-
// state-design §4.1.
func (m *MCPServer) toolListAgents(ctx context.Context, req *mcp.CallToolRequest, input listAgentsInput) (*mcp.CallToolResult, any, error) {
	resp, err := QueryListAgents(ctx, m.agentController, ListAgentsRequest{IncludeStopped: input.IncludeStopped})
	if err != nil {
		return agentErrorResult(err, "curl http://localhost:6931/v1/agents")
	}
	return marshalResult(resp)
}

// toolGetAgentState implements cog_get_agent_state — full snapshot of
// one agent. See agent-T-agent-state-design §4.1.
func (m *MCPServer) toolGetAgentState(ctx context.Context, req *mcp.CallToolRequest, input getAgentStateInput) (*mcp.CallToolResult, any, error) {
	snap, err := QueryGetAgent(ctx, m.agentController, GetAgentRequest{
		AgentID:      input.AgentID,
		IncludeTrace: input.IncludeTrace,
		TraceLimit:   input.TraceLimit,
	})
	if err != nil {
		return agentErrorResult(err, "curl http://localhost:6931/v1/agents/primary")
	}
	return marshalResult(snap)
}

// toolTriggerAgentLoop implements cog_trigger_agent_loop — manually
// invoke one cycle. See agent-T-agent-state-design §4.1.
func (m *MCPServer) toolTriggerAgentLoop(ctx context.Context, req *mcp.CallToolRequest, input triggerAgentLoopInput) (*mcp.CallToolResult, any, error) {
	result, err := QueryTriggerAgent(ctx, m.agentController, TriggerAgentRequest{
		AgentID: input.AgentID,
		Reason:  input.Reason,
		Wait:    input.Wait,
	})
	if err != nil {
		return agentErrorResult(err, "curl -X POST http://localhost:6931/v1/agents/primary/tick")
	}
	return marshalResult(result)
}

// agentErrorResult translates AgentControllerError into the MCP fallback
// format used by the rest of this file. Unknown errors are surfaced as
// internal-error text responses.
func agentErrorResult(err error, fallback string) (*mcp.CallToolResult, any, error) {
	msg := err.Error()
	if ace, ok := err.(*AgentControllerError); ok && ace != nil {
		msg = ace.Message
	}
	return fallbackResult(msg, fallback)
}

// toolReadToolCalls is the MCP handler for cog_read_tool_calls. It parses the
// input filters, invokes QueryToolCalls, and returns the stitched result.
func (m *MCPServer) toolReadToolCalls(ctx context.Context, req *mcp.CallToolRequest, input readToolCallsInput) (*mcp.CallToolResult, any, error) {
	q := ToolCallQuery{
		SessionID:     input.SessionID,
		ToolName:      input.ToolName,
		Status:        input.Status,
		Source:        input.Source,
		Ownership:     input.Ownership,
		CallID:        input.CallID,
		Limit:         input.Limit,
		Order:         input.Order,
		IncludeArgs:   input.IncludeArgs,
		IncludeOutput: input.IncludeOutput,
	}
	if input.Since != "" {
		ts, err := parseTimeOrDuration(input.Since)
		if err != nil {
			return textResult(fmt.Sprintf("parse since: %v", err))
		}
		q.Since = ts
	}
	if input.Until != "" {
		ts, err := parseTimeOrDuration(input.Until)
		if err != nil {
			return textResult(fmt.Sprintf("parse until: %v", err))
		}
		q.Until = ts
	}
	result, err := QueryToolCalls(m.cfg.WorkspaceRoot, q)
	if err != nil {
		return fallbackResult(fmt.Sprintf("query failed: %v", err),
			"grep '\"type\":\"tool\\.' .cog/ledger/*/events.jsonl")
	}
	return marshalResult(result)
}

// toolTailToolCalls returns a snapshot of the most recent tool-call rows for
// the filter set. When Agent N's event bus lands this will become a proper
// SSE-style stream; the snapshot behavior is a safe stand-in (same data, same
// shape) that does not rely on a broker.
func (m *MCPServer) toolTailToolCalls(ctx context.Context, req *mcp.CallToolRequest, input tailToolCallsInput) (*mcp.CallToolResult, any, error) {
	limit := input.MaxEvents
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	q := ToolCallQuery{
		SessionID: input.SessionID,
		ToolName:  input.ToolName,
		Status:    input.Status,
		Source:    input.Source,
		Ownership: input.Ownership,
		CallID:    input.CallID,
		Limit:     limit,
		Order:     "desc",
		// Tails default to showing full rows — callers here are actively
		// observing so the PII opt-out is moot.
		IncludeArgs:   true,
		IncludeOutput: true,
	}
	if input.Since != "" {
		ts, err := parseTimeOrDuration(input.Since)
		if err != nil {
			return textResult(fmt.Sprintf("parse since: %v", err))
		}
		q.Since = ts
	}
	// MaxDuration is accepted for forward compatibility with a real stream.
	// The snapshot path is instantaneous; no wait is needed.
	_ = input.MaxDuration

	result, err := QueryToolCalls(m.cfg.WorkspaceRoot, q)
	if err != nil {
		return fallbackResult(fmt.Sprintf("tail failed: %v", err),
			"tail -f .cog/ledger/*/events.jsonl | grep '\"type\":\"tool\\.'")
	}
	stopped := "snapshot"
	return marshalResult(map[string]any{
		"count":          result.Count,
		"events":         result.Calls,
		"stopped_reason": stopped,
		"truncated":      result.Truncated,
	})
}

// parseTimeOrDuration accepts either an RFC3339 timestamp ("2026-04-21T…") or
// a relative duration ("5m", "1h", "24h"). Durations subtract from "now".
func parseTimeOrDuration(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if ts, err := time.Parse(time.RFC3339, s); err == nil {
		return ts, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().UTC().Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("not RFC3339 and not a Go duration: %q", s)
}

// toolReadConversation is the MCP handler for cog_read_conversation.
// Thin wrapper over QueryConversation — same shape the HTTP surface returns.
func (m *MCPServer) toolReadConversation(ctx context.Context, req *mcp.CallToolRequest, input readConversationInput) (*mcp.CallToolResult, any, error) {
	sessionID := input.SessionID
	if sessionID == "" && m.process != nil {
		sessionID = m.process.SessionID()
	}
	includeFull := true
	if input.IncludeFull != nil {
		includeFull = *input.IncludeFull
	}
	includeTools := true
	if input.IncludeTools != nil {
		includeTools = *input.IncludeTools
	}
	q := ConversationQuery{
		SessionID:    sessionID,
		AfterTurn:    input.AfterTurn,
		BeforeTurn:   input.BeforeTurn,
		Limit:        input.Limit,
		IncludeFull:  includeFull,
		IncludeTools: includeTools,
		Order:        input.Order,
	}
	if input.Since != "" {
		t, err := time.Parse(time.RFC3339, input.Since)
		if err != nil {
			return textResult(fmt.Sprintf("invalid since (want RFC3339): %v", err))
		}
		q.Since = t
	}
	res, err := QueryConversation(m.cfg.WorkspaceRoot, q)
	if err != nil {
		return fallbackResult(fmt.Sprintf("query failed: %v", err),
			fmt.Sprintf("jq -c . .cog/run/turns/%s.jsonl", sessionID))
	}
	return marshalResult(res)
}

// ── Config Mutation API ──────────────────────────────────────────────────────

type readConfigInput struct {
	IncludeRawYAML  bool `json:"include_raw_yaml,omitempty" jsonschema:"Also return the raw kernel.yaml bytes"`
	IncludeDefaults bool `json:"include_defaults,omitempty" jsonschema:"Also return the hardcoded defaults for comparison"`
}

type writeConfigInput struct {
	Patch  map[string]any `json:"patch" jsonschema:"RFC 7396 merge-patch object. Fields: port, consolidation_interval, heartbeat_interval, salience_days_window, output_reserve, trm_weights_path, trm_embeddings_path, trm_chunks_path, ollama_embed_endpoint, ollama_embed_model, tool_call_validation_enabled, local_model, digest_paths. Explicit null deletes a key; missing keys preserved."`
	Scope  string         `json:"scope,omitempty" jsonschema:"Target section: 'top' (default) or 'v3'"`
	DryRun bool           `json:"dry_run,omitempty" jsonschema:"If true, validate + return diff without writing"`
}

type rollbackConfigInput struct {
	Backup   string `json:"backup,omitempty" jsonschema:"Backup filename (e.g. kernel.yaml.bak-2026-04-21T16-30-00Z). Empty = most recent."`
	ListOnly bool   `json:"list_only,omitempty" jsonschema:"If true, return the list of backups without restoring"`
}

func (m *MCPServer) toolReadConfig(ctx context.Context, req *mcp.CallToolRequest, input readConfigInput) (*mcp.CallToolResult, any, error) {
	snapshot, err := ReadConfigSnapshot(m.cfg.WorkspaceRoot, input.IncludeRawYAML, input.IncludeDefaults)
	if err != nil {
		// Parse error — still surface whatever we could read but tag the error.
		return marshalResult(map[string]any{
			"effective_config": snapshot.EffectiveConfig,
			"path":             snapshot.Path,
			"exists":           snapshot.Exists,
			"raw_yaml":         snapshot.RawYAML,
			"defaults":         snapshot.Defaults,
			"parse_error":      err.Error(),
		})
	}
	return marshalResult(snapshot)
}

func (m *MCPServer) toolWriteConfig(ctx context.Context, req *mcp.CallToolRequest, input writeConfigInput) (*mcp.CallToolResult, any, error) {
	result, err := WriteConfigPatch(m.cfg.WorkspaceRoot, input.Patch, WriteConfigOptions{
		Scope:  input.Scope,
		DryRun: input.DryRun,
	})
	if err != nil {
		return fallbackResult(fmt.Sprintf("write config failed: %v", err), "edit .cog/config/kernel.yaml and run './scripts/cog restart'")
	}
	return marshalResult(result)
}

func (m *MCPServer) toolRollbackConfig(ctx context.Context, req *mcp.CallToolRequest, input rollbackConfigInput) (*mcp.CallToolResult, any, error) {
	result, err := RollbackConfig(m.cfg.WorkspaceRoot, RollbackOptions{
		Backup:   input.Backup,
		ListOnly: input.ListOnly,
	})
	if err != nil {
		return fallbackResult(fmt.Sprintf("rollback failed: %v", err), "mv .cog/config/kernel.yaml.bak-<timestamp> .cog/config/kernel.yaml")
	}
	return marshalResult(result)
}

func (m *MCPServer) resourceConfig(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	snapshot, _ := ReadConfigSnapshot(m.cfg.WorkspaceRoot, false, true)
	b, err := json.Marshal(snapshot)
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{{
			URI:      req.Params.URI,
			MIMEType: "application/json",
			Text:     string(b),
		}},
	}, nil
}

// toolSearchTraces serves the MCP-side entry for `cog_search_traces`.
// Mirrors the HTTP /v1/traces handler: validates inputs, delegates to
// QueryTraces, returns a JSON-encoded TraceQueryResult.
func (m *MCPServer) toolSearchTraces(ctx context.Context, req *mcp.CallToolRequest, input searchTracesInput) (*mcp.CallToolResult, any, error) {
	tq, err := buildTraceQueryFromInput(input)
	if err != nil {
		return textResult(fmt.Sprintf("invalid trace query: %v", err))
	}
	res, err := QueryTraces(m.cfg.WorkspaceRoot, tq)
	if err != nil {
		return fallbackResult(
			fmt.Sprintf("trace search failed: %v", err),
			"ls .cog/run/*.jsonl && jq -c . .cog/run/<name>.jsonl | head",
		)
	}
	return marshalResult(res)
}

// buildTraceQueryFromInput validates the MCP input shape and normalizes it
// into a TraceQuery. Shares semantics with parseTraceQueryFromRequest so that
// the HTTP and MCP surfaces agree on defaults and bounds.
func buildTraceQueryFromInput(in searchTracesInput) (TraceQuery, error) {
	q := TraceQuery{
		Source:    TraceSource(strings.TrimSpace(in.Source)),
		Level:     strings.TrimSpace(in.Level),
		SessionID: strings.TrimSpace(in.SessionID),
		Substring: in.Substring,
		Limit:     in.Limit,
		Order:     strings.TrimSpace(in.Order),
	}
	if q.Source == "" {
		q.Source = SourceAll
	}
	if _, err := resolveSources(q.Source); err != nil {
		return TraceQuery{}, err
	}

	now := time.Now()
	if in.Since != "" {
		t, err := ParseTraceDurationOrTime(in.Since, now)
		if err != nil {
			return TraceQuery{}, fmt.Errorf("since: %w", err)
		}
		q.Since = t
	}
	if in.Until != "" {
		t, err := ParseTraceDurationOrTime(in.Until, now)
		if err != nil {
			return TraceQuery{}, fmt.Errorf("until: %w", err)
		}
		q.Until = t
	}
	if q.Limit < 0 {
		return TraceQuery{}, fmt.Errorf("limit: expected non-negative integer, got %d", q.Limit)
	}
	if q.Limit > maxTracesLimit {
		return TraceQuery{}, fmt.Errorf("limit: %d exceeds max %d", q.Limit, maxTracesLimit)
	}
	return q, nil
}

// slugify converts a string to a URL-friendly slug.
func slugify(s string) string {
	s = strings.ToLower(s)
	re := regexp.MustCompile(`[^a-z0-9]+`)
	s = re.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 50 {
		// Truncate at a hyphen boundary if possible.
		s = s[:50]
		if idx := strings.LastIndex(s, "-"); idx > 20 {
			s = s[:idx]
		}
	}
	return s
}

// buildIngestContent generates markdown body from an IngestResult.
func buildIngestContent(r *IngestResult) string {
	var sb strings.Builder
	sb.WriteString("# " + r.Title + "\n\n")
	if r.URL != "" {
		sb.WriteString("**URL:** " + r.URL + "\n\n")
	}
	if r.Domain != "" {
		sb.WriteString("**Domain:** " + r.Domain + "\n\n")
	}
	if r.Summary != "" {
		sb.WriteString("## Summary\n\n" + r.Summary + "\n\n")
	}
	if len(r.Fields) > 0 {
		sb.WriteString("## Metadata\n\n")
		for k, v := range r.Fields {
			sb.WriteString(fmt.Sprintf("- **%s:** %s\n", k, v))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func (p cogdocFrontmatterPatch) empty() bool {
	return strings.TrimSpace(p.Description) == "" && len(p.Tags) == 0 && strings.TrimSpace(p.Type) == ""
}

func patchTemplateForIssues(issues []string) map[string]any {
	if len(issues) == 0 {
		return nil
	}
	template := map[string]any{}
	for _, issue := range issues {
		switch issue {
		case "missing_description":
			template["description"] = ""
		case "missing_tags":
			template["tags"] = []string{}
		case "missing_type":
			template["type"] = ""
		}
	}
	if len(template) == 0 {
		return nil
	}
	return template
}

func hasSchemaIssue(issues []string, want string) bool {
	for _, issue := range issues {
		if issue == want {
			return true
		}
	}
	return false
}

func applyFrontmatterPatch(content string, patch cogdocFrontmatterPatch) (string, cogdocFrontmatter, error) {
	var (
		raw  map[string]any
		body string
	)

	yamlBlock, extractedBody, ok := extractFrontmatterYAML(content)
	if ok {
		if err := yaml.Unmarshal([]byte(yamlBlock), &raw); err != nil {
			return "", cogdocFrontmatter{}, fmt.Errorf("parse frontmatter: %w", err)
		}
		body = extractedBody
	} else {
		raw = map[string]any{}
		body = strings.TrimLeft(content, "\r\n")
	}
	if raw == nil {
		raw = map[string]any{}
	}

	if strings.TrimSpace(patch.Description) != "" {
		raw["description"] = strings.TrimSpace(patch.Description)
	}
	if len(patch.Tags) > 0 {
		raw["tags"] = patch.Tags
	}
	if strings.TrimSpace(patch.Type) != "" {
		raw["type"] = strings.TrimSpace(patch.Type)
	}

	marshaled, err := yaml.Marshal(raw)
	if err != nil {
		return "", cogdocFrontmatter{}, fmt.Errorf("marshal frontmatter: %w", err)
	}

	updated := fmt.Sprintf("---\n%s---\n", marshaled)
	if body != "" {
		updated += "\n" + body
	}

	fm, _ := parseCogdocFrontmatter(updated)
	return updated, fm, nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func marshalResult(data any) (*mcp.CallToolResult, any, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return textResult(fmt.Sprintf("marshal error: %v", err))
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(b)},
		},
	}, nil, nil
}

func textResult(text string) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: text},
		},
	}, nil, nil
}

// fallbackResult returns an error message with a CLI fallback command.
// This is the graceful degradation path — when the kernel is unavailable,
// the agent can fall back to shell commands that work without it.
func fallbackResult(errMsg, fallbackCmd string) (*mcp.CallToolResult, any, error) {
	text := fmt.Sprintf("%s\n\nFallback (kernel unavailable): %s", errMsg, fallbackCmd)
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: text},
		},
		IsError: true,
	}, nil, nil
}

// extractSection pulls a section from markdown by heading anchor.
func extractSection(content, anchor string) string {
	lines := strings.Split(content, "\n")
	var capturing bool
	var level int
	var result []string

	for _, line := range lines {
		if strings.Contains(line, "{#"+anchor+"}") || strings.Contains(line, "# "+anchor) {
			capturing = true
			level = strings.Count(strings.TrimLeft(line, " "), "#")
			result = append(result, line)
			continue
		}
		if capturing {
			// Stop at same or higher level heading
			trimmed := strings.TrimLeft(line, " ")
			if strings.HasPrefix(trimmed, "#") {
				headingLevel := strings.Count(trimmed, "#")
				if headingLevel <= level {
					break
				}
			}
			result = append(result, line)
		}
	}
	return strings.Join(result, "\n")
}

// init logging for MCP operations
func init() {
	_ = slog.Default() // ensure slog is initialized
}
