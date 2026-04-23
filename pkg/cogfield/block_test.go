package cogfield

import (
	"encoding/json"
	"testing"
)

func TestBlockJSONRoundTrip(t *testing.T) {
	block := Block{
		V:        2,
		ID:       "block-1",
		BusID:    "bus-abc",
		Seq:      1,
		Ts:       "2026-04-14T12:00:00Z",
		From:     "agent-a",
		To:       "agent-b",
		Type:     "bus.message",
		Payload:  map[string]interface{}{"content": "hello"},
		Prev:     []string{"hash-0"},
		PrevHash: "hash-0",
		Hash:     "hash-1",
		Merkle:   "merkle-1",
		Size:     42,
	}

	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded Block
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.V != 2 {
		t.Errorf("V = %d, want 2", decoded.V)
	}
	if decoded.BusID != "bus-abc" {
		t.Errorf("BusID = %q, want %q", decoded.BusID, "bus-abc")
	}
	if decoded.Hash != "hash-1" {
		t.Errorf("Hash = %q, want %q", decoded.Hash, "hash-1")
	}
	if len(decoded.Prev) != 1 || decoded.Prev[0] != "hash-0" {
		t.Errorf("Prev = %v, want [hash-0]", decoded.Prev)
	}
}

func TestBlockOmitsEmptyFields(t *testing.T) {
	block := Block{
		Ts:      "2026-04-14T12:00:00Z",
		From:    "agent-a",
		Type:    "bus.message",
		Payload: map[string]interface{}{},
		Hash:    "hash-1",
	}

	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}

	// These fields should be omitted when empty
	for _, field := range []string{"id", "bus_id", "to", "prev", "prev_hash", "merkle", "sig"} {
		if _, exists := raw[field]; exists {
			t.Errorf("field %q should be omitted when empty", field)
		}
	}
}

// TestBlockInlinePayloadRoundTrip verifies the legacy Phase-1 envelope shape:
// an inline Payload map with no ADR-084 digest reference.
func TestBlockInlinePayloadRoundTrip(t *testing.T) {
	block := Block{
		V:     2,
		ID:    "block-inline",
		BusID: "bus-inline",
		Seq:   7,
		Ts:    "2026-04-22T10:00:00Z",
		From:  "producer",
		Type:  "bus.message",
		Payload: map[string]interface{}{
			"content": "hello from inline",
			"count":   float64(3),
		},
		Hash: "hash-inline",
		Size: 128,
	}

	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}

	// Inline form must serialize payload and omit the ADR-084 reference fields.
	if _, ok := raw["payload"]; !ok {
		t.Error("inline block should serialize payload field")
	}
	for _, field := range []string{"digest", "media_type"} {
		if _, exists := raw[field]; exists {
			t.Errorf("inline block should omit %q when empty", field)
		}
	}

	var decoded Block
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Payload["content"] != "hello from inline" {
		t.Errorf("Payload.content = %v, want %q", decoded.Payload["content"], "hello from inline")
	}
	if decoded.Digest != "" {
		t.Errorf("Digest = %q, want empty", decoded.Digest)
	}
	if decoded.MediaType != "" {
		t.Errorf("MediaType = %q, want empty", decoded.MediaType)
	}
	if decoded.Size != 128 {
		t.Errorf("Size = %d, want 128", decoded.Size)
	}
}

// TestBlockByReferencePayloadRoundTrip verifies the ADR-084 Phase-2 envelope
// shape: Digest + MediaType + Size carry a by-reference payload pointer, and
// the inline Payload field is left empty so it does not serialize.
func TestBlockByReferencePayloadRoundTrip(t *testing.T) {
	block := Block{
		V:         2,
		ID:        "block-byref",
		BusID:     "bus-byref",
		Seq:       11,
		Ts:        "2026-04-22T10:05:00Z",
		From:      "producer",
		Type:      "trace.assessment",
		Digest:    "sha256:deadbeefcafebabe0000000000000000000000000000000000000000000000ff",
		MediaType: "application/vnd.cogos.trace.assessment.v1+json",
		Size:      512,
		Hash:      "hash-byref",
	}

	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}

	// By-reference form must serialize the ADR-084 fields and omit empty payload.
	for _, field := range []string{"digest", "media_type"} {
		if _, ok := raw[field]; !ok {
			t.Errorf("by-reference block should serialize %q", field)
		}
	}
	if _, exists := raw["payload"]; exists {
		t.Errorf("by-reference block should omit empty payload field, got %s", string(raw["payload"]))
	}

	var decoded Block
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Digest != block.Digest {
		t.Errorf("Digest = %q, want %q", decoded.Digest, block.Digest)
	}
	if decoded.MediaType != block.MediaType {
		t.Errorf("MediaType = %q, want %q", decoded.MediaType, block.MediaType)
	}
	if decoded.Size != 512 {
		t.Errorf("Size = %d, want 512", decoded.Size)
	}
	if len(decoded.Payload) != 0 {
		t.Errorf("Payload should be empty, got %v", decoded.Payload)
	}
}
