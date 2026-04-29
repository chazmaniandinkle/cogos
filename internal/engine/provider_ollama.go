// provider_ollama.go — OllamaProvider
//
// Implements Provider against a local Ollama server (http://localhost:11434).
// Uses /api/chat for multi-turn conversations (not /api/generate).
// Streaming: Ollama returns newline-delimited JSON chunks.
// think=false: disables qwen3's thinking mode to avoid silent token burn.
package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const defaultOllamaModel = "gemma4:e4b"

type ollamaModelProfile struct {
	Capabilities     []Capability
	MaxContextTokens int
	MaxOutputTokens  int
}

var ollamaModelProfiles = map[string]ollamaModelProfile{
	"gemma4:e4b": {
		Capabilities:     []Capability{CapStreaming, CapJSON, CapToolCallValidation},
		MaxContextTokens: 128000,
		MaxOutputTokens:  4096,
	},
	"gemma4:e2b": {
		Capabilities:     []Capability{CapStreaming, CapJSON, CapToolCallValidation},
		MaxContextTokens: 128000,
		MaxOutputTokens:  4096,
	},
	"qwen3.5:9b": {
		Capabilities:    []Capability{CapStreaming, CapJSON},
		MaxOutputTokens: 4096,
	},
}

func lookupOllamaModelProfile(model string) ollamaModelProfile {
	if profile, ok := ollamaModelProfiles[model]; ok {
		return profile
	}
	return ollamaModelProfile{
		Capabilities:    []Capability{CapStreaming, CapJSON},
		MaxOutputTokens: 4096,
	}
}

// OllamaProvider implements Provider against a local Ollama server.
type OllamaProvider struct {
	name          string
	endpoint      string // e.g. "http://localhost:11434"
	model         string
	contextWindow int // num_ctx to send per request; 0 = Ollama default (4096)
	timeout       time.Duration
	client        *http.Client
}

// NewOllamaProvider creates an OllamaProvider from a ProviderConfig.
func NewOllamaProvider(name string, cfg ProviderConfig) *OllamaProvider {
	endpoint := ResolveLocalLLMEndpoint(cfg.Endpoint, localLLMDefaultEndpoint)
	timeout := time.Duration(cfg.Timeout) * time.Second
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	return &OllamaProvider{
		name:          name,
		endpoint:      endpoint,
		model:         cfg.Model,
		contextWindow: cfg.ContextWindow,
		timeout:       timeout,
		client:        &http.Client{Timeout: timeout},
	}
}

// Name returns the provider identifier.
func (p *OllamaProvider) Name() string { return p.name }

// Available checks if Ollama is running and the configured model is loaded.
func (p *OllamaProvider) Available(ctx context.Context) bool {
	models, err := p.listModels(ctx)
	if err != nil {
		return false
	}
	if p.model == "" {
		return len(models) > 0
	}
	// Accept exact name or prefix (e.g. "qwen2.5:9b" matches "qwen2.5:9b-instruct").
	for _, model := range models {
		if model == p.model || strings.HasPrefix(model, p.model) {
			return true
		}
	}
	return false
}

// Capabilities returns what Ollama supports.
func (p *OllamaProvider) Capabilities() ProviderCapabilities {
	profile := lookupOllamaModelProfile(p.model)
	ctxTokens := p.contextWindow
	if ctxTokens <= 0 {
		ctxTokens = profile.MaxContextTokens
	}
	if ctxTokens <= 0 {
		ctxTokens = 4096
	}
	maxOutputTokens := profile.MaxOutputTokens
	if maxOutputTokens <= 0 {
		maxOutputTokens = 4096
	}
	return ProviderCapabilities{
		Capabilities:       append([]Capability(nil), profile.Capabilities...),
		MaxContextTokens:   ctxTokens,
		MaxOutputTokens:    maxOutputTokens,
		ModelsAvailable:    []string{p.model},
		IsLocal:            true,
		AgenticHarness:     false,
		CostPerInputToken:  0,
		CostPerOutputToken: 0,
	}
}

// ContextWindow returns the configured num_ctx for this provider.
func (p *OllamaProvider) ContextWindow() int {
	return p.contextWindow
}

// Ping measures round-trip latency to the Ollama server.
func (p *OllamaProvider) Ping(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint+"/api/version", nil)
	if err != nil {
		return 0, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return time.Since(start), nil
}

func (p *OllamaProvider) listModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(tags.Models))
	for _, model := range tags.Models {
		if model.Name == "" {
			continue
		}
		names = append(names, model.Name)
	}
	return names, nil
}

// ── Ollama wire types ─────────────────────────────────────────────────────────

type ollamaMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	Thinking   string           `json:"thinking,omitempty"`
	ToolCalls  []ollamaToolCall `json:"tool_calls,omitempty"`
	ToolName   string           `json:"tool_name,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type ollamaTool struct {
	Type     string               `json:"type"`
	Function ollamaToolDefinition `json:"function"`
}

type ollamaToolDefinition struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

type ollamaToolCall struct {
	Type     string                 `json:"type,omitempty"`
	Function ollamaToolCallFunction `json:"function"`
}

type ollamaToolCallFunction struct {
	Index     *int        `json:"index,omitempty"`
	Name      string      `json:"name"`
	Arguments interface{} `json:"arguments,omitempty"`
}

type ollamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Tools    []ollamaTool    `json:"tools,omitempty"`
	Stream   bool            `json:"stream"`
	Think    bool            `json:"think"` // false = disable thinking mode (qwen3)
	Options  map[string]any  `json:"options,omitempty"`
}

type ollamaChatResponse struct {
	Model      string        `json:"model"`
	CreatedAt  string        `json:"created_at"`
	Message    ollamaMessage `json:"message"`
	Done       bool          `json:"done"`
	DoneReason string        `json:"done_reason,omitempty"`
	// Token counts (only in final streaming chunk or non-streaming response).
	PromptEvalCount int `json:"prompt_eval_count"`
	EvalCount       int `json:"eval_count"`
}

// buildOllamaRequest converts a CompletionRequest to Ollama's /api/chat format.
// contextWindow sets num_ctx on the request; 0 means omit (use Ollama default of 4096).
func buildOllamaRequest(model string, req *CompletionRequest, stream bool, contextWindow int) *ollamaChatRequest {
	msgs := make([]ollamaMessage, 0, len(req.Messages)+1)
	if req.SystemPrompt != "" {
		msgs = append(msgs, ollamaMessage{Role: "system", Content: req.SystemPrompt})
	}
	toolNameByID := make(map[string]string)
	var pendingToolNames []string
	for _, m := range req.Messages {
		msg := ollamaMessage{
			Role:    m.Role,
			Content: m.Content,
		}
		switch {
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			msg.ToolCalls = make([]ollamaToolCall, 0, len(m.ToolCalls))
			for i, tc := range m.ToolCalls {
				toolArgs := decodeOllamaToolArguments(tc.Arguments)
				index := i
				msg.ToolCalls = append(msg.ToolCalls, ollamaToolCall{
					Type: "function",
					Function: ollamaToolCallFunction{
						Index:     &index,
						Name:      tc.Name,
						Arguments: toolArgs,
					},
				})
				if tc.ID != "" {
					toolNameByID[tc.ID] = tc.Name
				}
				pendingToolNames = append(pendingToolNames, tc.Name)
			}
		case m.Role == "tool":
			msg.ToolCallID = m.ToolCallID
			msg.ToolName = m.Name
			if msg.ToolName == "" && m.ToolCallID != "" {
				msg.ToolName = toolNameByID[m.ToolCallID]
			}
			if msg.ToolName == "" && len(pendingToolNames) > 0 {
				msg.ToolName = pendingToolNames[0]
				pendingToolNames = pendingToolNames[1:]
			}
		}
		msgs = append(msgs, msg)
	}

	opts := map[string]any{}
	if contextWindow > 0 {
		opts["num_ctx"] = contextWindow
	}
	if req.Temperature != nil {
		opts["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		opts["top_p"] = *req.TopP
	}
	if req.MaxTokens != 0 {
		opts["num_predict"] = req.MaxTokens
	}

	or := &ollamaChatRequest{
		Model:    model,
		Messages: msgs,
		Stream:   stream,
		Think:    false, // prevent silent token burn in qwen3 thinking mode
		Options:  opts,
	}
	if len(req.Tools) > 0 && req.ToolChoice != "none" {
		or.Tools = make([]ollamaTool, len(req.Tools))
		for i, tool := range req.Tools {
			or.Tools[i] = ollamaTool{
				Type: "function",
				Function: ollamaToolDefinition{
					Name:        tool.Name,
					Description: tool.Description,
					Parameters:  tool.InputSchema,
				},
			}
		}
	}
	return or
}

func decodeOllamaToolArguments(raw string) interface{} {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]interface{}{}
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var value interface{}
	if err := dec.Decode(&value); err != nil {
		return raw
	}
	return value
}

func encodeOllamaToolArguments(raw interface{}) string {
	if raw == nil {
		return "{}"
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func mapOllamaDoneReason(reason string) string {
	switch reason {
	case "", "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return reason
	}
}

func parseOllamaToolCalls(msg ollamaMessage) []ToolCall {
	if len(msg.ToolCalls) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(msg.ToolCalls))
	for i, tc := range msg.ToolCalls {
		name := tc.Function.Name
		args := encodeOllamaToolArguments(tc.Function.Arguments)
		out = append(out, ToolCall{
			ID:        ollamaToolCallID(i, name),
			Name:      name,
			Arguments: args,
		})
	}
	return out
}

func ollamaToolCallID(index int, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "tool"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	base := strings.Trim(b.String(), "-")
	if base == "" {
		base = "tool"
	}
	return "ollama-call-" + strconv.Itoa(index) + "-" + base
}

func parseOllamaResponse(or *ollamaChatResponse, providerName, model string, latency time.Duration) *CompletionResponse {
	resp := &CompletionResponse{
		Content: or.Message.Content,
		Usage: TokenUsage{
			InputTokens:  or.PromptEvalCount,
			OutputTokens: or.EvalCount,
		},
		ProviderMeta: ProviderMeta{
			Provider: providerName,
			Model:    model,
			Latency:  latency,
		},
	}

	resp.ToolCalls = parseOllamaToolCalls(or.Message)
	switch {
	case len(resp.ToolCalls) > 0:
		resp.StopReason = "tool_use"
	default:
		resp.StopReason = mapOllamaDoneReason(or.DoneReason)
	}
	if resp.StopReason == "" {
		resp.StopReason = "end_turn"
	}
	return resp
}

// effectiveModel returns the model to send to Ollama: request override if set,
// otherwise the provider's configured default.
func (p *OllamaProvider) effectiveModel(req *CompletionRequest) string {
	if req.ModelOverride != "" {
		return req.ModelOverride
	}
	return p.model
}

// Complete sends a non-streaming request and returns the full response.
func (p *OllamaProvider) Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	start := time.Now()
	model := p.effectiveModel(req)

	payload := buildOllamaRequest(model, req, false, p.contextWindow)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("ollama: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.endpoint+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ollama: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama: status %d: %s", resp.StatusCode, string(data))
	}

	var or ollamaChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&or); err != nil {
		return nil, fmt.Errorf("ollama: decode response: %w", err)
	}

	return parseOllamaResponse(&or, p.name, model, time.Since(start)), nil
}

// Stream sends a streaming request and returns a channel of chunks.
// The channel closes when generation is complete or the context is cancelled.
func (p *OllamaProvider) Stream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
	model := p.effectiveModel(req)
	payload := buildOllamaRequest(model, req, true, p.contextWindow)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("ollama: marshal stream request: %w", err)
	}

	// Use a separate client without a timeout — streaming can be long.
	streamClient := &http.Client{}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.endpoint+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama: build stream request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := streamClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ollama: stream request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("ollama: stream status %d: %s", resp.StatusCode, string(data))
	}

	ch := make(chan StreamChunk, 32)
	go func() {
		defer close(ch)
		defer resp.Body.Close()

		var sawToolCalls bool

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			var chunk ollamaChatResponse
			if err := json.Unmarshal([]byte(line), &chunk); err != nil {
				select {
				case ch <- StreamChunk{Error: fmt.Errorf("ollama: decode chunk: %w", err)}:
				case <-ctx.Done():
				}
				return
			}
			for idx, tc := range parseOllamaToolCalls(chunk.Message) {
				sawToolCalls = true
				tcd := &ToolCallDelta{
					Index:     idx,
					ID:        tc.ID,
					Name:      tc.Name,
					ArgsDelta: tc.Arguments,
				}
				select {
				case ch <- StreamChunk{ToolCallDelta: tcd}:
				case <-ctx.Done():
					return
				}
			}

			if chunk.Message.Content != "" || chunk.Done {
				sc := StreamChunk{
					Delta: chunk.Message.Content,
					Done:  chunk.Done,
				}
				if chunk.Done {
					stopReason := mapOllamaDoneReason(chunk.DoneReason)
					if sawToolCalls && stopReason == "end_turn" {
						stopReason = "tool_use"
					}
					sc.StopReason = stopReason
					sc.Usage = &TokenUsage{
						InputTokens:  chunk.PromptEvalCount,
						OutputTokens: chunk.EvalCount,
					}
					sc.ProviderMeta = &ProviderMeta{
						Provider: p.name,
						Model:    model,
					}
				}
				select {
				case ch <- sc:
				case <-ctx.Done():
					return
				}
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case ch <- StreamChunk{Error: fmt.Errorf("ollama: scan: %w", err)}:
			case <-ctx.Done():
			}
		}
	}()

	return ch, nil
}
