# Archival: MCP Always-On + Orphaned Dashboard Stubs (2026-04-21)

## What was removed

**Build-tag gates removed (`//go:build mcpserver` stripped from 14 files in `internal/engine/`):**
- `cogdoc_service.go`, `cogdoc_service_test.go`
- `ingest.go`, `ingest_policy.go`, `ingest_test.go`, `ingest_url.go`
- `mcp_server.go`, `mcp_server_test.go`, `mcp_stubs.go`
- `membrane_default.go`, `membrane_default_test.go`
- `serve_mcp.go`
- `tool_loop.go`, `tool_loop_test.go`

**Files deleted:**
- `internal/engine/serve_mcp_stub.go` — the noop `!mcpserver` fallback (8 lines).
- `serve_dashboard_stubs.go` — a local, never-committed working file with five stub handlers for `*serveServer` (`handleProprioceptiveStub`, `handleLightConeStub`, `handleContextStub`, `handleDebugLastStub`, `handleDebugContextStub`) plus a `readLastJSONLEntriesTail` helper.

## Rationale

### MCP build tag

The `mcpserver` build tag was gating cogos-v3's defining capabilities — MCP server, ingestion pipeline, tool loop, cogdoc service, membrane policy — behind an opt-in flag. Investigation found:

1. **MCP is foundational, not optional.** The kernel's primary use case is LLM collaboration via MCP (Claude Code, LM Studio). An MCP-less build doesn't serve that purpose.
2. **No build path ever enabled the tag.** No `Makefile`, no `.goreleaser.yml`, no CI workflow sets `-tags mcpserver`. Default `go build ./...` produced binaries with MCP silently off.
3. **The `!mcpserver` noop fallback served no use case.** `serve_mcp_stub.go` registered nothing; MCP was either on (via a tag no one set) or invisible.

Net effect: releases shipped without MCP by default, silently breaking the primary use case. Removing the tag makes the default build behave like the (intended but never configured) flagged build.

### Orphaned dashboard stubs

`serve_dashboard_stubs.go` was an in-progress working file (never committed to any branch) intended to bolt five empty-state handlers onto the root `serveServer`. Call-graph analysis determined the root `serveServer` is dead code in the default `cogos serve` flow:

```
cmd/cogos/main.go
  → engine.Main()                  (internal/engine/cli.go:36)
    → runServe()                    (internal/engine/cli.go:134)
      → engine.NewServer()          (internal/engine/serve.go:55)
```

Root `newServeServer` (in `./serve.go:97`) has exactly one caller — `serve_daemon.go:411`, inside an OCI-auto-reload code path not exercised by the default `cogos serve` command.

Meanwhile, `engine.NewServer()` in `internal/engine/serve.go` registers `/v1/proprioceptive`, `/v1/lightcone`, `/v1/context`, `/v1/debug/last`, `/v1/debug/context` natively (lines 62–70). The dashboard HTML (`internal/engine/web/dashboard.html`) fetches these endpoints and gets real data from the engine, not stubs.

The stubs would have added empty-state responses to a server that never receives traffic. Deleting them removes a footgun for anyone who might have later wired them up.

## Retrieval

**Build-tag state before this change:** `git show <THIS_COMMIT>^:internal/engine/serve_mcp_stub.go`

**This commit:** See the `chore: remove mcpserver build tag` commit on `chore/always-on-mcp` branch; commit hash filled in after merge.

**serve_dashboard_stubs.go was never committed.** The only copy existed in a working-tree of `feat/context-build-endpoint`. If the stub pattern is needed later, the file content can be reconstructed from conversational memory (see `cog-workspace/.cog/mem/` notes on 2026-04-20 dashboard work) or re-derived from the five handler signatures above.

## Followup candidates (not in this commit)

Call-graph analysis surfaced additional dead code that should be addressed in separate commits to keep review scope tight:

- `./serve.go` (~664 lines) — root `serveServer` is dead in the main binary path. Either delete entirely or clearly mark as legacy-for-OCI-only.
- `./mcp.go`, `./mcp_http.go` — root-package MCP server (stdio 4-tool, predates the engine's Go-SDK 10-tool MCP). Dead code if root `serveServer` is dead.
- `cog-workspace/.cog/cog.go` (~7537 lines) — legacy monolithic kernel predating the cogos extraction. Verify no callers, then archive.

Tracked in the consolidation map; to be landed as focused follow-up PRs.
