# CogOS Extraction Gaps

> Tracking all divergences between `apps/cogos/` (extracted kernel) and `.cog/` (workspace kernel).
> Generated: 2026-02-14

## Status Key

| Status | Meaning |
|--------|---------|
| DONE | Fixed and verified |
| IN_PROGRESS | Agent working on it |
| PENDING | Documented, not started |

---

## Phase 1: Search Performance (DONE)

Fixed the memory search hang — the critical agent-accessibility bug.

### salience.go — 3 fixes applied

| Fix | Description | Status |
|-----|-------------|--------|
| Churn gating | `COG_SALIENCE_CHURN` env var, default disabled | DONE |
| Early termination | `storer.ErrStop` past time window cutoff | DONE |
| Path normalization | Absolute → repo-relative for git `PathFilter` | DONE |

### memory.go — 4 fixes applied

| Fix | Description | Status |
|-----|-------------|--------|
| KeywordStrength | Added field to `MemorySearchResult` | DONE |
| Dead CQL removal | Removed `npx tsx cql.ts` codepath (script doesn't exist) | DONE |
| Two-phase scoring | Keyword+memory first, salience on top-N only | DONE |
| Env var controls | `COG_MEMORY_SALIENCE_LIMIT`, `_DISABLE`, `_FORCE` | DONE |

**Result:** `cog memory search "eigenform"` → 128 docs in <1s (was hanging at 15s+).

---

## Phase 2: FTS5 Wiring (PENDING)

Wire the existing constellation FTS5 index into `MemorySearch()` as the primary search path.

### Context

- `constellation.db` at `.cog/.state/` already has a fully built FTS5 index
- BM25 ranking, porter stemming, unicode61 tokenization
- `cog constellation search` already works
- Gap: constellation only indexes `.cog.md` files (1485), not plain `.md` (192 more in `.cog/mem/`)

### Tasks

| Task | Description | Status |
|------|-------------|--------|
| Primary search path | `MemorySearch()` calls `constellation.Search()` first | PENDING |
| Grep fallback | Retain grep as emergency fallback when DB unavailable | PENDING |
| Index coverage | Extend indexer to cover plain `.md` files in `.cog/mem/` | PENDING |
| Lazy index build | Build FTS5 index on first search if missing | PENDING |

---

## Phase 3: Section-Aware Operations (PENDING)

Port the section parser and section-aware memory commands that `CLAUDE.md` documents and agents rely on.

### Context

The workspace kernel's `CLAUDE.md` tells agents to use:
```bash
./scripts/cog memory toc <path>                    # See sections + sizes
./scripts/cog memory read <path> --section "Name"  # Read one section
./scripts/cog memory read <path> --frontmatter     # Just the metadata
```

These depend on section parsing functions in `.cog/lib/go/frontmatter/sections.go`.

### Tasks

| Task | Description | Status |
|------|-------------|--------|
| Section parser | Port `sections.go` from `.cog/lib/go/frontmatter/` | PENDING |
| MemoryTOC | `cog memory toc <path>` command | PENDING |
| MemoryReadSection | `cog memory read <path> --section "Name"` | PENDING |
| MemoryReadFrontmatter | `cog memory read <path> --frontmatter` | PENDING |

---

## Phase 4: HIGH Priority Subsystems (PENDING)

Port before standalone release — these are core capabilities.

### CogBus (17 files)

Inter-workspace coordination subsystem. The entire bus architecture.

| File | Purpose | Status |
|------|---------|--------|
| `bus.go` | Bus core | PENDING |
| `bus_bloom.go` | Bloom filter routing | PENDING |
| `bus_broadcast.go` | Broadcast messaging | PENDING |
| `bus_codec.go` | Message codec | PENDING |
| `bus_commands.go` | CLI commands | PENDING |
| `bus_config.go` | Configuration | PENDING |
| `bus_discovery.go` | Peer discovery | PENDING |
| `bus_envelope.go` | Message envelope | PENDING |
| `bus_health.go` | Health monitoring | PENDING |
| `bus_heartbeat.go` | Heartbeat protocol | PENDING |
| `bus_mesh.go` | Mesh networking | PENDING |
| `bus_metrics.go` | Metrics collection | PENDING |
| `bus_peer.go` | Peer management | PENDING |
| `bus_protocol.go` | Protocol definitions | PENDING |
| `bus_router.go` | Message routing | PENDING |
| `bus_transport.go` | Transport layer | PENDING |
| `bloom.go` | Bloom filter impl | PENDING |

### Crypto (2 files)

Ed25519 keypair management — bus dependency.

| File | Purpose | Status |
|------|---------|--------|
| `crypto.go` | Key generation/signing | PENDING |
| `crypto_test.go` | Tests | PENDING |

### MCP Server (1 file)

MCP server for IDE integration (Claude Code, etc.).

| File | Purpose | Status |
|------|---------|--------|
| `mcp.go` | MCP protocol server | PENDING |

### Event Index (2 files)

SQLite-backed event index and emission CLI.

| File | Purpose | Status |
|------|---------|--------|
| `event_index.go` | SQLite event storage | PENDING |
| `cmd_emit.go` | `cog emit` CLI | PENDING |

### Inference Resilience

Functions in `inference.go` that exist in workspace but not extraction.

| Function | Purpose | Status |
|----------|---------|--------|
| `loadWorkspaceSecrets` | Secret loading from `.cog/secrets/` | PENDING |
| `getAPIKey` | Multi-source API key resolution | PENDING |
| `RunInferenceWithFallback` | Model fallback chain | PENDING |

---

## Phase 5: MEDIUM Priority (PENDING)

### Intent / NLI (4 files)

Natural language interface — powers `scripts/cog -p` prompt mode.

| File | Purpose | Status |
|------|---------|--------|
| `intent_decoder.go` | NL → command mapping | PENDING |
| `intent_decoder_test.go` | Tests | PENDING |
| `cmd_intent.go` | `cog intent` CLI | PENDING |
| `pattern_loader.go` | Intent pattern definitions | PENDING |

### Capabilities Command

Self-description for agent discovery — `cog capabilities` / `cog caps`.

| Task | Status |
|------|--------|
| Port capabilities dispatch from workspace `cog.go` | PENDING |

### SDK Workflow Types (2 files)

ADR-052 workflow execution types.

| File | Purpose | Status |
|------|---------|--------|
| `workflow_types.go` | Workflow definitions | PENDING |
| `workflow_exec.go` | Workflow execution | PENDING |

---

## Phase 6: LOW Priority / Skip

These are workspace-specific or optimization tooling — not needed for standalone.

| System | Files | Reason to Skip |
|--------|-------|----------------|
| Model router | `model_router.go` | Inference optimization |
| Complexity scoring | `complexity.go` | Cost tracking |
| Cost tracker | `cost_tracker.go` | Cost tracking |
| Plugin loader | `plugin_loader.go` | Extensibility — not MVP |
| Capability router | `capability_router.go` | Extensibility — not MVP |
| App/field/release commands | various | Workspace-specific tooling |
| Compat layers | 4 files | Correctly omitted — types inlined |

---

## Correctly Handled in Extraction

These items are verified correct — no action needed.

| Item | Notes |
|------|-------|
| `cmdInit()` | Identical between both versions |
| SDK constellation | 1:1 mirror with extra `DB()` accessor |
| Inlined types | frontmatter, RBAC, policy, UCP — correct for standalone |
| CogField viz backend | New addition in extraction, not in workspace |
| Build system | Clean `make build`, `make test` — all pass |
| Identity signing | `cog init` + `cog verify` both work |
| Health/coherence | Full workspace health checks operational |

---

## Architecture Notes

The extraction relationship is:

```
apps/cogos/     = distributable standalone kernel (12MB, 5304 lines)
.cog/           = workspace-resident kernel with full local tooling (18MB, 7530 lines)
scripts/cog     = CLI wrapper dispatching to .cog/cog
```

The extraction reorganized rather than simply subset:
- UCP consolidated from 5 files → `ucp.go`
- Frontmatter standalone (not `_compat.go`)
- CogField adapters added (`cogfield.go`, `cogfield_adapters.go`, etc.)
- RBAC/policy consolidated
- Bus, bloom, crypto, intent decoder, MCP, model router, plugin loader stripped
