package main

// bus_api_contract_test.go — regression tests asserting the HTTP contract for
// the root-package bus API remains byte-compatible with pre-event-bus-PR
// consumers (e.g. cog-sandbox-mcp HTTP bridge).
//
// These tests do NOT exercise the internal/engine event bus. The two worlds
// coexist: internal/engine/ is the v3 kernel surface; the root package
// serves the legacy bus_* endpoints. The event-bus PR only touches
// internal/engine, but this file pins the root-package shape so an
// accidental reshape there is caught in CI.

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestBusSendResponseShape asserts the JSON keys and types on
// busSendResponse, which is what POST /v1/bus/send returns. Any rename,
// removal, or retyping would break the cog-sandbox-mcp bridge at
// localhost:7823.
//
// Expected shape (contract, byte-compatible with pre-PR daemons):
//
//	{"ok": bool, "seq": int, "hash": string}
func TestBusSendResponseShape(t *testing.T) {
	t.Parallel()
	resp := busSendResponse{OK: true, Seq: 42, Hash: "deadbeef"}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	want := map[string]interface{}{
		"ok":   true,
		"seq":  float64(42), // JSON numbers decode as float64
		"hash": "deadbeef",
	}
	if !reflect.DeepEqual(decoded, want) {
		t.Errorf("busSendResponse keys = %v; want %v (HTTP contract violation)", decoded, want)
	}

	// Extra paranoia: ensure exactly these three keys and no more.
	gotKeys := map[string]bool{}
	for k := range decoded {
		gotKeys[k] = true
	}
	wantKeys := map[string]bool{"ok": true, "seq": true, "hash": true}
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Errorf("busSendResponse key set = %v; want %v", gotKeys, wantKeys)
	}
}

// TestBusEventsResponseShape pins the per-event shape returned by
// GET /v1/bus/{bus_id}/events. The response is a JSON array of full
// CogBlock values; downstream consumers depend on `seq`, `hash`,
// `prev_hash`, `type`, `from`, `payload`, `ts`, and `bus_id`.
func TestBusEventsResponseShape(t *testing.T) {
	t.Parallel()
	// Build a minimal but realistic event — mirrors what appendBusEvent
	// produces.
	evt := CogBlock{
		V:        2,
		BusID:    "test-bus",
		Seq:      7,
		Ts:       "2026-04-21T00:00:00Z",
		From:     "contract-test",
		Type:     "message",
		Payload:  map[string]interface{}{"content": "hi"},
		Hash:     "h7",
		PrevHash: "h6",
		Prev:     []string{"h6"},
	}
	events := []CogBlock{evt}
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded []map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded) != 1 {
		t.Fatalf("len=%d; want 1", len(decoded))
	}
	got := decoded[0]

	// Core fields the cog-sandbox-mcp bridge consumes. We do NOT assert a
	// strict key-set because CogBlock has many optional fields (Sig, To,
	// Meta, Agent, Prev, LegacyPrev, etc.) that are conditionally present.
	for _, k := range []string{"bus_id", "seq", "ts", "from", "type", "payload", "hash"} {
		if _, ok := got[k]; !ok {
			t.Errorf("event missing required key %q; full value: %v", k, got)
		}
	}

	// seq must be a JSON number.
	if _, ok := got["seq"].(float64); !ok {
		t.Errorf("seq type = %T; want number", got["seq"])
	}
	// hash must be a string.
	if _, ok := got["hash"].(string); !ok {
		t.Errorf("hash type = %T; want string", got["hash"])
	}
	// payload must be a JSON object.
	if _, ok := got["payload"].(map[string]interface{}); !ok {
		t.Errorf("payload type = %T; want object", got["payload"])
	}
}

// TestBusSendRequestShape asserts the request body shape accepted by
// POST /v1/bus/send. A rename here would silently drop fields.
func TestBusSendRequestShape(t *testing.T) {
	t.Parallel()
	// Canonical request body that cog-sandbox-mcp sends.
	body := `{
		"bus_id": "b",
		"from": "alice",
		"to": "bob",
		"message": "hello",
		"type": "message"
	}`
	var req busSendRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.BusID != "b" || req.From != "alice" || req.To != "bob" ||
		req.Message != "hello" || req.Type != "message" {
		t.Errorf("decoded request = %+v; expected all fields set", req)
	}
}
