# Changelog

## [Unreleased]

## [2.6.0] - 2026-04-15 — Decomposition pipeline workbench

### Added
- `cog decompose` CLI command with 4-tier foveated decomposition via E4B:
  Tier 0 (one-sentence ~15 tokens), Tier 1 (paragraph ~100 tokens),
  Tier 2 (full CogDoc with frontmatter + sections + embeddings),
  Tier 3 (raw passthrough, gated)
- `DecompositionRunner` engine using `AgentHarness.GenerateJSON()` with
  JSON mode, per-tier schema validation, and one-retry error correction
- Interactive workbench TUI (`--workbench`): Bubbletea 2x2 viewport grid
  with tier focus switching, re-run, and metrics bar
- Embedding co-generation via nomic-embed-text (128-dim + 768-dim Matryoshka)
  for Tier 0, 1, and 2 output
- Content-addressed CogDoc storage at `.cog/mem/semantic/decompositions/`
  with full YAML frontmatter, section index, and source refs
- Constellation indexing for vector + FTS5 retrieval of decomposition output
- Bus event lifecycle (`decompose.start/tier.start/tier.complete/complete/error`)
  with file-based JSONL emission for standalone CLI runs
- Quality metrics: compression ratio, cross-tier embedding fidelity (cosine
  similarity), schema conformance tracking
- Dashboard Decompose tab with recent decomposition history, per-tier
  timing bars, and compression ratio color coding
- `GenerateJSON()` method on `AgentHarness` for general-purpose JSON-mode
  LLM completions (reusable beyond decomposition)
- 52 tests (unit + integration) across 4 test files, including mock Ollama
  server tests for prompt construction, retry logic, and event sequencing

### Files
- `decompose.go` — Core engine, types, prompts, CLI, formatter (846 lines)
- `decompose_store.go` — Embedding generation, CogDoc storage (306 lines)
- `decompose_tui.go` — Bubbletea workbench TUI (351 lines)
- `decompose_test.go` — Unit tests (1,325 lines)
- `decompose_store_test.go` — Storage tests (238 lines)
- `decompose_tui_test.go` — TUI tests (97 lines)
- `decompose_integration_test.go` — E2E with live Ollama (310 lines)

## [2.5.0] - 2026-04-14 — Gemma 4 default, dashboard model selector

### Changed
- Default Ollama model switched from Qwen 3.5 / Llama 3.2 to Gemma 4 E4B across all layers (inference.go, harness, serve_providers, provider_pi, dashboard HTML)
- Dashboard model selector updated: gemma4:e4b, gemma4:e2b, gemma4:26b, llama3.2:1b
- Provider model list now reflects locally available Ollama models
- Pi provider default model uses `defaultOllamaModel` constant instead of hardcoded string
- Help text in chat and benchmark commands updated for Gemma 4 examples

## [2.4.0]

### Added
- OpenAI-compatible provider for LM Studio, vLLM, llama.cpp (1,613 LOC, 18 tests)
- Auto-discovery of inference providers on localhost
- Professional README with progressive disclosure
- CONTRIBUTING.md
- Autoresearch pipeline (extract-signals, nightly-consolidation, survey-traces)
- Experiment harness for cross-node benchmarking
- Context assembly path fix for TRM-scored documents

### Changed
- README rewritten for clarity and approachability

### Fixed
- `Available()` in OpenAI-compat provider now returns false when the configured model is not in the server's model list

## [0.0.1] - 2026-04-03 — Performance: eliminate CPU burn in continuous process

The v3 daemon was consuming 200% CPU perpetually due to compounding
inefficiencies in the consolidation loop. This release fixes all of them
and brings idle CPU to 0%.

### Root cause

`RankFilesBySalience` called `ComputeFileSalience` per file (4,637 memory
files), and each call opened the 2.4 GB git repo from scratch via
`git.PlainOpen`. This ran every 5 minutes with no caching. The field never
successfully populated (stuck at `field_size=0`, state `consolidating`).

### Fixes

**salience.go — Single-pass batch scoring**
- `RankFilesBySalience` now walks the git log once via `batchCollectStats`,
  building a file-to-stats map in a single commit walk. Complexity drops from
  O(files x commits) to O(commits x changed_files_per_commit).
- `commitChangedFiles` uses tree diffing (no line counting) instead of the
  expensive `c.Stats()` call.

**field.go — HEAD-based caching and delta updates**
- Three update modes selected automatically:
  1. HEAD unchanged + scores exist -> no-op (instant)
  2. Previous HEAD known + new commits -> delta scan (rescore only changed files)
  3. No previous state -> full scan (startup only)
- `deltaUpdate` opens the repo once and reuses the handle for both tree
  diffing and per-file scoring.

**process.go — Cached coherence and conditional index rebuild**
- Coherence report is cached after each consolidation tick and reused by
  the heartbeat (previously ran the full 4-layer validation twice per cycle).
- `BuildIndex` is skipped when HEAD has not changed since the last rebuild.

**ledger.go — In-memory last-event cache**
- `AppendEvent` now caches the last event per session in memory. Previously
  it scanned the entire JSONL ledger file from the beginning on every append,
  producing O(N^2) I/O growth over the session lifetime.

**config.go — Default consolidation interval**
- Increased from 300s (5 min) to 900s (15 min). The HEAD cache makes this
  moot when nothing has changed, but it reduces unnecessary tick overhead
  even without the cache.

**serve_foveated_test.go — Fixed pre-existing test failure**
- `TestHandleFoveatedContext` now initializes a real git repo and builds
  the CogDoc index, matching production initialization.

### Results

| Metric | Before | After |
|--------|--------|-------|
| Steady-state CPU | 200% | 0% |
| Field populated | Never (0 files) | 4,358 files |
| Process state | Stuck `consolidating` | `receptive` |
| Initial scan | Never completed | ~13s, then idle |
| Repo opens per scan | 4,637 | 1 |
| Subsequent updates | Full rescan | Delta only |
| Ledger append cost | O(N) file scan | O(1) cached |
| Tests | 1 failing | All passing |
