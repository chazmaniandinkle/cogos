// agent_tools_propose.go — Proposal-based tools for the agent harness.
//
// The agent can observe the workspace freely but cannot modify it directly.
// Instead it writes proposals to a staging area (.cog/.state/agent/proposals/)
// that require authorization from a human or cloud-tier agent before applying.
//
// This enforces the read-and-propose cycle: observe → assess → propose → sleep.
// The agent sees its own proposals on the next cycle and can build on them,
// revise them, or move on.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	proposalsDir = ".cog/.state/agent/proposals"
)

// RegisterProposalTools replaces direct memory_write with gated proposal tools.
func RegisterProposalTools(h *AgentHarness, workspaceRoot string) {
	h.RegisterTool(proposeDef(), newProposeFunc(workspaceRoot))
	h.RegisterTool(listProposalsDef(), newListProposalsFunc(workspaceRoot))
	h.RegisterTool(readProposalDef(), newReadProposalFunc(workspaceRoot))
	h.RegisterTool(acknowledgeProposalDef(), newAcknowledgeProposalFunc(workspaceRoot))
}

// --- propose ---

func proposeDef() ToolDefinition {
	return ToolDefinition{
		Type: "function",
		Function: ToolFunction{
			Name: "propose",
			Description: `Write a proposal for a workspace change. Proposals are staged — they do NOT modify the workspace directly. A human or authorized agent must approve them before they take effect.

Use this to:
- Suggest memory document updates or new documents
- Propose code changes or refactors
- Record observations that should be acted on later
- Flag issues that need attention

Each proposal gets a unique ID and is visible on subsequent cycles.`,
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"type": {
						"type": "string",
						"description": "Proposal type: memory_update, memory_new, code_change, observation, escalation",
						"enum": ["memory_update", "memory_new", "code_change", "observation", "escalation"]
					},
					"target": {
						"type": "string",
						"description": "What this proposal targets (file path, memory path, or description)"
					},
					"title": {
						"type": "string",
						"description": "Short title for the proposal"
					},
					"body": {
						"type": "string",
						"description": "Full proposal content — the proposed change, observation, or escalation details"
					},
					"urgency": {
						"type": "number",
						"description": "How urgent is this proposal (0.0 = informational, 1.0 = critical)"
					}
				},
				"required": ["type", "title", "body"]
			}`),
		},
	}
}

func newProposeFunc(root string) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
		var p struct {
			Type    string  `json:"type"`
			Target  string  `json:"target"`
			Title   string  `json:"title"`
			Body    string  `json:"body"`
			Urgency float64 `json:"urgency"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, fmt.Errorf("parse args: %w", err)
		}
		if p.Title == "" || p.Body == "" {
			return json.Marshal(map[string]string{"error": "title and body are required"})
		}
		if p.Type == "" {
			p.Type = "observation"
		}

		// Create proposals directory
		dir := filepath.Join(root, proposalsDir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return json.Marshal(map[string]string{"error": "cannot create proposals dir: " + err.Error()})
		}

		// Generate proposal ID from timestamp
		now := time.Now()
		id := now.Format("20060102-150405")

		// Build proposal as markdown with frontmatter
		var sb strings.Builder
		sb.WriteString("---\n")
		sb.WriteString(fmt.Sprintf("id: %s\n", id))
		sb.WriteString(fmt.Sprintf("type: %s\n", p.Type))
		sb.WriteString(fmt.Sprintf("title: %q\n", p.Title))
		sb.WriteString(fmt.Sprintf("target: %q\n", p.Target))
		sb.WriteString(fmt.Sprintf("urgency: %.1f\n", p.Urgency))
		sb.WriteString(fmt.Sprintf("created: %s\n", now.Format(time.RFC3339)))
		sb.WriteString("status: pending\n")
		sb.WriteString("---\n\n")
		sb.WriteString(fmt.Sprintf("# %s\n\n", p.Title))
		sb.WriteString(p.Body)
		sb.WriteString("\n")

		filename := fmt.Sprintf("%s-%s.md", id, sanitizeFilename(p.Title))
		path := filepath.Join(dir, filename)

		if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
			return json.Marshal(map[string]string{"error": "write proposal: " + err.Error()})
		}

		return json.Marshal(map[string]string{
			"status":      "proposed",
			"proposal_id": id,
			"path":        path,
			"message":     "Proposal staged. It will be visible on your next cycle and requires authorization to apply.",
		})
	}
}

// --- list_proposals ---

func listProposalsDef() ToolDefinition {
	return ToolDefinition{
		Type: "function",
		Function: ToolFunction{
			Name:        "list_proposals",
			Description: "List all pending proposals you have made. Use this to see what you've already proposed so you don't repeat yourself.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"status": {
						"type": "string",
						"description": "Filter by status: pending, approved, rejected, all (default: pending)",
						"enum": ["pending", "approved", "rejected", "all"]
					}
				}
			}`),
		},
	}
}

func newListProposalsFunc(root string) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
		var p struct {
			Status string `json:"status"`
		}
		if args != nil {
			json.Unmarshal(args, &p)
		}
		if p.Status == "" {
			p.Status = "pending"
		}

		dir := filepath.Join(root, proposalsDir)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				return json.Marshal(map[string]interface{}{"proposals": []string{}, "count": 0})
			}
			return nil, err
		}

		var proposals []map[string]string
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}

			// Quick read to check status
			content, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}

			contentStr := string(content)
			if p.Status != "all" && !strings.Contains(contentStr, "status: "+p.Status) {
				continue
			}

			// Extract title and type from frontmatter
			title := extractFMField(contentStr, "title")
			pType := extractFMField(contentStr, "type")
			urgency := extractFMField(contentStr, "urgency")
			created := extractFMField(contentStr, "created")

			proposals = append(proposals, map[string]string{
				"file":    e.Name(),
				"title":   title,
				"type":    pType,
				"urgency": urgency,
				"created": created,
			})
		}

		return json.Marshal(map[string]interface{}{
			"proposals": proposals,
			"count":     len(proposals),
		})
	}
}

// --- read_proposal ---

func readProposalDef() ToolDefinition {
	return ToolDefinition{
		Type: "function",
		Function: ToolFunction{
			Name:        "read_proposal",
			Description: "Read pending proposals. With no arguments, returns ALL pending proposals with full content. With a filename, returns that specific proposal.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"filename": {"type": "string", "description": "Optional: specific proposal filename. If omitted, returns all pending proposals."}
				}
			}`),
		},
	}
}

func newReadProposalFunc(root string) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
		var p struct {
			Filename string `json:"filename"`
		}
		if args != nil {
			json.Unmarshal(args, &p)
		}

		// If a specific filename is given, read just that one
		if p.Filename != "" {
			path := filepath.Join(root, proposalsDir, filepath.Base(p.Filename))
			content, err := os.ReadFile(path)
			if err != nil {
				return json.Marshal(map[string]string{"error": "not found: " + err.Error()})
			}
			return json.Marshal(map[string]string{"content": string(content)})
		}

		// No filename — return ALL pending proposals with full content
		dir := filepath.Join(root, proposalsDir)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				return json.Marshal(map[string]interface{}{"proposals": []string{}, "count": 0})
			}
			return nil, err
		}

		var proposals []map[string]string
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			content, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			contentStr := string(content)
			if !strings.Contains(contentStr, "status: pending") {
				continue
			}
			proposals = append(proposals, map[string]string{
				"file":    e.Name(),
				"content": contentStr,
			})
		}

		return json.Marshal(map[string]interface{}{
			"proposals": proposals,
			"count":     len(proposals),
		})
	}
}

// --- acknowledge_proposal ---

func acknowledgeProposalDef() ToolDefinition {
	return ToolDefinition{
		Type: "function",
		Function: ToolFunction{
			Name:        "acknowledge_proposal",
			Description: "Mark a proposal as processed. Use after reading and noting a proposal's content. Prevents re-reading on future cycles.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"filename": {
						"type": "string",
						"description": "The proposal filename to acknowledge"
					},
					"status": {
						"type": "string",
						"description": "New status for the proposal",
						"enum": ["acknowledged", "approved", "rejected"]
					},
					"note": {
						"type": "string",
						"description": "Optional note to append to the proposal"
					}
				},
				"required": ["filename", "status"]
			}`),
		},
	}
}

func newAcknowledgeProposalFunc(root string) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
		var p struct {
			Filename string `json:"filename"`
			Status   string `json:"status"`
			Note     string `json:"note"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, fmt.Errorf("parse args: %w", err)
		}
		if p.Filename == "" || p.Status == "" {
			return json.Marshal(map[string]string{"error": "filename and status are required"})
		}
		validStatus := map[string]bool{"acknowledged": true, "approved": true, "rejected": true}
		if !validStatus[p.Status] {
			return json.Marshal(map[string]string{"error": "status must be acknowledged, approved, or rejected"})
		}

		path := filepath.Join(root, proposalsDir, filepath.Base(p.Filename))
		content, err := os.ReadFile(path)
		if err != nil {
			return json.Marshal(map[string]string{"error": "not found: " + err.Error()})
		}

		contentStr := string(content)
		contentStr = strings.Replace(contentStr, "status: pending", "status: "+p.Status, 1)

		if p.Note != "" {
			contentStr += "\n\n## Operator Note\n" + p.Note + "\n"
		}

		if err := os.WriteFile(path, []byte(contentStr), 0o644); err != nil {
			return json.Marshal(map[string]string{"error": "write failed: " + err.Error()})
		}

		return json.Marshal(map[string]string{
			"status":  p.Status,
			"file":    p.Filename,
			"message": fmt.Sprintf("Proposal marked as %s.", p.Status),
		})
	}
}

// --- Helpers ---

// sanitizeFilename makes a string safe for use as a filename.
func sanitizeFilename(s string) string {
	s = strings.ToLower(s)
	s = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		if r == ' ' {
			return '-'
		}
		return -1
	}, s)
	if len(s) > 50 {
		s = s[:50]
	}
	return s
}

// extractFMField pulls a simple field value from YAML frontmatter.
func extractFMField(content, field string) string {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, field+":") {
			val := strings.TrimSpace(strings.TrimPrefix(line, field+":"))
			val = strings.Trim(val, `"`)
			return val
		}
	}
	return ""
}
