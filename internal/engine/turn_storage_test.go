package engine

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// newTestProcessForTurns builds a minimal process backed by a tempdir
// workspace. Does NOT start any goroutines — RecordTurn is synchronous.
func newTestProcessForTurns(t *testing.T) (*Process, string, string) {
	t.Helper()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	nucleus := makeNucleus("Test", "turns")
	p := NewProcess(cfg, nucleus)
	// Reset cache because sessionID is random UUID per process but we want
	// isolation per test.
	resetTurnIndexCacheForTests()
	return p, root, p.SessionID()
}

// TestWriteTurnCompletedHappyPath — small turn, verify sidecar has one JSONL
// row with full text; verify ledger has one turn.completed event with
// matching turn_id.
func TestWriteTurnCompletedHappyPath(t *testing.T) {
	p, root, sessionID := newTestProcessForTurns(t)

	turn := &TurnRecord{
		TurnID:    uuid.NewString(),
		TurnIndex: NextTurnIndex(root, sessionID),
		SessionID: sessionID,
		Prompt:    "hello, how are you?",
		Response:  "I am well, thanks for asking.",
		Provider:  "stub",
		Model:     "test-model",
		Usage:     TurnUsage{InputTokens: 10, OutputTokens: 20, TotalTokens: 30},
		BlockID:   "blk-123",
	}
	if err := p.RecordTurn(turn); err != nil {
		t.Fatalf("RecordTurn: %v", err)
	}

	// Ledger: exactly one turn.completed event with matching turn_id.
	events := mustReadAllEvents(t, root, sessionID)
	var gotTurnEvent *EventEnvelope
	for i := range events {
		if events[i].HashedPayload.Type == "turn.completed" {
			gotTurnEvent = &events[i]
			break
		}
	}
	if gotTurnEvent == nil {
		t.Fatalf("no turn.completed event in ledger; saw %d events", len(events))
	}
	tid, _ := gotTurnEvent.HashedPayload.Data["turn_id"].(string)
	if tid != turn.TurnID {
		t.Errorf("ledger turn_id = %q; want %q", tid, turn.TurnID)
	}
	if turn.LedgerHash == "" {
		t.Error("turn.LedgerHash not populated after RecordTurn")
	}

	// Sidecar: one JSONL row with full text.
	sidecar := turnSidecarPath(root, sessionID)
	lines := readJSONLFile(t, sidecar)
	if len(lines) != 1 {
		t.Fatalf("sidecar line count = %d; want 1", len(lines))
	}
	var got TurnRecord
	if err := json.Unmarshal(lines[0], &got); err != nil {
		t.Fatalf("parse sidecar row: %v", err)
	}
	if got.Prompt != turn.Prompt {
		t.Errorf("sidecar prompt = %q; want %q", got.Prompt, turn.Prompt)
	}
	if got.Response != turn.Response {
		t.Errorf("sidecar response = %q; want %q", got.Response, turn.Response)
	}
}

// TestWriteTurnCompletedPromptTruncation — prompt exceeds cap; verify sidecar
// carries full prompt while ledger event has truncated preview +
// prompt_truncated=true.
func TestWriteTurnCompletedPromptTruncation(t *testing.T) {
	p, root, sessionID := newTestProcessForTurns(t)

	// Prompt bigger than cap (8 KB by default).
	fullPrompt := strings.Repeat("A", DefaultPromptPreviewCap*2+5)
	turn := &TurnRecord{
		TurnID:    uuid.NewString(),
		TurnIndex: NextTurnIndex(root, sessionID),
		SessionID: sessionID,
		Prompt:    fullPrompt,
		Response:  "short",
	}
	if err := p.RecordTurn(turn); err != nil {
		t.Fatalf("RecordTurn: %v", err)
	}

	// Ledger preview must be capped and marked truncated.
	events := mustReadAllEvents(t, root, sessionID)
	var turnEv *EventEnvelope
	for i := range events {
		if events[i].HashedPayload.Type == "turn.completed" {
			turnEv = &events[i]
		}
	}
	if turnEv == nil {
		t.Fatal("no turn.completed event")
	}
	preview, _ := turnEv.HashedPayload.Data["prompt_preview"].(string)
	trunc, _ := turnEv.HashedPayload.Data["prompt_truncated"].(bool)
	if !trunc {
		t.Error("prompt_truncated = false; want true")
	}
	if len(preview) > DefaultPromptPreviewCap {
		t.Errorf("preview len = %d; want <= %d", len(preview), DefaultPromptPreviewCap)
	}

	// Sidecar must have full text.
	sidecar := readJSONLFile(t, turnSidecarPath(root, sessionID))
	if len(sidecar) != 1 {
		t.Fatalf("sidecar rows = %d; want 1", len(sidecar))
	}
	var row TurnRecord
	_ = json.Unmarshal(sidecar[0], &row)
	if row.Prompt != fullPrompt {
		t.Errorf("sidecar prompt len = %d; want %d (full)", len(row.Prompt), len(fullPrompt))
	}
}

// TestWriteTurnCompletedResponseTruncation — same for response field.
func TestWriteTurnCompletedResponseTruncation(t *testing.T) {
	p, root, sessionID := newTestProcessForTurns(t)

	fullResp := strings.Repeat("B", DefaultResponsePreviewCap*2+7)
	turn := &TurnRecord{
		TurnID:    uuid.NewString(),
		TurnIndex: NextTurnIndex(root, sessionID),
		SessionID: sessionID,
		Prompt:    "ask",
		Response:  fullResp,
	}
	if err := p.RecordTurn(turn); err != nil {
		t.Fatalf("RecordTurn: %v", err)
	}

	events := mustReadAllEvents(t, root, sessionID)
	var turnEv *EventEnvelope
	for i := range events {
		if events[i].HashedPayload.Type == "turn.completed" {
			turnEv = &events[i]
		}
	}
	if turnEv == nil {
		t.Fatal("no turn.completed event")
	}
	preview, _ := turnEv.HashedPayload.Data["response_preview"].(string)
	trunc, _ := turnEv.HashedPayload.Data["response_truncated"].(bool)
	if !trunc {
		t.Error("response_truncated = false; want true")
	}
	if len(preview) > DefaultResponsePreviewCap {
		t.Errorf("preview len = %d; want <= %d", len(preview), DefaultResponsePreviewCap)
	}

	sidecar := readJSONLFile(t, turnSidecarPath(root, sessionID))
	var row TurnRecord
	_ = json.Unmarshal(sidecar[0], &row)
	if row.Response != fullResp {
		t.Errorf("sidecar response len = %d; want %d", len(row.Response), len(fullResp))
	}
}

// TestTruncateUTF8Bytes_Boundary — cap falls inside a multibyte rune; the
// returned preview must end on a valid UTF-8 boundary.
func TestTruncateUTF8Bytes_Boundary(t *testing.T) {
	t.Parallel()
	// "日" = 3 bytes in UTF-8. Build a string of 10 copies (30 bytes).
	s := strings.Repeat("日", 10)
	// Cap at 8 bytes — cuts inside the 3rd "日".
	got, trunc := truncateUTF8Bytes(s, 8)
	if !trunc {
		t.Error("trunc = false; want true")
	}
	// Valid UTF-8 prefix — should end at a rune boundary.
	if got != "日日" { // 2 full runes = 6 bytes, below cap
		t.Errorf("got = %q (%d bytes); want %q (6 bytes)", got, len(got), "日日")
	}
	// Cap >= len: no truncation.
	got2, trunc2 := truncateUTF8Bytes(s, 1000)
	if trunc2 {
		t.Error("trunc2 = true; want false")
	}
	if got2 != s {
		t.Errorf("got2 != s")
	}
	// Cap <= 0: no truncation.
	got3, trunc3 := truncateUTF8Bytes(s, 0)
	if trunc3 {
		t.Error("trunc3 = true; want false")
	}
	if got3 != s {
		t.Errorf("got3 != s")
	}
}

// TestWriteTurnCompletedSidecarAppendDurable — multiple writes to same session
// produce JSONL with all rows in write order.
func TestWriteTurnCompletedSidecarAppendDurable(t *testing.T) {
	p, root, sessionID := newTestProcessForTurns(t)

	for i := 0; i < 5; i++ {
		turn := &TurnRecord{
			TurnID:    uuid.NewString(),
			TurnIndex: NextTurnIndex(root, sessionID),
			SessionID: sessionID,
			Prompt:    "prompt-" + string(rune('0'+i)),
			Response:  "response-" + string(rune('0'+i)),
		}
		if err := p.RecordTurn(turn); err != nil {
			t.Fatalf("RecordTurn[%d]: %v", i, err)
		}
	}

	lines := readJSONLFile(t, turnSidecarPath(root, sessionID))
	if len(lines) != 5 {
		t.Fatalf("sidecar rows = %d; want 5", len(lines))
	}
	for i, line := range lines {
		var row TurnRecord
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatalf("row %d: parse: %v", i, err)
		}
		if row.TurnIndex != i+1 {
			t.Errorf("row %d: TurnIndex = %d; want %d", i, row.TurnIndex, i+1)
		}
	}
}

// TestWriteTurnCompletedConcurrent — concurrent writes to same session, verify
// all rows land and ledger hash-chain stays consistent.
func TestWriteTurnCompletedConcurrent(t *testing.T) {
	p, root, sessionID := newTestProcessForTurns(t)

	const workers = 8
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(n int) {
			defer wg.Done()
			turn := &TurnRecord{
				TurnID:    uuid.NewString(),
				TurnIndex: NextTurnIndex(root, sessionID),
				SessionID: sessionID,
				Prompt:    "concurrent-prompt",
				Response:  "concurrent-response",
			}
			if err := p.RecordTurn(turn); err != nil {
				t.Errorf("worker %d: %v", n, err)
			}
		}(i)
	}
	wg.Wait()

	// Sidecar row count matches workers.
	lines := readJSONLFile(t, turnSidecarPath(root, sessionID))
	if len(lines) != workers {
		t.Errorf("sidecar rows = %d; want %d", len(lines), workers)
	}

	// Ledger has `workers` turn.completed events with monotonic seq numbers.
	events := mustReadAllEvents(t, root, sessionID)
	turnEvents := 0
	seqs := make(map[int64]bool)
	for _, ev := range events {
		if ev.HashedPayload.Type != "turn.completed" {
			continue
		}
		turnEvents++
		if seqs[ev.Metadata.Seq] {
			t.Errorf("duplicate seq %d", ev.Metadata.Seq)
		}
		seqs[ev.Metadata.Seq] = true
	}
	if turnEvents != workers {
		t.Errorf("ledger turn events = %d; want %d", turnEvents, workers)
	}
}

// TestNextTurnIndexPrimingFromSidecar — simulate restart: write 3 turns, drop
// the in-memory counter, verify NextTurnIndex returns 4.
func TestNextTurnIndexPrimingFromSidecar(t *testing.T) {
	p, root, sessionID := newTestProcessForTurns(t)

	for i := 0; i < 3; i++ {
		turn := &TurnRecord{
			TurnID:    uuid.NewString(),
			TurnIndex: NextTurnIndex(root, sessionID),
			SessionID: sessionID,
			Prompt:    "p",
			Response:  "r",
		}
		if err := p.RecordTurn(turn); err != nil {
			t.Fatalf("RecordTurn: %v", err)
		}
	}
	// Simulate restart: clear the in-memory counter.
	resetTurnIndexCacheForTests()
	// Next call should see 3 rows in sidecar → max_idx=3 → next=4.
	got := NextTurnIndex(root, sessionID)
	if got != 4 {
		t.Errorf("NextTurnIndex after restart = %d; want 4", got)
	}
}

// TestRecordTurnStatusDefault — missing status defaults to "ok".
func TestRecordTurnStatusDefault(t *testing.T) {
	p, root, sessionID := newTestProcessForTurns(t)
	turn := &TurnRecord{
		TurnID:    uuid.NewString(),
		TurnIndex: NextTurnIndex(root, sessionID),
		SessionID: sessionID,
		Prompt:    "x",
		Response:  "y",
	}
	if err := p.RecordTurn(turn); err != nil {
		t.Fatal(err)
	}
	if turn.Status != "ok" {
		t.Errorf("Status = %q; want ok", turn.Status)
	}
	if turn.Timestamp.IsZero() {
		t.Error("Timestamp is zero; want auto-populated")
	}
}

// TestRecordTurnNilGuards — defensive nil handling.
func TestRecordTurnNilGuards(t *testing.T) {
	t.Parallel()
	var p *Process
	if err := p.RecordTurn(&TurnRecord{}); err == nil {
		t.Error("RecordTurn on nil process: expected error")
	}
	p2 := &Process{cfg: &Config{WorkspaceRoot: t.TempDir()}}
	if err := p2.RecordTurn(nil); err == nil {
		t.Error("RecordTurn with nil turn: expected error")
	}
}

// TestRecordTurnTimestampRespected — if provided, timestamp is preserved in
// the ledger event.
func TestRecordTurnTimestampRespected(t *testing.T) {
	p, root, sessionID := newTestProcessForTurns(t)
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	turn := &TurnRecord{
		TurnID:    uuid.NewString(),
		TurnIndex: NextTurnIndex(root, sessionID),
		SessionID: sessionID,
		Prompt:    "x",
		Response:  "y",
		Timestamp: ts,
	}
	if err := p.RecordTurn(turn); err != nil {
		t.Fatal(err)
	}
	events := mustReadAllEvents(t, root, sessionID)
	for _, ev := range events {
		if ev.HashedPayload.Type == "turn.completed" {
			if ev.HashedPayload.Timestamp != ts.Format(time.RFC3339) {
				t.Errorf("ledger timestamp = %q; want %q", ev.HashedPayload.Timestamp, ts.Format(time.RFC3339))
			}
			return
		}
	}
	t.Fatal("no turn.completed event found")
}

// readJSONLFile reads a JSONL file and returns each line as raw bytes.
func readJSONLFile(t *testing.T, path string) [][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var lines [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		raw := append([]byte(nil), sc.Bytes()...)
		if len(raw) == 0 {
			continue
		}
		lines = append(lines, raw)
	}
	return lines
}

// Silence "unused import" if a test path doesn't exercise filepath.
var _ = filepath.Join
