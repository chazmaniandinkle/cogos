package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// === E1: Schema Tests ===

func TestDecompTier0Unmarshal(t *testing.T) {
	raw := `{"summary": "This document describes the CogOS decomposition pipeline."}`
	var t0 Tier0Result
	if err := json.Unmarshal([]byte(raw), &t0); err != nil {
		t.Fatalf("failed to unmarshal Tier0Result: %v", err)
	}
	if t0.Summary == "" {
		t.Error("expected non-empty summary")
	}
	if !strings.Contains(t0.Summary, "CogOS") {
		t.Errorf("expected summary to contain 'CogOS', got: %s", t0.Summary)
	}
}

func TestDecompTier1Unmarshal(t *testing.T) {
	raw := `{
		"summary": "The CogOS decomposition pipeline breaks input into tiered summaries for memory indexing.",
		"key_terms": ["decomposition", "tiered", "memory", "indexing"]
	}`
	var t1 Tier1Result
	if err := json.Unmarshal([]byte(raw), &t1); err != nil {
		t.Fatalf("failed to unmarshal Tier1Result: %v", err)
	}
	if t1.Summary == "" {
		t.Error("expected non-empty summary")
	}
	if len(t1.KeyTerms) != 4 {
		t.Errorf("expected 4 key terms, got %d", len(t1.KeyTerms))
	}
}

func TestDecompTier2Unmarshal(t *testing.T) {
	raw := `{
		"title": "Decomposition Pipeline",
		"type": "architecture",
		"tags": ["decompose", "pipeline", "cogos"],
		"summary": "A multi-tier decomposition pipeline for CogOS memory ingestion.",
		"sections": [
			{"heading": "Overview", "content": "The pipeline decomposes text into four tiers."},
			{"heading": "Tiers", "content": "Tier 0 is a sentence, Tier 1 a paragraph, Tier 2 a CogDoc, Tier 3 raw."}
		],
		"refs": [
			{"uri": "cog://mem/semantic/architecture/pipeline", "relation": "extends"}
		]
	}`
	var t2 Tier2Result
	if err := json.Unmarshal([]byte(raw), &t2); err != nil {
		t.Fatalf("failed to unmarshal Tier2Result: %v", err)
	}
	if t2.Title != "Decomposition Pipeline" {
		t.Errorf("expected title 'Decomposition Pipeline', got: %s", t2.Title)
	}
	if t2.Type != "architecture" {
		t.Errorf("expected type 'architecture', got: %s", t2.Type)
	}
	if len(t2.Tags) != 3 {
		t.Errorf("expected 3 tags, got %d", len(t2.Tags))
	}
	if len(t2.Sections) != 2 {
		t.Errorf("expected 2 sections, got %d", len(t2.Sections))
	}
	if len(t2.Refs) != 1 {
		t.Errorf("expected 1 ref, got %d", len(t2.Refs))
	}
}

func TestDecompTier2NoRefs(t *testing.T) {
	raw := `{
		"title": "Simple Note",
		"type": "knowledge",
		"tags": ["note"],
		"summary": "A simple note.",
		"sections": [{"heading": "Content", "content": "Hello world."}]
	}`
	var t2 Tier2Result
	if err := json.Unmarshal([]byte(raw), &t2); err != nil {
		t.Fatalf("failed to unmarshal Tier2Result without refs: %v", err)
	}
	if len(t2.Refs) != 0 {
		t.Errorf("expected 0 refs, got %d", len(t2.Refs))
	}
}

func TestDecompMalformedJSON(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"truncated", `{"summary": "hello`},
		{"not json", `this is plain text`},
		{"wrong type", `{"summary": 42}`},
		{"empty", ``},
	}
	for _, tc := range cases {
		t.Run("Tier0_"+tc.name, func(t *testing.T) {
			var t0 Tier0Result
			err := json.Unmarshal([]byte(tc.raw), &t0)
			if err == nil && tc.name != "wrong type" {
				// "wrong type" may not error since int->string coercion differs
				// but at minimum the other cases should fail
				t.Errorf("expected error for %q, got nil", tc.name)
			}
		})
	}
}

// === CycleMemoryEntry Quality Round-trip Tests ===

// === Prompt Template Tests ===

func TestDecompPromptTemplates(t *testing.T) {
	t.Run("tier0", func(t *testing.T) {
		p := tier0SystemPrompt()
		if !strings.Contains(p, "one sentence") {
			t.Error("tier0 prompt should mention 'one sentence'")
		}
		if !strings.Contains(p, "JSON") {
			t.Error("tier0 prompt should mention JSON format")
		}
	})

	t.Run("tier1", func(t *testing.T) {
		p := tier1SystemPrompt()
		if !strings.Contains(p, "paragraph") {
			t.Error("tier1 prompt should mention 'paragraph'")
		}
		if !strings.Contains(p, "key_terms") {
			t.Error("tier1 prompt should mention key_terms")
		}
	})

	t.Run("tier2", func(t *testing.T) {
		p := tier2SystemPrompt()
		if !strings.Contains(p, "title") {
			t.Error("tier2 prompt should mention 'title'")
		}
		if !strings.Contains(p, "sections") {
			t.Error("tier2 prompt should mention 'sections'")
		}
	})

	t.Run("user", func(t *testing.T) {
		p := tierUserPrompt("hello world")
		if !strings.Contains(p, "hello world") {
			t.Error("user prompt should contain the input text")
		}
		if !strings.Contains(p, "Decompose") {
			t.Error("user prompt should contain 'Decompose'")
		}
	})
}

// === Input Normalization Tests ===

func TestDecompDetectFormat(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		expect string
	}{
		{"markdown_h1", "# Title\nSome content", "markdown"},
		{"markdown_h2", "Intro\n## Section\nContent", "markdown"},
		{"markdown_code", "Some text\n```go\nfmt.Println()\n```", "markdown"},
		{"conversation", "Human: hello\nAssistant: hi", "conversation"},
		{"plaintext", "Just some regular text with no special markers.", "plaintext"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detectFormat(tc.input)
			if got != tc.expect {
				t.Errorf("detectFormat(%q) = %q, want %q", tc.name, got, tc.expect)
			}
		})
	}
}

// === Tier Flag Parsing Tests ===

func TestDecompParseTierFlag(t *testing.T) {
	cases := []struct {
		flag    string
		want    []int
		wantErr bool
	}{
		{"all", []int{0, 1, 2, 3}, false},
		{"0", []int{0}, false},
		{"0,1", []int{0, 1}, false},
		{"2,3", []int{2, 3}, false},
		{"0,1,2,3", []int{0, 1, 2, 3}, false},
		{"invalid", nil, true},
		{"0,5", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.flag, func(t *testing.T) {
			got, err := parseTierFlag(tc.flag)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error for %q", tc.flag)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.flag, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len mismatch: got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("tier[%d] = %d, want %d", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// === DecompositionResult Round-trip Test ===

func TestDecompResultRoundTrip(t *testing.T) {
	result := &DecompositionResult{
		InputHash:   "abcdef123456",
		InputSize:   1024,
		InputFormat: "markdown",
		Tier0:       &Tier0Result{Summary: "Test summary."},
		Tier1: &Tier1Result{
			Summary:  "A longer test summary with details.",
			KeyTerms: []string{"test", "summary"},
		},
		Metrics: DecompMetrics{
			Tier0Tokens:      10,
			Tier0LatencyMs:   50,
			TotalLatencyMs:   150,
			CompressionRatio: 10.24,
		},
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var decoded DecompositionResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if decoded.InputHash != result.InputHash {
		t.Errorf("hash mismatch: %s vs %s", decoded.InputHash, result.InputHash)
	}
	if decoded.Tier0.Summary != result.Tier0.Summary {
		t.Errorf("tier0 summary mismatch")
	}
	if decoded.Tier1.Summary != result.Tier1.Summary {
		t.Errorf("tier1 summary mismatch")
	}
	if decoded.Metrics.CompressionRatio != result.Metrics.CompressionRatio {
		t.Errorf("compression ratio mismatch")
	}
}

// === Event Callback Test ===

func TestDecompEventConstants(t *testing.T) {
	// Verify event constants are non-empty and follow naming convention
	events := []string{
		DecompEventStart,
		DecompEventTierStart,
		DecompEventTierComplete,
		DecompEventComplete,
		DecompEventError,
	}
	for _, e := range events {
		if e == "" {
			t.Error("event constant should not be empty")
		}
		if !strings.HasPrefix(e, "decompose.") {
			t.Errorf("event %q should start with 'decompose.'", e)
		}
	}
}

// === E2: DecompositionRunner Unit Tests (Mock Ollama) ===

// mockOllamaResponse builds an agentChatResponse JSON with the given content string.
func mockOllamaResponse(content string) []byte {
	resp := agentChatResponse{
		Model: "test-model",
		Done:  true,
	}
	resp.Message.Role = "assistant"
	resp.Message.Content = content
	data, _ := json.Marshal(resp)
	return data
}

// tierResponseForSystemPrompt returns appropriate mock JSON based on the system prompt content.
func tierResponseForSystemPrompt(sysPrompt string) string {
	if strings.Contains(sysPrompt, "one sentence") {
		return `{"summary": "A concise one-sentence summary of the input."}`
	}
	if strings.Contains(sysPrompt, "paragraph") && strings.Contains(sysPrompt, "key_terms") {
		return `{"summary": "A detailed paragraph summary covering the main points of the input text.", "key_terms": ["testing", "decomposition", "pipeline"]}`
	}
	if strings.Contains(sysPrompt, "title") && strings.Contains(sysPrompt, "sections") {
		return `{"title": "Test Document", "type": "knowledge", "tags": ["test", "mock"], "summary": "A structured document from the input.", "sections": [{"heading": "Introduction", "content": "This is the intro section."}]}`
	}
	// Fallback
	return `{"summary": "fallback"}`
}

// TestDecompRunnerPromptConstruction verifies that the runner sends correct prompts to Ollama.
func TestDecompRunnerPromptConstruction(t *testing.T) {
	var mu sync.Mutex
	var captured []agentChatRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req agentChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("failed to decode request: %v", err)
			http.Error(w, "bad request", 400)
			return
		}
		mu.Lock()
		captured = append(captured, req)
		mu.Unlock()

		// Find system prompt to determine tier
		var sysPrompt string
		for _, m := range req.Messages {
			if m.Role == "system" {
				sysPrompt = m.Content
				break
			}
		}
		content := tierResponseForSystemPrompt(sysPrompt)
		w.Write(mockOllamaResponse(content))
	}))
	defer server.Close()

	harness := NewAgentHarness(AgentHarnessConfig{
		OllamaURL: server.URL,
		Model:     "test-model",
	})
	runner := NewDecompositionRunner(harness, nil)

	input := &DecompInput{Text: "The quick brown fox jumps over the lazy dog.", Format: "plaintext", ByteSize: 44}
	_, err := runner.Run(context.Background(), input)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	// Should have exactly 3 LLM calls (tier 0, 1, 2 — tier 3 is passthrough)
	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 3 {
		t.Fatalf("expected 3 LLM calls, got %d", len(captured))
	}

	// Tier 0 checks
	req0 := captured[0]
	if req0.Format != "json" {
		t.Errorf("tier 0: expected format 'json', got %q", req0.Format)
	}
	if req0.Think != false {
		t.Errorf("tier 0: expected think=false")
	}
	var sys0, user0 string
	for _, m := range req0.Messages {
		if m.Role == "system" {
			sys0 = m.Content
		}
		if m.Role == "user" {
			user0 = m.Content
		}
	}
	if !strings.Contains(sys0, "one sentence") {
		t.Errorf("tier 0 system prompt missing 'one sentence': %s", sys0)
	}
	if !strings.Contains(user0, "The quick brown fox") {
		t.Errorf("tier 0 user prompt missing input text: %s", user0)
	}

	// Tier 1 checks
	var sys1 string
	for _, m := range captured[1].Messages {
		if m.Role == "system" {
			sys1 = m.Content
		}
	}
	if !strings.Contains(sys1, "paragraph") {
		t.Errorf("tier 1 system prompt missing 'paragraph': %s", sys1)
	}
	if !strings.Contains(sys1, "key_terms") {
		t.Errorf("tier 1 system prompt missing 'key_terms': %s", sys1)
	}

	// Tier 2 checks
	var sys2 string
	for _, m := range captured[2].Messages {
		if m.Role == "system" {
			sys2 = m.Content
		}
	}
	if !strings.Contains(sys2, "title") {
		t.Errorf("tier 2 system prompt missing 'title': %s", sys2)
	}
	if !strings.Contains(sys2, "sections") {
		t.Errorf("tier 2 system prompt missing 'sections': %s", sys2)
	}
	if !strings.Contains(sys2, "tags") {
		t.Errorf("tier 2 system prompt missing 'tags': %s", sys2)
	}
}

// TestDecompRunnerResultAssembly verifies that the runner assembles results correctly from mock responses.
func TestDecompRunnerResultAssembly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req agentChatRequest
		json.NewDecoder(r.Body).Decode(&req)

		var sysPrompt string
		for _, m := range req.Messages {
			if m.Role == "system" {
				sysPrompt = m.Content
				break
			}
		}
		content := tierResponseForSystemPrompt(sysPrompt)
		w.Write(mockOllamaResponse(content))
	}))
	defer server.Close()

	harness := NewAgentHarness(AgentHarnessConfig{
		OllamaURL: server.URL,
		Model:     "test-model",
	})
	runner := NewDecompositionRunner(harness, nil)

	inputText := "Some important text that needs decomposition into tiers."
	input := &DecompInput{Text: inputText, Format: "plaintext", ByteSize: len(inputText)}
	result, err := runner.Run(context.Background(), input)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	// Tier 0
	if result.Tier0 == nil {
		t.Fatal("expected Tier0 to be populated")
	}
	if result.Tier0.Summary != "A concise one-sentence summary of the input." {
		t.Errorf("tier 0 summary mismatch: %q", result.Tier0.Summary)
	}

	// Tier 1
	if result.Tier1 == nil {
		t.Fatal("expected Tier1 to be populated")
	}
	if !strings.Contains(result.Tier1.Summary, "detailed paragraph") {
		t.Errorf("tier 1 summary mismatch: %q", result.Tier1.Summary)
	}
	if len(result.Tier1.KeyTerms) != 3 {
		t.Errorf("expected 3 key terms, got %d", len(result.Tier1.KeyTerms))
	}

	// Tier 2
	if result.Tier2 == nil {
		t.Fatal("expected Tier2 to be populated")
	}
	if result.Tier2.Title != "Test Document" {
		t.Errorf("tier 2 title mismatch: %q", result.Tier2.Title)
	}
	if result.Tier2.Type != "knowledge" {
		t.Errorf("tier 2 type mismatch: %q", result.Tier2.Type)
	}
	if len(result.Tier2.Tags) != 2 {
		t.Errorf("expected 2 tags, got %d", len(result.Tier2.Tags))
	}
	if len(result.Tier2.Sections) != 1 {
		t.Errorf("expected 1 section, got %d", len(result.Tier2.Sections))
	}
	if result.Tier2.Sections[0].Heading != "Introduction" {
		t.Errorf("section heading mismatch: %q", result.Tier2.Sections[0].Heading)
	}

	// Tier 3 raw
	if result.Tier3Raw != inputText {
		t.Errorf("tier 3 raw mismatch: %q", result.Tier3Raw)
	}

	// Metadata
	if result.InputHash == "" {
		t.Error("expected non-empty input hash")
	}
	if result.InputSize != len(inputText) {
		t.Errorf("input size mismatch: %d vs %d", result.InputSize, len(inputText))
	}
	if result.InputFormat != "plaintext" {
		t.Errorf("input format mismatch: %q", result.InputFormat)
	}
	if result.Metrics.CompressionRatio <= 0 {
		t.Error("expected positive compression ratio")
	}
	if result.Metrics.TotalLatencyMs < 0 {
		t.Error("expected non-negative total latency")
	}
}

// TestDecompRunnerRetryOnMalformedJSON verifies that the runner retries once on invalid JSON.
func TestDecompRunnerRetryOnMalformedJSON(t *testing.T) {
	var mu sync.Mutex
	callCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req agentChatRequest
		json.NewDecoder(r.Body).Decode(&req)

		var sysPrompt string
		for _, m := range req.Messages {
			if m.Role == "system" {
				sysPrompt = m.Content
				break
			}
		}

		mu.Lock()
		callCount++
		currentCall := callCount
		mu.Unlock()

		// First call for tier 0: return invalid JSON. Second call (retry): return valid.
		if currentCall == 1 && strings.Contains(sysPrompt, "one sentence") {
			w.Write(mockOllamaResponse(`{this is not valid json`))
			return
		}

		content := tierResponseForSystemPrompt(sysPrompt)
		w.Write(mockOllamaResponse(content))
	}))
	defer server.Close()

	harness := NewAgentHarness(AgentHarnessConfig{
		OllamaURL: server.URL,
		Model:     "test-model",
	})
	runner := NewDecompositionRunner(harness, nil)

	input := &DecompInput{Text: "Retry test input text.", Format: "plaintext", ByteSize: 22}
	result, err := runner.Run(context.Background(), input)
	if err != nil {
		t.Fatalf("Run should succeed after retry, got: %v", err)
	}

	if result.Tier0 == nil {
		t.Fatal("expected Tier0 to be populated after retry")
	}
	if result.Tier0.Summary == "" {
		t.Error("expected non-empty tier 0 summary after retry")
	}

	// Should have 4 calls: tier0 fail + tier0 retry + tier1 + tier2
	mu.Lock()
	defer mu.Unlock()
	if callCount != 4 {
		t.Errorf("expected 4 LLM calls (1 fail + 1 retry + tier1 + tier2), got %d", callCount)
	}
}

// TestDecompRunnerEventCallbackSequence verifies the correct sequence of bus events.
func TestDecompRunnerEventCallbackSequence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req agentChatRequest
		json.NewDecoder(r.Body).Decode(&req)

		var sysPrompt string
		for _, m := range req.Messages {
			if m.Role == "system" {
				sysPrompt = m.Content
				break
			}
		}
		content := tierResponseForSystemPrompt(sysPrompt)
		w.Write(mockOllamaResponse(content))
	}))
	defer server.Close()

	type eventRecord struct {
		eventType string
		payload   map[string]interface{}
	}
	var mu sync.Mutex
	var events []eventRecord

	callback := func(eventType string, payload map[string]interface{}) {
		mu.Lock()
		events = append(events, eventRecord{eventType, payload})
		mu.Unlock()
	}

	harness := NewAgentHarness(AgentHarnessConfig{
		OllamaURL: server.URL,
		Model:     "test-model",
	})
	runner := NewDecompositionRunner(harness, callback)

	input := &DecompInput{Text: "Event sequence test input.", Format: "plaintext", ByteSize: 26}
	_, err := runner.Run(context.Background(), input)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// Expected sequence:
	// decompose.start
	// decompose.tier.start (tier=0), decompose.tier.complete (tier=0)
	// decompose.tier.start (tier=1), decompose.tier.complete (tier=1)
	// decompose.tier.start (tier=2), decompose.tier.complete (tier=2)
	// decompose.complete
	expectedTypes := []string{
		DecompEventStart,
		DecompEventTierStart, DecompEventTierComplete,
		DecompEventTierStart, DecompEventTierComplete,
		DecompEventTierStart, DecompEventTierComplete,
		DecompEventComplete,
	}

	if len(events) != len(expectedTypes) {
		t.Fatalf("expected %d events, got %d: %v", len(expectedTypes), len(events), func() []string {
			var names []string
			for _, e := range events {
				names = append(names, e.eventType)
			}
			return names
		}())
	}

	for i, expected := range expectedTypes {
		if events[i].eventType != expected {
			t.Errorf("event[%d]: expected %q, got %q", i, expected, events[i].eventType)
		}
	}

	// Verify tier numbers in tier.start/tier.complete events
	tierEvents := []struct {
		idx  int
		tier float64
	}{
		{1, 0}, {2, 0}, // tier 0 start/complete
		{3, 1}, {4, 1}, // tier 1 start/complete
		{5, 2}, {6, 2}, // tier 2 start/complete
	}
	for _, te := range tierEvents {
		tierVal, ok := events[te.idx].payload["tier"]
		if !ok {
			t.Errorf("event[%d] missing 'tier' in payload", te.idx)
			continue
		}
		// payload values are interface{}, tier is int but compare as float64 for JSON compat
		if tierNum, ok := tierVal.(int); ok {
			if float64(tierNum) != te.tier {
				t.Errorf("event[%d]: expected tier=%v, got %v", te.idx, te.tier, tierNum)
			}
		} else {
			t.Errorf("event[%d]: tier is not int: %T", te.idx, tierVal)
		}
	}
}

// TestDecompRunnerSelectiveTiers verifies that only requested tiers are executed.
func TestDecompRunnerSelectiveTiers(t *testing.T) {
	var mu sync.Mutex
	callCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req agentChatRequest
		json.NewDecoder(r.Body).Decode(&req)

		mu.Lock()
		callCount++
		mu.Unlock()

		var sysPrompt string
		for _, m := range req.Messages {
			if m.Role == "system" {
				sysPrompt = m.Content
				break
			}
		}
		content := tierResponseForSystemPrompt(sysPrompt)
		w.Write(mockOllamaResponse(content))
	}))
	defer server.Close()

	type eventRecord struct {
		eventType string
		payload   map[string]interface{}
	}
	var eventMu sync.Mutex
	var events []eventRecord

	callback := func(eventType string, payload map[string]interface{}) {
		eventMu.Lock()
		events = append(events, eventRecord{eventType, payload})
		eventMu.Unlock()
	}

	harness := NewAgentHarness(AgentHarnessConfig{
		OllamaURL: server.URL,
		Model:     "test-model",
	})
	runner := NewDecompositionRunner(harness, callback)
	runner.tiers = []int{0, 2} // Skip tier 1, no tier 3

	inputText := "Selective tier test input."
	input := &DecompInput{Text: inputText, Format: "plaintext", ByteSize: len(inputText)}
	result, err := runner.Run(context.Background(), input)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	// Tier 0 should be populated
	if result.Tier0 == nil {
		t.Error("expected Tier0 to be populated")
	}

	// Tier 1 should be nil (skipped)
	if result.Tier1 != nil {
		t.Error("expected Tier1 to be nil (skipped)")
	}

	// Tier 2 should be populated
	if result.Tier2 == nil {
		t.Error("expected Tier2 to be populated")
	}

	// Tier 3 raw should be empty (tier 3 not requested)
	if result.Tier3Raw != "" {
		t.Errorf("expected empty Tier3Raw when tier 3 not requested, got %q", result.Tier3Raw)
	}

	// Only 2 LLM calls (tier 0 and tier 2)
	mu.Lock()
	if callCount != 2 {
		t.Errorf("expected 2 LLM calls, got %d", callCount)
	}
	mu.Unlock()

	// Verify events: start, tier0 start/complete, tier2 start/complete, complete
	eventMu.Lock()
	defer eventMu.Unlock()

	expectedTypes := []string{
		DecompEventStart,
		DecompEventTierStart, DecompEventTierComplete,
		DecompEventTierStart, DecompEventTierComplete,
		DecompEventComplete,
	}
	if len(events) != len(expectedTypes) {
		var names []string
		for _, e := range events {
			names = append(names, e.eventType)
		}
		t.Fatalf("expected %d events, got %d: %v", len(expectedTypes), len(events), names)
	}

	for i, expected := range expectedTypes {
		if events[i].eventType != expected {
			t.Errorf("event[%d]: expected %q, got %q", i, expected, events[i].eventType)
		}
	}

	// Verify tier numbers: should be 0 and 2, not 1
	tierStartEvents := []struct {
		idx  int
		tier int
	}{
		{1, 0}, {2, 0}, // tier 0
		{3, 2}, {4, 2}, // tier 2 (not tier 1)
	}
	for _, te := range tierStartEvents {
		tierVal, ok := events[te.idx].payload["tier"]
		if !ok {
			t.Errorf("event[%d] missing 'tier' in payload", te.idx)
			continue
		}
		if tierNum, ok := tierVal.(int); ok {
			if tierNum != te.tier {
				t.Errorf("event[%d]: expected tier=%d, got %d", te.idx, te.tier, tierNum)
			}
		}
	}
}

// === Wave 3 B3: Formatter Tests ===

func TestDecompFormatterOutput(t *testing.T) {
	result := &DecompositionResult{
		InputHash:   "abc123def456",
		InputSize:   2847,
		InputFormat: "markdown",
		Tier0:       &Tier0Result{Summary: "The CogOS kernel agent runs a homeostatic observation loop."},
		Tier1: &Tier1Result{
			Summary:  "The CogOS kernel agent implements a homeostatic loop that observes workspace state.",
			KeyTerms: []string{"kernel", "homeostatic", "agent", "observation"},
		},
		Tier2: &Tier2Result{
			Title:   "CogOS Kernel Agent Homeostatic Loop",
			Type:    "architecture",
			Tags:    []string{"kernel", "agent", "homeostatic"},
			Summary: "Detailed architecture overview.",
			Sections: []Tier2Section{
				{Heading: "Overview", Content: "The agent observes workspace state."},
				{Heading: "Design", Content: "Uses a reconciliation loop pattern."},
			},
			Refs: []Tier2Ref{
				{URI: "cog://mem/semantic/architecture/kernel", Relation: "extends"},
			},
		},
		Tier3Raw: "raw content here that is stored but not displayed in the output",
		Metrics: DecompMetrics{
			Tier0Tokens:      15,
			Tier0LatencyMs:   420,
			Tier1Tokens:      89,
			Tier1LatencyMs:   1200,
			Tier2Tokens:      342,
			Tier2LatencyMs:   3800,
			TotalLatencyMs:   5400,
			CompressionRatio: 190,
		},
	}

	var buf strings.Builder
	printDecompResultTo(&buf, result)
	output := buf.String()

	// Header checks
	if !strings.Contains(output, "Decomposition Results") {
		t.Error("expected 'Decomposition Results' header")
	}
	if !strings.Contains(output, "2,847 bytes") {
		t.Error("expected formatted byte count '2,847 bytes'")
	}
	if !strings.Contains(output, "abc123def456") {
		t.Error("expected input hash in output")
	}

	// Tier headers
	if !strings.Contains(output, "T0: Sentence") {
		t.Error("expected T0 tier header")
	}
	if !strings.Contains(output, "15 tok") {
		t.Error("expected tier 0 token count")
	}
	if !strings.Contains(output, "420ms") {
		t.Error("expected tier 0 latency")
	}

	if !strings.Contains(output, "T1: Paragraph") {
		t.Error("expected T1 tier header")
	}
	if !strings.Contains(output, "89 tok") {
		t.Error("expected tier 1 token count")
	}
	if !strings.Contains(output, "1.2s") {
		t.Error("expected tier 1 latency formatted as seconds")
	}
	if !strings.Contains(output, "Key terms:") {
		t.Error("expected key terms label")
	}

	if !strings.Contains(output, "T2: CogDoc") {
		t.Error("expected T2 tier header")
	}
	if !strings.Contains(output, "342 tok") {
		t.Error("expected tier 2 token count")
	}
	if !strings.Contains(output, "Title:") {
		t.Error("expected Title label in T2")
	}
	if !strings.Contains(output, "[Overview]") {
		t.Error("expected section heading inline")
	}
	if !strings.Contains(output, "cog://mem/semantic/architecture/kernel") {
		t.Error("expected ref URI in output")
	}

	if !strings.Contains(output, "T3: Raw") {
		t.Error("expected T3 tier header")
	}
	if !strings.Contains(output, "stored, not displayed") {
		t.Error("expected 'stored, not displayed' message for T3")
	}

	// Metrics footer
	if !strings.Contains(output, "Metrics") {
		t.Error("expected Metrics footer")
	}
	if !strings.Contains(output, "5.4s") {
		t.Error("expected total latency in footer")
	}
	if !strings.Contains(output, "Compression: 190:1") {
		t.Error("expected compression ratio in footer")
	}
}

func TestDecompFormatterPartialTiers(t *testing.T) {
	// Only T0, no other tiers
	result := &DecompositionResult{
		InputHash:   "deadbeef1234",
		InputSize:   100,
		InputFormat: "plaintext",
		Tier0:       &Tier0Result{Summary: "Simple summary."},
		Metrics: DecompMetrics{
			Tier0Tokens:    5,
			Tier0LatencyMs: 100,
			TotalLatencyMs: 100,
		},
	}

	var buf strings.Builder
	printDecompResultTo(&buf, result)
	output := buf.String()

	if !strings.Contains(output, "T0: Sentence") {
		t.Error("expected T0 header")
	}
	if strings.Contains(output, "T1:") {
		t.Error("should not contain T1 when not present")
	}
	if strings.Contains(output, "T2:") {
		t.Error("should not contain T2 when not present")
	}
	if strings.Contains(output, "T3:") {
		t.Error("should not contain T3 when not present")
	}
	// No compression ratio when zero
	if strings.Contains(output, "Compression:") {
		t.Error("should not show compression when ratio is 0")
	}
}

func TestDecompFormatLatency(t *testing.T) {
	cases := []struct {
		ms   int64
		want string
	}{
		{0, "0ms"},
		{420, "420ms"},
		{999, "999ms"},
		{1000, "1.0s"},
		{1200, "1.2s"},
		{5400, "5.4s"},
		{10500, "10.5s"},
	}
	for _, tc := range cases {
		got := formatLatency(tc.ms)
		if got != tc.want {
			t.Errorf("formatLatency(%d) = %q, want %q", tc.ms, got, tc.want)
		}
	}
}

func TestDecompFormatBytes(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{42, "42"},
		{999, "999"},
		{1000, "1,000"},
		{2847, "2,847"},
		{12345, "12,345"},
		{1234567, "1,234,567"},
	}
	for _, tc := range cases {
		got := decompFormatBytes(tc.n)
		if got != tc.want {
			t.Errorf("decompFormatBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// === Wave 3 D2: File Event Callback Tests ===

func TestDecompFileEventCallback(t *testing.T) {
	tmpDir := t.TempDir()

	// Create .cog directory structure
	cogDir := filepath.Join(tmpDir, ".cog")
	if err := os.MkdirAll(cogDir, 0755); err != nil {
		t.Fatalf("mkdir .cog: %v", err)
	}

	callback := newFileEventCallback(tmpDir)

	// Emit a few events
	callback(DecompEventStart, map[string]interface{}{
		"input_hash": "test123",
		"input_size": 100,
	})
	callback(DecompEventTierComplete, map[string]interface{}{
		"tier":       0,
		"latency_ms": 42,
	})
	callback(DecompEventComplete, map[string]interface{}{
		"input_hash":    "test123",
		"total_latency": 100,
	})

	// Read the events file
	eventsPath := filepath.Join(tmpDir, ".cog", ".state", "buses", "decompose", "events.jsonl")
	data, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read events file: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 event lines, got %d", len(lines))
	}

	// Parse first event
	var evt decompBusEvent
	if err := json.Unmarshal([]byte(lines[0]), &evt); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if evt.Type != DecompEventStart {
		t.Errorf("expected type %q, got %q", DecompEventStart, evt.Type)
	}
	if evt.From != "decompose" {
		t.Errorf("expected from 'decompose', got %q", evt.From)
	}
	if evt.Ts == "" {
		t.Error("expected non-empty timestamp")
	}
	if evt.Payload["input_hash"] != "test123" {
		t.Errorf("expected input_hash 'test123', got %v", evt.Payload["input_hash"])
	}

	// Parse last event
	var lastEvt decompBusEvent
	if err := json.Unmarshal([]byte(lines[2]), &lastEvt); err != nil {
		t.Fatalf("unmarshal last event: %v", err)
	}
	if lastEvt.Type != DecompEventComplete {
		t.Errorf("expected type %q, got %q", DecompEventComplete, lastEvt.Type)
	}
}

// === D4: Quality Metrics Tests ===

func TestDecompCosineSimilarity(t *testing.T) {
	// Identical vectors should give similarity ~1.0
	a := []float32{1, 0, 0, 0}
	b := []float32{1, 0, 0, 0}
	sim := cosineSimilarity(a, b)
	if sim < 0.999 || sim > 1.001 {
		t.Errorf("identical vectors: expected ~1.0, got %f", sim)
	}

	// Orthogonal vectors should give similarity ~0.0
	c := []float32{1, 0, 0, 0}
	d := []float32{0, 1, 0, 0}
	sim = cosineSimilarity(c, d)
	if sim < -0.001 || sim > 0.001 {
		t.Errorf("orthogonal vectors: expected ~0.0, got %f", sim)
	}

	// Opposite vectors should give similarity ~-1.0
	e := []float32{1, 0, 0, 0}
	f := []float32{-1, 0, 0, 0}
	sim = cosineSimilarity(e, f)
	if sim < -1.001 || sim > -0.999 {
		t.Errorf("opposite vectors: expected ~-1.0, got %f", sim)
	}

	// Empty vectors should give 0.0
	sim = cosineSimilarity(nil, a)
	if sim != 0.0 {
		t.Errorf("nil vector: expected 0.0, got %f", sim)
	}
	sim = cosineSimilarity([]float32{}, a)
	if sim != 0.0 {
		t.Errorf("empty vector: expected 0.0, got %f", sim)
	}

	// Zero vector should give 0.0
	sim = cosineSimilarity([]float32{0, 0, 0}, a)
	if sim != 0.0 {
		t.Errorf("zero vector: expected 0.0, got %f", sim)
	}
}

func TestDecompComputeQuality(t *testing.T) {
	result := &DecompositionResult{
		Metrics: DecompMetrics{CompressionRatio: 42.5},
		Embeddings: &DecompEmbeddings{
			Tier0_128: make([]float32, 128),
			Tier1_128: make([]float32, 128),
			Tier2_128: make([]float32, 128),
		},
	}
	// Set some values so cosine similarity is meaningful
	result.Embeddings.Tier0_128[0] = 1.0
	result.Embeddings.Tier1_128[0] = 0.8
	result.Embeddings.Tier1_128[1] = 0.6
	result.Embeddings.Tier2_128[0] = 1.0

	q := computeQuality(result)
	if q.CompressionRatio != 42.5 {
		t.Errorf("compression ratio mismatch: got %f", q.CompressionRatio)
	}
	if q.Tier0Fidelity < 0.99 {
		t.Errorf("tier0 fidelity should be ~1.0 (parallel vectors), got %f", q.Tier0Fidelity)
	}
	if q.Tier1Fidelity < 0.5 || q.Tier1Fidelity > 0.95 {
		t.Errorf("tier1 fidelity should be moderate, got %f", q.Tier1Fidelity)
	}
	if !q.SchemaConformant {
		t.Error("default schema conformant should be true")
	}
}

func TestDecompComputeQualityNoEmbeddings(t *testing.T) {
	result := &DecompositionResult{
		Metrics: DecompMetrics{CompressionRatio: 10.0},
	}
	q := computeQuality(result)
	if q.Tier0Fidelity != 0.0 {
		t.Errorf("expected 0.0 fidelity without embeddings, got %f", q.Tier0Fidelity)
	}
	if q.Tier1Fidelity != 0.0 {
		t.Errorf("expected 0.0 fidelity without embeddings, got %f", q.Tier1Fidelity)
	}
}

// === E4: Bus Event File Sequence Test ===

func TestDecompBusEventFileSequence(t *testing.T) {
	// Create temp workspace with .cog/ directory structure
	tmpDir := t.TempDir()
	cogDir := filepath.Join(tmpDir, ".cog")
	if err := os.MkdirAll(cogDir, 0755); err != nil {
		t.Fatalf("mkdir .cog: %v", err)
	}

	// Set up a mock Ollama server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req agentChatRequest
		json.NewDecoder(r.Body).Decode(&req)

		var sysPrompt string
		for _, m := range req.Messages {
			if m.Role == "system" {
				sysPrompt = m.Content
				break
			}
		}
		content := tierResponseForSystemPrompt(sysPrompt)
		w.Write(mockOllamaResponse(content))
	}))
	defer server.Close()

	// Create runner with the file-based event callback
	harness := NewAgentHarness(AgentHarnessConfig{
		OllamaURL: server.URL,
		Model:     "test-model",
	})
	callback := newFileEventCallback(tmpDir)
	runner := NewDecompositionRunner(harness, callback)

	// Run decomposition
	inputText := "The bus event sequence test verifies that decomposition events are written to JSONL files in the correct order."
	input := &DecompInput{Text: inputText, Format: "plaintext", ByteSize: len(inputText)}
	_, err := runner.Run(context.Background(), input)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	// Read back the JSONL file
	eventsPath := filepath.Join(tmpDir, ".cog", ".state", "buses", "decompose", "events.jsonl")
	data, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read events file: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	// Expected event sequence:
	// decompose.start
	// decompose.tier.start (tier=0)
	// decompose.tier.complete (tier=0)
	// decompose.tier.start (tier=1)
	// decompose.tier.complete (tier=1)
	// decompose.tier.start (tier=2)
	// decompose.tier.complete (tier=2)
	// decompose.complete
	expectedTypes := []string{
		DecompEventStart,
		DecompEventTierStart, DecompEventTierComplete,
		DecompEventTierStart, DecompEventTierComplete,
		DecompEventTierStart, DecompEventTierComplete,
		DecompEventComplete,
	}

	if len(lines) != len(expectedTypes) {
		var gotTypes []string
		for _, line := range lines {
			var evt decompBusEvent
			json.Unmarshal([]byte(line), &evt)
			gotTypes = append(gotTypes, evt.Type)
		}
		t.Fatalf("expected %d event lines, got %d: %v", len(expectedTypes), len(lines), gotTypes)
	}

	// Parse and verify each event
	var events []decompBusEvent
	for i, line := range lines {
		var evt decompBusEvent
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			t.Fatalf("unmarshal event line %d: %v", i, err)
		}
		events = append(events, evt)
	}

	// Verify event types in order
	for i, expected := range expectedTypes {
		if events[i].Type != expected {
			t.Errorf("event[%d]: expected type %q, got %q", i, expected, events[i].Type)
		}
	}

	// Verify all events have common fields
	for i, evt := range events {
		if evt.Ts == "" {
			t.Errorf("event[%d]: expected non-empty timestamp", i)
		}
		if evt.From != "decompose" {
			t.Errorf("event[%d]: expected from='decompose', got %q", i, evt.From)
		}
	}

	// Verify tier numbers in tier.start/tier.complete events
	tierChecks := []struct {
		idx  int
		tier float64
	}{
		{1, 0}, {2, 0}, // tier 0 start/complete
		{3, 1}, {4, 1}, // tier 1 start/complete
		{5, 2}, {6, 2}, // tier 2 start/complete
	}
	for _, tc := range tierChecks {
		tierVal, ok := events[tc.idx].Payload["tier"]
		if !ok {
			t.Errorf("event[%d] missing 'tier' in payload", tc.idx)
			continue
		}
		// JSON numbers decode as float64
		tierNum, ok := tierVal.(float64)
		if !ok {
			t.Errorf("event[%d]: tier is not float64: %T", tc.idx, tierVal)
			continue
		}
		if tierNum != tc.tier {
			t.Errorf("event[%d]: expected tier=%v, got %v", tc.idx, tc.tier, tierNum)
		}
	}

	// Verify tier.complete events have latency_ms
	for _, idx := range []int{2, 4, 6} {
		if _, ok := events[idx].Payload["latency_ms"]; !ok {
			t.Errorf("event[%d] (tier.complete) missing 'latency_ms' in payload", idx)
		}
	}

	// Verify decompose.complete has expected fields from enriched payload
	completePayload := events[7].Payload
	requiredFields := []string{"input_hash", "total_latency", "tier0_tokens", "tier0_latency_ms",
		"tier1_tokens", "tier1_latency_ms", "tier2_tokens", "tier2_latency_ms",
		"compression_ratio", "schema_conformant"}
	for _, field := range requiredFields {
		if _, ok := completePayload[field]; !ok {
			t.Errorf("decompose.complete missing field %q in payload", field)
		}
	}

	// Verify start event has input metadata
	startPayload := events[0].Payload
	if _, ok := startPayload["input_hash"]; !ok {
		t.Error("decompose.start missing 'input_hash'")
	}
	if _, ok := startPayload["input_size"]; !ok {
		t.Error("decompose.start missing 'input_size'")
	}
}

func TestDecompFileEventCallbackNoopOnBadDir(t *testing.T) {
	// Use a path that cannot be created (file as parent)
	tmpDir := t.TempDir()
	blocker := filepath.Join(tmpDir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a dir"), 0644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	// This should return a no-op callback without panic
	callback := newFileEventCallback(blocker)
	// Calling the no-op callback should not panic
	callback("test.event", map[string]interface{}{"key": "value"})
}

// TestDecompAcknowledgeProposal verifies that the acknowledge_proposal tool
// correctly transitions a proposal from pending to the requested status.
func TestDecompAcknowledgeProposal(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, proposalsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write a pending proposal
	proposal := `---
id: 20260415-120000
type: observation
title: "Test proposal"
target: ""
urgency: 0.5
created: 2026-04-15T12:00:00Z
status: pending
---

# Test proposal

This is a test proposal body.
`
	filename := "20260415-120000-test-proposal.md"
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(proposal), 0o644); err != nil {
		t.Fatal(err)
	}

	// Call the acknowledge function
	fn := newAcknowledgeProposalFunc(tmp)
	args, _ := json.Marshal(map[string]string{
		"filename": filename,
		"status":   "acknowledged",
	})
	result, err := fn(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var resp map[string]string
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp["status"] != "acknowledged" {
		t.Errorf("expected status=acknowledged, got %q", resp["status"])
	}

	// Verify the file was updated
	updated, err := os.ReadFile(filepath.Join(dir, filename))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(updated), "status: pending") {
		t.Error("file still contains 'status: pending'")
	}
	if !strings.Contains(string(updated), "status: acknowledged") {
		t.Error("file does not contain 'status: acknowledged'")
	}

	// Test with a note
	fn2 := newAcknowledgeProposalFunc(tmp)
	args2, _ := json.Marshal(map[string]string{
		"filename": filename,
		"status":   "approved",
		"note":     "Looks good, proceed.",
	})
	result2, err := fn2(context.Background(), args2)
	if err != nil {
		t.Fatalf("unexpected error on second call: %v", err)
	}
	var resp2 map[string]string
	json.Unmarshal(result2, &resp2)
	if resp2["status"] != "approved" {
		t.Errorf("expected status=approved, got %q", resp2["status"])
	}

	// Verify note was appended
	final, _ := os.ReadFile(filepath.Join(dir, filename))
	if !strings.Contains(string(final), "## Operator Note") {
		t.Error("file does not contain operator note header")
	}
	if !strings.Contains(string(final), "Looks good, proceed.") {
		t.Error("file does not contain the note text")
	}
}
