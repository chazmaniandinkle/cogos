package main

import (
	"testing"
	"time"
)

func TestDecompTUIModelInit(t *testing.T) {
	input := &DecompInput{
		Text:      "Test input for decomposition workbench.",
		Format:    "plaintext",
		SourceURI: "file://test.md",
		ByteSize:  39,
	}

	result := &DecompositionResult{
		InputHash:   "abc123def456",
		InputSize:   39,
		InputFormat: "plaintext",
		SourceURI:   "file://test.md",
		Tier0:       &Tier0Result{Summary: "A test summary."},
		Tier1: &Tier1Result{
			Summary:  "A longer test summary with more detail.",
			KeyTerms: []string{"test", "decomposition"},
		},
		Tier2: &Tier2Result{
			Title:   "Test Document",
			Type:    "knowledge",
			Tags:    []string{"test"},
			Summary: "Test summary for tier 2.",
			Sections: []Tier2Section{
				{Heading: "Overview", Content: "Some content."},
			},
		},
		Tier3Raw: "Test input for decomposition workbench.",
		Metrics: DecompMetrics{
			Tier0Tokens:      4,
			Tier0LatencyMs:   100,
			Tier1Tokens:      10,
			Tier1LatencyMs:   200,
			Tier2Tokens:      25,
			Tier2LatencyMs:   500,
			TotalLatencyMs:   800,
			CompressionRatio: 2.6,
		},
		CreatedAt: time.Now(),
	}

	// Runner can be nil for init test — we only check model state.
	m := initialDecompTUIModel(input, result, nil)

	// Verify initial state
	if m.active != 0 {
		t.Errorf("expected active=0, got %d", m.active)
	}
	if m.running {
		t.Error("expected running=false")
	}
	if m.err != nil {
		t.Errorf("expected nil error, got %v", m.err)
	}
	if m.input != input {
		t.Error("input not stored")
	}
	if m.result != result {
		t.Error("result not stored")
	}

	// Verify viewports were populated
	t0Content := m.viewports[0].View()
	if t0Content == "" {
		t.Error("T0 viewport should have content after populate")
	}

	// Init should return nil (no initial command)
	cmd := m.Init()
	if cmd != nil {
		t.Error("Init() should return nil")
	}
}

func TestDecompTUIModelNilResult(t *testing.T) {
	input := &DecompInput{
		Text:     "Some text",
		Format:   "plaintext",
		ByteSize: 9,
	}

	m := initialDecompTUIModel(input, nil, nil)

	if m.result != nil {
		t.Error("expected nil result")
	}
	if m.active != 0 {
		t.Errorf("expected active=0, got %d", m.active)
	}
}
