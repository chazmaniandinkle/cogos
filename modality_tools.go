// modality_tools.go — MCP tool handlers for the modality bus.
//
// Exposes three tools for agent-initiated modality operations:
//   - cog_synthesize: text-to-speech via bus.Act
//   - cog_vad: voice activity detection via bus.Perceive
//   - cog_modality_status: bus HUD for agent awareness
//
// These handlers define tool definitions and logic but do NOT register
// themselves in mcp.go — registration is an integration task.

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// ModalityToolSet holds MCP tool handlers for the modality bus.
type ModalityToolSet struct {
	bus *ModalityBus
}

// NewModalityToolSet creates a tool set backed by the given bus.
func NewModalityToolSet(bus *ModalityBus) *ModalityToolSet {
	return &ModalityToolSet{bus: bus}
}

// Tools returns the MCP tool definitions for registration.
func (ts *ModalityToolSet) Tools() []MCPTool {
	return []MCPTool{
		{
			Name:        "cog_synthesize",
			Description: "Synthesize speech from text via the modality bus. Returns metadata only — audio plays through the channel.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{
						"type":        "string",
						"description": "Text to synthesize into speech",
					},
					"voice": map[string]any{
						"type":        "string",
						"description": "Voice identifier (e.g., 'bm_lewis')",
					},
					"speed": map[string]any{
						"type":        "number",
						"description": "Playback speed multiplier (default: 1.0)",
						"default":     1.0,
					},
				},
				"required": []string{"text"},
			},
		},
		{
			Name:        "cog_vad",
			Description: "Voice activity detection — check whether audio contains speech via the modality bus.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"audio_base64": map[string]any{
						"type":        "string",
						"description": "Base64-encoded audio data",
					},
					"sample_rate": map[string]any{
						"type":        "integer",
						"description": "Audio sample rate in Hz (default: 16000)",
						"default":     16000,
					},
				},
				"required": []string{"audio_base64"},
			},
		},
		{
			Name:        "cog_modality_status",
			Description: "Returns current modality bus status — modules, channels, and recent events for agent awareness.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

// HandleSynthesize handles the cog_synthesize tool call.
func (ts *ModalityToolSet) HandleSynthesize(input map[string]any) (map[string]any, error) {
	text, _ := input["text"].(string)
	if text == "" {
		return nil, fmt.Errorf("cog_synthesize: 'text' is required")
	}

	params := make(map[string]any)
	if voice, ok := input["voice"].(string); ok && voice != "" {
		params["voice"] = voice
	}
	if speed, ok := input["speed"].(float64); ok && speed > 0 {
		params["speed"] = speed
	}

	intent := &CognitiveIntent{
		Modality: ModalityVoice,
		Content:  text,
		Params:   params,
	}

	output, err := ts.bus.Act(intent)
	if err != nil {
		return nil, fmt.Errorf("cog_synthesize: %w", err)
	}

	result := map[string]any{
		"status":    "ok",
		"mime_type": output.MimeType,
		"bytes":     len(output.Data),
	}
	if output.Duration > 0 {
		result["duration_sec"] = output.Duration.Seconds()
	}
	return result, nil
}

// HandleVAD handles the cog_vad tool call.
func (ts *ModalityToolSet) HandleVAD(input map[string]any) (map[string]any, error) {
	b64, _ := input["audio_base64"].(string)
	if b64 == "" {
		return nil, fmt.Errorf("cog_vad: 'audio_base64' is required")
	}

	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("cog_vad: invalid base64: %w", err)
	}

	event, err := ts.bus.Perceive(raw, ModalityVoice, "vad")
	if err != nil {
		return nil, fmt.Errorf("cog_vad: %w", err)
	}

	// Gate rejected (no speech detected) — Perceive returns (nil, nil).
	if event == nil {
		return map[string]any{
			"has_speech": false,
			"confidence": 0.0,
		}, nil
	}

	return map[string]any{
		"has_speech": true,
		"confidence": event.Confidence,
	}, nil
}

// HandleStatus handles the cog_modality_status tool call.
func (ts *ModalityToolSet) HandleStatus(_ map[string]any) (map[string]any, error) {
	return ts.bus.HUD(), nil
}

// ToMCPResult converts a handler result to an MCPToolCallResult.
func (ts *ModalityToolSet) ToMCPResult(data map[string]any, err error) *MCPToolCallResult {
	if err != nil {
		return &MCPToolCallResult{
			Content: []MCPToolContent{{Type: "text", Text: err.Error()}},
			IsError: true,
		}
	}
	encoded, _ := json.Marshal(data)
	return &MCPToolCallResult{
		Content: []MCPToolContent{{Type: "text", Text: string(encoded)}},
	}
}
