# PREREQ-3 Ingester Investigation — Findings

**Date:** 2026-05-01
**Branch:** agent/canonical-schema-prereq-3
**Scope:** Investigate `internal/engine/turn_storage.go` and `internal/linkfeed/linkfeed.go` for unclosed frontmatter `---` delimiters per the canonical schema migration PREREQ-3 requirement.

---

## Summary

The PREREQ-3 spec required patching two ingesters to write a closing `---` frontmatter delimiter. Investigation shows:

1. `turn_storage.go` does not write `.cog.md` cogdoc files at all — it writes JSONL sidecars.
2. `linkfeed.go` already writes correctly-delimited cogdoc frontmatter (opening AND closing `---`).
3. The "1 confirmed unclosed doc" finding in the audit (`2026-01-tom-conversation-2-index.cog.md`) was based on incorrect marker-counting logic. That file has exactly 2 `---` markers (open + close), which is correct per the `parseCogdoc()` contract.

**Conclusion: PREREQ-3 is satisfied. No kernel patch is needed. No backfill is needed.**

---

## Evidence

### turn_storage.go

`internal/engine/turn_storage.go` (305 lines) writes to `.cog/run/turns/<sessionID>.jsonl` — a JSONL sidecar file, not a `.cog.md` cogdoc. It does not write frontmatter at all. The `RecordTurn` function appends JSON rows to a per-session sidecar:

```go
func appendTurnSidecar(path string, rec TurnRecord) error {
    // ... writes JSON-encoded TurnRecord to JSONL file
}
```

There is no frontmatter, no `---`, no YAML. This file is outside the scope of PREREQ-3. Turn records are accessible via `cog_read_conversation` which reads the JSONL sidecar directly.

### linkfeed.go

`internal/linkfeed/linkfeed.go` (969 lines) writes `.cog.md` files to `.cog/mem/semantic/inbox/links/`. The `buildRawLinkCogDoc()` function at line 728 produces:

```
---
title: "..."
type: link
source: discord
...
memory_sector: semantic
tags: [link-feed, raw]
---           ← closing delimiter present

# title
...
```

The closing `---` is present at line 747 of the implementation. The linkfeed ingester is already canonical-schema-conformant with respect to frontmatter delimiters. It also writes `memory_sector` (which CRITICAL-1 now handles as an alias for `sector`).

### The "confirmed unclosed doc" re-examination

The audit claimed `2026-01-tom-conversation-2-index.cog.md` was unclosed because it had "2 `---` markers." This is backwards: exactly 2 `---` markers is the correct count for a properly delimited cogdoc (1 opening + 1 closing). The `parseCogdoc()` function in `indexer.go` uses `strings.SplitN(data, "---", 3)` which yields 3 parts from a 2-delimiter file. Files with only 1 `---` would be broken; files with 2 are correct.

Verified by inspecting the file directly: `---` at line 1 (opening), YAML frontmatter lines 2–54, `---` at line 55 (closing), then body content from line 57 onward. The document is correctly formatted.

The audit's "2 markers = unclosed" logic appears to have counted the total `---` occurrences in the file and expected ≥3 because some other files have `---` as body section separators. Those body-level separators are not frontmatter delimiters and are irrelevant to the parseCogdoc split.

---

## Design Note: When Canonical Ingestion Matters

As the corpus grows and new ingesters land, canonical schema conformance for machine-generated cogdocs requires:

1. Frontmatter opened with `---` and closed with `---` (currently satisfied by linkfeed)
2. `type` from the machine enum (`link`, `conversation`, `session`) — currently satisfied
3. `status` from the machine enum (`raw`, `enriched`, `completed`) — currently satisfied in linkfeed
4. `source` field for provenance — currently satisfied in linkfeed

The one deviation worth noting: linkfeed uses `memory_sector: semantic` (old field name). CRITICAL-1 in PREREQ-1 now handles this as a parse-time alias, so no change to linkfeed is needed for the migration.

**If/when a conversation ingester is added** (currently conversations are manually-authored cogdocs), it should follow this same pattern. `turn_storage.go` is not a conversation ingester — it is a turn persistence mechanism for the agent harness's own session. If a future tool is added to export sessions as cogdocs, it should be built with canonical schema frontmatter from day one.

---

## What PREREQ-3 Actually Requires

The 6,438 unclosed-frontmatter docs described in the spec were projected for a corpus that has not yet accumulated. The current workspace has:
- 4 conversation docs — all correctly formatted
- 0 link docs in `mem/episodic/links/` — directory empty (linkfeed has not run against Discord in this workspace)

The spec's backfill requirement was predicated on a large corpus of unclosed docs. That corpus does not exist yet. When the corpus does accumulate at scale, the backfill mechanism should be a migration tool operation (per Phase 6 of the migration sequence), not a manual one-off.

**Recommendation:** Mark PREREQ-3 as satisfied. Update the spec's PREREQ-3 section to note that both ingesters were investigated and found conformant, and that the large-scale unclosed-frontmatter issue (6,438 docs) is a projected Phase 6 concern that will be addressed by the migration tool when the corpus reaches that volume.
