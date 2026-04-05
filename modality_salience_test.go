package main

import (
	"math"
	"testing"
	"time"
)

func TestModalitySalience_OnEvent(t *testing.T) {
	ms := NewModalitySalience()
	ms.OnEvent(EventModalityInput, map[string]any{"channel": "discord-voice"})

	key := "modality:modality.input:discord-voice"
	score := ms.Score(key)
	if score != 1.0 {
		t.Fatalf("expected score 1.0 after input event, got %f", score)
	}

	// Second event on same key should stack.
	ms.OnEvent(EventModalityInput, map[string]any{"channel": "discord-voice"})
	if s := ms.Score(key); s != 2.0 {
		t.Fatalf("expected score 2.0 after two input events, got %f", s)
	}
}

func TestModalitySalience_Decay(t *testing.T) {
	ms := NewModalitySalience()
	ms.OnEvent(EventModalityInput, map[string]any{"channel": "mic"})

	key := "modality:modality.input:mic"
	before := ms.Score(key)
	if before != 1.0 {
		t.Fatalf("expected 1.0 before decay, got %f", before)
	}

	// Decay by exactly one half-life: score should halve.
	halfLife := 5 * time.Minute
	future := time.Now().Add(halfLife)
	ms.Decay(future, halfLife)

	after := ms.Score(key)
	expected := 0.5
	if math.Abs(after-expected) > 0.01 {
		t.Fatalf("expected ~%.2f after one half-life, got %f", expected, after)
	}

	// Decay far into the future: entry should be pruned.
	farFuture := time.Now().Add(100 * halfLife)
	ms.Decay(farFuture, halfLife)
	if s := ms.Score(key); s != 0 {
		t.Fatalf("expected 0 after extreme decay (pruned), got %f", s)
	}
}

func TestModalitySalience_TopN(t *testing.T) {
	ms := NewModalitySalience()

	// Fire events with different boost values.
	ms.OnEvent(EventModalityError, map[string]any{"channel": "a"})        // 1.5
	ms.OnEvent(EventModalityInput, map[string]any{"channel": "b"})        // 1.0
	ms.OnEvent(EventModalityOutput, map[string]any{"channel": "c"})       // 0.8
	ms.OnEvent(EventModalityStateChange, map[string]any{"channel": "d"})  // 0.5
	ms.OnEvent(EventModalityGate, map[string]any{"channel": "e"})         // 0.3

	top := ms.TopN(3)
	if len(top) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(top))
	}

	// Verify descending order.
	if top[0].Score < top[1].Score || top[1].Score < top[2].Score {
		t.Fatalf("TopN not sorted descending: %.2f, %.2f, %.2f",
			top[0].Score, top[1].Score, top[2].Score)
	}

	// Highest should be the error event (1.5).
	if top[0].Score != 1.5 {
		t.Fatalf("expected top score 1.5 (error), got %f", top[0].Score)
	}
}

func TestModalitySalience_EventListener(t *testing.T) {
	ms := NewModalitySalience()

	// Verify OnEvent matches the EventListener signature.
	var listener EventListener = ms.OnEvent

	listener(EventModalityOutput, map[string]any{
		"modality": "voice",
		"channel":  "speakers",
		"text":     "hello",
	})

	key := "modality:modality.output:speakers"
	if s := ms.Score(key); s != 0.8 {
		t.Fatalf("expected 0.8 for output event via listener, got %f", s)
	}

	snap := ms.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 entry in snapshot, got %d", len(snap))
	}
	if snap[key] != 0.8 {
		t.Fatalf("snapshot value mismatch: expected 0.8, got %f", snap[key])
	}
}
