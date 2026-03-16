// thread_parser.go — Track A, Wave 1: Thread Parser for the CogOS Context Engine.
//
// Normalizes, deduplicates, and strips metadata from incoming OpenClaw threads.
// OpenClaw sends the full conversation as []ChatMessage on every request.
// The parser extracts structured metadata, cleans content, and produces a ThreadView.

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
)

// ThreadParser normalizes incoming OpenClaw message threads into ThreadViews.
type ThreadParser struct{}

// NewThreadParser creates a ThreadParser instance.
func NewThreadParser() *ThreadParser {
	return &ThreadParser{}
}

// Parse normalizes an OpenClaw message thread into a ThreadView.
// It extracts system prompts, parses metadata, deduplicates messages,
// and identifies the last user message.
func (tp *ThreadParser) Parse(messages []ChatMessage, headers RequestHeaders) (*ThreadView, error) {
	view := &ThreadView{
		Origin: tp.detectOrigin(headers),
	}

	var systemParts []string
	seenIDs := make(map[string]bool)
	seenHashes := make(map[string]bool)
	seenContent := make(map[string]bool) // for starter echo dedup

	for _, msg := range messages {
		rawContent := msg.GetContent()

		// System messages get concatenated into SystemPrompt.
		if msg.Role == "system" {
			systemParts = append(systemParts, rawContent)
			continue
		}

		// Parse the message into a ThreadMessage.
		tm := tp.parseMessage(msg, rawContent)

		// Dedup by message ID.
		if tm.ID != "" {
			if seenIDs[tm.ID] {
				continue
			}
			seenIDs[tm.ID] = true
		}

		// Dedup by content hash for messages without IDs.
		if tm.ID == "" && tm.Content != "" {
			h := contentHash(tm.Content)
			if seenHashes[h] {
				continue
			}
			seenHashes[h] = true
		}

		// Thread starter echo dedup: if a starter duplicates earlier content, drop it.
		if tm.IsStarter {
			if seenContent[tm.Content] {
				continue
			}
		}

		// Track content for future starter-echo checks.
		if tm.Content != "" {
			seenContent[tm.Content] = true
		}

		// Token estimation: ~4 chars per token.
		tm.Tokens = len(tm.Content) / 4
		if tm.Tokens == 0 && len(tm.Content) > 0 {
			tm.Tokens = 1
		}

		view.Messages = append(view.Messages, tm)
	}

	view.SystemPrompt = strings.Join(systemParts, "\n")

	// Find last user message (non-starter, non-system).
	for i := len(view.Messages) - 1; i >= 0; i-- {
		m := view.Messages[i]
		if m.Role == "user" && !m.IsStarter {
			view.LastUserMsg = m.Content
			break
		}
	}

	// Count distinct user turns (non-starter).
	for _, m := range view.Messages {
		if m.Role == "user" && !m.IsStarter {
			view.TurnCount++
		}
	}

	return view, nil
}

// Regex patterns for OpenClaw metadata blocks.
var (
	// Matches "Conversation info (untrusted metadata):\n```json\n{...}\n```"
	reConversationInfo = regexp.MustCompile(`(?s)Conversation info \(untrusted metadata\):\s*` + "```json\n" + `(.*?)` + "\n```")

	// Matches "Sender (untrusted metadata):\n```json\n{...}\n```"
	reSenderInfo = regexp.MustCompile(`(?s)Sender \(untrusted metadata\):\s*` + "```json\n" + `(.*?)` + "\n```")

	// Matches <<<EXTERNAL_UNTRUSTED_CONTENT>>> blocks
	reExternalBlock = regexp.MustCompile(`(?s)<<<EXTERNAL_UNTRUSTED_CONTENT>>>(.*?)<<<EXTERNAL_UNTRUSTED_CONTENT>>>`)

	// Matches the thread starter prefix
	reThreadStarter = regexp.MustCompile(`^\[Thread starter - for context\]\s*`)

	// Matches [[reply_to_current]] prefix in assistant messages
	reReplyPrefix = regexp.MustCompile(`^\[\[reply_to_current\]\]\s*`)
)

// conversationMeta holds parsed conversation info metadata.
type conversationMeta struct {
	MessageID         string `json:"message_id"`
	SenderID          string `json:"sender_id"`
	ConversationLabel string `json:"conversation_label"`
	Sender            string `json:"sender"`
	GroupSubject      string `json:"group_subject"`
}

// senderMeta holds parsed sender metadata.
type senderMeta struct {
	Label    string `json:"label"`
	Name     string `json:"name"`
	Username string `json:"username"`
	Tag      string `json:"tag"`
}

// parseMessage extracts metadata and cleans content from a single message.
func (tp *ThreadParser) parseMessage(msg ChatMessage, rawContent string) ThreadMessage {
	tm := ThreadMessage{
		Role:       msg.Role,
		RawContent: rawContent,
		Metadata:   make(map[string]any),
	}

	content := rawContent

	// Detect thread starter.
	if reThreadStarter.MatchString(content) {
		tm.IsStarter = true
		content = reThreadStarter.ReplaceAllString(content, "")
	}

	// Extract conversation info metadata.
	if match := reConversationInfo.FindStringSubmatch(content); len(match) > 1 {
		var meta conversationMeta
		if err := json.Unmarshal([]byte(match[1]), &meta); err == nil {
			tm.ID = meta.MessageID
			if tm.SenderID == "" {
				tm.SenderID = meta.SenderID
			}
			// Store extra metadata.
			if meta.ConversationLabel != "" {
				tm.Metadata["conversation_label"] = meta.ConversationLabel
			}
			if meta.GroupSubject != "" {
				tm.Metadata["group_subject"] = meta.GroupSubject
			}
		}
		content = reConversationInfo.ReplaceAllString(content, "")
	}

	// Extract sender metadata.
	if match := reSenderInfo.FindStringSubmatch(content); len(match) > 1 {
		var meta senderMeta
		if err := json.Unmarshal([]byte(match[1]), &meta); err == nil {
			tm.Sender = meta.Name
			if tm.Sender == "" {
				tm.Sender = meta.Label
			}
			if meta.Username != "" {
				tm.Metadata["username"] = meta.Username
			}
		}
		content = reSenderInfo.ReplaceAllString(content, "")
	}

	// Strip <<<EXTERNAL_UNTRUSTED_CONTENT>>> blocks.
	content = reExternalBlock.ReplaceAllString(content, "")

	// Strip [[reply_to_current]] prefix from assistant messages.
	if msg.Role == "assistant" {
		content = reReplyPrefix.ReplaceAllString(content, "")
	}

	// Preserve tool call info in metadata.
	if msg.ToolCallID != "" {
		tm.Metadata["tool_call_id"] = msg.ToolCallID
	}
	if len(msg.ToolCalls) > 0 {
		tm.Metadata["has_tool_calls"] = true
	}

	tm.Content = strings.TrimSpace(content)
	return tm
}

// detectOrigin determines the message origin from headers or defaults to "http".
func (tp *ThreadParser) detectOrigin(headers RequestHeaders) string {
	if headers.Origin != "" {
		origin := strings.ToLower(headers.Origin)
		switch {
		case strings.Contains(origin, "discord"):
			return "discord"
		case strings.Contains(origin, "tui"):
			return "tui"
		default:
			return origin
		}
	}
	return "http"
}

// contentHash returns a short SHA-256 hash of message content for dedup.
func contentHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:8])
}
