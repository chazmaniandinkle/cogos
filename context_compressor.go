// context_compressor.go — Budget-constrained context window construction.
//
// The ContextCompressor takes a normalized ThreadView, session state, and TAA
// profile, then builds a ContextWindow that fits within the token budget.
//
// Three strategies:
//   - "resume" (short): Claude has the history, just send the new message
//   - "resume" (long):  New message + compressed context reminder
//   - "fresh":          Full reconstructed context (working memory + history + recent + message)

package main

import (
	"fmt"
	"strings"
)

// ContextCompressor builds budget-constrained context windows from ThreadViews.
type ContextCompressor struct {
	workspaceRoot string
}

// NewContextCompressor creates a ContextCompressor instance.
func NewContextCompressor(workspaceRoot string) *ContextCompressor {
	return &ContextCompressor{workspaceRoot: workspaceRoot}
}

// Compress builds a budget-constrained ContextWindow from a ThreadView and session state.
func (c *ContextCompressor) Compress(view *ThreadView, session *SessionState, profile *TAAProfile) (*ContextWindow, error) {
	if view == nil {
		return nil, fmt.Errorf("nil ThreadView")
	}

	budget := budgetFromProfile(profile)

	hasClaudeSession := session != nil && session.ClaudeSessionID != ""
	isShortThread := len(view.Messages) < 20

	if hasClaudeSession && isShortThread {
		// Resume, short thread — Claude has the history, just send last message.
		return &ContextWindow{
			SystemPrompt:  view.SystemPrompt,
			Prompt:        view.LastUserMsg,
			TotalTokens:   estimateTokens(view.LastUserMsg + view.SystemPrompt),
			Strategy:      "resume",
			ClaudeSession: session.ClaudeSessionID,
		}, nil
	}

	if hasClaudeSession {
		// Resume, long thread — last message + context reminder of recent exchanges.
		reminder := buildContextReminder(view.Messages, budget.RecentContext)
		prompt := view.LastUserMsg
		if reminder != "" {
			prompt = prompt + "\n" + reminder
		}
		return &ContextWindow{
			SystemPrompt:  view.SystemPrompt,
			Prompt:        prompt,
			TotalTokens:   estimateTokens(prompt + view.SystemPrompt),
			Strategy:      "resume",
			ClaudeSession: session.ClaudeSessionID,
		}, nil
	}

	// Fresh session — build full context window.
	var parts []string

	// Working memory block.
	if session != nil && session.WorkingMemory != nil {
		wmBlock := buildWorkingMemoryBlock(session.WorkingMemory, budget.WorkingMemory)
		if wmBlock != "" {
			parts = append(parts, wmBlock)
		}
	}

	// Split messages into older (compressed) and recent (verbatim).
	recentCount := 10
	if len(view.Messages) < recentCount*2 {
		recentCount = len(view.Messages) / 2
	}

	olderMsgs := view.Messages
	var recentMsgs []ThreadMessage
	if len(view.Messages) > recentCount {
		olderMsgs = view.Messages[:len(view.Messages)-recentCount]
		recentMsgs = view.Messages[len(view.Messages)-recentCount:]
	}

	// Compressed history of older exchanges.
	if len(olderMsgs) > 0 {
		histBlock := compressHistory(olderMsgs, budget.CompressedHistory)
		if histBlock != "" {
			parts = append(parts, histBlock)
		}
	}

	// Recent context verbatim.
	if len(recentMsgs) > 0 {
		recentBlock := buildRecentContext(recentMsgs, budget.RecentContext)
		if recentBlock != "" {
			parts = append(parts, recentBlock)
		}
	}

	// Current message.
	parts = append(parts, view.LastUserMsg)

	prompt := strings.Join(parts, "\n\n---\n\n")

	return &ContextWindow{
		SystemPrompt:  view.SystemPrompt,
		Prompt:        prompt,
		TotalTokens:   estimateTokens(prompt + view.SystemPrompt),
		Strategy:      "fresh",
		ClaudeSession: "",
	}, nil
}

// budgetFromProfile derives a CompressorBudget from a TAAProfile.
func budgetFromProfile(profile *TAAProfile) CompressorBudget {
	if profile == nil {
		return DefaultCompressorBudget()
	}
	total := profile.Tiers.TotalTokens
	if total <= 0 {
		total = 15000
	}
	return CompressorBudget{
		Total:             total,
		WorkingMemory:     total * 13 / 100,
		CompressedHistory: total * 20 / 100,
		RecentContext:     total * 54 / 100,
		CurrentMessage:    total * 13 / 100,
	}
}

// compressMessage compresses a single message to fit within maxTokens.
func compressMessage(msg ThreadMessage, maxTokens int) string {
	content := msg.Content
	if estimateTokens(content) <= maxTokens {
		return content
	}
	// Extract first sentence.
	first := firstSentence(content)
	return first + " [... truncated]"
}

// buildWorkingMemoryBlock formats working memory into a markdown block.
func buildWorkingMemoryBlock(wm *WorkingMemory, budget int) string {
	if wm == nil {
		return ""
	}

	var lines []string
	lines = append(lines, "## Working Memory")

	if len(wm.ActiveTopics) > 0 {
		lines = append(lines, "**Topics:** "+strings.Join(wm.ActiveTopics, ", "))
	}
	if len(wm.KeyDecisions) > 0 {
		lines = append(lines, "**Decisions:** "+strings.Join(wm.KeyDecisions, "; "))
	}
	if len(wm.ActiveArtifacts) > 0 {
		lines = append(lines, "**Artifacts:** "+strings.Join(wm.ActiveArtifacts, ", "))
	}
	if wm.Summary != "" {
		lines = append(lines, "**Summary:** "+wm.Summary)
	}

	block := strings.Join(lines, "\n")

	// Truncate if over budget.
	if budget > 0 && estimateTokens(block) > budget {
		maxChars := budget * 4
		if maxChars < len(block) {
			block = block[:maxChars] + "\n[... working memory truncated]"
		}
	}

	return block
}

// compressHistory compresses older exchanges into a summary block.
func compressHistory(messages []ThreadMessage, budget int) string {
	if len(messages) == 0 || budget <= 0 {
		return ""
	}

	var lines []string
	lines = append(lines, "## Earlier Context")

	tokenCount := estimateTokens(lines[0])

	for _, msg := range messages {
		var line string
		switch msg.Role {
		case "user":
			first := firstSentence(msg.Content)
			// Include code blocks for user messages if present.
			codeBlock := extractCodeBlock(msg.Content)
			if codeBlock != "" {
				line = fmt.Sprintf("[user] %s\n%s", first, codeBlock)
			} else {
				line = fmt.Sprintf("[user] %s", first)
			}
		case "assistant":
			line = fmt.Sprintf("[assistant] %s", firstSentence(msg.Content))
		default:
			continue
		}

		lineTokens := estimateTokens(line)
		if tokenCount+lineTokens > budget {
			break
		}
		lines = append(lines, line)
		tokenCount += lineTokens
	}

	if len(lines) <= 1 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// buildRecentContext formats recent messages verbatim within budget.
func buildRecentContext(messages []ThreadMessage, budget int) string {
	if len(messages) == 0 || budget <= 0 {
		return ""
	}

	var lines []string
	lines = append(lines, "## Recent Conversation")
	tokenCount := estimateTokens(lines[0])

	// Work backwards to prioritize most recent messages.
	var selected []string
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		var line string
		switch msg.Role {
		case "user":
			line = "**User:** " + msg.Content
		case "assistant":
			line = "**Assistant:** " + msg.Content
		default:
			line = "**" + msg.Role + ":** " + msg.Content
		}

		lineTokens := estimateTokens(line)
		if tokenCount+lineTokens > budget {
			break
		}
		selected = append([]string{line}, selected...)
		tokenCount += lineTokens
	}

	if len(selected) == 0 {
		return ""
	}
	lines = append(lines, selected...)
	return strings.Join(lines, "\n")
}

// buildContextReminder builds a brief reminder of recent exchanges for resume+long mode.
func buildContextReminder(messages []ThreadMessage, budget int) string {
	if len(messages) == 0 {
		return ""
	}

	// Take last 10 messages (up to 5 exchanges).
	start := len(messages) - 10
	if start < 0 {
		start = 0
	}
	recent := messages[start:]

	var summaries []string
	tokenCount := 0
	maxTokens := budget / 4 // Use only a fraction of the budget for the reminder.
	if maxTokens <= 0 {
		maxTokens = 500
	}

	for _, msg := range recent {
		if msg.Role != "user" && msg.Role != "assistant" {
			continue
		}
		summary := "- " + firstSentence(msg.Content)
		tokens := estimateTokens(summary)
		if tokenCount+tokens > maxTokens {
			break
		}
		summaries = append(summaries, summary)
		tokenCount += tokens
	}

	if len(summaries) == 0 {
		return ""
	}

	return "\n---\n[Context reminder - recent topics discussed:]\n" + strings.Join(summaries, "\n")
}

// firstSentence extracts the first sentence from text.
func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	// Look for sentence-ending punctuation.
	for i, ch := range s {
		if ch == '.' || ch == '!' || ch == '?' {
			// Make sure it's not an abbreviation (e.g., "e.g.") by checking
			// if the next char is a space or end of string.
			if i+1 >= len(s) || s[i+1] == ' ' || s[i+1] == '\n' {
				return s[:i+1]
			}
		}
		// Also stop at newlines for multi-paragraph messages.
		if ch == '\n' {
			result := strings.TrimSpace(s[:i])
			if result != "" {
				return result
			}
		}
	}

	// No sentence boundary found — return up to 200 chars.
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

// extractCodeBlock extracts the first fenced code block from text, if any.
func extractCodeBlock(s string) string {
	start := strings.Index(s, "```")
	if start < 0 {
		return ""
	}
	end := strings.Index(s[start+3:], "```")
	if end < 0 {
		return ""
	}
	return s[start : start+3+end+3]
}
