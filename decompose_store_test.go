package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestDecompBuildCogDoc(t *testing.T) {
	result := &DecompositionResult{
		InputHash:   "abcdef123456",
		InputSize:   2048,
		InputFormat: "markdown",
		SourceURI:   "file:///tmp/test.md",
		Tier0:       &Tier0Result{Summary: "A test document about decomposition."},
		Tier1: &Tier1Result{
			Summary:  "This document covers the CogOS decomposition pipeline in detail.",
			KeyTerms: []string{"decomposition", "pipeline", "CogOS"},
		},
		Tier2: &Tier2Result{
			Title:   "Decomposition Pipeline Overview",
			Type:    "architecture",
			Tags:    []string{"decompose", "pipeline", "cogos"},
			Summary: "A structured overview of the decomposition pipeline.",
			Sections: []Tier2Section{
				{Heading: "Overview", Content: "The pipeline processes text into tiers."},
				{Heading: "Implementation", Content: "Uses Ollama for LLM calls."},
			},
			Refs: []Tier2Ref{
				{URI: "cog://mem/semantic/architecture/pipeline", Relation: "extends"},
			},
		},
		Metrics: DecompMetrics{
			CompressionRatio: 15.3,
			TotalLatencyMs:   250,
		},
		CreatedAt: time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC),
	}

	doc := buildCogDoc(result)

	// Check YAML frontmatter
	if !strings.HasPrefix(doc, "---\n") {
		t.Error("CogDoc should start with YAML frontmatter delimiter")
	}
	if !strings.Contains(doc, "---\n\n") {
		t.Error("CogDoc should have closing frontmatter delimiter followed by blank line")
	}

	// Check frontmatter fields
	if !strings.Contains(doc, `title: "Decomposition Pipeline Overview"`) {
		t.Error("frontmatter should contain the tier 2 title")
	}
	if !strings.Contains(doc, "type: decomposition") {
		t.Error("frontmatter should contain type: decomposition")
	}
	if !strings.Contains(doc, "created: 2026-04-15T12:00:00Z") {
		t.Error("frontmatter should contain created timestamp")
	}
	if !strings.Contains(doc, "status: active") {
		t.Error("frontmatter should contain status: active")
	}
	if !strings.Contains(doc, "salience: medium") {
		t.Error("frontmatter should contain salience: medium")
	}
	if !strings.Contains(doc, "memory_sector: semantic") {
		t.Error("frontmatter should contain memory_sector: semantic")
	}
	if !strings.Contains(doc, `"decompose"`) {
		t.Error("frontmatter should contain tags")
	}
	if !strings.Contains(doc, `uri: "file:///tmp/test.md"`) {
		t.Error("frontmatter should contain source URI ref")
	}
	if !strings.Contains(doc, "rel: decomposes") {
		t.Error("frontmatter should contain rel: decomposes")
	}
	if !strings.Contains(doc, "sections: [tier-0, tier-1, tier-2, metadata]") {
		t.Error("frontmatter should list all 4 sections")
	}

	// Check all 4 body sections exist
	if !strings.Contains(doc, "# Tier 0 — Sentence") {
		t.Error("CogDoc should contain Tier 0 section")
	}
	if !strings.Contains(doc, "A test document about decomposition.") {
		t.Error("CogDoc should contain tier 0 summary text")
	}

	if !strings.Contains(doc, "# Tier 1 — Paragraph") {
		t.Error("CogDoc should contain Tier 1 section")
	}
	if !strings.Contains(doc, "**Key terms:** decomposition, pipeline, CogOS") {
		t.Error("CogDoc should contain key terms")
	}

	if !strings.Contains(doc, "# Tier 2 — Structured") {
		t.Error("CogDoc should contain Tier 2 section")
	}
	if !strings.Contains(doc, "## Overview") {
		t.Error("CogDoc should contain tier 2 section headings")
	}
	if !strings.Contains(doc, "## Implementation") {
		t.Error("CogDoc should contain tier 2 section headings")
	}

	if !strings.Contains(doc, "# Metadata") {
		t.Error("CogDoc should contain Metadata section")
	}
	if !strings.Contains(doc, "- Input: 2048 bytes (markdown)") {
		t.Error("CogDoc should contain input metadata")
	}
	if !strings.Contains(doc, "- Hash: abcdef123456") {
		t.Error("CogDoc should contain hash")
	}
	if !strings.Contains(doc, "- Compression: 15.3:1") {
		t.Error("CogDoc should contain compression ratio")
	}
}

func TestDecompBuildCogDocMinimal(t *testing.T) {
	// Minimal result with only Tier 0, no Tier 1/2
	result := &DecompositionResult{
		InputHash:   "deadbeef0000",
		InputSize:   100,
		InputFormat: "plaintext",
		Tier0:       &Tier0Result{Summary: "A short note."},
		CreatedAt:   time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC),
	}

	doc := buildCogDoc(result)

	// Title should fall back to hash-based
	if !strings.Contains(doc, `title: "Decomposition of deadbeef0000"`) {
		t.Error("minimal CogDoc should use hash-based title")
	}

	// Should have placeholder text for missing tiers
	if !strings.Contains(doc, "_(not generated)_") {
		t.Error("missing tiers should show placeholder text")
	}

	// Tags should be empty array
	if !strings.Contains(doc, "tags: []") {
		t.Error("minimal CogDoc should have empty tags array")
	}

	// Source URI should use hash scheme
	if !strings.Contains(doc, `uri: "hash://deadbeef0000"`) {
		t.Error("minimal CogDoc should use hash-based source URI")
	}
}

func TestDecompIndexInConstellationMissingRoot(t *testing.T) {
	// indexInConstellation should return an error (not panic) when the
	// workspace root doesn't contain a valid .cog directory or when the
	// cogdoc file doesn't exist.
	err := indexInConstellation("/nonexistent/workspace/root", "/nonexistent/file.cog.md")
	if err == nil {
		t.Fatal("expected error for nonexistent workspace root, got nil")
	}
}

func TestDecompIndexInConstellationBadFile(t *testing.T) {
	// Create a temp workspace with .cog/.state so constellation.Open succeeds,
	// but point to a nonexistent cogdoc file.
	tmpDir := t.TempDir()
	stateDir := tmpDir + "/.cog/.state"
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	err := indexInConstellation(tmpDir, tmpDir+"/nonexistent.cog.md")
	if err == nil {
		t.Fatal("expected error for nonexistent cogdoc file, got nil")
	}
}

func TestDecompIndexInConstellationSuccess(t *testing.T) {
	// Create a temp workspace with a valid cogdoc and verify it indexes.
	tmpDir := t.TempDir()
	stateDir := tmpDir + "/.cog/.state"
	memDir := tmpDir + "/.cog/mem/semantic/decompositions"
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cogdoc := `---
title: "Test Doc"
type: decomposition
created: 2026-04-15T12:00:00Z
status: active
tags: ["test"]
---

# Tier 0 — Sentence
A test document.
`
	cogdocPath := memDir + "/abc123.cog.md"
	if err := os.WriteFile(cogdocPath, []byte(cogdoc), 0o644); err != nil {
		t.Fatal(err)
	}

	err := indexInConstellation(tmpDir, cogdocPath)
	if err != nil {
		// FTS5 requires a build tag (-tags fts5). Skip if unavailable.
		if strings.Contains(err.Error(), "no such module: fts5") {
			t.Skip("FTS5 not available (build with -tags fts5)")
		}
		t.Fatalf("expected successful indexing, got error: %v", err)
	}
}

func TestDecompTruncateVec(t *testing.T) {
	v := make([]float32, 768)
	for i := range v {
		v[i] = float32(i)
	}

	t128 := truncateVec(v, 128)
	if len(t128) != 128 {
		t.Errorf("expected 128 dims, got %d", len(t128))
	}
	if t128[0] != 0 || t128[127] != 127 {
		t.Error("truncated vector should preserve first N elements")
	}

	// Truncating to larger size should return original
	short := []float32{1, 2, 3}
	result := truncateVec(short, 128)
	if len(result) != 3 {
		t.Errorf("truncating short vec should return original, got len %d", len(result))
	}
}
