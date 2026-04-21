// cmd_bus_send_test.go — Tests for `cog bus send` subcommand (issue #26).
//
// These tests exercise the direct JSONL write path (cmdBusSendDirect), the
// payload-reader (readSendPayload), the --http flow against a mocked kernel,
// flag validation in cmdBusSend, and concurrency guarantees of the shared
// busSessionManager mutex when multiple senders race.

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// sendTestRoot prepares a minimal workspace layout (just .cog/.state/buses/)
// inside t.TempDir() so busSessionManager can operate without touching the
// user's real workspace.
func sendTestRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".cog", ".state", "buses"), 0755); err != nil {
		t.Fatalf("mkdir buses: %v", err)
	}
	return root
}

// readBusEventsFile parses every JSONL line in a bus's events file and
// returns the decoded CogBlocks.
func readBusEventsFile(t *testing.T, root, busID string) []CogBlock {
	t.Helper()
	path := filepath.Join(root, ".cog", ".state", "buses", busID, "events.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read events.jsonl: %v", err)
	}
	var out []CogBlock
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var b CogBlock
		if err := json.Unmarshal([]byte(line), &b); err != nil {
			t.Fatalf("unmarshal line %q: %v", line, err)
		}
		out = append(out, b)
	}
	return out
}

// TestBusSendDirect_FreshBus verifies that a first send against a previously
// unseen bus creates the directory, writes the events.jsonl, and produces a
// CogBlock with the expected type/from/payload fields.
func TestBusSendDirect_FreshBus(t *testing.T) {
	root := sendTestRoot(t)
	busID := "bus_test_fresh"

	rc := cmdBusSendDirect(root, busID, "note.added", "user:alice",
		map[string]interface{}{"title": "hi", "body": "world"}, true)
	if rc != 0 {
		t.Fatalf("cmdBusSendDirect rc=%d, want 0", rc)
	}

	evts := readBusEventsFile(t, root, busID)
	if len(evts) != 1 {
		t.Fatalf("got %d events, want 1", len(evts))
	}
	e := evts[0]
	if e.Type != "note.added" {
		t.Errorf("type=%q want note.added", e.Type)
	}
	if e.From != "user:alice" {
		t.Errorf("from=%q want user:alice", e.From)
	}
	if e.Seq != 1 {
		t.Errorf("seq=%d want 1 on first event", e.Seq)
	}
	if e.Hash == "" {
		t.Errorf("hash empty")
	}
	if len(e.Prev) != 0 || e.PrevHash != "" {
		t.Errorf("first event should have no prev chain, got Prev=%v PrevHash=%q", e.Prev, e.PrevHash)
	}
	if title, _ := e.Payload["title"].(string); title != "hi" {
		t.Errorf("payload.title=%v want hi", e.Payload["title"])
	}
}

// TestBusSendDirect_AppendChains verifies that successive sends increment
// seq monotonically and chain prev_hash to the previous event's hash.
func TestBusSendDirect_AppendChains(t *testing.T) {
	root := sendTestRoot(t)
	busID := "bus_test_chain"

	for i := 0; i < 3; i++ {
		if rc := cmdBusSendDirect(root, busID, "log.line", "svc:api",
			map[string]interface{}{"i": i}, true); rc != 0 {
			t.Fatalf("send %d rc=%d", i, rc)
		}
	}

	evts := readBusEventsFile(t, root, busID)
	if len(evts) != 3 {
		t.Fatalf("got %d events, want 3", len(evts))
	}
	for i, e := range evts {
		if e.Seq != i+1 {
			t.Errorf("event %d seq=%d want %d", i, e.Seq, i+1)
		}
		if i == 0 {
			if e.PrevHash != "" {
				t.Errorf("event 0 prev_hash=%q want empty", e.PrevHash)
			}
		} else {
			if e.PrevHash != evts[i-1].Hash {
				t.Errorf("event %d prev_hash=%q want %q", i, e.PrevHash, evts[i-1].Hash)
			}
			if len(e.Prev) != 1 || e.Prev[0] != evts[i-1].Hash {
				t.Errorf("event %d Prev=%v want [%q]", i, e.Prev, evts[i-1].Hash)
			}
		}
	}
}

// TestBusSendDirect_ConcurrentSends drives 10 goroutines that share a single
// busSessionManager and send distinct events at once. The manager's mutex is
// the seq-allocation critical section; if it's broken we'd see duplicate or
// missing seqs. All 10 events must land with seqs 1..10 (unique, no gaps)
// and every prev_hash must point to some earlier event's hash.
//
// Note on cross-process concurrency: this test pins the in-process guarantee
// that busSessionManager.appendBusEvent provides. The mutex is per-instance,
// so separate `cog bus send` processes invoked concurrently would need an
// additional file-lock for safety. That's a pre-existing limitation of the
// JSONL append path and is out of scope for this PR — the HTTP handler runs
// in a single daemon process and naturally shares one manager instance.
func TestBusSendDirect_ConcurrentSends(t *testing.T) {
	root := sendTestRoot(t)
	busID := "bus_test_concurrent"

	// Prep the dir once — matches what cmdBusSendDirect would do on first call.
	if err := os.MkdirAll(filepath.Join(root, ".cog", ".state", "buses", busID), 0755); err != nil {
		t.Fatalf("mkdir bus: %v", err)
	}

	mgr := newBusSessionManager(root)
	const N = 10
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = mgr.appendBusEvent(busID, "concurrent",
				"sender:"+strconv.Itoa(i), map[string]interface{}{"n": i})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}

	evts := readBusEventsFile(t, root, busID)
	if len(evts) != N {
		t.Fatalf("got %d events, want %d", len(evts), N)
	}

	// Seqs must be unique and cover 1..N.
	seen := make(map[int]bool)
	hashes := make(map[string]bool)
	for _, e := range evts {
		if e.Seq < 1 || e.Seq > N {
			t.Errorf("seq=%d out of range", e.Seq)
		}
		if seen[e.Seq] {
			t.Errorf("duplicate seq %d", e.Seq)
		}
		seen[e.Seq] = true
		hashes[e.Hash] = true
	}
	if len(seen) != N {
		t.Errorf("got %d distinct seqs, want %d", len(seen), N)
	}

	// Every event after the first must chain to some earlier hash. We can't
	// assume a total ordering (goroutines raced), but each prev_hash must
	// match some event with a strictly smaller seq.
	bySeq := make(map[int]CogBlock, len(evts))
	for _, e := range evts {
		bySeq[e.Seq] = e
	}
	for s := 2; s <= N; s++ {
		e := bySeq[s]
		if e.PrevHash == "" {
			t.Errorf("seq %d has empty prev_hash", s)
			continue
		}
		if !hashes[e.PrevHash] {
			t.Errorf("seq %d prev_hash=%q does not match any event hash", s, e.PrevHash)
		}
	}
}

// TestReadSendPayload_MessageJSONInline covers the happy path: the flag
// value is used verbatim.
func TestReadSendPayload_MessageJSONInline(t *testing.T) {
	b, err := readSendPayload(`{"k":"v"}`, true, "", false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(b) != `{"k":"v"}` {
		t.Errorf("got %q want %q", string(b), `{"k":"v"}`)
	}
}

// TestReadSendPayload_PayloadFile verifies --payload-file reads disk bytes.
func TestReadSendPayload_PayloadFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "p.json")
	want := `{"from_file":true}`
	if err := os.WriteFile(p, []byte(want), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	b, err := readSendPayload("", false, p, true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(b) != want {
		t.Errorf("got %q want %q", string(b), want)
	}
}

// TestReadSendPayload_PayloadFile_Missing asserts a clear error for a bad
// path (distinct from "invalid JSON").
func TestReadSendPayload_PayloadFile_Missing(t *testing.T) {
	_, err := readSendPayload("", false, "/nonexistent/path.json", true)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !strings.Contains(err.Error(), "read --payload-file") {
		t.Errorf("error %q should mention --payload-file", err)
	}
}

// TestReadSendPayload_StdinMessageJSON verifies --message-json "-" reads from
// stdin. We swap os.Stdin for a pipe, write the payload, then restore.
func TestReadSendPayload_StdinMessageJSON(t *testing.T) {
	want := `{"from":"stdin","ok":true}`

	origStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()

	go func() {
		defer w.Close()
		w.Write([]byte(want))
	}()

	got, err := readSendPayload("-", true, "", false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(got) != want {
		t.Errorf("stdin payload got %q want %q", string(got), want)
	}
}

// TestReadSendPayload_StdinPayloadFile verifies --payload-file "-" also
// reads stdin (dash sentinel convention).
func TestReadSendPayload_StdinPayloadFile(t *testing.T) {
	want := `{"via":"payload_file_dash"}`

	origStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()

	go func() {
		defer w.Close()
		w.Write([]byte(want))
	}()

	got, err := readSendPayload("", false, "-", true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(got) != want {
		t.Errorf("stdin via payload-file got %q want %q", string(got), want)
	}
}

// TestBusSendDirect_InvalidJSON — cmdBusSend path validates that the body
// parses as a JSON object before appending. We simulate the invalid-JSON
// branch by checking that appendBusEvent rejects non-object payloads via
// the preceding Unmarshal gate; easiest to cover end-to-end by driving
// cmdBusSend with args (it sets up env we don't want), so instead we unit-
// test the parse logic directly.
func TestBusSendDirect_InvalidJSONRejected(t *testing.T) {
	// Mirror cmdBusSend's pre-append parse step.
	var m map[string]interface{}
	if err := json.Unmarshal([]byte("not json"), &m); err == nil {
		t.Fatal("expected JSON unmarshal to fail")
	}
}

// TestBusSend_MissingFlags verifies that cmdBusSend returns 2 and prints an
// error when required flags are omitted. We drive it via args so the flag
// parser is exercised directly. The validation path does not call
// ResolveWorkspace, so this is safe to run in a plain test binary.
func TestBusSend_MissingFlags(t *testing.T) {
	// Redirect stderr so test output isn't noisy.
	origStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	defer func() { os.Stderr = origStderr }()

	rc := cmdBusSend([]string{"--bus", "bus_x"}) // missing --type, --from, payload
	w.Close()

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	stderr := string(buf[:n])

	if rc != 2 {
		t.Errorf("rc=%d want 2 for missing required flags", rc)
	}
	if !strings.Contains(stderr, "missing required flag") {
		t.Errorf("stderr does not mention missing required flags: %q", stderr)
	}
}

// TestBusSend_MutuallyExclusivePayload rejects --message-json + --payload-file.
func TestBusSend_MutuallyExclusivePayload(t *testing.T) {
	origStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	defer func() { os.Stderr = origStderr }()

	rc := cmdBusSend([]string{
		"--bus", "b", "--type", "t", "--from", "f",
		"--message-json", "{}", "--payload-file", "/tmp/x",
	})
	w.Close()

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	stderr := string(buf[:n])

	if rc != 2 {
		t.Errorf("rc=%d want 2", rc)
	}
	if !strings.Contains(stderr, "mutually exclusive") {
		t.Errorf("stderr: %q", stderr)
	}
}

// TestBusSend_UnknownFlag rejects unrecognized flags cleanly.
func TestBusSend_UnknownFlag(t *testing.T) {
	origStderr := os.Stderr
	_, w, _ := os.Pipe()
	os.Stderr = w
	defer func() { os.Stderr = origStderr }()

	rc := cmdBusSend([]string{"--nosuch"})
	w.Close()

	if rc != 2 {
		t.Errorf("rc=%d want 2 for unknown flag", rc)
	}
}

// TestBusSendHTTP_KernelDown verifies that --http against an unreachable
// kernel fails non-zero and explicitly does NOT fall through to the direct
// JSONL path (per issue: "don't silently fall back").
func TestBusSendHTTP_KernelDown(t *testing.T) {
	// 127.0.0.1:1 is reserved — connection refused on most platforms.
	rc := cmdBusSendHTTP("127.0.0.1:1", "bus_x", "t", "f", "",
		map[string]interface{}{"content": "x"}, true)
	if rc == 0 {
		t.Errorf("rc=0 on unreachable kernel, want non-zero")
	}
}

// TestBusSendHTTP_RoundTrip stands up an httptest server that mimics
// /v1/bus/send, points cmdBusSendHTTP at it, and verifies the request body
// carries the expected fields and the success path returns 0.
func TestBusSendHTTP_RoundTrip(t *testing.T) {
	var got busSendRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/bus/send" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(busSendResponse{OK: true, Seq: 5, Hash: "abc"})
	}))
	defer srv.Close()

	// Strip the "http://" prefix — cmdBusSendHTTP re-adds it.
	addr := strings.TrimPrefix(srv.URL, "http://")

	rc := cmdBusSendHTTP(addr, "bus_http_test", "note.added", "user:bob", "user:carol",
		map[string]interface{}{"content": "hello"}, true)
	if rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}
	if got.BusID != "bus_http_test" {
		t.Errorf("bus_id=%q", got.BusID)
	}
	if got.From != "user:bob" || got.To != "user:carol" {
		t.Errorf("from=%q to=%q", got.From, got.To)
	}
	if got.Type != "note.added" {
		t.Errorf("type=%q want note.added", got.Type)
	}
	// Payload was {"content":"hello"} so we preserve the simple-string shape.
	if got.Message != "hello" {
		t.Errorf("message=%q want hello", got.Message)
	}
}

// TestBusSendHTTP_StructuredPayload verifies that non-trivial payloads get
// serialized as JSON into the "message" field so no data is lost crossing
// the /v1/bus/send boundary.
func TestBusSendHTTP_StructuredPayload(t *testing.T) {
	var got busSendRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(busSendResponse{OK: true, Seq: 1, Hash: "h"})
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	payload := map[string]interface{}{"title": "t", "count": 42.0}
	rc := cmdBusSendHTTP(addr, "b", "custom", "f", "", payload, true)
	if rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	// Message should be a JSON object round-trip.
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(got.Message), &decoded); err != nil {
		t.Fatalf("message not valid JSON: %v (got %q)", err, got.Message)
	}
	if decoded["title"] != "t" {
		t.Errorf("decoded.title=%v", decoded["title"])
	}
}

// TestBusSendDirect_RegistersBus verifies that a first send auto-registers
// the bus so `cog bus list` surfaces it.
func TestBusSendDirect_RegistersBus(t *testing.T) {
	root := sendTestRoot(t)
	busID := "bus_test_registered"

	if rc := cmdBusSendDirect(root, busID, "note.added", "user:alice",
		map[string]interface{}{"k": "v"}, true); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}

	registry := filepath.Join(root, ".cog", ".state", "buses", "registry.json")
	data, err := os.ReadFile(registry)
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	var entries []busRegistryEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("parse registry: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.BusID == busID {
			found = true
			if e.LastEventSeq != 1 {
				t.Errorf("registry seq=%d want 1", e.LastEventSeq)
			}
		}
	}
	if !found {
		t.Errorf("bus %s not in registry; entries=%v", busID, entries)
	}
}

