package engine

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestWorkspace creates a minimal workspace skeleton (so findWorkspaceRoot
// would work if the test calls into it) and returns its root path.
func newTestWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	configDir := filepath.Join(root, ".cog", "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir .cog/config: %v", err)
	}
	return root
}

// readLedger returns every EventEnvelope in .cog/ledger/<sessionID>/events.jsonl.
// Fails the test if the file is missing or malformed.
func readLedger(t *testing.T, root, sessionID string) []*EventEnvelope {
	t.Helper()
	path := filepath.Join(root, ".cog", "ledger", sessionID, "events.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	var out []*EventEnvelope
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var env EventEnvelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("unmarshal ledger line %q: %v", line, err)
		}
		out = append(out, &env)
	}
	return out
}

// TestRunEmitCmd_EmitsToLedger verifies that the hook-style invocation
// (--json/--identity/--source) appends the event to the per-session ledger
// via AppendEvent, with hash chaining intact.
func TestRunEmitCmd_EmitsToLedger(t *testing.T) {
	t.Parallel()
	t.Cleanup(resetLedgerCacheForTest)
	root := newTestWorkspace(t)
	sessionID := "test-session-emit-ledger"

	payload := map[string]interface{}{
		"type":       "SESSION_START",
		"session_id": sessionID,
		"data": map[string]interface{}{
			"cwd":         root,
			"environment": "test",
		},
	}
	payloadJSON, _ := json.Marshal(payload)

	var stdout, stderr bytes.Buffer
	code := runEmitCmdWithIO(
		[]string{
			"--json", string(payloadJSON),
			"--identity", "system",
			"--source", "hook",
		},
		root, &stdout, &stderr,
	)

	if code != 0 {
		t.Fatalf("exit code = %d; want 0 (stderr=%q)", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout should be empty; got %q", stdout.String())
	}

	// Ledger file must exist and contain exactly one event.
	events := readLedger(t, root, sessionID)
	if len(events) != 1 {
		t.Fatalf("event count = %d; want 1", len(events))
	}
	ev := events[0]
	if ev.HashedPayload.Type != "SESSION_START" {
		t.Errorf("type = %q; want SESSION_START", ev.HashedPayload.Type)
	}
	if ev.HashedPayload.SessionID != sessionID {
		t.Errorf("session_id = %q; want %q", ev.HashedPayload.SessionID, sessionID)
	}
	if ev.Metadata.Seq != 1 {
		t.Errorf("seq = %d; want 1", ev.Metadata.Seq)
	}
	if len(ev.Metadata.Hash) != 64 {
		t.Errorf("hash len = %d; want 64 (sha256)", len(ev.Metadata.Hash))
	}
	if ev.Metadata.Source != "hook" {
		t.Errorf("metadata.source = %q; want %q", ev.Metadata.Source, "hook")
	}
	if ev.HashedPayload.Data["identity"] != "system" {
		t.Errorf("data.identity = %v; want %q", ev.HashedPayload.Data["identity"], "system")
	}
	if ev.HashedPayload.Data["cwd"] != root {
		t.Errorf("data.cwd = %v; want %q", ev.HashedPayload.Data["cwd"], root)
	}

	// A second emit chains to the first.
	payload2 := map[string]interface{}{
		"type":       "SESSION_HEARTBEAT",
		"session_id": sessionID,
		"data":       map[string]interface{}{"tick": float64(1)},
	}
	p2JSON, _ := json.Marshal(payload2)
	stderr.Reset()
	stdout.Reset()
	code = runEmitCmdWithIO(
		[]string{"--json", string(p2JSON), "--identity", "system", "--source", "hook"},
		root, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("second emit exit = %d (stderr=%q)", code, stderr.String())
	}

	events = readLedger(t, root, sessionID)
	if len(events) != 2 {
		t.Fatalf("after second emit: event count = %d; want 2", len(events))
	}
	if events[1].HashedPayload.PriorHash != events[0].Metadata.Hash {
		t.Errorf("chain broken: events[1].prior_hash=%q events[0].hash=%q",
			events[1].HashedPayload.PriorHash, events[0].Metadata.Hash)
	}
}

// TestRunEmitCmd_FlagsMatchRootShape verifies the argument schema accepted by
// runEmitCmd matches what real hook invocations pass. The live shape
// (per `ps -ef` capture cited in Agent I2's plan) is:
//
//	cogos --workspace <path> emit --json {...} --identity system --source hook
//
// After the top-level flag.Parse() strips --workspace, runEmitCmd sees
// ["--json", "{...}", "--identity", "system", "--source", "hook"].
// It must ACCEPT this exact shape and succeed.
func TestRunEmitCmd_FlagsMatchRootShape(t *testing.T) {
	t.Parallel()
	root := newTestWorkspace(t)

	cases := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{
			name: "hook SESSION_START shape (exact form from ps -ef)",
			args: []string{
				"--json", `{"type":"SESSION_START","data":{"cwd":"/tmp","workspace_root":"/tmp","environment":"local"},"session_id":"abc-123"}`,
				"--identity", "system",
				"--source", "hook",
			},
		},
		{
			name: "flags in different order",
			args: []string{
				"--source", "hook",
				"--identity", "system",
				"--json", `{"type":"TOOL_INVOKED","session_id":"s","data":{"tool":"Read"}}`,
			},
		},
		{
			name: "only --json (identity and source optional)",
			args: []string{"--json", `{"type":"PING","session_id":"s","data":{}}`},
		},
		{
			name: "handler-style: bare event name, no dry-run",
			args: []string{"cog.widget.started"},
		},
		{
			name: "handler-style: bare event name + --dry-run",
			args: []string{"cog.session.start", "--dry-run"},
		},
		{
			name:    "no args → usage error",
			args:    []string{},
			wantErr: true,
		},
		{
			name:    "--json with empty value",
			args:    []string{"--json", ""},
			wantErr: true,
		},
		{
			name:    "--json with invalid payload",
			args:    []string{"--json", "not-json", "--identity", "x", "--source", "y"},
			wantErr: true,
		},
		{
			name:    "--json with payload missing type",
			args:    []string{"--json", `{"session_id":"s","data":{}}`, "--identity", "x"},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			code := runEmitCmdWithIO(tc.args, root, &stdout, &stderr)
			if tc.wantErr {
				if code == 0 {
					t.Errorf("expected non-zero exit; got 0 (stderr=%q)", stderr.String())
				}
			} else {
				if code != 0 {
					t.Errorf("expected 0 exit; got %d (stderr=%q)", code, stderr.String())
				}
			}
		})
	}
}

// TestRunEmitCmd_OutputMatchesRootShape checks stdout/stderr byte-compatibility
// for the cases where root's cmdEmit writes identifiable text:
//
//   - Root with no args: "Usage: cog emit <event> [--dry-run]\n\nEvents: cog.session.start, cog.session.end, etc.\n" on stderr.
//   - Root with bare event name + --dry-run (no handlers dir): "[dry-run] No handlers for event: <name>\n" on stderr.
//   - Root with hook-style flags: stdout/stderr both empty on success.
func TestRunEmitCmd_OutputMatchesRootShape(t *testing.T) {
	t.Parallel()

	t.Run("no args → root usage text on stderr", func(t *testing.T) {
		t.Parallel()
		var stdout, stderr bytes.Buffer
		code := runEmitCmdWithIO(nil, "", &stdout, &stderr)
		if code != 1 {
			t.Errorf("exit = %d; want 1", code)
		}
		wantStderr := "Usage: cog emit <event> [--dry-run]\n\nEvents: cog.session.start, cog.session.end, etc.\n"
		if stderr.String() != wantStderr {
			t.Errorf("stderr mismatch\n got: %q\nwant: %q", stderr.String(), wantStderr)
		}
		if stdout.String() != "" {
			t.Errorf("stdout should be empty; got %q", stdout.String())
		}
	})

	t.Run("handler-style dry-run → matches root stderr", func(t *testing.T) {
		t.Parallel()
		root := newTestWorkspace(t)
		var stdout, stderr bytes.Buffer
		code := runEmitCmdWithIO([]string{"cog.session.start", "--dry-run"}, root, &stdout, &stderr)
		if code != 0 {
			t.Errorf("exit = %d; want 0", code)
		}
		wantStderr := "[dry-run] No handlers for event: cog.session.start\n"
		if stderr.String() != wantStderr {
			t.Errorf("stderr mismatch\n got: %q\nwant: %q", stderr.String(), wantStderr)
		}
		if stdout.String() != "" {
			t.Errorf("stdout should be empty; got %q", stdout.String())
		}
	})

	t.Run("hook-style success → stdout+stderr both empty", func(t *testing.T) {
		t.Parallel()
		root := newTestWorkspace(t)
		var stdout, stderr bytes.Buffer
		code := runEmitCmdWithIO(
			[]string{
				"--json", `{"type":"X","session_id":"s","data":{}}`,
				"--identity", "system",
				"--source", "hook",
			},
			root, &stdout, &stderr,
		)
		if code != 0 {
			t.Errorf("exit = %d; want 0 (stderr=%q)", code, stderr.String())
		}
		if stdout.String() != "" {
			t.Errorf("stdout must be silent on hook success; got %q", stdout.String())
		}
		if stderr.String() != "" {
			t.Errorf("stderr must be silent on hook success; got %q", stderr.String())
		}
	})

	t.Run("handler-style non-dry-run → silent", func(t *testing.T) {
		t.Parallel()
		root := newTestWorkspace(t)
		var stdout, stderr bytes.Buffer
		code := runEmitCmdWithIO([]string{"cog.session.start"}, root, &stdout, &stderr)
		if code != 0 {
			t.Errorf("exit = %d; want 0", code)
		}
		if stdout.String() != "" {
			t.Errorf("stdout = %q; want empty", stdout.String())
		}
		if stderr.String() != "" {
			t.Errorf("stderr = %q; want empty", stderr.String())
		}
	})
}

// TestRunEmitCmd_ExitCodes pins the exit-code contract:
//   - 0 on successful emit (hook-style and handler-style)
//   - 1 on usage errors (no args, missing required flags, bad JSON)
//   - 1 on AppendEvent failure (e.g. unwritable workspace)
func TestRunEmitCmd_ExitCodes(t *testing.T) {
	t.Parallel()

	// Success: hook-style.
	t.Run("success hook-style → 0", func(t *testing.T) {
		t.Parallel()
		root := newTestWorkspace(t)
		var out, errW bytes.Buffer
		code := runEmitCmdWithIO(
			[]string{"--json", `{"type":"X","session_id":"s","data":{}}`, "--identity", "i", "--source", "s"},
			root, &out, &errW,
		)
		if code != 0 {
			t.Errorf("exit = %d; want 0 (stderr=%q)", code, errW.String())
		}
	})

	// Success: handler-style (no handlers configured, exit 0 like root).
	t.Run("success handler-style → 0", func(t *testing.T) {
		t.Parallel()
		root := newTestWorkspace(t)
		var out, errW bytes.Buffer
		code := runEmitCmdWithIO([]string{"some.event.name"}, root, &out, &errW)
		if code != 0 {
			t.Errorf("exit = %d; want 0", code)
		}
	})

	// Failure: no args at all.
	t.Run("no args → 1", func(t *testing.T) {
		t.Parallel()
		var out, errW bytes.Buffer
		code := runEmitCmdWithIO(nil, "", &out, &errW)
		if code != 1 {
			t.Errorf("exit = %d; want 1", code)
		}
	})

	// Failure: --json with malformed JSON.
	t.Run("bad JSON → 1", func(t *testing.T) {
		t.Parallel()
		root := newTestWorkspace(t)
		var out, errW bytes.Buffer
		code := runEmitCmdWithIO(
			[]string{"--json", "{not valid json", "--identity", "s", "--source", "h"},
			root, &out, &errW,
		)
		if code != 1 {
			t.Errorf("exit = %d; want 1", code)
		}
		if !strings.Contains(errW.String(), "invalid --json payload") {
			t.Errorf("stderr should mention invalid payload; got %q", errW.String())
		}
	})

	// Failure: --json empty.
	t.Run("empty --json value → 1", func(t *testing.T) {
		t.Parallel()
		root := newTestWorkspace(t)
		var out, errW bytes.Buffer
		code := runEmitCmdWithIO(
			[]string{"--json", "", "--identity", "s"},
			root, &out, &errW,
		)
		if code != 1 {
			t.Errorf("exit = %d; want 1", code)
		}
	})

	// Failure: --json payload missing "type".
	t.Run("json missing type → 1", func(t *testing.T) {
		t.Parallel()
		root := newTestWorkspace(t)
		var out, errW bytes.Buffer
		code := runEmitCmdWithIO(
			[]string{"--json", `{"session_id":"s","data":{}}`, "--identity", "sys", "--source", "hook"},
			root, &out, &errW,
		)
		if code != 1 {
			t.Errorf("exit = %d; want 1", code)
		}
		if !strings.Contains(errW.String(), "missing required \"type\" field") {
			t.Errorf("stderr should mention missing type; got %q", errW.String())
		}
	})

	// --dry-run in hook-style is accepted and is a noop (exit 0, no ledger write).
	t.Run("hook-style --dry-run → 0 and no ledger write", func(t *testing.T) {
		t.Parallel()
		root := newTestWorkspace(t)
		sessionID := "dry-run-sess"
		var out, errW bytes.Buffer
		code := runEmitCmdWithIO(
			[]string{
				"--json", `{"type":"X","session_id":"` + sessionID + `","data":{}}`,
				"--identity", "sys", "--source", "hook", "--dry-run",
			},
			root, &out, &errW,
		)
		if code != 0 {
			t.Errorf("exit = %d; want 0 (stderr=%q)", code, errW.String())
		}
		// Ledger file must NOT exist.
		ledger := filepath.Join(root, ".cog", "ledger", sessionID, "events.jsonl")
		if _, err := os.Stat(ledger); !os.IsNotExist(err) {
			t.Errorf("ledger should not exist on --dry-run; stat err = %v", err)
		}
	})

	// Missing session_id in JSON falls back to "unknown" (does not error).
	t.Run("missing session_id falls back to unknown bucket", func(t *testing.T) {
		t.Parallel()
		root := newTestWorkspace(t)
		var out, errW bytes.Buffer
		code := runEmitCmdWithIO(
			[]string{"--json", `{"type":"X","data":{}}`, "--identity", "sys", "--source", "hook"},
			root, &out, &errW,
		)
		if code != 0 {
			t.Errorf("exit = %d; want 0 (stderr=%q)", code, errW.String())
		}
		ledger := filepath.Join(root, ".cog", "ledger", "unknown", "events.jsonl")
		if _, err := os.Stat(ledger); err != nil {
			t.Errorf("expected unknown-session ledger at %s; stat err = %v", ledger, err)
		}
	})
}
