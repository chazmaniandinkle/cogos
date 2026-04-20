// agent_tools_wait.go — Sanctioned silent no-op for the agent harness.
//
// The `wait` tool gives the model an action other than rationalizing when
// an observation warrants no further work this cycle. Without it, the tool
// loop forces the model to either keep calling tools (fabricating work)
// or emit content (justifying in prose). Calling `wait` terminates the
// current Execute cycle cleanly and records the reason.
//
// Analogous to Poke's wait tool — addresses the justification-loop pathology
// documented in the corpus comparison (Gap 1).

package main

import (
	"context"
	"encoding/json"
	"log"
)

// waitToolName is the canonical name the Execute loop watches for.
const waitToolName = "wait"

// RegisterWaitTool adds the wait tool to the harness.
func RegisterWaitTool(h *AgentHarness, workspaceRoot string) {
	h.RegisterTool(waitDef(), newWaitFunc(workspaceRoot))
}

// waitDef returns the tool schema for `wait`.
func waitDef() ToolDefinition {
	return ToolDefinition{
		Type: "function",
		Function: ToolFunction{
			Name: waitToolName,
			Description: `Sanctioned no-op. Call this when the observation warrants no proposal, no bus event, and no further investigation — nothing needs doing right now. This ends the current execution cycle cleanly. Prefer this over fabricating work or rationalizing in prose when nothing genuinely needs to happen.`,
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"reason": {
						"type": "string",
						"description": "Brief reason for waiting (recorded to the agent log but not acted on)"
					}
				}
			}`),
		},
	}
}

// newWaitFunc returns the tool function for `wait`.
//
// The workspaceRoot is accepted for symmetry with other Register*Tool helpers
// and future extension (e.g. dedicated wait log at .cog/run/agent-waits.jsonl);
// current implementation writes only to stderr via log.Printf, matching the
// existing harness logging pattern.
func newWaitFunc(_ string) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
		var p struct {
			Reason string `json:"reason"`
		}
		if args != nil {
			_ = json.Unmarshal(args, &p)
		}

		reason := p.Reason
		if reason == "" {
			reason = "(no reason given)"
		}
		log.Printf("[agent] wait: %s", reason)

		return json.Marshal(map[string]string{
			"status": "waiting",
			"reason": reason,
		})
	}
}

// extractWaitReason parses the reason field from a tool result written by
// newWaitFunc. Used by Execute to surface the reason in its final return.
// Returns empty string if the JSON is malformed or the field is absent.
func extractWaitReason(result json.RawMessage) string {
	var r struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(result, &r); err != nil {
		return ""
	}
	return r.Reason
}
