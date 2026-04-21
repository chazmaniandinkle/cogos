// tool_calls_query.go — Query layer for tool.call / tool.result ledger events.
//
// Agent S (survey-2026-04-21-agent-S-tool-bridge-design §4.4) specifies a
// call+result stitching view over the hash-chained ledger. The query layer is
// a thin wrapper: it scans .cog/ledger/{sessionID}/events.jsonl (optionally
// across all sessions), picks out tool.call and tool.result entries, pairs
// them by call_id, applies filters, and returns a first-class row shape.
//
// The stitched view is the ergonomic win: a caller asking "show me the 10 most
// recent tool invocations and did they succeed?" gets one row per call with
// both timestamps and the status already joined — no two-round-trip client-
// side pairing like the generic event-read surface would require.
//
// When upstream lands Agent L's QueryLedger, this file can become a thin
// wrapper over that API (Agent S §5.1). For now it reads the ledger JSONL
// files directly — the same pattern process.go and cogblock_ledger.go use.
package engine

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ToolCallQuery describes the filter set for QueryToolCalls. Each filter
// applies conjunctively; empty means no restriction.
type ToolCallQuery struct {
	SessionID     string    // exact session; empty = all sessions
	ToolName      string    // exact match, or "cog_*" / "*_read" wildcard
	Status        string    // "success" | "error" | "rejected" | "timeout" | "pending"
	Source        string    // "mcp" | "openai-chat" | "anthropic-messages" | "kernel-loop"
	Ownership     string    // "kernel" | "client"
	CallID        string    // exact single-call lookup
	Since         time.Time // lower bound (inclusive); zero = no lower bound
	Until         time.Time // upper bound (exclusive); zero = no upper bound
	Limit         int       // default 100, max 500
	Order         string    // "desc" (default) | "asc"
	IncludeArgs   bool      // include arguments payload (default: false)
	IncludeOutput bool      // include output_summary (default: false)
}

const (
	defaultToolCallQueryLimit = 100
	maxToolCallQueryLimit     = 500
)

// ToolCallRow is one row of the call+result stitched result set.
type ToolCallRow struct {
	CallID        string          `json:"call_id"`
	ToolName      string          `json:"tool_name"`
	SessionID     string          `json:"session_id"`
	Source        string          `json:"source"`
	Ownership     string          `json:"ownership"`
	CalledAt      string          `json:"called_at"`
	CompletedAt   string          `json:"completed_at,omitempty"`
	DurationMs    int             `json:"duration_ms"`
	Status        string          `json:"status"`
	Reason        string          `json:"reason,omitempty"`
	OutputLength  int             `json:"output_length"`
	Arguments     json.RawMessage `json:"arguments,omitempty"`
	OutputSummary string          `json:"output_summary,omitempty"`
	InteractionID string          `json:"interaction_id,omitempty"`
	TurnIndex     int             `json:"turn_index,omitempty"`
	Provider      string          `json:"provider,omitempty"`
}

// ToolCallSourceCounts captures per-source totals in the scanned window.
type ToolCallSourceCounts struct {
	MCP        int `json:"mcp"`
	OpenAI     int `json:"openai_chat"`
	Anthropic  int `json:"anthropic_messages"`
	KernelLoop int `json:"kernel_loop"`
	Other      int `json:"other"`
}

// ToolCallQueryResult is the return shape of QueryToolCalls.
type ToolCallQueryResult struct {
	Count          int                  `json:"count"`
	Calls          []ToolCallRow        `json:"calls"`
	Truncated      bool                 `json:"truncated"`
	SourcesChecked ToolCallSourceCounts `json:"sources_checked"`
}

// QueryToolCalls reads the ledger for one or all sessions, extracts
// tool.call/tool.result events, pairs them by call_id, applies q's filters,
// and returns the stitched row set.
//
// This is a read-only, stateless function — safe to call concurrently with
// AppendEvent calls (JSONL appends are line-atomic; partial lines are skipped).
func QueryToolCalls(workspaceRoot string, q ToolCallQuery) (*ToolCallQueryResult, error) {
	if workspaceRoot == "" {
		return nil, fmt.Errorf("workspace_root is required")
	}

	limit := q.Limit
	if limit <= 0 {
		limit = defaultToolCallQueryLimit
	}
	if limit > maxToolCallQueryLimit {
		limit = maxToolCallQueryLimit
	}

	// Collect sessions to scan.
	sessions, err := listLedgerSessions(workspaceRoot, q.SessionID)
	if err != nil {
		return nil, err
	}

	// Read every tool.*-typed event into memory. Capped per-session reads keep
	// memory bounded; for multi-session scans we reach back across histories.
	type toolEvent struct {
		eventType string
		timestamp time.Time
		data      map[string]interface{}
	}
	var events []toolEvent
	var counts ToolCallSourceCounts

	for _, sid := range sessions {
		eventsFile := filepath.Join(workspaceRoot, ".cog", "ledger", sid, "events.jsonl")
		f, err := os.Open(eventsFile)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("open ledger %s: %w", sid, err)
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 4<<20) // 4 MB line cap
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			var env EventEnvelope
			if jErr := json.Unmarshal([]byte(line), &env); jErr != nil {
				continue
			}
			t := env.HashedPayload.Type
			if t != "tool.call" && t != "tool.result" {
				continue
			}
			ts, _ := time.Parse(time.RFC3339, env.HashedPayload.Timestamp)
			ev := toolEvent{
				eventType: t,
				timestamp: ts,
				data:      env.HashedPayload.Data,
			}
			// Make sure the row carries the session id even if Data omits it.
			if env.HashedPayload.SessionID != "" {
				if ev.data == nil {
					ev.data = map[string]interface{}{}
				}
				if _, ok := ev.data["session_id"]; !ok {
					ev.data["session_id"] = env.HashedPayload.SessionID
				}
			}
			events = append(events, ev)
			if t == "tool.call" {
				incSourceCount(&counts, asString(ev.data["source"]))
			}
		}
		_ = f.Close()
	}

	// Stitch: for each tool.call, attach the matching tool.result (if any).
	// Rows keyed by call_id. Late-arriving duplicate calls overwrite earlier
	// ones — the ledger is append-only so the latest tool.call for a call_id
	// is authoritative.
	rows := make(map[string]*ToolCallRow)
	for _, ev := range events {
		if ev.eventType != "tool.call" {
			continue
		}
		callID := asString(ev.data["call_id"])
		if callID == "" {
			continue
		}
		row := &ToolCallRow{
			CallID:        callID,
			ToolName:      asString(ev.data["tool_name"]),
			SessionID:     asString(ev.data["session_id"]),
			Source:        asString(ev.data["source"]),
			Ownership:     asString(ev.data["ownership"]),
			CalledAt:      ev.timestamp.Format(time.RFC3339),
			Status:        ToolStatusPending,
			Provider:      asString(ev.data["provider"]),
			InteractionID: asString(ev.data["interaction_id"]),
			TurnIndex:     asInt(ev.data["turn_index"]),
		}
		if q.IncludeArgs {
			row.Arguments = rawFromAny(ev.data["arguments"])
		}
		rows[callID] = row
	}
	for _, ev := range events {
		if ev.eventType != "tool.result" {
			continue
		}
		callID := asString(ev.data["call_id"])
		row, ok := rows[callID]
		if !ok {
			// Result without matching call — synthesize a placeholder row so
			// the caller can still see it when filtering by call_id.
			row = &ToolCallRow{
				CallID:    callID,
				ToolName:  asString(ev.data["tool_name"]),
				SessionID: asString(ev.data["session_id"]),
				Source:    asString(ev.data["source"]),
				Status:    ToolStatusPending,
			}
			rows[callID] = row
		}
		row.Status = asString(ev.data["status"])
		row.Reason = asString(ev.data["reason"])
		row.OutputLength = asInt(ev.data["output_length"])
		row.DurationMs = asInt(ev.data["duration_ms"])
		row.CompletedAt = ev.timestamp.Format(time.RFC3339)
		if q.IncludeOutput {
			row.OutputSummary = asString(ev.data["output_summary"])
		}
		// Prefer the result's tool_name if the call row didn't have one
		// (shouldn't happen in practice, but keeps the data self-healing).
		if row.ToolName == "" {
			row.ToolName = asString(ev.data["tool_name"])
		}
	}

	// Flatten, filter, sort.
	flat := make([]ToolCallRow, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		if !rowMatchesQuery(*r, q) {
			continue
		}
		flat = append(flat, *r)
	}
	order := strings.ToLower(q.Order)
	if order != "asc" {
		order = "desc"
	}
	sort.SliceStable(flat, func(i, j int) bool {
		ti, _ := time.Parse(time.RFC3339, flat[i].CalledAt)
		tj, _ := time.Parse(time.RFC3339, flat[j].CalledAt)
		if order == "asc" {
			return ti.Before(tj)
		}
		return ti.After(tj)
	})

	truncated := len(flat) > limit
	if truncated {
		flat = flat[:limit]
	}

	return &ToolCallQueryResult{
		Count:          len(flat),
		Calls:          flat,
		Truncated:      truncated,
		SourcesChecked: counts,
	}, nil
}

// rowMatchesQuery applies the non-limit filters in ToolCallQuery to one row.
func rowMatchesQuery(r ToolCallRow, q ToolCallQuery) bool {
	if q.CallID != "" && r.CallID != q.CallID {
		return false
	}
	if q.ToolName != "" && !toolNameMatches(r.ToolName, q.ToolName) {
		return false
	}
	if q.Status != "" && r.Status != q.Status {
		return false
	}
	if q.Source != "" && r.Source != q.Source {
		return false
	}
	if q.Ownership != "" && r.Ownership != q.Ownership {
		return false
	}
	if !q.Since.IsZero() {
		ts, _ := time.Parse(time.RFC3339, r.CalledAt)
		if ts.Before(q.Since) {
			return false
		}
	}
	if !q.Until.IsZero() {
		ts, _ := time.Parse(time.RFC3339, r.CalledAt)
		if !ts.Before(q.Until) {
			return false
		}
	}
	return true
}

// toolNameMatches implements simple "*"-wildcard match on tool_name.
// Wildcards allowed at prefix, suffix, or both (e.g. "cog_read_*", "*_cogdoc",
// "*search*"). Non-wildcard patterns match exactly.
func toolNameMatches(name, pattern string) bool {
	if pattern == "" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return name == pattern
	}
	// Split on '*' and require each segment appear in order.
	segs := strings.Split(pattern, "*")
	idx := 0
	for i, seg := range segs {
		if seg == "" {
			continue
		}
		if i == 0 && !strings.HasPrefix(name[idx:], seg) {
			return false
		}
		pos := strings.Index(name[idx:], seg)
		if pos < 0 {
			return false
		}
		idx += pos + len(seg)
	}
	// If pattern didn't end with *, require suffix match.
	if !strings.HasSuffix(pattern, "*") && segs[len(segs)-1] != "" {
		return strings.HasSuffix(name, segs[len(segs)-1])
	}
	return true
}

// incSourceCount bumps the matching bucket in ToolCallSourceCounts.
func incSourceCount(c *ToolCallSourceCounts, source string) {
	switch source {
	case ToolSourceMCP:
		c.MCP++
	case ToolSourceOpenAI:
		c.OpenAI++
	case ToolSourceAnthropic:
		c.Anthropic++
	case ToolSourceKernelLoop:
		c.KernelLoop++
	default:
		c.Other++
	}
}

// ── JSON helpers ────────────────────────────────────────────────────────────

// asString returns v coerced to string, or "" when v is nil or not a string.
func asString(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		return ""
	}
}

// asInt returns v coerced to int, or 0 when v is nil / not a number.
// JSON unmarshals numbers to float64 by default; we accept both.
func asInt(v interface{}) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return int(i)
		}
	}
	return 0
}

// rawFromAny returns a json.RawMessage representation of v, or nil when empty.
// Used to surface the arguments blob opt-in on reads without pulling the full
// interface-typed value into the row shape.
func rawFromAny(v interface{}) json.RawMessage {
	if v == nil {
		return nil
	}
	switch x := v.(type) {
	case json.RawMessage:
		if len(x) == 0 {
			return nil
		}
		return x
	case string:
		if x == "" {
			return nil
		}
		return json.RawMessage(x)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return nil
		}
		return b
	}
}
