// mcp_tool_defs.go — snapshot of the MCP tool registry as ToolDefinitions
// usable on the chat path (OpenAI-compat /v1/chat/completions, Anthropic-compat
// /v1/messages).
//
// Why this file exists: when the dashboard chat path targets the kernel-agent
// route with no client-supplied tools, the server needs to advertise its own
// MCP tool surface to the inference provider. The MCP server's internal
// schema-inferred input schemas are stored on copies (the SDK does
// `tt := *t` inside AddTool), so we can't read them off the originals.
// We instead spin up an in-process MCP client at construction time, call
// ListTools, and cache the resulting [ToolDefinition] slice. From then on
// the snapshot is read-only and lock-free — registerTools has already
// returned, so nothing mutates the set after this point.
//
// See cogos-dev/cogos#89 (auto-inject kernel tool registry on
// `model: kernel-agent`) for the user-visible motivation.
package engine

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// snapshotToolDefinitions queries the in-process MCP server for its full tool
// list and returns them as kernel-side [ToolDefinition] values, ready to set
// on a [CompletionRequest.Tools]. The query runs over an in-memory transport
// pair; no real I/O happens. Any failure is logged and an empty slice is
// returned — callers should treat the auto-injection as best-effort.
func snapshotToolDefinitions(server *mcp.Server) []ToolDefinition {
	if server == nil {
		return nil
	}

	// Bound the listing call so a misbehaving in-process transport can't hang
	// the kernel boot path.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		slog.Warn("mcp: tool-def snapshot: server connect failed", "err", err)
		return nil
	}
	defer serverSession.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "cogos-tool-snapshot", Version: "1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		slog.Warn("mcp: tool-def snapshot: client connect failed", "err", err)
		return nil
	}
	defer clientSession.Close()

	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		slog.Warn("mcp: tool-def snapshot: ListTools failed", "err", err)
		return nil
	}

	defs := make([]ToolDefinition, 0, len(res.Tools))
	for _, t := range res.Tools {
		if t == nil {
			continue
		}
		defs = append(defs, ToolDefinition{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: marshalInputSchema(t.InputSchema),
		})
	}
	return defs
}

// marshalInputSchema converts an mcp.Tool.InputSchema (typed `any`, but in
// practice either *jsonschema.Schema or map[string]any depending on whether
// the call came over the wire) into a plain map[string]any. Returns the
// canonical empty-object schema when conversion fails so providers always
// receive a valid JSON Schema object — never nil, never a non-object.
func marshalInputSchema(s any) map[string]interface{} {
	if s == nil {
		return map[string]interface{}{"type": "object"}
	}
	if m, ok := s.(map[string]interface{}); ok {
		// ListTools over a real transport would return a map already;
		// over the in-memory transport it does too because the schema
		// is JSON-marshaled and unmarshaled on its way through.
		return m
	}
	// Fallback: round-trip through JSON. Handles *jsonschema.Schema and any
	// other type that JSON-marshals to a schema object.
	data, err := json.Marshal(s)
	if err != nil {
		return map[string]interface{}{"type": "object"}
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]interface{}{"type": "object"}
	}
	if m == nil {
		return map[string]interface{}{"type": "object"}
	}
	return m
}

// ToolDefinitions returns a copy of the MCP tool registry snapshot as
// [ToolDefinition] values. Returns nil if the snapshot was never populated
// (e.g. NewMCPServer was constructed without going through the snapshot path,
// or the snapshot failed). Safe for concurrent reads — the underlying slice
// is set once during construction and never mutated.
func (m *MCPServer) ToolDefinitions() []ToolDefinition {
	if m == nil || len(m.toolDefs) == 0 {
		return nil
	}
	out := make([]ToolDefinition, len(m.toolDefs))
	copy(out, m.toolDefs)
	return out
}
