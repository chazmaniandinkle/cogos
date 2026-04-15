// MCP Mod3 Tools — forwards Mod3 TTS tools through the kernel MCP endpoint.
//
// Mod3 runs as an HTTP service (default: localhost:7860). These tools proxy
// MCP tool calls to the Mod3 REST API, allowing remote MCP clients to use
// voice synthesis without direct access to the Mod3 service.
//
// Tools:
//   - mod3_speak     → POST /v1/synthesize (returns JSON metrics, not audio)
//   - mod3_stop      → POST /v1/stop
//   - mod3_voices    → GET  /v1/voices
//   - mod3_status    → GET  /health

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// mod3BaseURL returns the Mod3 service URL from env or default.
func mod3BaseURL() string {
	if u := os.Getenv("MOD3_URL"); u != "" {
		return u
	}
	return "http://localhost:7860"
}

var mod3Client = &http.Client{
	Timeout: 30 * time.Second,
}

// GetMod3Tools returns the MCP tool definitions for Mod3.
func GetMod3Tools() []MCPTool {
	return []MCPTool{
		{
			Name:        "mod3_speak",
			Description: "Synthesize text to speech via Mod3. Audio is played on the server's speakers. Returns generation metrics (duration, RTF, engine used).",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"text": map[string]interface{}{
						"type":        "string",
						"description": "Text to synthesize",
					},
					"voice": map[string]interface{}{
						"type":        "string",
						"description": "Voice preset (default: bm_lewis). Use mod3_voices to list available voices.",
						"default":     "bm_lewis",
					},
					"speed": map[string]interface{}{
						"type":        "number",
						"description": "Speed multiplier (default: 1.25)",
						"default":     1.25,
					},
					"emotion": map[string]interface{}{
						"type":        "number",
						"description": "Emotion/exaggeration intensity 0.0-1.0 (Chatterbox engine only, default: 0.5)",
						"default":     0.5,
					},
				},
				"required": []string{"text"},
			},
		},
		{
			Name:        "mod3_stop",
			Description: "Stop current speech playback and/or cancel queued items on Mod3.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"job_id": map[string]interface{}{
						"type":        "string",
						"description": "Specific job ID to cancel. If omitted, interrupts current playback and clears the queue.",
					},
				},
			},
		},
		{
			Name:        "mod3_voices",
			Description: "List available TTS engines and their voice presets on Mod3.",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		{
			Name:        "mod3_status",
			Description: "Check Mod3 service health — engine status, loaded models, queue depth.",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
	}
}

// toolMod3Speak forwards a synthesize request to Mod3.
// Returns JSON metrics (not audio bytes) since MCP is a text protocol.
func toolMod3Speak(args map[string]interface{}) (interface{}, *JSONRPCError) {
	text, _ := args["text"].(string)
	if text == "" {
		return nil, &JSONRPCError{Code: InvalidParams, Message: "text is required"}
	}

	voice := "bm_lewis"
	if v, ok := args["voice"].(string); ok && v != "" {
		voice = v
	}
	speed := 1.25
	if s, ok := args["speed"].(float64); ok {
		speed = s
	}
	emotion := 0.5
	if e, ok := args["emotion"].(float64); ok {
		emotion = e
	}

	body, _ := json.Marshal(map[string]interface{}{
		"text":    text,
		"voice":   voice,
		"speed":   speed,
		"emotion": emotion,
		"format":  "wav",
	})

	resp, err := mod3Client.Post(mod3BaseURL()+"/v1/synthesize", "application/json", bytes.NewReader(body))
	if err != nil {
		return &MCPToolCallResult{
			Content: []MCPToolContent{{Type: "text", Text: fmt.Sprintf("Mod3 unreachable: %v", err)}},
			IsError: true,
		}, nil
	}
	defer resp.Body.Close()

	// The synthesize endpoint returns audio bytes with metrics in headers.
	// Discard the audio (it plays on the server), return the metrics.
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != 200 {
		return &MCPToolCallResult{
			Content: []MCPToolContent{{Type: "text", Text: fmt.Sprintf("Mod3 error: HTTP %d", resp.StatusCode)}},
			IsError: true,
		}, nil
	}

	result := map[string]interface{}{
		"status":   "ok",
		"job_id":   resp.Header.Get("X-Mod3-Job-Id"),
		"engine":   resp.Header.Get("X-Mod3-Engine"),
		"voice":    resp.Header.Get("X-Mod3-Voice"),
		"duration": resp.Header.Get("X-Mod3-Duration-Sec"),
		"rtf":      resp.Header.Get("X-Mod3-RTF"),
		"gen_time": resp.Header.Get("X-Mod3-Gen-Time-Sec"),
	}

	resultJSON, _ := json.MarshalIndent(result, "", "  ")
	return &MCPToolCallResult{
		Content: []MCPToolContent{{Type: "text", Text: string(resultJSON)}},
	}, nil
}

// toolMod3Stop forwards a stop request to Mod3.
func toolMod3Stop(args map[string]interface{}) (interface{}, *JSONRPCError) {
	jobID, _ := args["job_id"].(string)

	url := mod3BaseURL() + "/v1/stop"
	if jobID != "" {
		url += "?job_id=" + jobID
	}

	resp, err := mod3Client.Post(url, "application/json", nil)
	if err != nil {
		return &MCPToolCallResult{
			Content: []MCPToolContent{{Type: "text", Text: fmt.Sprintf("Mod3 unreachable: %v", err)}},
			IsError: true,
		}, nil
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return &MCPToolCallResult{
			Content: []MCPToolContent{{Type: "text", Text: fmt.Sprintf("Mod3 error: HTTP %d: %s", resp.StatusCode, string(respBody))}},
			IsError: true,
		}, nil
	}

	return &MCPToolCallResult{
		Content: []MCPToolContent{{Type: "text", Text: string(respBody)}},
	}, nil
}

// toolMod3Voices lists available voices from Mod3.
func toolMod3Voices(_ map[string]interface{}) (interface{}, *JSONRPCError) {
	resp, err := mod3Client.Get(mod3BaseURL() + "/v1/voices")
	if err != nil {
		return &MCPToolCallResult{
			Content: []MCPToolContent{{Type: "text", Text: fmt.Sprintf("Mod3 unreachable: %v", err)}},
			IsError: true,
		}, nil
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return &MCPToolCallResult{
			Content: []MCPToolContent{{Type: "text", Text: fmt.Sprintf("Mod3 error: HTTP %d: %s", resp.StatusCode, string(respBody))}},
			IsError: true,
		}, nil
	}

	// Pretty-print the JSON
	var parsed interface{}
	if err := json.Unmarshal(respBody, &parsed); err == nil {
		pretty, _ := json.MarshalIndent(parsed, "", "  ")
		return &MCPToolCallResult{
			Content: []MCPToolContent{{Type: "text", Text: string(pretty)}},
		}, nil
	}

	return &MCPToolCallResult{
		Content: []MCPToolContent{{Type: "text", Text: string(respBody)}},
	}, nil
}

// toolMod3Status checks Mod3 health.
func toolMod3Status(_ map[string]interface{}) (interface{}, *JSONRPCError) {
	resp, err := mod3Client.Get(mod3BaseURL() + "/health")
	if err != nil {
		return &MCPToolCallResult{
			Content: []MCPToolContent{{Type: "text", Text: fmt.Sprintf("Mod3 unreachable: %v", err)}},
			IsError: true,
		}, nil
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return &MCPToolCallResult{
			Content: []MCPToolContent{{Type: "text", Text: fmt.Sprintf("Mod3 error: HTTP %d: %s", resp.StatusCode, string(respBody))}},
			IsError: true,
		}, nil
	}

	var parsed interface{}
	if err := json.Unmarshal(respBody, &parsed); err == nil {
		pretty, _ := json.MarshalIndent(parsed, "", "  ")
		return &MCPToolCallResult{
			Content: []MCPToolContent{{Type: "text", Text: string(pretty)}},
		}, nil
	}

	return &MCPToolCallResult{
		Content: []MCPToolContent{{Type: "text", Text: string(respBody)}},
	}, nil
}
