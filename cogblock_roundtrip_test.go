package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

// === COGBLOCK CONTENT TYPES ===
// These types mirror the Anthropic Messages API content blocks.
// They are the MessageBlock primitive — the internal wire format for CogOS inference.

// MessageBlock is the Messages API content block — the MessageBlock primitive.
type MessageBlock struct {
	Type string `json:"type"` // "text", "image", "tool_use", "tool_result", "thinking"

	// Type-specific fields
	Text   string       `json:"text,omitempty"`   // for type="text" and type="thinking"
	Source *ImageSource `json:"source,omitempty"` // for type="image"

	ID    string          `json:"id,omitempty"`    // for type="tool_use"
	Name  string          `json:"name,omitempty"`  // for type="tool_use"
	Input json.RawMessage `json:"input,omitempty"` // for type="tool_use"

	ToolUseID string          `json:"tool_use_id,omitempty"` // for type="tool_result"
	Content   json.RawMessage `json:"content,omitempty"`     // for type="tool_result" (nested blocks)

	// Modality extension — annotation for modality context
	Modality *ModalityMeta `json:"modality,omitempty"`
}

// ImageSource represents a base64-encoded image source.
type ImageSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // "image/png", etc.
	Data      string `json:"data"`       // base64 encoded
}

// ModalityMeta annotates a content block with modality context.
type ModalityMeta struct {
	Source     string  `json:"source"`               // "voice", "text", "vision"
	Channel   string  `json:"channel"`               // "discord-voice", "claude-code"
	Confidence float64 `json:"confidence,omitempty"` // gate confidence
	LatencyMs  int    `json:"latency_ms,omitempty"`  // transform latency
}

// === ROUNDTRIP TESTS ===

func TestTextBlockRoundtrip(t *testing.T) {
	original := MessageBlock{
		Type: "text",
		Text: "Hello, world! This is a text content block.",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Failed to marshal text block: %v", err)
	}

	var decoded MessageBlock
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal text block: %v", err)
	}

	if decoded.Type != original.Type {
		t.Errorf("Type mismatch: got %q, want %q", decoded.Type, original.Type)
	}
	if decoded.Text != original.Text {
		t.Errorf("Text mismatch: got %q, want %q", decoded.Text, original.Text)
	}

	// Verify omitted fields are absent from JSON
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Failed to parse raw JSON: %v", err)
	}
	for _, field := range []string{"source", "id", "name", "input", "tool_use_id", "content", "modality"} {
		if _, ok := raw[field]; ok {
			t.Errorf("Field %q should be omitted from text block JSON", field)
		}
	}
}

func TestImageBlockRoundtrip(t *testing.T) {
	original := MessageBlock{
		Type: "image",
		Source: &ImageSource{
			Type:      "base64",
			MediaType: "image/png",
			Data:      "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==",
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Failed to marshal image block: %v", err)
	}

	var decoded MessageBlock
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal image block: %v", err)
	}

	if decoded.Type != original.Type {
		t.Errorf("Type mismatch: got %q, want %q", decoded.Type, original.Type)
	}
	if decoded.Source == nil {
		t.Fatal("Source is nil after roundtrip")
	}
	if decoded.Source.Type != original.Source.Type {
		t.Errorf("Source.Type mismatch: got %q, want %q", decoded.Source.Type, original.Source.Type)
	}
	if decoded.Source.MediaType != original.Source.MediaType {
		t.Errorf("Source.MediaType mismatch: got %q, want %q", decoded.Source.MediaType, original.Source.MediaType)
	}
	if decoded.Source.Data != original.Source.Data {
		t.Errorf("Source.Data mismatch: got %q, want %q", decoded.Source.Data, original.Source.Data)
	}
}

func TestToolUseBlockRoundtrip(t *testing.T) {
	inputJSON := json.RawMessage(`{"query":"weather in SF","units":"celsius"}`)

	original := MessageBlock{
		Type:  "tool_use",
		ID:    "toolu_01A09q90qw90lq917835lq9",
		Name:  "get_weather",
		Input: inputJSON,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Failed to marshal tool_use block: %v", err)
	}

	var decoded MessageBlock
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal tool_use block: %v", err)
	}

	if decoded.Type != original.Type {
		t.Errorf("Type mismatch: got %q, want %q", decoded.Type, original.Type)
	}
	if decoded.ID != original.ID {
		t.Errorf("ID mismatch: got %q, want %q", decoded.ID, original.ID)
	}
	if decoded.Name != original.Name {
		t.Errorf("Name mismatch: got %q, want %q", decoded.Name, original.Name)
	}

	// Compare Input as parsed objects to avoid whitespace differences
	var originalInput, decodedInput map[string]any
	if err := json.Unmarshal(original.Input, &originalInput); err != nil {
		t.Fatalf("Failed to parse original input: %v", err)
	}
	if err := json.Unmarshal(decoded.Input, &decodedInput); err != nil {
		t.Fatalf("Failed to parse decoded input: %v", err)
	}
	if !reflect.DeepEqual(originalInput, decodedInput) {
		t.Errorf("Input mismatch:\nOriginal: %s\nDecoded:  %s", original.Input, decoded.Input)
	}
}

func TestToolResultBlockRoundtrip(t *testing.T) {
	nestedContent := json.RawMessage(`[{"type":"text","text":"The weather in SF is 18°C and cloudy."}]`)

	original := MessageBlock{
		Type:      "tool_result",
		ToolUseID: "toolu_01A09q90qw90lq917835lq9",
		Content:   nestedContent,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Failed to marshal tool_result block: %v", err)
	}

	var decoded MessageBlock
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal tool_result block: %v", err)
	}

	if decoded.Type != original.Type {
		t.Errorf("Type mismatch: got %q, want %q", decoded.Type, original.Type)
	}
	if decoded.ToolUseID != original.ToolUseID {
		t.Errorf("ToolUseID mismatch: got %q, want %q", decoded.ToolUseID, original.ToolUseID)
	}

	// Verify nested content roundtrips correctly
	var originalNested, decodedNested []any
	if err := json.Unmarshal(original.Content, &originalNested); err != nil {
		t.Fatalf("Failed to parse original content: %v", err)
	}
	if err := json.Unmarshal(decoded.Content, &decodedNested); err != nil {
		t.Fatalf("Failed to parse decoded content: %v", err)
	}
	if !reflect.DeepEqual(originalNested, decodedNested) {
		t.Errorf("Content mismatch:\nOriginal: %s\nDecoded:  %s", original.Content, decoded.Content)
	}
}

func TestThinkingBlockRoundtrip(t *testing.T) {
	original := MessageBlock{
		Type: "thinking",
		Text: "Let me analyze this step by step. The user is asking about weather patterns in coastal cities.",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Failed to marshal thinking block: %v", err)
	}

	var decoded MessageBlock
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal thinking block: %v", err)
	}

	if decoded.Type != original.Type {
		t.Errorf("Type mismatch: got %q, want %q", decoded.Type, original.Type)
	}
	if decoded.Text != original.Text {
		t.Errorf("Text mismatch: got %q, want %q", decoded.Text, original.Text)
	}

	// Verify the JSON serializes with type="thinking", not "text"
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Failed to parse raw JSON: %v", err)
	}
	if raw["type"] != "thinking" {
		t.Errorf("JSON type field should be 'thinking', got %q", raw["type"])
	}
}

func TestModalityAnnotation(t *testing.T) {
	original := MessageBlock{
		Type: "text",
		Text: "Turn the lights on in the kitchen",
		Modality: &ModalityMeta{
			Source:     "voice",
			Channel:   "discord-voice",
			Confidence: 0.94,
			LatencyMs:  230,
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Failed to marshal block with modality: %v", err)
	}

	var decoded MessageBlock
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal block with modality: %v", err)
	}

	if decoded.Type != original.Type {
		t.Errorf("Type mismatch: got %q, want %q", decoded.Type, original.Type)
	}
	if decoded.Text != original.Text {
		t.Errorf("Text mismatch: got %q, want %q", decoded.Text, original.Text)
	}
	if decoded.Modality == nil {
		t.Fatal("Modality is nil after roundtrip")
	}
	if decoded.Modality.Source != original.Modality.Source {
		t.Errorf("Modality.Source mismatch: got %q, want %q", decoded.Modality.Source, original.Modality.Source)
	}
	if decoded.Modality.Channel != original.Modality.Channel {
		t.Errorf("Modality.Channel mismatch: got %q, want %q", decoded.Modality.Channel, original.Modality.Channel)
	}
	if decoded.Modality.Confidence != original.Modality.Confidence {
		t.Errorf("Modality.Confidence mismatch: got %v, want %v", decoded.Modality.Confidence, original.Modality.Confidence)
	}
	if decoded.Modality.LatencyMs != original.Modality.LatencyMs {
		t.Errorf("Modality.LatencyMs mismatch: got %d, want %d", decoded.Modality.LatencyMs, original.Modality.LatencyMs)
	}

	// Verify that a block WITHOUT modality still produces valid Messages API JSON
	apiBlock := MessageBlock{
		Type: "text",
		Text: "Plain text without modality",
	}
	apiData, err := json.Marshal(apiBlock)
	if err != nil {
		t.Fatalf("Failed to marshal API-compatible block: %v", err)
	}
	var apiRaw map[string]any
	if err := json.Unmarshal(apiData, &apiRaw); err != nil {
		t.Fatalf("Failed to parse API JSON: %v", err)
	}
	if _, hasModality := apiRaw["modality"]; hasModality {
		t.Error("Modality field should be omitted when nil (Messages API compatibility)")
	}
}

func TestMultiBlockMessage(t *testing.T) {
	blocks := []MessageBlock{
		{
			Type: "thinking",
			Text: "The user wants to know the weather. Let me use the tool.",
		},
		{
			Type: "text",
			Text: "I'll check the weather for you.",
		},
		{
			Type:  "tool_use",
			ID:    "toolu_01XFDUDYJgAACzvnptvVer6R",
			Name:  "get_weather",
			Input: json.RawMessage(`{"location":"San Francisco","units":"celsius"}`),
		},
	}

	data, err := json.Marshal(blocks)
	if err != nil {
		t.Fatalf("Failed to marshal multi-block message: %v", err)
	}

	var decoded []MessageBlock
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal multi-block message: %v", err)
	}

	if len(decoded) != len(blocks) {
		t.Fatalf("Block count mismatch: got %d, want %d", len(decoded), len(blocks))
	}

	// Verify each block type survived the roundtrip
	expectedTypes := []string{"thinking", "text", "tool_use"}
	for i, block := range decoded {
		if block.Type != expectedTypes[i] {
			t.Errorf("Block %d: type mismatch: got %q, want %q", i, block.Type, expectedTypes[i])
		}
	}

	// Verify thinking block
	if decoded[0].Text != blocks[0].Text {
		t.Errorf("Thinking block text mismatch: got %q, want %q", decoded[0].Text, blocks[0].Text)
	}

	// Verify text block
	if decoded[1].Text != blocks[1].Text {
		t.Errorf("Text block text mismatch: got %q, want %q", decoded[1].Text, blocks[1].Text)
	}

	// Verify tool_use block
	if decoded[2].ID != blocks[2].ID {
		t.Errorf("Tool use ID mismatch: got %q, want %q", decoded[2].ID, blocks[2].ID)
	}
	if decoded[2].Name != blocks[2].Name {
		t.Errorf("Tool use Name mismatch: got %q, want %q", decoded[2].Name, blocks[2].Name)
	}
}

// === COMPATIBILITY TESTS ===

func TestMessageBlockInEventPayload(t *testing.T) {
	// Verify a MessageBlock can be stored in EventPayload.Data (map[string]any)
	block := MessageBlock{
		Type: "text",
		Text: "inference result from Claude",
		Modality: &ModalityMeta{
			Source:  "text",
			Channel: "claude-code",
		},
	}

	// Marshal to JSON then unmarshal into map[string]any — this is
	// exactly what happens when you store structured data in EventPayload.Data
	blockJSON, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("Failed to marshal block: %v", err)
	}

	var asMap map[string]any
	if err := json.Unmarshal(blockJSON, &asMap); err != nil {
		t.Fatalf("Failed to unmarshal block into map: %v", err)
	}

	// Store in EventPayload.Data
	payload := EventPayload{
		Type:      "inference.response",
		Timestamp: "2026-04-05T12:00:00Z",
		SessionID: "test-session-cogblock",
		Data: map[string]any{
			"content_block": asMap,
			"model":         "claude-opus-4-20250514",
		},
	}

	// Roundtrip the entire payload through JSON
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Failed to marshal payload: %v", err)
	}

	var decoded EventPayload
	if err := json.Unmarshal(payloadJSON, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal payload: %v", err)
	}

	// Extract the content block back out
	blockData, ok := decoded.Data["content_block"].(map[string]any)
	if !ok {
		t.Fatal("content_block not found in decoded payload Data")
	}

	// Re-marshal and unmarshal into MessageBlock
	blockJSON2, err := json.Marshal(blockData)
	if err != nil {
		t.Fatalf("Failed to re-marshal block data: %v", err)
	}

	var recovered MessageBlock
	if err := json.Unmarshal(blockJSON2, &recovered); err != nil {
		t.Fatalf("Failed to unmarshal recovered block: %v", err)
	}

	if recovered.Type != block.Type {
		t.Errorf("Type mismatch after EventPayload roundtrip: got %q, want %q", recovered.Type, block.Type)
	}
	if recovered.Text != block.Text {
		t.Errorf("Text mismatch after EventPayload roundtrip: got %q, want %q", recovered.Text, block.Text)
	}
	if recovered.Modality == nil {
		t.Fatal("Modality lost after EventPayload roundtrip")
	}
	if recovered.Modality.Source != block.Modality.Source {
		t.Errorf("Modality.Source mismatch: got %q, want %q", recovered.Modality.Source, block.Modality.Source)
	}
	if recovered.Modality.Channel != block.Modality.Channel {
		t.Errorf("Modality.Channel mismatch: got %q, want %q", recovered.Modality.Channel, block.Modality.Channel)
	}

	// Also verify the payload can be canonicalized (ledger-compatible)
	canonical, err := CanonicalizeEvent(&payload)
	if err != nil {
		t.Fatalf("Failed to canonicalize payload with content block: %v", err)
	}
	if len(canonical) == 0 {
		t.Error("Canonical form is empty")
	}
}

func TestMessageBlockFromJSON(t *testing.T) {
	// Parse real Anthropic Messages API JSON into MessageBlock structs
	tests := []struct {
		name     string
		json     string
		wantType string
		validate func(t *testing.T, block MessageBlock)
	}{
		{
			name:     "text response",
			json:     `{"type":"text","text":"Hello! How can I help you today?"}`,
			wantType: "text",
			validate: func(t *testing.T, block MessageBlock) {
				if block.Text != "Hello! How can I help you today?" {
					t.Errorf("Text mismatch: got %q", block.Text)
				}
			},
		},
		{
			name:     "tool use",
			json:     `{"type":"tool_use","id":"toolu_01A09q90qw90lq917835lq9","name":"get_weather","input":{"location":"San Francisco","unit":"fahrenheit"}}`,
			wantType: "tool_use",
			validate: func(t *testing.T, block MessageBlock) {
				if block.ID != "toolu_01A09q90qw90lq917835lq9" {
					t.Errorf("ID mismatch: got %q", block.ID)
				}
				if block.Name != "get_weather" {
					t.Errorf("Name mismatch: got %q", block.Name)
				}
				var input map[string]any
				if err := json.Unmarshal(block.Input, &input); err != nil {
					t.Fatalf("Failed to parse input: %v", err)
				}
				if input["location"] != "San Francisco" {
					t.Errorf("Input.location mismatch: got %v", input["location"])
				}
			},
		},
		{
			name:     "tool result with string content",
			json:     `{"type":"tool_result","tool_use_id":"toolu_01A09q90qw90lq917835lq9","content":[{"type":"text","text":"15 degrees"}]}`,
			wantType: "tool_result",
			validate: func(t *testing.T, block MessageBlock) {
				if block.ToolUseID != "toolu_01A09q90qw90lq917835lq9" {
					t.Errorf("ToolUseID mismatch: got %q", block.ToolUseID)
				}
				// Parse nested content blocks
				var nested []MessageBlock
				if err := json.Unmarshal(block.Content, &nested); err != nil {
					t.Fatalf("Failed to parse nested content: %v", err)
				}
				if len(nested) != 1 {
					t.Fatalf("Expected 1 nested block, got %d", len(nested))
				}
				if nested[0].Type != "text" || nested[0].Text != "15 degrees" {
					t.Errorf("Nested block mismatch: %+v", nested[0])
				}
			},
		},
		{
			name:     "image block",
			json:     `{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"/9j/4AAQ"}}`,
			wantType: "image",
			validate: func(t *testing.T, block MessageBlock) {
				if block.Source == nil {
					t.Fatal("Source is nil")
				}
				if block.Source.Type != "base64" {
					t.Errorf("Source.Type mismatch: got %q", block.Source.Type)
				}
				if block.Source.MediaType != "image/jpeg" {
					t.Errorf("Source.MediaType mismatch: got %q", block.Source.MediaType)
				}
				if block.Source.Data != "/9j/4AAQ" {
					t.Errorf("Source.Data mismatch: got %q", block.Source.Data)
				}
			},
		},
		{
			name:     "thinking block",
			json:     `{"type":"thinking","text":"Let me work through this problem carefully."}`,
			wantType: "thinking",
			validate: func(t *testing.T, block MessageBlock) {
				if block.Text != "Let me work through this problem carefully." {
					t.Errorf("Text mismatch: got %q", block.Text)
				}
			},
		},
		{
			name:     "text block with unknown extra fields (forward compat)",
			json:     `{"type":"text","text":"Hello","citations":[{"start":0,"end":5}]}`,
			wantType: "text",
			validate: func(t *testing.T, block MessageBlock) {
				// Unknown fields are silently dropped — this is expected.
				// The core fields should still parse correctly.
				if block.Text != "Hello" {
					t.Errorf("Text mismatch: got %q", block.Text)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var block MessageBlock
			if err := json.Unmarshal([]byte(tt.json), &block); err != nil {
				t.Fatalf("Failed to parse Messages API JSON: %v", err)
			}
			if block.Type != tt.wantType {
				t.Errorf("Type mismatch: got %q, want %q", block.Type, tt.wantType)
			}
			tt.validate(t, block)
		})
	}
}
