package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── Fixture helpers ────────────────────────────────────────────────────────

// appendTestEvent writes a real, hash-chained event via AppendEvent. The
// caller controls the type and data; timestamp defaults to now unless
// supplied via ts.
func appendTestEvent(t *testing.T, root, sessionID, evtType string, data map[string]interface{}, ts string) *EventEnvelope {
	t.Helper()
	env := &EventEnvelope{
		HashedPayload: EventPayload{
			Type:      evtType,
			Timestamp: ts,
			SessionID: sessionID,
			Data:      data,
		},
		Metadata: EventMetadata{Source: "test"},
	}
	if ts == "" {
		env.HashedPayload.Timestamp = nowISO()
	}
	if err := AppendEvent(root, sessionID, env); err != nil {
		t.Fatalf("AppendEvent %s/%s: %v", sessionID, evtType, err)
	}
	return env
}

// readRawEventLines returns the on-disk JSONL lines verbatim.
func readRawEventLines(t *testing.T, root, sessionID string) []string {
	t.Helper()
	path := filepath.Join(root, ".cog", "ledger", sessionID, "events.jsonl")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	raw := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	return raw
}

// overwriteEventsFile writes the given JSONL lines back to disk (tests use
// this to simulate tampering).
func overwriteEventsFile(t *testing.T, root, sessionID string, lines []string) {
	t.Helper()
	path := filepath.Join(root, ".cog", "ledger", sessionID, "events.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatalf("overwrite %s: %v", path, err)
	}
}

// ── QueryLedger tests ──────────────────────────────────────────────────────

func TestQueryLedgerEmpty(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	result, err := QueryLedger(root, LedgerQuery{})
	if err != nil {
		t.Fatalf("QueryLedger: %v", err)
	}
	if result.Count != 0 {
		t.Errorf("Count = %d; want 0", result.Count)
	}
	if len(result.Events) != 0 {
		t.Errorf("events len = %d; want 0", len(result.Events))
	}
	if result.Truncated {
		t.Errorf("Truncated = true; want false")
	}
	if result.Verification.Requested {
		t.Errorf("Verification.Requested = true; want false")
	}
}

func TestQueryLedgerSingleSession(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sid := "session-single"

	appendTestEvent(t, root, sid, "process.start", nil, "")
	appendTestEvent(t, root, sid, "attention.boost", map[string]interface{}{"weight": 1.0}, "")
	appendTestEvent(t, root, sid, "insight.captured", map[string]interface{}{"summary": "hi"}, "")

	result, err := QueryLedger(root, LedgerQuery{SessionID: sid})
	if err != nil {
		t.Fatalf("QueryLedger: %v", err)
	}
	if result.Count != 3 {
		t.Errorf("Count = %d; want 3", result.Count)
	}
	// Intra-session events are ascending by seq.
	for i, ev := range result.Events {
		if ev.Seq != int64(i+1) {
			t.Errorf("events[%d].Seq = %d; want %d", i, ev.Seq, i+1)
		}
		if ev.SessionID != sid {
			t.Errorf("events[%d].SessionID = %q; want %q", i, ev.SessionID, sid)
		}
		if ev.Hash == "" {
			t.Errorf("events[%d].Hash empty", i)
		}
	}
	if result.NextAfterSeq != 3 {
		t.Errorf("NextAfterSeq = %d; want 3", result.NextAfterSeq)
	}
}

func TestQueryLedgerMultiSessionOrdering(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Session A first (older), then session B (newer) — mtime desc means B first.
	appendTestEvent(t, root, "sess-a", "a.one", nil, "")
	// Nudge mtime so the ordering is deterministic.
	timeA := time.Now().Add(-2 * time.Minute)
	_ = os.Chtimes(filepath.Join(root, ".cog", "ledger", "sess-a", "events.jsonl"), timeA, timeA)
	appendTestEvent(t, root, "sess-b", "b.one", nil, "")
	appendTestEvent(t, root, "sess-b", "b.two", nil, "")
	timeB := time.Now()
	_ = os.Chtimes(filepath.Join(root, ".cog", "ledger", "sess-b", "events.jsonl"), timeB, timeB)

	result, err := QueryLedger(root, LedgerQuery{})
	if err != nil {
		t.Fatalf("QueryLedger: %v", err)
	}
	if result.Count != 3 {
		t.Fatalf("Count = %d; want 3", result.Count)
	}
	// Expect sess-b events first (newer mtime), then sess-a.
	if result.Events[0].SessionID != "sess-b" {
		t.Errorf("events[0].SessionID = %q; want sess-b", result.Events[0].SessionID)
	}
	if result.Events[2].SessionID != "sess-a" {
		t.Errorf("events[2].SessionID = %q; want sess-a", result.Events[2].SessionID)
	}
	// next_after_seq is only set for single-session queries.
	if result.NextAfterSeq != 0 {
		t.Errorf("NextAfterSeq = %d; want 0 (unset for multi-session)", result.NextAfterSeq)
	}
}

func TestQueryLedgerFilterByTypeExact(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sid := "session-filter-exact"
	appendTestEvent(t, root, sid, "attention.boost", nil, "")
	appendTestEvent(t, root, sid, "insight.captured", nil, "")
	appendTestEvent(t, root, sid, "attention.boost", nil, "")

	result, err := QueryLedger(root, LedgerQuery{SessionID: sid, EventType: "attention.boost"})
	if err != nil {
		t.Fatalf("QueryLedger: %v", err)
	}
	if result.Count != 2 {
		t.Fatalf("Count = %d; want 2", result.Count)
	}
	for _, ev := range result.Events {
		if ev.Type != "attention.boost" {
			t.Errorf("event.Type = %q; want attention.boost", ev.Type)
		}
	}
}

func TestQueryLedgerFilterByTypeWildcard(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sid := "session-filter-wild"
	appendTestEvent(t, root, sid, "attention.boost", nil, "")
	appendTestEvent(t, root, sid, "attention.decay", nil, "")
	appendTestEvent(t, root, sid, "insight.captured", nil, "")

	result, err := QueryLedger(root, LedgerQuery{SessionID: sid, EventType: "attention.*"})
	if err != nil {
		t.Fatalf("QueryLedger: %v", err)
	}
	if result.Count != 2 {
		t.Fatalf("Count = %d; want 2", result.Count)
	}
	for _, ev := range result.Events {
		if !strings.HasPrefix(ev.Type, "attention") {
			t.Errorf("event.Type = %q; want attention.*", ev.Type)
		}
	}
}

func TestQueryLedgerAfterSeqPagination(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sid := "session-paginate"
	for i := 0; i < 5; i++ {
		appendTestEvent(t, root, sid, fmt.Sprintf("e.%d", i), nil, "")
	}

	result, err := QueryLedger(root, LedgerQuery{SessionID: sid, AfterSeq: 2})
	if err != nil {
		t.Fatalf("QueryLedger: %v", err)
	}
	if result.Count != 3 {
		t.Fatalf("Count = %d; want 3", result.Count)
	}
	if result.Events[0].Seq != 3 {
		t.Errorf("events[0].Seq = %d; want 3", result.Events[0].Seq)
	}
	if result.NextAfterSeq != 5 {
		t.Errorf("NextAfterSeq = %d; want 5", result.NextAfterSeq)
	}
}

func TestQueryLedgerAfterSeqRequiresSessionID(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	_, err := QueryLedger(root, LedgerQuery{AfterSeq: 5})
	if !errors.Is(err, ErrAfterSeqRequiresSession) {
		t.Fatalf("err = %v; want ErrAfterSeqRequiresSession", err)
	}
}

func TestQueryLedgerSinceTimestamp(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sid := "session-since"

	// Use fixed timestamps so the filter is deterministic.
	appendTestEvent(t, root, sid, "old.event", nil, "2024-01-01T00:00:00Z")
	appendTestEvent(t, root, sid, "new.event", nil, "2026-06-01T00:00:00Z")
	appendTestEvent(t, root, sid, "newer.event", nil, "2026-07-01T00:00:00Z")

	result, err := QueryLedger(root, LedgerQuery{
		SessionID:      sid,
		SinceTimestamp: "2026-05-01T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("QueryLedger: %v", err)
	}
	if result.Count != 2 {
		t.Fatalf("Count = %d; want 2", result.Count)
	}
	for _, ev := range result.Events {
		if ev.Timestamp < "2026-05-01T00:00:00Z" {
			t.Errorf("event.Timestamp = %q; should be >= 2026-05-01", ev.Timestamp)
		}
	}
}

func TestQueryLedgerSinceTimestampBadRFC3339(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	_, err := QueryLedger(root, LedgerQuery{SinceTimestamp: "not-a-date"})
	if err == nil {
		t.Fatal("expected error for malformed timestamp")
	}
	if !strings.Contains(err.Error(), "since_timestamp") {
		t.Errorf("err = %v; want since_timestamp mention", err)
	}
}

func TestQueryLedgerLimitAndTruncation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sid := "session-limit"
	for i := 0; i < 10; i++ {
		appendTestEvent(t, root, sid, fmt.Sprintf("e.%d", i), nil, "")
	}

	result, err := QueryLedger(root, LedgerQuery{SessionID: sid, Limit: 4})
	if err != nil {
		t.Fatalf("QueryLedger: %v", err)
	}
	if result.Count != 4 {
		t.Errorf("Count = %d; want 4", result.Count)
	}
	if !result.Truncated {
		t.Error("Truncated should be true")
	}
	if result.NextAfterSeq != 4 {
		t.Errorf("NextAfterSeq = %d; want 4", result.NextAfterSeq)
	}
}

func TestQueryLedgerVerifyChainValid(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sid := "session-verify-good"
	for i := 0; i < 3; i++ {
		appendTestEvent(t, root, sid, fmt.Sprintf("e.%d", i), map[string]interface{}{"i": i}, "")
	}

	result, err := QueryLedger(root, LedgerQuery{SessionID: sid, VerifyChain: true})
	if err != nil {
		t.Fatalf("QueryLedger: %v", err)
	}
	if !result.Verification.Requested {
		t.Error("Verification.Requested = false; want true")
	}
	if !result.Verification.Valid {
		t.Errorf("Verification.Valid = false; errors=%v", result.Verification.Errors)
	}
	if result.Verification.TotalChecked != 3 {
		t.Errorf("TotalChecked = %d; want 3", result.Verification.TotalChecked)
	}
	if len(result.Verification.Errors) != 0 {
		t.Errorf("errors = %v; want empty", result.Verification.Errors)
	}
}

func TestQueryLedgerVerifyChainBroken(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sid := "session-verify-broken"
	appendTestEvent(t, root, sid, "e.0", nil, "")
	appendTestEvent(t, root, sid, "e.1", nil, "")
	appendTestEvent(t, root, sid, "e.2", nil, "")

	// Tamper with the middle event's stored hash.
	lines := readRawEventLines(t, root, sid)
	var env EventEnvelope
	if err := json.Unmarshal([]byte(lines[1]), &env); err != nil {
		t.Fatalf("unmarshal middle line: %v", err)
	}
	env.Metadata.Hash = "deadbeef" + env.Metadata.Hash[8:]
	tampered, err := json.Marshal(&env)
	if err != nil {
		t.Fatalf("marshal tampered: %v", err)
	}
	lines[1] = string(tampered)
	overwriteEventsFile(t, root, sid, lines)

	result, err := QueryLedger(root, LedgerQuery{SessionID: sid, VerifyChain: true})
	if err != nil {
		t.Fatalf("QueryLedger: %v", err)
	}
	// Data is still returned — the ledger is tamper-evident, not tamper-censoring.
	if result.Count != 3 {
		t.Errorf("Count = %d; want 3 (data should still be returned)", result.Count)
	}
	if result.Verification.Valid {
		t.Error("Verification.Valid = true; want false")
	}
	if result.Verification.FirstBrokenSeq != 2 {
		t.Errorf("FirstBrokenSeq = %d; want 2", result.Verification.FirstBrokenSeq)
	}
	if result.Verification.FirstBrokenSession != sid {
		t.Errorf("FirstBrokenSession = %q; want %q", result.Verification.FirstBrokenSession, sid)
	}
	if len(result.Verification.Errors) == 0 {
		t.Error("expected at least one error entry")
	}
}

func TestQueryLedgerMalformedJSONLine(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sid := "session-malformed"
	appendTestEvent(t, root, sid, "good.event.1", nil, "")
	appendTestEvent(t, root, sid, "good.event.2", nil, "")

	// Inject a malformed line between the two valid events.
	lines := readRawEventLines(t, root, sid)
	lines = []string{lines[0], "{not valid json", lines[1]}
	overwriteEventsFile(t, root, sid, lines)

	// Non-verify path: malformed line is skipped, valid events returned.
	result, err := QueryLedger(root, LedgerQuery{SessionID: sid})
	if err != nil {
		t.Fatalf("QueryLedger: %v", err)
	}
	if result.Count != 2 {
		t.Errorf("Count = %d; want 2 (malformed line skipped)", result.Count)
	}

	// Verify path: malformed line is flagged in errors and chain is invalid.
	vResult, err := QueryLedger(root, LedgerQuery{SessionID: sid, VerifyChain: true})
	if err != nil {
		t.Fatalf("QueryLedger (verify): %v", err)
	}
	if vResult.Verification.Valid {
		t.Error("Verification.Valid = true; want false (malformed line breaks chain)")
	}
	if len(vResult.Verification.Errors) == 0 {
		t.Error("expected verification.Errors to contain the unmarshal failure")
	}
	// FirstBrokenSession should be set even though seq is unknown for the bad line.
	if vResult.Verification.FirstBrokenSession != sid {
		t.Errorf("FirstBrokenSession = %q; want %q", vResult.Verification.FirstBrokenSession, sid)
	}
}

func TestQueryLedgerSessionNotFound(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	_, err := QueryLedger(root, LedgerQuery{SessionID: "does-not-exist"})
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("err = %v; want ErrSessionNotFound", err)
	}
}

func TestQueryLedgerExcludesGenesis(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Create a genesis session that should be excluded from unscoped queries.
	appendTestEvent(t, root, "genesis", "node.genesis", nil, "")
	appendTestEvent(t, root, "real-session", "e.1", nil, "")

	result, err := QueryLedger(root, LedgerQuery{})
	if err != nil {
		t.Fatalf("QueryLedger: %v", err)
	}
	for _, ev := range result.Events {
		if ev.SessionID == "genesis" {
			t.Errorf("unscoped query returned genesis event: %+v", ev)
		}
	}

	// But genesis is reachable when explicitly requested.
	scoped, err := QueryLedger(root, LedgerQuery{SessionID: "genesis"})
	if err != nil {
		t.Fatalf("QueryLedger(genesis): %v", err)
	}
	if scoped.Count == 0 {
		t.Error("explicit genesis query returned no events")
	}
}

// ── MCP tool roundtrip ─────────────────────────────────────────────────────

func TestToolReadLedgerRoundtrip(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	process := NewProcess(cfg, makeNucleus("Cog", "tester"))
	server := NewMCPServer(cfg, makeNucleus("Cog", "tester"), process)

	appendTestEvent(t, root, "sess-x", "attention.boost", map[string]interface{}{"weight": 0.5}, "")
	appendTestEvent(t, root, "sess-x", "insight.captured", nil, "")

	result, _, err := server.toolReadLedger(context.Background(), nil, readLedgerInput{
		SessionID:   "sess-x",
		VerifyChain: true,
	})
	if err != nil {
		t.Fatalf("toolReadLedger: %v", err)
	}

	var decoded LedgerQueryResult
	decodeMCPJSON(t, result, &decoded)
	if decoded.Count != 2 {
		t.Errorf("Count = %d; want 2", decoded.Count)
	}
	if !decoded.Verification.Valid {
		t.Errorf("Verification.Valid = false; errors=%v", decoded.Verification.Errors)
	}
	if decoded.Events[0].Type != "attention.boost" {
		t.Errorf("events[0].Type = %q; want attention.boost", decoded.Events[0].Type)
	}
}

// ── HTTP handler shape ─────────────────────────────────────────────────────

func TestHandleLedgerOK(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	root := srv.cfg.WorkspaceRoot

	appendTestEvent(t, root, "sess-http", "e.1", nil, "")
	appendTestEvent(t, root, "sess-http", "e.2", nil, "")

	req := httptest.NewRequest(http.MethodGet, "/v1/ledger?session_id=sess-http&limit=5", nil)
	w := httptest.NewRecorder()
	srv.handleLedger(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body=%s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q; want application/json", ct)
	}
	var body LedgerQueryResult
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Count != 2 {
		t.Errorf("Count = %d; want 2", body.Count)
	}
}

func TestHandleLedgerBadFilter(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	// after_seq without session_id.
	req := httptest.NewRequest(http.MethodGet, "/v1/ledger?after_seq=5", nil)
	w := httptest.NewRecorder()
	srv.handleLedger(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("after_seq without session_id: status = %d; want 400", w.Code)
	}

	// Unparseable after_seq.
	req = httptest.NewRequest(http.MethodGet, "/v1/ledger?session_id=s&after_seq=abc", nil)
	w = httptest.NewRecorder()
	srv.handleLedger(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad after_seq: status = %d; want 400", w.Code)
	}

	// Bad since_timestamp.
	vals := url.Values{}
	vals.Set("since_timestamp", "nope")
	req = httptest.NewRequest(http.MethodGet, "/v1/ledger?"+vals.Encode(), nil)
	w = httptest.NewRecorder()
	srv.handleLedger(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad since_timestamp: status = %d; want 400", w.Code)
	}
}

func TestHandleLedgerSessionNotFound(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/ledger?session_id=ghost", nil)
	w := httptest.NewRecorder()
	srv.handleLedger(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404 (body=%s)", w.Code, w.Body.String())
	}
}

func TestHandleLedgerBrokenChainStillReturns200(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	root := srv.cfg.WorkspaceRoot
	sid := "sess-broken"
	appendTestEvent(t, root, sid, "e.1", nil, "")
	appendTestEvent(t, root, sid, "e.2", nil, "")

	// Tamper with the second event's stored hash.
	lines := readRawEventLines(t, root, sid)
	var env EventEnvelope
	if err := json.Unmarshal([]byte(lines[1]), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env.Metadata.Hash = "dead" + env.Metadata.Hash[4:]
	tampered, _ := json.Marshal(&env)
	lines[1] = string(tampered)
	overwriteEventsFile(t, root, sid, lines)

	req := httptest.NewRequest(http.MethodGet, "/v1/ledger?session_id="+sid+"&verify_chain=true", nil)
	w := httptest.NewRecorder()
	srv.handleLedger(w, req)

	// Status must be 200: the whole point of the tamper-evident ledger is
	// surfacing the evidence, not hiding it behind an error code.
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body=%s)", w.Code, w.Body.String())
	}
	var body LedgerQueryResult
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Verification.Valid {
		t.Error("Verification.Valid = true; want false")
	}
	if body.Count != 2 {
		t.Errorf("Count = %d; want 2 (data should still flow)", body.Count)
	}
}
