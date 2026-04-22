// decompose.go — Decomposition Pipeline for CogOS
//
// Decomposes input text into a tiered representation:
//   Tier 0: one-sentence summary (~15 tokens)
//   Tier 1: paragraph summary + key terms (~100 tokens)
//   Tier 2: full CogDoc structure (title, type, tags, sections, refs)
//   Tier 3: raw passthrough (no LLM call)
//
// Uses AgentHarness.GenerateJSON() against a local model (Gemma E4B via Ollama).

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// === Wave 0: Foundation — Tier JSON Schemas ===

// Tier0Result is a one-sentence summary (~15 tokens).
type Tier0Result struct {
	Summary string `json:"summary"`
}

// Tier1Result is a paragraph summary with key terms (~100 tokens).
type Tier1Result struct {
	Summary  string   `json:"summary"`
	KeyTerms []string `json:"key_terms"`
}

// Tier2Result is a full CogDoc structure.
type Tier2Result struct {
	Title    string   `json:"title"`
	Type     string   `json:"type"` // knowledge, architecture, etc
	Tags     []string `json:"tags"`
	Summary  string   `json:"summary"`
	Sections []Tier2Section `json:"sections"`
	Refs     []Tier2Ref     `json:"refs,omitempty"`
}

// Tier2Section is a single section in the Tier 2 document.
type Tier2Section struct {
	Heading string `json:"heading"`
	Content string `json:"content"`
}

// Tier2Ref is a reference link in the Tier 2 document.
type Tier2Ref struct {
	URI      string `json:"uri"`
	Relation string `json:"relation"`
}

// Tier 3 is raw passthrough — no schema needed, just store the input.

// === Wave 0: Foundation — DecompositionResult ===

// DecompositionResult holds the complete output of a decomposition run.
type DecompositionResult struct {
	InputHash   string            `json:"input_hash"`
	InputSize   int               `json:"input_size"`
	InputFormat string            `json:"input_format"`
	SourceURI   string            `json:"source_uri,omitempty"`
	Tier0       *Tier0Result      `json:"tier_0,omitempty"`
	Tier1       *Tier1Result      `json:"tier_1,omitempty"`
	Tier2       *Tier2Result      `json:"tier_2,omitempty"`
	Tier3Raw    string            `json:"tier_3_raw,omitempty"`
	Embeddings  *DecompEmbeddings `json:"embeddings,omitempty"`
	Quality     *DecompQuality    `json:"quality,omitempty"`
	Metrics     DecompMetrics     `json:"metrics"`
	CreatedAt   time.Time         `json:"created_at"`
}

// DecompQuality holds quality metrics computed after decomposition completes.
type DecompQuality struct {
	CompressionRatio float64 `json:"compression_ratio"` // input chars / tier0 chars
	Tier0Fidelity    float64 `json:"tier0_fidelity"`    // cosine sim(T0 embed, T2 embed)
	Tier1Fidelity    float64 `json:"tier1_fidelity"`    // cosine sim(T1 embed, T2 embed)
	SchemaConformant bool    `json:"schema_conformant"` // all tiers parsed cleanly
}

// cosineSimilarity computes the cosine similarity between two float32 vectors.
// Returns 0.0 if either vector is nil/empty or if norms are zero.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0.0
	}
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var dot, normA, normB float64
	for i := 0; i < n; i++ {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0.0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// computeQuality computes quality metrics from a completed decomposition result.
func computeQuality(result *DecompositionResult) *DecompQuality {
	q := &DecompQuality{
		CompressionRatio: result.Metrics.CompressionRatio,
		SchemaConformant: true, // overridden to false if any tier needed a retry
	}
	if result.Embeddings != nil {
		if len(result.Embeddings.Tier0_128) > 0 && len(result.Embeddings.Tier2_128) > 0 {
			q.Tier0Fidelity = cosineSimilarity(result.Embeddings.Tier0_128, result.Embeddings.Tier2_128)
		}
		if len(result.Embeddings.Tier1_128) > 0 && len(result.Embeddings.Tier2_128) > 0 {
			q.Tier1Fidelity = cosineSimilarity(result.Embeddings.Tier1_128, result.Embeddings.Tier2_128)
		}
	}
	return q
}

// DecompEmbeddings holds embedding vectors at different tier/dimensionalities.
type DecompEmbeddings struct {
	Tier0_128 []float32 `json:"tier0_128,omitempty"`
	Tier1_128 []float32 `json:"tier1_128,omitempty"`
	Tier2_128 []float32 `json:"tier2_128,omitempty"`
	Tier2_768 []float32 `json:"tier2_768,omitempty"`
}

// DecompMetrics records timing and compression data for each tier.
type DecompMetrics struct {
	Tier0Tokens      int     `json:"tier0_tokens"`
	Tier0LatencyMs   int64   `json:"tier0_latency_ms"`
	Tier1Tokens      int     `json:"tier1_tokens"`
	Tier1LatencyMs   int64   `json:"tier1_latency_ms"`
	Tier2Tokens      int     `json:"tier2_tokens"`
	Tier2LatencyMs   int64   `json:"tier2_latency_ms"`
	TotalLatencyMs   int64   `json:"total_latency_ms"`
	CompressionRatio float64 `json:"compression_ratio"`
}

// === Wave 0: Foundation — Input Normalization ===

// DecompInput is the normalized input for decomposition.
type DecompInput struct {
	Text      string
	Format    string // "markdown", "plaintext", "conversation"
	SourceURI string // set if from file
	ByteSize  int
}

// normalizeDecompInput reads input from args (file path) or stdin.
func normalizeDecompInput(args []string, stdin io.Reader) (*DecompInput, error) {
	var text string
	var sourceURI string

	if len(args) > 0 && args[0] != "-" {
		// Read from file
		path := args[0]
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read input file: %w", err)
		}
		text = string(data)
		sourceURI = "file://" + path
	} else {
		// Read from stdin (explicit "-" or piped stdin)
		fi, err := os.Stdin.Stat()
		if err != nil {
			return nil, fmt.Errorf("stat stdin: %w", err)
		}
		if len(args) > 0 && args[0] == "-" || (fi.Mode()&os.ModeCharDevice) == 0 {
			data, err := io.ReadAll(stdin)
			if err != nil {
				return nil, fmt.Errorf("read stdin: %w", err)
			}
			text = string(data)
		} else {
			return nil, fmt.Errorf("no input provided: pass a file path, pipe stdin, or use '-' for stdin")
		}
	}

	if len(strings.TrimSpace(text)) == 0 {
		return nil, fmt.Errorf("empty input")
	}

	format := detectFormat(text)

	if len(text) > 8192 {
		fmt.Fprintf(os.Stderr, "Warning: input is %d chars (>8K), chunking is future work\n", len(text))
	}

	return &DecompInput{
		Text:      text,
		Format:    format,
		SourceURI: sourceURI,
		ByteSize:  len(text),
	}, nil
}

// detectFormat guesses whether input is markdown, conversation, or plaintext.
func detectFormat(text string) string {
	// Check for markdown indicators
	if strings.Contains(text, "\n# ") || strings.Contains(text, "\n## ") ||
		strings.HasPrefix(text, "# ") || strings.Contains(text, "```") ||
		strings.Contains(text, "\n- ") || strings.Contains(text, "\n* ") {
		return "markdown"
	}
	// Check for conversation markers
	if strings.Contains(text, "\nHuman:") || strings.Contains(text, "\nAssistant:") ||
		strings.Contains(text, "\nUser:") || strings.Contains(text, "\nAI:") {
		return "conversation"
	}
	return "plaintext"
}

// === Wave 0: Foundation — Bus Event Types ===

const (
	DecompEventStart        = "decompose.start"
	DecompEventTierStart    = "decompose.tier.start"
	DecompEventTierComplete = "decompose.tier.complete"
	DecompEventComplete     = "decompose.complete"
	DecompEventError        = "decompose.error"
)

// DecompEventCallback is the bus event emission interface.
type DecompEventCallback func(eventType string, payload map[string]interface{})

// === Wave 1: Assembly — Prompt Templates ===

func tier0SystemPrompt() string {
	return `Summarize the following text in exactly one sentence of 15 words maximum. Capture the core meaning as concisely as possible.

Example of good output: {"summary": "CogOS decomposes text into tiered summaries for efficient memory indexing."}

Do not include markdown formatting in JSON string values.
Respond as JSON: {"summary": "..."}`
}

func tier1SystemPrompt() string {
	return `Write a paragraph summary of the following text in 3-5 sentences, then list 3-5 key terms that capture the main concepts.

Do not include markdown formatting in JSON string values.
Respond as JSON: {"summary": "your 3-5 sentence paragraph here", "key_terms": ["term1", "term2", "term3"]}`
}

func tier2SystemPrompt() string {
	return `Produce a structured document from the following text. Respond as JSON with this exact structure:

{"title": "short descriptive title", "type": "knowledge", "tags": ["tag1", "tag2"], "summary": "paragraph summary", "sections": [{"heading": "Section Title", "content": "section body text"}], "refs": []}

Field requirements:
- title: short descriptive title (5-10 words)
- type: one of "knowledge", "architecture", "procedure", "insight", "reference"
- tags: 2-5 lowercase tags
- summary: one paragraph summarizing the content
- sections: array of objects with "heading" (string) and "content" (string) capturing the logical structure
- refs: array of objects with "uri" and "relation" fields; only include if the text explicitly references other documents; use empty array [] otherwise

Do not include markdown formatting in JSON string values. All values must be plain text strings.`
}

func tierUserPrompt(input string) string {
	return "Decompose the following:\n\n" + input
}

// parseTierFlag parses the --tier flag value into a slice of tier numbers.
func parseTierFlag(flag string) ([]int, error) {
	if flag == "all" {
		return []int{0, 1, 2, 3}, nil
	}
	parts := strings.Split(flag, ",")
	var tiers []int
	for _, p := range parts {
		p = strings.TrimSpace(p)
		switch p {
		case "0":
			tiers = append(tiers, 0)
		case "1":
			tiers = append(tiers, 1)
		case "2":
			tiers = append(tiers, 2)
		case "3":
			tiers = append(tiers, 3)
		default:
			return nil, fmt.Errorf("invalid tier %q: must be 0, 1, 2, 3, all, or comma-separated", p)
		}
	}
	if len(tiers) == 0 {
		return nil, fmt.Errorf("no tiers specified")
	}
	return tiers, nil
}

// === Wave 3 B3: CLI Output Formatter ===

// ANSI color codes for decompose terminal output.
const (
	decompReset      = "\033[0m"
	decompBold       = "\033[1m"
	decompDim        = "\033[2m"
	decompMagenta    = "\033[0;35m"
	decompBoldCyan   = "\033[1;36m"
	decompBoldGreen  = "\033[1;32m"
	decompBoldYellow = "\033[1;33m"
	decompBoldBlue   = "\033[1;34m"
)

// decompTierColor returns the ANSI color for a given tier level.
func decompTierColor(tier int) string {
	switch tier {
	case 0:
		return decompBoldCyan
	case 1:
		return decompBoldGreen
	case 2:
		return decompBoldYellow
	case 3:
		return decompBoldBlue
	default:
		return decompBold
	}
}

// formatLatency formats milliseconds into a human-readable string.
func formatLatency(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000.0)
}

// decompFormatBytes formats a byte count with comma separators.
func decompFormatBytes(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 1000000 {
		return fmt.Sprintf("%d,%03d", n/1000, n%1000)
	}
	return fmt.Sprintf("%d,%03d,%03d", n/1000000, (n/1000)%1000, n%1000)
}

// printDecompResult prints a polished, ANSI-colored summary of the decomposition.
func printDecompResult(r *DecompositionResult) {
	printDecompResultTo(os.Stdout, r)
}

// printDecompResultTo writes the formatted decomposition result to the given writer.
func printDecompResultTo(w io.Writer, r *DecompositionResult) {
	// Header
	fmt.Fprintf(w, "\n%s═══ Decomposition Results ═══%s\n", decompBoldCyan, decompReset)
	fmt.Fprintf(w, "Input: %s bytes (%s) %s[%s]%s\n",
		decompFormatBytes(r.InputSize), r.InputFormat, decompDim, r.InputHash, decompReset)
	if r.SourceURI != "" {
		fmt.Fprintf(w, "Source: %s\n", r.SourceURI)
	}

	// Tier 0
	if r.Tier0 != nil {
		color := decompTierColor(0)
		fmt.Fprintf(w, "\n%s── T0: Sentence (%d tok, %s) ──%s\n",
			color, r.Metrics.Tier0Tokens, formatLatency(r.Metrics.Tier0LatencyMs), decompReset)
		fmt.Fprintf(w, "%s\n", r.Tier0.Summary)
	}

	// Tier 1
	if r.Tier1 != nil {
		color := decompTierColor(1)
		fmt.Fprintf(w, "\n%s── T1: Paragraph (%d tok, %s) ──%s\n",
			color, r.Metrics.Tier1Tokens, formatLatency(r.Metrics.Tier1LatencyMs), decompReset)
		fmt.Fprintf(w, "%s\n", r.Tier1.Summary)
		if len(r.Tier1.KeyTerms) > 0 {
			fmt.Fprintf(w, "%sKey terms:%s %s\n", decompDim, decompReset, strings.Join(r.Tier1.KeyTerms, ", "))
		}
	}

	// Tier 2
	if r.Tier2 != nil {
		color := decompTierColor(2)
		fmt.Fprintf(w, "\n%s── T2: CogDoc (%d tok, %s) ──%s\n",
			color, r.Metrics.Tier2Tokens, formatLatency(r.Metrics.Tier2LatencyMs), decompReset)
		fmt.Fprintf(w, "%sTitle:%s %s\n", decompDim, decompReset, r.Tier2.Title)
		fmt.Fprintf(w, "%sType:%s  %s\n", decompDim, decompReset, r.Tier2.Type)
		fmt.Fprintf(w, "%sTags:%s  %s\n", decompDim, decompReset, strings.Join(r.Tier2.Tags, ", "))

		// Section headings inline
		if len(r.Tier2.Sections) > 0 {
			var headings []string
			for _, s := range r.Tier2.Sections {
				headings = append(headings, "["+s.Heading+"]")
			}
			fmt.Fprintf(w, "%sSections:%s %s\n", decompDim, decompReset, strings.Join(headings, " "))
			// Section content
			for _, s := range r.Tier2.Sections {
				fmt.Fprintf(w, "\n  %s%s%s\n", decompBold, s.Heading, decompReset)
				fmt.Fprintf(w, "  %s\n", s.Content)
			}
		}

		// Refs
		if len(r.Tier2.Refs) > 0 {
			fmt.Fprintf(w, "\n  %sRefs:%s\n", decompDim, decompReset)
			for _, ref := range r.Tier2.Refs {
				fmt.Fprintf(w, "    %s (%s)\n", ref.URI, ref.Relation)
			}
		}
	}

	// Tier 3
	if r.Tier3Raw != "" {
		color := decompTierColor(3)
		fmt.Fprintf(w, "\n%s── T3: Raw (%s bytes) ──%s\n",
			color, decompFormatBytes(len(r.Tier3Raw)), decompReset)
		fmt.Fprintf(w, "%s[stored, not displayed — use --tier 3 to view]%s\n", decompDim, decompReset)
	}

	// Metrics footer
	fmt.Fprintf(w, "\n%s── Metrics ──%s\n", decompMagenta, decompReset)
	parts := []string{
		fmt.Sprintf("Total: %s", formatLatency(r.Metrics.TotalLatencyMs)),
	}
	if r.Metrics.CompressionRatio > 0 {
		parts = append(parts, fmt.Sprintf("Compression: %.0f:1 (T0)", r.Metrics.CompressionRatio))
	}
	parts = append(parts, fmt.Sprintf("Input hash: %s", r.InputHash))
	fmt.Fprintf(w, "%s\n\n", strings.Join(parts, " | "))
}

// === Wave 3 D2: File-based Bus Event Emission ===

// decompBusEvent is the JSONL structure written to the bus event file.
type decompBusEvent struct {
	Ts      string                 `json:"ts"`
	Type    string                 `json:"type"`
	From    string                 `json:"from"`
	Payload map[string]interface{} `json:"payload"`
}

// newFileEventCallback creates a DecompEventCallback that writes JSONL events
// to .cog/.state/buses/decompose/events.jsonl under the given workspace root.
// If the bus directory cannot be created, the callback silently no-ops.
func newFileEventCallback(root string) DecompEventCallback {
	busDir := filepath.Join(root, ".cog", ".state", "buses", "decompose")
	if err := os.MkdirAll(busDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: cannot create decompose bus dir: %v\n", err)
		return func(string, map[string]interface{}) {} // no-op
	}

	eventsPath := filepath.Join(busDir, "events.jsonl")

	return func(eventType string, payload map[string]interface{}) {
		evt := decompBusEvent{
			Ts:      time.Now().UTC().Format(time.RFC3339Nano),
			Type:    eventType,
			From:    "decompose",
			Payload: payload,
		}
		line, err := json.Marshal(evt)
		if err != nil {
			return
		}
		f, err := os.OpenFile(eventsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return
		}
		_, _ = f.WriteString(string(line) + "\n")
		f.Close()
	}
}

// === Wave 2: Engine — GenerateJSON method on AgentHarness ===

// GenerateJSON sends a prompt to the model with JSON mode and returns raw content.
func (h *AgentHarness) GenerateJSON(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	req := agentChatRequest{
		Model: h.model,
		Messages: []agentChatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		Stream: false,
		Think:  false,
		Format: "json",
	}
	resp, err := h.chatCompletion(ctx, req)
	if err != nil {
		return "", err
	}
	return resp.Message.Content, nil
}

// === Wave 2: Engine — DecompositionRunner ===

// DecompositionRunner orchestrates the multi-tier decomposition pipeline.
type DecompositionRunner struct {
	harness  *AgentHarness
	callback DecompEventCallback
	tiers    []int // which tiers to run (default: [0,1,2,3])
	retried  bool  // set to true if any tier needed a JSON parse retry
}

// NewDecompositionRunner creates a runner with the given harness and optional event callback.
func NewDecompositionRunner(harness *AgentHarness, callback DecompEventCallback) *DecompositionRunner {
	return &DecompositionRunner{
		harness:  harness,
		callback: callback,
		tiers:    []int{0, 1, 2, 3},
	}
}

// emit sends a bus event if a callback is registered.
func (r *DecompositionRunner) emit(eventType string, payload map[string]interface{}) {
	if r.callback != nil {
		r.callback(eventType, payload)
	}
}

// hasTier checks whether a tier is in the requested set.
func (r *DecompositionRunner) hasTier(tier int) bool {
	for _, t := range r.tiers {
		if t == tier {
			return true
		}
	}
	return false
}

// Run executes the decomposition pipeline on the given input.
func (r *DecompositionRunner) Run(ctx context.Context, input *DecompInput) (*DecompositionResult, error) {
	totalStart := time.Now()

	// 1. Compute input hash (sha256, first 12 hex chars)
	h := sha256.Sum256([]byte(input.Text))
	inputHash := hex.EncodeToString(h[:])[:12]

	result := &DecompositionResult{
		InputHash:   inputHash,
		InputSize:   input.ByteSize,
		InputFormat: input.Format,
		SourceURI:   input.SourceURI,
		CreatedAt:   time.Now(),
	}

	// 2. Emit start event
	r.emit(DecompEventStart, map[string]interface{}{
		"input_hash": inputHash,
		"input_size": input.ByteSize,
		"tiers":      r.tiers,
	})

	userPrompt := tierUserPrompt(input.Text)

	// 3. Run each requested tier
	if r.hasTier(0) {
		t0, metrics, err := r.runTier0(ctx, userPrompt)
		if err != nil {
			r.emit(DecompEventError, map[string]interface{}{"tier": 0, "error": err.Error()})
			return nil, fmt.Errorf("tier 0: %w", err)
		}
		result.Tier0 = t0
		result.Metrics.Tier0Tokens = metrics.tokens
		result.Metrics.Tier0LatencyMs = metrics.latencyMs
	}

	if r.hasTier(1) {
		t1, metrics, err := r.runTier1(ctx, userPrompt)
		if err != nil {
			r.emit(DecompEventError, map[string]interface{}{"tier": 1, "error": err.Error()})
			return nil, fmt.Errorf("tier 1: %w", err)
		}
		result.Tier1 = t1
		result.Metrics.Tier1Tokens = metrics.tokens
		result.Metrics.Tier1LatencyMs = metrics.latencyMs
	}

	if r.hasTier(2) {
		t2, metrics, err := r.runTier2(ctx, userPrompt)
		if err != nil {
			r.emit(DecompEventError, map[string]interface{}{"tier": 2, "error": err.Error()})
			return nil, fmt.Errorf("tier 2: %w", err)
		}
		result.Tier2 = t2
		result.Metrics.Tier2Tokens = metrics.tokens
		result.Metrics.Tier2LatencyMs = metrics.latencyMs
	}

	// 4. Tier 3 = raw passthrough
	if r.hasTier(3) {
		result.Tier3Raw = input.Text
	}

	// 5. Compute compression ratio (input chars / tier0 chars)
	if result.Tier0 != nil && len(result.Tier0.Summary) > 0 {
		result.Metrics.CompressionRatio = float64(len(input.Text)) / float64(len(result.Tier0.Summary))
	}

	// 6. Total latency
	result.Metrics.TotalLatencyMs = time.Since(totalStart).Milliseconds()

	// 7. Emit complete
	// Build complete event payload with full metrics
	completePayload := map[string]interface{}{
		"input_hash":        inputHash,
		"input_size":        input.ByteSize,
		"total_latency":     result.Metrics.TotalLatencyMs,
		"tier0_tokens":      result.Metrics.Tier0Tokens,
		"tier0_latency_ms":  result.Metrics.Tier0LatencyMs,
		"tier1_tokens":      result.Metrics.Tier1Tokens,
		"tier1_latency_ms":  result.Metrics.Tier1LatencyMs,
		"tier2_tokens":      result.Metrics.Tier2Tokens,
		"tier2_latency_ms":  result.Metrics.Tier2LatencyMs,
		"compression_ratio": result.Metrics.CompressionRatio,
		"schema_conformant": !r.retried,
	}
	r.emit(DecompEventComplete, completePayload)

	return result, nil
}

// tierMetrics captures per-tier timing and token estimation.
type tierMetrics struct {
	tokens    int
	latencyMs int64
}

// runTier0 generates a one-sentence summary.
func (r *DecompositionRunner) runTier0(ctx context.Context, userPrompt string) (*Tier0Result, tierMetrics, error) {
	r.emit(DecompEventTierStart, map[string]interface{}{"tier": 0})
	start := time.Now()

	raw, err := r.harness.GenerateJSON(ctx, tier0SystemPrompt(), userPrompt)
	if err != nil {
		return nil, tierMetrics{}, err
	}

	var t0 Tier0Result
	if err := json.Unmarshal([]byte(raw), &t0); err != nil {
		// Retry once with error-correction prompt
		r.retried = true
		correctionPrompt := tier0SystemPrompt() + "\n\nYour previous response was invalid JSON: " + err.Error() + "\nPlease respond with valid JSON only."
		raw, err = r.harness.GenerateJSON(ctx, correctionPrompt, userPrompt)
		if err != nil {
			return nil, tierMetrics{}, fmt.Errorf("retry failed: %w", err)
		}
		if err := json.Unmarshal([]byte(raw), &t0); err != nil {
			return nil, tierMetrics{}, fmt.Errorf("parse after retry: %w", err)
		}
	}

	m := tierMetrics{
		tokens:    len(raw) / 4,
		latencyMs: time.Since(start).Milliseconds(),
	}
	r.emit(DecompEventTierComplete, map[string]interface{}{"tier": 0, "latency_ms": m.latencyMs})
	return &t0, m, nil
}

// runTier1 generates a paragraph summary with key terms.
func (r *DecompositionRunner) runTier1(ctx context.Context, userPrompt string) (*Tier1Result, tierMetrics, error) {
	r.emit(DecompEventTierStart, map[string]interface{}{"tier": 1})
	start := time.Now()

	raw, err := r.harness.GenerateJSON(ctx, tier1SystemPrompt(), userPrompt)
	if err != nil {
		return nil, tierMetrics{}, err
	}

	var t1 Tier1Result
	if err := json.Unmarshal([]byte(raw), &t1); err != nil {
		r.retried = true
		correctionPrompt := tier1SystemPrompt() + "\n\nYour previous response was invalid JSON: " + err.Error() + "\nPlease respond with valid JSON only."
		raw, err = r.harness.GenerateJSON(ctx, correctionPrompt, userPrompt)
		if err != nil {
			return nil, tierMetrics{}, fmt.Errorf("retry failed: %w", err)
		}
		if err := json.Unmarshal([]byte(raw), &t1); err != nil {
			return nil, tierMetrics{}, fmt.Errorf("parse after retry: %w", err)
		}
	}

	m := tierMetrics{
		tokens:    len(raw) / 4,
		latencyMs: time.Since(start).Milliseconds(),
	}
	r.emit(DecompEventTierComplete, map[string]interface{}{"tier": 1, "latency_ms": m.latencyMs})
	return &t1, m, nil
}

// runTier2 generates a full CogDoc structure.
func (r *DecompositionRunner) runTier2(ctx context.Context, userPrompt string) (*Tier2Result, tierMetrics, error) {
	r.emit(DecompEventTierStart, map[string]interface{}{"tier": 2})
	start := time.Now()

	raw, err := r.harness.GenerateJSON(ctx, tier2SystemPrompt(), userPrompt)
	if err != nil {
		return nil, tierMetrics{}, err
	}

	var t2 Tier2Result
	if err := json.Unmarshal([]byte(raw), &t2); err != nil {
		r.retried = true
		correctionPrompt := tier2SystemPrompt() + "\n\nYour previous response was invalid JSON: " + err.Error() + "\nPlease respond with valid JSON only."
		raw, err = r.harness.GenerateJSON(ctx, correctionPrompt, userPrompt)
		if err != nil {
			return nil, tierMetrics{}, fmt.Errorf("retry failed: %w", err)
		}
		if err := json.Unmarshal([]byte(raw), &t2); err != nil {
			return nil, tierMetrics{}, fmt.Errorf("parse after retry: %w", err)
		}
	}

	m := tierMetrics{
		tokens:    len(raw) / 4,
		latencyMs: time.Since(start).Milliseconds(),
	}
	r.emit(DecompEventTierComplete, map[string]interface{}{"tier": 2, "latency_ms": m.latencyMs})
	return &t2, m, nil
}
