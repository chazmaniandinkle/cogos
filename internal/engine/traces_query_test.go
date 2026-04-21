// traces_query_test.go — unit tests for QueryTraces.
//
// Test plan matches Agent Q §6.1 (12 unit tests) plus §6.2-6.3 (HTTP + MCP
// roundtrip + /v1/proprioceptive regression guard).

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── fixtures ───────────────────────────────────────────────────────────────

// writeTraceFixture writes lines to .cog/run/<name>.jsonl under root.
// Each line is serialized via json.Marshal; use writeTraceRaw for malformed input.
func writeTraceFixture(t *testing.T, root, name string, rows []map[string]any) {
	t.Helper()
	runDir := filepath.Join(root, ".cog", "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("mkdir run: %v", err)
	}
	path := filepath.Join(runDir, name)
	var buf bytes.Buffer
	for _, row := range rows {
		b, err := json.Marshal(row)
		if err != nil {
			t.Fatalf("marshal row: %v", err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeTraceRaw writes arbitrary bytes — useful for malformed-line tests.
func writeTraceRaw(t *testing.T, root, name, contents string) {
	t.Helper()
	runDir := filepath.Join(root, ".cog", "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("mkdir run: %v", err)
	}
	path := filepath.Join(runDir, name)
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// ── §6.1 unit tests ────────────────────────────────────────────────────────

// 1. Empty workspace — no files — returns count=0, file_exists=false for all sources.
func TestQueryEmptyWorkspace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	res, err := QueryTraces(root, TraceQuery{Source: SourceAll})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if res.Count != 0 {
		t.Errorf("Count = %d; want 0", res.Count)
	}
	if len(res.SourcesChecked) != len(canonicalSourceOrder) {
		t.Errorf("SourcesChecked len = %d; want %d", len(res.SourcesChecked), len(canonicalSourceOrder))
	}
	for _, st := range res.SourcesChecked {
		if st.FileExists {
			t.Errorf("source %q: file_exists = true; want false", st.Name)
		}
	}
}

// 2. Single-source read of turn_metrics — all 3 rows returned with ts + session.
func TestQuerySingleSourceTurnMetrics(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTraceFixture(t, root, "turn_metrics.jsonl", []map[string]any{
		{"session_id": "s1", "turn_index": 1, "timestamp": "2026-04-20T10:00:00Z", "query": "hello"},
		{"session_id": "s1", "turn_index": 2, "timestamp": "2026-04-20T10:01:00Z", "query": "world"},
		{"session_id": "s2", "turn_index": 1, "timestamp": "2026-04-20T10:02:00Z", "query": "second"},
	})

	res, err := QueryTraces(root, TraceQuery{Source: SourceTurnMetrics})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if res.Count != 3 {
		t.Fatalf("Count = %d; want 3", res.Count)
	}
	// desc order by default — newest first.
	if res.Results[0].SessionID != "s2" {
		t.Errorf("Results[0].SessionID = %q; want s2 (newest)", res.Results[0].SessionID)
	}
	if res.Results[0].Timestamp.IsZero() {
		t.Error("timestamp not populated")
	}
	// line preserved as raw JSON.
	var decoded map[string]any
	if err := json.Unmarshal(res.Results[0].Line, &decoded); err != nil {
		t.Fatalf("decode Line: %v", err)
	}
	if decoded["query"].(string) != "second" {
		t.Errorf("Line.query = %v; want second", decoded["query"])
	}
}

// 3. Multi-source all + merged desc ordering.
func TestQueryAllSourcesMerged(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTraceFixture(t, root, "turn_metrics.jsonl", []map[string]any{
		{"session_id": "s1", "timestamp": "2026-04-20T10:00:00Z", "query": "first"},
		{"session_id": "s1", "timestamp": "2026-04-20T10:05:00Z", "query": "third"},
	})
	writeTraceFixture(t, root, "attention.jsonl", []map[string]any{
		{"participant_id": "p1", "target_uri": "cog://x", "signal_type": "visit", "occurred_at": "2026-04-20T10:02:00Z"},
		{"participant_id": "p1", "target_uri": "cog://y", "signal_type": "read", "occurred_at": "2026-04-20T10:06:00Z"},
	})

	res, err := QueryTraces(root, TraceQuery{Source: SourceAll})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if res.Count != 4 {
		t.Fatalf("Count = %d; want 4", res.Count)
	}
	// Verify desc timestamps.
	for i := 1; i < len(res.Results); i++ {
		prev, cur := res.Results[i-1].Timestamp, res.Results[i].Timestamp
		if cur.After(prev) {
			t.Errorf("order violated at i=%d: %v is after %v", i, cur, prev)
		}
	}
	// Both source names appear.
	seen := map[string]bool{}
	for _, r := range res.Results {
		seen[r.Source] = true
	}
	if !seen["turn_metrics"] || !seen["attention"] {
		t.Errorf("merged sources = %v; want both turn_metrics and attention", seen)
	}
}

// 4. session_id filter — attention source (no session) gracefully returns 0.
func TestQueryFilterBySessionID(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTraceFixture(t, root, "turn_metrics.jsonl", []map[string]any{
		{"session_id": "s1", "timestamp": "2026-04-20T10:00:00Z"},
		{"session_id": "s2", "timestamp": "2026-04-20T10:01:00Z"},
		{"session_id": "s1", "timestamp": "2026-04-20T10:02:00Z"},
	})
	writeTraceFixture(t, root, "attention.jsonl", []map[string]any{
		{"participant_id": "p", "target_uri": "cog://x", "signal_type": "visit", "occurred_at": "2026-04-20T10:03:00Z"},
	})

	res, err := QueryTraces(root, TraceQuery{Source: SourceAll, SessionID: "s1"})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if res.Count != 2 {
		t.Fatalf("Count = %d; want 2", res.Count)
	}
	for _, r := range res.Results {
		if r.SessionID != "s1" {
			t.Errorf("SessionID = %q; want s1", r.SessionID)
		}
	}
	// attention source was scanned but contributed 0.
	var attStatus SourceStatus
	for _, st := range res.SourcesChecked {
		if st.Name == "attention" {
			attStatus = st
		}
	}
	if !attStatus.FileExists {
		t.Error("attention.file_exists = false; want true")
	}
	if attStatus.Matched != 0 {
		t.Errorf("attention.matched = %d; want 0 (no session_id field)", attStatus.Matched)
	}
}

// 5. since= as duration AND RFC3339.
func TestQueryFilterBySinceDuration(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-2 * time.Hour).Format(time.RFC3339)
	recent := now.Add(-10 * time.Minute).Format(time.RFC3339)
	writeTraceFixture(t, root, "turn_metrics.jsonl", []map[string]any{
		{"session_id": "s1", "timestamp": old},
		{"session_id": "s1", "timestamp": recent},
	})

	res, err := QueryTraces(root, TraceQuery{
		Source: SourceTurnMetrics,
		Since:  now.Add(-30 * time.Minute),
	})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if res.Count != 1 {
		t.Fatalf("Count = %d; want 1 (recent only)", res.Count)
	}

	// Same test via ParseTraceDurationOrTime resolution.
	since, err := ParseTraceDurationOrTime("30m", now)
	if err != nil {
		t.Fatalf("ParseTraceDurationOrTime: %v", err)
	}
	res2, err := QueryTraces(root, TraceQuery{Source: SourceTurnMetrics, Since: since})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if res2.Count != 1 {
		t.Fatalf("duration path Count = %d; want 1", res2.Count)
	}
}

// 6. until= filter drops newer rows.
func TestQueryFilterByUntilBound(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTraceFixture(t, root, "turn_metrics.jsonl", []map[string]any{
		{"session_id": "s1", "timestamp": "2026-04-20T10:00:00Z"},
		{"session_id": "s2", "timestamp": "2026-04-20T12:00:00Z"},
	})

	until, _ := time.Parse(time.RFC3339, "2026-04-20T11:00:00Z")
	res, err := QueryTraces(root, TraceQuery{Source: SourceTurnMetrics, Until: until})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if res.Count != 1 {
		t.Fatalf("Count = %d; want 1", res.Count)
	}
	if res.Results[0].SessionID != "s1" {
		t.Errorf("SessionID = %q; want s1", res.Results[0].SessionID)
	}
}

// 7. substring match — case-insensitive.
func TestQueryFilterBySubstring(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTraceFixture(t, root, "turn_metrics.jsonl", []map[string]any{
		{"session_id": "s1", "timestamp": "2026-04-20T10:00:00Z", "query": "QUOTA exceeded"},
		{"session_id": "s2", "timestamp": "2026-04-20T10:01:00Z", "query": "plain request"},
	})

	res, err := QueryTraces(root, TraceQuery{
		Source:    SourceTurnMetrics,
		Substring: "quota",
	})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if res.Count != 1 {
		t.Fatalf("Count = %d; want 1", res.Count)
	}
	if res.Results[0].SessionID != "s1" {
		t.Errorf("SessionID = %q; want s1", res.Results[0].SessionID)
	}
}

// 8. level filter — proprioceptive maps event->level.
func TestQueryFilterByLevelProprioceptive(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTraceFixture(t, root, "proprioceptive.jsonl", []map[string]any{
		{"timestamp": "2026-04-20T10:00:00Z", "event": "tool_call_rejected", "query": "q1"},
		{"timestamp": "2026-04-20T10:01:00Z", "event": "prediction_logged", "query": "q2"},
		{"timestamp": "2026-04-20T10:02:00Z", "event": "tool_call_rejected", "query": "q3"},
	})

	res, err := QueryTraces(root, TraceQuery{
		Source: SourceProprioceptive,
		Level:  "tool_call_rejected",
	})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if res.Count != 2 {
		t.Fatalf("Count = %d; want 2", res.Count)
	}
	for _, r := range res.Results {
		if r.Level != "tool_call_rejected" {
			t.Errorf("Level = %q; want tool_call_rejected", r.Level)
		}
	}

	// level=other excludes all.
	resOther, err := QueryTraces(root, TraceQuery{Source: SourceProprioceptive, Level: "nonexistent"})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if resOther.Count != 0 {
		t.Errorf("Count for unmatched level = %d; want 0", resOther.Count)
	}
}

// 9. truncation — 150 rows, limit=100 → 100 + truncated.
func TestQueryTruncation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	var rows []map[string]any
	for i := 0; i < 150; i++ {
		rows = append(rows, map[string]any{
			"session_id": "s1",
			"timestamp":  time.Date(2026, 4, 20, 10, 0, i, 0, time.UTC).Format(time.RFC3339),
		})
	}
	writeTraceFixture(t, root, "turn_metrics.jsonl", rows)

	res, err := QueryTraces(root, TraceQuery{Source: SourceTurnMetrics, Limit: 100})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if res.Count != 100 {
		t.Fatalf("Count = %d; want 100", res.Count)
	}
	if !res.Truncated {
		t.Error("Truncated = false; want true")
	}
}

// 10. malformed line — scanned increments, row skipped, no error.
func TestQueryMalformedLineSkipped(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTraceRaw(t, root, "turn_metrics.jsonl",
		`{"session_id":"s1","timestamp":"2026-04-20T10:00:00Z"}`+"\n"+
			`not-json-garbage`+"\n"+
			`{"session_id":"s2","timestamp":"2026-04-20T10:01:00Z"}`+"\n")

	res, err := QueryTraces(root, TraceQuery{Source: SourceTurnMetrics})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if res.Count != 2 {
		t.Fatalf("Count = %d; want 2 (malformed skipped)", res.Count)
	}
	var status SourceStatus
	for _, st := range res.SourcesChecked {
		if st.Name == "turn_metrics" {
			status = st
		}
	}
	if status.Scanned != 3 {
		t.Errorf("Scanned = %d; want 3 (including garbage)", status.Scanned)
	}
}

// 11. order=asc — chronologically increasing.
func TestQueryOrderAsc(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTraceFixture(t, root, "turn_metrics.jsonl", []map[string]any{
		{"session_id": "s1", "timestamp": "2026-04-20T12:00:00Z"},
		{"session_id": "s2", "timestamp": "2026-04-20T10:00:00Z"},
		{"session_id": "s3", "timestamp": "2026-04-20T11:00:00Z"},
	})

	res, err := QueryTraces(root, TraceQuery{Source: SourceTurnMetrics, Order: "asc"})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if res.Count != 3 {
		t.Fatalf("Count = %d; want 3", res.Count)
	}
	for i := 1; i < len(res.Results); i++ {
		if res.Results[i].Timestamp.Before(res.Results[i-1].Timestamp) {
			t.Errorf("asc order violated at i=%d", i)
		}
	}
	if res.Results[0].SessionID != "s2" {
		t.Errorf("Results[0].SessionID = %q; want s2 (oldest)", res.Results[0].SessionID)
	}
}

// 12. bad source rejected.
func TestQueryBadSourceRejected(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	_, err := QueryTraces(root, TraceQuery{Source: "bogus"})
	if err == nil {
		t.Fatal("QueryTraces with bogus source returned nil error; want validation error")
	}
	if !strings.Contains(err.Error(), "unknown source") {
		t.Errorf("error = %v; want 'unknown source'", err)
	}
}

// 13. Normalization correctness — attention (occurred_at) and turn_metrics (timestamp)
// surface with populated Timestamp fields from different JSON keys, and
// internal_requests (float unix) parses too (drift from spec §3.4).
func TestQueryNormalizationAttentionVsTurnMetrics(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTraceFixture(t, root, "attention.jsonl", []map[string]any{
		{"participant_id": "p1", "target_uri": "cog://x", "signal_type": "visit", "occurred_at": "2026-04-20T10:00:00Z"},
	})
	writeTraceFixture(t, root, "turn_metrics.jsonl", []map[string]any{
		{"session_id": "s1", "timestamp": "2026-04-20T10:01:00-04:00"},
	})
	writeTraceFixture(t, root, "internal-requests.jsonl", []map[string]any{
		// Unix float timestamp — matches live workspace drift from spec.
		{"type": "request", "timestamp": 1769281958.2577438, "request_id": "r1"},
	})

	res, err := QueryTraces(root, TraceQuery{Source: SourceAll})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if res.Count != 3 {
		t.Fatalf("Count = %d; want 3 (one per source)", res.Count)
	}
	// All timestamps populated (non-zero) — proves the normalization worked.
	for _, r := range res.Results {
		if r.Timestamp.IsZero() {
			t.Errorf("source %q: Timestamp zero; want populated", r.Source)
		}
	}
	// attention row has no session_id — should be empty string.
	for _, r := range res.Results {
		if r.Source == "attention" && r.SessionID != "" {
			t.Errorf("attention SessionID = %q; want empty", r.SessionID)
		}
		if r.Source == "turn_metrics" && r.SessionID != "s1" {
			t.Errorf("turn_metrics SessionID = %q; want s1", r.SessionID)
		}
	}
}

// 14. Substring-too-long rejected.
func TestQuerySubstringTooLong(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	long := strings.Repeat("a", maxSubstringLen+1)
	_, err := QueryTraces(root, TraceQuery{Substring: long})
	if err == nil {
		t.Fatal("expected error for substring exceeding cap")
	}
}

// ── §6.2 HTTP + MCP roundtrip ──────────────────────────────────────────────

// 15. HTTP — GET /v1/traces returns a well-shaped TraceQueryResult.
func TestHandleTracesHTTP(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	root := srv.cfg.WorkspaceRoot
	writeTraceFixture(t, root, "turn_metrics.jsonl", []map[string]any{
		{"session_id": "sA", "timestamp": "2026-04-20T10:00:00Z", "query": "alpha"},
		{"session_id": "sA", "timestamp": "2026-04-20T10:01:00Z", "query": "beta"},
	})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/traces?source=turn_metrics&limit=5")
	if err != nil {
		t.Fatalf("GET /v1/traces: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}

	var decoded TraceQueryResult
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Count != 2 {
		t.Fatalf("Count = %d; want 2", decoded.Count)
	}
	if len(decoded.SourcesChecked) != 1 {
		t.Fatalf("SourcesChecked len = %d; want 1", len(decoded.SourcesChecked))
	}
	if decoded.SourcesChecked[0].Name != "turn_metrics" {
		t.Errorf("SourcesChecked[0].Name = %q; want turn_metrics", decoded.SourcesChecked[0].Name)
	}

	// 400 on unknown source.
	resp400, err := http.Get(ts.URL + "/v1/traces?source=bogus")
	if err != nil {
		t.Fatalf("GET /v1/traces (bad source): %v", err)
	}
	defer resp400.Body.Close()
	if resp400.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", resp400.StatusCode)
	}
}

// 16. MCP — invoke toolSearchTraces directly.
func TestToolSearchTracesMCP(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	process := NewProcess(cfg, makeNucleus("Cog", "tester"))
	server := NewMCPServer(cfg, makeNucleus("Cog", "tester"), process)

	writeTraceFixture(t, root, "turn_metrics.jsonl", []map[string]any{
		{"session_id": "mcp-s1", "timestamp": "2026-04-20T09:00:00Z", "query": "first"},
		{"session_id": "mcp-s2", "timestamp": "2026-04-20T09:01:00Z", "query": "second"},
	})

	result, _, err := server.toolSearchTraces(context.Background(), nil, searchTracesInput{
		Source: "turn_metrics",
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("toolSearchTraces: %v", err)
	}
	var decoded TraceQueryResult
	decodeMCPJSON(t, result, &decoded)
	if decoded.Count != 2 {
		t.Fatalf("Count = %d; want 2", decoded.Count)
	}
	// Filter path — session_id.
	result2, _, err := server.toolSearchTraces(context.Background(), nil, searchTracesInput{
		Source:    "turn_metrics",
		SessionID: "mcp-s1",
	})
	if err != nil {
		t.Fatalf("toolSearchTraces (filter): %v", err)
	}
	var filtered TraceQueryResult
	decodeMCPJSON(t, result2, &filtered)
	if filtered.Count != 1 {
		t.Fatalf("filtered Count = %d; want 1", filtered.Count)
	}
}

// ── §6.3 regression — /v1/proprioceptive shape unchanged ───────────────────

// 17. Guards the backwards-compat commitment for dashboard.html:1265 and
// canvas.html:1706, which consume {entries, light_cone}. If this test fails
// the dashboard breaks silently.
func TestProprioceptiveEndpointUnchanged(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	root := srv.cfg.WorkspaceRoot
	writeTraceFixture(t, root, "proprioceptive.jsonl", []map[string]any{
		{"timestamp": "2026-04-20T10:00:00Z", "event": "tool_call_rejected", "query": "guarded", "predicted": []string{}, "actual": []string{}, "hits": 0, "delta": 0.0, "response_len": 0},
	})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/proprioceptive")
	if err != nil {
		t.Fatalf("GET /v1/proprioceptive: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Top-level keys: exactly "entries" and "light_cone".
	if _, ok := raw["entries"]; !ok {
		t.Error("missing top-level 'entries' key — dashboard.html:1265 WILL BREAK")
	}
	if _, ok := raw["light_cone"]; !ok {
		t.Error("missing top-level 'light_cone' key — dashboard.html:1449/1458 WILL BREAK")
	}
	// Any *extra* top-level key would also count as a reshape; flag it loudly.
	for k := range raw {
		if k != "entries" && k != "light_cone" {
			t.Errorf("unexpected top-level key %q — /v1/proprioceptive shape changed; dashboard consumers may break", k)
		}
	}

	// entries must decode as []json.RawMessage (slice of raw JSONL rows).
	var entries []json.RawMessage
	if err := json.Unmarshal(raw["entries"], &entries); err != nil {
		t.Fatalf("entries is not an array: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries len = %d; want 1", len(entries))
	}
	// Entry is the raw proprioceptive row — must still contain the 'event' field.
	var e map[string]any
	if err := json.Unmarshal(entries[0], &e); err != nil {
		t.Fatalf("entry not an object: %v", err)
	}
	if e["event"] != "tool_call_rejected" {
		t.Errorf("entry.event = %v; want tool_call_rejected", e["event"])
	}

	// light_cone must be an object with at least 'active' (dashboard reads it).
	var lc map[string]any
	if err := json.Unmarshal(raw["light_cone"], &lc); err != nil {
		t.Fatalf("light_cone not an object: %v", err)
	}
	if _, ok := lc["active"]; !ok {
		t.Error("light_cone.active key missing — dashboard check will fail")
	}
}

// ── misc helper self-test ──────────────────────────────────────────────────

// 18. ParseTraceDurationOrTime accepts RFC3339, duration, and rejects junk.
func TestParseTraceDurationOrTime(t *testing.T) {
	t.Parallel()
	now, _ := time.Parse(time.RFC3339, "2026-04-20T10:00:00Z")
	t1, err := ParseTraceDurationOrTime("30m", now)
	if err != nil {
		t.Fatalf("duration: %v", err)
	}
	if !t1.Equal(now.Add(-30 * time.Minute)) {
		t.Errorf("duration result = %v; want %v", t1, now.Add(-30*time.Minute))
	}
	t2, err := ParseTraceDurationOrTime("2026-04-20T09:00:00Z", now)
	if err != nil {
		t.Fatalf("RFC3339: %v", err)
	}
	if t2.IsZero() {
		t.Error("RFC3339 parse returned zero")
	}
	if _, err := ParseTraceDurationOrTime("garbage", now); err == nil {
		t.Error("expected error on garbage input")
	}
}
