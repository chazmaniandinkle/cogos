package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// helper: create a pipeline, start it, register a text channel, bind to session.
func setupPipeline(t *testing.T) (*ModalityPipeline, string) {
	t.Helper()
	tmpDir := t.TempDir()
	sessionID := "pipe-test-001"
	p := NewModalityPipeline(&PipelineConfig{
		WorkspaceRoot: tmpDir,
		SessionID:     sessionID,
	})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	desc := &ChannelDescriptor{
		ID:        "test-channel",
		Transport: "test",
		Input:     []ModalityType{ModalityText},
		Output:    []ModalityType{ModalityText},
	}
	if err := p.Channels.Register(desc); err != nil {
		t.Fatalf("Register channel: %v", err)
	}
	if err := p.Channels.BindToSession("test-channel", sessionID); err != nil {
		t.Fatalf("BindToSession: %v", err)
	}
	return p, sessionID
}

func TestPipeline_TextIngress(t *testing.T) {
	p, _ := setupPipeline(t)
	defer p.Stop(context.Background())

	event, err := p.HandleIngress("test-channel", ModalityText, []byte("Hello from the test"))
	if err != nil {
		t.Fatalf("HandleIngress: %v", err)
	}
	if event == nil {
		t.Fatal("HandleIngress returned nil event")
	}
	if event.Content != "Hello from the test" {
		t.Errorf("Content = %q, want %q", event.Content, "Hello from the test")
	}
	if event.Modality != ModalityText {
		t.Errorf("Modality = %s, want text", event.Modality)
	}
	if event.Channel != "test-channel" {
		t.Errorf("Channel = %q, want %q", event.Channel, "test-channel")
	}
}

func TestPipeline_TextEgress(t *testing.T) {
	p, sessionID := setupPipeline(t)
	defer p.Stop(context.Background())

	intent := CognitiveIntent{
		Modality: ModalityText,
		Content:  "Response from agent",
	}
	outputs, err := p.HandleEgress(sessionID, &intent)
	if err != nil {
		t.Fatalf("HandleEgress: %v", err)
	}
	out, ok := outputs["test-channel"]
	if !ok {
		t.Fatal("outputs missing test-channel entry")
	}
	if string(out.Data) != "Response from agent" {
		t.Errorf("Data = %q, want %q", out.Data, "Response from agent")
	}
	if out.MimeType != "text/plain" {
		t.Errorf("MimeType = %q, want %q", out.MimeType, "text/plain")
	}
}

func TestPipeline_TextRoundtrip(t *testing.T) {
	p, sessionID := setupPipeline(t)
	defer p.Stop(context.Background())

	// Ingress
	event, err := p.HandleIngress("test-channel", ModalityText, []byte("roundtrip payload"))
	if err != nil {
		t.Fatalf("HandleIngress: %v", err)
	}
	if event == nil {
		t.Fatal("HandleIngress returned nil event")
	}
	if event.Content != "roundtrip payload" {
		t.Errorf("ingress Content = %q, want %q", event.Content, "roundtrip payload")
	}

	// Egress using content from ingress
	intent := CognitiveIntent{
		Modality: ModalityText,
		Content:  event.Content,
	}
	outputs, err := p.HandleEgress(sessionID, &intent)
	if err != nil {
		t.Fatalf("HandleEgress: %v", err)
	}
	out, ok := outputs["test-channel"]
	if !ok {
		t.Fatal("outputs missing test-channel entry")
	}
	if string(out.Data) != "roundtrip payload" {
		t.Errorf("roundtrip Data = %q, want %q", out.Data, "roundtrip payload")
	}
}

func TestPipeline_LedgerRecording(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "ledger-test-001"
	p := NewModalityPipeline(&PipelineConfig{
		WorkspaceRoot: tmpDir,
		SessionID:     sessionID,
	})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop(context.Background())

	desc := &ChannelDescriptor{
		ID:        "test-channel",
		Transport: "test",
		Input:     []ModalityType{ModalityText},
		Output:    []ModalityType{ModalityText},
	}
	if err := p.Channels.Register(desc); err != nil {
		t.Fatalf("Register channel: %v", err)
	}
	if err := p.Channels.BindToSession("test-channel", sessionID); err != nil {
		t.Fatalf("BindToSession: %v", err)
	}

	// Ingress
	if _, err := p.HandleIngress("test-channel", ModalityText, []byte("input text")); err != nil {
		t.Fatalf("HandleIngress: %v", err)
	}

	// Egress
	intent := CognitiveIntent{Modality: ModalityText, Content: "output text"}
	if _, err := p.HandleEgress(sessionID, &intent); err != nil {
		t.Fatalf("HandleEgress: %v", err)
	}

	// Read ledger file
	eventsFile := filepath.Join(tmpDir, ".cog", "ledger", sessionID, "events.jsonl")
	f, err := os.Open(eventsFile)
	if err != nil {
		t.Fatalf("Open ledger: %v", err)
	}
	defer f.Close()

	var envs []EventEnvelope
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var env EventEnvelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("unmarshal event: %v", err)
		}
		envs = append(envs, env)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner: %v", err)
	}

	if len(envs) < 2 {
		t.Fatalf("expected at least 2 events, got %d", len(envs))
	}

	// Verify event types
	if envs[0].HashedPayload.Type != EventModalityInput {
		t.Errorf("event[0] type = %q, want %q", envs[0].HashedPayload.Type, EventModalityInput)
	}
	if envs[1].HashedPayload.Type != EventModalityOutput {
		t.Errorf("event[1] type = %q, want %q", envs[1].HashedPayload.Type, EventModalityOutput)
	}

	// Verify hash chain
	if envs[0].HashedPayload.PriorHash != "" {
		t.Errorf("event[0] prior_hash should be empty, got %q", envs[0].HashedPayload.PriorHash)
	}
	for i := 1; i < len(envs); i++ {
		if envs[i].HashedPayload.PriorHash != envs[i-1].Metadata.Hash {
			t.Errorf("event[%d] broken chain: prior=%q, want=%q",
				i, envs[i].HashedPayload.PriorHash, envs[i-1].Metadata.Hash)
		}
	}

	// Cross-check with VerifyLedger
	if err := VerifyLedger(tmpDir, sessionID); err != nil {
		t.Errorf("VerifyLedger: %v", err)
	}
}

func TestPipeline_SalienceScoring(t *testing.T) {
	p, _ := setupPipeline(t)
	defer p.Stop(context.Background())

	// First ingress
	if _, err := p.HandleIngress("test-channel", ModalityText, []byte("first")); err != nil {
		t.Fatalf("HandleIngress 1: %v", err)
	}
	snap1 := p.Salience.Snapshot()
	if len(snap1) == 0 {
		t.Fatal("salience snapshot empty after first ingress")
	}
	// Find the score for input events on test-channel
	key := "modality:" + EventModalityInput + ":test-channel"
	score1 := snap1[key]
	if score1 <= 0 {
		t.Errorf("score after first ingress = %f, want > 0", score1)
	}

	// Second ingress should stack
	if _, err := p.HandleIngress("test-channel", ModalityText, []byte("second")); err != nil {
		t.Fatalf("HandleIngress 2: %v", err)
	}
	score2 := p.Salience.Snapshot()[key]
	if score2 <= score1 {
		t.Errorf("score after second ingress = %f, want > %f", score2, score1)
	}
}

func TestPipeline_HUD(t *testing.T) {
	tmpDir := t.TempDir()
	p := NewModalityPipeline(&PipelineConfig{
		WorkspaceRoot: tmpDir,
		SessionID:     "hud-test-001",
	})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop(context.Background())

	// HUD format
	md := p.HUD.Format()
	if md == "" {
		t.Fatal("HUD.Format() returned empty string")
	}
	if !strings.Contains(md, "text") {
		t.Errorf("HUD output should contain 'text', got:\n%s", md)
	}
	if !strings.Contains(md, "Modality Bus") {
		t.Errorf("HUD output should contain markdown header, got:\n%s", md)
	}

	// Pipeline status
	status := p.Status()
	if _, ok := status["bus"]; !ok {
		t.Error("Status() missing 'bus' key")
	}
	if _, ok := status["channels"]; !ok {
		t.Error("Status() missing 'channels' key")
	}

	// Verify bus sub-map has modules
	busMap, ok := status["bus"].(map[string]any)
	if !ok {
		t.Fatal("bus status is not map[string]any")
	}
	if _, ok := busMap["modules"]; !ok {
		t.Error("bus status missing 'modules' key")
	}
}

func TestPipeline_MultiChannel(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "multi-ch-001"
	p := NewModalityPipeline(&PipelineConfig{
		WorkspaceRoot: tmpDir,
		SessionID:     sessionID,
	})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop(context.Background())

	// Register two channels both supporting text
	for _, id := range []string{"ch-alpha", "ch-beta"} {
		desc := &ChannelDescriptor{
			ID:        id,
			Transport: "test",
			Input:     []ModalityType{ModalityText},
			Output:    []ModalityType{ModalityText},
		}
		if err := p.Channels.Register(desc); err != nil {
			t.Fatalf("Register %s: %v", id, err)
		}
		if err := p.Channels.BindToSession(id, sessionID); err != nil {
			t.Fatalf("Bind %s: %v", id, err)
		}
	}

	intent := CognitiveIntent{Modality: ModalityText, Content: "broadcast message"}
	outputs, err := p.HandleEgress(sessionID, &intent)
	if err != nil {
		t.Fatalf("HandleEgress: %v", err)
	}

	for _, id := range []string{"ch-alpha", "ch-beta"} {
		out, ok := outputs[id]
		if !ok {
			t.Errorf("outputs missing %q", id)
			continue
		}
		if string(out.Data) != "broadcast message" {
			t.Errorf("%s data = %q, want %q", id, out.Data, "broadcast message")
		}
	}
}
