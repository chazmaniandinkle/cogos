package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModalityLedger_AllEventTypes(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "modality-test-001"
	ml := NewModalityLedger(tmpDir, sessionID)

	// Emit all 6 event types
	for _, tc := range []struct {
		name string
		fn   func() error
	}{
		{"Input", func() error {
			return ml.EmitInput(&ModalityInputData{
				Modality: "voice", Channel: "mic-0", Transcript: "hello world",
				GateConfidence: 0.95, LatencyMs: 120,
			})
		}},
		{"Output", func() error {
			return ml.EmitOutput(&ModalityOutputData{
				Modality: "voice", Channel: "speaker-0", Text: "hi there",
				Engine: "kokoro", Voice: "bm_lewis", DurationSec: 1.2,
			})
		}},
		{"Transform", func() error {
			return ml.EmitTransform(&ModalityTransformData{
				FromModality: "voice", ToModality: "text", Step: "stt",
				Engine: "whisper", LatencyMs: 80, InputBytes: 16000, OutputChars: 11,
			})
		}},
		{"Gate", func() error {
			return ml.EmitGate(&ModalityGateData{
				Modality: "voice", Channel: "mic-0", Decision: "accept",
				Confidence: 0.92, SpeechRatio: 0.75,
			})
		}},
		{"StateChange", func() error {
			return ml.EmitStateChange(&ModalityStateChangeData{
				Modality: "voice", Module: "vad", FromState: "idle", ToState: "listening", PID: 12345,
			})
		}},
		{"Error", func() error {
			return ml.EmitError(&ModalityErrorData{
				Modality: "voice", Module: "tts", Error: "model not loaded",
				ErrorType: "init", Recoverable: true,
			})
		}},
	} {
		if err := tc.fn(); err != nil {
			t.Fatalf("Emit%s: %v", tc.name, err)
		}
	}

	// Read back all events
	eventsFile := filepath.Join(tmpDir, ".cog", "ledger", sessionID, "events.jsonl")
	data, err := os.ReadFile(eventsFile)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := splitNonEmpty(string(data))
	if len(lines) != 6 {
		t.Fatalf("expected 6 events, got %d", len(lines))
	}

	var events []EventEnvelope
	for i, line := range lines {
		var env EventEnvelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("event %d unmarshal: %v", i, err)
		}
		events = append(events, env)
	}

	// Verify types, source, sequence
	wantTypes := []string{
		EventModalityInput, EventModalityOutput, EventModalityTransform,
		EventModalityGate, EventModalityStateChange, EventModalityError,
	}
	for i, env := range events {
		if env.HashedPayload.Type != wantTypes[i] {
			t.Errorf("event %d: type = %q, want %q", i, env.HashedPayload.Type, wantTypes[i])
		}
		if env.Metadata.Source != "modality-bus" {
			t.Errorf("event %d: source = %q, want %q", i, env.Metadata.Source, "modality-bus")
		}
		if env.Metadata.Seq != int64(i+1) {
			t.Errorf("event %d: seq = %d, want %d", i, env.Metadata.Seq, i+1)
		}
	}

	// Verify hash chain
	if events[0].HashedPayload.PriorHash != "" {
		t.Errorf("event 0: prior_hash should be empty, got %q", events[0].HashedPayload.PriorHash)
	}
	for i := 1; i < len(events); i++ {
		if events[i].HashedPayload.PriorHash != events[i-1].Metadata.Hash {
			t.Errorf("event %d: broken chain: prior=%q want=%q", i,
				events[i].HashedPayload.PriorHash, events[i-1].Metadata.Hash)
		}
	}

	// Cross-check with VerifyLedger
	if err := VerifyLedger(tmpDir, sessionID); err != nil {
		t.Errorf("VerifyLedger: %v", err)
	}
}

func TestModalityLedger_DataRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	ml := NewModalityLedger(tmpDir, "rt-001")

	if err := ml.EmitInput(&ModalityInputData{
		Modality: "voice", Channel: "mic-0", Transcript: "round trip",
		GateConfidence: 0.88, LatencyMs: 45, Engine: "whisper",
	}); err != nil {
		t.Fatalf("EmitInput: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, ".cog", "ledger", "rt-001", "events.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var env EventEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	d := env.HashedPayload.Data
	checks := map[string]any{
		"modality": "voice", "channel": "mic-0", "transcript": "round trip",
		"engine": "whisper", "latency_ms": float64(45),
	}
	for k, want := range checks {
		if d[k] != want {
			t.Errorf("%s = %v, want %v", k, d[k], want)
		}
	}
}

func TestModalityLedger_ValidationErrors(t *testing.T) {
	tmpDir := t.TempDir()
	ml := NewModalityLedger(tmpDir, "val-001")

	errs := []struct {
		name string
		fn   func() error
	}{
		{"Input/empty modality", func() error {
			return ml.EmitInput(&ModalityInputData{Channel: "mic-0", Transcript: "hello"})
		}},
		{"Output/empty text", func() error {
			return ml.EmitOutput(&ModalityOutputData{Modality: "voice", Channel: "spk-0"})
		}},
		{"Transform/empty step", func() error {
			return ml.EmitTransform(&ModalityTransformData{FromModality: "voice", ToModality: "text"})
		}},
		{"Gate/empty decision", func() error {
			return ml.EmitGate(&ModalityGateData{Modality: "voice", Channel: "mic-0"})
		}},
		{"StateChange/empty to_state", func() error {
			return ml.EmitStateChange(&ModalityStateChangeData{Modality: "voice", Module: "vad", FromState: "idle"})
		}},
		{"Error/empty error", func() error {
			return ml.EmitError(&ModalityErrorData{Modality: "voice", Module: "tts"})
		}},
	}
	for _, tc := range errs {
		if err := tc.fn(); err == nil {
			t.Errorf("%s: expected validation error, got nil", tc.name)
		}
	}
	// No events should have been written
	eventsFile := filepath.Join(tmpDir, ".cog", "ledger", "val-001", "events.jsonl")
	if _, err := os.Stat(eventsFile); err == nil {
		t.Error("ledger file should not exist after only validation failures")
	}
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}
