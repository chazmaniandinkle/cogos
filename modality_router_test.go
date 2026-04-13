package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestRouter(t *testing.T) (*EventRouter, string) {
	t.Helper()
	tmpDir := t.TempDir()
	sessionID := "router-test-001"
	bus := NewModalityBus()
	ledger := NewModalityLedger(tmpDir, sessionID)
	router := NewEventRouter(bus, ledger, sessionID)
	return router, tmpDir
}

func readLedgerEvents(t *testing.T, tmpDir, sessionID string) []EventEnvelope {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(tmpDir, ".cog", "ledger", sessionID, "events.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var envs []EventEnvelope
	for _, line := range splitNonEmpty(string(data)) {
		var env EventEnvelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		envs = append(envs, env)
	}
	return envs
}

func TestEventRouter_RoutePerceive(t *testing.T) {
	router, tmpDir := newTestRouter(t)
	event := &CognitiveEvent{
		Modality: ModalityVoice, Channel: "mic-0",
		Content: "hello world", Confidence: 0.95, Timestamp: time.Now(),
	}
	if err := router.RoutePerceive(event, nil); err != nil {
		t.Fatalf("RoutePerceive: %v", err)
	}
	envs := readLedgerEvents(t, tmpDir, "router-test-001")
	if len(envs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(envs))
	}
	if envs[0].HashedPayload.Type != EventModalityInput {
		t.Errorf("type = %q, want %q", envs[0].HashedPayload.Type, EventModalityInput)
	}
	if envs[0].HashedPayload.Data["transcript"] != "hello world" {
		t.Errorf("transcript = %v, want %q", envs[0].HashedPayload.Data["transcript"], "hello world")
	}
}

func TestEventRouter_RouteAct(t *testing.T) {
	router, tmpDir := newTestRouter(t)
	intent := &CognitiveIntent{
		Modality: ModalityVoice, Channel: "speaker-0",
		Content: "hi there", Params: map[string]any{"engine": "kokoro", "voice": "bm_lewis"},
	}
	output := &EncodedOutput{Modality: ModalityVoice, Data: []byte("fake-audio"), MimeType: "audio/wav"}
	if err := router.RouteAct(intent, output); err != nil {
		t.Fatalf("RouteAct: %v", err)
	}
	envs := readLedgerEvents(t, tmpDir, "router-test-001")
	if len(envs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(envs))
	}
	if envs[0].HashedPayload.Type != EventModalityOutput {
		t.Errorf("type = %q, want %q", envs[0].HashedPayload.Type, EventModalityOutput)
	}
	if envs[0].HashedPayload.Data["text"] != "hi there" {
		t.Errorf("text = %v, want %q", envs[0].HashedPayload.Data["text"], "hi there")
	}
}

func TestEventRouter_Listener(t *testing.T) {
	router, _ := newTestRouter(t)

	var mu sync.Mutex
	var captured []string
	router.AddListener(func(eventType string, data map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, eventType)
	})

	if err := router.RouteStateChange("voice", "vad", "idle", "listening", 1234); err != nil {
		t.Fatalf("RouteStateChange: %v", err)
	}
	if err := router.RouteError("voice", "tts", "model not loaded", "init", true); err != nil {
		t.Fatalf("RouteError: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 2 {
		t.Fatalf("expected 2 listener calls, got %d", len(captured))
	}
	if captured[0] != EventModalityStateChange {
		t.Errorf("event[0] = %q, want %q", captured[0], EventModalityStateChange)
	}
	if captured[1] != EventModalityError {
		t.Errorf("event[1] = %q, want %q", captured[1], EventModalityError)
	}
}

func TestEventRouter_GateEvent(t *testing.T) {
	router, tmpDir := newTestRouter(t)
	event := &CognitiveEvent{
		Modality: ModalityVoice, Channel: "mic-0",
		Content: "gated input", Confidence: 0.90, Timestamp: time.Now(),
	}
	gate := &GateResult{Allowed: true, Confidence: 0.92, Reason: "speech detected"}
	if err := router.RoutePerceive(event, gate); err != nil {
		t.Fatalf("RoutePerceive with gate: %v", err)
	}
	envs := readLedgerEvents(t, tmpDir, "router-test-001")
	if len(envs) != 2 {
		t.Fatalf("expected 2 events (gate + input), got %d", len(envs))
	}
	if envs[0].HashedPayload.Type != EventModalityGate {
		t.Errorf("event[0] type = %q, want %q", envs[0].HashedPayload.Type, EventModalityGate)
	}
	if envs[0].HashedPayload.Data["decision"] != "accept" {
		t.Errorf("gate decision = %v, want %q", envs[0].HashedPayload.Data["decision"], "accept")
	}
	if envs[1].HashedPayload.Type != EventModalityInput {
		t.Errorf("event[1] type = %q, want %q", envs[1].HashedPayload.Type, EventModalityInput)
	}
	if envs[1].HashedPayload.Data["transcript"] != "gated input" {
		t.Errorf("transcript = %v, want %q", envs[1].HashedPayload.Data["transcript"], "gated input")
	}
}
