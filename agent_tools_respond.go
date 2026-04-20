// agent_tools_respond.go — Dashboard response tool for the agent harness.
//
// Paired with agent_bus_inlet.go: the inlet feeds dashboard user messages
// into the metabolic cycle, and this tool lets the cycle's agent reply.
//
// The respond tool publishes a structured agent_response payload onto
// bus_dashboard_response, where Mod³ subscribes via /v1/events/stream.
//
// Additive only: the standard RegisterCoreTools flow keeps its current
// behaviour; RegisterRespondTool is a separate call site so deployments that
// don't want the dashboard channel can skip it.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
)

// respondToolName is the canonical name the Execute loop watches for.
const respondToolName = "respond"

// errDashboardNotInstalled is returned when the respond tool is invoked
// before InstallDashboardInlet has wired a bus manager.
var errDashboardNotInstalled = errors.New("dashboard inlet not installed: call InstallDashboardInlet before using the respond tool")

// respondInvokeCount is incremented on every successful respond-tool
// dispatch. The agent cycle snapshots the value before Execute and compares
// after — a delta means `respond` landed this turn and the auto-fallback
// publisher must skip (otherwise the dashboard sees a double reply).
//
// Package-global and atomic so the tool func (a closure) can increment it
// without plumbing state through the harness registry.
var respondInvokeCount uint64

// respondPublish is the seam used by the respond tool to emit a reply. Tests
// swap this to capture fan-out without standing up a live bus manager;
// production points it at the bus-publishing implementation. Must be called
// per-session during fan-out so multi-session cycles reach every recipient.
var respondPublish = publishDashboardResponse

// respondInvokeSnapshot returns the current respond-call counter. Pair with a
// later check `respondInvokedSince(snapshot)` around the Execute call.
func respondInvokeSnapshot() uint64 {
	return atomic.LoadUint64(&respondInvokeCount)
}

// respondInvokedSince reports whether respond was called since the given
// snapshot was taken. Used by the agent cycle to dedup the auto-fallback.
func respondInvokedSince(snapshot uint64) bool {
	return atomic.LoadUint64(&respondInvokeCount) > snapshot
}

// RegisterRespondTool adds the respond tool to the harness. Callers should
// invoke this after InstallDashboardInlet so the tool has a bus manager to
// publish through; if called earlier, the tool will simply return an error
// at invocation time rather than at registration.
func RegisterRespondTool(h *AgentHarness) {
	if h == nil {
		return
	}
	h.RegisterTool(respondDef(), newRespondFunc())
}

// respondDef returns the tool schema for `respond`.
func respondDef() ToolDefinition {
	return ToolDefinition{
		Type: "function",
		Function: ToolFunction{
			Name: respondToolName,
			Description: `Send a response to the user in the current dashboard conversation. Use this after you have observed a user_message event and want to reply. The message is published on bus_dashboard_response for the Mod³ dashboard to render. Call at most once per user turn; use wait if no reply is warranted.`,
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"text": {
						"type": "string",
						"description": "The reply text to show the user."
					},
					"reasoning": {
						"type": "string",
						"description": "Optional internal reasoning/trace for auditing. Not shown to the user directly."
					}
				},
				"required": ["text"]
			}`),
		},
	}
}

// newRespondFunc returns the ToolFunc for `respond`.
//
// On success: {"ok": true, "bytes": <payload-size>}.
// On failure: an error wrapping the underlying cause (bus manager missing,
// bus write failure, malformed args). Errors are returned rather than
// swallowed so the harness can surface them in its transcript.
func newRespondFunc() ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
		var p struct {
			Text      string `json:"text"`
			Reasoning string `json:"reasoning"`
		}
		if len(args) > 0 {
			if err := json.Unmarshal(args, &p); err != nil {
				return nil, err
			}
		}
		if p.Text == "" {
			return json.Marshal(map[string]interface{}{
				"ok":    false,
				"error": "text is required",
			})
		}

		// Fan out across every session_id observed on the cycle's pending
		// queue. A cycle that drained N messages from N different tabs owes
		// each of them a reply — without this loop, only the first session
		// would ever hear back and the rest would wait forever.
		//
		// sessionIDsFromContext returns nil when the cycle wasn't wired for
		// fan-out (single-message cycles, non-dashboard calls, tests); we
		// fall back to the legacy single-id path and let publishDashboardResponse
		// omit the field when the id is empty (broadcast).
		sessionIDs := sessionIDsFromContext(ctx)
		if len(sessionIDs) == 0 {
			sessionIDs = []string{sessionIDFromContext(ctx)}
		}

		var (
			totalBytes int
			firstErr   error
			anySuccess bool
		)
		for _, sid := range sessionIDs {
			n, err := respondPublish(p.Text, p.Reasoning, sid)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			anySuccess = true
			totalBytes += n
		}

		if !anySuccess {
			// All publishes failed — surface the first error and do NOT bump
			// the invocation counter so the auto-fallback still fires.
			return json.Marshal(map[string]interface{}{
				"ok":    false,
				"error": firstErr.Error(),
			})
		}

		// At least one publish landed. Bump the counter exactly once per
		// respond-tool invocation so the auto-fallback dedups correctly —
		// N fan-out publishes still count as one tool call.
		atomic.AddUint64(&respondInvokeCount, 1)

		return json.Marshal(map[string]interface{}{
			"ok":         true,
			"bytes":      totalBytes,
			"recipients": len(sessionIDs),
		})
	}
}
