// bus_session_test.go — unit tests for BusSessionManager.
//
// Track 5 Phase 3: verifies hash-chain continuity, bus isolation, and the
// byte-compat storage layout (.cog/.state/buses/{id}/events.jsonl).
package engine

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBusSessionAppendAndRead covers the basic seq/hash chain and JSONL
// storage layout.
func TestBusSessionAppendAndRead(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mgr := NewBusSessionManager(root)

	if err := mgr.EnsureBus("bus-a"); err != nil {
		t.Fatalf("EnsureBus: %v", err)
	}

	e1, err := mgr.AppendEvent("bus-a", "message", "alice", map[string]interface{}{"content": "hello"})
	if err != nil {
		t.Fatalf("AppendEvent 1: %v", err)
	}
	if e1.Seq != 1 {
		t.Errorf("seq = %d, want 1", e1.Seq)
	}
	if e1.Hash == "" || len(e1.Hash) != 64 {
		t.Errorf("hash not 64-char hex: %q", e1.Hash)
	}
	if _, err := hex.DecodeString(e1.Hash); err != nil {
		t.Errorf("hash not lowercase hex: %v", err)
	}
	if len(e1.Prev) != 0 || e1.PrevHash != "" {
		t.Errorf("first event should have no prev: prev=%v prev_hash=%q", e1.Prev, e1.PrevHash)
	}

	e2, err := mgr.AppendEvent("bus-a", "message", "bob", map[string]interface{}{"content": "world"})
	if err != nil {
		t.Fatalf("AppendEvent 2: %v", err)
	}
	if e2.Seq != 2 {
		t.Errorf("seq = %d, want 2", e2.Seq)
	}
	if len(e2.Prev) != 1 || e2.Prev[0] != e1.Hash {
		t.Errorf("prev chain broken: got %v, want [%s]", e2.Prev, e1.Hash)
	}
	if e2.PrevHash != e1.Hash {
		t.Errorf("prev_hash = %q, want %q", e2.PrevHash, e1.Hash)
	}

	// Verify storage layout matches root: .cog/.state/buses/{bus_id}/events.jsonl
	eventsFile := filepath.Join(root, ".cog", ".state", "buses", "bus-a", "events.jsonl")
	b, err := os.ReadFile(eventsFile)
	if err != nil {
		t.Fatalf("read events.jsonl: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Errorf("events.jsonl has %d lines, want 2", len(lines))
	}

	// Verify ReadEvents de-dups by seq and preserves order.
	events, err := mgr.ReadEvents("bus-a")
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(events))
	}
	if events[0].Seq != 1 || events[1].Seq != 2 {
		t.Errorf("seq order broken: %d, %d", events[0].Seq, events[1].Seq)
	}
}

// TestBusSessionHashChainRecompute verifies that re-computing the hash of a
// read-back event yields the stored hash (byte-compat with root's hash).
func TestBusSessionHashChainRecompute(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mgr := NewBusSessionManager(root)

	_, _ = mgr.AppendEvent("chain", "event.a", "sender", map[string]interface{}{"k": "v"})
	_, _ = mgr.AppendEvent("chain", "event.b", "sender", map[string]interface{}{"k": 42.0})
	_, _ = mgr.AppendEvent("chain", "event.c", "sender", map[string]interface{}{"k": true})

	events, err := mgr.ReadEvents("chain")
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}

	var prevHash string
	for i, e := range events {
		// Re-compute the hash from the event and compare.
		recomputed := computeBusBlockHash(&e)
		if recomputed != e.Hash {
			t.Errorf("event %d: recomputed hash %q != stored hash %q", i, recomputed, e.Hash)
		}
		// Verify the chain links.
		if i == 0 {
			if e.PrevHash != "" {
				t.Errorf("event 0 PrevHash = %q, want empty", e.PrevHash)
			}
		} else if e.PrevHash != prevHash {
			t.Errorf("event %d PrevHash = %q, want %q", i, e.PrevHash, prevHash)
		}
		prevHash = e.Hash
	}
}

// TestBusSessionIsolation verifies two buses don't cross-contaminate.
func TestBusSessionIsolation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mgr := NewBusSessionManager(root)

	a1, _ := mgr.AppendEvent("bus-a", "m", "x", map[string]interface{}{"v": 1})
	b1, _ := mgr.AppendEvent("bus-b", "m", "x", map[string]interface{}{"v": 1})
	a2, _ := mgr.AppendEvent("bus-a", "m", "x", map[string]interface{}{"v": 2})

	if a1.Seq != 1 || b1.Seq != 1 || a2.Seq != 2 {
		t.Errorf("bus seqs wrong: a1=%d b1=%d a2=%d", a1.Seq, b1.Seq, a2.Seq)
	}
	if len(b1.Prev) != 0 {
		t.Errorf("bus-b first event shouldn't chain from bus-a: prev=%v", b1.Prev)
	}

	aEvents, _ := mgr.ReadEvents("bus-a")
	bEvents, _ := mgr.ReadEvents("bus-b")
	if len(aEvents) != 2 {
		t.Errorf("bus-a has %d events, want 2", len(aEvents))
	}
	if len(bEvents) != 1 {
		t.Errorf("bus-b has %d events, want 1", len(bEvents))
	}
}

// TestBusSessionRegistry covers RegisterBus + LoadRegistry round-trip and
// verifies the registry.json format matches root.
func TestBusSessionRegistry(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mgr := NewBusSessionManager(root)

	if err := mgr.EnsureBus("r1"); err != nil {
		t.Fatalf("EnsureBus: %v", err)
	}
	if err := mgr.RegisterBus("r1", "sess1", "test"); err != nil {
		t.Fatalf("RegisterBus: %v", err)
	}

	entries := mgr.LoadRegistry()
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	if entries[0].BusID != "r1" || entries[0].State != "active" {
		t.Errorf("registry shape wrong: %+v", entries[0])
	}
	if entries[0].Transport != "file" {
		t.Errorf("Transport = %q, want 'file'", entries[0].Transport)
	}
	if entries[0].Endpoint != filepath.Join(".cog", ".state", "buses", "r1") {
		t.Errorf("Endpoint = %q", entries[0].Endpoint)
	}

	// Append an event, verify registry got updated.
	_, _ = mgr.AppendEvent("r1", "m", "from", map[string]interface{}{"x": 1})
	entries = mgr.LoadRegistry()
	if entries[0].LastEventSeq != 1 || entries[0].EventCount != 1 {
		t.Errorf("registry not updated after append: %+v", entries[0])
	}

	// Verify registry.json is valid JSON with the expected field names.
	regBytes, err := os.ReadFile(mgr.RegistryPath())
	if err != nil {
		t.Fatalf("read registry.json: %v", err)
	}
	var parsed []map[string]interface{}
	if err := json.Unmarshal(regBytes, &parsed); err != nil {
		t.Fatalf("registry.json not valid JSON: %v", err)
	}
	wantFields := []string{"bus_id", "state", "participants", "transport", "endpoint", "created_at", "last_event_seq", "last_event_at", "event_count"}
	for _, f := range wantFields {
		if _, ok := parsed[0][f]; !ok {
			t.Errorf("registry.json missing field %q", f)
		}
	}
}

// TestBusSessionByteCompatJSONShape verifies that the JSON encoding of a bus
// event matches the shape captured from the live cogos-v3 daemon:
//
//	{v, bus_id, seq, ts, from, type, payload, prev?, prev_hash?, hash}
func TestBusSessionByteCompatJSONShape(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mgr := NewBusSessionManager(root)

	// First event — no prev chain.
	e1, err := mgr.AppendEvent("phase3-test", "message", "phase3-test",
		map[string]interface{}{"content": "shape-probe"})
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	raw, _ := json.Marshal(e1)
	var parsed map[string]interface{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Required fields that the bridge (cogos_bridge.py) reads on every
	// event. If any of these go missing, the bridge breaks.
	wantRequired := []string{"v", "bus_id", "seq", "ts", "from", "type", "payload", "hash"}
	for _, f := range wantRequired {
		if _, ok := parsed[f]; !ok {
			t.Errorf("first event missing required field %q", f)
		}
	}
	// On the first event `prev` + `prev_hash` should be omitted (omitempty).
	if _, has := parsed["prev"]; has {
		t.Errorf("first event unexpectedly has 'prev': %v", parsed["prev"])
	}
	if _, has := parsed["prev_hash"]; has {
		t.Errorf("first event unexpectedly has 'prev_hash': %v", parsed["prev_hash"])
	}

	// Second event — should carry prev and prev_hash.
	e2, _ := mgr.AppendEvent("phase3-test", "message", "phase3-test",
		map[string]interface{}{"content": "second"})
	raw2, _ := json.Marshal(e2)
	var p2 map[string]interface{}
	_ = json.Unmarshal(raw2, &p2)
	if _, ok := p2["prev"]; !ok {
		t.Errorf("second event missing 'prev'")
	}
	if _, ok := p2["prev_hash"]; !ok {
		t.Errorf("second event missing 'prev_hash'")
	}

	// Value-level checks on the first event.
	if parsed["v"].(float64) != 2 {
		t.Errorf("v = %v, want 2", parsed["v"])
	}
	if parsed["bus_id"].(string) != "phase3-test" {
		t.Errorf("bus_id = %v", parsed["bus_id"])
	}
	if parsed["seq"].(float64) != 1 {
		t.Errorf("seq = %v, want 1", parsed["seq"])
	}
	if parsed["type"].(string) != "message" {
		t.Errorf("type = %v", parsed["type"])
	}
	// Ts must be RFC3339-nano style.
	ts, ok := parsed["ts"].(string)
	if !ok || !strings.Contains(ts, "T") || !strings.HasSuffix(ts, "Z") {
		t.Errorf("ts shape wrong: %q", ts)
	}
	// Hash is lowercase hex, 64 chars.
	h, ok := parsed["hash"].(string)
	if !ok || len(h) != 64 {
		t.Errorf("hash shape wrong: %q", h)
	}
	if _, err := hex.DecodeString(h); err != nil {
		t.Errorf("hash not hex: %v", err)
	}
}

// TestBusSessionEventHandlerDispatch verifies that registered handlers
// fire after AppendEvent, outside the lock.
func TestBusSessionEventHandlerDispatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mgr := NewBusSessionManager(root)

	seen := make(chan *BusBlock, 4)
	mgr.AddEventHandler("test", func(busID string, block *BusBlock) {
		seen <- block
	})

	_, _ = mgr.AppendEvent("bus-h", "m", "x", map[string]interface{}{"v": 1})
	_, _ = mgr.AppendEvent("bus-h", "m", "x", map[string]interface{}{"v": 2})

	for i := 0; i < 2; i++ {
		select {
		case evt := <-seen:
			if evt.Seq != i+1 {
				t.Errorf("handler received seq=%d, want %d", evt.Seq, i+1)
			}
		default:
			t.Errorf("handler didn't receive event %d", i+1)
		}
	}

	mgr.RemoveEventHandler("test")
	_, _ = mgr.AppendEvent("bus-h", "m", "x", map[string]interface{}{"v": 3})
	select {
	case evt := <-seen:
		t.Errorf("handler fired after removal: %+v", evt)
	default:
		// ok — handler was properly removed
	}
}
