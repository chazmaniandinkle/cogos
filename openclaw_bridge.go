// openclaw_bridge.go — HTTP proxy to the OpenClaw gateway.
//
// Bridges tool calls from surviving CogOS subsystems to the OpenClaw
// gateway's /tools/invoke endpoint. This file was split out from mcp.go
// in Track 5 when the root MCP stdio/HTTP servers were deleted — the
// bridge itself still has non-MCP callers (bus_tool_router.go, which
// owns the in-process tool dispatch, and reconcile_mcp_tools.go, which
// pulls the external tool manifest via tools_list).
//
// When bridge calls hit an error the result shape is preserved so that
// callers comparing to the historical MCP tool contract still work.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// === TOOL RESULT TYPES (MCP-shaped) ===

// MCPTool describes a tool in tools/list — the manifest shape returned by
// reconcile_mcp_tools when it pulls the tool list from the gateway.
type MCPTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

// MCPToolCallResult for tools/call responses. The bridge returns this
// shape from ExecuteTool so surviving callers (bus_tool_router) can
// propagate errors with the historical contract.
type MCPToolCallResult struct {
	Content []MCPToolContent `json:"content"`
	IsError bool             `json:"isError,omitempty"`
}

// MCPToolContent is a single content block in an MCP tool result.
type MCPToolContent struct {
	Type string `json:"type"` // "text" or "image" or "resource"
	Text string `json:"text,omitempty"`
}

// openAIFunctionTool is the OpenAI-format tool definition used when
// deserializing tools out of a chat request body.
type openAIFunctionTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string                 `json:"name"`
		Description string                 `json:"description"`
		Parameters  map[string]interface{} `json:"parameters"`
	} `json:"function"`
}

// convertOpenAIToolsToMCP converts OpenAI-format tool definitions into the
// MCP tool manifest format used by kernel_harness.ConvertOpenAITools.
// Still referenced by the harness adapter even though the root MCP server
// itself was deleted in Track 5.
func convertOpenAIToolsToMCP(tools []json.RawMessage) []MCPTool {
	var mcpTools []MCPTool
	for _, raw := range tools {
		var oaiTool openAIFunctionTool
		if err := json.Unmarshal(raw, &oaiTool); err != nil {
			continue // skip malformed
		}
		if oaiTool.Type != "function" || oaiTool.Function.Name == "" {
			continue
		}
		schema := oaiTool.Function.Parameters
		if schema == nil {
			schema = map[string]interface{}{"type": "object"}
		}
		mcpTools = append(mcpTools, MCPTool{
			Name:        oaiTool.Function.Name,
			Description: oaiTool.Function.Description,
			InputSchema: schema,
		})
	}
	return mcpTools
}

// === OPENCLAW BRIDGE ===

// OpenClawBridge proxies tool calls to the OpenClaw gateway via HTTP.
type OpenClawBridge struct {
	BaseURL    string // e.g. "http://localhost:18789"
	Token      string // Bearer token for auth
	SessionKey string // Session context for tool execution
	client     *http.Client
}

// NewOpenClawBridge creates a new bridge client.
func NewOpenClawBridge(baseURL, token, sessionKey string) *OpenClawBridge {
	return &OpenClawBridge{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		Token:      token,
		SessionKey: sessionKey,
		client: &http.Client{
			Timeout: 60 * time.Second, // Browser/canvas actions can take 10-30s
		},
	}
}

// ProbeGateway verifies connectivity to the OpenClaw gateway. Returns nil
// if the gateway is reachable and authenticated.
func (b *OpenClawBridge) ProbeGateway(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "mcp.bridge.probe_gateway",
		trace.WithAttributes(
			attribute.String("openclaw.url", b.BaseURL),
		),
	)
	defer span.End()

	probeBody, _ := json.Marshal(map[string]interface{}{
		"tool":   "agents_list",
		"action": "json",
		"args":   map[string]interface{}{},
	})

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", b.BaseURL+"/tools/invoke", bytes.NewReader(probeBody))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "create probe request")
		return fmt.Errorf("create probe request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if b.Token != "" {
		req.Header.Set("Authorization", "Bearer "+b.Token)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "gateway unreachable")
		return fmt.Errorf("gateway probe failed: %w", err)
	}
	defer resp.Body.Close()

	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))

	if resp.StatusCode == 401 {
		err := fmt.Errorf("gateway auth failed (401) — check OPENCLAW_TOKEN")
		span.RecordError(err)
		span.SetStatus(codes.Error, "auth failed")
		return err
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		err := fmt.Errorf("gateway probe returned %d: %s", resp.StatusCode, string(body))
		span.RecordError(err)
		span.SetStatus(codes.Error, "probe failed")
		return err
	}

	span.SetStatus(codes.Ok, "gateway reachable")
	return nil
}

// ExecuteTool calls a tool on the OpenClaw gateway via POST /tools/invoke.
// Uses the bridge's default SessionKey for gateway tracking.
func (b *OpenClawBridge) ExecuteTool(ctx context.Context, name string, args map[string]interface{}) (*MCPToolCallResult, error) {
	return b.ExecuteToolWithSession(ctx, name, args, b.SessionKey)
}

// ExecuteToolWithSession calls a tool on the OpenClaw gateway via POST
// /tools/invoke with an explicit session key. If sessionKey is empty no
// session key is included in the request. Used by bus_tool_router to
// derive per-call session keys from the bus event context.
func (b *OpenClawBridge) ExecuteToolWithSession(ctx context.Context, name string, args map[string]interface{}, sessionKey string) (*MCPToolCallResult, error) {
	ctx, span := tracer.Start(ctx, "openclaw.tool.execute",
		trace.WithAttributes(
			attribute.String("tool.name", name),
			attribute.String("openclaw.url", b.BaseURL),
		),
	)
	defer span.End()

	body := map[string]interface{}{
		"tool": name,
		"args": args,
	}
	if sessionKey != "" {
		body["sessionKey"] = sessionKey
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	span.SetAttributes(attribute.Int("request.size", len(jsonBody)))

	req, err := http.NewRequestWithContext(ctx, "POST", b.BaseURL+"/tools/invoke", bytes.NewReader(jsonBody))
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if b.Token != "" {
		req.Header.Set("Authorization", "Bearer "+b.Token)
	}

	// Inject trace context into outgoing HTTP request for distributed tracing
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))

	resp, err := b.client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "tool invocation failed")
		return nil, fmt.Errorf("invoke tool: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("read response: %w", err)
	}

	span.SetAttributes(
		attribute.Int("http.status_code", resp.StatusCode),
		attribute.Int("response.size", len(respBody)),
	)

	if resp.StatusCode != 200 {
		span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", resp.StatusCode))
		return &MCPToolCallResult{
			Content: []MCPToolContent{
				{Type: "text", Text: fmt.Sprintf("Tool error (HTTP %d): %s", resp.StatusCode, string(respBody))},
			},
			IsError: true,
		}, nil
	}

	// Parse the OpenClaw response: { ok: true, result: ... }
	var ocResp struct {
		OK     bool        `json:"ok"`
		Result interface{} `json:"result"`
		Error  *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &ocResp); err != nil {
		return &MCPToolCallResult{
			Content: []MCPToolContent{
				{Type: "text", Text: string(respBody)},
			},
		}, nil
	}

	if !ocResp.OK && ocResp.Error != nil {
		span.SetAttributes(attribute.String("tool.error", ocResp.Error.Message))
		span.SetStatus(codes.Error, "tool returned error")
		return &MCPToolCallResult{
			Content: []MCPToolContent{
				{Type: "text", Text: fmt.Sprintf("Tool error: %s", ocResp.Error.Message)},
			},
			IsError: true,
		}, nil
	}

	span.SetStatus(codes.Ok, "tool executed")

	// Convert result to text
	resultText, err := json.MarshalIndent(ocResp.Result, "", "  ")
	if err != nil {
		resultText = respBody
	}

	return &MCPToolCallResult{
		Content: []MCPToolContent{
			{Type: "text", Text: string(resultText)},
		},
	}, nil
}
