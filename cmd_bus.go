// cmd_bus.go — CLI commands for the CogOS bus system.
//
// Commands:
//   cog bus watch [flags]     — Live event watcher with filtering
//   cog bus list              — List registered buses
//   cog bus tail [N]          — Last N events from a bus
//   cog bus send [flags]      — Append an event to a bus
//   cog bus help              — Show usage

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ─── Dispatcher ─────────────────────────────────────────────────────────────────

func cmdBus(args []string) int {
	if len(args) == 0 {
		cmdBusHelp()
		return 0
	}

	switch args[0] {
	case "watch":
		return cmdBusWatch(args[1:])
	case "list":
		return cmdBusList(args[1:])
	case "tail":
		return cmdBusTail(args[1:])
	case "send":
		return cmdBusSend(args[1:])
	case "help", "-h", "--help":
		cmdBusHelp()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "Unknown bus command: %s\n", args[0])
		cmdBusHelp()
		return 1
	}
}

// ─── watch ──────────────────────────────────────────────────────────────────────

func cmdBusWatch(args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	watcher := &BusWatcher{
		format:     "line",
		kernelAddr: "localhost:5100",
		root:       root,
	}

	var triggerExprs []string

	// Parse flags
	i := 0
	for i < len(args) {
		switch args[i] {
		case "-t", "--type":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: %s requires a value\n", args[i])
				return 1
			}
			i++
			watcher.filter.Types = append(watcher.filter.Types, args[i])

		case "-f", "--from":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: %s requires a value\n", args[i])
				return 1
			}
			i++
			watcher.filter.From = append(watcher.filter.From, args[i])

		case "--to":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: --to requires a value\n")
				return 1
			}
			i++
			watcher.filter.To = append(watcher.filter.To, args[i])

		case "-F", "--field":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: %s requires a value\n", args[i])
				return 1
			}
			i++
			ff, err := ParseFieldFilter(args[i])
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: invalid field filter %q: %v\n", args[i], err)
				return 1
			}
			watcher.filter.Fields = append(watcher.filter.Fields, ff)

		case "-b", "--bus":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: %s requires a value\n", args[i])
				return 1
			}
			i++
			watcher.busID = args[i]

		case "--format":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: --format requires a value\n")
				return 1
			}
			i++
			switch args[i] {
			case "line", "json", "full":
				watcher.format = args[i]
			default:
				fmt.Fprintf(os.Stderr, "Error: unknown format %q (use line, json, or full)\n", args[i])
				return 1
			}

		case "--no-replay":
			watcher.filter.NoReplay = true

		case "-n", "--limit":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: %s requires a value\n", args[i])
				return 1
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 1 {
				fmt.Fprintf(os.Stderr, "Error: invalid limit %q\n", args[i])
				return 1
			}
			watcher.limit = n

		case "--since":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: --since requires a value\n")
				return 1
			}
			i++
			t, err := parseSinceDuration(args[i])
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				return 1
			}
			watcher.filter.Since = t

		case "--trigger":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: --trigger requires a value\n")
				return 1
			}
			i++
			triggerExprs = append(triggerExprs, args[i])

		case "--offline":
			watcher.offline = true

		case "-q", "--quiet":
			watcher.quiet = true

		case "-h", "--help":
			cmdBusWatchHelp()
			return 0

		default:
			// Treat positional arg as bus ID if not set
			if watcher.busID == "" && !strings.HasPrefix(args[i], "-") {
				watcher.busID = args[i]
			} else {
				fmt.Fprintf(os.Stderr, "Error: unknown flag %q\n", args[i])
				return 1
			}
		}
		i++
	}

	// Build trigger filter if provided
	if len(triggerExprs) > 0 {
		trigger := &WatchFilter{}
		for _, expr := range triggerExprs {
			ff, err := ParseFieldFilter(expr)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: invalid trigger %q: %v\n", expr, err)
				return 1
			}
			trigger.Fields = append(trigger.Fields, ff)
		}
		watcher.trigger = trigger
	}

	// Auto-detect bus if not specified
	if watcher.busID == "" {
		busID, err := autoDetectBus(root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n  Use -b <bus_id> to specify a bus, or 'cog bus list' to see available buses.\n", err)
			return 1
		}
		watcher.busID = busID
	}

	// Set up signal handling for clean shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		cancel()
	}()

	if err := watcher.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	return 0
}

// ─── list ───────────────────────────────────────────────────────────────────────

func cmdBusList(args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	registryPath := filepath.Join(root, ".cog", ".state", "buses", "registry.json")
	data, err := os.ReadFile(registryPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("No buses registered.")
			return 0
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	var entries []busRegistryEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		fmt.Fprintf(os.Stderr, "Error: parse registry: %v\n", err)
		return 1
	}

	if len(entries) == 0 {
		fmt.Println("No buses registered.")
		return 0
	}

	// Check for JSON output flag
	for _, a := range args {
		if a == "--json" {
			out, _ := json.MarshalIndent(entries, "", "  ")
			fmt.Println(string(out))
			return 0
		}
	}

	// Table output
	fmt.Printf("%-40s  %-8s  %6s  %s\n", "BUS ID", "STATE", "EVENTS", "PARTICIPANTS")
	fmt.Printf("%-40s  %-8s  %6s  %s\n", strings.Repeat("─", 40), strings.Repeat("─", 8), strings.Repeat("─", 6), strings.Repeat("─", 30))
	for _, e := range entries {
		participants := strings.Join(e.Participants, ", ")
		if len(participants) > 50 {
			participants = participants[:47] + "..."
		}
		fmt.Printf("%-40s  %-8s  %6d  %s\n", e.BusID, e.State, e.EventCount, participants)
	}

	return 0
}

// ─── tail ───────────────────────────────────────────────────────────────────────

func cmdBusTail(args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	n := 10 // default
	busID := ""
	format := "line"

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-b", "--bus":
			if i+1 < len(args) {
				i++
				busID = args[i]
			}
		case "--format":
			if i+1 < len(args) {
				i++
				format = args[i]
			}
		case "-h", "--help":
			fmt.Println("Usage: cog bus tail [N] [-b bus_id] [--format line|json|full]")
			fmt.Println("\nShow the last N events from a bus (default: 10).")
			return 0
		default:
			if !strings.HasPrefix(args[i], "-") {
				if parsed, err := strconv.Atoi(args[i]); err == nil {
					n = parsed
				} else if busID == "" {
					busID = args[i]
				}
			}
		}
	}

	// Auto-detect bus
	if busID == "" {
		busID, err = autoDetectBus(root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n  Use -b <bus_id> to specify a bus.\n", err)
			return 1
		}
	}

	// Read all events from JSONL
	mgr := newBusSessionManager(root)
	events, err := mgr.readBusEvents(busID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	if len(events) == 0 {
		fmt.Printf("No events in bus %s\n", busID)
		return 0
	}

	// Take last N
	start := 0
	if len(events) > n {
		start = len(events) - n
	}
	tail := events[start:]

	// Format output
	w := &BusWatcher{format: format}
	for i := range tail {
		fmt.Println(w.formatEvent(&tail[i]))
	}

	return 0
}

// ─── help ───────────────────────────────────────────────────────────────────────

func cmdBusHelp() {
	fmt.Println(`Usage: cog bus <command> [flags]

Commands:
  watch    Live event watcher with filtering
  list     List registered buses
  tail     Show last N events from a bus
  send     Append an event to a bus
  help     Show this help

Run 'cog bus <command> --help' for command-specific help.`)
}

func cmdBusSendHelp() {
	fmt.Println(`Usage: cog bus send --bus <id> --type <event-type> --from <sender> \
                    (--message-json <json> | --payload-file <path>) [flags]

Appends a single event to a bus's JSONL event chain. Writes directly to
.cog/.state/buses/<bus_id>/events.jsonl by default — no kernel required
(offline-capable, symmetric with 'cog bus tail').

Required flags:
  -b, --bus <id>           Target bus ID
  -t, --type <str>         Event type (e.g. "message", "signal", "handoff.offer")
  -f, --from <str>         Sender identifier (e.g. "user:alice", "agent:planner")

Payload (exactly one required):
      --message-json <j>   Inline JSON payload. Use "-" to read from stdin.
      --payload-file <p>   Read JSON payload from file. Use "-" for stdin.

Optional:
      --to <str>           Target recipient (written into payload.to for
                           compatibility with /v1/bus/send)
      --http               POST to the running kernel's /v1/bus/send endpoint
                           instead of writing JSONL directly. Triggers SSE
                           broadcast. Fails if kernel is unreachable.
      --kernel <addr>      Kernel address when --http is set
                           (default: localhost:5100)
  -q, --quiet              Suppress the "seq=N hash=HH" confirmation line
  -h, --help               Show this help

Examples:
  cog bus send -b bus_notes -t "note.added" -f "user:alice" \
               --message-json '{"title":"hi","body":"world"}'

  echo '{"level":"info","msg":"ok"}' | \
    cog bus send -b bus_logs -t "log.line" -f "svc:api" --message-json -

  cog bus send -b bus_chat_sess1 -t "chat.request" -f "user:alice" \
               --payload-file req.json --http`)
}

// ─── send ───────────────────────────────────────────────────────────────────────

// cmdBusSend implements 'cog bus send' — the write counterpart to watch/tail.
//
// Default path writes directly to the bus's events.jsonl file using the same
// busSessionManager.appendBusEvent code path that the HTTP handler uses, so
// it works offline. --http opts into a POST /v1/bus/send call against a
// running kernel for SSE broadcast.
func cmdBusSend(args []string) int {
	var (
		busID       string
		evtType     string
		from        string
		to          string
		messageJSON string
		payloadFile string
		useHTTP     bool
		kernelAddr  = "localhost:5100"
		quiet       bool
		// sentinels so we can tell "flag omitted" from "flag set to empty".
		messageJSONSet bool
		payloadFileSet bool
	)

	i := 0
	for i < len(args) {
		switch args[i] {
		case "-b", "--bus":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: %s requires a value\n", args[i])
				return 2
			}
			i++
			busID = args[i]
		case "-t", "--type":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: %s requires a value\n", args[i])
				return 2
			}
			i++
			evtType = args[i]
		case "-f", "--from":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: %s requires a value\n", args[i])
				return 2
			}
			i++
			from = args[i]
		case "--to":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: --to requires a value\n")
				return 2
			}
			i++
			to = args[i]
		case "--message-json":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: --message-json requires a value\n")
				return 2
			}
			i++
			messageJSON = args[i]
			messageJSONSet = true
		case "--payload-file":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: --payload-file requires a value\n")
				return 2
			}
			i++
			payloadFile = args[i]
			payloadFileSet = true
		case "--http":
			useHTTP = true
		case "--kernel":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: --kernel requires a value\n")
				return 2
			}
			i++
			kernelAddr = args[i]
		case "-q", "--quiet":
			quiet = true
		case "-h", "--help":
			cmdBusSendHelp()
			return 0
		default:
			fmt.Fprintf(os.Stderr, "Error: unknown flag %q\n", args[i])
			cmdBusSendHelp()
			return 2
		}
		i++
	}

	// Required flag validation.
	var missing []string
	if busID == "" {
		missing = append(missing, "--bus")
	}
	if evtType == "" {
		missing = append(missing, "--type")
	}
	if from == "" {
		missing = append(missing, "--from")
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "Error: missing required flag(s): %s\n", strings.Join(missing, ", "))
		cmdBusSendHelp()
		return 2
	}
	if messageJSONSet && payloadFileSet {
		fmt.Fprintln(os.Stderr, "Error: --message-json and --payload-file are mutually exclusive")
		return 2
	}
	if !messageJSONSet && !payloadFileSet {
		fmt.Fprintln(os.Stderr, "Error: one of --message-json or --payload-file is required")
		cmdBusSendHelp()
		return 2
	}

	// Read payload bytes.
	payloadBytes, err := readSendPayload(messageJSON, messageJSONSet, payloadFile, payloadFileSet)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	// Parse into map[string]interface{} so we can attach "to" and hand off to
	// appendBusEvent (which takes a map). Reject non-object JSON — payloads
	// must be objects to match the CogBlock payload contract.
	var payload map[string]interface{}
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		fmt.Fprintf(os.Stderr, "Error: invalid JSON payload: %v\n", err)
		return 1
	}
	if payload == nil {
		payload = make(map[string]interface{})
	}
	if to != "" {
		// Only set if not already present — don't overwrite caller intent.
		if _, exists := payload["to"]; !exists {
			payload["to"] = to
		}
	}

	if useHTTP {
		return cmdBusSendHTTP(kernelAddr, busID, evtType, from, to, payload, quiet)
	}
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	return cmdBusSendDirect(root, busID, evtType, from, payload, quiet)
}

// readSendPayload resolves the JSON payload bytes from flags, supporting "-"
// as a stdin sentinel for both --message-json and --payload-file.
func readSendPayload(messageJSON string, messageJSONSet bool, payloadFile string, payloadFileSet bool) ([]byte, error) {
	if messageJSONSet {
		if messageJSON == "-" {
			return io.ReadAll(os.Stdin)
		}
		if strings.TrimSpace(messageJSON) == "" {
			return nil, fmt.Errorf("--message-json payload is empty")
		}
		return []byte(messageJSON), nil
	}
	// payloadFileSet
	if payloadFile == "-" {
		return io.ReadAll(os.Stdin)
	}
	data, err := os.ReadFile(payloadFile)
	if err != nil {
		return nil, fmt.Errorf("read --payload-file %q: %w", payloadFile, err)
	}
	return data, nil
}

// cmdBusSendDirect writes the event directly to the bus's events.jsonl file
// via busSessionManager.appendBusEvent. Offline-capable. The caller supplies
// an already-resolved workspace root so tests can target a temp directory
// without mutating ResolveWorkspace's cached singleton.
func cmdBusSendDirect(root, busID, evtType, from string, payload map[string]interface{}, quiet bool) int {
	mgr := newBusSessionManager(root)

	// Ensure the bus directory and events file exist — mirror the HTTP
	// handleBusSend path so a fresh bus_id is auto-created on first send.
	busDir := filepath.Join(mgr.busesDir(), busID)
	wasNew := false
	if _, statErr := os.Stat(busDir); os.IsNotExist(statErr) {
		wasNew = true
	}
	if err := os.MkdirAll(busDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Error: create bus dir: %v\n", err)
		return 1
	}
	eventsFile := filepath.Join(busDir, "events.jsonl")
	if _, err := os.Stat(eventsFile); os.IsNotExist(err) {
		f, err := os.Create(eventsFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: create events file: %v\n", err)
			return 1
		}
		f.Close()
	}

	// Register on first use so `cog bus list` surfaces the bus. Existing
	// entries are left untouched (registerBus returns early when present).
	if wasNew {
		if err := mgr.registerBus(busID, from, "cli:cog-bus-send"); err != nil {
			// Non-fatal: append will still work; just warn.
			fmt.Fprintf(os.Stderr, "warning: could not register bus %s: %v\n", busID, err)
		}
	}

	evt, err := mgr.appendBusEvent(busID, evtType, from, payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: append event: %v\n", err)
		return 1
	}

	if !quiet {
		fmt.Printf("seq=%d hash=%s\n", evt.Seq, evt.Hash)
	}
	return 0
}

// cmdBusSendHTTP POSTs to /v1/bus/send on the kernel so the event is both
// appended and broadcast via SSE. Fails hard if the kernel is unreachable —
// --http opts into the kernel path explicitly, so we don't fall back silently.
func cmdBusSendHTTP(kernelAddr, busID, evtType, from, to string, payload map[string]interface{}, quiet bool) int {
	// The /v1/bus/send handler expects a "message" string (content body), not
	// a structured payload map. To avoid losing structure, serialize the map
	// as JSON and stash it in "message"; consumers can re-parse. Callers with
	// simple string payloads can pass {"content":"..."} and the server will
	// copy content into payload.content anyway.
	//
	// Contract: busSendRequest{bus_id, from, to, message, type}.
	var message string
	if content, ok := payload["content"].(string); ok && len(payload) == 1 {
		// Simple case: payload is exactly {"content": "..."} — preserve the
		// string-only shape the HTTP handler already treats as canonical.
		message = content
	} else {
		buf, err := json.Marshal(payload)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: marshal payload for HTTP: %v\n", err)
			return 1
		}
		message = string(buf)
	}

	body, err := json.Marshal(map[string]interface{}{
		"bus_id":  busID,
		"from":    from,
		"to":      to,
		"message": message,
		"type":    evtType,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: marshal request: %v\n", err)
		return 1
	}

	url := fmt.Sprintf("http://%s/v1/bus/send", kernelAddr)
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: build request: %v\n", err)
		return 1
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: POST %s: %v\n  (is the kernel running? drop --http to write locally)\n", url, err)
		return 1
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "Error: kernel returned %d: %s\n", resp.StatusCode, strings.TrimSpace(string(respBody)))
		return 1
	}

	if !quiet {
		var decoded busSendResponse
		if err := json.Unmarshal(respBody, &decoded); err == nil && decoded.Hash != "" {
			fmt.Printf("seq=%d hash=%s\n", decoded.Seq, decoded.Hash)
		} else {
			// Fall back to raw response if shape surprised us.
			fmt.Println(strings.TrimSpace(string(respBody)))
		}
	}
	return 0
}

func cmdBusWatchHelp() {
	fmt.Println(`Usage: cog bus watch [bus_id] [flags]

Live event watcher with schema-based filtering. Connects to the kernel's
SSE endpoint for real-time events, or reads from JSONL files (--offline).

Flags:
  -t, --type <glob>      Event type filter (e.g. "chat.*", "*.message")
  -f, --from <glob>      Source filter (e.g. "user:*", "openclaw@*")
      --to <glob>        Target filter
  -F, --field <expr>     Payload field filter (e.g. "model=claude*", "tokens_used>100")
  -b, --bus <id>         Bus ID (default: auto-detect active bus)
      --format <fmt>     Output format: line (default), json, full
      --no-replay        Skip replaying historical events
  -n, --limit <N>        Stop after N matching events
      --since <dur>      Only events after timestamp/duration (e.g. "5m", RFC3339)
      --trigger <expr>   Break condition (same syntax as --field)
      --offline          Read from JSONL files instead of SSE
  -q, --quiet            Suppress status messages

Filter composition:
  Same flag type: OR'd    cog bus watch -t "chat.*" -t "tool.*"
  Different flags: AND'd  cog bus watch -t "chat.*" -f "user:*"

Field filter operators:
  key              Field exists (any value)
  key=val*         Glob match
  key!=val         Not equal
  key>N            Greater than (numeric)
  key<N            Less than (numeric)
  key>=N           Greater than or equal
  key<=N           Less than or equal

Examples:
  cog bus watch                                    # all events on auto-detected bus
  cog bus watch -t "chat.*"                        # chat lifecycle events
  cog bus watch -t "*.message"                     # channel messages
  cog bus watch -F "tokens_used>500" --format json # high-token events as JSON
  cog bus watch --trigger "finish_reason=stop" -n 1
  cog bus watch --offline -b bus_chat_http -t "chat.error"`)
}
