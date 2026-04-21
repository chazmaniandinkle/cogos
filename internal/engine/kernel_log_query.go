// kernel_log_query.go — Read-side of the kernel slog API.
//
// Implements Part (b) of Agent U's kernel-slog-api design — the surface half.
// Exposes QueryKernelLog, which reads the JSONL sink produced by the
// teeHandler (see log_capture.go) and returns the most recent entries in
// newest-first order, optionally filtered by level, substring, and time range.
//
// Filter shape mirrors Agent L's QueryLedger and Agent Q's QueryTraces (from
// their respective in-flight PRs) so the three observability surfaces — ledger,
// traces, kernel slog — present a consistent query grammar to Claude:
//
//   - ledger:  hash-chained events (Agent L)
//   - traces:  client-originated metabolites (Agent Q)
//   - slog:    operator/debug diagnostic text (this file)
//
// The on-disk format is one slog.NewJSONHandler record per line. Top-level
// fields "time", "level", "msg" are extracted into typed fields; anything
// else becomes an entry in the Attrs map. Malformed lines are skipped
// silently (matches the readLastJSONLEntries tolerance used by the
// proprioceptive endpoint) — corrupted lines are a data-quality signal,
// not an API error.
//
// Scan strategy: forward-scan into a bounded ring with a 1 MiB bufio buffer
// (same primitive as serve.go:readLastJSONLEntries). For v1 file sizes
// (sub-GB for years of uptime on active workspaces) this is fast enough;
// a reverse-scan optimization can land in v1.5 alongside rotation.
package engine

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultKernelLogLimit is the default tail size when no limit is given.
	DefaultKernelLogLimit = 100

	// MaxKernelLogLimit is the upper bound on a single query. Above this
	// callers should paginate via tighter since/until bounds.
	MaxKernelLogLimit = 1000

	// MaxKernelLogSubstring is the hard cap on substring filter length.
	// Anything longer is rejected as a 400 (substring is intended for short
	// operator queries, not full-text search).
	MaxKernelLogSubstring = 1024
)

// KernelLogQuery is the filter/pagination shape accepted by QueryKernelLog.
// Zero values mean "no filter" for every field.
type KernelLogQuery struct {
	// Limit bounds the returned entries. 0 → DefaultKernelLogLimit;
	// > MaxKernelLogLimit is clamped.
	Limit int

	// Level filters by exact (case-insensitive) level match. One of
	// "", "debug", "info", "warn", "error". v1 does NOT support
	// ">=warn"-style comparator filtering (see Agent U §4.4).
	Level string

	// Substring is a case-insensitive byte-scan filter applied to the
	// raw JSON line before JSON parse (cheap pre-filter). Max
	// MaxKernelLogSubstring characters.
	Substring string

	// Since, when non-zero, excludes entries earlier than this time.
	Since time.Time

	// Until, when non-zero, excludes entries later than this time.
	Until time.Time
}

// KernelLogEntry is one parsed row returned to callers. Time/Level/Msg are
// extracted from the top-level slog fields; Attrs holds everything else
// (including nested groups) as parsed JSON. Line is the raw source line for
// callers that want bit-exact fidelity.
type KernelLogEntry struct {
	Time  time.Time       `json:"time"`
	Level string          `json:"level"`
	Msg   string          `json:"msg"`
	Attrs map[string]any  `json:"attrs,omitempty"`
	Line  json.RawMessage `json:"line"`
}

// KernelLogResult is the typed result returned by QueryKernelLog and emitted
// verbatim by the /v1/kernel-log handler and the cog_tail_kernel_log MCP tool.
type KernelLogResult struct {
	Count     int              `json:"count"`
	Entries   []KernelLogEntry `json:"entries"`
	Truncated bool             `json:"truncated"`
	File      KernelLogFile    `json:"file"`
}

// KernelLogFile reports the state of the on-disk sink at query time. Returned
// even when no entries match so callers can distinguish "no sink wired" from
// "sink wired but nothing matched".
type KernelLogFile struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	Exists    bool   `json:"exists"`
}

// QueryKernelLog reads the JSONL kernel-log sink at path and returns the most
// recent entries that satisfy q's filters, in newest-first order. Missing
// files yield an empty result with Exists=false (no error). Malformed JSONL
// lines are skipped silently.
//
// Truncated is true when more than Limit entries matched the filters; the
// returned slice is the newest Limit.
func QueryKernelLog(path string, q KernelLogQuery) (*KernelLogResult, error) {
	res := &KernelLogResult{
		Entries: []KernelLogEntry{},
		File:    KernelLogFile{Path: path},
	}

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return res, nil
		}
		return nil, fmt.Errorf("stat kernel log: %w", err)
	}
	res.File.Exists = true
	res.File.SizeBytes = info.Size()

	limit := q.Limit
	if limit <= 0 {
		limit = DefaultKernelLogLimit
	}
	if limit > MaxKernelLogLimit {
		limit = MaxKernelLogLimit
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open kernel log: %w", err)
	}
	defer f.Close()

	substringLower := strings.ToLower(q.Substring)
	wantLevel := strings.ToUpper(strings.TrimSpace(q.Level))

	// Forward-scan into a bounded ring keeping the newest `limit` matches.
	ring := make([]KernelLogEntry, 0, limit)
	totalMatches := 0

	scanner := bufio.NewScanner(f)
	// 1 MiB per line (matches serve.go:readLastJSONLEntries).
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// Cheap byte-scan pre-filter before JSON parse.
		if substringLower != "" && !bytes.Contains(bytes.ToLower(line), []byte(substringLower)) {
			continue
		}

		entry, ok := extractKernelLogEntry(line)
		if !ok {
			continue
		}

		if wantLevel != "" && !strings.EqualFold(entry.Level, wantLevel) {
			continue
		}
		if !q.Since.IsZero() && entry.Time.Before(q.Since) {
			continue
		}
		if !q.Until.IsZero() && entry.Time.After(q.Until) {
			continue
		}

		totalMatches++
		if len(ring) < limit {
			ring = append(ring, entry)
		} else {
			// FIFO: drop oldest, keep newest `limit`.
			copy(ring, ring[1:])
			ring[limit-1] = entry
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan kernel log: %w", err)
	}

	// Deliver in newest-first order: file is ascending time, ring holds
	// newest last, so reverse in place.
	for i, j := 0, len(ring)-1; i < j; i, j = i+1, j-1 {
		ring[i], ring[j] = ring[j], ring[i]
	}

	res.Entries = ring
	res.Count = len(ring)
	res.Truncated = totalMatches > limit
	return res, nil
}

// extractKernelLogEntry parses a single JSONL line as emitted by
// slog.NewJSONHandler. Returns (entry, true) on success; (zero, false) if
// the line is not valid JSON.
func extractKernelLogEntry(line []byte) (KernelLogEntry, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return KernelLogEntry{}, false
	}
	// Copy line into owned memory so later scanner refills don't mutate it.
	owned := make([]byte, len(line))
	copy(owned, line)

	e := KernelLogEntry{
		Line:  json.RawMessage(owned),
		Attrs: map[string]any{},
	}

	if ts, ok := raw["time"]; ok {
		var s string
		if err := json.Unmarshal(ts, &s); err == nil {
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				e.Time = t
			}
		}
	}
	if lvl, ok := raw["level"]; ok {
		_ = json.Unmarshal(lvl, &e.Level)
	}
	if msg, ok := raw["msg"]; ok {
		_ = json.Unmarshal(msg, &e.Msg)
	}
	for k, v := range raw {
		if k == "time" || k == "level" || k == "msg" {
			continue
		}
		var decoded any
		if err := json.Unmarshal(v, &decoded); err == nil {
			e.Attrs[k] = decoded
		}
	}
	if len(e.Attrs) == 0 {
		e.Attrs = nil
	}
	return e, true
}

// ParseKernelLogSince parses a "since" / "until" query string value. Accepts
// RFC3339 timestamps AND relative durations (e.g. "5m", "2h", "24h") — the
// latter is resolved against `now`.
func ParseKernelLogSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid time value %q (expect RFC3339 or duration like '5m')", s)
}

// ParseKernelLogUntil is identical to ParseKernelLogSince except that bare
// durations are treated as absolute (now - d). The semantic is the same —
// "entries older than this" on Since, "entries newer than this" on Until —
// so we reuse the parser.
func ParseKernelLogUntil(s string, now time.Time) (time.Time, error) {
	return ParseKernelLogSince(s, now)
}

// ValidateKernelLogLevel normalises and validates a level filter string.
// Empty input returns ("", nil); unknown values return an error suitable for
// a 400 response body.
func ValidateKernelLogLevel(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	switch strings.ToLower(s) {
	case "debug", "info", "warn", "error":
		return strings.ToUpper(s), nil
	default:
		return "", fmt.Errorf("invalid level %q (expect debug|info|warn|error)", s)
	}
}

// BuildKernelLogQueryFromValues parses the query-string form shared by the
// HTTP handler and the MCP tool. All parameters are optional; returns a
// zero-valued KernelLogQuery when none are present.
//
//	limit      int        1..MaxKernelLogLimit
//	level      string     debug|info|warn|error (case-insensitive)
//	substring  string     <= MaxKernelLogSubstring
//	since      string     RFC3339 OR duration (5m, 2h, …)
//	until      string     RFC3339 OR duration
func BuildKernelLogQueryFromValues(
	limit, level, substring, since, until string,
	now time.Time,
) (KernelLogQuery, error) {
	q := KernelLogQuery{}

	if v := strings.TrimSpace(limit); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return q, fmt.Errorf("invalid limit %q: %w", v, err)
		}
		if n < 0 {
			return q, fmt.Errorf("invalid limit %d (must be >= 0)", n)
		}
		if n > MaxKernelLogLimit {
			return q, fmt.Errorf("limit %d exceeds max %d", n, MaxKernelLogLimit)
		}
		q.Limit = n
	}

	normalized, err := ValidateKernelLogLevel(level)
	if err != nil {
		return q, err
	}
	q.Level = normalized

	if len(substring) > MaxKernelLogSubstring {
		return q, fmt.Errorf("substring length %d exceeds max %d", len(substring), MaxKernelLogSubstring)
	}
	q.Substring = substring

	if v := strings.TrimSpace(since); v != "" {
		t, err := ParseKernelLogSince(v, now)
		if err != nil {
			return q, fmt.Errorf("since: %w", err)
		}
		q.Since = t
	}
	if v := strings.TrimSpace(until); v != "" {
		t, err := ParseKernelLogUntil(v, now)
		if err != nil {
			return q, fmt.Errorf("until: %w", err)
		}
		q.Until = t
	}

	return q, nil
}

// handleKernelLog serves GET /v1/kernel-log. Purely additive — does not
// reshape /v1/proprioceptive (byte-locked) or /v1/traces.
func (s *Server) handleKernelLog(w http.ResponseWriter, r *http.Request) {
	q, err := BuildKernelLogQueryFromValues(
		r.URL.Query().Get("limit"),
		r.URL.Query().Get("level"),
		r.URL.Query().Get("substring"),
		r.URL.Query().Get("since"),
		r.URL.Query().Get("until"),
		time.Now(),
	)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	path := kernelLogPathFor(s.cfg)
	result, err := QueryKernelLog(path, q)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// kernelLogPathFor returns the effective kernel-log path for a Config —
// the override if set, else the per-workspace default.
func kernelLogPathFor(cfg *Config) string {
	if cfg == nil {
		return ""
	}
	if cfg.KernelLogPath != "" {
		return cfg.KernelLogPath
	}
	return DefaultKernelLogPath(cfg.WorkspaceRoot)
}
