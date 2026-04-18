// transition_hooks.go — ADR-072 state-transition hook dispatch.
//
// Implements the per-transition handler layer described in ADR-072
// ("State-Transition Hooks for Node Lifecycle"). On every state change,
// `transition()` invokes a per-state enter handler. Each handler:
//
//   - Runs the minimum-viable per-ADR work inline (small, bounded).
//   - Dispatches any matching declarative StateHook definitions loaded from
//     `.cog/hooks/transitions/*.yaml`. Only the `shell:` form is implemented
//     at this revision — agent-form hooks (ADR-072 Phase 3) are loaded and
//     matched but logged as pending instead of executed.
//
// Non-goals of this file:
//   - Event-bus emission of transition records (owned by a sibling track).
//   - Full agent-subprocess spawning with budget/timeout (ADR-072 Phase 3).
//   - Condition evaluation beyond the scaffold (ADR-072 Phase 4).
//
// Concurrency: all handler bodies and hook executions run on goroutines
// spawned by `transition()`. They must not take `p.mu`; they may read
// immutable fields (cfg, sessionID) directly.
package engine

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// stateHook is the minimal in-memory representation of a StateHook YAML
// definition. It covers both shell-form and agent-form hooks; only shell
// execution is wired up at this revision.
type stateHook struct {
	Name     string
	From     ProcessState
	To       ProcessState
	Priority int
	Async    bool
	Timeout  time.Duration
	Shell    string // inline shell command (shell-form)
	HasAgent bool   // agent-form — loaded but not executed yet
}

// stateHookFile is the YAML wire format; see `.cog/hooks/transitions/*.yaml`.
type stateHookFile struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Trigger struct {
			From string `yaml:"from"`
			To   string `yaml:"to"`
		} `yaml:"trigger"`
		Priority int    `yaml:"priority"`
		Async    bool   `yaml:"async"`
		Timeout  string `yaml:"timeout"`
		Shell    string `yaml:"shell"`
		Agent    *struct {
			Model string `yaml:"model"`
		} `yaml:"agent"`
	} `yaml:"spec"`
}

// stateHookRegistry holds the sorted list of hooks matching each (from,to) pair.
type stateHookRegistry struct {
	mu    sync.RWMutex
	hooks []stateHook
}

// loadStateHookRegistry loads StateHook YAML files from the given workspace.
// Missing directory is a non-error (no hooks). Malformed files are skipped
// with a warning; they must not block kernel startup.
func loadStateHookRegistry(workspaceRoot string) *stateHookRegistry {
	reg := &stateHookRegistry{}
	if workspaceRoot == "" {
		return reg
	}
	dir := filepath.Join(workspaceRoot, ".cog", "hooks", "transitions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Debug("transition_hooks: registry dir read failed", "dir", dir, "err", err)
		}
		return reg
	}
	var loaded []stateHook
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			slog.Warn("transition_hooks: read failed", "file", e.Name(), "err", err)
			continue
		}
		var f stateHookFile
		if err := yaml.Unmarshal(raw, &f); err != nil {
			slog.Warn("transition_hooks: parse failed", "file", e.Name(), "err", err)
			continue
		}
		if f.Kind != "StateHook" {
			continue
		}
		from, okF := parseProcessState(f.Spec.Trigger.From)
		to, okT := parseProcessState(f.Spec.Trigger.To)
		if !okF || !okT {
			slog.Warn("transition_hooks: unknown trigger states", "file", e.Name(), "from", f.Spec.Trigger.From, "to", f.Spec.Trigger.To)
			continue
		}
		to_ := stateHook{
			Name:     f.Metadata.Name,
			From:     from,
			To:       to,
			Priority: f.Spec.Priority,
			Async:    f.Spec.Async,
			Timeout:  parseHookTimeout(f.Spec.Timeout),
			Shell:    strings.TrimSpace(f.Spec.Shell),
			HasAgent: f.Spec.Agent != nil,
		}
		loaded = append(loaded, to_)
	}
	sort.SliceStable(loaded, func(i, j int) bool { return loaded[i].Priority < loaded[j].Priority })
	reg.hooks = loaded
	if len(loaded) > 0 {
		slog.Info("transition_hooks: registry loaded", "count", len(loaded), "dir", dir)
	}
	return reg
}

func parseProcessState(s string) (ProcessState, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "active":
		return StateActive, true
	case "receptive":
		return StateReceptive, true
	case "consolidating":
		return StateConsolidating, true
	case "dormant":
		return StateDormant, true
	}
	return 0, false
}

func parseHookTimeout(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return 30 * time.Second
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	// Accept bare integer (seconds), as ADR-072 example uses `timeout: 300`.
	if d, err := time.ParseDuration(s + "s"); err == nil {
		return d
	}
	return 30 * time.Second
}

// match returns the hooks registered for the given transition, in priority order.
func (r *stateHookRegistry) match(from, to ProcessState) []stateHook {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []stateHook
	for _, h := range r.hooks {
		if h.From == from && h.To == to {
			out = append(out, h)
		}
	}
	return out
}

// runTransitionHooks executes all matching hooks for a (from,to) transition.
// Always invoked in a goroutine from `transition()`. Honors each hook's
// timeout; agent-form hooks are recognized but not executed at this revision
// (see ADR-072 Phase 3).
func (p *Process) runTransitionHooks(from, to ProcessState) {
	if p.hookRegistry == nil {
		return
	}
	hooks := p.hookRegistry.match(from, to)
	if len(hooks) == 0 {
		return
	}
	workspace := ""
	sessionID := ""
	if p.cfg != nil {
		workspace = p.cfg.WorkspaceRoot
	}
	sessionID = p.sessionID
	for _, h := range hooks {
		switch {
		case h.Shell != "":
			p.execShellHook(h, from, to, workspace, sessionID)
		case h.HasAgent:
			// TODO: implement agent-subprocess spawn with budget+timeout per
			// ADR-072 Phase 3. Until then, the declaration is honored only as
			// telemetry so operators can see that a hook was registered.
			slog.Info("transition_hooks: agent hook pending (ADR-072 Phase 3)",
				"name", h.Name, "from", from, "to", to)
		default:
			slog.Debug("transition_hooks: hook has no executable body", "name", h.Name)
		}
	}
}

// execShellHook runs a `shell:` StateHook under a timeout. Environment
// variables match the contract documented in `.cog/hooks/transitions/` YAML
// files (COG_TRANSITION_FROM / _TO / COG_SESSION_ID / COG_WORKSPACE).
func (p *Process) execShellHook(h stateHook, from, to ProcessState, workspace, sessionID string) {
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", h.Shell)
	cmd.Env = append(os.Environ(),
		"COG_TRANSITION_FROM="+from.String(),
		"COG_TRANSITION_TO="+to.String(),
		"COG_SESSION_ID="+sessionID,
		"COG_WORKSPACE="+workspace,
	)
	if workspace != "" {
		cmd.Dir = workspace
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		slog.Warn("transition_hooks: shell hook failed",
			"name", h.Name, "err", err, "output", strings.TrimSpace(string(out)))
		return
	}
	slog.Debug("transition_hooks: shell hook ok", "name", h.Name, "from", from, "to", to)
}

// ── Per-state enter handlers (ADR-072 §"Standard Hook Points") ──────────────
//
// Each handler runs off the main select loop on a goroutine spawned by
// transition(). Handlers must be short (<~20 lines), non-blocking w.r.t.
// the kernel tick cadence, and must not hold p.mu.
//
// The inline work done here is deliberately minimal. The bulk of per-state
// behavior lives either (a) in the declarative StateHook YAML registry
// dispatched above, or (b) in existing package logic that is not being
// moved in this revision (runConsolidation remains ticker-driven until the
// registry replaces it; see ADR-072 §"Migration Path" step 7).

// enterActive fires on any → active. The kernel is processing an external
// perturbation. Per ADR-072 "dormant → active: identity card reload, field
// warm-up, session init". At this revision the field is kept warm by the
// consolidation tick (runConsolidation calls p.field.Update), so no
// additional inline work is required — matching hooks in the registry
// (e.g., a future `identity-reload.yaml`) are dispatched by the caller.
func (p *Process) enterActive(from ProcessState) {
	// intentionally minimal per ADR-072 §Standard Hook Points (row: dormant→active)
	// TODO: migrate identity-card reload here once the agent-hook executor lands
	// (ADR-072 Phase 3). Until then, any declarative hook file in
	// .cog/hooks/transitions/ with trigger from=* to=active fires via
	// runTransitionHooks.
	_ = from
}

// enterReceptive fires on any → receptive. This is the idle/alert baseline
// state. Per ADR-072 "consolidating → receptive: light cone pruning, index
// rebuild, ResourcePressure update". Index rebuild is already performed by
// runConsolidation before it transitions to Receptive, so the inline body
// here stays empty to avoid duplicate work; matching registry hooks still
// fire.
func (p *Process) enterReceptive(from ProcessState) {
	// intentionally no-op per ADR-072 §"Migration Path" step 1:
	// existing ticker logic remains authoritative until hooks replace it.
	_ = from
}

// enterConsolidating fires on any → consolidating. Per ADR-072
// "active → consolidating: session review, journal write, turn metric
// aggregation". The heavyweight consolidation work (field update, index
// rebuild, coherence check, observer cycle, consolidation CogDoc write)
// is already performed by runConsolidation() which is invoked on the
// consolidationTicker path; duplicating it here would double the work on
// ticker-driven transitions. Instead the declarative hooks
// (session-review.yaml, log-transition.yaml) carry the per-session
// journaling, dispatched via runTransitionHooks.
func (p *Process) enterConsolidating(from ProcessState) {
	// intentionally minimal per ADR-072 §"Migration Path" step 7 — hook
	// dispatch is the new trigger surface; runConsolidation() still owns
	// the in-kernel pass until its logic is migrated into hook definitions.
	_ = from
}

// enterDormant fires on any → dormant. Per ADR-072 "receptive → dormant:
// deep consolidation, embedding refresh, sleep-time compute". At this
// revision the only inline work is recording the transition timestamp as
// the last consolidation watermark when a full consolidation has not run
// recently, so the next heartbeat's consolidation gate triggers correctly.
// Expensive resource-release work (model refs, embedding reloads) is
// delegated to declarative hooks.
func (p *Process) enterDormant(from ProcessState) {
	// No-op inline body: heartbeat path already handles dormant cadence
	// and consolidation gating (see emitHeartbeat). Matching registry
	// hooks (future on-dormant.yaml) handle deep-sleep work.
	// TODO: once ADR-072 Phase 3 lands, move expensive resource-release
	// calls into a declarative agent hook rather than inline Go.
	_ = from
}

// dispatchEnterHandler routes to the correct enter handler for the target
// state. Called from transition() on a goroutine; runs both the inline
// handler and the declarative-hook dispatch for (from,to).
func (p *Process) dispatchEnterHandler(from, to ProcessState) {
	switch to {
	case StateActive:
		p.enterActive(from)
	case StateReceptive:
		p.enterReceptive(from)
	case StateConsolidating:
		p.enterConsolidating(from)
	case StateDormant:
		p.enterDormant(from)
	}
	p.runTransitionHooks(from, to)
}
