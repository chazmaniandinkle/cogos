// bus_event_format.go — One-line bus event formatters used by the CLI
// bus watch output.
//
// Previously lived in event_discord_bridge.go; extracted into its own file
// when the Discord bridge was deleted in Track 5. bus_watch.go still uses
// formatBusEvent for its default "line" output mode.

package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// formatBusEvent creates a one-line summary for a bus event.
// Format: [HH:MM:SS] emoji eventType | context | summary
// Returns empty string for event types we don't format.
func formatBusEvent(evt *CogBlock) string {
	if evt == nil {
		return ""
	}

	ts := formatEventTimestamp(evt.Ts)

	switch evt.Type {
	case BlockChatRequest:
		// Prefer agent field (from UCP identity) over origin for display
		agent := extractPayloadString(evt.Payload, "agent", "")
		if agent == "" {
			agent = extractPayloadString(evt.Payload, "origin", evt.From)
		}
		content := extractPayloadString(evt.Payload, "content", "")
		content = sanitizeAndTruncate(content, 50)
		return fmt.Sprintf("[%s] \U0001F504 chat.request | %s | \"%s\"", ts, agent, content)

	case BlockChatResponse:
		agent := evt.From
		tokens := extractPayloadInt(evt.Payload, "tokens_used")
		duration := extractPayloadInt64(evt.Payload, "duration_ms")
		parts := []string{}
		if tokens > 0 {
			parts = append(parts, fmt.Sprintf("%d tokens", tokens))
		}
		if duration > 0 {
			parts = append(parts, fmt.Sprintf("%dms", duration))
		}
		// Show cache hit tokens when present
		cacheRead := extractPayloadInt(evt.Payload, "cache_read_tokens")
		if cacheRead > 0 {
			parts = append(parts, fmt.Sprintf("cache:%d", cacheRead))
		}
		// Show finish reason when non-default
		finishReason := extractPayloadString(evt.Payload, "finish_reason", "")
		if finishReason != "" && finishReason != "stop" {
			parts = append(parts, finishReason)
		}
		summary := strings.Join(parts, ", ")
		if summary == "" {
			summary = "completed"
		}
		return fmt.Sprintf("[%s] \u2705 chat.response | %s | %s", ts, agent, summary)

	case BlockChatError:
		agent := evt.From
		errType := extractPayloadString(evt.Payload, "error_type", "")
		errMsg := extractPayloadString(evt.Payload, "error", "unknown error")
		errMsg = sanitizeAndTruncate(errMsg, 80)
		if errType != "" {
			return fmt.Sprintf("[%s] \u274C chat.error | %s | [%s] %s", ts, agent, errType, errMsg)
		}
		return fmt.Sprintf("[%s] \u274C chat.error | %s | %s", ts, agent, errMsg)

	case BlockToolInvoke:
		caller := extractPayloadString(evt.Payload, "callerAgent", evt.From)
		target := extractPayloadString(evt.Payload, "targetAgent", "any")
		tool := extractPayloadString(evt.Payload, "tool", "unknown")
		return fmt.Sprintf("[%s] \U0001F6E0\uFE0F tool.invoke | %s \u2192 %s | %s", ts, caller, target, tool)

	case BlockToolResult:
		executor := extractPayloadString(evt.Payload, "executedBy", evt.From)
		tool := extractPayloadString(evt.Payload, "tool", "")
		duration := extractPayloadInt64(evt.Payload, "durationMs")
		summary := tool
		if duration > 0 {
			summary = fmt.Sprintf("%s (%dms)", tool, duration)
		}
		if summary == "" {
			summary = "completed"
		}
		return fmt.Sprintf("[%s] \U0001F4E6 tool.result | %s | %s", ts, executor, summary)

	case BlockAgentCapabilities:
		agentID := extractPayloadString(evt.Payload, "agentId", evt.From)
		toolCount := countCapabilityTools(evt.Payload)
		return fmt.Sprintf("[%s] \U0001F4E1 agent.capabilities | %s | %d tools", ts, agentID, toolCount)

	case BlockSystemStartup:
		shortHash := evt.Hash
		if len(shortHash) > 8 {
			shortHash = shortHash[:8]
		}
		return fmt.Sprintf("[%s] \U0001F7E2 system.startup | %s | %s", ts, evt.From, shortHash)

	case BlockSystemShutdown:
		return fmt.Sprintf("[%s] \U0001F534 system.shutdown | %s", ts, evt.From)

	case BlockSystemHealth:
		return fmt.Sprintf("[%s] \U0001F49A system.health | %s", ts, evt.From)

	default:
		// Channel messages bridged from OpenClaw (e.g. "discord.message", "telegram.message")
		if strings.HasSuffix(evt.Type, BlockChannelMessageSuffix) && evt.Type != BlockMessage {
			sender := extractPayloadString(evt.Payload, "username", "")
			if sender == "" {
				sender = extractPayloadString(evt.Payload, "from", evt.From)
			}
			channel := extractPayloadString(evt.Payload, "channel_name", "")
			if channel == "" {
				channel = extractPayloadString(evt.Payload, "channel", "")
			}
			content := extractPayloadString(evt.Payload, "content", "")
			content = sanitizeAndTruncate(content, 60)
			prefix := evt.Type[:len(evt.Type)-len(BlockChannelMessageSuffix)]
			if channel != "" {
				return fmt.Sprintf("[%s] \U0001F4AC %s.message | #%s | %s: \"%s\"", ts, prefix, channel, sender, content)
			}
			return fmt.Sprintf("[%s] \U0001F4AC %s.message | %s | \"%s\"", ts, prefix, sender, content)
		}

		// Unknown event type — emit a generic line
		return fmt.Sprintf("[%s] \u2022 %s | %s", ts, evt.Type, evt.From)
	}
}

// --- Helper functions ---

// formatEventTimestamp extracts HH:MM:SS from an RFC3339 timestamp.
func formatEventTimestamp(ts string) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		// Fall back to raw timestamp or current time
		t, err = time.Parse(time.RFC3339, ts)
		if err != nil {
			return time.Now().UTC().Format("15:04:05")
		}
	}
	return t.Format("15:04:05")
}

// extractPayloadString extracts a string value from a payload map with a fallback.
func extractPayloadString(payload map[string]interface{}, key, fallback string) string {
	if payload == nil {
		return fallback
	}
	if v, ok := payload[key]; ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return fallback
}

// extractPayloadInt extracts an integer value from a payload map.
func extractPayloadInt(payload map[string]interface{}, key string) int {
	if payload == nil {
		return 0
	}
	v, ok := payload[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

// extractPayloadInt64 extracts an int64 value from a payload map.
func extractPayloadInt64(payload map[string]interface{}, key string) int64 {
	if payload == nil {
		return 0
	}
	v, ok := payload[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

// countCapabilityTools counts the number of allowed tools in a capabilities payload.
func countCapabilityTools(payload map[string]interface{}) int {
	if payload == nil {
		return 0
	}
	tools, ok := payload["tools"]
	if !ok {
		return 0
	}
	toolsMap, ok := tools.(map[string]interface{})
	if !ok {
		return 0
	}
	allow, ok := toolsMap["allow"]
	if !ok {
		return 0
	}
	allowSlice, ok := allow.([]interface{})
	if !ok {
		return 0
	}
	return len(allowSlice)
}

// sanitizeAndTruncate strips newlines (for single-line display) then shortens
// to maxLen characters, appending "..." if truncated.
func sanitizeAndTruncate(s string, maxLen int) string {
	// Remove newlines for single-line display
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")

	return truncate(s, maxLen)
}
