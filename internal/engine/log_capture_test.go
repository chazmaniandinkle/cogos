// log_capture_test.go — Unit tests for the kernel slog tee handler and
// upgradeLoggerWithFileSink (Agent U's kernel-slog-api, capture half).
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// teeToBuffers is a test-only helper — mirrors newTeeHandler but accepts
// io.Writer-like buffers so the fan-out can be asserted without touching
// the filesystem.
func teeToBuffers(textBuf, jsonBuf *bytes.Buffer, level slog.Level) *teeHandler {
	opts := &slog.HandlerOptions{Level: level}
	return &teeHandler{
		text: slog.NewTextHandler(textBuf, opts),
		json: slog.NewJSONHandler(jsonBuf, opts),
	}
}

func TestTeeHandlerWritesToBothSinks(t *testing.T) {
	t.Parallel()
	var textBuf, jsonBuf bytes.Buffer
	h := teeToBuffers(&textBuf, &jsonBuf, slog.LevelInfo)
	logger := slog.New(h)

	logger.Info("hello", "k", "v", "port", 6931)

	textOut := textBuf.String()
	if !strings.Contains(textOut, `msg=hello`) {
		t.Fatalf("text sink missing msg=hello: %q", textOut)
	}
	if !strings.Contains(textOut, `k=v`) {
		t.Fatalf("text sink missing k=v: %q", textOut)
	}

	jsonOut := strings.TrimSpace(jsonBuf.String())
	var decoded map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &decoded); err != nil {
		t.Fatalf("json sink is not valid JSON: %v; raw=%q", err, jsonOut)
	}
	if decoded["msg"] != "hello" {
		t.Fatalf("json msg = %v; want hello", decoded["msg"])
	}
	if decoded["k"] != "v" {
		t.Fatalf("json k = %v; want v", decoded["k"])
	}
	// slog encodes port as float64 in JSON.
	if port, _ := decoded["port"].(float64); port != 6931 {
		t.Fatalf("json port = %v; want 6931", decoded["port"])
	}
	if decoded["level"] != "INFO" {
		t.Fatalf("json level = %v; want INFO", decoded["level"])
	}
}

func TestTeeHandlerWithAttrsAndWithGroupFanOut(t *testing.T) {
	t.Parallel()
	var textBuf, jsonBuf bytes.Buffer
	h := teeToBuffers(&textBuf, &jsonBuf, slog.LevelInfo)

	// WithAttrs must return a teeHandler that keeps the fan-out.
	scoped := h.WithAttrs([]slog.Attr{slog.String("session", "abc123")})
	if _, ok := scoped.(*teeHandler); !ok {
		t.Fatalf("WithAttrs returned %T; want *teeHandler", scoped)
	}
	// WithGroup must also stay a teeHandler.
	grouped := h.WithGroup("g")
	if _, ok := grouped.(*teeHandler); !ok {
		t.Fatalf("WithGroup returned %T; want *teeHandler", grouped)
	}

	scopedLogger := slog.New(scoped)
	scopedLogger.Info("scoped")

	if !strings.Contains(textBuf.String(), "session=abc123") {
		t.Fatalf("text sink missing session attr: %q", textBuf.String())
	}
	var decoded map[string]any
	_ = json.Unmarshal(bytes.TrimSpace(jsonBuf.Bytes()), &decoded)
	if decoded["session"] != "abc123" {
		t.Fatalf("json sink session = %v; want abc123", decoded["session"])
	}
}

func TestTeeHandlerEnabledIsOR(t *testing.T) {
	t.Parallel()
	// Text at Warn, JSON at Debug — teeHandler.Enabled should be Debug-or-higher.
	var tb, jb bytes.Buffer
	h := &teeHandler{
		text: slog.NewTextHandler(&tb, &slog.HandlerOptions{Level: slog.LevelWarn}),
		json: slog.NewJSONHandler(&jb, &slog.HandlerOptions{Level: slog.LevelDebug}),
	}
	ctx := context.Background()
	if !h.Enabled(ctx, slog.LevelDebug) {
		t.Fatal("Debug should be enabled (JSON side accepts)")
	}
	if !h.Enabled(ctx, slog.LevelError) {
		t.Fatal("Error should be enabled")
	}
}

func TestUpgradeLoggerWithFileSinkCreatesJSONLFile(t *testing.T) {
	root := t.TempDir()
	cfg := &Config{WorkspaceRoot: root}

	// Preserve and restore the default logger so we don't leak handler
	// state into sibling tests. t.Cleanup orders correctly.
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	upgradeLoggerWithFileSink(cfg)

	slog.Info("capture this", "field", "value")
	// Close the logger's underlying os.File by replacing default before
	// we read — the pending Write is synchronous so contents are on disk.
	slog.SetDefault(prev)

	path := filepath.Join(root, ".cog", "run", "kernel.log.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("kernel.log.jsonl not created: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("kernel.log.jsonl is empty")
	}
	// Last non-empty line should contain our message.
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	found := false
	for _, line := range lines {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("line is not JSON: %q (err=%v)", line, err)
		}
		if row["msg"] == "capture this" && row["field"] == "value" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected 'capture this' line in %s; got:\n%s", path, string(data))
	}
}

func TestUpgradeLoggerHonorsOverridePath(t *testing.T) {
	root := t.TempDir()
	overridePath := filepath.Join(root, "elsewhere", "custom.jsonl")
	cfg := &Config{
		WorkspaceRoot: root,
		KernelLogPath: overridePath,
	}

	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	upgradeLoggerWithFileSink(cfg)
	slog.Info("custom path")
	slog.SetDefault(prev)

	if _, err := os.Stat(overridePath); err != nil {
		t.Fatalf("override path %s not created: %v", overridePath, err)
	}
	// Default path must NOT be created when the override is set.
	if _, err := os.Stat(DefaultKernelLogPath(root)); !os.IsNotExist(err) {
		t.Fatalf("default path should not exist when override set; stat err = %v", err)
	}
}

func TestUpgradeLoggerStderrStillReceivesOutput(t *testing.T) {
	// Regression guard for the non-negotiable backwards-compat requirement:
	// after upgradeLoggerWithFileSink, stderr must still receive text output.
	// We redirect os.Stderr through a pipe, invoke the upgrade, log a line,
	// and assert the expected key=value appears on the captured stderr.
	root := t.TempDir()
	cfg := &Config{WorkspaceRoot: root}

	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w

	prev := slog.Default()
	t.Cleanup(func() {
		os.Stderr = origStderr
		slog.SetDefault(prev)
	})

	upgradeLoggerWithFileSink(cfg)
	slog.Info("stderr compat", "probe", "yes")
	// Flush and restore before reading.
	_ = w.Close()
	os.Stderr = origStderr
	slog.SetDefault(prev)

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read stderr pipe: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, `msg="stderr compat"`) {
		t.Fatalf("stderr missing msg: %q", got)
	}
	if !strings.Contains(got, `probe=yes`) {
		t.Fatalf("stderr missing probe=yes: %q", got)
	}
}

func TestUpgradeLoggerFailsOpenGracefully(t *testing.T) {
	// If KernelLogPath points somewhere we cannot create (e.g. under a
	// non-existent root that also isn't creatable), upgradeLoggerWithFileSink
	// must NOT panic — it should log a warning and leave the default logger
	// untouched.
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	// Point at a path whose parent dir is a file (so MkdirAll fails).
	tmp := t.TempDir()
	blockerFile := filepath.Join(tmp, "block")
	if err := os.WriteFile(blockerFile, []byte("x"), 0644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	cfg := &Config{
		WorkspaceRoot: tmp,
		KernelLogPath: filepath.Join(blockerFile, "cannot", "exist.jsonl"),
	}
	upgradeLoggerWithFileSink(cfg) // must not panic
}
