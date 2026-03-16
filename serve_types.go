package main

import (
	"encoding/json"
	"strings"
)

// serve_types.go — OpenAI-compatible API types, Claude CLI types, and streaming types

// === OPENAI API TYPES ===

// ChatCompletionRequest represents an OpenAI-format chat completion request
type ChatCompletionRequest struct {
	Model          string          `json:"model"`
	Messages       []ChatMessage   `json:"messages"`
	Stream         bool            `json:"stream,omitempty"`
	Temperature    *float64        `json:"temperature,omitempty"`
	MaxTokens      *int            `json:"max_tokens,omitempty"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
	SystemPrompt   string          `json:"system_prompt,omitempty"` // Extension for explicit system
	TAA            json.RawMessage `json:"taa,omitempty"`           // TAA context: false/absent=none, true=default, "name"=profile
	Tools          []json.RawMessage `json:"tools,omitempty"`       // OpenAI-format tool definitions
}

// GetTAAProfile parses the TAA field and returns the profile name to use.
// Returns: ("", false) for no TAA, ("default", true) for taa:true, ("name", true) for taa:"name"
func (r *ChatCompletionRequest) GetTAAProfile() (string, bool) {
	if len(r.TAA) == 0 {
		return "", false
	}

	// Try parsing as boolean
	var boolVal bool
	if err := json.Unmarshal(r.TAA, &boolVal); err == nil {
		if boolVal {
			return "default", true
		}
		return "", false
	}

	// Try parsing as string (profile name)
	var strVal string
	if err := json.Unmarshal(r.TAA, &strVal); err == nil && strVal != "" {
		return strVal, true
	}

	return "", false
}

// GetTAAProfileWithHeader checks both the request body TAA field and the X-TAA-Profile header.
// Header takes precedence over body field if both are present.
func (r *ChatCompletionRequest) GetTAAProfileWithHeader(header string) (string, bool) {
	// Header takes precedence
	if header != "" {
		return header, true
	}
	// Fall back to body field
	return r.GetTAAProfile()
}

// ChatMessage represents a single message in the conversation
// Content can be either a string or an array of content parts (OpenAI SDK format)
type ChatMessage struct {
	Role       string          `json:"role,omitempty"`
	Content    json.RawMessage `json:"content"`               // Can be string or array
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`  // Assistant tool call requests
	ToolCallID string          `json:"tool_call_id,omitempty"` // For role:"tool" — which call this answers
}

// GetContent extracts the text content from a ChatMessage
// Handles both string format and array-of-parts format (OpenAI SDK)
func (m *ChatMessage) GetContent() string {
	if len(m.Content) == 0 {
		return ""
	}

	// Try to unmarshal as a simple string first
	var strContent string
	if err := json.Unmarshal(m.Content, &strContent); err == nil {
		return strContent
	}

	// Try to unmarshal as an array of content parts
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		var result strings.Builder
		for _, part := range parts {
			if part.Type == "text" || part.Type == "" {
				result.WriteString(part.Text)
			}
		}
		return result.String()
	}

	// Fallback: return raw content as string
	return string(m.Content)
}

// GetToolCallsSummary extracts a text summary of tool calls from an assistant message.
// Returns empty string if no tool calls are present.
func (m *ChatMessage) GetToolCallsSummary() string {
	if len(m.ToolCalls) == 0 {
		return ""
	}
	var calls []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(m.ToolCalls, &calls); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, call := range calls {
		if call.Function.Name != "" {
			sb.WriteString("[Tool call: ")
			sb.WriteString(call.Function.Name)
			// Include a truncated version of args for context
			args := call.Function.Arguments
			if len(args) > 200 {
				args = args[:200] + "..."
			}
			if args != "" {
				sb.WriteString("(")
				sb.WriteString(args)
				sb.WriteString(")")
			}
			sb.WriteString("]")
		}
	}
	return sb.String()
}

// StringToRawContent converts a string to json.RawMessage for ChatMessage.Content
func StringToRawContent(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return json.RawMessage(b)
}

// ResponseFormat for JSON schema responses
type ResponseFormat struct {
	Type       string          `json:"type,omitempty"`
	JSONSchema json.RawMessage `json:"json_schema,omitempty"`
}

// ChatCompletionResponse represents the non-streaming response
type ChatCompletionResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   *UsageInfo   `json:"usage,omitempty"`
}

// ChatChoice represents a single completion choice
type ChatChoice struct {
	Index        int          `json:"index"`
	Message      *ChatMessage `json:"message,omitempty"`
	Delta        *ChatMessage `json:"delta,omitempty"`
	FinishReason string       `json:"finish_reason,omitempty"`
}


// UsageInfo represents token usage in the OpenAI-compatible response format.
// The cache and cost fields are Anthropic extensions — they're omitted for
// non-Claude providers, so standard OpenAI clients ignore them gracefully.
type UsageInfo struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`

	// Anthropic extensions (omitted when zero)
	CacheReadTokens   int     `json:"cache_read_input_tokens,omitempty"`
	CacheCreateTokens int     `json:"cache_creation_input_tokens,omitempty"`
	CostUSD           float64 `json:"cost_usd,omitempty"`
}

// StreamChunk represents a streaming response chunk
type StreamChunk struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   *UsageInfo   `json:"usage,omitempty"`
}

// ModelListResponse represents the /v1/models response
type ModelListResponse struct {
	Object string      `json:"object"`
	Data   []ModelInfo `json:"data"`
}

// ModelInfo represents a single model
type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// === PROVIDER API TYPES (ADR-046) ===

// ProviderHealth represents the health status of a provider
type ProviderHealth struct {
	LastCheck *string `json:"last_check"` // ISO8601 timestamp or null
	LatencyMs *int    `json:"latency_ms"` // Latency in ms or null
	Error     *string `json:"error"`      // Error message or null
}

// ProviderInfo represents a single provider in the API response
type ProviderInfo struct {
	ID     string               `json:"id"`
	Name   string               `json:"name"`
	Status string               `json:"status"` // "online", "offline", "unknown", "degraded"
	Active bool                 `json:"active"`
	Models []string             `json:"models"`
	Config ProviderPublicConfig `json:"config"`
	Health ProviderHealth       `json:"health"`
}

// ProviderPublicConfig represents publicly-visible provider configuration
// Note: API keys are never exposed, only has_api_key boolean
type ProviderPublicConfig struct {
	BaseURL   string `json:"base_url"`
	HasAPIKey bool   `json:"has_api_key"`
}

// ProviderListResponse represents the /v1/providers response
type ProviderListResponse struct {
	Object        string         `json:"object"`
	Data          []ProviderInfo `json:"data"`
	Active        string         `json:"active"`
	FallbackChain []string       `json:"fallback_chain"`
}

// ErrorResponse represents an API error
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail contains error information
type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

// === CLAUDE CLI TYPES ===

// ClaudeStreamMessage represents a message from Claude's stream-json output
type ClaudeStreamMessage struct {
	Type             string           `json:"type"`
	Subtype          string           `json:"subtype,omitempty"`
	Message          *ClaudeMessage   `json:"message,omitempty"`
	Result           string           `json:"result,omitempty"`
	StructuredOutput json.RawMessage  `json:"structured_output,omitempty"`
	Usage            *ClaudeUsage     `json:"usage,omitempty"`
	ToolUseResult    *ToolUseResultEx `json:"tool_use_result,omitempty"` // For user messages with tool results
}

// ToolUseResultEx contains extended tool result info from Claude CLI
type ToolUseResultEx struct {
	Stdout      string `json:"stdout,omitempty"`
	Stderr      string `json:"stderr,omitempty"`
	Interrupted bool   `json:"interrupted,omitempty"`
	IsImage     bool   `json:"isImage,omitempty"`
}

// ClaudeMessage represents the nested message in assistant responses
type ClaudeMessage struct {
	Content    []ClaudeContent `json:"content,omitempty"`
	StopReason string          `json:"stop_reason,omitempty"`
	Usage      *ClaudeUsage    `json:"usage,omitempty"`
}

// ClaudeContent represents a content block in the message
type ClaudeContent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`          // For tool_use blocks
	Name      string          `json:"name,omitempty"`        // For tool_use blocks (e.g., "StructuredOutput")
	Input     json.RawMessage `json:"input,omitempty"`       // For tool_use blocks - contains structured output JSON
	ToolUseID string          `json:"tool_use_id,omitempty"` // For tool_result blocks
	Content   string          `json:"content,omitempty"`     // For tool_result blocks (the result content)
	IsError   bool            `json:"is_error,omitempty"`    // For tool_result blocks
}

// ClaudeUsage represents token usage info
type ClaudeUsage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
}

// === RICH STREAMING TYPES ===

// ClaudeStreamEvent represents the event wrapper for --include-partial-messages mode
type ClaudeStreamEvent struct {
	Type  string          `json:"type"`
	Event json.RawMessage `json:"event,omitempty"`
}

// StreamEventData represents the data inside a stream_event
type StreamEventData struct {
	Type         string          `json:"type"` // message_start, content_block_start, content_block_delta, etc.
	Index        int             `json:"index,omitempty"`
	ContentBlock *ContentBlock   `json:"content_block,omitempty"`
	Delta        *DeltaContent   `json:"delta,omitempty"`
	Message      json.RawMessage `json:"message,omitempty"`
}

// ContentBlock represents a content block in streaming
type ContentBlock struct {
	Type  string          `json:"type"` // text, tool_use
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`    // for tool_use
	Name  string          `json:"name,omitempty"`  // for tool_use
	Input json.RawMessage `json:"input,omitempty"` // for tool_use
}

// DeltaContent represents delta content in streaming
type DeltaContent struct {
	Type        string `json:"type"` // text_delta, input_json_delta
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}
