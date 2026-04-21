// serve_context_build.go — Context build endpoint.
//
// POST /v1/context/build runs the CogOS context-engine pipeline (thread parse,
// session resolve, compressor) and returns the constructed ContextWindow as JSON.
// Unlike /v1/chat/completions, this endpoint does NOT invoke inference — no
// Claude CLI, no bus chat.response event, no tool-bridge.
//
// This lets external callers (e.g., LM Studio plugins running a local model)
// use CogOS as the context authority while doing generation elsewhere. Aligns
// with the EA/EFM thesis (externalized attention: the substrate decides what
// matters before the model ever sees it).
//
// Side effects: the context engine may rotate its internal session state via
// SessionManager.Rotate. No lifecycle/thread/bus side effects fire here —
// those remain chat-completions-only. If you need the full side-effect path,
// use /v1/chat/completions.

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// ContextBuildResponse is the JSON payload returned by POST /v1/context/build.
// Shape intentionally mirrors the fields of ContextWindow + additional diagnostic
// blocks so callers can inspect what CogOS decided was relevant.
type ContextBuildResponse struct {
	SessionID       string           `json:"session_id"`
	ClaudeSessionID string           `json:"claude_session_id,omitempty"`
	Strategy        string           `json:"strategy,omitempty"` // "resume" | "fresh" | "fallback"
	TotalTokens     int              `json:"total_tokens"`
	SystemPrompt    string           `json:"system_prompt"`
	Prompt          string           `json:"prompt"`
	TAA             *ContextBuildTAA `json:"taa,omitempty"`
}

// ContextBuildTAA is the TAA diagnostic block exposed in the response.
type ContextBuildTAA struct {
	Profile        string         `json:"profile,omitempty"`
	Enabled        bool           `json:"enabled"`
	TotalTokens    int            `json:"total_tokens,omitempty"`
	CoherenceScore float64        `json:"coherence,omitempty"`
	Tiers          map[string]int `json:"tiers,omitempty"`
}

// handleContextBuild runs the context engine without inference and returns
// the constructed ContextWindow. Shape of the request matches
// ChatCompletionRequest; `stream` and `tools` are accepted but have no effect.
func (s *serveServer) handleContextBuild(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB limit
	var req ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error(), "invalid_request")
		return
	}

	if s.contextEngine == nil {
		s.writeError(w, http.StatusServiceUnavailable, "Context engine not initialized", "unavailable")
		return
	}

	workspaceRoot := ""
	if s.kernel != nil {
		workspaceRoot = s.kernel.Root()
	}

	// Parse UCP headers (same semantics as /v1/chat/completions).
	ucpContext, err := parseUCPHeaders(r, workspaceRoot)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid UCP headers: "+err.Error(), "invalid_request")
		return
	}

	// Flatten system + user messages the same way the chat handler does.
	systemPrompt := req.SystemPrompt
	var userPrompt strings.Builder
	for _, msg := range req.Messages {
		content := msg.GetContent()
		switch msg.Role {
		case "system":
			if systemPrompt == "" {
				systemPrompt = content
			} else {
				systemPrompt += "\n\n" + content
			}
		case "user":
			if userPrompt.Len() > 0 {
				userPrompt.WriteString("\n\n")
			}
			userPrompt.WriteString(content)
		case "assistant":
			if userPrompt.Len() > 0 {
				userPrompt.WriteString("\n\nAssistant: ")
				if content != "" {
					userPrompt.WriteString(content)
				}
				if toolSummary := msg.GetToolCallsSummary(); toolSummary != "" {
					if content != "" {
						userPrompt.WriteString("\n")
					}
					userPrompt.WriteString(toolSummary)
				}
				userPrompt.WriteString("\n\nUser: ")
			}
		case "tool":
			if userPrompt.Len() > 0 && content != "" {
				toolResult := content
				if len(toolResult) > 500 {
					toolResult = toolResult[:500] + "...(truncated)"
				}
				if msg.ToolCallID != "" {
					userPrompt.WriteString(fmt.Sprintf("\n[Tool result (%s): %s]", msg.ToolCallID, toolResult))
				} else {
					userPrompt.WriteString(fmt.Sprintf("\n[Tool result: %s]", toolResult))
				}
			}
		}
	}

	if userPrompt.Len() == 0 {
		s.writeError(w, http.StatusBadRequest, "No user message provided", "invalid_request")
		return
	}

	// Derive session ID (same rules as chat completions).
	sessionID := r.Header.Get("X-Session-ID")
	if sessionID == "" {
		sessionID = r.Header.Get("X-Eidolon-ID")
	}
	if sessionID == "" {
		origin := r.Header.Get("X-Origin")
		if origin == "" {
			origin = "http"
		}
		if ucpContext != nil && ucpContext.Identity != nil && ucpContext.Identity.Name != "" {
			sessionID = origin + ":" + strings.ToLower(ucpContext.Identity.Name)
		} else {
			sessionID = origin
		}
	}

	// Resolve TAA profile (UCP packet > X-TAA-Profile header > body field).
	var taaProfile string
	var taaEnabled bool
	if ucpContext != nil && ucpContext.TAA != nil {
		taaProfile = ucpContext.TAA.Profile
		taaEnabled = true
	} else {
		taaProfile, taaEnabled = req.GetTAAProfileWithHeader(r.Header.Get("X-TAA-Profile"))
	}

	// Run the context engine.
	ctxWindow, buildErr := s.contextEngine.Build(req.Messages, sessionID, taaProfile, RequestHeaders{
		Origin:       r.Header.Get("X-Origin"),
		SessionReset: r.Header.Get("X-Session-Reset") == "true",
		UserID:       r.Header.Get("X-OpenClaw-User-ID"),
		UserName:     r.Header.Get("X-OpenClaw-User-Name"),
	})
	if buildErr != nil {
		log.Printf("[context-build] context engine error: %v", buildErr)
		s.writeError(w, http.StatusInternalServerError, buildErr.Error(), "context_build_failed")
		return
	}

	// Assemble response. When the context engine returns a populated window,
	// prefer its prompt/system values (they reflect compression + session-resume
	// decisions). Otherwise fall back to the flat prompt we just constructed —
	// callers still get a usable payload.
	resp := &ContextBuildResponse{
		SessionID:       sessionID,
		ClaudeSessionID: ctxWindow.ClaudeSession,
		Strategy:        ctxWindow.Strategy,
		TotalTokens:     ctxWindow.TotalTokens,
		SystemPrompt:    ctxWindow.SystemPrompt,
		Prompt:          ctxWindow.Prompt,
	}
	if resp.SystemPrompt == "" {
		resp.SystemPrompt = systemPrompt
	}
	if resp.Prompt == "" {
		resp.Prompt = userPrompt.String()
	}

	// TAA diagnostics — run the same ConstructContextStateWithProfile call the
	// chat handler does, so callers see the tier breakdown + coherence score.
	if taaEnabled {
		taa := &ContextBuildTAA{Profile: taaProfile, Enabled: true}
		contextState, cerr := ConstructContextStateWithProfile(req.Messages, sessionID, workspaceRoot, taaProfile)
		if cerr == nil && contextState != nil {
			taa.TotalTokens = contextState.TotalTokens
			taa.CoherenceScore = contextState.CoherenceScore
			tiers := make(map[string]int)
			if contextState.Tier1Identity != nil {
				tiers["tier1"] = contextState.Tier1Identity.Tokens
			}
			if contextState.Tier2Temporal != nil {
				tiers["tier2"] = contextState.Tier2Temporal.Tokens
			}
			if contextState.Tier3Present != nil {
				tiers["tier3"] = contextState.Tier3Present.Tokens
			}
			if contextState.Tier4Semantic != nil {
				tiers["tier4"] = contextState.Tier4Semantic.Tokens
			}
			if len(tiers) > 0 {
				taa.Tiers = tiers
			}
		}
		resp.TAA = taa
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
