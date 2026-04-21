// cli_emit.go — `cogos emit` subcommand: ledger-append for hook-fired events.
//
// Supports two invocation shapes, both historically dispatched by the root
// package's `case "emit":` (cog.go:5625 → cmdEmit at cog.go:2621).
//
// Hook-style (the live form, used by .cog/hooks/* via lib/cog_emit.py):
//
//	cogos emit --json '{"type":"SESSION_START","session_id":"…","data":{…}}' \
//	           --identity system --source hook
//
// Handler-style (historical, used by SDK event client and manual invocations):
//
//	cogos emit <event-name> [--dry-run]
//
// Phase 1 of Track 5 (per Agent I2's revised dead-code plan):
// this engine implementation exists alongside the root `cmdEmit`. The root
// binary still dispatches to root's path today; once Phase 4 flips the
// Makefile default build target to cmd/cogos/, the engine path takes over
// for the installed `cogos` binary. Hook invocations must continue to
// succeed at every step of the cutover.
//
// Engine-side behavior vs root:
//   - Hook-style (--json path): engine ACTUALLY writes the event to the
//     per-session ledger via AppendEvent (hash-chained). Root silently
//     accepted these flags but treated "--json" as the event name and
//     fired no handlers — so on-disk effect was zero. This is a strict
//     improvement: hooks always returned 0 under root, and they continue
//     to return 0 here, but now the event actually lands in the ledger.
//   - Handler-style (<event> [--dry-run] path): engine emits a noop for
//     now (no handler index port in Phase 1; events dir is empty in live
//     workspaces). Root's path is retained until Phase 5 sweep.
package engine

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// runEmitCmd dispatches `cogos emit` arguments to the appropriate path and
// returns an exit code (0 = success). Stdout/stderr are written via the
// package-level os.Stdout/os.Stderr; the *out/*errW parameters allow tests
// to capture output. Passing nil uses os.Stdout/os.Stderr.
//
// This function does NOT call os.Exit; the caller in Main() does.
func runEmitCmd(args []string, defaultWorkspace string) int {
	return runEmitCmdWithIO(args, defaultWorkspace, os.Stdout, os.Stderr)
}

// runEmitCmdWithIO is the testable core. stdout/stderr default to os.Stdout/os.Stderr
// when nil is passed.
func runEmitCmdWithIO(args []string, defaultWorkspace string, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}

	// Detect invocation shape.
	//
	// Hook-style is recognized by the presence of "--json" anywhere in args.
	// Handler-style is recognized by a bare positional first arg.
	// Empty args mirror root's usage message and exit 1.
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: cog emit <event> [--dry-run]")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Events: cog.session.start, cog.session.end, etc.")
		return 1
	}

	// Hook-style path: --json must appear as a flag.
	if hasJSONFlag(args) {
		return runEmitHookStyle(args, defaultWorkspace, stdout, stderr)
	}

	// Handler-style path: <event> [--dry-run].
	return runEmitHandlerStyle(args, defaultWorkspace, stdout, stderr)
}

// hasJSONFlag reports whether args contains the "--json" flag.
// It matches both "--json" (space-separated value) and "--json=…" shapes.
func hasJSONFlag(args []string) bool {
	for _, a := range args {
		if a == "--json" || strings.HasPrefix(a, "--json=") {
			return true
		}
	}
	return false
}

// runEmitHookStyle parses the hook-style flag set and appends the event to
// the per-session ledger via AppendEvent. Flags:
//
//	--json <payload>   (required) JSON envelope: {type, session_id, data}
//	--identity <id>    (optional) who is emitting (e.g. "system", "hook")
//	--source <src>     (optional) source tag (e.g. "hook")
//
// On success: stdout empty, stderr empty, exit 0. On malformed flags or bad
// JSON: a short error on stderr + exit 1.
func runEmitHookStyle(args []string, defaultWorkspace string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("emit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonPayload := fs.String("json", "", "JSON event payload")
	identity := fs.String("identity", "", "Identity emitting the event")
	source := fs.String("source", "", "Source tag for the event")
	// --dry-run is accepted but ignored in hook-style; it's a noop since
	// AppendEvent is always effectful. Preserves compat with callers that
	// blindly append --dry-run.
	dryRun := fs.Bool("dry-run", false, "Preview without emitting (noop in hook-style)")

	if err := fs.Parse(args); err != nil {
		return 1
	}

	if *jsonPayload == "" {
		fmt.Fprintln(stderr, "error: --json payload is required")
		return 1
	}

	// Parse the envelope. Required: "type". Optional: "session_id", "data".
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(*jsonPayload), &payload); err != nil {
		fmt.Fprintf(stderr, "error: invalid --json payload: %v\n", err)
		return 1
	}

	eventType, _ := payload["type"].(string)
	if eventType == "" {
		fmt.Fprintln(stderr, "error: --json payload missing required \"type\" field")
		return 1
	}

	sessionID, _ := payload["session_id"].(string)
	if sessionID == "" {
		// Mirror cog_emit.py which omits session_id when unknown. Fall back
		// to a deterministic "unknown" bucket so the ledger still captures
		// the event rather than silently dropping it.
		sessionID = "unknown"
	}

	data, _ := payload["data"].(map[string]interface{})

	// Resolve workspace.
	workspace := defaultWorkspace
	if workspace == "" {
		wd, _ := os.Getwd()
		ws, err := findWorkspaceRoot(wd)
		if err != nil {
			fmt.Fprintf(stderr, "error: could not detect workspace: %v\n", err)
			return 1
		}
		workspace = ws
	}

	if *dryRun {
		// No-op: don't actually append.
		return 0
	}

	// Build the envelope and append.
	metaSource := *source
	if metaSource == "" {
		metaSource = "emit-cli"
	}
	if *identity != "" {
		// Fold identity into Data under "identity" so downstream consumers can
		// see it. Do NOT shadow any existing "identity" key the caller set.
		if data == nil {
			data = make(map[string]interface{})
		}
		if _, ok := data["identity"]; !ok {
			data["identity"] = *identity
		}
	}

	env := &EventEnvelope{
		HashedPayload: EventPayload{
			Type:      eventType,
			Timestamp: nowISO(),
			SessionID: sessionID,
			Data:      data,
		},
		Metadata: EventMetadata{Source: metaSource},
	}
	if err := AppendEvent(workspace, sessionID, env); err != nil {
		fmt.Fprintf(stderr, "error: append event: %v\n", err)
		return 1
	}

	return 0
}

// runEmitHandlerStyle mirrors root's behavior for the legacy `<event> [--dry-run]`
// invocation: print a dry-run notice when requested, otherwise noop. Phase 1
// does NOT port BuildEventIndex/execEffect into engine — the events/ directory
// is empty in live workspaces, and root's path is still active in parallel
// until Phase 5 sweeps it. Returning 0 matches root's behavior when no
// handlers match the trigger.
func runEmitHandlerStyle(args []string, defaultWorkspace string, stdout, stderr io.Writer) int {
	eventName := args[0]
	dryRun := false
	for _, a := range args[1:] {
		if a == "--dry-run" {
			dryRun = true
		}
	}

	// Root prints this to stderr when dry-run is set and no handlers match.
	if dryRun {
		fmt.Fprintf(stderr, "[dry-run] No handlers for event: %s\n", eventName)
	}

	return 0
}
