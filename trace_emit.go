// trace_emit.go — Bus-publish helper for cycle-trace events (ADR-083).
//
// This file is the bridge between the schema-only `trace` subpackage and the
// kernel's bus API (`busSessionManager.appendBusEvent`). Emission is
// best-effort and non-blocking: errors are logged, never propagated. Losing
// a trace event is always preferable to stalling the metabolic cycle.
//
// Wiring:
//   - At serve startup, call `InstallTraceEmitter(busManager, root)` once.
//     That sets:
//       * trace bus ensured on disk (`bus_cycle_trace`)
//       * `engine.TraceEmitter` hook wired to `emitCycleEvent`
//       * `engine.TraceIdentity` wired to the active identity name
//     After that, engine/process.go and the agent harness can call
//     `emitCycleEvent` (or the engine hook) without further setup.
package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/cogos-dev/cogos/internal/engine"
	"github.com/cogos-dev/cogos/trace"
)

// traceBusID is the dedicated bus that carries cycle-trace events (B5 /
// ADR-083). External consumers (e.g. Mod³ dashboard) subscribe via the
// existing /v1/events/stream SSE endpoint filtered on this bus.
const traceBusID = "bus_cycle_trace"

// traceBusMgr holds the bus manager used by emitCycleEvent. Set once by
// InstallTraceEmitter. Accessed without a lock — a single writer at startup.
var traceBusMgr atomic.Pointer[busSessionManager]

// traceIdentityName is the cached active-identity name used as the `source`
// field on emitted events. Falls back to "cog" until wired.
var traceIdentityName atomic.Value // string

func init() {
	traceIdentityName.Store("cog")
}

// InstallTraceEmitter wires the engine's TraceEmitter / TraceIdentity hooks
// to the kernel bus and the active identity. Safe to call once at daemon
// start; subsequent calls overwrite the bus manager.
//
// root is the workspace root (used to resolve the identity name).
func InstallTraceEmitter(mgr *busSessionManager, root string) {
	if mgr == nil {
		return
	}
	traceBusMgr.Store(mgr)
	ensureTraceBus(mgr)

	if name, err := GetIdentityName(root); err == nil && name != "" {
		traceIdentityName.Store(name)
	}
	// TODO: wire to active identity (re-fetch if identity switches at runtime).

	engine.TraceEmitter = emitCycleEvent
	engine.TraceIdentity = func() string {
		if v := traceIdentityName.Load(); v != nil {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
		return "cog"
	}
	log.Printf("[trace] cycle-trace emitter installed (bus=%s identity=%s)",
		traceBusID, engine.TraceIdentity())
}

// ensureTraceBus creates the trace bus directory, events file, and registry
// entry if they don't already exist.
func ensureTraceBus(mgr *busSessionManager) {
	busDir := filepath.Join(mgr.busesDir(), traceBusID)
	if err := os.MkdirAll(busDir, 0o755); err != nil {
		log.Printf("[trace] create bus dir: %v", err)
		return
	}
	eventsFile := filepath.Join(busDir, "events.jsonl")
	if _, err := os.Stat(eventsFile); os.IsNotExist(err) {
		if f, err := os.Create(eventsFile); err == nil {
			f.Close()
		}
	}
	if err := mgr.registerBus(traceBusID, "kernel:trace", "kernel:trace"); err != nil {
		log.Printf("[trace] register bus: %v", err)
	}
}

// emitCycleEvent publishes a CycleEvent onto the trace bus. Best-effort:
// marshal or append errors are logged, never returned. The publish runs on a
// goroutine so the caller (state machine, harness loop) is never blocked by
// bus I/O or handler dispatch.
func emitCycleEvent(ev trace.CycleEvent) {
	mgr := traceBusMgr.Load()
	if mgr == nil {
		return
	}
	// Marshal via the trace package's canonical JSON shape, then re-decode
	// into a map so appendBusEvent can carry it as a CogBlock payload.
	raw, err := json.Marshal(ev)
	if err != nil {
		log.Printf("[trace] marshal: %v", err)
		return
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		log.Printf("[trace] unmarshal: %v", err)
		return
	}
	eventType := "cycle." + string(ev.Kind)
	go func() {
		if _, err := mgr.appendBusEvent(traceBusID, eventType, engine.TraceIdentity(), payload); err != nil {
			log.Printf("[trace] append: %v", err)
		}
	}()
}

// --- cycleID context plumbing for the agent harness ---------------------------
//
// The harness's Assess/Execute signatures are public; threading a cycleID
// parameter through them would be a breaking change. Instead we stash the ID
// on the context so tool-dispatch sites and per-turn emission can pull it
// out without plumbing. A missing value yields "" and emission falls back to
// a one-shot ID. See agent_harness.go / agent_serve.go for the real helpers.

type cycleIDKey struct{}
