// kernel_log_query_test.go — Tests for QueryKernelLog + the HTTP/MCP
// handlers that wrap it (Agent U's kernel-slog-api, surface half).
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedKernelLog writes a sequence of JSONL rows to
// <root>/.cog/run/kernel.log.jsonl and returns the absolute path.
// Each row is one JSON object; callers pass pre-formed JSON strings so
// the test can inject malformed lines.
func seedKernelLog(t *testing.T, root string, lines []string) string {
	t.Helper()
	path := filepath.Join(root, ".cog", "run", "kernel.log.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	return path
}

// jsonlLine formats a kernel-log row as slog.NewJSONHandler would.
func jsonlLine(ts time.Time, level, msg string, extras map[string]any) string {
	row := map[string]any{
		"time":  ts.Format(time.RFC3339Nano),
		"level": level,
		"msg":   msg,
	}
	for k, v := range extras {
		row[k] = v
	}
	b, _ := json.Marshal(row)
	return string(b)
}

func TestQueryEmptyFileReturnsNoError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, ".cog", "run", "kernel.log.jsonl")

	res, err := QueryKernelLog(path, KernelLogQuery{})
	if err != nil {
		t.Fatalf("QueryKernelLog: %v", err)
	}
	if res.File.Exists {
		t.Fatalf("file.exists = true; want false")
	}
	if res.Count != 0 {
		t.Fatalf("count = %d; want 0", res.Count)
	}
	if res.Truncated {
		t.Fatalf("truncated = true; want false")
	}
	if res.File.Path != path {
		t.Fatalf("file.path = %q; want %q", res.File.Path, path)
	}
}

func TestQueryReadsAllEntriesNewestFirst(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	lines := []string{
		jsonlLine(now.Add(-4*time.Minute), "INFO", "first", nil),
		jsonlLine(now.Add(-3*time.Minute), "INFO", "second", nil),
		jsonlLine(now.Add(-2*time.Minute), "WARN", "third", nil),
		jsonlLine(now.Add(-1*time.Minute), "ERROR", "fourth", nil),
		jsonlLine(now, "INFO", "fifth", nil),
	}
	path := seedKernelLog(t, root, lines)

	res, err := QueryKernelLog(path, KernelLogQuery{})
	if err != nil {
		t.Fatalf("QueryKernelLog: %v", err)
	}
	if res.Count != 5 {
		t.Fatalf("count = %d; want 5", res.Count)
	}
	// Newest-first: index 0 should be "fifth", index 4 should be "first".
	if res.Entries[0].Msg != "fifth" {
		t.Fatalf("entries[0].msg = %q; want fifth", res.Entries[0].Msg)
	}
	if res.Entries[4].Msg != "first" {
		t.Fatalf("entries[4].msg = %q; want first", res.Entries[4].Msg)
	}
	if res.File.SizeBytes <= 0 {
		t.Fatalf("file.size_bytes = %d; want > 0", res.File.SizeBytes)
	}
}

func TestQueryFilterByLevelExactMatchCaseInsensitive(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	now := time.Now().UTC()
	lines := []string{
		jsonlLine(now.Add(-4*time.Minute), "INFO", "a", nil),
		jsonlLine(now.Add(-3*time.Minute), "INFO", "b", nil),
		jsonlLine(now.Add(-2*time.Minute), "WARN", "c", nil),
		jsonlLine(now.Add(-1*time.Minute), "INFO", "d", nil),
		jsonlLine(now, "WARN", "e", nil),
	}
	path := seedKernelLog(t, root, lines)

	// lower-case warn
	res, err := QueryKernelLog(path, KernelLogQuery{Level: "warn"})
	if err != nil {
		t.Fatalf("QueryKernelLog: %v", err)
	}
	if res.Count != 2 {
		t.Fatalf("warn count = %d; want 2", res.Count)
	}
	// upper-case WARN should match the same.
	res2, err := QueryKernelLog(path, KernelLogQuery{Level: "WARN"})
	if err != nil {
		t.Fatalf("QueryKernelLog upper: %v", err)
	}
	if res2.Count != 2 {
		t.Fatalf("WARN count = %d; want 2", res2.Count)
	}
	// ERROR filter returns nothing (none in fixture).
	res3, err := QueryKernelLog(path, KernelLogQuery{Level: "error"})
	if err != nil {
		t.Fatalf("QueryKernelLog error: %v", err)
	}
	if res3.Count != 0 {
		t.Fatalf("error count = %d; want 0", res3.Count)
	}
}

func TestQueryFilterBySubstringCaseInsensitive(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	now := time.Now().UTC()
	lines := []string{
		jsonlLine(now.Add(-3*time.Minute), "ERROR", "daemon lifecycle failed", nil),
		jsonlLine(now.Add(-2*time.Minute), "INFO", "Daemon state written", nil),
		jsonlLine(now.Add(-1*time.Minute), "INFO", "unrelated message", nil),
	}
	path := seedKernelLog(t, root, lines)

	res, err := QueryKernelLog(path, KernelLogQuery{Substring: "daemon"})
	if err != nil {
		t.Fatalf("QueryKernelLog: %v", err)
	}
	if res.Count != 2 {
		t.Fatalf("count = %d; want 2", res.Count)
	}
	for _, e := range res.Entries {
		if !strings.Contains(strings.ToLower(e.Msg), "daemon") {
			t.Fatalf("entry.msg = %q; expected to contain 'daemon' (any case)", e.Msg)
		}
	}
}

func TestQueryFilterBySinceDuration(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	now := time.Now().UTC()
	lines := []string{
		jsonlLine(now.Add(-2*time.Hour), "INFO", "old", nil),
		jsonlLine(now.Add(-5*time.Minute), "INFO", "recent", nil),
		jsonlLine(now.Add(-30*time.Second), "INFO", "veryrecent", nil),
	}
	path := seedKernelLog(t, root, lines)

	since, err := ParseKernelLogSince("30m", now)
	if err != nil {
		t.Fatalf("ParseKernelLogSince: %v", err)
	}
	res, err := QueryKernelLog(path, KernelLogQuery{Since: since})
	if err != nil {
		t.Fatalf("QueryKernelLog: %v", err)
	}
	if res.Count != 2 {
		t.Fatalf("count = %d; want 2 (recent + veryrecent)", res.Count)
	}
	for _, e := range res.Entries {
		if e.Msg == "old" {
			t.Fatalf("old entry should have been filtered out")
		}
	}
}

func TestQueryLimitAndTruncated(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	now := time.Now().UTC()
	lines := make([]string, 0, 150)
	for i := 0; i < 150; i++ {
		lines = append(lines, jsonlLine(
			now.Add(time.Duration(i)*time.Second),
			"INFO",
			fmt.Sprintf("row-%d", i),
			nil,
		))
	}
	path := seedKernelLog(t, root, lines)

	res, err := QueryKernelLog(path, KernelLogQuery{Limit: 100})
	if err != nil {
		t.Fatalf("QueryKernelLog: %v", err)
	}
	if res.Count != 100 {
		t.Fatalf("count = %d; want 100", res.Count)
	}
	if !res.Truncated {
		t.Fatalf("truncated = false; want true (150 > 100)")
	}
	// Newest-first: the first entry should be row-149.
	if res.Entries[0].Msg != "row-149" {
		t.Fatalf("entries[0].msg = %q; want row-149", res.Entries[0].Msg)
	}
}

func TestQueryMalformedLineSkippedSilently(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	now := time.Now().UTC()
	lines := []string{
		jsonlLine(now.Add(-2*time.Minute), "INFO", "ok1", nil),
		"this is not JSON at all",
		jsonlLine(now.Add(-1*time.Minute), "INFO", "ok2", nil),
	}
	path := seedKernelLog(t, root, lines)

	res, err := QueryKernelLog(path, KernelLogQuery{})
	if err != nil {
		t.Fatalf("QueryKernelLog: %v", err)
	}
	if res.Count != 2 {
		t.Fatalf("count = %d; want 2 (malformed line skipped)", res.Count)
	}
}

func TestQueryExtractsAttrsCorrectly(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	now := time.Now().UTC()
	lines := []string{
		jsonlLine(now, "INFO", "config loaded", map[string]any{
			"workspace": "/tmp/ws",
			"port":      6931,
			"err":       "",
		}),
	}
	path := seedKernelLog(t, root, lines)

	res, err := QueryKernelLog(path, KernelLogQuery{})
	if err != nil {
		t.Fatalf("QueryKernelLog: %v", err)
	}
	if res.Count != 1 {
		t.Fatalf("count = %d; want 1", res.Count)
	}
	e := res.Entries[0]
	if e.Msg != "config loaded" {
		t.Fatalf("msg = %q; want config loaded", e.Msg)
	}
	if e.Level != "INFO" {
		t.Fatalf("level = %q; want INFO", e.Level)
	}
	if e.Attrs["workspace"] != "/tmp/ws" {
		t.Fatalf("attrs.workspace = %v; want /tmp/ws", e.Attrs["workspace"])
	}
	if port, _ := e.Attrs["port"].(float64); port != 6931 {
		t.Fatalf("attrs.port = %v; want 6931", e.Attrs["port"])
	}
	// time/level/msg must NOT appear in attrs.
	if _, ok := e.Attrs["time"]; ok {
		t.Fatalf("attrs contains time key; should be hoisted")
	}
	if _, ok := e.Attrs["level"]; ok {
		t.Fatalf("attrs contains level key; should be hoisted")
	}
	if _, ok := e.Attrs["msg"]; ok {
		t.Fatalf("attrs contains msg key; should be hoisted")
	}
	if len(e.Line) == 0 {
		t.Fatalf("line is empty; want raw JSON")
	}
}

func TestBuildKernelLogQueryValidation(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	cases := []struct {
		name        string
		limit       string
		level       string
		substring   string
		since       string
		until       string
		wantErrPart string
	}{
		{"bad_limit", "not-an-int", "", "", "", "", "invalid limit"},
		{"limit_too_large", fmt.Sprintf("%d", MaxKernelLogLimit+1), "", "", "", "", "exceeds max"},
		{"bad_level", "", "fatal", "", "", "", "invalid level"},
		{"bad_since", "", "", "", "yesterday", "", "since"},
		{"substring_too_long", "", "", strings.Repeat("x", MaxKernelLogSubstring+1), "", "", "substring length"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := BuildKernelLogQueryFromValues(tc.limit, tc.level, tc.substring, tc.since, tc.until, now)
			if err == nil {
				t.Fatalf("expected error containing %q; got nil", tc.wantErrPart)
			}
			if !strings.Contains(err.Error(), tc.wantErrPart) {
				t.Fatalf("err = %q; want contains %q", err.Error(), tc.wantErrPart)
			}
		})
	}
}

func TestHandleKernelLogHTTP(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	now := time.Now().UTC()
	lines := []string{
		jsonlLine(now.Add(-3*time.Minute), "INFO", "alpha", nil),
		jsonlLine(now.Add(-2*time.Minute), "ERROR", "beta", nil),
		jsonlLine(now.Add(-1*time.Minute), "ERROR", "gamma", nil),
	}
	seedKernelLog(t, root, lines)

	cfg := &Config{WorkspaceRoot: root}
	s := &Server{cfg: cfg}

	// Valid query.
	req := httptest.NewRequest(http.MethodGet, "/v1/kernel-log?level=error&limit=5", nil)
	rec := httptest.NewRecorder()
	s.handleKernelLog(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body KernelLogResult
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v; raw=%s", err, rec.Body.String())
	}
	if body.Count != 2 {
		t.Fatalf("count = %d; want 2", body.Count)
	}
	if body.Entries[0].Msg != "gamma" {
		t.Fatalf("entries[0].msg = %q; want gamma (newest)", body.Entries[0].Msg)
	}
	if !body.File.Exists {
		t.Fatalf("file.exists = false; want true")
	}

	// Invalid level → 400.
	req2 := httptest.NewRequest(http.MethodGet, "/v1/kernel-log?level=fatal", nil)
	rec2 := httptest.NewRecorder()
	s.handleKernelLog(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400; body=%s", rec2.Code, rec2.Body.String())
	}
}

func TestToolTailKernelLogMCP(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	process := NewProcess(cfg, makeNucleus("Cog", "tester"))
	server := NewMCPServer(cfg, makeNucleus("Cog", "tester"), process)

	now := time.Now().UTC()
	lines := []string{
		jsonlLine(now.Add(-2*time.Minute), "INFO", "ok", nil),
		jsonlLine(now.Add(-1*time.Minute), "WARN", "heads up", nil),
		jsonlLine(now, "ERROR", "kaboom", nil),
	}
	seedKernelLog(t, root, lines)

	result, _, err := server.toolTailKernelLog(context.Background(), nil, tailKernelLogInput{
		Level: "warn",
	})
	if err != nil {
		t.Fatalf("toolTailKernelLog: %v", err)
	}
	var decoded KernelLogResult
	decodeMCPJSON(t, result, &decoded)
	if decoded.Count != 1 {
		t.Fatalf("count = %d; want 1 (only WARN row)", decoded.Count)
	}
	if decoded.Entries[0].Msg != "heads up" {
		t.Fatalf("msg = %q; want heads up", decoded.Entries[0].Msg)
	}
}

func TestQueryExistsFalseWhenMissing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := &Config{WorkspaceRoot: root}
	s := &Server{cfg: cfg}

	req := httptest.NewRequest(http.MethodGet, "/v1/kernel-log", nil)
	rec := httptest.NewRecorder()
	s.handleKernelLog(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	var body KernelLogResult
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Count != 0 {
		t.Fatalf("count = %d; want 0", body.Count)
	}
	if body.File.Exists {
		t.Fatalf("file.exists = true; want false (no sink written yet)")
	}
	if body.File.Path == "" {
		t.Fatalf("file.path empty; should reflect the expected sink location")
	}
}
