// tools.go maps OpenAI-format tool definitions to Claude CLI tool names
// and classifies tool ownership (internal = CogOS executes, external = client
// executes). This is the plumbing that lets BrowserOS-style clients forward
// their `browser_*` tool definitions through the kernel without the harness
// silently dropping them.
//
// Key entry points:
//
//   - [MapToolsToCLINames] — extract Claude CLI `--allowed-tools` names from
//     OpenAI-format tool defs. Only internal tools have CLI names; external
//     tools are not returned (by design — they're registered through the
//     MCP bridge, not `--allowed-tools`).
//   - [ClassifyTool] — ownership lookup for a single tool name.
//   - [PartitionTools] — split a raw tool list into (internal, external).
//   - [ExtractToolName] — pull the function name out of one OpenAI tool def.
//
// The internal-tool set is closed (the CogOS kernel has a fixed set of
// built-ins: Bash, Read, Write, Edit, Grep, Glob). Anything else is assumed
// external — the model is told about them via an MCP bridge and any
// `tool_use` events Claude emits for them are returned to the client as
// OpenAI-format `tool_calls` rather than executed server-side.
package harness

import (
	"encoding/json"
	"strings"
)

// ToolOwnership describes who is responsible for executing a tool call.
type ToolOwnership int

const (
	// ToolInternal indicates the CogOS kernel or Claude CLI subprocess will
	// execute this tool directly (filesystem, shell, etc).
	ToolInternal ToolOwnership = iota

	// ToolExternal indicates the calling client (e.g. BrowserOS) owns
	// execution. The harness returns `tool_calls` to the client and expects
	// the client to send back a `role: "tool"` message with the result on
	// the next turn.
	ToolExternal
)

// String returns a human-readable ownership label, mainly for logs/telemetry.
func (o ToolOwnership) String() string {
	switch o {
	case ToolInternal:
		return "internal"
	case ToolExternal:
		return "external"
	default:
		return "unknown"
	}
}

// internalToolCLINames is the closed set of tool names the CogOS harness
// knows how to execute through Claude CLI's `--allowed-tools`. Keys are
// normalised (lowercase) OpenAI function names; values are the CamelCase
// names Claude CLI expects. Anything NOT in this map is considered external.
//
// Extending the set of internal tools is intentionally additive — add a
// new row here and the classifier, MapToolsToCLINames, and PartitionTools
// all pick it up automatically.
var internalToolCLINames = map[string]string{
	"exec":        "Bash",
	"bash":        "Bash",
	"shell":       "Bash",
	"read":        "Read",
	"file_read":   "Read",
	"write":       "Write",
	"file_write":  "Write",
	"edit":        "Edit",
	"apply-patch": "Edit",
	"apply_patch": "Edit",
	"search":      "Grep",
	"grep":        "Grep",
	"glob":        "Glob",
	"find":        "Glob",
}

// ExtractToolName pulls the function name out of a single OpenAI-format
// tool definition. Returns "" for malformed input.
func ExtractToolName(raw json.RawMessage) string {
	var tool struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &tool); err != nil {
		return ""
	}
	return tool.Function.Name
}

// ClassifyTool reports whether a tool name is internal (executed by CogOS /
// Claude CLI) or external (executed by the client). Matching is
// case-insensitive and name-only — schema is irrelevant.
func ClassifyTool(name string) ToolOwnership {
	if _, ok := internalToolCLINames[strings.ToLower(name)]; ok {
		return ToolInternal
	}
	return ToolExternal
}

// PartitionTools splits a list of OpenAI-format tool definitions into two
// disjoint slices preserving input order:
//
//   - internal: tools the harness will execute itself (mapped via Claude CLI
//     `--allowed-tools` or the MCP bridge for CogOS built-ins).
//   - external: tools to be forwarded to the client as `tool_calls`.
//
// Malformed entries (bad JSON, missing function.name) are dropped from both
// outputs — identical to the old MapToolsToCLINames silent-skip behaviour.
func PartitionTools(tools []json.RawMessage) (internal, external []json.RawMessage) {
	for _, raw := range tools {
		name := ExtractToolName(raw)
		if name == "" {
			continue
		}
		switch ClassifyTool(name) {
		case ToolInternal:
			internal = append(internal, raw)
		case ToolExternal:
			external = append(external, raw)
		}
	}
	return internal, external
}

// MapToolsToCLINames extracts function names from OpenAI-format tool definitions
// and maps them to Claude CLI built-in tool names where possible.
//
// Only internal tools produce output. External tools are silently dropped —
// they're registered through `--mcp-config` (or forwarded to the client as
// `tool_calls`) rather than `--allowed-tools`, so including them here would
// just make Claude CLI fail to start.
func MapToolsToCLINames(tools []json.RawMessage) []string {
	var result []string
	seen := make(map[string]bool)

	for _, raw := range tools {
		name := ExtractToolName(raw)
		if name == "" {
			continue
		}

		cliName := mapToolName(name)
		if cliName == "" {
			continue
		}
		if !seen[cliName] {
			seen[cliName] = true
			result = append(result, cliName)
		}
	}
	return result
}

// mapToolName maps an OpenAI-format tool function name to a Claude CLI tool
// name. Returns "" when the tool is external (client-owned).
func mapToolName(name string) string {
	return internalToolCLINames[strings.ToLower(name)]
}
