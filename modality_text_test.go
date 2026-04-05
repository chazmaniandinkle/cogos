package main

import (
	"context"
	"testing"
)

// TestTextModule_Interface verifies TextModule satisfies ModalityModule.
func TestTextModule_Interface(t *testing.T) {
	var _ ModalityModule = (*TextModule)(nil)
}

// TestTextModule_Lifecycle tests Start, Health, Stop transitions.
func TestTextModule_Lifecycle(t *testing.T) {
	m := NewTextModule()
	if m.Health() != ModuleStatusStopped {
		t.Fatalf("initial status = %s, want stopped", m.Health())
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if m.Health() != ModuleStatusHealthy {
		t.Fatalf("after start = %s, want healthy", m.Health())
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if m.Health() != ModuleStatusStopped {
		t.Fatalf("after stop = %s, want stopped", m.Health())
	}
}

// TestTextModule_Decode tests raw bytes to CognitiveEvent.
func TestTextModule_Decode(t *testing.T) {
	m := NewTextModule()
	ev, err := m.Decoder().Decode([]byte("hello world"), ModalityText, "stdin")
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if ev.Modality != ModalityText {
		t.Errorf("modality = %s, want text", ev.Modality)
	}
	if ev.Content != "hello world" {
		t.Errorf("content = %q, want %q", ev.Content, "hello world")
	}
	if ev.Channel != "stdin" {
		t.Errorf("channel = %q, want %q", ev.Channel, "stdin")
	}
	if ev.Confidence != 1.0 {
		t.Errorf("confidence = %f, want 1.0", ev.Confidence)
	}
}

// TestTextModule_Encode tests CognitiveIntent to EncodedOutput.
func TestTextModule_Encode(t *testing.T) {
	m := NewTextModule()
	intent := &CognitiveIntent{
		Modality: ModalityText,
		Channel:  "stdout",
		Content:  "goodbye world",
	}
	out, err := m.Encoder().Encode(intent)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if out.Modality != ModalityText {
		t.Errorf("modality = %s, want text", out.Modality)
	}
	if string(out.Data) != "goodbye world" {
		t.Errorf("data = %q, want %q", out.Data, "goodbye world")
	}
	if out.MimeType != "text/plain" {
		t.Errorf("mime = %q, want text/plain", out.MimeType)
	}
}

// TestTextModule_NilGate verifies Gate returns nil.
func TestTextModule_NilGate(t *testing.T) {
	m := NewTextModule()
	if m.Gate() != nil {
		t.Fatal("Gate() should return nil for text module")
	}
}

// TestTextModule_WithBus tests registration, Perceive, and Act on the bus.
func TestTextModule_WithBus(t *testing.T) {
	bus := NewModalityBus()
	m := NewTextModule()
	if err := bus.Register(m); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Perceive
	ev, err := bus.Perceive([]byte("bus test"), ModalityText, "ch1")
	if err != nil {
		t.Fatalf("Perceive: %v", err)
	}
	if ev.Content != "bus test" {
		t.Errorf("perceive content = %q, want %q", ev.Content, "bus test")
	}

	// Act
	intent := &CognitiveIntent{Modality: ModalityText, Channel: "ch1", Content: "reply"}
	out, err := bus.Act(intent)
	if err != nil {
		t.Fatalf("Act: %v", err)
	}
	if string(out.Data) != "reply" {
		t.Errorf("act data = %q, want %q", out.Data, "reply")
	}
}
