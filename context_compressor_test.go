package main

import (
	"strings"
	"testing"
	"time"
)

// makeMessages creates n ThreadMessages alternating user/assistant.
func makeMessages(n int) []ThreadMessage {
	msgs := make([]ThreadMessage, n)
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs[i] = ThreadMessage{
			Role:    role,
			Content: strings.Repeat("word ", 20), // ~100 chars each
			Tokens:  25,
		}
	}
	return msgs
}

func TestCompressorResumeShortThread(t *testing.T) {
	cc := NewContextCompressor("/tmp/test")
	view := &ThreadView{
		Messages:     makeMessages(10),
		SystemPrompt: "You are helpful.",
		LastUserMsg:  "What is 2+2?",
	}
	session := &SessionState{
		ClaudeSessionID: "claude-abc-123",
	}

	win, err := cc.Compress(view, session, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if win.Strategy != "resume" {
		t.Errorf("expected strategy 'resume', got %q", win.Strategy)
	}
	if win.Prompt != "What is 2+2?" {
		t.Errorf("expected prompt to be just last message, got %q", win.Prompt)
	}
	if win.ClaudeSession != "claude-abc-123" {
		t.Errorf("expected ClaudeSession 'claude-abc-123', got %q", win.ClaudeSession)
	}
	if win.SystemPrompt != "You are helpful." {
		t.Errorf("expected system prompt preserved, got %q", win.SystemPrompt)
	}
}

func TestCompressorResumeLongThread(t *testing.T) {
	cc := NewContextCompressor("/tmp/test")
	msgs := makeMessages(25)
	// Give the last few messages distinct content for reminder detection.
	msgs[22].Content = "Tell me about quantum computing."
	msgs[23].Content = "Quantum computing uses qubits to perform parallel computations."
	msgs[24].Content = "How does entanglement work?"

	view := &ThreadView{
		Messages:     msgs,
		SystemPrompt: "You are helpful.",
		LastUserMsg:  "Explain superposition.",
	}
	session := &SessionState{
		ClaudeSessionID: "claude-long-session",
	}

	win, err := cc.Compress(view, session, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if win.Strategy != "resume" {
		t.Errorf("expected strategy 'resume', got %q", win.Strategy)
	}
	if !strings.HasPrefix(win.Prompt, "Explain superposition.") {
		t.Errorf("expected prompt to start with last message, got prefix %q", win.Prompt[:40])
	}
	if !strings.Contains(win.Prompt, "Context reminder") {
		t.Errorf("expected context reminder in prompt for long thread")
	}
	if win.ClaudeSession != "claude-long-session" {
		t.Errorf("expected ClaudeSession preserved, got %q", win.ClaudeSession)
	}
}

func TestCompressorFreshSession(t *testing.T) {
	cc := NewContextCompressor("/tmp/test")
	msgs := makeMessages(16)
	msgs[14].Content = "What is Go?"
	msgs[15].Content = "Go is a compiled programming language."

	view := &ThreadView{
		Messages:     msgs,
		SystemPrompt: "You are a Go expert.",
		LastUserMsg:  "How do goroutines work?",
	}

	// No ClaudeSessionID → fresh.
	session := &SessionState{}

	win, err := cc.Compress(view, session, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if win.Strategy != "fresh" {
		t.Errorf("expected strategy 'fresh', got %q", win.Strategy)
	}
	if win.ClaudeSession != "" {
		t.Errorf("expected empty ClaudeSession for fresh, got %q", win.ClaudeSession)
	}
	if !strings.Contains(win.Prompt, "How do goroutines work?") {
		t.Errorf("expected current message in prompt")
	}
	// Fresh should have section separators.
	if !strings.Contains(win.Prompt, "---") {
		t.Errorf("expected section separators in fresh context")
	}
}

func TestCompressorFreshWithWorkingMemory(t *testing.T) {
	cc := NewContextCompressor("/tmp/test")
	view := &ThreadView{
		Messages:     makeMessages(6),
		SystemPrompt: "System prompt.",
		LastUserMsg:  "Continue from where we left off.",
	}
	session := &SessionState{
		WorkingMemory: &WorkingMemory{
			ActiveTopics:    []string{"context engine", "TAA"},
			KeyDecisions:    []string{"use budget allocation"},
			ActiveArtifacts: []string{"context_compressor.go"},
			Summary:         "Building the context compression layer.",
			UpdatedAt:       time.Now(),
		},
	}

	win, err := cc.Compress(view, session, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if win.Strategy != "fresh" {
		t.Errorf("expected strategy 'fresh', got %q", win.Strategy)
	}
	if !strings.Contains(win.Prompt, "## Working Memory") {
		t.Errorf("expected Working Memory block in fresh prompt")
	}
	if !strings.Contains(win.Prompt, "context engine") {
		t.Errorf("expected topic 'context engine' in working memory block")
	}
	if !strings.Contains(win.Prompt, "use budget allocation") {
		t.Errorf("expected decision in working memory block")
	}
}

func TestCompressorBudgetEnforcement(t *testing.T) {
	cc := NewContextCompressor("/tmp/test")

	// Create messages with very long content.
	msgs := make([]ThreadMessage, 30)
	for i := 0; i < 30; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs[i] = ThreadMessage{
			Role:    role,
			Content: strings.Repeat("This is a very long message with lots of content. ", 100),
			Tokens:  1250,
		}
	}

	view := &ThreadView{
		Messages:     msgs,
		SystemPrompt: "System.",
		LastUserMsg:  "Final question.",
	}
	session := &SessionState{} // fresh

	// Use a small profile budget.
	profile := &TAAProfile{
		Tiers: TAAProfileTiers{
			TotalTokens: 5000,
		},
	}

	win, err := cc.Compress(view, session, profile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if win.Strategy != "fresh" {
		t.Errorf("expected strategy 'fresh', got %q", win.Strategy)
	}
	// The compressed output should be significantly smaller than the raw total.
	rawTotal := 0
	for _, m := range msgs {
		rawTotal += len(m.Content)
	}
	if len(win.Prompt) >= rawTotal {
		t.Errorf("expected compressed prompt to be smaller than raw total (%d >= %d)", len(win.Prompt), rawTotal)
	}
}

func TestCompressorEmptyThread(t *testing.T) {
	cc := NewContextCompressor("/tmp/test")
	view := &ThreadView{
		Messages:     []ThreadMessage{},
		SystemPrompt: "System.",
		LastUserMsg:  "",
	}
	session := &SessionState{}

	win, err := cc.Compress(view, session, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if win.Strategy != "fresh" {
		t.Errorf("expected strategy 'fresh', got %q", win.Strategy)
	}
	if win.TotalTokens < 0 {
		t.Errorf("expected non-negative token count")
	}
}

func TestCompressorNilSession(t *testing.T) {
	cc := NewContextCompressor("/tmp/test")
	view := &ThreadView{
		Messages:     makeMessages(5),
		SystemPrompt: "System.",
		LastUserMsg:  "Hello.",
	}

	win, err := cc.Compress(view, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Nil session means no ClaudeSessionID → fresh.
	if win.Strategy != "fresh" {
		t.Errorf("expected strategy 'fresh', got %q", win.Strategy)
	}
	if win.ClaudeSession != "" {
		t.Errorf("expected empty ClaudeSession, got %q", win.ClaudeSession)
	}
}

func TestCompressorNilProfile(t *testing.T) {
	cc := NewContextCompressor("/tmp/test")
	view := &ThreadView{
		Messages:     makeMessages(10),
		SystemPrompt: "System.",
		LastUserMsg:  "Hello.",
	}
	session := &SessionState{
		ClaudeSessionID: "session-123",
	}

	win, err := cc.Compress(view, session, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// With session and <20 messages → resume short.
	if win.Strategy != "resume" {
		t.Errorf("expected strategy 'resume', got %q", win.Strategy)
	}
	// Should not error — default budget is used internally.
}

func TestCompressorNilView(t *testing.T) {
	cc := NewContextCompressor("/tmp/test")
	_, err := cc.Compress(nil, nil, nil)
	if err == nil {
		t.Fatal("expected error for nil view")
	}
}

func TestBudgetFromProfile(t *testing.T) {
	// Nil profile → default.
	b := budgetFromProfile(nil)
	if b.Total != 15000 {
		t.Errorf("expected default total 15000, got %d", b.Total)
	}

	// Profile with total tokens.
	profile := &TAAProfile{
		Tiers: TAAProfileTiers{TotalTokens: 10000},
	}
	b = budgetFromProfile(profile)
	if b.Total != 10000 {
		t.Errorf("expected total 10000, got %d", b.Total)
	}
	if b.WorkingMemory != 1300 {
		t.Errorf("expected WorkingMemory 1300, got %d", b.WorkingMemory)
	}
	if b.CompressedHistory != 2000 {
		t.Errorf("expected CompressedHistory 2000, got %d", b.CompressedHistory)
	}
	if b.RecentContext != 5400 {
		t.Errorf("expected RecentContext 5400, got %d", b.RecentContext)
	}
	if b.CurrentMessage != 1300 {
		t.Errorf("expected CurrentMessage 1300, got %d", b.CurrentMessage)
	}

	// Profile with zero total → fallback to 15000.
	profile2 := &TAAProfile{
		Tiers: TAAProfileTiers{TotalTokens: 0},
	}
	b = budgetFromProfile(profile2)
	if b.Total != 15000 {
		t.Errorf("expected fallback total 15000, got %d", b.Total)
	}
}

func TestEstimateTokens(t *testing.T) {
	if estimateTokens("") != 0 {
		t.Error("empty string should be 0 tokens")
	}
	// 100 chars → 25 tokens.
	s := strings.Repeat("a", 100)
	if estimateTokens(s) != 25 {
		t.Errorf("expected 25 tokens, got %d", estimateTokens(s))
	}
}

func TestFirstSentence(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Hello world.", "Hello world."},
		{"First sentence. Second sentence.", "First sentence."},
		{"No period here", "No period here"},
		{"", ""},
		{"Line one\nLine two", "Line one"},
		{"Question? Yes.", "Question?"},
	}

	for _, tt := range tests {
		got := firstSentence(tt.input)
		if got != tt.expected {
			t.Errorf("firstSentence(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestExtractCodeBlock(t *testing.T) {
	input := "Some text\n```go\nfunc main() {}\n```\nMore text"
	got := extractCodeBlock(input)
	if !strings.Contains(got, "func main()") {
		t.Errorf("expected code block content, got %q", got)
	}

	// No code block.
	if extractCodeBlock("just plain text") != "" {
		t.Error("expected empty string for text without code block")
	}
}
