# CogOS

A cognitive daemon for AI agents. Written in Go. Runs locally. Gives Claude Code, Cursor, and other AI tools persistent memory, scored context, and workspace continuity.

```sh
make build && ./cogos serve --workspace ~/my-project
# http://localhost:6931/health
```

---

## What it does

- **Foveated context assembly** -- A live hook (`UserPromptSubmit`) fires on every Claude Code prompt, scores all available documents by relevance, and injects a focused context window. No manual `@`-file selection; the system decides what matters.

- **Learned retrieval via TRM** -- A 2.3M-parameter Mamba SSM (Tiny Recursive Model) trained to 0.878 mean NDCG@10 (0.900 peak) through 500+ tracked experiments. Scores documents by temporal salience, edit recency, and semantic relevance. Runs inference locally in ~6KB of state. See [docs/EVALUATION.md](docs/EVALUATION.md) for full methodology.

- **Persistent memory** -- Hierarchical memory system with salience scoring and temporal attention. Your workspace remembers across sessions, models, and tools. Switch from Claude Code to Cursor and back -- same memory, same context.

- **Multi-provider routing** -- OpenAI-compatible and Anthropic Messages-compatible HTTP API. Works with Ollama, LM Studio, Claude, and any OpenAI-compatible endpoint. Local models preferred by default.

- **Hash-chained ledger** -- CogBlock protocol for content-addressed, hash-chained records. Every routing decision, context assembly, state transition, turn completion, tool call, and config mutation is recorded in an append-only ledger (SHA-256, RFC 8785) with optional chain verification.

- **Three observability lanes** -- The kernel exposes ledger (durable hash-chained events), traces (client metabolites + attention + proprioceptive state), and kernel slog (structured runtime logs) as non-overlapping surfaces. Each has a dedicated MCP tool, HTTP endpoint, and on-disk format.

- **Live event bus** -- `AppendEvent` fans into an in-process broker with SSE streaming at `/v1/bus/:id/events/stream`. Subscribers see writes in real time; offline writers go straight to JSONL.

- **Conversation persistence** -- `turn.completed` ledger events plus a per-session sidecar at `.cog/run/turns/<sessionID>.jsonl` preserve full prompt and response text (8KB / 16KB truncated previews in ledger, full body in sidecar).

- **Config mutation API** -- `cog_read_config` / `cog_write_config` / `cog_rollback_config` MCP tools and matching REST surface. RFC 7396 merge-patch semantics with atomic writes and rotating backups.

- **Library extraction** -- Seven importable Go packages in `pkg/` covering the core type system: content-addressed blocks, coordination primitives, BEP wire protocol, reconciliation framework, modality bus, field graph types, and URI parsing. All usable independently of the kernel.

- **Native agent harness** -- A homeostatic agent loop that runs as a goroutine inside the kernel process. Calls Gemma E4B via Ollama's native `/api/chat` endpoint with six kernel-native tools. Adaptive interval (5m-30m) based on assessment urgency, with panic recovery. Loop triggers, state snapshots, and listings now reachable over MCP and REST.

- **MCP Streamable HTTP** -- Full MCP transport at `POST /mcp` with JSON-RPC 2.0, session management, and 30-minute expiry. Always-on (no build tag). 22 tools spanning observability, agent control, config, memory, and voice.

- **Anthropic Messages API proxy** -- Transparent proxy at `POST /v1/messages` that forwards to the real Anthropic API with streaming SSE passthrough. Enables `cog claude` to route Claude Code through the kernel via `ANTHROPIC_BASE_URL`.

- **Foveated decomposition pipeline** -- `cog decompose` processes any input through E4B into four tiers: Tier 0 (one-sentence, ~15 tokens), Tier 1 (paragraph, ~100 tokens), Tier 2 (full CogDoc with sections and embeddings), Tier 3 (raw, gated). Includes an interactive workbench TUI (`--workbench`), embedding co-generation via nomic-embed-text, content-addressed CogDoc storage, and bus event emission for observability. This is the DECOMPOSE stage of the CogOS Hypercycle.

---

## Architecture

CogOS runs as a single Go binary daemon. The kernel has three layers:

```
┌──────────────────────────────────────────────────────────────┐
│  Membrane          HTTP API · MCP Server · Provider Router   │
│                    Event Broker (SSE) · Config API           │
├──────────────────────────────────────────────────────────────┤
│  Workspace         Context Engine · Memory · Ledger          │
│                    Salience Scorer · Blob Store · Traces     │
│                    Conversation Sidecars · Kernel Slog       │
├──────────────────────────────────────────────────────────────┤
│  Nucleus           Process Loop · Identity · State FSM       │
│                    Agent Harness · CogBus · Tool-call Gate   │
└──────────────────────────────────────────────────────────────┘
```

**Membrane** -- The API surface. Serves OpenAI and Anthropic-compatible chat endpoints, the always-on MCP Streamable HTTP server, the Anthropic Messages API proxy, the event broker with SSE streaming, the config mutation API, and foveated context assembly. Routes inference requests to local or cloud providers. Binds to `127.0.0.1:6931` by default; `--bind` or `bind_addr` in YAML relaxes CORS when set to a non-loopback interface.

**Workspace** -- Where state lives. The context engine scores documents and arranges them into stability zones optimized for KV cache reuse. The ledger is append-only and hash-chained. Traces capture attention, proprioceptive state, and internal request metabolites. Conversation sidecars persist full turn text. Memory persists across sessions.

**Nucleus** -- The process loop. Runs continuously through four states (Active, Receptive, Consolidating, Dormant). Manages identity, consolidation, workspace lifecycle, the homeostatic agent harness, the tool-call hallucination gate, and emits to the kernel slog (stderr tee plus `.cog/run/kernel.log.jsonl`).

### How foveated context works

When you submit a prompt in Claude Code, the `UserPromptSubmit` hook fires and calls the CogOS daemon. The context engine:

1. Scores all workspace documents using TRM + git-derived salience
2. Ranks by a composite signal (edit recency, semantic match, structural importance)
3. Assembles a context window organized into stability zones:

| Zone | Contents | Behavior |
|------|----------|----------|
| 0 -- Nucleus | Identity, system config | Always present, never evicted |
| 1 -- Knowledge | Workspace docs, indexed memory | Shifts slowly, high cache hit rate |
| 2 -- History | Conversation turns | Scored by relevance, evictable |
| 3 -- Current | The current message | Always present |

4. Injects the assembled context into the prompt before it reaches the model

The model sees a pre-focused window instead of everything-or-nothing.

---

## Exposure surfaces

Three non-overlapping observability lanes, each with an MCP tool, HTTP endpoint, and on-disk artifact:

| Lane | What it captures | MCP tool | HTTP | On-disk |
|------|------------------|----------|------|---------|
| **Ledger** | Durable hash-chained events (turns, config mutations, tool calls, state changes) | `cog_read_ledger` | `GET /v1/ledger` (optional `?verify_chain=true`) | Append-only CogBlock chain |
| **Traces** | Client metabolites, attention, proprioceptive state, internal requests | `cog_search_traces` | `GET /v1/traces` (legacy `/v1/proprioceptive` preserved) | Trace files |
| **Kernel slog** | Structured runtime logs via `teeHandler` | `cog_tail_kernel_log` | `GET /v1/kernel-log` | stderr + `.cog/run/kernel.log.jsonl` |

The live event bus is a fourth surface for real-time subscribers: SSE at `/v1/bus/:id/events/stream`, plus `cog_tail_events` and `cog_query_events` MCP tools. All `AppendEvent` writes fan through the broker.

---

## Library packages (pkg/)

Seven importable Go packages extracted into a `go.work` multi-module workspace. Each has its own `go.mod` and can be imported independently of the kernel. Six are stdlib-only; `pkg/bep` requires `google.golang.org/protobuf`.

| Package | What it provides |
|---------|-----------------|
| `pkg/cogblock` | Content-addressed block format, CogBlockKind enum, provenance/trust types, EventEnvelope, ledger (RFC 8785 canonicalization, hash chain, verify) |
| `pkg/coordination` | Claim/Handoff/Broadcast types, 13 coordination functions, AgentID |
| `pkg/bep` | BEP wire protocol types, TLS/DeviceID, index/version vectors, events, Engine/SyncProvider interfaces |
| `pkg/reconcile` | Reconcilable interface (7 methods), State/Plan/Action types, registry, Kahn's topological sort, meta-orchestrator |
| `pkg/modality` | Module interface, Bus, wire protocol (D2), events, salience tracker, channels, ProcessSupervisor |
| `pkg/cogfield` | Node/Edge/Graph types, Block, BlockAdapter interface, conditions, signals, sessions, documents |
| `pkg/uri` | URI struct, Parse/Format, 35 namespaces, ExtractInlineRefs, error types |

69 files, ~10,200 lines, 190 tests across all packages.

---

## Agent harness

The native Go agent harness runs a homeostatic assessment loop inside the kernel process:

- Calls Gemma E4B via Ollama's native `/api/chat` (with `think: false`)
- Adaptive interval: 5m idle, scales to 30m when assessment urgency is low
- Panic recovery -- a crash in the agent goroutine doesn't take down the kernel
- State and loop control over MCP (`cog_list_agents`, `cog_get_agent_state`, `cog_trigger_agent_loop`) and REST (`/v1/agents[/...]`). The singular `/v1/agent/{status,traces,trigger}` routes are preserved byte-for-byte for the embedded dashboard.

Six kernel-native tools are available to the agent itself:

| Tool | Description |
|------|-------------|
| `memory_search` | Search CogDocs by query |
| `memory_read` | Read a specific memory document |
| `memory_write` | Write or update a memory document |
| `coherence_check` | Run drift detection on the workspace |
| `bus_emit` | Emit an event to the CogBus |
| `workspace_status` | Get workspace health and metrics |

---

## HTTP API

| Endpoint | Description |
|----------|-------------|
| `POST /v1/chat/completions` | OpenAI-compatible chat (streaming + non-streaming) |
| `POST /v1/messages` | Anthropic Messages API proxy (streaming SSE passthrough) |
| `POST /v1/context/foveated` | Foveated context assembly |
| `POST /v1/context/build` | Context engine without inference step |
| `GET /v1/context` | Current attentional field |
| `GET /v1/ledger` | Read hash-chained ledger; `?verify_chain=true` walks the chain |
| `GET /v1/traces` | Client metabolites, attention, proprioceptive, internal requests |
| `GET /v1/proprioceptive` | Legacy byte-compatible trace subset |
| `GET /v1/kernel-log` | Structured kernel slog tail |
| `GET /v1/conversation` | Turn history with full prompt/response text |
| `GET /v1/tool-calls` | Tool-call records and correlation state |
| `GET /v1/config` · `PATCH /v1/config` | Read or RFC 7396 merge-patch configuration |
| `POST /v1/config/rollback` | Roll back to a previous atomic backup |
| `GET /v1/agents` · `GET /v1/agents/:id/state` · `POST /v1/agents/:id/trigger` | Plural agent control surface |
| `GET /v1/agent/status` · `GET /v1/agent/traces` · `POST /v1/agent/trigger` | Singular agent routes (preserved for dashboard byte-compat) |
| `GET /v1/bus/:id/events/stream` | SSE stream of broker events |
| `GET /health` | Liveness probe (identity, state, trust) |
| `GET /dashboard` | Embedded web dashboard |
| `POST /mcp` · `DELETE /mcp` | MCP Streamable HTTP (JSON-RPC 2.0, session lifecycle) |

All endpoints serve on port **6931** by default. `--bind <addr>` (or `bind_addr` in YAML) overrides; CORS is strict on loopback and relaxed on non-loopback binds.

### MCP tools (22 total)

The always-on MCP server groups tools by surface. `mcpserver` build tag was removed in #9 -- MCP ships in every binary.

| Category | Tool | Purpose |
|----------|------|---------|
| **Observability** | `cog_read_ledger` | Read hash-chained ledger events (optional chain verify) |
| | `cog_search_traces` | Query client metabolites + attention + proprioceptive + internal-request traces |
| | `cog_tail_kernel_log` | Stream the structured kernel slog |
| | `cog_tail_events` | Tail the live event bus |
| | `cog_query_events` | Filter event bus records by predicate |
| **Conversation / tool calls** | `cog_read_conversation` | Read turn sidecars with full prompt/response text |
| | `cog_read_tool_calls` | Read tool-call records + correlation state |
| | `cog_tail_tool_calls` | Live stream of tool-call activity |
| **Agent control** | `cog_list_agents` | Enumerate running agents |
| | `cog_get_agent_state` | State snapshot for an agent |
| | `cog_trigger_agent_loop` | Manually trigger an assessment cycle |
| **Config** | `cog_read_config` | Read the live config |
| | `cog_write_config` | RFC 7396 merge-patch (atomic write, rotating backups, `requires_restart=true` in v1) |
| | `cog_rollback_config` | Restore a prior atomic backup |
| **Memory** | `cogos_memory_search` | Search CogDocs |
| | `cogos_memory_read` | Read a memory document |
| | `cogos_memory_write` | Write or update a memory document |
| | `cogos_coherence_check` | Drift detection across the workspace |
| **Voice (Mod3 bridge)** | `mod3_speak` · `mod3_stop` · `mod3_voices` · `mod3_status` | TTS and voice channel control |

Sessions are created on `initialize` and expire after 30 minutes of inactivity.

### Providers

Ships with adapters for Anthropic, Ollama, Claude Code, and Codex. New providers implement [six methods](docs/writing-a-provider.md).

---

## CLI

`cog` wraps the daemon with subcommands for the common lifecycle plus the event bus:

```sh
cog serve               # Start the daemon (--bind <addr> to expose beyond loopback)
cog claude              # Launch Claude Code with ANTHROPIC_BASE_URL set to the kernel
cog decompose ...       # Run the 4-tier foveated decomposition pipeline
cog emit ...            # Write an event through the engine (no more silent drops)
cog bus watch|tail|list # Read the event bus
cog bus send ...        # Write to the bus. Direct JSONL by default; --http for SSE broadcast
```

`cog emit` was migrated to the engine in #23 (Track 5 Phase 1): the root `cmdEmit` is retained for compat but the engine path fixes the silent-drop bug where hooks returned success without writing to the ledger.

---

## Getting started

### Requirements

- Go 1.24+
- macOS, Linux, or Windows

### Build and run

```sh
git clone https://github.com/cogos-dev/cogos.git
cd cogos
make build

# Initialize a workspace
./cogos init --workspace ~/my-project

# Start the daemon
./cogos serve --workspace ~/my-project

# Verify it's running
curl -s http://localhost:6931/health | jq .
```

### Cross-compile

```sh
make build-linux-amd64
make build-linux-arm64    # Unblocked in #31 (syscall.Dup2 → unix.Dup2)
make build-darwin-arm64
make build-windows-amd64  # Added in #15; see docs for install steps
```

### Route Claude Code through the kernel

```sh
cog claude
# Sets ANTHROPIC_BASE_URL to http://localhost:6931 and starts Claude Code
```

### Developer setup

```sh
./scripts/setup-dev.sh    # Build, install to ~/.cogos/bin, configure PATH
```

### Docker

```sh
make image        # Build production image
make run          # Run with workspace volume mount
make e2e          # Build + run full cold-start test in a container
```

A Docker Compose topology with `bridge-{primary,secondary}` and `tailscale-{primary,secondary}` siblings landed in #19.

---

## Testing

```sh
make test         # Unit tests (with -race)
make e2e-local    # Full cold-start lifecycle test
make e2e          # Containerized e2e (Docker)
```

Ledger and sync-watcher tests are stable under `-count>=2` (#30). Roughly 190 tests across the `pkg/` library packages plus the kernel suite in `internal/engine/`.

---

## Project layout

```
cmd/cogos/              Entry point (thin — delegates to internal/engine)
internal/engine/        Kernel sources and tests
pkg/                    Importable library packages (go.work multi-module)
  cogblock/             Content-addressed blocks and ledger
  coordination/         Agent coordination primitives
  bep/                  BEP wire protocol types
  reconcile/            Reconciliation framework
  modality/             Modality bus and channel types
  cogfield/             Field graph types
  uri/                  URI parsing and namespaces
sdk/                    Go SDK for CogOS clients
docs/                   Specs, architecture docs, provider guide
scripts/                Setup, CLI wrapper, e2e tests, experiment harnesses
```

---

## Status

**v3 kernel** -- Ground-up rewrite after a year of daily use across Claude Code, Cursor, and custom agent harnesses.

### Working

- Continuous process daemon with four-state FSM
- Foveated context assembly with Mamba TRM (0.878 mean NDCG@10)
- Hash-chained append-only ledger with optional chain verification
- Three-lane observability: ledger, traces, kernel slog
- Live event bus with in-process broker and SSE streaming
- Conversation persistence (turn sidecars + ledger `turn.completed` events)
- Tool-call observability with pending-call correlation cache (1024 / 10min)
- Config mutation API (MCP + REST, merge-patch, atomic write, rollback)
- Agent state and loop control over MCP and REST (singular routes preserved)
- Multi-provider routing (Ollama, Anthropic, Claude Code, Codex)
- Always-on MCP Streamable HTTP server (22 tools, sessions, JSON-RPC 2.0)
- Anthropic Messages API proxy with streaming SSE
- Native Go agent harness with adaptive interval and 6 kernel tools
- Embedded web dashboard with agent status, cycle history, and decomposition panel
- Foveated decomposition pipeline (`cog decompose`) with 4-tier output, workbench TUI, embeddings, and bus events
- Library extraction: 7 packages in pkg/ (~10.2K LOC, 190 tests)
- Content-addressed blob store
- Git-derived salience scoring
- Tool-call hallucination gate (activated by `NormalizeMCPRequest` path in #25)
- Digestion pipeline (Claude Code + OpenClaw adapters)
- Memory consolidation
- OpenAI and Anthropic API compatibility
- `cog bus send` subcommand for write-side symmetry (direct JSONL or opt-in SSE)
- `--bind` flag with non-loopback CORS relaxation
- Windows cross-compile (`make build-windows-amd64`) and linux/arm64
- End-to-end test suite
- OpenTelemetry instrumentation
- Port consolidation on 6931

### Next

- Wire digestion tailers into process loop
- Constellation library integration (multi-node sync)
- Multi-agent process management
- Further agent state surface (beyond the v1 trigger/state/list)

---

## Ecosystem

CogOS is one piece of a larger system. Each component is its own repo with independent releases:

| Repo | Purpose | Status |
|------|---------|--------|
| **[cogos](https://github.com/cogos-dev/cogos)** | The daemon -- this repo | Active |
| [constellation](https://github.com/cogos-dev/constellation) | Distributed identity and workspace sync (BEP-based) | Active |
| [mod3](https://github.com/cogos-dev/mod3) | Modality bus -- voice I/O, TTS, channel multiplexing | Active |
| [skills](https://github.com/cogos-dev/skills) | Agent skill library (Claude Code compatible) | Active |
| [charts](https://github.com/cogos-dev/charts) | Helm charts and Docker Compose for deployment | Active |

---

## Design documents

- [System Specification](docs/SYSTEM-SPEC.md) -- Multi-level spec from ontology to deployment
- [Architectural Principles](docs/architecture/principles.md) -- Core engineering constraints
- [Writing a Provider](docs/writing-a-provider.md) -- How to add a new inference provider
- [MCP Specification](docs/MCP-SPEC.md) -- MCP server contract
- [Provider Specification](docs/PROVIDER-SPEC.md) -- Provider interface contract
- [Architecture Diagrams](docs/architecture-diagram-source.md) -- Cell model, topology views
- [Cognitive GitOps](docs/architecture/cognitive-gitops.md) -- Substrate-coordinated repo model
- [E2E Test Plan](docs/E2E-TEST-PLAN.md) -- End-to-end test strategy

---

## License

[MIT](LICENSE) -- Copyright (c) 2025-2026 Chaz Dinkle
