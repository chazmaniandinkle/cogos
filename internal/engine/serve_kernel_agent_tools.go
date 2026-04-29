// serve_kernel_agent_tools.go — auto-injection of the kernel's MCP tool
// registry on the kernel-agent chat route.
//
// Closes cogos-dev/cogos#89.
//
// Background: when a chat request lands on the kernel-agent route, the
// inference provider receives whatever ToolDefinitions the caller chose to
// pass through. The dashboard at internal/engine/web/dashboard.html builds
// `{model, messages, stream}` with no `tools` array, so the model is told
// about zero tools and falls back to narrating tool calls in prose
// ("I will use cog_read_cogdoc...") that never fire. The user perceives
// this as the kernel handoff being "duplicated/dropped/disconnected" —
// it's none of those; the tools are simply not plumbed.
//
// This helper resolves that asymmetry by treating the kernel-agent route as
// the authoritative provider of its own tools. When the caller did not
// supply any, we inject the snapshot the MCP server captured at startup
// (see mcp_tool_defs.go) and partition by ownership so the existing chat
// pipeline routes server-side tools (Bash/Read/etc.) to the kernel and
// external tools (cog_*, mod3_*, etc.) back through the MCP bridge or the
// client per classifyToolOwnership.
package engine

import "log/slog"

// injectKernelAgentTools sets creq.Tools (and the partitioned
// creq.ExternalTools) from the MCP server's cached tool registry snapshot.
// No-op if the snapshot is empty. Idempotent: callers must guard with
// `len(creq.Tools) == 0` so an explicit (even empty) caller-provided
// tools array still wins.
//
// The function is package-internal and intentionally narrow — it's the
// single place the chat path reaches into the MCP registry, which keeps
// the coupling auditable.
func injectKernelAgentTools(creq *CompletionRequest, m *MCPServer) {
	if creq == nil || m == nil {
		return
	}
	defs := m.ToolDefinitions()
	if len(defs) == 0 {
		return
	}

	// Allocate fresh slices so we don't accidentally alias the snapshot.
	// The snapshot is read-only, but downstream code (provider adapters)
	// has historically appended to creq.Tools; copying keeps us safe from
	// future mutation regressions.
	tools := make([]ToolDefinition, len(defs))
	copy(tools, defs)
	creq.Tools = tools

	// Partition by ownership the same way the OpenAI-compat path does:
	// internal-ownership tools execute server-side; client-ownership tools
	// are forwarded back as tool_calls to whoever sent the request. The
	// classifyToolOwnership helper currently maps Bash/Read/Write/...
	// to kernel and everything else to client; that means cog_* MCP tools
	// land in ExternalTools today. That is consistent with the current
	// dispatch path — extending kernel-side execution to cog_* tools is a
	// separate, larger change tracked by the audit Tier-2 work.
	for _, t := range tools {
		if classifyToolOwnership(t.Name) == ToolOwnershipClient {
			creq.ExternalTools = append(creq.ExternalTools, t)
		}
	}

	slog.Info("chat: kernel-agent auto-injected MCP tool registry",
		"request_id", creq.Metadata.RequestID,
		"tool_count", len(tools),
		"external_count", len(creq.ExternalTools),
	)
}
